package moderation_test

import (
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
)

func TestCommunityPostViewerActions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		authority       moderation.CommunityViewerAuthority
		admissionStatus string
		authorHoldsRole bool
		want            []string
	}{
		{name: "moderator on an accepted post by an author without a role", authority: moderation.CommunityAuthorityModerator, admissionStatus: "accepted", want: []string{"remove", "ban-author"}},
		{name: "moderator on a community-removed post", authority: moderation.CommunityAuthorityModerator, admissionStatus: "removed", want: []string{"restore", "ban-author"}},
		{name: "moderator on a pending post", authority: moderation.CommunityAuthorityModerator, admissionStatus: "pending", want: []string{"remove", "ban-author"}},
		{name: "moderator on a post pending reacceptance", authority: moderation.CommunityAuthorityModerator, admissionStatus: "pending_reacceptance", want: []string{"remove", "ban-author"}},
		{name: "moderator on a rejected post", authority: moderation.CommunityAuthorityModerator, admissionStatus: "rejected", want: []string{"ban-author"}},
		{name: "moderator on a post with no admission row", authority: moderation.CommunityAuthorityModerator, admissionStatus: "", want: []string{"ban-author"}},
		{name: "moderator on a rejected post whose author holds a role", authority: moderation.CommunityAuthorityModerator, admissionStatus: "rejected", authorHoldsRole: true, want: nil},
		{name: "moderator on a post with no admission row whose author holds a role", authority: moderation.CommunityAuthorityModerator, admissionStatus: "", authorHoldsRole: true, want: nil},
		{name: "viewer without community authority on a post with no admission row", authority: moderation.CommunityAuthorityNone, admissionStatus: "", want: nil},
		{name: "moderator on an accepted post whose author holds a role", authority: moderation.CommunityAuthorityModerator, admissionStatus: "accepted", authorHoldsRole: true, want: []string{"remove"}},
		{name: "moderator on a removed post whose author holds a role", authority: moderation.CommunityAuthorityModerator, admissionStatus: "removed", authorHoldsRole: true, want: []string{"restore"}},
		{name: "viewer without community authority on an accepted post", authority: moderation.CommunityAuthorityNone, admissionStatus: "accepted", want: nil},
		{name: "viewer without community authority on a removed post", authority: moderation.CommunityAuthorityNone, admissionStatus: "removed", want: nil},
		{name: "owner on an accepted post", authority: moderation.CommunityAuthorityOwner, admissionStatus: "accepted", want: []string{"remove", "ban-author"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moderation.CommunityPostViewerActions(tc.authority, tc.admissionStatus, tc.authorHoldsRole)
			if tc.want == nil {
				assert.Empty(t, got, "the viewer may take no community action, so viewer.communityModeration must be omitted")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommunityCommentViewerActions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		authority        moderation.CommunityViewerAuthority
		communityRemoved bool
		authorDeleted    bool
		authorHoldsRole  bool
		want             []string
	}{
		{name: "moderator on a live comment by an author without a role", authority: moderation.CommunityAuthorityModerator, want: []string{"remove", "ban-author"}},
		{name: "moderator on a community-removed comment", authority: moderation.CommunityAuthorityModerator, communityRemoved: true, want: []string{"restore", "ban-author"}},
		{name: "moderator on a comment deleted by its author", authority: moderation.CommunityAuthorityModerator, authorDeleted: true, want: nil},
		{name: "moderator on a live comment whose author holds a role", authority: moderation.CommunityAuthorityModerator, authorHoldsRole: true, want: []string{"remove"}},
		{name: "moderator on a community-removed comment whose author holds a role", authority: moderation.CommunityAuthorityModerator, communityRemoved: true, authorHoldsRole: true, want: []string{"restore"}},
		{name: "owner on a live comment", authority: moderation.CommunityAuthorityOwner, want: []string{"remove", "ban-author"}},
		{name: "viewer without community authority on a live comment", authority: moderation.CommunityAuthorityNone, want: nil},
		{name: "viewer without community authority on a community-removed comment", authority: moderation.CommunityAuthorityNone, communityRemoved: true, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moderation.CommunityCommentViewerActions(tc.authority, tc.communityRemoved, tc.authorDeleted, tc.authorHoldsRole)
			if tc.want == nil {
				assert.Empty(t, got, "the viewer may take no community action, so viewer.communityModeration must be omitted")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommunityViewerActions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authority moderation.CommunityViewerAuthority
		want      []string
	}{
		{name: "owner", authority: moderation.CommunityAuthorityOwner, want: []string{"ban-account", "unban-account", "grant-role", "remove-role", "transfer-ownership"}},
		{name: "moderator", authority: moderation.CommunityAuthorityModerator, want: []string{"ban-account", "unban-account"}},
		{name: "no community authority", authority: moderation.CommunityAuthorityNone, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moderation.CommunityViewerActions(tc.authority)
			if tc.want == nil {
				assert.Empty(t, got, "the viewer may take no community action, so viewer.communityModeration must be omitted")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
