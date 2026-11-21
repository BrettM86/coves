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

// communityRosterService answers both roster procedures with one configured
// result or error and records the requests it received.
type communityRosterService struct {
	mu             sync.Mutex
	result         *moderation.CommunityActionResult
	err            error
	resignRequests []moderation.ResignRoleRequest
	removeRequests []moderation.RemoveModeratorRequest
}

func (s *communityRosterService) ResignRole(_ context.Context, req moderation.ResignRoleRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resignRequests = append(s.resignRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityRosterService) RemoveModerator(_ context.Context, req moderation.RemoveModeratorRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRequests = append(s.removeRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityRosterService) set(result *moderation.CommunityActionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result, s.err = result, err
	s.resignRequests, s.removeRequests = nil, nil
}

// TestCommunityRosterRoutes drives social.coves.community.resignModerator and
// social.coves.community.removeModerator through their route registration:
// authentication, the request the service receives, the success body and the
// mapping of service refusals.
func TestCommunityRosterRoutes(t *testing.T) {
	const (
		communityDID = "did:plc:rosteralpha"
		ownerDID     = "did:plc:rosterowner"
		moderatorDID = "did:plc:troublemaker456"
	)
	service := &communityRosterService{}
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityRosterRoutes(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	post := func(t *testing.T, procedure, body, caller string) (int, string) {
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

	procedures := []struct {
		name   string
		nsid   string
		caller string
		body   string
		// result is what the service returns; wantBody is the response to it.
		result   *moderation.CommunityActionResult
		wantBody string
		// received checks the one request the service received.
		received func(t *testing.T)
	}{
		{
			name:   "removeModerator",
			nsid:   "social.coves.community.removeModerator",
			caller: ownerDID,
			body:   `{"community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456","expectedVersion":7,"idempotencyKey":"remove-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: communityDID, SubjectDID: moderatorDID,
				RecordURI: "at://did:plc:rosteralpha/social.coves.community.moderator/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
				ActionID:  "community-action-1", Version: 8,
			},
			wantBody: `{"community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456","version":8}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.RemoveModeratorRequest{{
					ActorDID: ownerDID, CommunityDID: communityDID, SubjectDID: moderatorDID,
					ExpectedVersion: 7, IdempotencyKey: "remove-1",
				}}, service.removeRequests, "the authenticated caller is the actor")
				assert.Empty(t, service.resignRequests)
			},
		},
		{
			name:   "resignModerator",
			nsid:   "social.coves.community.resignModerator",
			caller: moderatorDID,
			body:   `{"community":"did:plc:rosteralpha","expectedVersion":7,"idempotencyKey":"resign-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: communityDID, SubjectDID: moderatorDID,
				RecordURI: "at://did:plc:rosteralpha/social.coves.community.moderator/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
				ActionID:  "community-action-2", Version: 9,
			},
			wantBody: `{"community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456","version":9}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.ResignRoleRequest{{
					ActorDID: moderatorDID, CommunityDID: communityDID,
					ExpectedVersion: 7, IdempotencyKey: "resign-1",
				}}, service.resignRequests, "the authenticated caller is the actor")
				assert.Empty(t, service.removeRequests)
			},
		},
	}

	refusals := []struct {
		err        error
		wantStatus int
		wantError  string
	}{
		{err: moderation.ErrAuthRequired, wantStatus: http.StatusUnauthorized, wantError: "AuthRequired"},
		{err: moderation.ErrForbidden, wantStatus: http.StatusForbidden, wantError: "Forbidden"},
		{err: moderation.ErrInvalidRequest, wantStatus: http.StatusBadRequest, wantError: "InvalidRequest"},
		{err: moderation.ErrStateConflict, wantStatus: http.StatusConflict, wantError: "StateConflict"},
		{err: moderation.ErrIdempotencyConflict, wantStatus: http.StatusConflict, wantError: "IdempotencyConflict"},
		{err: moderation.ErrOwnerMustTransfer, wantStatus: http.StatusBadRequest, wantError: "OwnerMustTransfer"},
		{err: moderation.ErrCannotRemoveOwner, wantStatus: http.StatusBadRequest, wantError: "CannotRemoveOwner"},
		{err: moderation.ErrRoleNotFound, wantStatus: http.StatusNotFound, wantError: "RoleNotFound"},
		{err: moderation.ErrModerationUnavailable, wantStatus: http.StatusServiceUnavailable, wantError: "ModerationUnavailable"},
	}

	for _, procedure := range procedures {
		t.Run(procedure.name, func(t *testing.T) {
			t.Run("unauthenticated", func(t *testing.T) {
				service.set(procedure.result, nil)

				status, body := post(t, procedure.nsid, procedure.body, "")

				assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
				assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
			})

			t.Run("success", func(t *testing.T) {
				service.set(procedure.result, nil)

				status, body := post(t, procedure.nsid, procedure.body, procedure.caller)

				assert.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.JSONEq(t, procedure.wantBody, body, "exactly community, subject and version")
				procedure.received(t)
			})

			for _, refusal := range refusals {
				t.Run(refusal.wantError, func(t *testing.T) {
					service.set(nil, fmt.Errorf("%w: refused by the service", refusal.err))

					status, body := post(t, procedure.nsid, procedure.body, procedure.caller)

					assert.Equal(t, refusal.wantStatus, status, "body: %s", body)
					assert.Equal(t, refusal.wantError, xrpcErrorName(t, body))
				})
			}
		})
	}
}

// TestCommunityRosterRoutesExpectedVersion pins that both roster procedures
// require expectedVersion: an omitted or null value is refused with
// InvalidRequest before the service runs, while an explicit 0 is a valid
// version passed through unchanged.
func TestCommunityRosterRoutesExpectedVersion(t *testing.T) {
	const (
		communityDID = "did:plc:rosteralpha"
		ownerDID     = "did:plc:rosterowner"
		moderatorDID = "did:plc:troublemaker456"
	)
	service := &communityRosterService{}
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityRosterRoutes(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	post := func(t *testing.T, procedure, body, caller string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			server.URL+"/xrpc/"+procedure, strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+sessions.add(caller))
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		responseBody, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, string(responseBody)
	}
	result := &moderation.CommunityActionResult{
		CommunityDID: communityDID, SubjectDID: moderatorDID, ActionID: "community-action-3", Version: 1,
	}

	procedures := []struct {
		name   string
		nsid   string
		caller string
		// fields is the request body without expectedVersion.
		fields string
		// receivedVersions returns the expectedVersion of each request the
		// service received.
		receivedVersions func() []int64
	}{
		{
			name:   "removeModerator",
			nsid:   "social.coves.community.removeModerator",
			caller: ownerDID,
			fields: `"community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456","idempotencyKey":"remove-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				versions := []int64{}
				for _, request := range service.removeRequests {
					versions = append(versions, request.ExpectedVersion)
				}
				return versions
			},
		},
		{
			name:   "resignModerator",
			nsid:   "social.coves.community.resignModerator",
			caller: moderatorDID,
			fields: `"community":"did:plc:rosteralpha","idempotencyKey":"resign-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				versions := []int64{}
				for _, request := range service.resignRequests {
					versions = append(versions, request.ExpectedVersion)
				}
				return versions
			},
		},
	}

	for _, procedure := range procedures {
		t.Run(procedure.name, func(t *testing.T) {
			for _, missing := range []struct{ name, body string }{
				{name: "omitted", body: "{" + procedure.fields + "}"},
				{name: "null", body: "{" + procedure.fields + `,"expectedVersion":null}`},
			} {
				t.Run(missing.name, func(t *testing.T) {
					service.set(result, nil)

					status, body := post(t, procedure.nsid, missing.body, procedure.caller)

					assert.Equal(t, http.StatusBadRequest, status, "body: %s", body)
					assert.Equal(t, "InvalidRequest", xrpcErrorName(t, body))
					assert.Empty(t, procedure.receivedVersions(), "the service must not run without expectedVersion")
				})
			}

			t.Run("explicit zero", func(t *testing.T) {
				service.set(result, nil)

				status, body := post(t, procedure.nsid, "{"+procedure.fields+`,"expectedVersion":0}`, procedure.caller)

				assert.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.Equal(t, []int64{0}, procedure.receivedVersions())
			})
		})
	}
}
