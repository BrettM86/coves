//go:build integration

package moderation_test

import (
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community comment remove and restore against real Postgres: the real
// community action store, comment resolver, comment-removal index and role
// authorizer, the community's PDS repo faked. Threads are indexed through the
// production comment consumer; outcomes are read back through getComments,
// the modlog tables, the comment's community version and the rev gate.

const (
	commentDBReasonSpam          = "social.coves.moderation.defs#reasonSpam"
	commentDBReasonRuleViolation = "social.coves.moderation.defs#reasonRuleViolation"

	// commentDBRewrittenCID is a removal record CID the index never saw.
	commentDBRewrittenCID = "bafyreiacommentremovalrewrittenaaaaaaaaaaaaaaaaaaaaaaaa"
)

// I1, I6: a moderator, or an instance admin with no role, removes a comment
// with code spam and a reason. Before RemoveComment returns, getComments
// serves the comment as a moderator-removed placeholder attributed to the
// community while its reply is still served; one apply-removal records it;
// the community decision is active; the comment's version advanced by one and
// the removal record's rev gate holds the commit rev.
func TestCommunityCommentActionsDB_LocalRemove(t *testing.T) {
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
			f := newCommunityCommentDBFixture(t)
			thread := f.seedThread(t)
			require.Equal(t, commentDBContent, f.servedComment(t, thread.Post.URI, thread.Comment.URI).Content,
				"fixture: the comment is served with its content")

			result, err := f.removeComment(tt.actorDID, thread.Comment.URI, "spam", commentRemovalReason, 0, "remove-comment-1")

			require.NoError(t, err)
			require.NotNil(t, result, "RemoveComment returns its result")
			served := f.servedComment(t, thread.Post.URI, thread.Comment.URI)
			assert.Equal(t, communityRemovedModeration(), served.Moderation,
				"the comment is served removed, attributed to the community")
			assert.Equal(t, "moderator", served.DeletionReason, "the placeholder reads as a moderator removal")
			assert.NotEqual(t, commentDBContent, served.Content, "the placeholder does not serve the content")
			assert.Equal(t, []string{thread.ReplyURI}, served.ReplyURIs, "the reply is still served under the placeholder")
			assert.Equal(t, commentDBReplyContent, f.servedComment(t, thread.Post.URI, thread.ReplyURI).Content,
				"the reply keeps its content")
			assert.Equal(t, []storedPostAction{
				localCommentAction("apply-removal", tt.actorDID, tt.adminActing, thread.Comment.URI, commentDBReasonSpam),
			}, f.commentActions(t, thread.Comment.URI), "one apply-removal records the removal")
			active, exists := f.commentDecision(t, thread.Comment.URI)
			assert.True(t, exists && active, "the community's removal decision is active")
			assert.EqualValues(t, 1, f.commentVersion(t, thread.Comment.URI), "the comment's version advanced by one")
			assert.EqualValues(t, 1, result.Version, "the result carries the comment's version after the removal")
			assert.Equal(t, commentAlphaRemovalURI, result.RecordURI, "the result names the removal record")
			assert.Equal(t, rosterHeadOneRev, f.recordGateRev(t, commentAlphaRemovalURI),
				"the removal record's rev gate holds the commit rev")
		})
	}
}

// I2: the moderator restores a comment the community removed. After
// RestoreComment returns, getComments serves the comment with its content and
// no removal; one retract-removal names the moderator with the removal's
// mapped reason; the decision is inactive; the version advanced by one and the
// rev gate holds the restore's commit rev.
func TestCommunityCommentActionsDB_LocalRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityCommentDBFixture(t)
	thread := f.seedThread(t)
	f.standExternalCommentRemoval(t, thread.Comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	before := len(f.commentActions(t, thread.Comment.URI))

	result, err := f.restoreComment(rosterModerator, thread.Comment.URI, 1, "restore-comment-1")

	require.NoError(t, err)
	require.NotNil(t, result, "RestoreComment returns its result")
	served := f.servedComment(t, thread.Post.URI, thread.Comment.URI)
	assert.Nil(t, served.Moderation, "the comment is served with no removal")
	assert.Empty(t, served.DeletionReason)
	assert.Equal(t, commentDBContent, served.Content, "the comment is served with its content")
	assert.Equal(t, []storedPostAction{
		localCommentAction("retract-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonSpam),
	}, f.commentActions(t, thread.Comment.URI)[before:], "one retract-removal names the moderator with the removal's mapped reason")
	active, exists := f.commentDecision(t, thread.Comment.URI)
	assert.True(t, exists, "the community's removal decision is kept")
	assert.False(t, active, "the community's removal decision is inactive")
	assert.EqualValues(t, 2, f.commentVersion(t, thread.Comment.URI), "the comment's version advanced by one")
	assert.EqualValues(t, 2, result.Version)
	assert.Equal(t, commentAlphaRemovalURI, result.RecordURI, "the result names the removal record")
	assert.Equal(t, rosterHeadTwoRev, f.recordGateRev(t, commentAlphaRemovalURI),
		"the removal record's rev gate holds the restore's commit rev")
}

// I3: the community a comment action authorizes against is the one its
// parent chain reaches through the production resolver. A comment whose root
// names rosteralpha's post but whose parent is rosterbeta's post is not
// rosteralpha's to moderate; a comment whose parent is not indexed resolves
// nowhere. Either way nothing is written.
func TestCommunityCommentActionsDB_ResolutionFollowsTheParentChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rkey    string
		parent  func(f *communityCommentDBFixture, t *testing.T) (uri, cid string)
		wantErr error
	}{
		{
			name: "root names rosteralpha's post, parent is rosterbeta's post",
			rkey: "3kcommentforged1",
			parent: func(f *communityCommentDBFixture, t *testing.T) (string, string) {
				insertRosterCommunity(t, f.db, rosterBeta, "rosterbeta.test", true)
				betaPost := f.indexAcceptedPost(t, rosterBeta, postDBSeedFor("3kpostbeta1"))
				return betaPost.URI, betaPost.CID
			},
			wantErr: moderation.ErrForbidden,
		},
		{
			name: "parent comment is not indexed",
			rkey: "3kcommentorphan1",
			parent: func(*communityCommentDBFixture, *testing.T) (string, string) {
				return commentDanglingURI, "bafyreiacommentdanglingaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantErr: moderation.ErrCommentNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			parentURI, parentCID := tt.parent(f, t)
			comment := f.indexComment(t, tt.rkey, "bafyreiacomment"+tt.rkey, "a comment", post.URI, post.CID, parentURI, parentCID, commentRemovalWrittenCID)

			_, err := f.removeComment(rosterModerator, comment.URI, "spam", commentRemovalReason, 0, "remove-comment-1")

			assert.ErrorIs(t, err, tt.wantErr)
			assert.Empty(t, f.repo.batchesSnapshot(), "no batch reaches the community repo")
			assert.Empty(t, f.intents(t), "no intent is stored")
			assert.Empty(t, f.commentActions(t, comment.URI), "no action is written")
			_, exists := f.commentDecision(t, comment.URI)
			assert.False(t, exists, "no decision is written")
			assert.EqualValues(t, 0, f.commentVersion(t, comment.URI), "the version is unchanged")
		})
	}
}

// I4: the firehose copy of a local action's commit, carrying the rev the PDS
// reported, is no new change: a removal create copy leaves one apply-removal
// naming the moderator and one version advance; a delete copy of a restore
// leaves one retract-removal, nothing on the post path, and the version where
// the restore put it. A second delivery changes nothing either.
func TestCommunityCommentActionsDB_FirehoseCopyOfALocalAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seed puts in place what the local action acts on, when anything.
		seed func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread)
		// act runs the local action.
		act         func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread)
		copyOf      func(thread commentDBThread) *jetstream.JetstreamEvent
		wantActions func(thread commentDBThread) []storedPostAction
		wantVersion int64
	}{
		{
			name: "removal create copy of a local removal",
			act: func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) {
				result, err := f.removeComment(rosterModerator, thread.Comment.URI, "spam", commentRemovalReason, 0, "remove-comment-1")
				require.NoError(t, err)
				require.NotNil(t, result, "RemoveComment returns its result")
			},
			copyOf: func(thread commentDBThread) *jetstream.JetstreamEvent {
				return postStandEvent("create", posts.RemovalCollection, thread.Comment.SubjectRkey, rosterHeadOneRev,
					commentRemovalWrittenCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, "2026-11-13T12:00:00.000Z"))
			},
			wantActions: func(thread commentDBThread) []storedPostAction {
				return []storedPostAction{
					localCommentAction("apply-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonSpam),
				}
			},
			wantVersion: 1,
		},
		{
			name: "removal delete copy of a local restore",
			seed: func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) {
				f.standExternalCommentRemoval(t, thread.Comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
					commentRemovalCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
			},
			act: func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) {
				result, err := f.restoreComment(rosterModerator, thread.Comment.URI, 1, "restore-comment-1")
				require.NoError(t, err)
				require.NotNil(t, result, "RestoreComment returns its result")
			},
			copyOf: func(thread commentDBThread) *jetstream.JetstreamEvent {
				return postStandEvent("delete", posts.RemovalCollection, thread.Comment.SubjectRkey, rosterHeadTwoRev, "", nil)
			},
			wantActions: func(thread commentDBThread) []storedPostAction {
				return []storedPostAction{
					localCommentAction("retract-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonSpam),
				}
			},
			wantVersion: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			thread := f.seedThread(t)
			if tt.seed != nil {
				tt.seed(t, f, thread)
			}
			communityBefore := len(communityActionsIn(t, f.db, rosterCommunity))
			commentBefore := len(f.commentActions(t, thread.Comment.URI))
			tt.act(t, f, thread)
			want := tt.wantActions(thread)

			for _, delivery := range []string{"the copy", "a replay"} {
				require.NoError(t, f.deliverPost(tt.copyOf(thread)), "%s is handled", delivery)

				all := communityActionsIn(t, f.db, rosterCommunity)
				require.GreaterOrEqual(t, len(all), communityBefore)
				assert.Len(t, all[communityBefore:], len(want), "after %s the community holds only the local action, nothing on the post path", delivery)
				actions := f.commentActions(t, thread.Comment.URI)
				require.GreaterOrEqual(t, len(actions), commentBefore)
				assert.Equal(t, want, actions[commentBefore:], "after %s the local action is the only one, naming the moderator", delivery)
				assert.EqualValues(t, tt.wantVersion, f.commentVersion(t, thread.Comment.URI), "after %s the version advanced once in total", delivery)
				assert.Empty(t, f.postActions(t), "after %s nothing is logged about the post", delivery)
			}
		})
	}
}

// I5: comment removals share the post removal limits, counted from the
// stored removals and open removal intents. Prior removals are made by the
// service at the window's start; the counted removals are attempted later on
// the injected clock.
func TestCommunityCommentActionsDB_SharedRemovalRateLimits(t *testing.T) {
	t.Parallel()
	type prior struct {
		kind         string // "post" or "comment"
		actorDID     string
		lostResponse bool // the PDS commits the write and the response is lost: the intent stays open
	}
	type attempt struct {
		kind     string
		actorDID string
		wantErr  error
	}
	tests := []struct {
		name      string
		limits    moderation.CommunityRemovalLimits
		priors    []prior
		attemptAt time.Duration // after the window's start
		attempts  []attempt
	}{
		{
			name:      "a post and a comment removal by the moderator fill the per-moderator limit",
			limits:    moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 100, Window: time.Hour},
			priors:    []prior{{kind: "post", actorDID: rosterModerator}, {kind: "comment", actorDID: rosterModerator}},
			attemptAt: 30 * time.Minute,
			attempts: []attempt{
				{kind: "comment", actorDID: rosterModerator, wantErr: moderation.ErrRateLimited},
				{kind: "post", actorDID: rosterModerator, wantErr: moderation.ErrRateLimited},
			},
		},
		{
			name:      "an open comment-remove intent counts toward the per-moderator limit",
			limits:    moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 100, Window: time.Hour},
			priors:    []prior{{kind: "post", actorDID: rosterModerator}, {kind: "comment", actorDID: rosterModerator, lostResponse: true}},
			attemptAt: 5 * time.Minute,
			attempts: []attempt{
				{kind: "comment", actorDID: rosterModerator, wantErr: moderation.ErrRateLimited},
			},
		},
		{
			name:      "admin-acting removals are not counted",
			limits:    moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 2, Window: time.Hour},
			priors:    []prior{{kind: "post", actorDID: rosterAdmin}, {kind: "comment", actorDID: rosterAdmin}},
			attemptAt: 30 * time.Minute,
			attempts: []attempt{
				{kind: "comment", actorDID: rosterModerator},
				{kind: "post", actorDID: rosterModerator},
			},
		},
		{
			name:      "removals older than the window are not counted",
			limits:    moderation.CommunityRemovalLimits{PerModerator: 2, PerCommunity: 100, Window: time.Hour},
			priors:    []prior{{kind: "post", actorDID: rosterModerator}, {kind: "comment", actorDID: rosterModerator}},
			attemptAt: 62 * time.Minute,
			attempts: []attempt{
				{kind: "comment", actorDID: rosterModerator},
			},
		},
		{
			name:      "a post removal by M and a comment removal by M2 fill the per-community limit",
			limits:    moderation.CommunityRemovalLimits{PerModerator: 100, PerCommunity: 2, Window: time.Hour},
			priors:    []prior{{kind: "post", actorDID: rosterModerator}, {kind: "comment", actorDID: rosterSecondModerator}},
			attemptAt: 30 * time.Minute,
			attempts: []attempt{
				{kind: "comment", actorDID: rosterOwner, wantErr: moderation.ErrRateLimited},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			service := f.serviceWithLimits(tt.limits)
			thread := f.seedThread(t)
			priorPost := f.seedAcceptedPost(t, postDBSeedFor("3kpostprior1"))
			targetPost := f.seedAcceptedPost(t, postDBSeedFor("3kposttarget1"))
			priorComment := f.indexComment(t, "3kcommentprior1", "bafyreiacommentprior1", "an earlier comment",
				thread.Post.URI, thread.Post.CID, thread.Post.URI, thread.Post.CID, "bafyreiacommentremovalprior1")
			targetComment := thread.Comment

			for i, p := range tt.priors {
				f.clock.Set(rosterClockStart.Add(time.Duration(i) * time.Minute))
				switch p.kind {
				case "post":
					_, err := removePostWith(service, p.actorDID, priorPost.URI, "spam", "", postDBSeededVersion, "prior-remove-post")
					require.NoError(t, err, "prior post removal")
				case "comment":
					if p.lostResponse {
						f.repo.mu.Lock()
						f.repo.applyThenErr = errors.New("connection reset by peer")
						f.repo.mu.Unlock()
						_, err := removeCommentWith(service, p.actorDID, priorComment.URI, "spam", "", 0, "prior-remove-comment")
						require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "prior comment removal: the lost response reads as unavailable")
						require.Equal(t, 1, f.openIntents(t), "prior comment removal: its intent stays open")
						continue
					}
					result, err := removeCommentWith(service, p.actorDID, priorComment.URI, "spam", "", 0, "prior-remove-comment")
					require.NoError(t, err, "prior comment removal")
					require.NotNil(t, result, "prior comment removal returns its result")
				}
			}
			f.clock.Set(rosterClockStart.Add(tt.attemptAt))

			for _, a := range tt.attempts {
				var err error
				var result *moderation.CommunityActionResult
				switch a.kind {
				case "post":
					result, err = removePostWith(service, a.actorDID, targetPost.URI, "spam", "", postDBSeededVersion, "remove-post-target")
				case "comment":
					result, err = removeCommentWith(service, a.actorDID, targetComment.URI, "spam", "", 0, "remove-comment-target")
				}
				if a.wantErr != nil {
					assert.ErrorIs(t, err, a.wantErr, "%s removal by %s", a.kind, a.actorDID)
					switch a.kind {
					case "post":
						assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, targetPost.URI).Status, "the target post stays accepted")
					case "comment":
						assert.Empty(t, f.commentActions(t, targetComment.URI), "nothing is logged about the target comment")
						assert.EqualValues(t, 0, f.commentVersion(t, targetComment.URI), "the target comment's version is unchanged")
					}
					continue
				}
				require.NoError(t, err, "%s removal by %s proceeds", a.kind, a.actorDID)
				require.NotNil(t, result, "%s removal by %s returns its result", a.kind, a.actorDID)
			}
		})
	}
}

// I10: a moderator rewrites an applied spam removal to off-topic. One new
// apply-removal names the moderator with the new code's mapped reason, the
// still-active decision points to it, and the version advances by one. A
// following restore writes one retract-removal reversing it, with the
// rewritten code's mapped reason, and the decision goes inactive.
func TestCommunityCommentActionsDB_RewriteThenRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityCommentDBFixture(t)
	thread := f.seedThread(t)
	f.standExternalCommentRemoval(t, thread.Comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	before := len(f.commentActions(t, thread.Comment.URI))

	rewrite, err := f.removeComment(rosterModerator, thread.Comment.URI, "off-topic", "", 1, "rewrite-comment-1")

	require.NoError(t, err)
	require.NotNil(t, rewrite, "RemoveComment returns its result")
	assert.False(t, rewrite.AlreadyApplied, "a changed code is a rewrite, not already applied")
	assert.Equal(t, []storedPostAction{
		localCommentAction("apply-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonRuleViolation),
	}, f.commentActions(t, thread.Comment.URI)[before:], "one apply-removal names the moderator with the new code's reason")
	active, _ := f.commentDecision(t, thread.Comment.URI)
	assert.True(t, active, "the decision stays active")
	assert.Equal(t, rewrite.ActionID, f.commentDecisionActionID(t, thread.Comment.URI), "the decision points to the rewrite's action")
	assert.EqualValues(t, 2, f.commentVersion(t, thread.Comment.URI), "the rewrite advanced the version by one")
	assert.EqualValues(t, 2, rewrite.Version)
	assert.Equal(t, communityRemovedModeration(), f.servedComment(t, thread.Post.URI, thread.Comment.URI).Moderation,
		"the comment stays removed")

	f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
	restore, err := f.restoreComment(rosterModerator, thread.Comment.URI, 2, "restore-comment-1")

	require.NoError(t, err)
	require.NotNil(t, restore, "RestoreComment returns its result")
	assert.Equal(t, []storedPostAction{
		localCommentAction("apply-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonRuleViolation),
		localCommentAction("retract-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonRuleViolation),
	}, f.commentActions(t, thread.Comment.URI)[before:], "one retract-removal with the rewritten code's reason")
	assert.Equal(t, rewrite.ActionID, f.reversedActionID(t, restore.ActionID), "the retract-removal reverses the rewrite")
	active, exists := f.commentDecision(t, thread.Comment.URI)
	assert.True(t, exists)
	assert.False(t, active, "the decision is inactive")
	assert.EqualValues(t, 3, f.commentVersion(t, thread.Comment.URI), "the restore advanced the version by one")
	assert.Nil(t, f.servedComment(t, thread.Post.URI, thread.Comment.URI).Moderation, "the comment is served with no removal")
}

// I12: with the current expectedVersion, a removal record in the community
// repo whose CID is not the one the index holds (one never indexed, or a
// standing removal rewritten outside Coves and not yet indexed) refuses the
// action with ErrStateConflict before anything is written.
func TestCommunityCommentActionsDB_RepoRecordDiffersFromTheIndex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// stand puts the records in place and returns the comment's current
		// version.
		stand func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) int64
		act   func(f *communityCommentDBFixture, thread commentDBThread, version int64) (*moderation.CommunityActionResult, error)
	}{
		{
			name: "remove while an unindexed removal stands in the repo",
			stand: func(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) int64 {
				f.repo.setPostRecord(posts.RemovalCollection, thread.Comment.SubjectRkey, commentDBRewrittenCID,
					commentRemovalBody(thread.Comment, "off-topic", "", commentDBExternalCreatedAt))
				return 0
			},
			act: func(f *communityCommentDBFixture, thread commentDBThread, version int64) (*moderation.CommunityActionResult, error) {
				return f.removeComment(rosterModerator, thread.Comment.URI, "spam", commentRemovalReason, version, "remove-comment-1")
			},
		},
		{
			name:  "restore while the indexed removal was rewritten in the repo",
			stand: standRewrittenRemoval,
			act: func(f *communityCommentDBFixture, thread commentDBThread, version int64) (*moderation.CommunityActionResult, error) {
				return f.restoreComment(rosterModerator, thread.Comment.URI, version, "restore-comment-1")
			},
		},
		{
			name:  "rewrite while the indexed removal was rewritten in the repo",
			stand: standRewrittenRemoval,
			act: func(f *communityCommentDBFixture, thread commentDBThread, version int64) (*moderation.CommunityActionResult, error) {
				return f.removeComment(rosterModerator, thread.Comment.URI, "harassment", "", version, "rewrite-comment-1")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			thread := f.seedThread(t)
			version := tt.stand(t, f, thread)
			actionsBefore := f.commentActions(t, thread.Comment.URI)
			activeBefore, existsBefore := f.commentDecision(t, thread.Comment.URI)

			_, err := tt.act(f, thread, version)

			assert.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Empty(t, f.repo.batchesSnapshot(), "no batch reaches the community repo")
			assert.Empty(t, f.intents(t), "no intent is stored")
			assert.Equal(t, actionsBefore, f.commentActions(t, thread.Comment.URI), "no action is written")
			activeAfter, existsAfter := f.commentDecision(t, thread.Comment.URI)
			assert.Equal(t, []bool{activeBefore, existsBefore}, []bool{activeAfter, existsAfter}, "the decision is unchanged")
			assert.Equal(t, version, f.commentVersion(t, thread.Comment.URI), "the version is unchanged")
		})
	}
}

// standRewrittenRemoval indexes an external spam removal of the comment, then
// rewrites the record in the repo to off-topic without the index seeing it.
func standRewrittenRemoval(t *testing.T, f *communityCommentDBFixture, thread commentDBThread) int64 {
	t.Helper()
	f.standExternalCommentRemoval(t, thread.Comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	f.repo.setPostRecord(posts.RemovalCollection, thread.Comment.SubjectRkey, commentDBRewrittenCID,
		commentRemovalBody(thread.Comment, "off-topic", "", commentDBExternalCreatedAt))
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
	return 1
}

// A removal rewritten outside Coves (spam to doxing) after the restore's
// CID check but before its batch, and not yet indexed when the restore
// completes: the batch deleted the doxing record, so the restore's
// retract-removal carries the doxing code's mapped reason, recorded on the
// batch's attempt, not the indexed spam code.
func TestCommunityCommentActionsDB_RestoreLogsTheCodeItsBatchDeleted(t *testing.T) {
	t.Parallel()
	f := newCommunityCommentDBFixture(t)
	thread := f.seedThread(t)
	f.standExternalCommentRemoval(t, thread.Comment, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		commentRemovalCID, commentRemovalBody(thread.Comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))
	f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
	before := len(f.commentActions(t, thread.Comment.URI))

	// The service's head read is the first; the writer's is the second. The
	// rewrite lands in the repo right after the writer's first head read, so
	// the service's CID check passed and the writer retries on the moved head.
	f.repo.mu.Lock()
	f.repo.afterHeadRead = func(r *fakeRosterRepo) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.afterHeadRead = func(r *fakeRosterRepo) {
			r.setPostRecord(posts.RemovalCollection, thread.Comment.SubjectRkey, commentDBRewrittenCID,
				commentRemovalBody(thread.Comment, "doxing", "", commentDBExternalCreatedAt))
			r.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
		}
	}
	f.repo.mu.Unlock()

	result, err := f.restoreComment(rosterModerator, thread.Comment.URI, 1, "restore-comment-1")

	require.NoError(t, err)
	require.NotNil(t, result, "RestoreComment returns its result")
	assert.False(t, f.repo.hasRecord(posts.RemovalCollection, thread.Comment.SubjectRkey), "the batch deleted the rewritten record")
	assert.Equal(t, []storedPostAction{
		localCommentAction("retract-removal", rosterModerator, false, thread.Comment.URI,
			"social.coves.moderation.defs#reasonDoxing"),
	}, f.commentActions(t, thread.Comment.URI)[before:], "the retract-removal carries the deleted record's code")
	active, exists := f.commentDecision(t, thread.Comment.URI)
	assert.True(t, exists)
	assert.False(t, active, "the decision is inactive")
	assert.EqualValues(t, 2, f.commentVersion(t, thread.Comment.URI), "the restore advanced the version by one")
}
