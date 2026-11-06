package comments

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommentServiceGetCommentsAttributesAccountBanHideToInstance: a comment
// recorded hidden as account_ban (written during the ban) keeps naming this
// instance as its moderation source after the ban ends, when the commenter is
// no longer restricted, and names it once when a removal already does.
func TestCommentServiceGetCommentsAttributesAccountBanHideToInstance(t *testing.T) {
	instanceSource := []ModerationSourceView{{AuthorityDID: moderationViewInstanceDID, Scope: ModerationScopeView{Kind: "instance"}}}
	commenter := "did:plc:commentauthor"
	for _, tc := range []struct {
		name       string
		restricted bool
		removals   []RemovalSource
		viewer     *string
	}{
		{name: "after unban, anonymous viewer"},
		{name: "after unban, the commenter", viewer: &commenter},
		{name: "during the ban", restricted: true},
		{name: "instance removal already named", removals: []RemovalSource{{AuthorityDID: moderationViewInstanceDID, ScopeKind: "instance"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo, request, comment, _ := moderationViewFixture(t)
			hideReason := HideReasonAccountBan
			comment.HideReason = &hideReason
			comment.CommenterRestricted = tc.restricted
			if tc.removals != nil {
				repo.activeRemovalsByURI = map[string][]RemovalSource{comment.URI: tc.removals}
			}
			request.ViewerDID = tc.viewer
			userRepo := newMockUserRepo()
			userRepo.users[commenter] = createTestUser(commenter, "author.test")
			postRepo := newMockPostRepo()
			postRepo.posts[request.PostURI] = createTestPost(request.PostURI, "did:plc:postauthor", "did:plc:community")
			service := NewCommentService(repo, userRepo, postRepo, newMockCommunityRepo(), nil, nil, nil, WithInstanceDID(moderationViewInstanceDID))

			response, err := service.GetComments(context.Background(), request)
			require.NoError(t, err)
			require.Len(t, response.Comments, 1)
			view := response.Comments[0].Comment
			require.NotNil(t, view.DeletionReason)
			assert.Equal(t, DeletionReasonModerator, *view.DeletionReason)
			require.NotNil(t, view.Moderation, "the placeholder names its moderation source")
			assert.Equal(t, instanceSource, view.Moderation.Sources)
		})
	}
}
