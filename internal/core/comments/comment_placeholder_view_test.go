package comments

import (
	"Coves/internal/validation"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const placeholderTestCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"

// TestCommentService_buildThreadViews_PlaceholderContract pins PRD section 14.4's
// privacy-preserving deleted view for both deletion reasons. The placeholder
// must remain structurally valid without leaking content, profile details, vote
// totals, or viewer state while preserving the references needed for threading.
func TestCommentService_buildThreadViews_PlaceholderContract(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"),
		"deleted comment placeholders cannot prove their served shape without the repository lexicons")

	for _, deletionReason := range []string{DeletionReasonAuthor, DeletionReasonModerator} {
		t.Run(deletionReason, func(t *testing.T) {
			commentRepository := newMockCommentRepo()
			service := NewCommentService(
				commentRepository,
				newMockUserRepo(),
				newMockPostRepo(),
				newMockCommunityRepo(),
				nil,
				nil,
				nil,
			).(*commentService)

			rootURI := "at://did:plc:def456/social.coves.community.postv2/3k7a3dmb5bk2c"
			parentURI := "at://did:plc:def456/social.coves.community.comment/3k7a3dmb5bk2d"
			comment := createTestComment(
				"at://did:plc:abc123/social.coves.community.comment/3k7a3dmb5bk2c",
				"did:plc:abc123",
				"author.test",
				rootURI,
				parentURI,
				3,
			)
			deletedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			comment.DeletedAt = &deletedAt
			comment.DeletionReason = &deletionReason
			comment.CID = placeholderTestCID
			comment.RootCID = placeholderTestCID
			comment.ParentCID = placeholderTestCID
			comment.UpvoteCount = 9
			comment.DownvoteCount = 4
			comment.Score = 5
			embed := `{"$type":"social.coves.embed.post"}`
			comment.Embed = &embed
			viewerDID := "did:plc:viewer123"

			threadViews, err := service.buildThreadViews(
				context.Background(),
				[]*Comment{comment},
				0,
				"hot",
				&viewerDID,
			)
			require.NoError(t, err, "building a deleted comment placeholder must not fail the thread")
			require.Len(t, threadViews, 1, "a deleted comment must retain its place in the thread")
			view := threadViews[0].Comment
			require.NotNil(t, view, "the retained thread node must carry a comment placeholder")

			require.NotNil(t, view.Author, "the placeholder must retain its author's DID")
			assert.Equal(t, "did:plc:abc123", view.Author.DID,
				"the placeholder author must identify the deleted comment's author")
			assert.Equal(t, "handle.invalid", view.Author.Handle,
				"the placeholder must use the valid sentinel handle instead of an empty invalid handle")
			assert.Nil(t, view.Author.DisplayName, "a deleted placeholder must not leak the author's display name")
			assert.Nil(t, view.Author.Avatar, "a deleted placeholder must not leak the author's avatar")
			assert.Nil(t, view.Author.Reputation, "a deleted placeholder must not leak the author's reputation")

			assert.Nil(t, view.Record, "a deleted placeholder must not expose the verbatim comment record")
			assert.True(t, view.IsDeleted, "the placeholder must identify itself as deleted")
			require.NotNil(t, view.DeletionReason, "the placeholder must explain whether its author or a moderator deleted it")
			assert.Equal(t, deletionReason, *view.DeletionReason,
				"the placeholder must preserve the indexed deletion reason")
			assert.Nil(t, view.Embed, "a deleted placeholder must not expose embedded content")
			assert.Nil(t, view.Viewer, "a deleted placeholder must not expose viewer state")

			require.NotNil(t, view.Stats, "the placeholder must retain thread statistics")
			assert.Zero(t, view.Stats.Upvotes, "a deleted placeholder must hide its prior upvotes")
			assert.Zero(t, view.Stats.Downvotes, "a deleted placeholder must hide its prior downvotes")
			assert.Zero(t, view.Stats.Score, "a deleted placeholder must hide its prior score")
			assert.Equal(t, 3, view.Stats.ReplyCount,
				"the placeholder must preserve replyCount so clients can retain its descendants")
			assert.Equal(t, &CommentRef{URI: rootURI, CID: placeholderTestCID}, view.Post,
				"the placeholder must preserve its root post reference")
			assert.Equal(t, &CommentRef{URI: parentURI, CID: placeholderTestCID}, view.Parent,
				"the placeholder must preserve its immediate parent reference")

			serialized, err := json.Marshal(view)
			require.NoError(t, err, "the served placeholder must serialize to JSON")
			// atdata decoding narrows JSON numbers to integers the way the
			// AppView's own lexicon validation does; encoding/json would
			// yield float64 and fail every integer property.
			data, err := atdata.UnmarshalJSON(serialized)
			require.NoError(t, err, "the serialized placeholder must decode for lexicon validation")
			assert.NoError(t, validation.ValidateData(
				catalog,
				data,
				"social.coves.community.comment.defs#commentView",
				lexicon.AllowLenientDatetime,
			), "the exact serialized placeholder served by the AppView must satisfy commentView")
		})
	}
}
