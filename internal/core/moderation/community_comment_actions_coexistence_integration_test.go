//go:build integration

package moderation_test

import (
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community removal and an instance removal of the same comment are
// independent decisions. The moderator acts through the community action
// service; the instance admin acts with instance authority through the
// instance moderation service, whose migration-080 outbox mirrors the removal
// into the community's repo when no removal stands there. Both write into one
// fake community repo over one database; outcomes are what getComments serves
// and the records standing in the community repo.

var (
	coexistInstanceSource  = comments.ModerationSourceView{AuthorityDID: rosterInstanceDID, Scope: comments.ModerationScopeView{Kind: "instance"}}
	coexistCommunitySource = comments.ModerationSourceView{
		AuthorityDID: rosterCommunity,
		Scope:        comments.ModerationScopeView{Kind: "community", CommunityDID: rosterCommunity},
	}
)

// I9 rows:
//
//	(a) the moderator removes, the admin removes; the moderator restores → the
//	    comment stays removed, attributed to the instance
//	(b) the moderator removes, the admin removes; the admin restores → the
//	    comment stays removed, attributed to the community
//	(c) the admin removes (the outbox writes the community's removal record,
//	    which the firehose indexes); the moderator restores, deleting that
//	    record → the comment stays removed by the instance decision
//	(d) the admin removes (outbox record indexed); the moderator rewrites the
//	    record to off-topic; the admin restores → the community's removal stays
//	    in force with the moderator's record standing
//	(e) the admin removes (outbox record indexed); the moderator removes with
//	    the outbox record's own code and no reason; the admin restores → the
//	    community's removal stays in force: the moderator's removal took the
//	    record over rather than finding it already applied
//	(f) as (e), but the moderator restores → the instance removal stays in
//	    force
//
// In every row an instance restore sends no batch to the community repo, and
// once both removals are restored the comment is served with its content.
func TestCommunityCommentActionsDB_CommunityAndInstanceRemovalsAreIndependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// instanceFirst: the admin removes before the moderator acts, and the
		// outbox's removal record is indexed from the firehose.
		instanceFirst bool
		// moderatorRemoves: the moderator writes a removal (a rewrite of the
		// outbox's record when instanceFirst).
		moderatorRemoves bool
		// sameCode: over the outbox's record, the moderator removes with its
		// code (spam) and no reason instead of rewriting to off-topic.
		sameCode          bool
		communityRestores bool // the moderator restores first; else the admin
		wantAfterFirst    []comments.ModerationSourceView
		// wantRecordCID is the removal record standing in the community repo
		// after the first restore, "" for none.
		wantRecordCID string
	}{
		{
			name:             "(a) moderator then admin remove, moderator restores",
			moderatorRemoves: true, communityRestores: true,
			wantAfterFirst: []comments.ModerationSourceView{coexistInstanceSource},
		},
		{
			name:             "(b) moderator then admin remove, admin restores",
			moderatorRemoves: true,
			wantAfterFirst:   []comments.ModerationSourceView{coexistCommunitySource},
			wantRecordCID:    commentRemovalWrittenCID,
		},
		{
			name:          "(c) admin removes through the outbox, moderator restores",
			instanceFirst: true, communityRestores: true,
			wantAfterFirst: []comments.ModerationSourceView{coexistInstanceSource},
		},
		{
			name:          "(d) admin removes through the outbox, moderator rewrites, admin restores",
			instanceFirst: true, moderatorRemoves: true,
			wantAfterFirst: []comments.ModerationSourceView{coexistCommunitySource},
			wantRecordCID:  coexistRewrittenRemovalCID,
		},
		{
			name:          "(e) admin removes through the outbox, moderator removes with its code and no reason, admin restores",
			instanceFirst: true, moderatorRemoves: true, sameCode: true,
			wantAfterFirst: []comments.ModerationSourceView{coexistCommunitySource},
			wantRecordCID:  coexistRewrittenRemovalCID,
		},
		{
			name:          "(f) admin removes through the outbox, moderator removes with its code and no reason, moderator restores",
			instanceFirst: true, moderatorRemoves: true, sameCode: true, communityRestores: true,
			wantAfterFirst: []comments.ModerationSourceView{coexistInstanceSource},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			thread := f.seedThread(t)
			comment := thread.Comment
			subject := seededCommunityPost{URI: comment.URI, CID: comment.CID}
			instance := f.instanceModeration(t)

			moderatorRemoves := func() {
				t.Helper()
				code, reason, cid := "spam", commentRemovalReason, commentRemovalWrittenCID
				switch {
				case tt.instanceFirst && tt.sameCode:
					reason, cid = "", coexistRewrittenRemovalCID
				case tt.instanceFirst:
					code, cid = "off-topic", coexistRewrittenRemovalCID
				}
				f.setCommentRemovalWriteCID(comment, cid)
				result, err := f.removeComment(rosterModerator, comment.URI, code, reason,
					f.commentVersion(t, comment.URI), "coexist-community-remove")
				require.NoError(t, err)
				require.NotNil(t, result, "the moderator's removal returns its result")
				assert.False(t, result.AlreadyApplied, "the moderator's removal is the community's own decision")
				assert.Contains(t, f.commentActions(t, comment.URI),
					localCommentAction("apply-removal", rosterModerator, false, comment.URI, moderation.CommunityRemovalActionReason(code)),
					"the moderator's removal is logged")
				removal := f.standRecord(posts.RemovalCollection, comment.SubjectRkey)
				require.NotNil(t, removal, "the moderator's removal record stands in the community repo")
				require.Equal(t, cid, removal.CID, "the standing removal is the moderator's")
				assert.Equal(t, code, removal.Value["code"], "the removal record carries the moderator's code")
			}

			var adminRemoval *moderation.Action
			if tt.instanceFirst {
				f.setCommitHead(coexistHeadOne)
				f.setCommentRemovalWriteCID(comment, coexistInstanceRemovalCID)
				adminRemoval = instance.removeContent(t, subject, "coexist-instance-remove")
				outboxRemoval := f.standRecord(posts.RemovalCollection, comment.SubjectRkey)
				require.NotNil(t, outboxRemoval, "fixture: the outbox wrote the community's removal record")
				require.Equal(t, coexistInstanceRemovalCID, outboxRemoval.CID, "fixture: the outbox's removal record")
				require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey,
					coexistHeadOne.Rev, outboxRemoval.CID, outboxRemoval.Value)), "fixture: the firehose indexes the outbox's record")
				if tt.moderatorRemoves {
					f.setCommitHead(coexistHeadTwo)
					moderatorRemoves()
				}
			} else {
				f.setCommitHead(coexistHeadOne)
				moderatorRemoves()
				f.setCommitHead(coexistHeadTwo)
				adminRemoval = instance.removeContent(t, subject, "coexist-instance-remove")
				removal := f.standRecord(posts.RemovalCollection, comment.SubjectRkey)
				require.NotNil(t, removal)
				require.Equal(t, commentRemovalWrittenCID, removal.CID, "the outbox leaves the moderator's removal alone")
			}
			both := f.servedComment(t, thread.Post.URI, comment.URI).Moderation
			if assert.NotNil(t, both, "the comment is served removed while both removals stand") {
				assert.Contains(t, both.Sources, coexistInstanceSource, "the instance removal is in force")
			}

			moderatorRestores := func() {
				t.Helper()
				f.setCommitHead(coexistHeadThree)
				restored, err := f.restoreComment(rosterModerator, comment.URI, f.commentVersion(t, comment.URI), "coexist-community-restore")
				require.NoError(t, err)
				require.NotNil(t, restored, "the moderator's restore returns its result")
			}
			adminRestores := func() {
				t.Helper()
				batchesBefore := len(f.repo.batchesSnapshot())
				f.setCommitHead(coexistHeadFour)
				instance.restoreContent(t, subject, adminRemoval, "coexist-instance-restore")
				instance.drain(t)
				assert.Len(t, f.repo.batchesSnapshot(), batchesBefore, "the instance restore sends no batch to the community repo")
			}

			if tt.communityRestores {
				moderatorRestores()
			} else {
				adminRestores()
			}

			served := f.servedComment(t, thread.Post.URI, comment.URI)
			if assert.NotNil(t, served.Moderation, "the remaining removal keeps the comment removed") {
				assert.Equal(t, "removed", served.Moderation.State)
				assert.Equal(t, tt.wantAfterFirst, served.Moderation.Sources, "attributed to the remaining authority")
			}
			assert.NotEqual(t, commentDBContent, served.Content, "the placeholder does not serve the content")
			removal := f.standRecord(posts.RemovalCollection, comment.SubjectRkey)
			if tt.wantRecordCID == "" {
				assert.Nil(t, removal, "no removal record stands in the community repo")
			} else if assert.NotNil(t, removal, "the community's removal record still stands") {
				assert.Equal(t, tt.wantRecordCID, removal.CID)
			}

			if tt.communityRestores {
				adminRestores()
			} else {
				moderatorRestores()
			}

			final := f.servedComment(t, thread.Post.URI, comment.URI)
			assert.Nil(t, final.Moderation, "with both removals restored the comment is served with no removal")
			assert.Equal(t, commentDBContent, final.Content, "the comment is served with its content")
			assert.Nil(t, f.standRecord(posts.RemovalCollection, comment.SubjectRkey), "no removal record stands")
		})
	}
}
