package routes_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityPostModeratorDID = "did:plc:postmoderator1"
	communityPostURI          = "at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v"
)

// communityPostService answers both post procedures with one configured
// result or error and records the requests it received.
type communityPostService struct {
	mu              sync.Mutex
	result          *moderation.CommunityActionResult
	err             error
	removeRequests  []moderation.RemovePostRequest
	restoreRequests []moderation.RestorePostRequest
}

func (s *communityPostService) RemovePost(_ context.Context, req moderation.RemovePostRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRequests = append(s.removeRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityPostService) RestorePost(_ context.Context, req moderation.RestorePostRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreRequests = append(s.restoreRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityPostService) set(result *moderation.CommunityActionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result, s.err = result, err
	s.removeRequests, s.restoreRequests = nil, nil
}

func (s *communityPostService) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.removeRequests) + len(s.restoreRequests)
}

// communityPostServer serves the two post procedures through their route
// registration and returns a POST helper; caller "" sends no credentials.
func communityPostServer(t *testing.T, service *communityPostService) func(t *testing.T, procedure, body, caller string) (int, string) {
	t.Helper()
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityPostRoutes(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return func(t *testing.T, procedure, body, caller string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			server.URL+"/xrpc/"+procedure, strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if caller != "" {
			request.Header.Set("Authorization", "Bearer "+sessions.add(caller))
		}
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		responseBody, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, string(responseBody)
	}
}

// communityPostProcedures is one row per procedure: a request whose body names
// an imposter actor, the service result and the response it must produce.
func communityPostProcedures(service *communityPostService) []struct {
	name     string
	nsid     string
	body     string
	result   *moderation.CommunityActionResult
	wantBody string
	received func(t *testing.T)
} {
	return []struct {
		name     string
		nsid     string
		body     string
		result   *moderation.CommunityActionResult
		wantBody string
		received func(t *testing.T)
	}{
		{
			name: "removePost",
			nsid: "social.coves.community.removePost",
			body: `{"actor":"did:plc:postimposter1","subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v",` +
				`"code":"spam","reason":"selling followers","expectedVersion":4,"idempotencyKey":"remove-post-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: "did:plc:postcommunity1",
				RecordURI:    "at://did:plc:postcommunity1/social.coves.community.removal/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
				ActionID:     "community-action-11", Version: 5,
			},
			wantBody: `{"uri":"at://did:plc:postcommunity1/social.coves.community.removal/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",` +
				`"version":5,"actionId":"community-action-11","alreadyApplied":false}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.RemovePostRequest{{
					ActorDID: communityPostModeratorDID, PostURI: communityPostURI,
					Code: "spam", Reason: "selling followers",
					ExpectedVersion: 4, IdempotencyKey: "remove-post-1",
				}}, service.removeRequests, "the authenticated caller is the actor; the body's actor is ignored")
				assert.Empty(t, service.restoreRequests)
			},
		},
		{
			name: "restorePost",
			nsid: "social.coves.community.restorePost",
			body: `{"actor":"did:plc:postimposter1","subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v",` +
				`"expectedVersion":5,"idempotencyKey":"restore-post-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: "did:plc:postcommunity1",
				RecordURI:    "at://did:plc:postcommunity1/social.coves.community.acceptance/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
				Version:      5, AlreadyApplied: true,
			},
			// Already restored: no action was written, so actionId is absent.
			wantBody: `{"uri":"at://did:plc:postcommunity1/social.coves.community.acceptance/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",` +
				`"version":5,"alreadyApplied":true}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.RestorePostRequest{{
					ActorDID: communityPostModeratorDID, PostURI: communityPostURI,
					ExpectedVersion: 5, IdempotencyKey: "restore-post-1",
				}}, service.restoreRequests, "the authenticated caller is the actor; the body's actor is ignored")
				assert.Empty(t, service.removeRequests)
			},
		},
	}
}

// TestCommunityPostRoutes drives social.coves.community.removePost and
// social.coves.community.restorePost through their route registration: the
// request the service receives, the success body and authentication.
func TestCommunityPostRoutes(t *testing.T) {
	service := &communityPostService{}
	post := communityPostServer(t, service)

	for _, procedure := range communityPostProcedures(service) {
		t.Run(procedure.name, func(t *testing.T) {
			t.Run("success", func(t *testing.T) {
				service.set(procedure.result, nil)

				status, body := post(t, procedure.nsid, procedure.body, communityPostModeratorDID)

				assert.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.JSONEq(t, procedure.wantBody, body)
				procedure.received(t)
			})

			t.Run("unauthenticated", func(t *testing.T) {
				service.set(procedure.result, nil)

				status, body := post(t, procedure.nsid, procedure.body, "")

				assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
				assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
				assert.Zero(t, service.calls(), "the service must not run without authentication")
			})
		})
	}
}

// TestCommunityPostRoutesMapRefusals pins the XRPC error for each service
// refusal, and that a missing expectedVersion is refused before the service
// runs.
func TestCommunityPostRoutesMapRefusals(t *testing.T) {
	service := &communityPostService{}
	post := communityPostServer(t, service)

	refusals := []struct {
		err        error
		wantStatus int
		wantError  string
	}{
		{err: moderation.ErrRateLimited, wantStatus: http.StatusTooManyRequests, wantError: "RateLimitExceeded"},
		{err: moderation.ErrPostNotFound, wantStatus: http.StatusNotFound, wantError: "PostNotFound"},
		{err: moderation.ErrStateConflict, wantStatus: http.StatusConflict, wantError: "StateConflict"},
		{err: moderation.ErrIdempotencyConflict, wantStatus: http.StatusConflict, wantError: "IdempotencyConflict"},
		{err: moderation.ErrForbidden, wantStatus: http.StatusForbidden, wantError: "Forbidden"},
		{err: moderation.ErrInvalidRequest, wantStatus: http.StatusBadRequest, wantError: "InvalidRequest"},
		{err: moderation.ErrModerationUnavailable, wantStatus: http.StatusServiceUnavailable, wantError: "ModerationUnavailable"},
	}

	for _, procedure := range []struct {
		name string
		nsid string
		body string
		// withoutVersion is body with expectedVersion omitted.
		withoutVersion string
	}{
		{
			name:           "removePost",
			nsid:           "social.coves.community.removePost",
			body:           `{"subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v","code":"spam","expectedVersion":4,"idempotencyKey":"remove-post-2"}`,
			withoutVersion: `{"subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v","code":"spam","idempotencyKey":"remove-post-2"}`,
		},
		{
			name:           "restorePost",
			nsid:           "social.coves.community.restorePost",
			body:           `{"subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v","expectedVersion":5,"idempotencyKey":"restore-post-2"}`,
			withoutVersion: `{"subject":"at://did:plc:postauthor1/social.coves.community.postv2/3kjzl5kcb2s2v","idempotencyKey":"restore-post-2"}`,
		},
	} {
		t.Run(procedure.name, func(t *testing.T) {
			for _, refusal := range refusals {
				t.Run(refusal.wantError, func(t *testing.T) {
					service.set(nil, fmt.Errorf("%w: refused by the service", refusal.err))

					status, body := post(t, procedure.nsid, procedure.body, communityPostModeratorDID)

					assert.Equal(t, refusal.wantStatus, status, "body: %s", body)
					assert.Equal(t, refusal.wantError, xrpcErrorName(t, body))
				})
			}

			t.Run("missing expectedVersion", func(t *testing.T) {
				service.set(&moderation.CommunityActionResult{Version: 1}, nil)

				status, body := post(t, procedure.nsid, procedure.withoutVersion, communityPostModeratorDID)

				assert.Equal(t, http.StatusBadRequest, status, "body: %s", body)
				assert.Equal(t, "InvalidRequest", xrpcErrorName(t, body))
				assert.Zero(t, service.calls(), "the service must not run without expectedVersion")
			})
		})
	}
}
