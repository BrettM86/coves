//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community post actions racing role removals and each other against real
// Postgres. Every interleaving is forced with the pipeline's hooks and
// channels, and a request waiting on another's transaction is observed in
// pg_stat_activity.

type postRaceOutcome struct {
	result *moderation.CommunityActionResult
	err    error
}

// postRaceIdleBackend is the one backend in the test's private database idle
// inside a transaction: a request paused with its action locks held.
func postRaceIdleBackend(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var pid int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'idle in transaction'
	`).Scan(&pid), "find the paused request's backend")
	return pid
}

// awaitWaitBehind returns once some backend waits on a lock held by
// holderPID. It fails the test if the waiting request returns first.
func awaitWaitBehind(ctx context.Context, t *testing.T, db *sql.DB, holderPID int, returned <-chan postRaceOutcome, what string) {
	t.Helper()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for blocked := false; !blocked; {
		select {
		case got := <-returned:
			t.Fatalf("%s returned while the post action held its locks: %v", what, got.err)
		case <-ctx.Done():
			t.Fatalf("%s never waited on a lock held by the post action", what)
		case <-poll.C:
		}
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(pid))
			)
		`, holderPID).Scan(&blocked), "read the lock wait")
	}
}

// L1: a moderator's post removal racing the owner's removal of that
// moderator's role. (a) M's removal is authorized and paused before any lock
// while O removes M and returns; when M resumes, the authorization recheck
// under the locks refuses it and nothing about the post is written. (b) M2's
// removal holds its locks, paused before its PDS write; O's removal of M2,
// started then, waits on those locks, so M2's removal commits with one action
// naming M2 and the role removal completes afterwards.
func TestCommunityPostActionsRace_RoleRemoval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(ctx context.Context, t *testing.T, f *communityPostDBFixture, post seededCommunityPost)
	}{
		{
			name: "removal authorized before the role removal is refused by the recheck",
			run: func(ctx context.Context, t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				paused := make(chan struct{})
				resume := make(chan struct{})
				removing := f.serviceWithHooks(moderation.CommunityActionHooks{
					AfterAuthorize: func(context.Context) {
						close(paused)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					},
				})
				removed := make(chan postRaceOutcome, 1)
				go func() {
					result, err := removing.RemovePost(ctx, moderation.RemovePostRequest{
						ActorDID: rosterModerator, PostURI: post.URI, Code: "spam", Reason: postDBRemovalReason,
						ExpectedVersion: postDBSeededVersion, IdempotencyKey: "remove-post-m",
					})
					removed <- postRaceOutcome{result, err}
				}()
				select {
				case <-paused:
				case got := <-removed:
					t.Fatalf("M's removal returned before pausing after authorization: %v", got.err)
				case <-ctx.Done():
					t.Fatal("M's removal never reached the after-authorize pause")
				}

				_, err := f.remove(ctx, rosterOwner, "remove-role-m")
				require.NoError(t, err, "O removes M while M's post removal is paused")
				close(resume)

				select {
				case got := <-removed:
					assert.ErrorIs(t, got.err, moderation.ErrForbidden, "the recheck under the locks refuses M")
					assert.Nil(t, got.result, "M's removal has no result")
				case <-ctx.Done():
					t.Fatal("M's removal did not finish after resuming")
				}

				batches := f.repo.batchesSnapshot()
				if assert.Len(t, batches, 1, "one PDS batch: O's role delete") {
					assert.Equal(t, []pds.Write{{Op: pds.WriteOpDelete, Collection: communities.RoleCollection, RKey: rosterModeratorKey}},
						batches[0].writes, "the one batch deletes M's role record")
				}
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
					"only O's intent; none for M's removal")
				assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
					"one remove-role action naming O; no post action")
				assert.Nil(t, f.standRecord(posts.RemovalCollection, post.SubjectRkey), "no removal record stands")
				assert.NotNil(t, f.standRecord(posts.AcceptanceCollection, post.SubjectRkey), "the acceptance still stands")
				assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "the post stays accepted")
				assert.EqualValues(t, 1, f.postVersion(t, post.URI), "the post's community version is unchanged")
			},
		},
		{
			name: "role removal waits on the locks of a removal already writing",
			run: func(ctx context.Context, t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				// M2's role record stands in the community repo with its
				// rev gate at head zero, so O can remove it.
				f.repo.mu.Lock()
				f.repo.setRecord(communities.RoleCollection, rosterSecondModeratorKey, rosterSecondModeratorCID)
				f.repo.mu.Unlock()
				_, err := f.db.ExecContext(ctx, `INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`,
					rosterSecondModeratorURI, rosterHeadZeroRev)
				require.NoError(t, err)

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
				removed := make(chan postRaceOutcome, 1)
				go func() {
					result, err := removing.RemovePost(ctx, moderation.RemovePostRequest{
						ActorDID: rosterSecondModerator, PostURI: post.URI, Code: "spam", Reason: postDBRemovalReason,
						ExpectedVersion: postDBSeededVersion, IdempotencyKey: "remove-post-m2",
					})
					removed <- postRaceOutcome{result, err}
				}()
				select {
				case <-paused:
				case got := <-removed:
					t.Fatalf("M2's removal returned before pausing with its locks held: %v", got.err)
				case <-ctx.Done():
					t.Fatal("M2's removal never reached the before-write pause")
				}
				holderPID := postRaceIdleBackend(ctx, t, f.db)

				roleRemoved := make(chan postRaceOutcome, 1)
				go func() {
					result, err := f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
						ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterSecondModerator,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "remove-role-m2",
					})
					roleRemoved <- postRaceOutcome{result, err}
				}()
				awaitWaitBehind(ctx, t, f.db, holderPID, roleRemoved, "O's removal of M2")

				close(resume)
				select {
				case got := <-removed:
					require.NoError(t, got.err, "M2's removal commits once resumed")
				case <-ctx.Done():
					t.Fatal("M2's removal did not finish after resuming")
				}
				select {
				case got := <-roleRemoved:
					require.NoError(t, got.err, "O's removal of M2 completes after M2's removal")
				case <-ctx.Done():
					t.Fatal("O's removal of M2 did not finish after M2's removal committed")
				}

				batches := f.repo.batchesSnapshot()
				if assert.Len(t, batches, 2, "two PDS batches: M2's removal, then O's role delete") {
					assert.Contains(t, batches[0].writes,
						pds.Write{Op: pds.WriteOpDelete, Collection: posts.AcceptanceCollection, RKey: post.SubjectRkey},
						"the first batch is M2's removal: it deletes the acceptance")
					assert.Equal(t, []pds.Write{{Op: pds.WriteOpDelete, Collection: communities.RoleCollection, RKey: rosterSecondModeratorKey}},
						batches[1].writes, "the second batch deletes M2's role record")
				}
				assert.Equal(t, []storedPostAction{
					localPostAction("remove", rosterSecondModerator, false, post.URI, postDBReasonSpam),
				}, f.postActions(t), "one remove action naming M2")
				assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "the post is removed")
				assert.EqualValues(t, 2, f.postVersion(t, post.URI), "the post's community version advanced once")
				var m2Indexed int
				require.NoError(t, f.db.QueryRow(`
					SELECT COUNT(*) FROM community_roles WHERE community_did = $1 AND subject_did = $2
				`, rosterCommunity, rosterSecondModerator).Scan(&m2Indexed))
				assert.Zero(t, m2Indexed, "M2's role row is gone")
				assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once, by the role removal")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			tc.run(ctx, t, f, post)
		})
	}
}

// postRaceRequestKey labels a request's context so the head-read rendezvous
// holds each request at its first community repo head read only.
type postRaceRequestKey struct{}

// headReadRendezvous is the community repo as one request sees it: its first
// head read (the one between the removal count and the intent it stores)
// waits until the other request has read the head too, or is waiting on a
// lock this request holds.
type headReadRendezvous struct {
	posts.CommunityRepo
	arrive func(ctx context.Context)
}

func (r headReadRendezvous) GetLatestCommit(ctx context.Context) (*pds.LatestCommit, error) {
	head, err := r.CommunityRepo.GetLatestCommit(ctx)
	r.arrive(ctx)
	return head, err
}

// L2: with a per-community limit of one removal, two moderators remove two
// different posts at once. Both are authorized and released together; the
// count and the intent that reserves a removal against it serialize per
// community, so exactly one removal is written and the other is rate limited.
func TestCommunityPostActionsRace_PerCommunityRateLimit(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	alpha := f.seedAcceptedPost(t, postDBAlpha)
	beta := f.seedAcceptedPost(t, postDBSeedFor("3kpostbeta1"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Both requests pass authorization before either goes on.
	var authorizedOnce sync.WaitGroup
	authorizedOnce.Add(2)
	release := make(chan struct{})
	// The first head read of each request waits for the other's, or for the
	// other to wait on a lock.
	var mu sync.Mutex
	seen := map[string]bool{}
	bothRead := make(chan struct{})
	arrive := func(ctx context.Context) {
		label, _ := ctx.Value(postRaceRequestKey{}).(string)
		mu.Lock()
		if label == "" || seen[label] {
			mu.Unlock()
			return
		}
		seen[label] = true
		if len(seen) == 2 {
			close(bothRead)
		}
		mu.Unlock()
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-bothRead:
				return
			case <-ctx.Done():
				return
			case <-poll.C:
			}
			var waiting bool
			if err := f.db.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
				)
			`).Scan(&waiting); err == nil && waiting {
				return
			}
		}
	}
	config := f.config
	config.RemovalLimits = moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 1, Window: time.Hour}
	config.Hooks = moderation.CommunityActionHooks{
		AfterAuthorize: func(ctx context.Context) {
			authorizedOnce.Done()
			select {
			case <-release:
			case <-ctx.Done():
			}
		},
	}
	config.Repos = func(ctx context.Context, communityDID string) (posts.CommunityRepo, error) {
		repo, err := f.factory.open(ctx, communityDID)
		if err != nil {
			return nil, err
		}
		return headReadRendezvous{CommunityRepo: repo, arrive: arrive}, nil
	}
	service := moderation.NewCommunityActionService(config)

	type request struct {
		actorDID string
		post     seededCommunityPost
	}
	requests := []request{{rosterModerator, alpha}, {rosterSecondModerator, beta}}
	outcomes := make([]chan postRaceOutcome, len(requests))
	for i, req := range requests {
		outcomes[i] = make(chan postRaceOutcome, 1)
		go func() {
			requestCtx := context.WithValue(ctx, postRaceRequestKey{}, req.actorDID)
			result, err := service.RemovePost(requestCtx, moderation.RemovePostRequest{
				ActorDID: req.actorDID, PostURI: req.post.URI, Code: "spam", Reason: postDBRemovalReason,
				ExpectedVersion: postDBSeededVersion, IdempotencyKey: "remove-post-" + req.post.SubjectRkey,
			})
			outcomes[i] <- postRaceOutcome{result, err}
		}()
	}
	authorized := make(chan struct{})
	go func() {
		authorizedOnce.Wait()
		close(authorized)
	}()
	select {
	case <-authorized:
	case <-ctx.Done():
		t.Fatal("both removals never passed authorization")
	}
	close(release)

	got := make([]postRaceOutcome, len(requests))
	for i := range requests {
		select {
		case got[i] = <-outcomes[i]:
		case <-ctx.Done():
			t.Fatalf("removal %d did not return", i+1)
		}
	}

	winner, loser := -1, -1
	for i, outcome := range got {
		if outcome.err == nil {
			winner = i
		} else {
			assert.ErrorIs(t, outcome.err, moderation.ErrRateLimited, "the other removal is rate limited")
			loser = i
		}
	}
	require.True(t, winner >= 0 && loser >= 0, "exactly one removal succeeds: %v, %v", got[0].err, got[1].err)
	wantActions := map[int][]storedPostAction{
		0: {localPostAction("remove", rosterModerator, false, postAlphaURI, postDBReasonSpam)},
		1: {localPostAction("remove", rosterSecondModerator, false, "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostbeta1", postDBReasonSpam)},
	}
	assert.Equal(t, wantActions[winner], f.postActions(t), "exactly one remove action, naming the winner")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS batch: the winner's removal")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"only the winner's intent; none for the rate-limited removal")
	assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, requests[winner].post.URI).Status, "the winner's post is removed")
	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, requests[loser].post.URI).Status, "the other post stays accepted")
	assert.EqualValues(t, 1, f.postVersion(t, requests[loser].post.URI), "the other post's version is unchanged")
}

// An open removal intent counts toward the per-community limit: M's removal
// fails at the PDS before committing and leaves its intent open, so with a
// per-community limit of one, M2's removal of another post is rate limited and
// writes nothing.
func TestCommunityPostActionsDB_OpenRemovalIntentCountsTowardTheCommunityLimit(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	service := f.serviceWithLimits(moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 1, Window: time.Hour})
	alpha := f.seedAcceptedPost(t, postDBAlpha)
	beta := f.seedAcceptedPost(t, postDBSeedFor("3kpostbeta1"))
	f.repo.mu.Lock()
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	f.repo.mu.Unlock()
	_, err := removePostWith(service, rosterModerator, alpha.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-alpha")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: M's write fails before committing")
	require.Equal(t, 1, f.openIntents(t), "fixture: M's removal intent stays open")

	_, err = removePostWith(service, rosterSecondModerator, beta.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-beta")

	assert.ErrorIs(t, err, moderation.ErrRateLimited, "M's open intent fills the community's one removal")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "no batch for M2's removal; only M's failed one")
	assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "no intent for M2's removal")
	assert.Empty(t, f.postActions(t), "no action is written")
	assert.Nil(t, f.standRecord(posts.RemovalCollection, beta.SubjectRkey), "no removal of beta stands")
	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, beta.URI).Status, "beta stays accepted")
	assert.EqualValues(t, 1, f.postVersion(t, beta.URI), "beta's version is unchanged")
}
