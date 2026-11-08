package common

import (
	"context"
	"encoding/json"
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
	subscribed map[string]int
	blocked    bool
	blockErr   error
}

func (r *viewerStateRepository) GetSubscribedCommunityDIDs(
	context.Context,
	string,
	[]string,
) (map[string]int, error) {
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
	repo := &viewerStateRepository{subscribed: map[string]int{}}

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

func TestPopulateCommunityViewerState_ReportsContentVisibility(t *testing.T) {
	communityA := &communities.Community{DID: "did:plc:communitya"}
	communityB := &communities.Community{DID: "did:plc:communityb"}
	communityC := &communities.Community{DID: "did:plc:communityc"}
	repo := &viewerStateRepository{subscribed: map[string]int{
		"did:plc:communitya": 4,
		"did:plc:communityb": 1,
	}}

	PopulateCommunityViewerState(
		context.Background(),
		authenticatedRequest(),
		repo,
		[]*communities.Community{communityA, communityB, communityC},
	)

	viewerJSON := func(t *testing.T, c *communities.Community) map[string]any {
		t.Helper()
		require.NotNil(t, c.Viewer, "%s has no viewer state", c.DID)
		raw, err := json.Marshal(c.Viewer)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw, &out))
		return out
	}

	a := viewerJSON(t, communityA)
	b := viewerJSON(t, communityB)
	c := viewerJSON(t, communityC)

	assert.Equal(t, true, a["subscribed"], "community A subscribed")
	assert.Equal(t, float64(4), a["contentVisibility"], "community A contentVisibility")
	assert.Equal(t, true, b["subscribed"], "community B subscribed")
	assert.Equal(t, float64(1), b["contentVisibility"], "community B contentVisibility")
	assert.Equal(t, false, c["subscribed"], "community C subscribed")
	assert.NotContains(t, c, "contentVisibility", "unsubscribed community C must omit contentVisibility")
}
