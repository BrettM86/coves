//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B8 (cycle): account erasure racing the role writes that can name the same
// account. In each row one side holds its locks while the other is observed
// waiting on a lock it holds (pg_stat_activity, pg_blocking_pids); then both
// run to the end. Neither may deadlock (40P01) or hang, and the end state has
// no role row and no invitation for the erased account.

// erasureRace is one row's view of the fixture: separate pools for the test's
// hold transaction, the erasure, the role consumer and the lock observer, so
// none of them waits for a connection held by another.
type erasureRace struct {
	f        *communityActionDBFixture
	hold     *sql.DB
	erasure  *sql.DB
	consumer *sql.DB
	observer *sql.DB
}

func newErasureRace(t *testing.T, f *communityActionDBFixture) *erasureRace {
	t.Helper()
	return &erasureRace{
		f:        f,
		hold:     openTestPool(t, f.db, "race-hold", 1),
		erasure:  openTestPool(t, f.db, "race-erasure", 2),
		consumer: openTestPool(t, f.db, "race-consumer", 2),
		observer: openTestPool(t, f.db, "race-observer", 1),
	}
}

// holdTransaction opens the test's own transaction and returns it with its
// backend PID. It is rolled back at cleanup if the row has not ended it.
func (r *erasureRace) holdTransaction(ctx context.Context, t *testing.T) (*sql.Tx, int) {
	t.Helper()
	tx, err := r.hold.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return tx, pid
}

// stallErasureAfterItsGate holds an uncommitted erasure marker for did, so an
// erasure of did takes its exclusive erasure gate and then waits at its own
// marker insert (step 0) until the hold ends.
func (r *erasureRace) stallErasureAfterItsGate(ctx context.Context, t *testing.T, did string) (*sql.Tx, int) {
	t.Helper()
	hold, pid := r.holdTransaction(ctx, t)
	_, err := hold.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, did)
	require.NoError(t, err)
	return hold, pid
}

// erase runs the production erasure of did in a goroutine.
func (r *erasureRace) erase(ctx context.Context, did string) <-chan error {
	return raceGo(func() error { return postgres.NewUserRepository(r.erasure).Delete(ctx, did) })
}

// awaitBlockedBy waits until a backend in the test database is waiting on a
// lock held by holderPID and returns that backend's PID. It fails if pending
// returned first: the call was expected to wait.
func (r *erasureRace) awaitBlockedBy(ctx context.Context, t *testing.T, holderPID int, pending <-chan error, waiter string) int {
	t.Helper()
	var waiterPID int
	var waitEvent, query string
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		if len(pending) > 0 {
			return false, errors.New(waiter + " returned before it waited on a lock")
		}
		err := r.observer.QueryRowContext(ctx, `
			SELECT pid, wait_event, left(query, 60) FROM pg_stat_activity
			WHERE datname = current_database()
				AND wait_event_type = 'Lock'
				AND $1 = ANY(pg_blocking_pids(pid))
			LIMIT 1`, holderPID).Scan(&waiterPID, &waitEvent, &query)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("%s waiting on a lock held by backend %d", waiter, holderPID))
	t.Logf("%s waits on a %s lock held by backend %d at %q", waiter, waitEvent, holderPID, query)
	return waiterPID
}

// pausedAccept starts invitee's accept of invitationID on a service paused
// before its repo write, with the action's locks held, and returns its
// result channel, the paused action's backend PID and the resume function.
func (r *erasureRace) pausedAccept(ctx context.Context, t *testing.T, invitee, invitationID string) (<-chan lockOutcome, int, func()) {
	t.Helper()
	service, paused, resume := r.f.pauseAtBeforeWrite(ctx)
	resumed := false
	resumeOnce := func() {
		if !resumed {
			resumed = true
			resume()
		}
	}
	t.Cleanup(resumeOnce)
	accepted := make(chan lockOutcome, 1)
	go func() {
		result, err := service.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
			ActorDID: invitee, InvitationID: invitationID, ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-1",
		})
		accepted <- lockOutcome{result: result, err: err}
	}()
	select {
	case <-paused:
	case got := <-accepted:
		t.Fatalf("the accept returned before its write: %v", got.err)
	case <-ctx.Done():
		t.Fatalf("the accept never reached its write: %v", ctx.Err())
	}
	return accepted, r.f.pausedActionPID(ctx, t), resumeOnce
}

func raceGo(call func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return done
}

func raceResult(ctx context.Context, t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatalf("%s did not finish: %v", name, ctx.Err())
		return nil
	}
}

func raceAcceptResult(ctx context.Context, t *testing.T, done <-chan lockOutcome) lockOutcome {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-ctx.Done():
		t.Fatalf("the accept did not finish: %v", ctx.Err())
		return lockOutcome{}
	}
}

func isDeadlock(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && databaseError.Code == "40P01"
}

// invitationsNaming is the number of stored invitations, in any community and
// state, with did as invitee or inviter.
func (f *communityActionDBFixture) invitationsNaming(t *testing.T, did string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM community_role_invitations WHERE invitee_did = $1 OR inviter_did = $1
	`, did).Scan(&count))
	return count
}

// rolesOf is the number of community_roles rows, in any community, for did.
func (f *communityActionDBFixture) rolesOf(t *testing.T, did string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM community_roles WHERE subject_did = $1`, did).Scan(&count))
	return count
}

func TestCommunityErasureRaces(t *testing.T) {
	t.Parallel()

	outsiderCreate := roleWriteEvent(rosterCommunity, "create", rosterOutsiderKey, rosterHeadOneRev, rosterOutsideGrantCID,
		roleRecord(rosterOutsider, communities.RoleModerator))

	tests := []struct {
		name           string
		erased, handle string
		// race stages the contention, lets both sides finish and returns
		// what the contending operation and the erasure returned.
		race func(ctx context.Context, t *testing.T, r *erasureRace) (contender, erasure error)
		// contender checks what the contending operation returned.
		contender   func(t *testing.T, err error)
		wantRoles   map[string]indexedRole
		wantVersion int64
		wantPending []pendingRoleRecordDelete
	}{
		{
			// The consumer waits on X's erasure gate, then finds X erased:
			// it indexes nothing and queues the event record's delete.
			name:   "(a1) erasure holds its gate, then a firehose create of X's role arrives",
			erased: rosterOutsider, handle: "rosteroutsider.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				hold, holdPID := r.stallErasureAfterItsGate(ctx, t, rosterOutsider)
				erased := r.erase(ctx, rosterOutsider)
				erasurePID := r.awaitBlockedBy(ctx, t, holdPID, erased, "the erasure")
				consumed := raceGo(func() error { return firehoseOn(r.consumer).HandleEvent(ctx, outsiderCreate) })
				r.awaitBlockedBy(ctx, t, erasurePID, consumed, "the role consumer")
				require.NoError(t, hold.Rollback())
				return raceResult(ctx, t, consumed, "the role consumer"), raceResult(ctx, t, erased, "the erasure")
			},
			contender:   func(t *testing.T, err error) { assert.NoError(t, err, "the consumer moves on") },
			wantRoles:   seededRoles(),
			wantVersion: 7,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsideGrantCID}},
		},
		{
			// The consumer holds X's shared gate and has written X's row; it
			// is held at the roster version row. The erasure waits for it,
			// then deletes the indexed row (+1 each: 7, 8, 9).
			name:   "(a2) a firehose create of X's role is in flight, then X is erased",
			erased: rosterOutsider, handle: "rosteroutsider.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				hold, holdPID := r.holdTransaction(ctx, t)
				_, err := hold.ExecContext(ctx, `
					SELECT 1 FROM community_subject_versions WHERE community_did = $1 AND subject = $1 FOR UPDATE
				`, rosterCommunity)
				require.NoError(t, err)
				consumed := raceGo(func() error { return firehoseOn(r.consumer).HandleEvent(ctx, outsiderCreate) })
				consumerPID := r.awaitBlockedBy(ctx, t, holdPID, consumed, "the role consumer")
				erased := r.erase(ctx, rosterOutsider)
				r.awaitBlockedBy(ctx, t, consumerPID, erased, "the erasure")
				require.NoError(t, hold.Rollback())
				return raceResult(ctx, t, consumed, "the role consumer"), raceResult(ctx, t, erased, "the erasure")
			},
			contender:   func(t *testing.T, err error) { assert.NoError(t, err, "the consumer indexes the create") },
			wantRoles:   seededRoles(),
			wantVersion: 9,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsideGrantCID}},
		},
		{
			// X's accept of O's moderator invitation is paused before its
			// write with its locks held; the erasure of X waits for it, then
			// deletes the accepted invitation and the new role row.
			name:   "(b) X's acceptance of a moderator invitation is in flight, then X is erased",
			erased: rosterOutsider, handle: "rosteroutsider.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				r.f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
				accepted, actionPID, resume := r.pausedAccept(ctx, t, rosterOutsider, rosterModeratorInvitationID)
				erased := r.erase(ctx, rosterOutsider)
				r.awaitBlockedBy(ctx, t, actionPID, erased, "the erasure")
				resume()
				return acceptedAtVersion8(ctx, t, accepted), raceResult(ctx, t, erased, "the erasure")
			},
			contender:   func(t *testing.T, err error) { assert.NoError(t, err, "the accept completes before the erasure") },
			wantRoles:   seededRoles(),
			wantVersion: 9,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderWrittenCID}},
		},
		{
			// The invite waits on X's erasure gate, then finds X erased and
			// writes nothing.
			name:   "(c1) erasure holds its gate, then O invites X",
			erased: rosterOutsider, handle: "rosteroutsider.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				hold, holdPID := r.stallErasureAfterItsGate(ctx, t, rosterOutsider)
				erased := r.erase(ctx, rosterOutsider)
				erasurePID := r.awaitBlockedBy(ctx, t, holdPID, erased, "the erasure")
				invited := raceGo(func() error { return r.invite(ctx) })
				r.awaitBlockedBy(ctx, t, erasurePID, invited, "the invite")
				require.NoError(t, hold.Rollback())
				return raceResult(ctx, t, invited, "the invite"), raceResult(ctx, t, erased, "the erasure")
			},
			contender: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, moderation.ErrAccountNotFound, "the erased invitee is refused")
			},
			wantRoles:   seededRoles(),
			wantVersion: 7,
			wantPending: []pendingRoleRecordDelete{},
		},
		{
			// O's invite of X holds X's shared gate and waits at superseding
			// X's earlier open invitation (row held by the test); the erasure
			// waits for the invite, then deletes both invitations.
			name:   "(c2) O's invite of X is in flight, then X is erased",
			erased: rosterOutsider, handle: "rosteroutsider.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				r.f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
				hold, holdPID := r.holdTransaction(ctx, t)
				_, err := hold.ExecContext(ctx, `SELECT 1 FROM community_role_invitations WHERE id = $1 FOR UPDATE`,
					rosterModeratorInvitationID)
				require.NoError(t, err)
				invited := raceGo(func() error { return r.invite(ctx) })
				invitePID := r.awaitBlockedBy(ctx, t, holdPID, invited, "the invite")
				erased := r.erase(ctx, rosterOutsider)
				r.awaitBlockedBy(ctx, t, invitePID, erased, "the erasure")
				require.NoError(t, hold.Rollback())
				return raceResult(ctx, t, invited, "the invite"), raceResult(ctx, t, erased, "the erasure")
			},
			contender:   func(t *testing.T, err error) { assert.NoError(t, err, "the invite commits before the erasure") },
			wantRoles:   seededRoles(),
			wantVersion: 7,
			wantPending: []pendingRoleRecordDelete{},
		},
		{
			// M's accept of O's ownership offer (O the paired previous owner)
			// is paused before its write; the erasure of O waits for it, then
			// deletes O's demoted moderator row and the offer O sent.
			name:   "(d) a transfer acceptance naming owner P as previous owner is in flight, then P is erased",
			erased: rosterOwner, handle: "rosterowner.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				r.f.seedInvitation(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false))
				accepted, actionPID, resume := r.pausedAccept(ctx, t, rosterModerator, rosterOwnershipOfferID)
				erased := r.erase(ctx, rosterOwner)
				r.awaitBlockedBy(ctx, t, actionPID, erased, "the erasure")
				resume()
				return acceptedAtVersion8(ctx, t, accepted), raceResult(ctx, t, erased, "the erasure")
			},
			contender: func(t *testing.T, err error) { assert.NoError(t, err, "the transfer completes before the erasure") },
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorWrittenCID},
			},
			wantVersion: 9,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerWrittenCID}},
		},
		{
			// An admin's ownership offer, so the previous owner O is gated
			// only as the transfer's paired subject. M's accept loses its
			// response with the transfer committed; the erasure of O holds
			// its gate; the firehose copy of M's promotion then waits on O's
			// gate and, once O is erased, indexes M as owner but not O's
			// demotion, queuing that record's delete (+1 each: 7, 8, 9).
			name:   "(d2) erasure of previous owner P holds its gate, then the firehose copy of a lost transfer's promotion arrives",
			erased: rosterOwner, handle: "rosterowner.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				r.f.writeBodyCIDs()
				r.f.loseAcceptResponse(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterAdmin, true), "accept-1")
				hold, holdPID := r.stallErasureAfterItsGate(ctx, t, rosterOwner)
				erased := r.erase(ctx, rosterOwner)
				erasurePID := r.awaitBlockedBy(ctx, t, holdPID, erased, "the erasure")
				consumed := raceGo(func() error {
					return firehoseOn(r.consumer).HandleEvent(ctx, moderatorPromotionCopy(rosterHeadOneRev))
				})
				r.awaitBlockedBy(ctx, t, erasurePID, consumed, "the role consumer")
				require.NoError(t, hold.Rollback())
				return raceResult(ctx, t, consumed, "the role consumer"), raceResult(ctx, t, erased, "the erasure")
			},
			contender: func(t *testing.T, err error) { assert.NoError(t, err, "the consumer completes the transfer") },
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
			},
			wantVersion: 9,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerModeratorBodyCID}},
		},
		{
			// The outsider's accept of O's moderator invitation is paused
			// before its completion accepts O's invitation; the erasure of
			// inviter O waits for it, then deletes the invitation and O's
			// owner row.
			name:   "(e) a completion updating inviter X's invitation is in flight, then X is erased",
			erased: rosterOwner, handle: "rosterowner.test",
			race: func(ctx context.Context, t *testing.T, r *erasureRace) (error, error) {
				r.f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
				accepted, actionPID, resume := r.pausedAccept(ctx, t, rosterOutsider, rosterModeratorInvitationID)
				erased := r.erase(ctx, rosterOwner)
				r.awaitBlockedBy(ctx, t, actionPID, erased, "the erasure")
				resume()
				return acceptedAtVersion8(ctx, t, accepted), raceResult(ctx, t, erased, "the erasure")
			},
			contender: func(t *testing.T, err error) { assert.NoError(t, err, "the accept completes before the erasure") },
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorCID},
				rosterOutsider:  {Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID},
			},
			wantVersion: 9,
			wantPending: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerCID}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityActionDBFixture(t)
			f.indexUser(t, tc.erased, tc.handle)
			r := newErasureRace(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			contenderErr, erasureErr := tc.race(ctx, t, r)

			assert.False(t, isDeadlock(contenderErr), "the contending operation deadlocked: %v", contenderErr)
			assert.False(t, isDeadlock(erasureErr), "the erasure deadlocked: %v", erasureErr)
			require.NoError(t, erasureErr, "the erasure succeeds")
			tc.contender(t, contenderErr)
			assert.Zero(t, f.rolesOf(t, tc.erased), "no role row for the erased account")
			assert.Zero(t, f.invitationsNaming(t, tc.erased), "no invitation names the erased account")
			assert.Equal(t, tc.wantRoles, f.indexedRoles(t), "the community's roles")
			assert.EqualValues(t, tc.wantVersion, f.rosterVersion(t), "the roster version")
			assert.Equal(t, tc.wantPending, f.pendingRoleRecordDeletes(t), "the queued role record deletes")
		})
	}
}

// invite has O invite the outsider as moderator.
func (r *erasureRace) invite(ctx context.Context) error {
	_, err := r.f.invitations.InviteModerator(ctx, moderation.InviteRequest{
		ActorDID: rosterOwner, CommunityDID: rosterCommunity, Invitee: "rosteroutsider.test",
		ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
	})
	return err
}

// acceptedAtVersion8 waits for the accept and checks that a completed accept
// reports roster version 8; it returns the accept's error.
func acceptedAtVersion8(ctx context.Context, t *testing.T, accepted <-chan lockOutcome) error {
	t.Helper()
	got := raceAcceptResult(ctx, t, accepted)
	if got.err == nil && assert.NotNil(t, got.result) {
		assert.EqualValues(t, 8, got.result.Version, "the accept advanced the roster version once")
	}
	return got.err
}

// The inviter's erasure racing an accept of the inviter's invitation, where
// opening the community repo writes the community row (a credential refresh)
// on another connection. Inviter O subscribes to the community, so O's erasure
// locks the community row before it deletes O's invitations. The accept holds
// the invitation row while Repos refreshes the community's credentials; if the
// erasure got past its gate it would hold the community row and wait on the
// invitation, and the refresh would wait on the erasure: a cycle Postgres
// cannot see. The accept takes the inviter's shared erasure gate, so the
// erasure waits at its gate and both finish.
func TestCommunityErasureRaces_InviterErasedWhileAcceptRefreshesCredentials(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.indexUser(t, rosterOwner, "rosterowner.test")
	_, err := f.db.Exec(`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, rosterOwner, rosterCommunity)
	require.NoError(t, err)
	f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
	r := newErasureRace(t, f)
	refresh := openTestPool(t, f.db, "race-refresh", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opened := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	config := f.config
	config.Repos = func(ctx context.Context, communityDID string) (posts.CommunityRepo, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(opened)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// The refresh persists rotated credentials on the application
			// pool, as EnsureFreshToken does.
			if _, err := refresh.ExecContext(ctx, `
				UPDATE communities SET pds_access_token_encrypted = pds_access_token_encrypted WHERE did = $1
			`, communityDID); err != nil {
				return nil, err
			}
		}
		return f.factory.open(ctx, communityDID)
	}
	service := moderation.NewCommunityActionService(config)

	accepted := make(chan lockOutcome, 1)
	go func() {
		result, err := service.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
			ActorDID: rosterOutsider, InvitationID: rosterModeratorInvitationID,
			ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-1",
		})
		accepted <- lockOutcome{result: result, err: err}
	}()
	select {
	case <-opened:
	case got := <-accepted:
		t.Fatalf("the accept returned before it opened the community repo: %v", got.err)
	case <-ctx.Done():
		t.Fatalf("the accept never opened the community repo: %v", ctx.Err())
	}
	actionPID := f.pausedActionPID(ctx, t)
	erased := r.erase(ctx, rosterOwner)
	r.awaitBlockedBy(ctx, t, actionPID, erased, "the erasure")
	close(release)

	acceptErr := acceptedAtVersion8(ctx, t, accepted)
	erasureErr := raceResult(ctx, t, erased, "the erasure")
	require.NoError(t, acceptErr, "the accept completes before the erasure")
	require.NoError(t, erasureErr, "the erasure succeeds")
	assert.Zero(t, f.rolesOf(t, rosterOwner), "no role row for the erased inviter")
	assert.Zero(t, f.invitationsNaming(t, rosterOwner), "no invitation names the erased inviter")
	assert.Equal(t, map[string]indexedRole{
		rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorCID},
		rosterOutsider:  {Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID},
	}, f.indexedRoles(t), "the community's roles")
	assert.EqualValues(t, 9, f.rosterVersion(t), "the roster version")
}
