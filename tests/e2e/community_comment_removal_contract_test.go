//go:build e2e

package e2e

import (
	"context"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommunityCommentRemovalIngestion proves a community can take down a
// comment in a thread it hosts by writing a removal record to its own repo, and
// undo it by deleting that record. Every write goes to the PDS; every
// observation is social.coves.community.comment.getComments.
//
//	removal record → the comment is a content-free "moderator" placeholder
//	                 attributed to the community, and its reply survives under it
//	record deleted → the comment is served again with its original text
func TestCommunityCommentRemovalIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "ccr")
	commenter := p.IndexedAccount(t, "ccrc")
	community := indexedCommunity(t, p, "ccr", author.DID)
	post := indexedPost(t, p, community, author, "a thread its community moderates")

	const commentText = "a comment the community will take down"
	const replyText = "a reply that outlives its parent's removal"

	commentRKey := testkit.TID()
	commentAt := commentURI(commenter.DID, commentRKey)
	commentRec := commenter.PutRecord(t, commentCollection, commentRKey, commentRecord(post, post, commentText))
	comment := strongRef{URI: commentAt, CID: commentRec.CID}

	replyRKey := testkit.TID()
	replyAt := commentURI(commenter.DID, replyRKey)
	commenter.PutRecord(t, commentCollection, replyRKey, commentRecord(post, comment, replyText))

	observe := func(description string, accept func(threadNode) bool) threadNode {
		t.Helper()
		var observed threadNode
		p.Await(t, description, func() (bool, error) {
			thread, err := p.Thread(context.Background(), post.URI, nil)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			node, ok := thread.find(commentAt)
			if !ok {
				return false, nil
			}
			observed = node
			return accept(node), nil
		}, withReadCadence())
		return observed
	}
	hasReply := func(n threadNode) bool {
		reply, ok := findNode(n.Replies, replyAt)
		return ok && reply.Comment.Record["content"] == replyText
	}

	observe("the comment and its reply to be served before any removal", func(n threadNode) bool {
		return n.Comment.Record["content"] == commentText && hasReply(n)
	})

	// ---- the community removes the comment -----------------------------------
	removalRKey := subjectRkey(commentAt)
	community.PutRecord(t, removalCollection, removalRKey, removalRecord(commentAt, commentRec.CID, "rule-violation"))

	p.FreshReadQuota(t, "community-comment-removal")
	removed := observe("the community's removal to render the comment as a moderator placeholder", func(n threadNode) bool {
		return n.Comment.IsDeleted && n.Comment.Moderation != nil && hasReply(n)
	})
	require.NotNil(t, removed.Comment.DeletionReason)
	assert.Equal(t, "moderator", *removed.Comment.DeletionReason)
	assert.Nil(t, removed.Comment.Record, "a removed comment's record must not be served")
	require.NotNil(t, removed.Comment.Moderation)
	assert.Equal(t, "removed", removed.Comment.Moderation.State)
	require.Len(t, removed.Comment.Moderation.Sources, 1, "the removal must be attributed to exactly one source")
	source := removed.Comment.Moderation.Sources[0]
	assert.Equal(t, community.DID, source.AuthorityDID)
	assert.Equal(t, "community", source.Scope.Kind)
	assert.Equal(t, community.DID, source.Scope.CommunityDID)

	// ---- the community withdraws the removal ---------------------------------
	community.DeleteExistingRecord(t, removalCollection, removalRKey)

	p.FreshReadQuota(t, "community-comment-restore")
	restored := observe("deleting the removal record to serve the comment again", func(n threadNode) bool {
		return !n.Comment.IsDeleted && n.Comment.Record["content"] == commentText
	})
	assert.Nil(t, restored.Comment.Moderation, "a withdrawn removal must leave no moderation state")
	assert.Nil(t, restored.Comment.DeletionReason)
	assert.True(t, hasReply(restored), "the reply stays under the restored comment")
}
