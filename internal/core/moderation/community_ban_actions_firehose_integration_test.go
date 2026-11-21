//go:build integration

package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Firehose convergence of community bans: the production ban consumer (the
// post consumer with the ban index and the real rev gate) over the same
// database as the community action service, with the community repo faked.

// I8: the same-rev firehose copies of a local ban and a local unban change
// nothing: one remove and one restore action naming M, the version advanced
// once per change.
func TestCommunityBanFirehoseDB_LocalWriteCopies(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	tests := []struct {
		name  string
		unban bool
	}{
		{name: "copy of a local ban"},
		{name: "copies of a local ban and its unban", unban: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityBanDBFixture(t)
			banned, err := f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
			require.NoError(t, err, "M bans the account")
			require.NotNil(t, banned, "the ban returns its result")
			want := []storedBanAction{localBanAction("remove", rosterModerator, false, &expiry, time.Time{})}
			wantVersion := int64(1)
			if tt.unban {
				f.clock.Set(rosterClockStart.Add(time.Minute))
				f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
				unbanned, err := f.unban(rosterModerator, banSubject, 1, "unban-1")
				require.NoError(t, err, "M unbans the account")
				require.NotNil(t, unbanned, "the unban returns its result")
				want = append(want, localBanAction("restore", rosterModerator, false, nil, time.Time{}))
				wantVersion = 2
			}
			before := f.banActions(t, banSubject)
			require.Equal(t, want, withoutIDsOrTimes(before), "fixture: the local actions")

			// The ban's commit landed at head one, the unban's at head two.
			f.deliverBan(t, rosterCommunity, "create", rosterHeadOneRev, banWrittenCID,
				banRecordBody(banReason, &expiry, banLocalCreatedAt))
			if tt.unban {
				f.deliverBan(t, rosterCommunity, "delete", rosterHeadTwoRev, "", nil)
			}

			after := f.banActions(t, banSubject)
			assert.Equal(t, banActionIDs(before), banActionIDs(after), "the copies add no action")
			assert.Equal(t, want, withoutIDsOrTimes(after), "the actions still name M")
			assert.EqualValues(t, wantVersion, f.accountVersion(t, banSubject), "the version advanced once per change")
			if tt.unban {
				assert.False(t, f.banRowExists(t, banSubject), "the ban stays lifted")
			} else {
				assert.Equal(t, &communities.CommunityBan{
					CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
					RecordCID: banWrittenCID, Reason: banReason, ExpiresAt: &expiry,
				}, f.banRowIn(t, rosterCommunity, banSubject), "the ban stays indexed as M wrote it")
			}
		})
	}
}

// I8: a ban written outside Coves, in a hosted and in a non-hosted indexed
// community, records one remove action with no actor, origin inherited and
// the moderator-discretion reason, carrying the expiry; its delete records
// one restore with no actor. Identical redeliveries, and the identical record
// at a later rev, record nothing and advance no version.
func TestCommunityBanFirehoseDB_BanWrittenOutsideCoves(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	tests := []struct {
		name      string
		community string
		setup     func(t *testing.T, f *communityBanDBFixture)
	}{
		{name: "hosted community", community: rosterCommunity, setup: func(*testing.T, *communityBanDBFixture) {}},
		{
			name: "non-hosted indexed community", community: postDBUnhostedCommunity,
			setup: func(t *testing.T, f *communityBanDBFixture) {
				insertRosterCommunity(t, f.db, postDBUnhostedCommunity, "rosterbridged.test", false)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityBanDBFixture(t)
			tt.setup(t, f)
			body := banRecordBody(banExternalReason, &expiry, "2026-11-13T11:00:00Z")
			const createRev, laterRev, deleteRev = "3kbanextrev01", "3kbanextrev02", "3kbanextrev03"

			f.deliverBan(t, tt.community, "create", createRev, banExternalCID, body)

			assert.Equal(t, &communities.CommunityBan{
				CommunityDID: tt.community, SubjectDID: banSubject, RecordKey: banSubjectKey,
				RecordCID: banExternalCID, Reason: banExternalReason, ExpiresAt: &expiry,
			}, f.banRowIn(t, tt.community, banSubject), "the ban is indexed")
			banned := f.banActionsIn(t, tt.community, banSubject)
			assert.Equal(t, []storedBanAction{externalBanAction(tt.community, "remove", &expiry)},
				withoutIDsOrTimes(banned), "one remove action with no actor")
			assert.NotContains(t, f.banActionRowsTextIn(t, tt.community, banSubject), banExternalReason,
				"the free-text ban reason is not in the action")
			assert.EqualValues(t, 1, f.accountVersionIn(t, tt.community, banSubject), "the version advanced once")

			f.deliverBan(t, tt.community, "create", createRev, banExternalCID, body)
			f.deliverBan(t, tt.community, "update", laterRev, banExternalCID, body)

			assert.Equal(t, banActionIDs(banned), banActionIDs(f.banActionsIn(t, tt.community, banSubject)),
				"the redelivery and the identical record at a later rev add no action")
			assert.EqualValues(t, 1, f.accountVersionIn(t, tt.community, banSubject), "nor advance the version")

			f.deliverBan(t, tt.community, "delete", deleteRev, "", nil)

			assert.Nil(t, f.banRowIn(t, tt.community, banSubject), "the ban is lifted")
			lifted := f.banActionsIn(t, tt.community, banSubject)
			assert.Equal(t, []storedBanAction{
				externalBanAction(tt.community, "remove", &expiry),
				externalBanAction(tt.community, "restore", nil),
			}, withoutIDsOrTimes(lifted), "the delete records one restore with no actor")
			assert.EqualValues(t, 2, f.accountVersionIn(t, tt.community, banSubject), "the delete advanced the version once")

			f.deliverBan(t, tt.community, "delete", deleteRev, "", nil)

			assert.Equal(t, banActionIDs(lifted), banActionIDs(f.banActionsIn(t, tt.community, banSubject)),
				"the redelivered delete adds no action")
			assert.EqualValues(t, 2, f.accountVersionIn(t, tt.community, banSubject), "nor advances the version")
		})
	}
}

// Review #8: a ban written outside Coves and then rewritten outside Coves (an
// update with a new CID and reason) records a second remove action with no
// actor, advances the version a second time and indexes the rewrite.
func TestCommunityBanFirehoseDB_BanRewrittenOutsideCoves(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	f := newCommunityBanDBFixture(t)
	f.deliverBan(t, rosterCommunity, "create", "3kbanextrev01", banExternalCID,
		banRecordBody(banExternalReason, &expiry, "2026-11-13T11:00:00Z"))
	require.EqualValues(t, 1, f.accountVersion(t, banSubject), "fixture: the external ban advanced the version")

	f.deliverBan(t, rosterCommunity, "update", "3kbanextrev02", banDBRewrittenCID,
		banRecordBody(banDBRewriteReason, nil, "2026-11-13T11:30:00Z"))

	assert.Equal(t, &communities.CommunityBan{
		CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
		RecordCID: banDBRewrittenCID, Reason: banDBRewriteReason,
	}, f.banRowIn(t, rosterCommunity, banSubject), "the rewrite is indexed")
	assert.Equal(t, []storedBanAction{
		externalBanAction(rosterCommunity, "remove", &expiry),
		externalBanAction(rosterCommunity, "remove", nil),
	}, withoutIDsOrTimes(f.banActions(t, banSubject)), "the rewrite records a second remove with no actor")
	assert.EqualValues(t, 2, f.accountVersion(t, banSubject), "the rewrite advanced the version once more")
}

// Review #4: a ban written outside Coves, gated at head zero; M's unban
// (guarded at head zero) commits at head two, stamping the record's rev gate
// there. A delayed update of the ban record at head one, between the two, is
// older than the unban: the ban stays lifted and no action is added.
func TestCommunityBanFirehoseDB_DelayedUpdateBeforeALocalUnban(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	body := banRecordBody(banExternalReason, nil, "2026-11-13T11:00:00Z")
	f.deliverBan(t, rosterCommunity, "create", rosterHeadZeroRev, banExternalCID, body)
	f.repo.setPostRecord(communities.BanCollection, banSubjectKey, banExternalCID, body)
	require.EqualValues(t, 1, f.accountVersion(t, banSubject), "fixture: the external ban advanced the version")
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	unbanned, err := f.unban(rosterModerator, banSubject, 1, "unban-1")
	require.NoError(t, err, "M unbans the account")
	require.NotNil(t, unbanned, "the unban returns its result")
	require.False(t, f.banRowExists(t, banSubject), "fixture: the unban lifts the ban")
	actions := f.banActions(t, banSubject)

	f.deliverBan(t, rosterCommunity, "update", rosterHeadOneRev, banDBRewrittenCID,
		banRecordBody(banDBRewriteReason, nil, "2026-11-13T11:30:00Z"))

	assert.False(t, f.banRowExists(t, banSubject), "the older update does not bring the ban back")
	assert.Equal(t, banActionIDs(actions), banActionIDs(f.banActions(t, banSubject)), "nor adds an action")
	assert.EqualValues(t, 2, f.accountVersion(t, banSubject), "nor advances the version")
}

// Review #5: the response of M's ban or unban is lost, and before the
// same-key retry a later change of the ban record written outside Coves is
// indexed from the firehose. The retry completes the intent at its guard rev,
// which the record's gate has passed: the later change stays indexed and only
// M's action is logged.
func TestCommunityBanFirehoseDB_LaterExternalChangeStandsOverTheRetry(t *testing.T) {
	t.Parallel()
	expiry := banDBFarExpiry
	external := &communities.CommunityBan{
		CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
		RecordCID: banExternalCID, Reason: banExternalReason,
	}
	externalBody := banRecordBody(banExternalReason, nil, "2026-11-13T12:30:00Z")
	tests := []struct {
		name  string
		unban bool
	}{
		{name: "ban whose record is rewritten outside Coves"},
		{name: "unban whose record is recreated outside Coves", unban: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityBanDBFixture(t)
			f.setBanWriteCID(banLocalBodyCID)
			act := func() (*moderation.CommunityActionResult, error) {
				return f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "ban-1")
			}
			// The ban's commit lands at head one; the external change at head three.
			wantAction := localBanAction("remove", rosterModerator, false, &expiry, time.Time{})
			if tt.unban {
				banned, err := act()
				require.NoError(t, err, "fixture: M bans the account")
				require.NotNil(t, banned, "fixture: the ban returns its result")
				f.clock.Set(rosterClockStart.Add(time.Minute))
				// The unban's commit lands at head two.
				f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
				act = func() (*moderation.CommunityActionResult, error) {
					return f.unban(rosterModerator, banSubject, 1, "unban-1")
				}
				wantAction = localBanAction("restore", rosterModerator, false, nil, time.Time{})
			}
			f.loseNextResponse()
			_, err := act()
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the response is lost")
			require.Equal(t, 1, f.openIntents(t), "fixture: the intent stays open")

			f.deliverBan(t, rosterCommunity, "update", rosterHeadThreeRev, banExternalCID, externalBody)
			require.Equal(t, external, f.banRowIn(t, rosterCommunity, banSubject), "fixture: the external change is indexed")
			versionBefore := f.accountVersion(t, banSubject)

			result, err := act()

			require.NoError(t, err, "the same-key retry settles the lost commit")
			require.NotNil(t, result, "the retry returns its result")
			assert.Equal(t, external, f.banRowIn(t, rosterCommunity, banSubject),
				"the later external change stays indexed")
			assert.Equal(t, versionBefore, f.accountVersion(t, banSubject), "the retry advances no version")
			var retried []storedBanAction
			for _, action := range f.banActions(t, banSubject) {
				if action.Action == wantAction.Action && action.ActorDID == rosterModerator {
					retried = append(retried, action)
				}
			}
			if assert.Len(t, retried, 1, "the retry logs one %s naming M", wantAction.Action) {
				assert.Equal(t, retried[0].ID, result.ActionID, "the retry's result names its action")
				assert.Equal(t, []storedBanAction{wantAction}, withoutIDsOrTimes(retried), "the retry logs M's action")
			}
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
		})
	}
}
