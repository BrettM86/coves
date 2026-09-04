package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type viewerStateRepository struct {
	communities.Repository
	subscribed map[string]bool
	blocked    bool
	blockErr   error
}

func (r *viewerStateRepository) GetSubscribedCommunityDIDs(
	context.Context,
	string,
	[]string,
) (map[string]bool, error) {
	return r.subscribed, nil
}

func (r *viewerStateRepository) IsBlocked(context.Context, string, string) (bool, error) {
	return r.blocked, r.blockErr
}

func authenticatedRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := middleware.SetTestUserDID(req.Context(), "did:plc:viewer")
	return req.WithContext(ctx)
}

func TestPopulateCommunityViewerState_MergesExistingFields(t *testing.T) {
	blocked := true
	member := true
	community := &communities.Community{
		DID: "did:plc:community",
		Viewer: &communities.CommunityViewerState{
			Blocked: &blocked,
			Member:  &member,
		},
	}
	repo := &viewerStateRepository{subscribed: map[string]bool{community.DID: false}}

	PopulateCommunityViewerState(
		context.Background(),
		authenticatedRequest(),
		repo,
		[]*communities.Community{community},
	)

	require.NotNil(t, community.Viewer)
	require.NotNil(t, community.Viewer.Subscribed)
	assert.False(t, *community.Viewer.Subscribed)
	require.NotNil(t, community.Viewer.Blocked)
	assert.True(t, *community.Viewer.Blocked, "subscription enrichment erased viewer.blocked")
	require.NotNil(t, community.Viewer.Member)
	assert.True(t, *community.Viewer.Member, "subscription enrichment erased viewer.member")
}

func TestPopulateCommunityBlockState_PropagatesLookupFailure(t *testing.T) {
	cause := errors.New("repository unavailable")
	community := &communities.Community{DID: "did:plc:community"}

	err := PopulateCommunityBlockState(
		context.Background(),
		authenticatedRequest(),
		&viewerStateRepository{blockErr: cause},
		community,
	)

	require.ErrorIs(t, err, cause)
	assert.Nil(t, community.Viewer, "an unknown block state must not be serialized as false")
}
