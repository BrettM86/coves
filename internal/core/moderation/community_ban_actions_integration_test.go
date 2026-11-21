//go:build integration

package moderation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I1: a moderator bans an account with no role, with a reason and an expiry.
// Before the response returns, the ban index carries the reason and expiry,
// the account's next post, comment and vote in the community are refused by
// each production path, one community-scope remove action names the
// moderator, carries the expiry and the moderator-discretion reason (never
// the free-text ban reason), and the (community, account) version advanced
// once.
func TestCommunityBanActionsDB_ModeratorBansAnAccount(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	expiry := banDBFarExpiry

	result, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")

	require.NoError(t, err)
	require.NotNil(t, result, "the ban returns its result")
	assert.Equal(t, banSubjectURI, result.RecordURI)
	assert.EqualValues(t, 1, result.Version)
	assert.False(t, result.AlreadyApplied)
	assert.Equal(t, &communities.CommunityBan{
		CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
		RecordCID: banWrittenCID, Reason: banReason, ExpiresAt: &expiry,
	}, f.inForceBan(t, banSubject, rosterClockStart), "the ban index carries the reason and expiry")

	assert.Equal(t, posts.DecisionAuthorBanned, f.decidePost(t, nil, banSubject, "3kbannedpost1").Code,
		"the account's next post in the community is refused")
	account, err := f.createComment(t, banSubject, post)
	assert.ErrorIs(t, err, comments.ErrBanned, "the account's next comment in the community is refused")
	assert.Empty(t, account.callsSnapshot(), "the refused comment never reaches the account's PDS")
	account, err = f.createVote(t, banSubject, post)
	assert.ErrorIs(t, err, votes.ErrBanned, "the account's next vote in the community is refused")
	assert.Empty(t, account.callsSnapshot(), "the refused vote never reaches the account's PDS")

	actions := f.banActions(t, banSubject)
	assert.Equal(t, []storedBanAction{localBanAction("remove", rosterModerator, false, &expiry, rosterClockStart)},
		withoutIDs(actions), "one community remove action naming M")
	if assert.Len(t, actions, 1) {
		assert.Equal(t, result.ActionID, actions[0].ID, "the result names the action")
	}
	assert.NotContains(t, f.banActionRowsText(t, banSubject), banReason, "the free-text ban reason is not in the action")
	assert.EqualValues(t, 1, f.accountVersion(t, banSubject), "the account's version advanced once")
}

// I2 with Round 1 #7: after M unbans the account, the ban row is gone and a
// post by the account is accepted; a comment and a vote it wrote straight to
// its PDS during the ban stay hidden (the vote uncounted), while a new comment
// and vote after the unban are served and counted (the new vote is on a second
// post, so it does not replace the hidden one). One restore action names M and
// reverses the
// ban action.
func TestCommunityBanActionsDB_UnbanLiftsTheBanAndKeepsDuringBanHides(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	secondPost := f.seedAcceptedPost(t, postDBSeedFor("3kpostsecond"))
	expiry := banDBFarExpiry
	banned, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
	require.NoError(t, err, "M bans the account")
	require.NotNil(t, banned)

	duringComment := f.indexCommentBy(t, banSubject, "3kduringcomment", post)
	duringVote := f.indexUpvoteBy(t, banSubject, "3kduringvote", post)
	require.Equal(t, comments.HideReasonCommunityBan, f.hideReason(t, "comments", duringComment),
		"the comment written during the ban is hidden")
	require.Equal(t, comments.HideReasonCommunityBan, f.hideReason(t, "votes", duringVote),
		"the vote written during the ban is hidden")

	unbannedAt := rosterClockStart.Add(time.Minute)
	f.clock.Set(unbannedAt)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	unbanned, err := f.unban(rosterModerator, banSubject, 1, "unban-1")

	require.NoError(t, err)
	require.NotNil(t, unbanned, "the unban returns its result")
	assert.Equal(t, banSubjectURI, unbanned.RecordURI)
	assert.EqualValues(t, 2, unbanned.Version)
	assert.False(t, unbanned.AlreadyApplied)
	assert.False(t, f.banRowExists(t, banSubject), "the ban row is gone")
	assert.False(t, f.repo.hasRecord(communities.BanCollection, banSubjectKey), "the ban record is deleted")

	assert.True(t, f.decidePost(t, nil, banSubject, "3kunbannedpost").Admitted(), "a post by the account is accepted")
	account, err := f.createComment(t, banSubject, post)
	assert.NotErrorIs(t, err, comments.ErrBanned, "a comment by the account is no longer refused")
	assert.NotEmpty(t, account.callsSnapshot(), "the comment write reaches the account's PDS")
	account, err = f.createVote(t, banSubject, secondPost)
	assert.NotErrorIs(t, err, votes.ErrBanned, "a vote by the account is no longer refused")
	assert.NotEmpty(t, account.callsSnapshot(), "the vote write reaches the account's PDS")

	assert.Equal(t, comments.HideReasonCommunityBan, f.hideReason(t, "comments", duringComment),
		"the comment written during the ban stays hidden")
	assert.Equal(t, comments.HideReasonCommunityBan, f.hideReason(t, "votes", duringVote),
		"the vote written during the ban stays hidden")
	freshComment := f.indexCommentBy(t, banSubject, "3kfreshcomment", post)
	freshVote := f.indexUpvoteBy(t, banSubject, "3kfreshvote", secondPost)
	assert.Empty(t, f.hideReason(t, "comments", freshComment), "the new comment is not hidden")
	assert.Empty(t, f.hideReason(t, "votes", freshVote), "the new vote is not hidden")
	assert.True(t, f.threadServes(t, post.URI, freshComment), "the new comment is served in the thread")
	assert.Equal(t, comments.HideReasonCommunityBan, f.hideReason(t, "votes", duringVote),
		"the vote written during the ban is still hidden after the new vote")
	_, duringUpvotes := f.postCounts(t, post.URI)
	assert.Equal(t, 0, duringUpvotes, "the vote written during the ban is not counted")
	_, freshUpvotes := f.postCounts(t, secondPost.URI)
	assert.Equal(t, 1, freshUpvotes, "the new vote is counted")

	actions := f.banActions(t, banSubject)
	assert.Equal(t, []storedBanAction{
		localBanAction("remove", rosterModerator, false, &expiry, rosterClockStart),
		localBanAction("restore", rosterModerator, false, nil, unbannedAt),
	}, withoutIDs(actions), "the ban's remove and one restore naming M")
	if assert.Len(t, actions, 2) {
		assert.Equal(t, actions[0].ID, actions[1].ReversesActionID, "the restore reverses the ban action")
		assert.Equal(t, unbanned.ActionID, actions[1].ID, "the result names the restore")
	}
}

// I3: a ban of the invitee and the invitee's acceptance of a moderator
// invitation race, each order as a row: the first is held at BeforeWrite
// with its locks taken while the second runs and waits on them. The end
// state never has a role holder with a ban in force: ban first refuses the
// acceptance (ErrInviteeBanned); acceptance first refuses the ban
// (ErrSubjectHoldsRole).
// banFixtureRoles is the roster newCommunityBanDBFixture seeds: the invitation
// fixture's owner and moderator plus the post fixture's second moderator.
func banFixtureRoles() map[string]indexedRole {
	roles := seededRoles()
	roles[rosterSecondModerator] = indexedRole{Role: communities.RoleModerator, CID: rosterSecondModeratorCID}
	return roles
}

// removeOrRestoreActions keeps the ban and unban actions, dropping role
// actions (an acceptance's grant-role) about the same account.
func removeOrRestoreActions(actions []storedBanAction) []storedBanAction {
	out := []storedBanAction{}
	for _, action := range actions {
		if action.Action == "remove" || action.Action == "restore" {
			out = append(out, action)
		}
	}
	return out
}

func TestCommunityBanActionsDB_BanAndAcceptanceRace(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	banCall := func(ctx context.Context, service *moderation.CommunityActionService) (*moderation.CommunityActionResult, error) {
		return service.BanCommunityAccount(ctx, moderation.BanCommunityAccountRequest{
			ActorDID: rosterModerator, CommunityDID: rosterCommunity, SubjectDID: banSubject,
			Reason: banReason, ExpiresAt: &expiry, ExpectedVersion: 0, IdempotencyKey: "ban-1",
		})
	}
	acceptCall := func(ctx context.Context, service *moderation.CommunityActionService) (*moderation.CommunityActionResult, error) {
		return service.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
			ActorDID: banSubject, InvitationID: rosterModeratorInvitationID,
			ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-1",
		})
	}
	type call func(context.Context, *moderation.CommunityActionService) (*moderation.CommunityActionResult, error)

	tests := []struct {
		name          string
		first, second call
		check         func(t *testing.T, f *communityBanDBFixture, second lockOutcome)
	}{
		{
			name:  "ban first: the acceptance is refused",
			first: banCall, second: acceptCall,
			check: func(t *testing.T, f *communityBanDBFixture, second lockOutcome) {
				assert.ErrorIs(t, second.err, moderation.ErrInviteeBanned)
				assert.Equal(t, banFixtureRoles(), f.indexedRoles(t), "the invitee holds no role")
				assert.NotNil(t, f.inForceBan(t, banSubject, rosterClockStart), "the ban is in force")
				assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
			},
		},
		{
			name:  "acceptance first: the ban is refused",
			first: acceptCall, second: banCall,
			check: func(t *testing.T, f *communityBanDBFixture, second lockOutcome) {
				assert.ErrorIs(t, second.err, moderation.ErrSubjectHoldsRole)
				want := banFixtureRoles()
				want[banSubject] = indexedRole{Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID}
				assert.Equal(t, want, f.indexedRoles(t), "the invitee is a moderator")
				assert.False(t, f.banRowExists(t, banSubject), "no ban is indexed")
				assert.Empty(t, removeOrRestoreActions(f.banActions(t, banSubject)), "no ban action")
				assert.False(t, f.repo.hasRecord(communities.BanCollection, banSubjectKey), "no ban record is written")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityBanDBFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, banSubject, communities.RoleModerator, rosterOwner, false))

			held, paused, resume := f.pauseAtBeforeWrite(ctx)
			first := make(chan lockOutcome, 1)
			go func() {
				result, err := tt.first(ctx, held)
				first <- lockOutcome{result, err}
			}()
			select {
			case <-paused:
			case got := <-first:
				t.Fatalf("the first call returned before pausing with its locks held: %v", got.err)
			case <-ctx.Done():
				t.Fatal("the first call never reached the before-write pause")
			}
			heldPID := f.pausedActionPID(ctx, t)

			second := make(chan lockOutcome, 1)
			go func() {
				result, err := tt.second(ctx, f.service)
				second <- lockOutcome{result, err}
			}()
			f.awaitAdvisoryWaiter(ctx, t, heldPID, second, "the second call")
			resume()

			select {
			case got := <-first:
				require.NoError(t, got.err, "the first call completes once resumed")
			case <-ctx.Done():
				t.Fatal("the first call did not finish after resuming")
			}
			var got lockOutcome
			select {
			case got = <-second:
			case <-ctx.Done():
				t.Fatal("the second call did not finish")
			}
			assert.Nil(t, got.result, "the refused call has no result")
			tt.check(t, f, got)
		})
	}
}

// I3 with Round 1 #6: the ban takes the subject's ban-subject lock. A test
// transaction holding only pg_advisory_xact_lock on the subject's ban-subject
// key blocks the ban before its write; once it ends, the ban completes.
func TestCommunityBanActionsDB_BanWaitsOnTheSubjectsBanSubjectLock(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	holder, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var holderPID int
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, banSubject)
	require.NoError(t, err, "the test transaction takes the subject's ban-subject lock")

	reachedWrite := make(chan struct{})
	var once sync.Once
	service := f.serviceWithHooks(moderation.CommunityActionHooks{
		BeforeWrite: func(context.Context) { once.Do(func() { close(reachedWrite) }) },
	})
	expiry := banDBFarExpiry
	returned := make(chan lockOutcome, 1)
	go func() {
		result, err := banWith(service, rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
		returned <- lockOutcome{result, err}
	}()

	f.awaitAdvisoryWaiter(ctx, t, holderPID, returned, "the ban")
	select {
	case <-reachedWrite:
		t.Error("the ban reached its write while another transaction held the subject's ban-subject lock")
	default:
	}
	assert.Empty(t, f.repo.batchesSnapshot(), "nothing is written while the lock is held")

	require.NoError(t, holder.Rollback(), "the test transaction ends")
	select {
	case got := <-returned:
		require.NoError(t, got.err, "the ban completes once the lock is free")
		require.NotNil(t, got.result)
	case <-ctx.Done():
		t.Fatal("the ban did not finish after the lock was released")
	}
	assert.NotNil(t, f.inForceBan(t, banSubject, rosterClockStart), "the ban is in force")
}

// I4: a trusted aggregator banned in the community still has its post
// accepted, while the same post decided as an ordinary account is refused.
func TestCommunityBanActionsDB_TrustedAggregatorBannedStillPosts(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	const aggregator = "did:plc:bantrustedagg"
	f.repo.mu.Lock()
	f.repo.writeCIDs[communities.BanCollection+"/"+communities.BanRecordKey(aggregator)] = banWrittenCID
	f.repo.mu.Unlock()
	expiry := banDBFarExpiry

	result, err := f.ban(rosterModerator, aggregator, banReason, &expiry, 0, "ban-aggregator")

	require.NoError(t, err)
	require.NotNil(t, result, "the ban returns its result")
	require.NotNil(t, f.inForceBan(t, aggregator, time.Now()), "the aggregator's ban is in force")
	assert.Equal(t, posts.DecisionAuthorBanned, f.decidePost(t, nil, aggregator, "3kaggpostuser").Code,
		"control: the banned account's post decided as an ordinary account is refused")
	assert.True(t, f.decidePost(t, map[string]bool{aggregator: true}, aggregator, "3kaggposttrust").Admitted(),
		"the trusted aggregator's post is accepted")
}

// I5 with Round 1 #5: the ban counts in SQL. Seeded in rosteralpha (window
// one hour before attempt time): M's two local ban actions in the window, M2's
// one, an admin-acting ban, M's ban older than the window, M's ban in another
// community, an inherited ban with no actor (a ban written outside Coves), M's
// post removal and comment removal; M's open ban intent within
// TTL, M's open ban intent whose TTL passed unresolved, M's completed intent
// beside a logged ban, a failed ban intent and an open ban intent older than
// the window. RecentCommunityBans counts M: 2 actions + 2 open intents; the
// community: those plus M2's ban. RecentCommunityRemovals counts the two
// removals only: open ban intents are not removals.
func TestCommunityBanActionsDB_RecentBanCountsInSQL(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	insertRosterCommunity(t, f.db, postRateOtherCommunity, "rosterbeta.test", true)
	start := rosterClockStart
	attemptAt := start.Add(30 * time.Minute)
	since := attemptAt.Add(-time.Hour)

	banAction := func(id, community, actor string, adminActing bool, subject string, createdAt time.Time) {
		t.Helper()
		_, err := f.db.Exec(`
			INSERT INTO moderation_actions (id, actor_did, authority_did, scope_kind, scope_community_did,
				subject_uri, subject_kind, action, reason, origin, expires_at, admin_acting, created_at)
			VALUES ($1, $2, $3, 'community', $3, 'at://' || $4, 'account', 'remove', $5, 'local', $6, $7, $8)
		`, id, actor, community, subject, banDBReasonDiscretion, banDBFarExpiry, adminActing, createdAt)
		require.NoError(t, err, "fixture: ban action %s", id)
	}
	removalAction := func(id, action, collection, subjectURI string, createdAt time.Time) {
		t.Helper()
		_, err := f.db.Exec(`
			INSERT INTO moderation_actions (id, actor_did, authority_did, scope_kind, scope_community_did,
				subject_uri, subject_collection, subject_community_did, action, reason, origin, admin_acting, created_at)
			VALUES ($1, $2, $3, 'community', $3, $4, $5, $3, $6, $7, 'local', false, $8)
		`, id, rosterModerator, rosterCommunity, subjectURI, collection, action, postDBReasonSpam, createdAt)
		require.NoError(t, err, "fixture: removal action %s", id)
	}
	banIntent := func(id, actor, subject, state string, createdAt, expiresAt time.Time, actionID string) {
		t.Helper()
		var commitRev, finishedAt, action any
		if state == "completed" {
			commitRev, action = rosterHeadOneRev, actionID
		}
		if state != "open" {
			finishedAt = createdAt.Add(time.Second)
		}
		_, err := f.db.Exec(`
			INSERT INTO community_write_intents (id, action, actor_did, admin_acting, community_did, subject_did,
				target_uri, op, record, guard_commit_cid, guard_rev, idempotency_key, fingerprint,
				idempotency_retention_microseconds, state, commit_rev, action_id, created_at, expires_at, finished_at)
			VALUES ($1, 'remove', $2, false, $3, $4,
				'at://' || $3 || '/social.coves.moderation.ban/' || $1, 'create', '{}'::jsonb, $5, $6, $1, $1,
				86400000000, $7, $8, $9, $10, $11, $12)
		`, id, actor, rosterCommunity, subject, rosterHeadZeroCID, rosterHeadZeroRev,
			state, commitRev, action, createdAt, expiresAt, finishedAt)
		require.NoError(t, err, "fixture: ban intent %s", id)
	}

	banAction("ban-m-a", rosterCommunity, rosterModerator, false, "did:plc:bancounta", start)
	banAction("ban-m-b", rosterCommunity, rosterModerator, false, "did:plc:bancountb", start.Add(time.Minute))
	banAction("ban-m2-c", rosterCommunity, rosterSecondModerator, false, "did:plc:bancountc", start.Add(2*time.Minute))
	banAction("ban-admin-d", rosterCommunity, rosterAdmin, true, "did:plc:bancountd", start.Add(3*time.Minute))
	banAction("ban-m-old", rosterCommunity, rosterModerator, false, "did:plc:bancountold", start.Add(-2*time.Hour))
	banAction("ban-m-beta", postRateOtherCommunity, rosterModerator, false, "did:plc:bancountbeta", start)
	_, err := f.db.Exec(`
		INSERT INTO moderation_actions (id, actor_did, authority_did, scope_kind, scope_community_did,
			subject_uri, subject_kind, action, reason, origin, admin_acting, created_at)
		VALUES ('ban-inherited-e', '', $1, 'community', $1, 'at://did:plc:bancounte', 'account', 'remove', $2,
			'inherited', false, $3)
	`, rosterCommunity, banDBReasonDiscretion, start.Add(4*time.Minute))
	require.NoError(t, err, "fixture: inherited ban action")
	removalAction("removal-post", "remove", moderation.PostV2Collection,
		"at://"+postDBAuthor+"/"+moderation.PostV2Collection+"/3kcountpost", start.Add(4*time.Minute))
	removalAction("removal-comment", "apply-removal", moderation.CommentCollection,
		"at://"+postDBAuthor+"/"+moderation.CommentCollection+"/3kcountcomment", start.Add(5*time.Minute))

	banIntent("intent-m-a", rosterModerator, "did:plc:bancounta", "completed", start, start.Add(10*time.Minute), "ban-m-a")
	banIntent("intent-m-ttl-passed", rosterModerator, "did:plc:bancountg", "open", start.Add(6*time.Minute), start.Add(16*time.Minute), "")
	banIntent("intent-m-open", rosterModerator, "did:plc:bancounth", "open", start.Add(25*time.Minute), start.Add(35*time.Minute), "")
	banIntent("intent-m-failed", rosterModerator, "did:plc:bancounti", "failed", start.Add(7*time.Minute), start.Add(17*time.Minute), "")
	banIntent("intent-m-old", rosterModerator, "did:plc:bancountj", "open", start.Add(-2*time.Hour), attemptAt.Add(time.Hour), "")

	tx, err := f.config.Store.BeginAction(ctx, moderation.CommunityActionLock{CommunityDID: rosterCommunity, Roster: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	byActor, byCommunity, err := tx.RecentCommunityBans(ctx, rosterCommunity, rosterModerator, since)
	require.NoError(t, err)
	assert.Equal(t, 4, byActor, "M's two logged bans and two unresolved ban intents in the window")
	assert.Equal(t, 5, byCommunity, "M's four plus M2's logged ban; the inherited ban does not count")

	removalsByActor, removalsByCommunity, err := tx.RecentCommunityRemovals(ctx, rosterCommunity, rosterModerator, since)
	require.NoError(t, err)
	assert.Equal(t, 2, removalsByActor, "M's post and comment removals; open ban intents are not removals")
	assert.Equal(t, 2, removalsByCommunity, "the community's post and comment removals only")
}

// I6: the list query. rosteralpha has one in-force ban and one lapsed ban,
// and another community bans the same account: a moderator's list serves
// both of rosteralpha's with reason, expiry and active against the service
// clock; limit 1 pages through them with a cursor; a non-moderator is refused
// by the production authorizer.
func TestCommunityBanActionsDB_ListBans(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	ctx := context.Background()
	insertRosterCommunity(t, f.db, postRateOtherCommunity, "rosterbeta.test", true)
	const (
		listedActive = "did:plc:banlistactive"
		listedLapsed = "did:plc:banlistlapsed"
	)
	activeExpiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	lapsedExpiry := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, ban := range []communities.CommunityBan{
		{
			CommunityDID: rosterCommunity, SubjectDID: listedActive, RecordKey: "3kbanlistactive",
			RecordCID: "bafyreibanlistactive", Reason: "Spamming invite links", ExpiresAt: &activeExpiry,
		},
		{
			CommunityDID: rosterCommunity, SubjectDID: listedLapsed, RecordKey: "3kbanlistlapsed",
			RecordCID: "bafyreibanlistlapsed", Reason: "Brigading", ExpiresAt: &lapsedExpiry,
		},
		{
			CommunityDID: postRateOtherCommunity, SubjectDID: listedActive, RecordKey: "3kbanlistbeta",
			RecordCID: "bafyreibanlistbeta", Reason: "Elsewhere",
		},
	} {
		require.NoError(t, f.bans.UpsertCommunityBan(ctx, ban), "fixture: index ban %s", ban.RecordKey)
	}
	want := map[string]moderation.CommunityBanEntry{
		listedActive: {
			SubjectDID: listedActive, URI: "at://did:plc:rosteralpha/social.coves.moderation.ban/3kbanlistactive",
			CID: "bafyreibanlistactive", Reason: "Spamming invite links", ExpiresAt: &activeExpiry, Active: true,
		},
		listedLapsed: {
			SubjectDID: listedLapsed, URI: "at://did:plc:rosteralpha/social.coves.moderation.ban/3kbanlistlapsed",
			CID: "bafyreibanlistlapsed", Reason: "Brigading", ExpiresAt: &lapsedExpiry, Active: false,
		},
	}
	list := func(t *testing.T, actorDID string, limit int, cursor string) (*moderation.CommunityBanPage, error) {
		t.Helper()
		return f.service.ListCommunityBans(ctx, moderation.ListCommunityBansRequest{
			ActorDID: actorDID, CommunityDID: rosterCommunity, Limit: limit, Cursor: cursor,
		})
	}
	// byDID keys the page's entries by subject, with IndexedAt checked set
	// and then cleared, CreatedAt cleared (the index stores none) and expiries
	// in UTC.
	byDID := func(t *testing.T, entries []moderation.CommunityBanEntry) map[string]moderation.CommunityBanEntry {
		t.Helper()
		out := map[string]moderation.CommunityBanEntry{}
		for _, entry := range entries {
			assert.False(t, entry.IndexedAt.IsZero(), "%s carries its index time", entry.SubjectDID)
			entry.IndexedAt, entry.CreatedAt = time.Time{}, nil
			if entry.ExpiresAt != nil {
				utc := entry.ExpiresAt.UTC()
				entry.ExpiresAt = &utc
			}
			out[entry.SubjectDID] = entry
		}
		return out
	}

	t.Run("a moderator lists both of the community's bans", func(t *testing.T) {
		page, err := list(t, rosterModerator, 50, "")
		require.NoError(t, err)
		require.NotNil(t, page, "the list returns a page")
		assert.Equal(t, want, byDID(t, page.Bans))
	})

	t.Run("limit 1 pages through the bans with a cursor", func(t *testing.T) {
		first, err := list(t, rosterModerator, 1, "")
		require.NoError(t, err)
		require.NotNil(t, first, "the first page")
		require.Len(t, first.Bans, 1, "one ban on the first page")
		require.NotEmpty(t, first.Cursor, "the first page has a cursor")
		second, err := list(t, rosterModerator, 1, first.Cursor)
		require.NoError(t, err)
		require.NotNil(t, second, "the second page")
		require.Len(t, second.Bans, 1, "one ban on the second page")
		got := byDID(t, append(append([]moderation.CommunityBanEntry{}, first.Bans...), second.Bans...))
		assert.Equal(t, want, got, "the two pages hold both bans once each")
	})

	t.Run("a non-moderator is refused", func(t *testing.T) {
		page, err := list(t, rosterOutsider, 50, "")
		assert.ErrorIs(t, err, moderation.ErrForbidden)
		assert.Nil(t, page)
	})
}

// I7: an instance admin with no role bans an account: the remove action is
// admin-acting and names the admin.
func TestCommunityBanActionsDB_AdminActingBan(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	expiry := banDBFarExpiry

	result, err := f.ban(rosterAdmin, banSubject, banReason, &expiry, 0, "ban-admin")

	require.NoError(t, err)
	require.NotNil(t, result, "the ban returns its result")
	assert.NotNil(t, f.inForceBan(t, banSubject, rosterClockStart), "the ban is in force")
	assert.Equal(t, []storedBanAction{localBanAction("remove", rosterAdmin, true, &expiry, rosterClockStart)},
		withoutIDs(f.banActions(t, banSubject)), "one admin-acting remove action naming the admin")
}

// I11 with Round 1 #8: a rewrite and a lapsed record. A ban (reason A,
// expiry X) rewritten to (reason B, no expiry) leaves B and no expiry in the
// index, advances the version once per change and logs two remove actions;
// the unban reverses the latest. A lapsed ban still standing is deleted by an
// unban, not AlreadyApplied, with one restore.
func TestCommunityBanActionsDB_RewriteAndLapsedBan(t *testing.T) {
	t.Parallel()

	t.Run("rewrite then unban", func(t *testing.T) {
		t.Parallel()
		f := newCommunityBanDBFixture(t)
		expiry := banDBFarExpiry
		_, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
		require.NoError(t, err, "M bans with reason A and expiry X")

		rewrittenAt := rosterClockStart.Add(time.Minute)
		f.clock.Set(rewrittenAt)
		f.repo.mu.Lock()
		f.repo.writeCIDs[communities.BanCollection+"/"+banSubjectKey] = banDBRewrittenCID
		f.repo.mu.Unlock()
		f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
		rewritten, err := f.ban(rosterModerator, banSubject, banDBRewriteReason, nil, 1, "ban-2")

		require.NoError(t, err, "M rewrites the ban with reason B and no expiry")
		require.NotNil(t, rewritten, "the rewrite returns its result")
		assert.EqualValues(t, 2, rewritten.Version)
		assert.False(t, rewritten.AlreadyApplied)
		assert.Equal(t, &communities.CommunityBan{
			CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
			RecordCID: banDBRewrittenCID, Reason: banDBRewriteReason,
		}, f.inForceBan(t, banSubject, rosterClockStart), "the index holds reason B and no expiry")
		assert.EqualValues(t, 2, f.accountVersion(t, banSubject), "one version per change")

		unbannedAt := rosterClockStart.Add(2 * time.Minute)
		f.clock.Set(unbannedAt)
		f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
		unbanned, err := f.unban(rosterModerator, banSubject, 2, "unban-1")

		require.NoError(t, err)
		require.NotNil(t, unbanned, "the unban returns its result")
		assert.EqualValues(t, 3, unbanned.Version)
		assert.False(t, f.banRowExists(t, banSubject), "the ban row is gone")
		actions := f.banActions(t, banSubject)
		assert.Equal(t, []storedBanAction{
			localBanAction("remove", rosterModerator, false, &expiry, rosterClockStart),
			localBanAction("remove", rosterModerator, false, nil, rewrittenAt),
			localBanAction("restore", rosterModerator, false, nil, unbannedAt),
		}, withoutIDs(actions), "two remove actions and one restore")
		if assert.Len(t, actions, 3) {
			assert.Equal(t, actions[1].ID, actions[2].ReversesActionID, "the unban reverses the latest ban action")
			assert.Equal(t, rewritten.ActionID, actions[1].ID, "the rewrite's result names its action")
		}
		text := f.banActionRowsText(t, banSubject)
		assert.NotContains(t, text, banReason, "free-text reason A is not in any action")
		assert.NotContains(t, text, banDBRewriteReason, "free-text reason B is not in any action")
	})

	t.Run("unban of a lapsed ban still standing", func(t *testing.T) {
		t.Parallel()
		f := newCommunityBanDBFixture(t)
		lapsed := banDBLapsedExpiry
		f.standBan(t, banReason, &lapsed)
		require.EqualValues(t, 1, f.accountVersion(t, banSubject), "fixture: indexing the ban advanced the version")
		require.Nil(t, f.inForceBan(t, banSubject, rosterClockStart), "fixture: the ban has lapsed")

		unbanned, err := f.unban(rosterModerator, banSubject, 1, "unban-lapsed")

		require.NoError(t, err)
		require.NotNil(t, unbanned, "the unban returns its result")
		assert.False(t, unbanned.AlreadyApplied, "a standing lapsed record is deleted, not already applied")
		assert.EqualValues(t, 2, unbanned.Version)
		assert.False(t, f.banRowExists(t, banSubject), "the lapsed ban row is gone")
		assert.False(t, f.repo.hasRecord(communities.BanCollection, banSubjectKey), "the lapsed record is deleted")
		actions := f.banActions(t, banSubject)
		assert.Equal(t, []storedBanAction{localBanAction("restore", rosterModerator, false, nil, rosterClockStart)},
			withoutIDs(actions), "one restore naming M")
		if assert.Len(t, actions, 1) {
			assert.Empty(t, actions[0].ReversesActionID, "the indexed ban had no action to reverse")
			assert.Equal(t, unbanned.ActionID, actions[0].ID, "the result names the restore")
		}
	})
}
