package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The get handler's contract with a client: which identifier reaches the
// service, what a missing or unknown community answers, and the shape of a
// success.
//
// These came down a tier from tests/integration/community_e2e_test.go's
// "Get via XRPC endpoint" subtest, which needed a real PDS, a real Jetstream
// and a provisioned community to assert that a DID round-trips. The behaviour
// under test is the handler's, so it belongs here where it costs nothing and
// every case can be covered; the pipeline half of it — that a community indexed
// from the firehose is really served by the running AppView — is
// TestCommunityProfileIngestion in tests/e2e.

// getTestService reuses mockCommunityService's stubs (create_test.go) and
// overrides only the method the get handler calls, so this file adds one method
// rather than a second copy of the thirty-method Service interface.
type getTestService struct {
	*mockCommunityService
	get func(ctx context.Context, identifier string) (*communities.Community, error)
}

func (s *getTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return s.get(ctx, identifier)
}

// getTestRepo is the viewer-state repository. The get handler hands it every
// community it is about to serve; listTestRepo (list_test.go) already
// implements the interface as a no-op, which is what an unauthenticated request
// needs — viewer state for a request with no user is nil either way.
func getTestRepo() communities.Repository { return &listTestRepo{} }

type failingBlockStateRepo struct {
	communities.Repository
	err error
}

func (r *failingBlockStateRepo) GetSubscribedCommunityDIDs(
	context.Context,
	string,
	[]string,
) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (r *failingBlockStateRepo) IsBlocked(context.Context, string, string) (bool, error) {
	return false, r.err
}

func newGetHandler(t *testing.T, get func(ctx context.Context, identifier string) (*communities.Community, error)) *GetHandler {
	t.Helper()
	return NewGetHandler(&getTestService{mockCommunityService: &mockCommunityService{}, get: get}, getTestRepo())
}

func TestGetHandler_PassesTheIdentifierThrough(t *testing.T) {
	t.Parallel()

	// Every identifier form social.coves.community.get accepts. The handler must
	// not interpret any of them — resolution is
	// communities.ResolveCommunityIdentifier's job, and a handler that
	// normalised, lowercased or stripped a prefix on the way past would break
	// the scoped form without any service-level test noticing.
	for _, identifier := range []string{
		"did:plc:abc123",
		"c-gaming.coves.social",
		"@c-gaming.coves.social",
		"!gaming@coves.social",
	} {
		t.Run(identifier, func(t *testing.T) {
			t.Parallel()

			var seen string
			handler := newGetHandler(t, func(_ context.Context, id string) (*communities.Community, error) {
				seen = id
				return &communities.Community{DID: "did:plc:abc123", Handle: "c-gaming.coves.social", Name: "gaming"}, nil
			})

			w := httptest.NewRecorder()
			handler.HandleGet(w, httptest.NewRequest(http.MethodGet,
				"/xrpc/social.coves.community.get?community="+url.QueryEscape(identifier), nil))

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, identifier, seen, "the handler altered the identifier before resolving it")
		})
	}
}

func TestGetHandler_MissingCommunityParameter(t *testing.T) {
	t.Parallel()

	handler := newGetHandler(t, func(context.Context, string) (*communities.Community, error) {
		t.Error("the service was called for a request with no community parameter")
		return nil, nil
	})

	w := httptest.NewRecorder()
	handler.HandleGet(w, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get", nil))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "InvalidRequest", decodeXRPCError(t, w).Error)
}

func TestGetHandler_UnknownCommunityIsAnXRPCNotFound(t *testing.T) {
	t.Parallel()

	// The error name matters as much as the status. testkit.IsNotFound — which
	// every pipeline wait uses to tell "not indexed yet" from "the endpoint is
	// broken" — only treats a 404 as not-found when the body is an XRPC error
	// envelope, so a bare http.Error here would make the whole T2 tier wait out
	// its budget on a community that will never arrive.
	handler := newGetHandler(t, func(context.Context, string) (*communities.Community, error) {
		return nil, communities.ErrCommunityNotFound
	})

	w := httptest.NewRecorder()
	handler.HandleGet(w, httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.get?community=did:plc:missing", nil))

	require.Equal(t, http.StatusNotFound, w.Code)
	body := decodeXRPCError(t, w)
	assert.Equal(t, "NotFound", body.Error)
	assert.NotEmpty(t, body.Message)
}

func TestGetHandler_ServiceFailureIsNotA404(t *testing.T) {
	t.Parallel()

	// A datastore that is down must not be reported as a community that does not
	// exist: a client would cache the absence, and a pipeline wait would treat it
	// as "not yet" and hide the outage for the length of its budget.
	handler := newGetHandler(t, func(context.Context, string) (*communities.Community, error) {
		return nil, errors.New("connection refused")
	})

	w := httptest.NewRecorder()
	handler.HandleGet(w, httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.get?community=did:plc:abc123", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestGetHandler_BlockStateFailureIsNotRenderedAsUnblocked(t *testing.T) {
	t.Parallel()

	repositoryFailure := errors.New("community block lookup failed")
	repo := &failingBlockStateRepo{Repository: getTestRepo(), err: repositoryFailure}
	handler := NewGetHandler(&getTestService{
		mockCommunityService: &mockCommunityService{},
		get: func(context.Context, string) (*communities.Community, error) {
			return &communities.Community{
				DID:    "did:plc:abc123",
				Handle: "c-gaming.coves.social",
				Name:   "gaming",
				Viewer: nil,
			}, nil
		},
	}, repo)

	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.get?community=did:plc:abc123", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), "did:plc:viewer"))
	w := httptest.NewRecorder()
	handler.HandleGet(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "InternalServerError", decodeXRPCError(t, w).Error)
	assert.NotContains(t, w.Body.String(), repositoryFailure.Error())
}

func TestGetHandler_ServesTheDetailedView(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	handler := newGetHandler(t, func(context.Context, string) (*communities.Community, error) {
		return &communities.Community{
			DID:             "did:plc:abc123",
			Handle:          "c-gaming.coves.social",
			Name:            "gaming",
			DisplayName:     "Gaming",
			Description:     "games and the playing of them",
			CreatedByDID:    "did:plc:creator",
			HostedByDID:     "did:web:coves.social",
			Visibility:      "public",
			SubscriberCount: 7,
			MemberCount:     3,
			PostCount:       11,
			CreatedAt:       created,
			// Credentials are on the entity and must never reach a client. The
			// view type is what keeps them off the wire, so a test that serves a
			// community carrying them is the one place that can prove it.
			PDSPassword:     "hunter2",
			PDSAccessToken:  "access-jwt",
			PDSRefreshToken: "refresh-jwt",
		}, nil
	})

	w := httptest.NewRecorder()
	handler.HandleGet(w, httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.get?community=did:plc:abc123", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var view map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &view))

	assert.Equal(t, "did:plc:abc123", view["did"])
	assert.Equal(t, "c-gaming.coves.social", view["handle"])
	assert.Equal(t, "gaming", view["name"])
	assert.Equal(t, "Gaming", view["displayName"])
	assert.Equal(t, "games and the playing of them", view["description"])
	assert.Equal(t, "did:plc:creator", view["createdBy"])
	assert.Equal(t, "did:web:coves.social", view["hostedBy"])
	assert.Equal(t, "public", view["visibility"])
	assert.EqualValues(t, 7, view["subscriberCount"])
	assert.EqualValues(t, 3, view["memberCount"])
	assert.EqualValues(t, 11, view["postCount"])

	// An unauthenticated request has no viewer state, rather than a viewer
	// object full of defaults that a client would read as "not subscribed".
	assert.NotContains(t, view, "viewer")

	assert.NotContains(t, w.Body.String(), "hunter2", "the community's PDS password reached a client")
	assert.NotContains(t, w.Body.String(), "access-jwt", "the community's PDS access token reached a client")
	assert.NotContains(t, w.Body.String(), "refresh-jwt", "the community's PDS refresh token reached a client")
}

// xrpcError is the {error, message} envelope every XRPC failure carries.
type xrpcError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// decodeXRPCError reads that envelope, failing the test when the body is not
// one — which is the interesting case: a handler that answers with plain text
// is invisible to clients (and to testkit) that match on the error name.
func decodeXRPCError(t *testing.T, w *httptest.ResponseRecorder) xrpcError {
	t.Helper()
	var body xrpcError
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &body),
		"expected an XRPC error envelope, got %q", w.Body.String())
	require.NotEmptyf(t, body.Error, "expected an XRPC error name, got %q", w.Body.String())
	return body
}

func TestGetHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	handler := newGetHandler(t, func(context.Context, string) (*communities.Community, error) {
		t.Error("the service was called for a POST")
		return nil, nil
	})

	w := httptest.NewRecorder()
	handler.HandleGet(w, httptest.NewRequest(http.MethodPost,
		"/xrpc/social.coves.community.get?community=did:plc:abc123", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
