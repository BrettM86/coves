//go:build integration

package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community post remove and restore against real Postgres: the real community
// action store, post resolver, admission index and role authorizer, the
// community's PDS repo faked. Outcomes are read back from the database and the
// production read paths.

// I1, I4: a moderator, or an instance admin with no role, removes an accepted
// post with code spam and a reason. Before RemovePost returns the admission is
// removed with the code and reason, the author's own view carries them, the
// community feed drops the post, one action records the removal and the post's
// community version advanced by one.
func TestCommunityPostActionsDB_LocalRemove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		actorDID    string
		adminActing bool
	}{
		{name: "moderator", actorDID: rosterModerator},
		{name: "instance admin with no role", actorDID: rosterAdmin, adminActing: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			require.Contains(t, f.feedURIs(t), post.URI, "fixture: the accepted post is in the community feed")

			result, err := f.removePost(tt.actorDID, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

			require.NoError(t, err)
			require.NotNil(t, result, "RemovePost returns its result")
			admission := f.admission(t, post.URI)
			assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "the admission is removed before RemovePost returns")
			if assert.NotNil(t, admission.DecisionCode, "the removal's code is stored") {
				assert.Equal(t, "spam", *admission.DecisionCode)
			}
			if assert.NotNil(t, admission.RemovalReason, "the removal's reason is stored") {
				assert.Equal(t, postDBRemovalReason, *admission.RemovalReason)
			}
			view := f.authorView(t, post.URI)
			if assert.NotNil(t, view, "the author still reads the post") {
				assert.Equal(t, string(posts.AdmissionStatusRemoved), view.Status)
				assert.Equal(t, &posts.CommunityRemoval{Code: "spam", Reason: postDBRemovalReason}, view.CommunityRemoval,
					"the author's view carries the community's code and reason")
			}
			assert.NotContains(t, f.feedURIs(t), post.URI, "the community feed no longer lists the post")
			assert.Equal(t, []storedPostAction{
				localPostAction("remove", tt.actorDID, tt.adminActing, post.URI, postDBReasonSpam),
			}, f.postActions(t), "one action records the removal")
			assert.EqualValues(t, 2, f.postVersion(t, post.URI), "the post's community version advanced by one")
			assert.EqualValues(t, 2, result.Version, "the result carries the post's version after the removal")
			assert.Equal(t, postAlphaRemovalURI, result.RecordURI, "the result names the removal record")
		})
	}
}

// I2: the moderator restores the post they removed. The admission is accepted
// pinned to the post's current CID, the community feed serves it again, one
// restore action names the moderator with the removal's mapped reason, and the
// version advanced by one more.
func TestCommunityPostActionsDB_LocalRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "the removal landed")
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

	result, err := f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")

	require.NoError(t, err)
	require.NotNil(t, result, "RestorePost returns its result")
	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
	}, f.postActions(t), "one restore action names the moderator, its reason mapped from the removal's code")
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "the admission is accepted again")
	if assert.NotNil(t, admission.AcceptedCID, "the acceptance pins a CID") {
		assert.Equal(t, postAlphaCID, *admission.AcceptedCID, "pinned to the post's current CID")
	}
	assert.Contains(t, f.feedURIs(t), post.URI, "the community feed serves the post again")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the post's community version advanced by one")
	assert.EqualValues(t, 3, result.Version)
	assert.Equal(t, postAlphaAcceptanceURI, result.RecordURI, "the result names the acceptance record")
}

// Chunk 13 B6: M removes a post with code doxing, then restores it. The
// unfiltered public log lists both without the post or the community; the
// subject, collection and community filters list neither; the admin log names
// the post on both.
func TestCommunityModlog_DoxingRemovalAndRestoreWithholdThePost(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "doxing", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.NoError(t, err)
	const postURI = "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostalpha1"

	entries := publicModlog(t, f.db, moderation.ListActionsParams{})
	require.Len(t, entries, 2, "the unfiltered public log lists the removal and the restore")
	for i, wantAction := range []string{"restore", "remove"} {
		entry := entries[i]
		assert.Equal(t, wantAction, entry["action"], "entry %d", i)
		assert.Equal(t, "social.coves.moderation.defs#reasonDoxing", entry["reason"], "%s reason", wantAction)
		assert.Equal(t, map[string]any{"did": "did:plc:troublemaker456"}, entry["actor"], "%s actor", wantAction)
		assert.NotContains(t, entry, "subject", "%s withholds the post", wantAction)
		assert.Equal(t, map[string]any{"kind": "community"}, entry["scope"], "%s withholds the community", wantAction)
		assert.Equal(t, "did:plc:rosteralpha", entry["authorityDid"], "%s authority stays the community", wantAction)
	}

	for name, params := range map[string]moderation.ListActionsParams{
		"subject":    {Subject: postURI},
		"collection": {Collection: "social.coves.community.postv2"},
		"community":  {Community: "did:plc:rosteralpha"},
		"authority":  {Authority: "did:plc:rosteralpha"},
	} {
		assert.Empty(t, publicModlog(t, f.db, params), "the %s filter lists neither entry", name)
	}

	admin := adminModlog(t, f.db, moderation.ListAdminActionsParams{})
	require.Len(t, admin, 2, "the admin log lists both")
	for i, wantAction := range []string{"restore", "remove"} {
		assert.Equal(t, wantAction, modlogObject(t, admin[i]["action"])["action"], "admin entry %d", i)
		if assert.Contains(t, admin[i], "privateSubject", "%s names the post to admins", wantAction) {
			assert.Equal(t, postURI, modlogObject(t, admin[i]["privateSubject"])["uri"], "%s private subject", wantAction)
		}
	}
}

// I3: a moderator whose account is under an in-force instance ban is refused;
// nothing reaches the community repo or the database.
func TestCommunityPostActionsDB_BannedModeratorIsRefused(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.banAccount(t, rosterModerator)
	require.True(t, f.moderatorRowIndexed(t), "fixture: M still holds the moderator role")

	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

	assert.ErrorIs(t, err, moderation.ErrForbidden)
	assert.Empty(t, f.repo.batchesSnapshot(), "no batch reaches the community repo")
	assert.Empty(t, f.intents(t), "no intent is stored")
	assert.Empty(t, f.postActions(t), "no action is written")
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "the admission is unchanged")
	if assert.NotNil(t, admission.AcceptedCID) {
		assert.Equal(t, postAlphaCID, *admission.AcceptedCID)
	}
	assert.EqualValues(t, postDBSeededVersion, f.postVersion(t, post.URI), "the version is unchanged")
}

// I5: removal limits are counted from the stored removals. Prior removals are
// made by the service itself at the window's start; the counted removal is
// attempted later on the injected clock. Removals older than the window and
// admin-acting removals are not counted.
func TestCommunityPostActionsDB_RemovalRateLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		limits    moderation.CommunityRemovalLimits
		priorBy   []string      // the actor of each prior removal, in order
		attemptAt time.Duration // after the prior removals
		wantErr   error
	}{
		{
			name:    "moderator at the per-moderator limit inside the window",
			limits:  moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 100, Window: time.Hour},
			priorBy: []string{rosterModerator, rosterModerator}, attemptAt: 30 * time.Minute,
			wantErr: moderation.ErrRateLimited,
		},
		{
			name:    "the moderator's prior removals are older than the window",
			limits:  moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 100, Window: time.Hour},
			priorBy: []string{rosterModerator, rosterModerator}, attemptAt: 61 * time.Minute,
		},
		{
			name:    "the prior removals were admin-acting",
			limits:  moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 2, Window: time.Hour},
			priorBy: []string{rosterAdmin, rosterAdmin}, attemptAt: 30 * time.Minute,
		},
		{
			name:    "community at the per-community limit by other moderators",
			limits:  moderation.CommunityRemovalLimits{PerModerator: 100, PerCommunity: 2, Window: time.Hour},
			priorBy: []string{rosterSecondModerator, rosterOwner}, attemptAt: 30 * time.Minute,
			wantErr: moderation.ErrRateLimited,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			service := f.serviceWithLimits(tt.limits)
			prior := []seededCommunityPost{
				f.seedAcceptedPost(t, postDBSeedFor("3kpostprior1")),
				f.seedAcceptedPost(t, postDBSeedFor("3kpostprior2")),
			}
			target := f.seedAcceptedPost(t, postDBAlpha)
			wantActions := []storedPostAction{}
			for i, actorDID := range tt.priorBy {
				f.clock.Set(rosterClockStart.Add(time.Duration(i) * time.Minute))
				_, err := removePostWith(service, actorDID, prior[i].URI, "spam", "", postDBSeededVersion, "prior-remove-"+prior[i].SubjectRkey)
				require.NoError(t, err, "prior removal %d", i+1)
				wantActions = append(wantActions, localPostAction("remove", actorDID, actorDID == rosterAdmin, prior[i].URI, postDBReasonSpam))
			}
			f.clock.Set(rosterClockStart.Add(tt.attemptAt))

			_, err := removePostWith(service, rosterModerator, target.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Equal(t, wantActions, f.postActions(t), "only the prior removals are recorded")
				assert.Len(t, f.repo.batchesSnapshot(), len(tt.priorBy), "no batch for the refused removal")
				assert.Len(t, f.intents(t), len(tt.priorBy), "no intent for the refused removal")
				assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, target.URI).Status, "the target stays accepted")
				assert.EqualValues(t, postDBSeededVersion, f.postVersion(t, target.URI), "the target's version is unchanged")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, append(wantActions, localPostAction("remove", rosterModerator, false, target.URI, postDBReasonSpam)),
				f.postActions(t), "the removal proceeds and is recorded")
			assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, target.URI).Status, "the target is removed")
		})
	}
}

// I6: after a local removal, the firehose copy of its commit (the removal
// create and the acceptance delete at the commit rev, the removal carrying the
// CID the PDS returned) is no new change: one action naming the moderator, one
// version advance in total. A second delivery changes nothing either.
func TestCommunityPostActionsDB_FirehoseCopyOfALocalRemoval(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	copyOfCommit := func() {
		t.Helper()
		require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, post.SubjectRkey, rosterHeadOneRev,
			postRemovalWrittenCID, removalRecordBody(post, "spam", postDBRemovalReason, "2026-11-13T12:00:00.000Z"))))
		require.NoError(t, f.deliverPost(postStandEvent("delete", posts.AcceptanceCollection, post.SubjectRkey, rosterHeadOneRev, "", nil)))
	}
	want := []storedPostAction{localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam)}

	copyOfCommit()

	assert.Equal(t, want, f.postActions(t), "the firehose copy adds no action; the one action names the moderator")
	assert.EqualValues(t, 2, f.postVersion(t, post.URI), "the version advanced once in total")
	afterCopy := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusRemoved, afterCopy.Status)

	copyOfCommit()

	assert.Equal(t, want, f.postActions(t), "a replay adds no action")
	assert.EqualValues(t, 2, f.postVersion(t, post.URI), "a replay leaves the version")
	assert.Equal(t, afterCopy, f.admission(t, post.URI), "a replay leaves the admission row")
}
