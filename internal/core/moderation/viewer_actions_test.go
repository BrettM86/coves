package moderation_test

import (
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
)

const (
	viewerActionsInstanceDID  = "did:web:coves.example"
	viewerActionsOtherDID     = "did:web:other-instance.example"
	viewerActionsCommunityDID = "did:plc:viewercommunity"
)

func TestPostViewerActions(t *testing.T) {
	instanceSource := posts.ModerationSourceView{
		AuthorityDID: viewerActionsInstanceDID,
		Scope:        posts.ModerationScopeView{Kind: "instance"},
	}
	otherAuthoritySource := posts.ModerationSourceView{
		AuthorityDID: viewerActionsOtherDID,
		Scope:        posts.ModerationScopeView{Kind: "instance"},
	}
	communityScopedSource := posts.ModerationSourceView{
		AuthorityDID: viewerActionsInstanceDID,
		Scope:        posts.ModerationScopeView{Kind: "community", CommunityDID: viewerActionsCommunityDID},
	}
	nsfwFrom := func(source posts.ModerationSourceView) *posts.ModerationView {
		return &posts.ModerationView{
			State:         "clear",
			ContentLabels: []posts.ContentLabelView{{Value: "nsfw", Sources: []posts.ModerationSourceView{source}}},
		}
	}

	for _, tc := range []struct {
		name       string
		moderation *posts.ModerationView
		want       []string
	}{
		{name: "live post without labels", moderation: nil, want: []string{"remove", "label"}},
		{name: "live post labeled nsfw by this instance", moderation: nsfwFrom(instanceSource), want: []string{"remove", "retract-label"}},
		{name: "live post labeled nsfw only by another authority", moderation: nsfwFrom(otherAuthoritySource), want: []string{"remove", "label"}},
		{name: "live post labeled nsfw by this instance DID at community scope", moderation: nsfwFrom(communityScopedSource), want: []string{"remove", "label"}},
		{
			name:       "post removed by this instance",
			moderation: &posts.ModerationView{State: "removed", Sources: []posts.ModerationSourceView{instanceSource}},
			want:       []string{"restore"},
		},
		{
			name: "post removed by this instance and labeled nsfw by this instance",
			moderation: &posts.ModerationView{
				State:         "removed",
				Sources:       []posts.ModerationSourceView{instanceSource},
				ContentLabels: []posts.ContentLabelView{{Value: "nsfw", Sources: []posts.ModerationSourceView{instanceSource}}},
			},
			want: []string{"restore"},
		},
		{
			name:       "post removed solely by another authority",
			moderation: &posts.ModerationView{State: "removed", Sources: []posts.ModerationSourceView{otherAuthoritySource}},
			want:       nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moderation.PostViewerActions(viewerActionsInstanceDID, tc.moderation)
			if tc.want == nil {
				assert.Empty(t, got, "the viewer may take no action, so viewer.moderation must be omitted")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommentViewerActions(t *testing.T) {
	author, moderator := comments.DeletionReasonAuthor, comments.DeletionReasonModerator
	removedBy := func(authorityDID string) *comments.ModerationView {
		return &comments.ModerationView{
			State: "removed",
			Sources: []comments.ModerationSourceView{{
				AuthorityDID: authorityDID,
				Scope:        comments.ModerationScopeView{Kind: "instance"},
			}},
		}
	}
	const commentURI = "at://did:plc:commenter/social.coves.community.comment/3kcomment"

	for _, tc := range []struct {
		name string
		view *comments.CommentView
		want []string
	}{
		{name: "live comment", view: &comments.CommentView{URI: commentURI}, want: []string{"remove"}},
		{
			name: "comment removed by this instance",
			view: &comments.CommentView{URI: commentURI, IsDeleted: true, DeletionReason: &moderator, Moderation: removedBy(viewerActionsInstanceDID)},
			want: []string{"restore"},
		},
		{
			name: "author-deleted comment also removed by this instance",
			view: &comments.CommentView{URI: commentURI, IsDeleted: true, DeletionReason: &author, Moderation: removedBy(viewerActionsInstanceDID)},
			want: []string{"restore"},
		},
		{
			name: "author-deleted comment without removal",
			view: &comments.CommentView{URI: commentURI, IsDeleted: true, DeletionReason: &author},
			want: nil,
		},
		{
			name: "comment removed solely by another authority",
			view: &comments.CommentView{URI: commentURI, IsDeleted: true, DeletionReason: &moderator, Moderation: removedBy(viewerActionsOtherDID)},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moderation.CommentViewerActions(viewerActionsInstanceDID, tc.view)
			if tc.want == nil {
				assert.Empty(t, got, "the viewer may take no action, so viewer.moderation must be omitted")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
