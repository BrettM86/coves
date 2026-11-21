//go:build integration

package moderation_test

import (
	"errors"
	"testing"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Recovery of community comment removals and restores against real Postgres:
// the PDS commits the moderator's write and the response is lost. Whichever of
// the same-key retry and the commit's firehose copy settles it first, the
// moderator's change is recorded once; the other then changes nothing.

// I7 with Round 1 #1: a removal (the comment unremoved, version 0) or a
// restore (an external spam removal standing at head one, version 1) by M
// commits and the response is lost. Settled by the retry first, or by the
// commit's firehose copy first: exactly one action names M, the version
// advanced once, the retry's result names that action, and no intent is left
// open.
func TestCommunityCommentRecoveryDB_LostResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		restore       bool
		firehoseFirst bool
	}{
		{name: "removal recovered by the same-key retry first"},
		{name: "removal whose firehose copy arrives before the retry", firehoseFirst: true},
		{name: "restore recovered by the same-key retry first", restore: true},
		{name: "restore whose firehose delete copy arrives before the retry", restore: true, firehoseFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			thread := f.seedThread(t)
			comment := thread.Comment

			var (
				version    int64
				wantAction storedPostAction
				act        func() (*moderation.CommunityActionResult, error)
				copyEvent  *jetstream.JetstreamEvent
			)
			if tt.restore {
				f.standExternalCommentRemoval(t, comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
					commentRemovalCID, commentRemovalBody(comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
				version = 1
				wantAction = localCommentAction("retract-removal", rosterModerator, false, comment.URI, commentDBReasonSpam)
				act = func() (*moderation.CommunityActionResult, error) {
					return f.restoreComment(rosterModerator, comment.URI, version, "restore-comment-1")
				}
				// The restore's commit lands at head two.
				copyEvent = postStandEvent("delete", posts.RemovalCollection, comment.SubjectRkey, rosterHeadTwoRev, "", nil)
			} else {
				f.setCommentRemovalWriteCID(comment, commentDBRemovalBodyCID)
				wantAction = localCommentAction("apply-removal", rosterModerator, false, comment.URI, commentDBReasonSpam)
				act = func() (*moderation.CommunityActionResult, error) {
					return f.removeComment(rosterModerator, comment.URI, "spam", commentRemovalReason, version, "remove-comment-1")
				}
				// The removal's commit lands at head one.
				copyEvent = postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey, rosterHeadOneRev,
					commentDBRemovalBodyCID, commentRemovalBody(comment, "spam", commentRemovalReason, postDBIntentCreatedAt))
			}
			actionsBefore := f.commentActions(t, comment.URI)
			idsBefore := f.actionIDs(t)
			servedBefore := f.servedComment(t, thread.Post.URI, comment.URI)

			f.repo.mu.Lock()
			f.repo.applyThenErr = errors.New("pds: connection reset by peer")
			f.repo.mu.Unlock()
			_, err := act()

			assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
			assert.Equal(t, 1, f.openIntents(t), "the intent stays open")
			assert.Empty(t, newCommentActions(actionsBefore, f.commentActions(t, comment.URI)), "no action yet")
			assert.Equal(t, version, f.commentVersion(t, comment.URI), "the version is unchanged")
			assert.Equal(t, servedBefore.Moderation, f.servedComment(t, thread.Post.URI, comment.URI).Moderation,
				"the comment is served as before")

			// assertSettled: one action names M, the version advanced once, and
			// the comment is served as M's change left it.
			assertSettled := func(when string) {
				t.Helper()
				assert.Equal(t, []storedPostAction{wantAction}, newCommentActions(actionsBefore, f.commentActions(t, comment.URI)),
					"%s: exactly one action names the moderator", when)
				assert.Equal(t, version+1, f.commentVersion(t, comment.URI), "%s: the version advanced once", when)
				served := f.servedComment(t, thread.Post.URI, comment.URI)
				if tt.restore {
					assert.Nil(t, served.Moderation, "%s: the comment is served with no removal", when)
					assert.Equal(t, commentDBContent, served.Content, "%s: the comment is served with its content", when)
				} else {
					assert.Equal(t, communityRemovedModeration(), served.Moderation, "%s: the comment is served removed by the community", when)
				}
			}

			if tt.firehoseFirst {
				require.NoError(t, f.deliverPost(copyEvent), "the firehose copy is handled")
				assertSettled("after the firehose copy")
				assert.Equal(t, 0, f.openIntents(t), "the copy completes the open intent")
			}

			result, err := act()

			require.NoError(t, err, "the same-key retry settles the lost commit")
			require.NotNil(t, result, "the retry returns its result")
			assertSettled("after the retry")
			ids := newActionIDs(idsBefore, f.actionIDs(t))
			if assert.Len(t, ids, 1, "one action was written") {
				assert.Equal(t, ids[0], result.ActionID, "the retry's result names the one action")
			}
			assert.Equal(t, version+1, result.Version, "the retry's result carries the version after the change")
			assert.Equal(t, commentAlphaRemovalURI, result.RecordURI, "the result names the removal record")
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second batch")

			if !tt.firehoseFirst {
				require.NoError(t, f.deliverPost(copyEvent), "the delayed firehose copy is handled")
				assertSettled("after the delayed firehose copy")
				assert.Equal(t, ids, newActionIDs(idsBefore, f.actionIDs(t)), "the copy adds no action")
			}
		})
	}
}

// I7c with Round 1 #2: an external spam removal is indexed at head one (rev
// gate R1); M's restore is guarded by head one and commits at head two, and
// the response is lost. The same-key retry completes it at the guard rev,
// equal to the gate already held: the comment is served, one retract-removal
// names M, and the gate holds R1. The real commit's delete copy at R2 then adds
// nothing; a later removal written outside Coves at R3 applies with its own
// actorless action.
func TestCommunityCommentRecoveryDB_RestoreCompletedAtTheGateRev(t *testing.T) {
	t.Parallel()
	f := newCommunityCommentDBFixture(t)
	thread := f.seedThread(t)
	comment := thread.Comment
	f.standExternalCommentRemoval(t, comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	require.Equal(t, "3krosterrev01", f.recordGateRev(t, commentAlphaRemovalURI), "fixture: the gate holds the removal's rev")
	actionsBefore := f.commentActions(t, comment.URI)

	f.repo.mu.Lock()
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	f.repo.mu.Unlock()
	_, err := f.restoreComment(rosterModerator, comment.URI, 1, "restore-comment-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	result, err := f.restoreComment(rosterModerator, comment.URI, 1, "restore-comment-1")

	require.NoError(t, err, "the same-key retry completes the restore")
	require.NotNil(t, result, "the retry returns its result")
	served := f.servedComment(t, thread.Post.URI, comment.URI)
	assert.Nil(t, served.Moderation, "the comment is served with no removal")
	assert.Equal(t, commentDBContent, served.Content, "the comment is served with its content")
	retract := localCommentAction("retract-removal", rosterModerator, false, comment.URI, commentDBReasonSpam)
	assert.Equal(t, []storedPostAction{retract}, newCommentActions(actionsBefore, f.commentActions(t, comment.URI)),
		"one retract-removal names the moderator")
	assert.Equal(t, "3krosterrev01", f.recordGateRev(t, commentAlphaRemovalURI),
		"the completion stamps the guard rev, equal to the gate already held")
	assert.EqualValues(t, 2, f.commentVersion(t, comment.URI), "the restore advanced the version once")
	active, _ := f.commentDecision(t, comment.URI)
	assert.False(t, active, "the community's decision is inactive")

	// The real commit's delete copy, at head two's rev.
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, comment.SubjectRkey, "3krosterrev02", "", nil)))

	assert.Equal(t, []storedPostAction{retract}, newCommentActions(actionsBefore, f.commentActions(t, comment.URI)),
		"the delete copy adds no action")
	assert.EqualValues(t, 2, f.commentVersion(t, comment.URI), "the delete copy advances no version")
	assert.Nil(t, f.servedComment(t, thread.Post.URI, comment.URI).Moderation, "the comment stays served")

	// A later removal written outside Coves is a new change.
	const laterRemovalCID = "bafyreiacommentremovallaterexternalaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey, "3krosterrev03",
		laterRemovalCID, commentRemovalBody(comment, "harassment", "", "2026-11-13T13:00:00Z"))))

	assert.ElementsMatch(t, []storedPostAction{
		retract,
		externalCommentAction(rosterCommunity, "apply-removal", comment.URI, "social.coves.moderation.defs#reasonHarassment"),
	}, newCommentActions(actionsBefore, f.commentActions(t, comment.URI)),
		"the later removal records its own actorless action with the mapped reason")
	assert.EqualValues(t, 3, f.commentVersion(t, comment.URI), "the later removal advances the version")
	assert.Equal(t, communityRemovedModeration(), f.servedComment(t, thread.Post.URI, comment.URI).Moderation,
		"the later removal applies")
	assert.Equal(t, "3krosterrev03", f.recordGateRev(t, commentAlphaRemovalURI), "the gate holds the later removal's rev")
}

// Round 2 #1 and #3: the removal is rewritten outside Coves (spam to doxing)
// and not yet indexed when the restore's batch deletes the doxing record, and
// the batch's response is lost. The commit's firehose delete arrives before
// the rewrite's: it does not match the intent (whose batch deleted the
// rewritten CID, not the indexed one), so it names no actor, logs the indexed
// spam code and advances the gate past the restore's guard. The same-key
// retry proves the delete committed and attributes that retract-removal to the
// moderator with the doxing code its batch deleted: one action for the
// restore, the version advanced once.
func TestCommunityCommentRecoveryDB_RestoreOfAnUnindexedRewriteWhoseDeleteArrivesFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityCommentDBFixture(t)
	thread := f.seedThread(t)
	comment := thread.Comment
	f.standExternalCommentRemoval(t, comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
	before := len(f.commentActions(t, comment.URI))

	// The rewrite lands right after the writer's first head read, as in
	// TestCommunityCommentActionsDB_RestoreLogsTheCodeItsBatchDeleted; the
	// batch then commits at head three and its response is lost.
	f.repo.mu.Lock()
	f.repo.afterHeadRead = func(r *fakeRosterRepo) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.afterHeadRead = func(r *fakeRosterRepo) {
			r.setPostRecord(posts.RemovalCollection, comment.SubjectRkey, commentDBRewrittenCID,
				commentRemovalBody(comment, "doxing", "", commentDBExternalCreatedAt))
			r.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
		}
	}
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	f.repo.mu.Unlock()
	_, err := f.restoreComment(rosterModerator, comment.URI, 1, "restore-comment-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
	require.False(t, f.repo.hasRecord(posts.RemovalCollection, comment.SubjectRkey), "fixture: the batch deleted the rewritten record")

	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, comment.SubjectRkey,
		rosterHeadThreeRev, "", nil)), "the batch's firehose delete is handled")
	require.Equal(t, []storedPostAction{
		externalCommentAction(rosterCommunity, "retract-removal", comment.URI, commentDBReasonSpam),
	}, f.commentActions(t, comment.URI)[before:], "fixture: the delete logs an actorless retract-removal with the indexed code")

	result, err := f.restoreComment(rosterModerator, comment.URI, 1, "restore-comment-1")

	require.NoError(t, err, "the same-key retry settles the lost commit")
	require.NotNil(t, result, "the retry returns its result")
	assert.Equal(t, []storedPostAction{
		localCommentAction("retract-removal", rosterModerator, false, comment.URI,
			"social.coves.moderation.defs#reasonDoxing"),
	}, f.commentActions(t, comment.URI)[before:],
		"one retract-removal names the moderator with the deleted record's code")
	assert.EqualValues(t, 2, f.commentVersion(t, comment.URI), "the version advanced once in total")
	assert.EqualValues(t, 2, result.Version, "the retry's result carries the version after the change")
	active, exists := f.commentDecision(t, comment.URI)
	assert.True(t, exists)
	assert.False(t, active, "the decision is inactive")
	assert.Nil(t, f.servedComment(t, thread.Post.URI, comment.URI).Moderation, "the comment is served with no removal")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}
