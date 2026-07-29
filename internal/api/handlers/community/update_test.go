package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The update handler's contract with a client: which fields reach the service,
// who the service is told is making the change, and what a success answers.
//
// This was the last community write endpoint with no handler test at all. Its
// only coverage was tests/integration/community_e2e_test.go's "Update via XRPC
// endpoint" subtest — a real PDS, a real Jetstream and a provisioned community
// to establish that three fields round-tripped — so deleting that file left
// HandleUpdate uninvoked by anything. The service half (write-forward to the
// community's repo, creator-only authorization) is
// TestCommunityService_UpdateWithRealPDS; the pipeline half (a directly-written
// update reaching a serving endpoint) is TestCommunityProfileIngestion in
// tests/e2e. What is missing without this file is the HTTP boundary between
// them, which is where a renamed JSON key or a dropped optional field lives.

// updateTestService reuses mockCommunityService's stubs (create_test.go) and
// overrides only the method the update handler calls.
type updateTestService struct {
	*mockCommunityService
	update func(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error)
}

func (s *updateTestService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return s.update(ctx, req)
}

// updatedBy runs one update request as userDID (empty for an unauthenticated
// caller) and returns the service request it produced alongside the recorder.
func updatedBy(t *testing.T, userDID string, body any) (communities.UpdateCommunityRequest, *httptest.ResponseRecorder) {
	t.Helper()

	var forwarded communities.UpdateCommunityRequest
	handler := NewUpdateHandler(&updateTestService{
		mockCommunityService: &mockCommunityService{},
		update: func(_ context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
			forwarded = req
			return &communities.Community{
				DID:       req.CommunityDID,
				Handle:    "c-gaming.coves.social",
				RecordURI: "at://" + req.CommunityDID + "/social.coves.community.profile/self",
				RecordCID: "bafyupdated",
				// Seeded so the response assertion can prove the handler serves a
				// hand-built envelope rather than the entity: a future refactor
				// that encodes the community directly would put these on the wire.
				PDSPassword:     "hunter2",
				PDSAccessToken:  "access-jwt",
				PDSRefreshToken: "refresh-jwt",
			}, nil
		},
	})

	var encoded []byte
	switch payload := body.(type) {
	case string:
		encoded = []byte(payload)
	default:
		var err error
		encoded, err = json.Marshal(payload)
		require.NoError(t, err, "encoding the request body")
	}

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.update", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if userDID != "" {
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, userDID))
	}

	w := httptest.NewRecorder()
	handler.HandleUpdate(w, req)
	return forwarded, w
}

func TestUpdateHandler_ForwardsEveryChangedField(t *testing.T) {
	t.Parallel()

	// Every optional field is a pointer, because nil and "" mean different
	// things to the service: nil keeps the stored value, a pointer to "" clears
	// it. A handler that flattened them would silently erase a description on
	// every update that did not mention one.
	forwarded, w := updatedBy(t, "did:plc:author", map[string]any{
		"communityDid":           "did:plc:community",
		"displayName":            "Gaming Renamed",
		"description":            "now with more games",
		"visibility":             "unlisted",
		"allowExternalDiscovery": false,
		"moderationType":         "sortition",
		"contentWarnings":        []string{"nsfw"},
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "did:plc:community", forwarded.CommunityDID)
	require.NotNil(t, forwarded.DisplayName)
	assert.Equal(t, "Gaming Renamed", *forwarded.DisplayName)
	require.NotNil(t, forwarded.Description)
	assert.Equal(t, "now with more games", *forwarded.Description)
	require.NotNil(t, forwarded.Visibility)
	assert.Equal(t, "unlisted", *forwarded.Visibility)
	require.NotNil(t, forwarded.AllowExternalDiscovery)
	assert.False(t, *forwarded.AllowExternalDiscovery,
		"a false allowExternalDiscovery must survive as false, not as absent")
	require.NotNil(t, forwarded.ModerationType)
	assert.Equal(t, "sortition", *forwarded.ModerationType)
	assert.Equal(t, []string{"nsfw"}, forwarded.ContentWarnings)
}

func TestUpdateHandler_OmittedFieldsStayNil(t *testing.T) {
	t.Parallel()

	// The other half of the pointer contract: a request that mentions only the
	// display name must leave everything else nil, so the service keeps the
	// stored values instead of overwriting them with zeroes.
	forwarded, w := updatedBy(t, "did:plc:author", map[string]any{
		"communityDid": "did:plc:community",
		"displayName":  "Only This",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, forwarded.DisplayName)
	assert.Equal(t, "Only This", *forwarded.DisplayName)
	assert.Nil(t, forwarded.Description)
	assert.Nil(t, forwarded.Visibility)
	assert.Nil(t, forwarded.AllowExternalDiscovery)
	assert.Nil(t, forwarded.ModerationType)
	assert.Nil(t, forwarded.ContentWarnings)
}

func TestUpdateHandler_DerivesTheUpdaterFromTheSession(t *testing.T) {
	t.Parallel()

	// DEFINED BEHAVIOUR, and it is not the create handler's: a client-supplied
	// updatedByDid is OVERWRITTEN with the session's DID rather than refused
	// with a 400 (update.go: `req.UpdatedByDID = userDID`, unconditionally).
	// Either policy is defensible — what matters is that the value the service
	// authorizes against can only come from the session, which is what this
	// pins. The asymmetry with create's explicit rejection is worth knowing
	// about before anyone "makes them consistent" in one direction or the other.
	forwarded, w := updatedBy(t, "did:plc:realauthor", map[string]any{
		"communityDid": "did:plc:community",
		"updatedByDid": "did:plc:someoneelse",
		"displayName":  "Renamed",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "did:plc:realauthor", forwarded.UpdatedByDID,
		"the service must authorize against the session's DID, never the body's")
}

func TestUpdateHandler_RequiresAuth(t *testing.T) {
	t.Parallel()

	forwarded, w := updatedBy(t, "", map[string]any{
		"communityDid": "did:plc:community",
		"displayName":  "Renamed",
	})

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "AuthRequired", decodeXRPCError(t, w).Error)
	assert.Empty(t, forwarded.CommunityDID, "the handler called the service for an unauthenticated request")
}

func TestUpdateHandler_RejectsIncompleteRequests(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body any
	}{
		{name: "no communityDid", body: map[string]any{"displayName": "Renamed"}},
		{name: "empty communityDid", body: map[string]any{"communityDid": "", "displayName": "Renamed"}},
		{name: "malformed body", body: "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forwarded, w := updatedBy(t, "did:plc:author", tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "InvalidRequest", decodeXRPCError(t, w).Error)
			assert.Empty(t, forwarded.CommunityDID, "the handler forwarded an invalid request to the service")
		})
	}
}

func TestUpdateHandler_ServiceErrors(t *testing.T) {
	t.Parallel()

	// The two that a client can actually provoke, mapped by errors.go: a
	// community that is gone, and one the caller does not own. A 403 arriving as
	// a 500 would tell a client to retry an update it will never be allowed to
	// make.
	for _, tc := range []struct {
		name     string
		err      error
		status   int
		xrpcName string
	}{
		{name: "unknown community", err: communities.ErrCommunityNotFound, status: http.StatusNotFound, xrpcName: "NotFound"},
		{name: "not the creator", err: communities.ErrUnauthorized, status: http.StatusForbidden, xrpcName: "Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler := NewUpdateHandler(&updateTestService{
				mockCommunityService: &mockCommunityService{},
				update: func(context.Context, communities.UpdateCommunityRequest) (*communities.Community, error) {
					return nil, tc.err
				},
			})

			body, err := json.Marshal(map[string]any{"communityDid": "did:plc:community", "displayName": "Renamed"})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.update", bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, "did:plc:author"))

			w := httptest.NewRecorder()
			handler.HandleUpdate(w, req)

			require.Equal(t, tc.status, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tc.xrpcName, decodeXRPCError(t, w).Error)
		})
	}
}

func TestUpdateHandler_SuccessResponse(t *testing.T) {
	t.Parallel()

	_, w := updatedBy(t, "did:plc:author", map[string]any{
		"communityDid": "did:plc:community",
		"displayName":  "Renamed",
	})
	require.Equal(t, http.StatusOK, w.Code)

	assertRecordWriteEnvelope(t, w, map[string]string{
		"uri":    "at://did:plc:community/social.coves.community.profile/self",
		"cid":    "bafyupdated",
		"did":    "did:plc:community",
		"handle": "c-gaming.coves.social",
	})
}

func TestUpdateHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	handler := NewUpdateHandler(&updateTestService{
		mockCommunityService: &mockCommunityService{},
		update: func(context.Context, communities.UpdateCommunityRequest) (*communities.Community, error) {
			t.Error("the service was called for a GET")
			return nil, nil
		},
	})

	w := httptest.NewRecorder()
	handler.HandleUpdate(w, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.update", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// assertRecordWriteEnvelope checks a write endpoint's success body against the
// EXACT key set its lexicon promises.
//
// Exact, not a subset: the community entity these handlers build their response
// from carries the community's PDS password and both of its tokens, so the
// interesting failure is not a missing key but an extra one. A handler that
// grew a field — or was refactored to encode the entity directly — would still
// satisfy every "the uri is right" assertion while putting credentials on the
// wire. The fixtures seed those three secrets precisely so this can catch it.
func assertRecordWriteEnvelope(t *testing.T, w *httptest.ResponseRecorder, expected map[string]string) {
	t.Helper()

	var response map[string]any
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &response),
		"decoding the response: %q", w.Body.String())

	keys := make([]string, 0, len(response))
	for key := range response {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, []string{"uri", "cid", "did", "handle"}, keys,
		"the response key set is the lexicon's output shape, exactly: %s", w.Body.String())

	for key, want := range expected {
		assert.Equalf(t, want, response[key], "response field %q", key)
	}

	for _, secret := range []string{"hunter2", "access-jwt", "refresh-jwt"} {
		assert.NotContainsf(t, w.Body.String(), secret,
			"a community PDS credential (%s) reached a client", secret)
	}
}
