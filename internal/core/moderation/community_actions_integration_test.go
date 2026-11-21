//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The community action service against real Postgres: the real community
// action store, role index, role authorizer and subject state reader, with the
// community's PDS repo faked (fakeRosterRepo). Results are read back from the
// database.

const (
	rosterInstanceDID = "did:web:test.local"
	rosterOwnerURI    = "at://did:plc:rosteralpha/social.coves.community.moderator/5jbtoxiw4eatw5k4ht7uq6zz37en4i73i5b6yu6ctid6v23oimya"
)

type communityActionDBFixture struct {
	db      *sql.DB
	clock   *queueClock
	repo    *fakeRosterRepo
	factory *rosterRepoFactory
	service *moderation.CommunityActionService
	// invitations is the invitation service over the same store and clock.
	invitations *moderation.CommunityInvitationService
	config      moderation.CommunityActionConfig
	authorizer  communities.RoleAuthorizer
	state       *moderation.CommunityStateService
}

// newCommunityActionDBFixture: rosteralpha is hosted (a stored PDS refresh
// token) at roster version 7, with owner O and moderator M indexed, each
// record's rev gate at head zero's rev, and both records in the community repo
// at head zero. rosteradmin is an instance admin with no role. A create or
// update of O's, M's or the outsider's record returns its written CID, and the
// invitation service resolves rosterowner.test, rostermod.test and
// rosteroutsider.test.
func newCommunityActionDBFixture(t *testing.T) *communityActionDBFixture {
	t.Helper()
	db := testkit.DB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at, pds_refresh_token_encrypted)
		VALUES ($1, 'rosteralpha.test', 'Roster Alpha', $1, $1, $2, NOW(), $3)
	`, rosterCommunity, rosterInstanceDID, []byte("hosted"))
	require.NoError(t, err)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, role := range []communities.CommunityRole{
		rosterRole(rosterCommunity, rosterOwner, communities.RoleOwner, rosterOwnerKey, rosterOwnerCID, created),
		rosterRole(rosterCommunity, rosterModerator, communities.RoleModerator, rosterModeratorKey, rosterModeratorCID, created.Add(time.Hour)),
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO community_roles (community_did, subject_did, role, record_key, record_cid, record_created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, role.CommunityDID, role.SubjectDID, role.Role, role.RecordKey, role.RecordCID, role.RecordCreatedAt)
		require.NoError(t, err)
	}
	for _, uri := range []string{rosterOwnerURI, rosterModeratorURI} {
		_, err := db.ExecContext(ctx, `INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`, uri, rosterHeadZeroRev)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subject_versions (community_did, subject, version) VALUES ($1, $1, $2)
	`, rosterCommunity, rosterStartVersion)
	require.NoError(t, err)

	repo := &fakeRosterRepo{
		did:        rosterCommunity,
		records:    map[string]*pds.RecordResponse{},
		head:       pds.LatestCommit{CID: rosterHeadZeroCID, Rev: rosterHeadZeroRev},
		commitHead: pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		writeCIDs: map[string]string{
			communities.RoleCollection + "/" + rosterOutsiderKey:  rosterOutsiderWrittenCID,
			communities.RoleCollection + "/" + rosterModeratorKey: rosterModeratorWrittenCID,
			communities.RoleCollection + "/" + rosterOwnerKey:     rosterOwnerWrittenCID,
		},
	}
	repo.setRecord(communities.RoleCollection, rosterOwnerKey, rosterOwnerCID)
	repo.setRecord(communities.RoleCollection, rosterModeratorKey, rosterModeratorCID)
	factory := &rosterRepoFactory{repo: repo}

	clock := &queueClock{now: rosterClockStart}
	authorizer := rosterAuthorizer(db, clock)
	state := moderation.NewCommunityStateService(postgres.NewCommunityBanRepository(db), authorizer,
		postgres.NewCommunitySubjectStateReader(db))
	config := moderation.CommunityActionConfig{
		Authorizer:     authorizer,
		Store:          postgres.NewCommunityActionRepository(db, openTestPool(t, db, "community-actions", 0)),
		Repos:          factory.open,
		Now:            clock.Now,
		IntentTTL:      10 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	}
	service := moderation.NewCommunityActionService(config)
	invitations := moderation.NewCommunityInvitationService(moderation.CommunityInvitationConfig{
		Authorizer:  authorizer,
		Store:       config.Store,
		Invitations: postgres.NewCommunityInvitationRepository(db),
		Resolver: rosterHandles{
			"rosterowner.test":    rosterOwner,
			"rostermod.test":      rosterModerator,
			"rosteroutsider.test": rosterOutsider,
		},
		Now:            clock.Now,
		IdempotencyTTL: config.IdempotencyTTL,
	})

	// Fixture check: M is a moderator the authorizer and the state read accept.
	auth, err := authorizer.Authorize(ctx, rosterCommunity, rosterModerator, communities.RoleModerator)
	require.NoError(t, err, "fixture: M must be authorized as moderator")
	require.Equal(t, communities.RoleModerator, auth.Role, "fixture: M holds the moderator role")
	seeded, err := state.GetCommunityState(ctx, moderation.CommunityStateRequest{
		ViewerDID: rosterModerator, Subject: rosterCommunity, CommunityDID: rosterCommunity,
	})
	require.NoError(t, err, "fixture: M must read the roster state")
	require.EqualValues(t, rosterStartVersion, seeded.Version, "fixture: roster version seeded")

	return &communityActionDBFixture{
		db: db, clock: clock, repo: repo, factory: factory, service: service, invitations: invitations,
		config: config, authorizer: authorizer, state: state,
	}
}

// rosterAuthorizer is the real role authorizer with every read on db.
func rosterAuthorizer(db *sql.DB, clock *queueClock) communities.RoleAuthorizer {
	return communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:        postgres.NewHostedCommunityQuery(db),
		Removals:      postgres.NewCommunityRemovalReader(db, rosterInstanceDID),
		Roles:         postgres.NewCommunityRoleRepository(db),
		AccountBans:   postgres.NewAccountBanReader(db, rosterInstanceDID),
		AccountStatus: postgres.NewAccountStatusRepository(db),
		Admins:        moderation.NewAllowlistAuthority([]string{rosterAdmin}),
		Now:           clock.Now,
	})
}

// openTestPool opens a second handle on db's test database, capped at
// maxOpen connections (0 is unlimited), as the server opens its dedicated
// pools.
func openTestPool(t *testing.T, db *sql.DB, applicationName string, maxOpen int) *sql.DB {
	t.Helper()
	var name string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))
	pool, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(name)+"&application_name="+url.QueryEscape(applicationName))
	require.NoError(t, err)
	pool.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = pool.Close() })
	require.NoError(t, pool.Ping(), "ping the %s pool", applicationName)
	return pool
}

// serviceWithHooks is a second service over the fixture's database and
// community repo whose requests run hooks; f.service stays hook-free.
func (f *communityActionDBFixture) serviceWithHooks(hooks moderation.CommunityActionHooks) *moderation.CommunityActionService {
	config := f.config
	config.Hooks = hooks
	return moderation.NewCommunityActionService(config)
}

func (f *communityActionDBFixture) remove(ctx context.Context, actorDID, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
		ActorDID: actorDID, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
		ExpectedVersion: rosterStartVersion, IdempotencyKey: key,
	})
}

// pastIntentExpiry moves the clock just past the first intent's expiry.
func (f *communityActionDBFixture) pastIntentExpiry() {
	f.clock.Set(time.Date(2026, 11, 13, 12, 10, 1, 0, time.UTC))
}

func (f *communityActionDBFixture) moderatorRowIndexed(t *testing.T) bool {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM community_roles WHERE community_did = $1 AND subject_did = $2
	`, rosterCommunity, rosterModerator).Scan(&count))
	return count == 1
}

func (f *communityActionDBFixture) rosterVersion(t *testing.T) int64 {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(
		context.Background(), rosterCommunity, rosterCommunity, moderation.CommunitySubjectRoster)
	require.NoError(t, err)
	return state.Version
}

func (f *communityActionDBFixture) moderatorGateRev(t *testing.T) string {
	t.Helper()
	var rev string
	require.NoError(t, f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, rosterModeratorURI).Scan(&rev))
	return rev
}

// storedIntent is the intent fields these tests pin, oldest first.
type storedIntent struct {
	State     string
	CommitRev string
}

func (f *communityActionDBFixture) intents(t *testing.T) []storedIntent {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT state, COALESCE(commit_rev, '') FROM community_write_intents
		WHERE community_did = $1 ORDER BY created_at, id
	`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []storedIntent{}
	for rows.Next() {
		var intent storedIntent
		require.NoError(t, rows.Scan(&intent.State, &intent.CommitRev))
		out = append(out, intent)
	}
	require.NoError(t, rows.Err())
	return out
}

// storedAction is one moderation_actions row in the community.
type storedAction struct {
	Action, ActorDID                           string
	AdminActing                                bool
	AuthorityDID, ScopeKind, ScopeCommunityDID string
	SubjectURI, Origin                         string
}

func (f *communityActionDBFixture) actions(t *testing.T) []storedAction {
	t.Helper()
	return communityActionsIn(t, f.db, rosterCommunity)
}

// communityActionsIn is the moderation_actions rows scoped to communityDID,
// oldest first.
func communityActionsIn(t *testing.T, db *sql.DB, communityDID string) []storedAction {
	t.Helper()
	rows, err := db.Query(`
		SELECT action, actor_did, admin_acting, authority_did, scope_kind, scope_community_did, subject_uri, origin
		FROM moderation_actions WHERE scope_community_did = $1 ORDER BY created_at, id
	`, communityDID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []storedAction{}
	for rows.Next() {
		var action storedAction
		require.NoError(t, rows.Scan(&action.Action, &action.ActorDID, &action.AdminActing, &action.AuthorityDID,
			&action.ScopeKind, &action.ScopeCommunityDID, &action.SubjectURI, &action.Origin))
		out = append(out, action)
	}
	require.NoError(t, rows.Err())
	return out
}

// moderatorRemovalAction is the one action a local removal of M writes.
func moderatorRemovalAction(actorDID string, adminActing bool) storedAction {
	return storedAction{
		Action: "remove-role", ActorDID: actorDID, AdminActing: adminActing,
		AuthorityDID: rosterCommunity, ScopeKind: "community", ScopeCommunityDID: rosterCommunity,
		SubjectURI: "at://did:plc:troublemaker456", Origin: "local",
	}
}

// B6: owner O removes moderator M. On return the record and the index row are
// gone, M has lost its authority, and one action, the roster version, the rev
// gate and the intent record the removal.
func TestCommunityActionDB_OwnerRemovesModerator(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()

	_, err := f.remove(ctx, rosterOwner, "remove-1")

	require.NoError(t, err)
	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey), "M's record is gone from the community repo")
	assert.False(t, f.moderatorRowIndexed(t), "M's community_roles row is gone before RemoveModerator returns")
	_, err = f.state.GetCommunityState(ctx, moderation.CommunityStateRequest{
		ViewerDID: rosterModerator, Subject: rosterCommunity, CommunityDID: rosterCommunity,
	})
	assert.ErrorIs(t, err, moderation.ErrForbidden, "M's immediate getCommunityState is refused")
	_, err = f.authorizer.Authorize(ctx, rosterCommunity, rosterModerator, communities.RoleModerator)
	assert.ErrorIs(t, err, communities.ErrUnauthorized, "the authorizer refuses M as moderator")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced by one")
	assert.Equal(t, rosterHeadOneRev, f.moderatorGateRev(t), "the rev gate holds the commit rev the PDS returned")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t))
}

// B7: M resigns; the index row is gone before return and one action names M.
func TestCommunityActionDB_ModeratorResigns(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)

	_, err := f.service.ResignRole(context.Background(), moderation.ResignRoleRequest{
		ActorDID: rosterModerator, CommunityDID: rosterCommunity, ExpectedVersion: rosterStartVersion, IdempotencyKey: "resign-1",
	})

	require.NoError(t, err)
	assert.False(t, f.moderatorRowIndexed(t), "M's community_roles row is gone before ResignRole returns")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced by one")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterModerator, false)}, f.actions(t))
}

// B8: an instance admin with no role removes M in a hosted community.
func TestCommunityActionDB_InstanceAdminRemovesModerator(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)

	_, err := f.remove(context.Background(), rosterAdmin, "admin-remove-1")

	require.NoError(t, err)
	assert.False(t, f.moderatorRowIndexed(t), "M's community_roles row is gone")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterAdmin, true)}, f.actions(t),
		"the action names the admin and is marked admin-acting")
}

// Chunk 13 admin acting: an instance admin with no role removes M in the
// hosted community; the public log, filtered to the community, names the admin
// as actor and marks the entry admin-acting.
func TestCommunityModlog_AdminActingRemovalListsPublicly(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	_, err := f.remove(context.Background(), rosterAdmin, "admin-remove-1")
	require.NoError(t, err)

	entries := publicModlog(t, f.db, moderation.ListActionsParams{Community: "did:plc:rosteralpha"})

	require.Len(t, entries, 1, "the admin's removal is listed once")
	entry := entries[0]
	assert.Equal(t, "remove-role", entry["action"])
	assert.Equal(t, true, entry["adminActing"], "the entry is marked admin-acting")
	assert.Equal(t, map[string]any{"did": "did:plc:rosteradmin"}, entry["actor"], "the admin is named")
	assert.Equal(t, "moderator", entry["role"], "the removed role is named")
	assert.Equal(t, map[string]any{"kind": "community", "communityDid": "did:plc:rosteralpha"}, entry["scope"])
}

// B9: the PDS delete fails before committing; once the intent expires the
// sweep finds the head unchanged and expires it, leaving the role in place.
func TestCommunityActionDB_SweepExpiresAnUncommittedIntent(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.remove(ctx, rosterOwner, "remove-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	f.pastIntentExpiry()

	swept, err := f.service.SweepExpiredIntents(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, swept, "the one expired intent is reconciled")
	assert.True(t, f.moderatorRowIndexed(t), "M's row stays indexed")
	assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
	assert.Equal(t, rosterHeadZeroRev, f.moderatorGateRev(t), "rev gate unchanged")
	assert.Equal(t, []storedIntent{{State: "expired"}}, f.intents(t))
	assert.Empty(t, f.actions(t), "no action")
}

// R2-2: a reconcile's terminal transition persists in Postgres.
func TestCommunityActionDB_ReconcileTransitionsPersist(t *testing.T) {
	t.Parallel()

	t.Run("head moved with the replaced record still present fails the intent", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		ctx := context.Background()
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.remove(ctx, rosterOwner, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

		_, err = f.remove(ctx, rosterOwner, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict)
		assert.Equal(t, []storedIntent{{State: "failed"}}, f.intents(t), "the failed transition is committed")
		assert.True(t, f.moderatorRowIndexed(t), "M's row unchanged")
		assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey), "M's record stays in the repo")
		assert.Empty(t, f.actions(t), "no action")
	})

	t.Run("expired same-key retry restarts with a fresh intent", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.remove(context.Background(), rosterOwner, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		f.pastIntentExpiry()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := f.remove(ctx, rosterOwner, "remove-1")

		require.NoError(t, err, "the retry must finish, not wait on its own expired intent")
		require.NoError(t, ctx.Err(), "the retry finished before the deadline")
		if assert.NotNil(t, result) {
			assert.EqualValues(t, 8, result.Version)
			assert.Equal(t, rosterModerator, result.SubjectDID)
		}
		assert.Equal(t, []storedIntent{{State: "expired"}, {State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
			"the old intent is expired and a new one completed")
		assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
		assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t))
	})
}

// R1-9 with R2-1: the PDS applies the delete and the response is lost; no
// retry comes, and the sweep completes the intent at the head rev it read
// before the record read, advancing the rev gate only to the intent's guard
// rev (head zero, where the fixture's gate already is).
func TestCommunityActionDB_SweepCompletesALostResponse(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.remove(ctx, rosterOwner, "remove-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	f.pastIntentExpiry()
	// Another commit lands between the sweep's head read and its record read.
	f.repo.afterHeadRead = func(r *fakeRosterRepo) {
		r.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	}

	swept, err := f.service.SweepExpiredIntents(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, swept, "the one expired intent is reconciled")
	assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced by one")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"completed at the head rev read before the record read")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
		"one action naming the actor")
	assert.Equal(t, rosterHeadZeroRev, f.moderatorGateRev(t), "reconciliation advances the rev gate only to the guard rev")
}

// R1-3: the local completion is one transaction. A failure inserting the
// action leaves the index row, the roster version, the rev gate and the intent
// as they were.
func TestCommunityActionDB_LocalCompletionIsOneTransaction(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	_, err := f.db.ExecContext(ctx, `
		CREATE FUNCTION fail_roster_action_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected moderation_actions insert failure';
		END
		$$
	`)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, `
		CREATE TRIGGER fail_roster_action_insert BEFORE INSERT ON moderation_actions
		FOR EACH ROW WHEN (NEW.scope_community_did = 'did:plc:rosteralpha')
		EXECUTE FUNCTION fail_roster_action_insert()
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DROP TRIGGER IF EXISTS fail_roster_action_insert ON moderation_actions`)
		_, _ = f.db.Exec(`DROP FUNCTION IF EXISTS fail_roster_action_insert()`)
	})

	_, err = f.remove(ctx, rosterOwner, "remove-1")

	assert.Error(t, err, "the failed action insert fails the removal")
	assert.True(t, f.moderatorRowIndexed(t), "M's row unchanged")
	assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
	assert.Equal(t, rosterHeadZeroRev, f.moderatorGateRev(t), "rev gate unchanged")
	assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent stays open")
	assert.Empty(t, f.actions(t), "no action")
}

// B14: a removed moderator cannot finish an action. (a) M's resign is
// authorized and paused before the locks while O removes M; when M resumes,
// the recheck under the locks refuses it. (b) An action by M on another record
// holds M's per-account lock; O's removal of M waits on that lock and
// completes once the holder ends. (c) O's removal pauses before its PDS write
// with its locks held; M's resign, authorized while M is still indexed, waits
// on those locks, and once O commits the recheck under the locks refuses it.
// Every way, O's removal is the only write.
func TestCommunityActionDB_RemovedModeratorCannotFinishAnAction(t *testing.T) {
	t.Parallel()
	type outcome struct {
		result *moderation.CommunityActionResult
		err    error
	}
	tests := []struct {
		name string
		run  func(ctx context.Context, t *testing.T, f *communityActionDBFixture)
	}{
		{
			name: "resign authorized before the removal is refused by the recheck",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				paused := make(chan struct{})
				resume := make(chan struct{})
				resigning := f.serviceWithHooks(moderation.CommunityActionHooks{
					AfterAuthorize: func(context.Context) {
						close(paused)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					},
				})
				resigned := make(chan outcome, 1)
				go func() {
					result, err := resigning.ResignRole(ctx, moderation.ResignRoleRequest{
						ActorDID: rosterModerator, CommunityDID: rosterCommunity,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "resign-1",
					})
					resigned <- outcome{result, err}
				}()
				select {
				case <-paused:
				case got := <-resigned:
					t.Fatalf("M's resign returned before pausing after authorization: %v", got.err)
				case <-ctx.Done():
					t.Fatal("M's resign never reached the after-authorize pause")
				}

				_, err := f.remove(ctx, rosterOwner, "remove-1")
				require.NoError(t, err, "O removes M while M's resign is paused")
				close(resume)

				select {
				case got := <-resigned:
					assert.ErrorIs(t, got.err, moderation.ErrForbidden, "the recheck under the locks refuses M")
					assert.Nil(t, got.result, "M's resign has no result")
				case <-ctx.Done():
					t.Fatal("M's resign did not finish after resuming")
				}
			},
		},
		{
			name: "removal waits on the subject's account lock held by its other action",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				holder, err := postgres.NewCommunityActionRepository(f.db, f.db).BeginAction(ctx, moderation.CommunityActionLock{
					CommunityDID: rosterCommunity,
					AccountDIDs:  []string{rosterModerator},
					TargetURI:    "at://did:plc:rosteralpha/social.coves.community.post/3m5otherrecord",
				})
				require.NoError(t, err, "M's other action takes its locks")
				defer func() { _ = holder.Rollback() }()
				// The holder is the one backend in this test's private database
				// idle inside a transaction.
				var holderPID int
				require.NoError(t, f.db.QueryRowContext(ctx, `
					SELECT pid FROM pg_stat_activity
					WHERE datname = current_database() AND state = 'idle in transaction'
				`).Scan(&holderPID), "find the holder's backend")

				removed := make(chan outcome, 1)
				go func() {
					result, err := f.remove(ctx, rosterOwner, "remove-1")
					removed <- outcome{result, err}
				}()

				poll := time.NewTicker(10 * time.Millisecond)
				defer poll.Stop()
				blockedByHolder := false
				for !blockedByHolder {
					select {
					case got := <-removed:
						t.Fatalf("O's removal returned while M's action held M's account lock: %v", got.err)
					case <-ctx.Done():
						t.Fatal("O's removal never waited on an advisory lock held by M's action")
					case <-poll.C:
					}
					err := f.db.QueryRowContext(ctx, `
						SELECT EXISTS (
							SELECT 1 FROM pg_stat_activity
							WHERE datname = current_database()
							  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
							  AND $1 = ANY(pg_blocking_pids(pid))
						)
					`, holderPID).Scan(&blockedByHolder)
					require.NoError(t, err, "read the removal's lock wait")
				}
				select {
				case got := <-removed:
					t.Fatalf("O's removal returned while the holder is open: %v", got.err)
				default:
				}

				require.NoError(t, holder.Rollback(), "M's other action ends")
				select {
				case got := <-removed:
					require.NoError(t, got.err, "O's removal completes once the holder ends")
				case <-ctx.Done():
					t.Fatal("O's removal did not finish after the holder ended")
				}
			},
		},
		{
			name: "resign waiting on the removal's locks is refused by the recheck",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				paused := make(chan struct{})
				resume := make(chan struct{})
				removing := f.serviceWithHooks(moderation.CommunityActionHooks{
					BeforeWrite: func(context.Context) {
						close(paused)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					},
				})
				removed := make(chan outcome, 1)
				go func() {
					result, err := removing.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
						ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "remove-1",
					})
					removed <- outcome{result, err}
				}()
				select {
				case <-paused:
				case got := <-removed:
					t.Fatalf("O's removal returned before pausing with its locks held: %v", got.err)
				case <-ctx.Done():
					t.Fatal("O's removal never reached the before-write pause")
				}
				// O's action transaction is the one backend in this test's
				// private database idle inside a transaction.
				var removerPID int
				require.NoError(t, f.db.QueryRowContext(ctx, `
					SELECT pid FROM pg_stat_activity
					WHERE datname = current_database() AND state = 'idle in transaction'
				`).Scan(&removerPID), "find the removal's backend")

				// M is still indexed, so its resign passes the first
				// authorization and then waits on the locks O holds.
				resigned := make(chan outcome, 1)
				go func() {
					result, err := f.service.ResignRole(ctx, moderation.ResignRoleRequest{
						ActorDID: rosterModerator, CommunityDID: rosterCommunity,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "resign-1",
					})
					resigned <- outcome{result, err}
				}()
				poll := time.NewTicker(10 * time.Millisecond)
				defer poll.Stop()
				blockedByRemoval := false
				for !blockedByRemoval {
					select {
					case got := <-resigned:
						t.Fatalf("M's resign returned while O's removal held the locks: %v", got.err)
					case <-ctx.Done():
						t.Fatal("M's resign never waited on an advisory lock held by O's removal")
					case <-poll.C:
					}
					require.NoError(t, f.db.QueryRowContext(ctx, `
						SELECT EXISTS (
							SELECT 1 FROM pg_stat_activity
							WHERE datname = current_database()
							  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
							  AND $1 = ANY(pg_blocking_pids(pid))
						)
					`, removerPID).Scan(&blockedByRemoval), "read the resign's lock wait")
				}

				close(resume)
				select {
				case got := <-removed:
					require.NoError(t, got.err, "O's removal completes once resumed")
				case <-ctx.Done():
					t.Fatal("O's removal did not finish after resuming")
				}
				select {
				case got := <-resigned:
					assert.ErrorIs(t, got.err, moderation.ErrForbidden, "the recheck under the locks refuses M")
					assert.Nil(t, got.result, "M's resign has no result")
				case <-ctx.Done():
					t.Fatal("M's resign did not finish after O's removal committed")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			tc.run(ctx, t, f)

			assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS delete: O's")
			assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey), "M's record is gone")
			assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
			assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
			assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
				"only O's intent; none for M")
			assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
				"one remove-role action naming O")
		})
	}
}

// A removal that waited on the roster lock while a firehose update of M's
// record committed logs its action after that update's grant-role action:
// the action time is read once the locks are held, not at transaction start.
func TestCommunityActionDB_RemovalLogsAfterAChangeCommittedWhileItWaited(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	holder, err := postgres.NewCommunityActionRepository(f.db, f.db).BeginAction(ctx, moderation.CommunityActionLock{
		CommunityDID: rosterCommunity, Roster: true,
	})
	require.NoError(t, err, "another roster action takes the roster lock")
	defer func() { _ = holder.Rollback() }()
	var holderPID int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'idle in transaction'
	`).Scan(&holderPID), "find the holder's backend")

	type outcome struct {
		result *moderation.CommunityActionResult
		err    error
	}
	removed := make(chan outcome, 1)
	go func() {
		// The update below advances the roster to version 8 before the
		// removal reads it.
		result, err := f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
			ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
			ExpectedVersion: 8, IdempotencyKey: "remove-1",
		})
		removed <- outcome{result, err}
	}()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	blocked := false
	for !blocked {
		select {
		case got := <-removed:
			t.Fatalf("O's removal returned while the roster lock was held: %v", got.err)
		case <-ctx.Done():
			t.Fatal("O's removal never waited on the roster lock")
		case <-poll.C:
		}
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
				  AND $1 = ANY(pg_blocking_pids(pid))
			)
		`, holderPID).Scan(&blocked))
	}

	// An outside update of M's record commits while the removal waits.
	f.repo.mu.Lock()
	f.repo.setRecord(communities.RoleCollection, rosterModeratorKey, rosterModeratorUpdatedCID)
	f.repo.head = pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterBetweenHeadsRev}
	f.repo.mu.Unlock()
	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rosterBetweenHeadsRev,
		rosterModeratorUpdatedCID, roleRecord(rosterModerator, communities.RoleModerator))))

	require.NoError(t, holder.Rollback(), "the other roster action ends")
	select {
	case got := <-removed:
		require.NoError(t, got.err, "O's removal completes")
	case <-ctx.Done():
		t.Fatal("O's removal did not finish after the roster lock was released")
	}

	assert.Equal(t, []storedAction{
		actorlessRoleAction("grant-role", rosterCommunity, rosterModerator),
		moderatorRemovalAction(rosterOwner, false),
	}, f.actions(t), "the removal logs after the update it removed")
}

// A second open intent on one target record is a state conflict.
func TestCommunityActionDB_SecondOpenIntentOnATargetConflicts(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := postgres.NewCommunityActionRepository(db, db)
	ctx := context.Background()
	intent := moderation.CommunityIntent{
		ID: "intent-1", Action: moderation.ActionRemoveRole, ActorDID: rosterOwner,
		CommunityDID: rosterCommunity, SubjectDID: rosterModerator, TargetURI: rosterModeratorURI, Op: "delete",
		ReplacedCID: rosterModeratorCID, GuardCommitCID: rosterHeadZeroCID, GuardRev: rosterHeadZeroRev,
		IdempotencyKey: "remove-1", Fingerprint: "fingerprint", IdempotencyRetention: 24 * time.Hour,
		State: moderation.CommunityIntentOpen, CreatedAt: rosterClockStart, ExpiresAt: rosterClockStart.Add(10 * time.Minute),
	}
	require.NoError(t, store.SaveIntent(ctx, intent))
	intent.ID, intent.ActorDID, intent.IdempotencyKey = "intent-2", rosterAdmin, "admin-1"

	err := store.SaveIntent(ctx, intent)

	assert.ErrorIs(t, err, moderation.ErrStateConflict)
}

// Roster actions queued on one community's roster lock hold connections of
// the action pool, not the request pool, so the lock holder still gets the
// request-pool connections its authorization, roster and intent need. With a
// two-connection request pool, three concurrent removals queued behind one
// lock holder all return: one removes M, the others find M gone.
func TestCommunityActionDB_RosterLockWaitersDoNotStarveTheHolder(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requests := openTestPool(t, f.db, "roster-requests", 2)
	config := f.config
	config.Authorizer = rosterAuthorizer(requests, f.clock)
	config.Store = postgres.NewCommunityActionRepository(requests, openTestPool(t, f.db, "roster-actions", 3))
	service := moderation.NewCommunityActionService(config)

	holder, err := postgres.NewCommunityActionRepository(f.db, f.db).BeginAction(ctx, moderation.CommunityActionLock{
		CommunityDID: rosterCommunity, Roster: true,
	})
	require.NoError(t, err, "another roster action takes the roster lock")
	defer func() { _ = holder.Rollback() }()
	var holderPID int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'idle in transaction'
	`).Scan(&holderPID), "find the holder's backend")

	// The removals' own bound: a stalled holder fails at this deadline
	// rather than at the test's.
	actionCtx, cancelActions := context.WithTimeout(ctx, 10*time.Second)
	defer cancelActions()
	const removals = 3
	results := make(chan error, removals)
	for i := range removals {
		go func() {
			_, err := service.RemoveModerator(actionCtx, moderation.RemoveModeratorRequest{
				ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
				ExpectedVersion: rosterStartVersion, IdempotencyKey: fmt.Sprintf("remove-%d", i),
			})
			results <- err
		}()
	}
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for waiting := 0; waiting < 2; {
		select {
		case err := <-results:
			t.Fatalf("a removal returned while the roster lock was held: %v", err)
		case <-ctx.Done():
			t.Fatal("two removals never queued on the roster lock")
		case <-poll.C:
		}
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
			  AND $1 = ANY(pg_blocking_pids(pid))
		`, holderPID).Scan(&waiting))
	}

	require.NoError(t, holder.Rollback(), "the other roster action ends")
	var errs []error
	for range removals {
		select {
		case err := <-results:
			errs = append(errs, err)
		case <-ctx.Done():
			t.Fatal("the removals did not all return")
		}
	}
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		assert.ErrorIs(t, err, moderation.ErrRoleNotFound, "a later removal finds M gone, not a stalled pool")
	}
	assert.Equal(t, 1, succeeded, "one removal removes M; errors: %v", errs)
	assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
		"one remove-role action naming O")
}

// Idempotency records are scoped to the caller: after O removes M with key
// "remove-1", a moderator who may not remove moderators sends the same body
// with the same key and is refused, not served O's stored result.
func TestCommunityActionDB_AnotherCallersKeyDoesNotReplay(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	_, err := f.db.ExecContext(ctx, `
		INSERT INTO community_roles (community_did, subject_did, role, record_key, record_cid, record_created_at)
		VALUES ($1, $2, 'moderator', $3, $4, '2026-11-01T02:00:00Z')
	`, rosterCommunity, rosterSecondModerator, rosterSecondModeratorKey, rosterSecondModeratorCID)
	require.NoError(t, err)
	_, err = f.remove(ctx, rosterOwner, "remove-1")
	require.NoError(t, err, "O removes M")

	result, err := f.remove(ctx, rosterSecondModerator, "remove-1")

	assert.ErrorIs(t, err, moderation.ErrForbidden, "a non-owner is refused even with O's key and body")
	assert.Nil(t, result, "O's stored result is not served to another caller")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "only O's action")
}
