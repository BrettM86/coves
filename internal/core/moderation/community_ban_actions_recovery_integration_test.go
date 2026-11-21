//go:build integration

package moderation_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Recovery of community bans and unbans against real Postgres: the PDS
// commits M's write and the response is lost. The community repo is faked;
// the index, intents, actions and the ban consumer (with the real rev gate)
// are real.

// I9 (a)-(d): a ban (banSubject unbanned, version 0) or an unban (M's ban
// standing, version 1) by M commits and the response is lost. Settled by the
// same-key retry first or by the commit's firehose copy first: exactly one
// action of the change names M, the version advanced once, the retry's result
// names that action, no intent is left open, and the other settles nothing
// further.
func TestCommunityBanRecoveryDB_LostResponse(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	tests := []struct {
		name          string
		unban         bool
		firehoseFirst bool
	}{
		{name: "ban recovered by the same-key retry first"},
		{name: "ban whose firehose copy arrives before the retry", firehoseFirst: true},
		{name: "unban recovered by the same-key retry first", unban: true},
		{name: "unban whose firehose delete copy arrives before the retry", unban: true, firehoseFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityBanDBFixture(t)
			f.setBanWriteCID(banLocalBodyCID)

			var (
				act         func() (*moderation.CommunityActionResult, error)
				deliverCopy func()
				want        []storedBanAction
				wantVersion int64
			)
			if tt.unban {
				banned, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
				require.NoError(t, err, "fixture: M bans the account")
				require.NotNil(t, banned, "fixture: the ban returns its result")
				f.clock.Set(rosterClockStart.Add(time.Minute))
				// The unban's commit lands at head two.
				f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
				act = func() (*moderation.CommunityActionResult, error) {
					return f.unban(rosterModerator, banSubject, 1, "unban-1")
				}
				deliverCopy = func() { f.deliverBan(t, rosterCommunity, "delete", rosterHeadTwoRev, "", nil) }
				want = []storedBanAction{
					localBanAction("remove", rosterModerator, false, &expiry, time.Time{}),
					localBanAction("restore", rosterModerator, false, nil, time.Time{}),
				}
				wantVersion = 2
			} else {
				act = func() (*moderation.CommunityActionResult, error) {
					return f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
				}
				// The ban's commit lands at head one.
				deliverCopy = func() {
					f.deliverBan(t, rosterCommunity, "create", rosterHeadOneRev, banLocalBodyCID,
						banRecordBody(banReason, &expiry, banLocalCreatedAt))
				}
				want = []storedBanAction{localBanAction("remove", rosterModerator, false, &expiry, time.Time{})}
				wantVersion = 1
			}
			actionsBefore := f.banActions(t, banSubject)
			versionBefore := f.accountVersion(t, banSubject)
			f.loseNextResponse()

			_, err := act()

			require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
			assert.Equal(t, 1, f.openIntents(t), "the intent stays open")
			assert.Equal(t, actionsBefore, f.banActions(t, banSubject), "no action yet")
			assert.Equal(t, versionBefore, f.accountVersion(t, banSubject), "the version is unchanged")
			batches := len(f.repo.batchesSnapshot())

			if tt.firehoseFirst {
				deliverCopy()

				assert.Equal(t, want, withoutIDsOrTimes(f.banActions(t, banSubject)),
					"the copy completes the intent with M")
				assert.EqualValues(t, wantVersion, f.accountVersion(t, banSubject), "the copy advanced the version once")
				assert.Equal(t, 0, f.openIntents(t), "the copy completes the open intent")
			}

			result, err := act()

			require.NoError(t, err, "the same-key retry settles the lost commit")
			require.NotNil(t, result, "the retry returns its result")
			assert.Equal(t, banSubjectURI, result.RecordURI)
			assert.EqualValues(t, wantVersion, result.Version)
			if !tt.firehoseFirst {
				assert.False(t, result.AlreadyApplied, "the retry completed this intent's own change")
			}
			actions := f.banActions(t, banSubject)
			assert.Equal(t, want, withoutIDsOrTimes(actions), "one action of the change names M")
			if assert.Len(t, actions, len(want)) {
				assert.Equal(t, actions[len(actions)-1].ID, result.ActionID, "the retry's result names the action")
				if tt.unban {
					assert.Equal(t, actions[0].ID, actions[1].ReversesActionID, "the restore reverses the ban action")
				}
			}
			assert.EqualValues(t, wantVersion, f.accountVersion(t, banSubject), "the version advanced once")
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
			assert.Len(t, f.repo.batchesSnapshot(), batches, "the retry sends no second batch")
			if tt.unban {
				assert.False(t, f.banRowExists(t, banSubject), "the ban is lifted")
			} else {
				assert.Equal(t, &communities.CommunityBan{
					CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
					RecordCID: banLocalBodyCID, Reason: banReason, ExpiresAt: &expiry,
				}, f.banRowIn(t, rosterCommunity, banSubject), "the ban is indexed as M wrote it")
			}

			if !tt.firehoseFirst {
				deliverCopy()

				assert.Equal(t, banActionIDs(actions), banActionIDs(f.banActions(t, banSubject)),
					"the delayed copy adds no action")
				assert.EqualValues(t, wantVersion, f.accountVersion(t, banSubject), "nor advances the version")
			}
		})
	}
}

// I9 (e), Round 1 #2: a standing ban; an unrelated commit moves the head to
// head one, then M's unban (guarded at head one) commits at head two and the
// response is lost. A delayed delete of the ban record at a rev before the
// unban's guard rev is not the unban's commit: it does not complete the open
// unban intent or record an action naming M.
func TestCommunityBanRecoveryDB_OlderDelayedDeleteDoesNotCompleteTheUnban(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	f.standBan(t, banReason, nil)
	require.EqualValues(t, 1, f.accountVersion(t, banSubject), "fixture: indexing the ban advanced the version")
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev})
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.loseNextResponse()
	_, err := f.unban(rosterModerator, banSubject, 1, "unban-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
	require.Equal(t, 1, f.openIntents(t), "fixture: the unban intent is open")

	// rosterBetweenHeadsRev sorts after head zero and before head one, the
	// unban intent's guard rev.
	f.deliverBan(t, rosterCommunity, "delete", rosterBetweenHeadsRev, "", nil)

	assert.Equal(t, 1, f.openIntents(t), "the older delete does not complete the unban intent")
	for _, action := range f.banActions(t, banSubject) {
		assert.NotEqual(t, rosterModerator, action.ActorDID, "no action names M: %+v", action)
	}
}

// I9 (f), Round 1 #3: M's ban expiring at 12:05 commits and the response is
// lost; the service clock passes the expiry before the same-key retry. The
// retry reconciles its own commit (one remove action naming M, version +1)
// rather than refusing the past expiry.
func TestCommunityBanRecoveryDB_RetryAfterTheExpiryPasses(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	expiry := time.Date(2026, 11, 13, 12, 5, 0, 0, time.UTC)
	f.loseNextResponse()
	_, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
	f.clock.Set(time.Date(2026, 11, 13, 12, 6, 0, 0, time.UTC))

	result, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")

	require.NoError(t, err, "the retry reconciles, not InvalidRequest")
	require.NotNil(t, result, "the retry returns its result")
	assert.False(t, result.AlreadyApplied)
	assert.EqualValues(t, 1, result.Version)
	actions := f.banActions(t, banSubject)
	assert.Equal(t, []storedBanAction{localBanAction("remove", rosterModerator, false, &expiry, time.Time{})},
		withoutIDsOrTimes(actions), "one remove action names M")
	if assert.Len(t, actions, 1) {
		assert.Equal(t, actions[0].ID, result.ActionID, "the retry's result names the action")
	}
	assert.EqualValues(t, 1, f.accountVersion(t, banSubject), "the version advanced once")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second batch")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}

// I10, Round 1 #1: an invitation acceptance and a ban of the invitee whose
// PDS write committed with its response lost and no firehose copy yet: the
// opposite operation is refused with StateConflict and writes nothing.
func TestCommunityBanInFlightDB_OppositeUnresolvedIntents(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry

	t.Run("acceptance in flight refuses the ban", func(t *testing.T) {
		t.Parallel()
		f := newCommunityBanDBFixture(t)
		f.loseAcceptResponse(t, rosterInvitation(rosterModeratorInvitationID, banSubject,
			communities.RoleModerator, rosterOwner, false), "accept-1")
		require.Equal(t, 1, f.openIntents(t), "fixture: the acceptance intent is open")

		result, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict, "the acceptance in flight refuses the ban")
		assert.Nil(t, result)
		assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS commit: the acceptance's")
		assert.False(t, f.repo.hasRecord(communities.BanCollection, banSubjectKey), "no ban record is written")
		assert.False(t, f.banRowExists(t, banSubject), "no ban is indexed")
		assert.Empty(t, f.banActions(t, banSubject), "no ban action")
		assert.Empty(t, f.idempotencyActors(t, "ban-1"), "the refused ban stores no result")
		assert.Equal(t, 1, f.openIntents(t), "only the acceptance's intent is open")
		assert.EqualValues(t, 0, f.accountVersion(t, banSubject), "the account's version is unchanged")
	})

	t.Run("ban in flight refuses the acceptance", func(t *testing.T) {
		t.Parallel()
		f := newCommunityBanDBFixture(t)
		f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, banSubject,
			communities.RoleModerator, rosterOwner, false))
		f.loseNextResponse()
		_, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the ban's response is lost")
		require.Equal(t, 1, f.openIntents(t), "fixture: the ban intent is open")
		rolesBefore := f.indexedRoles(t)

		result, err := f.acceptInvitation(banSubject, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict, "the ban in flight refuses the acceptance")
		assert.Nil(t, result)
		assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS commit: the ban's")
		assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no role record is written")
		assert.Equal(t, rolesBefore, f.indexedRoles(t), "no role is indexed")
		assert.NotContains(t, f.indexedRoles(t), banSubject)
		assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
		assert.Empty(t, f.idempotencyActors(t, "accept-1"), "the refused acceptance stores no result")
		assert.Equal(t, 1, f.openIntents(t), "only the ban's intent is open")
	})
}

// Review #2: a ban of the invitee in flight whose open intent row a firehose
// completion holds (completeBanIntentFromEvent locks it FOR UPDATE before it
// takes the ban-subject lock). The acceptance, which holds the invitee's
// ban-subject lock, reads the intent without waiting on that row lock and is
// refused with StateConflict; the completion then takes the ban-subject lock
// with no deadlock.
func TestCommunityBanInFlightDB_AcceptanceDoesNotWaitOnALockedBanIntent(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	f := newCommunityBanDBFixture(t)
	f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, banSubject,
		communities.RoleModerator, rosterOwner, false))
	f.loseNextResponse()
	_, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the ban's response is lost")
	require.Equal(t, 1, f.openIntents(t), "fixture: the ban intent is open")

	ctx := context.Background()
	completion, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = completion.Rollback() }()
	var locked int
	require.NoError(t, completion.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
		    SELECT id FROM community_write_intents WHERE target_uri = $1 AND state = 'open' FOR UPDATE
		) intents
	`, banSubjectURI).Scan(&locked), "the completion locks the open ban intent row")
	require.Equal(t, 1, locked, "fixture: the completion holds the ban intent row")

	type outcome struct {
		result *moderation.CommunityActionResult
		err    error
	}
	accepted := make(chan outcome, 1)
	go func() {
		result, err := f.acceptInvitation(banSubject, rosterModeratorInvitationID, rosterStartVersion, "accept-1")
		accepted <- outcome{result, err}
	}()
	var got outcome
	select {
	case got = <-accepted:
	case <-time.After(10 * time.Second):
		_ = completion.Rollback()
		<-accepted
		t.Fatal("the acceptance waited on the ban intent row the completion holds")
	}

	assert.ErrorIs(t, got.err, moderation.ErrStateConflict, "the ban in flight refuses the acceptance")
	assert.Nil(t, got.result)
	_, err = completion.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, banSubject)
	require.NoError(t, err, "the completion then takes the ban-subject lock")
	require.NoError(t, completion.Rollback())
	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no role record is written")
	assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
}

// Review #9: a standing ban; M's unban (guarded at head one) commits at head
// two and the response is lost; a delayed delete of the ban record at a rev
// before the guard is not the unban's commit, so it lifts the ban and logs one
// restore with no actor. The same-key retry then completes the unban linked to
// that restore: exactly one restore, the version advanced once by the lift.
func TestCommunityBanRecoveryDB_RetryAfterAnUnmatchedDeleteLogsOneRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	f.standBan(t, banReason, nil)
	require.EqualValues(t, 1, f.accountVersion(t, banSubject), "fixture: indexing the ban advanced the version")
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev})
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.loseNextResponse()
	_, err := f.unban(rosterModerator, banSubject, 1, "unban-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the unban's response is lost")
	f.deliverBan(t, rosterCommunity, "delete", rosterBetweenHeadsRev, "", nil)
	require.False(t, f.banRowExists(t, banSubject), "fixture: the delayed delete lifts the ban")
	lifted := f.banActions(t, banSubject)
	require.Equal(t, []storedBanAction{externalBanAction(rosterCommunity, "restore", nil)},
		withoutIDsOrTimes(lifted), "fixture: the delayed delete logs one restore with no actor")
	require.Equal(t, 1, f.openIntents(t), "fixture: the unban intent is still open")

	result, err := f.unban(rosterModerator, banSubject, 1, "unban-1")

	require.NoError(t, err, "the same-key retry settles the lost commit")
	require.NotNil(t, result, "the retry returns its result")
	assert.Equal(t, banActionIDs(lifted), banActionIDs(f.banActions(t, banSubject)), "exactly one restore action")
	assert.Equal(t, lifted[0].ID, result.ActionID, "the retry's result names the restore already logged")
	assert.EqualValues(t, 2, result.Version)
	assert.EqualValues(t, 2, f.accountVersion(t, banSubject), "the version advanced once for the lift")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}

// Review r2: a standing ban A; M's unban (guarded at head one) commits at head
// two and the response is lost. Before the retry, a writer outside Coves
// creates ban B at the same record key (a different CID) and then deletes it;
// neither event is M's commit, so the firehose logs B's remove and B's restore
// with no actor. That restore removed B, not A, so the same-key retry must not
// link to it: it logs M's own restore and leaves the index and version as the
// firehose left them.
func TestCommunityBanRecoveryDB_RetryAfterALaterExternalBanCycleLogsItsOwnRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	f.standBan(t, banReason, nil)
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev})
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.loseNextResponse()
	_, err := f.unban(rosterModerator, banSubject, 1, "unban-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the unban's response is lost")
	f.deliverBan(t, rosterCommunity, "create", rosterHeadThreeRev, banExternalCID,
		banRecordBody(banExternalReason, nil, "2026-11-13T12:30:00Z"))
	f.deliverBan(t, rosterCommunity, "delete", rosterHeadFiveRev, "", nil)
	require.False(t, f.banRowExists(t, banSubject), "fixture: the external delete lifts ban B")
	external := f.banActions(t, banSubject)
	require.Equal(t, []storedBanAction{
		externalBanAction(rosterCommunity, "remove", nil),
		externalBanAction(rosterCommunity, "restore", nil),
	}, withoutIDsOrTimes(external), "fixture: ban B's cycle logs a remove and a restore with no actor")
	require.Equal(t, 1, f.openIntents(t), "fixture: the unban intent is still open")
	version := f.accountVersion(t, banSubject)

	result, err := f.unban(rosterModerator, banSubject, 1, "unban-1")

	require.NoError(t, err, "the same-key retry settles the lost commit")
	require.NotNil(t, result, "the retry returns its result")
	// M's restore is stamped at the service clock's completion time, so it is
	// found by ID rather than by its place in time order.
	actions := f.banActions(t, banSubject)
	require.Len(t, actions, 3, "M's restore is logged beside ban B's cycle")
	assert.NotContains(t, banActionIDs(external), result.ActionID, "the retry does not link to ban B's actions")
	var own, others []storedBanAction
	for _, action := range actions {
		if action.ID == result.ActionID {
			own = append(own, action)
		} else {
			others = append(others, action)
		}
	}
	assert.Equal(t, withoutIDsOrTimes(external), withoutIDsOrTimes(others), "ban B's actions stand unchanged")
	require.Len(t, own, 1, "the retry's result names a logged action")
	assert.Equal(t, "restore", own[0].Action)
	assert.Equal(t, rosterModerator, own[0].ActorDID, "the restore names M")
	assert.Equal(t, "local", own[0].Origin)
	assert.False(t, f.banRowExists(t, banSubject), "the index is unchanged")
	assert.Equal(t, version, f.accountVersion(t, banSubject), "the version is unchanged")
	assert.Equal(t, version, result.Version)
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}
