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
	communityCommentModeratorDID = "did:plc:commentmoderator1"
	communityCommentURI          = "at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v"
)

// communityCommentService answers both comment procedures with one configured
// result or error and records the requests it received.
type communityCommentService struct {
	mu              sync.Mutex
	result          *moderation.CommunityActionResult
	err             error
	removeRequests  []moderation.RemoveCommentRequest
	restoreRequests []moderation.RestoreCommentRequest
}

func (s *communityCommentService) RemoveComment(_ context.Context, req moderation.RemoveCommentRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRequests = append(s.removeRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityCommentService) RestoreComment(_ context.Context, req moderation.RestoreCommentRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreRequests = append(s.restoreRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityCommentService) set(result *moderation.CommunityActionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result, s.err = result, err
	s.removeRequests, s.restoreRequests = nil, nil
}

func (s *communityCommentService) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.removeRequests) + len(s.restoreRequests)
}

// communityCommentServer serves the two comment procedures through their
// route registration and returns a POST helper; caller "" sends no
// credentials.
func communityCommentServer(t *testing.T, service *communityCommentService) func(t *testing.T, procedure, body, caller string) (int, string) {
	t.Helper()
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityCommentRoutes(mux,
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

// TestCommunityCommentRoutes drives social.coves.community.removeComment and
// social.coves.community.restoreComment through their route registration: the
// request the service receives (the actor is the authenticated caller, never
// the body's), the success body with alreadyApplied, and authentication.
func TestCommunityCommentRoutes(t *testing.T) {
	service := &communityCommentService{}
	post := communityCommentServer(t, service)

	procedures := []struct {
		name     string
		nsid     string
		body     string
		result   *moderation.CommunityActionResult
		wantBody string
		received func(t *testing.T)
	}{
		{
			name: "removeComment",
			nsid: "social.coves.community.removeComment",
			body: `{"actor":"did:plc:commentimposter1","subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v",` +
				`"code":"spam","reason":"phishing links","expectedVersion":4,"idempotencyKey":"remove-comment-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: "did:plc:commentcommunity1",
				RecordURI:    "at://did:plc:commentcommunity1/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya",
				ActionID:     "community-action-12", Version: 5,
			},
			wantBody: `{"uri":"at://did:plc:commentcommunity1/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya",` +
				`"version":5,"actionId":"community-action-12","alreadyApplied":false}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.RemoveCommentRequest{{
					ActorDID: communityCommentModeratorDID, CommentURI: communityCommentURI,
					Code: "spam", Reason: "phishing links",
					ExpectedVersion: 4, IdempotencyKey: "remove-comment-1",
				}}, service.removeRequests, "the authenticated caller is the actor; the body's actor is ignored")
				assert.Empty(t, service.restoreRequests)
			},
		},
		{
			name: "restoreComment",
			nsid: "social.coves.community.restoreComment",
			body: `{"actor":"did:plc:commentimposter1","subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v",` +
				`"expectedVersion":5,"idempotencyKey":"restore-comment-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: "did:plc:commentcommunity1",
				RecordURI:    "at://did:plc:commentcommunity1/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya",
				Version:      5, AlreadyApplied: true,
			},
			// Already restored: no action was written, so actionId is absent.
			wantBody: `{"uri":"at://did:plc:commentcommunity1/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya",` +
				`"version":5,"alreadyApplied":true}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.RestoreCommentRequest{{
					ActorDID: communityCommentModeratorDID, CommentURI: communityCommentURI,
					ExpectedVersion: 5, IdempotencyKey: "restore-comment-1",
				}}, service.restoreRequests, "the authenticated caller is the actor; the body's actor is ignored")
				assert.Empty(t, service.removeRequests)
			},
		},
	}

	for _, procedure := range procedures {
		t.Run(procedure.name, func(t *testing.T) {
			t.Run("success", func(t *testing.T) {
				service.set(procedure.result, nil)

				status, body := post(t, procedure.nsid, procedure.body, communityCommentModeratorDID)

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

// TestCommunityCommentRoutesMapRefusals pins the XRPC error for each service
// refusal, and that a missing expectedVersion is refused before the service
// runs.
func TestCommunityCommentRoutesMapRefusals(t *testing.T) {
	service := &communityCommentService{}
	post := communityCommentServer(t, service)

	refusals := []struct {
		err        error
		wantStatus int
		wantError  string
	}{
		{err: moderation.ErrCommentNotFound, wantStatus: http.StatusNotFound, wantError: "CommentNotFound"},
		{err: moderation.ErrRateLimited, wantStatus: http.StatusTooManyRequests, wantError: "RateLimitExceeded"},
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
			name:           "removeComment",
			nsid:           "social.coves.community.removeComment",
			body:           `{"subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v","code":"spam","expectedVersion":4,"idempotencyKey":"remove-comment-2"}`,
			withoutVersion: `{"subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v","code":"spam","idempotencyKey":"remove-comment-2"}`,
		},
		{
			name:           "restoreComment",
			nsid:           "social.coves.community.restoreComment",
			body:           `{"subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v","expectedVersion":5,"idempotencyKey":"restore-comment-2"}`,
			withoutVersion: `{"subject":"at://did:plc:commentauthor1/social.coves.community.comment/3kjzl5kcb2c2v","idempotencyKey":"restore-comment-2"}`,
		},
	} {
		t.Run(procedure.name, func(t *testing.T) {
			for _, refusal := range refusals {
				t.Run(refusal.wantError, func(t *testing.T) {
					service.set(nil, fmt.Errorf("%w: refused by the service", refusal.err))

					status, body := post(t, procedure.nsid, procedure.body, communityCommentModeratorDID)

					assert.Equal(t, refusal.wantStatus, status, "body: %s", body)
					assert.Equal(t, refusal.wantError, xrpcErrorName(t, body))
				})
			}

			t.Run("missing expectedVersion", func(t *testing.T) {
				service.set(&moderation.CommunityActionResult{Version: 1}, nil)

				status, body := post(t, procedure.nsid, procedure.withoutVersion, communityCommentModeratorDID)

				assert.Equal(t, http.StatusBadRequest, status, "body: %s", body)
				assert.Equal(t, "InvalidRequest", xrpcErrorName(t, body))
				assert.Zero(t, service.calls(), "the service must not run without expectedVersion")
			})
		})
	}
}
