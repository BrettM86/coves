package comments

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GetActorComments' viewer DID is a SECURITY parameter on the community-filtered
// path, not a personalization one.
//
// The repository resolves each comment's root through the read-path visibility
// predicate when a community is named (PRD §6.2), and that predicate's author
// branch is `p.author_did = $viewer`: the bound viewer is what decides whether
// the caller may see comments rooted at pending / rejected / removed posts. A
// service that forgot to thread it would silently downgrade every authenticated
// author to an anonymous read — the author's own pending threads vanish from
// their own profile — while threading the WRONG string would hand a caller
// another author's unadmitted content. Both directions are pinned here, at T0,
// because the repository cannot tell a dropped viewer from a genuinely anonymous
// one.
func TestGetActorComments_ViewerDIDReachesTheRepository(t *testing.T) {
	const actorDID = "did:plc:actorwithcomments"
	const viewerDID = "did:plc:authenticatedviewer"

	capture := func(t *testing.T, viewer *string) ListByCommenterRequest {
		t.Helper()

		commentRepo := newMockCommentRepo()
		var got ListByCommenterRequest
		commentRepo.listByCommenterWithCursorFunc = func(ctx context.Context, req ListByCommenterRequest) ([]*Comment, *string, error) {
			got = req
			return []*Comment{}, nil, nil
		}

		service := NewCommentService(commentRepo, newMockUserRepo(), newMockPostRepo(), newMockCommunityRepo(), nil, nil, nil)
		_, err := service.GetActorComments(context.Background(), &GetActorCommentsRequest{
			ActorDID:  actorDID,
			Community: "did:plc:somecommunity",
			ViewerDID: viewer,
			Limit:     50,
		})
		require.NoError(t, err)
		return got
	}

	t.Run("an authenticated viewer is forwarded verbatim", func(t *testing.T) {
		t.Parallel()

		viewer := viewerDID
		got := capture(t, &viewer)

		assert.Equal(t, viewerDID, got.ViewerDID,
			"the authenticated viewer never reached the repository, so the community-filtered listing runs as an "+
				"anonymous read and an author loses their own pending threads from their own profile")
	})

	t.Run("an anonymous request forwards an empty viewer", func(t *testing.T) {
		t.Parallel()

		got := capture(t, nil)

		assert.Equal(t, "", got.ViewerDID,
			"an unauthenticated request must bind an EMPTY viewer DID: any non-empty value unlocks the visibility "+
				"predicate's author carve-out for whoever that DID names")
	})

	t.Run("the community filter is resolved alongside it", func(t *testing.T) {
		t.Parallel()

		viewer := viewerDID
		got := capture(t, &viewer)

		require.NotNil(t, got.CommunityDID, "a DID-form community must be forwarded as the filter")
		assert.Equal(t, "did:plc:somecommunity", *got.CommunityDID)
		assert.Equal(t, actorDID, got.CommenterDID)
	})
}
