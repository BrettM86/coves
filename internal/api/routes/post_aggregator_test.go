package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// social.coves.community.post.create seen from the wire, with an AGGREGATOR as
// the authenticated principal.
//
// # WHY THIS IS A ROUTE TEST AND NOT A HANDLER TEST
//
// An aggregator posting is the one write path in the product that a non-human
// principal takes, and it is assembled from three pieces that live in three
// packages: RegisterPostRoutes decides the NSID is a POST behind RequireAuth,
// the auth middleware decides which DID the request is acting as, and
// CreateHandler decides what to do with the pair. Each piece has its own tests
// and none of them can see a mistake in the seam — a route registered without
// its middleware, a handler reading a different context key than the middleware
// writes, an error class the mapper does not know about — because each one
// stubs the other two.
//
// So this drives the real chi router, built by the real registration function,
// with a middleware that injects the principal the way DualAuthMiddleware does
// after it validates a service JWT (auth.go:486-488: the aggregator's DID under
// UserDIDKey, AuthMethodServiceJWT under AuthMethodKey). What is faked is the
// posts service, and only the posts service: it answers with the three outcomes
// an aggregator actually meets, and the assertions are about what a client sees
// for each.
//
// # WHAT IS STILL NOT PROVEN HERE, STATED PLAINLY
//
// The middleware itself. A real service JWT is minted by the PDS
// (com.atproto.server.getServiceAuth) and validated against the aggregators
// table, so "a valid JWT from a DID that is not a registered aggregator is
// refused" needs the running stack and a token, which is Phase-5 pre-work — it
// is the only credential the pipeline tier could ever construct for itself, and
// there is no testkit helper for it yet. Until then this test injects the
// principal that middleware would produce, which proves everything downstream
// of the decision and nothing about the decision.
//
// The behaviour behind the mapped errors — that the quota really stops the
// eleventh post, that a revoked authorization really refuses the next one — is
// proven against real Postgres in internal/core/posts/service_aggregator_test.go.
// These assertions came from tests/integration/aggregator_e2e_test.go, whose
// version drove the handler function directly and so never touched the router
// or the middleware it is registered with.

// aggregatorPrincipalDID is the bot doing the posting.
const aggregatorPrincipalDID = "did:plc:aaaaaaaaaaaaaggregator"

// stubPostService answers CreatePost with a canned outcome and records what it
// was asked for.
//
// The embedded interface is nil on purpose: if the create path grows a second
// service call, this panics rather than returning a zero value that quietly
// changes what the test proves.
type stubPostService struct {
	posts.Service

	response *posts.CreatePostResponse
	err      error

	received posts.CreatePostRequest
	calls    int
}

func (s *stubPostService) CreatePost(_ context.Context, _ *oauth.ClientSessionData, req posts.CreatePostRequest) (*posts.CreatePostResponse, error) {
	s.calls++
	s.received = req
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

func (s *stubPostService) UpdatePost(_ context.Context, _ *oauth.ClientSessionData, _ posts.UpdatePostRequest) (*posts.UpdatePostResponse, error) {
	return nil, nil
}

// serviceJWTPrincipal is the auth middleware DualAuthMiddleware becomes once a
// service JWT has been validated: it puts the aggregator's DID in the context
// and lets the request through.
type serviceJWTPrincipal struct{ did string }

func (m serviceJWTPrincipal) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := middleware.SetTestUserDID(r.Context(), m.did)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refusingAuth is the same middleware before a credential has been presented.
//
// Its refusal carries a marker header, and that detail is the whole test below:
// CreateHandler ALSO answers 401 when no DID reached the context, so a status
// code alone cannot tell "the middleware refused this" from "the route is
// unguarded and the handler noticed". The marker can.
type refusingAuth struct{}

// refusedByMiddleware marks a response written by refusingAuth rather than by
// the handler behind it.
const refusedByMiddleware = "X-Refused-By-Middleware"

func (refusingAuth) RequireAuth(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(refusedByMiddleware, "1")
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// postRouter builds the router exactly as cmd/server does, with auth as the
// write-path middleware.
//
// The OAuth middleware is constructed with nil collaborators because only its
// OptionalAuth is registered, on the public get route, and the tests below post
// to the create route. RegisterPostRoutes panics on a nil one, which is the
// wiring bug it is there to catch — so it gets a real value.
func postRouter(service posts.Service, auth middleware.AuthMiddleware) http.Handler {
	router := chi.NewRouter()
	RegisterPostRoutes(router, service, nil, nil, auth, middleware.NewOAuthAuthMiddleware(nil, nil))
	return router
}

// createPost posts a minimal well-formed create request and returns the
// recorder.
func createPost(t *testing.T, router http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPostCreate_AggregatorPrincipalReachesTheService(t *testing.T) {
	service := &stubPostService{response: &posts.CreatePostResponse{
		URI: "at://did:plc:community/social.coves.community.post/abc",
		CID: "bafyaggregatorpost",
	}}
	router := postRouter(service, serviceJWTPrincipal{did: aggregatorPrincipalDID})

	rec := createPost(t, router, map[string]any{
		"community": "did:plc:community",
		"title":     "from the feed",
		"content":   "an aggregated item",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var resp posts.CreatePostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if resp.URI != service.response.URI || resp.CID != service.response.CID {
		t.Errorf("response = %+v, want the URI and CID the service returned (%+v)", resp, service.response)
	}

	// Reaching 200 at all is itself the positive half of the wiring proof: the
	// principal is injected BY the middleware, so an unwrapped route would
	// arrive at the handler with no DID and answer 401.
	//
	// The author is taken from the authenticated principal, never from the
	// body. For a human this stops impersonation; for an aggregator it is also
	// what makes the authorization check meaningful, since the community
	// authorized a DID and not a display name.
	if service.received.AuthorDID != aggregatorPrincipalDID {
		t.Errorf("service received authorDid %q, want the authenticated aggregator %q",
			service.received.AuthorDID, aggregatorPrincipalDID)
	}
}

func TestPostCreate_RouteIsBehindItsAuthMiddleware(t *testing.T) {
	// The check no handler test can make: RegisterPostRoutes wrapping the
	// create route in RequireAuth.
	//
	// Dropping the wrapping does NOT change the status code — CreateHandler's
	// own guard answers 401 when no DID reached the context — so this asserts
	// on the marker the middleware writes and the handler cannot. Mutation-
	// checked by registering the route without RequireAuth: the status stays
	// 401 and this test goes red on the marker, which is the point of it.
	service := &stubPostService{response: &posts.CreatePostResponse{URI: "at://x", CID: "bafy"}}
	router := postRouter(service, refusingAuth{})

	rec := createPost(t, router, map[string]any{"community": "did:plc:community", "title": "t", "content": "c"})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get(refusedByMiddleware) == "" {
		t.Errorf("the 401 came from the handler, not from RequireAuth: the create route is " +
			"registered without its auth middleware, so every request reaches CreateHandler and " +
			"only the ones that happen to carry no DID are refused")
	}
	if service.calls != 0 {
		t.Errorf("the service was called %d time(s) by a request the middleware refused: the create "+
			"route is reachable without passing through RequireAuth", service.calls)
	}
}

func TestPostCreate_AggregatorRefusalsReachTheClientAsTheirOwnStatus(t *testing.T) {
	// The two outcomes that belong to aggregators alone, checked through the
	// router so that the error class, the mapper and the route agree.
	//
	// The wrapping matters: posts.CreatePost returns these as
	// fmt.Errorf("aggregator not authorized: %w", err) (service.go:174), so a
	// mapper matching on the bare sentinel would pass its own unit test and
	// answer 500 here.
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a community that never authorized this aggregator",
			err:        fmt.Errorf("aggregator not authorized: %w", aggregators.ErrNotAuthorized),
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
		},
		{
			name:       "an aggregator over its hourly quota",
			err:        fmt.Errorf("aggregator rate limited: %w", aggregators.ErrRateLimitExceeded),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "RateLimitExceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := postRouter(&stubPostService{err: tc.err}, serviceJWTPrincipal{did: aggregatorPrincipalDID})

			rec := createPost(t, router, map[string]any{
				"community": "did:plc:community",
				"title":     "from the feed",
				"content":   "an aggregated item",
			})

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d. Body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if body.Error != tc.wantCode {
				t.Errorf("error code = %q, want %q — an aggregator retries on %q and gives up on the "+
					"others, so the code is the part of this response that is load-bearing",
					body.Error, tc.wantCode, "RateLimitExceeded")
			}
		})
	}
}
