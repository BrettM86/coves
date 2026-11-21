package routes_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// creationRecordingService is a communities.Service that records whether
// CreateCommunity was reached. Every other method is the nil embedded
// interface and panics if called.
type creationRecordingService struct {
	communities.Service
	createCalls atomic.Int32
}

func (s *creationRecordingService) CreateCommunity(_ context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	s.createCalls.Add(1)
	return &communities.Community{DID: "did:plc:createdcommunity", Handle: "c-" + req.Name + ".coves.social"}, nil
}

// TestCommunityCreateRoute_InstanceAdminsPolicy drives
// social.coves.community.create through RegisterCommunityRoutes, proving the
// policy and admin checker reach the handler: under instance-admins an
// authenticated non-admin is refused before the service, and the recognized
// admin reaches it.
func TestCommunityCreateRoute_InstanceAdminsPolicy(t *testing.T) {
	const (
		adminDID  = "did:plc:creationrouteadmin"
		memberDID = "did:plc:creationroutemember"
	)

	for _, test := range []struct {
		name        string
		caller      string
		wantStatus  int
		wantError   string
		wantCreates int32
	}{
		{name: "non-admin is refused", caller: memberDID, wantStatus: http.StatusForbidden, wantError: "CommunityCreationRestricted", wantCreates: 0},
		{name: "admin reaches the service", caller: adminDID, wantStatus: http.StatusOK, wantCreates: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessions := newViewerAuthoritySessions()
			service := &creationRecordingService{}
			mux := chi.NewRouter()
			routes.RegisterCommunityRoutes(mux, service, nil,
				middleware.NewOAuthAuthMiddleware(sessions, sessions),
				communities.CreationInstanceAdmins, moderation.NewAllowlistAuthority([]string{adminDID}))
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)

			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				server.URL+"/xrpc/social.coves.community.create",
				strings.NewReader(`{"name":"gardening","visibility":"public"}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+sessions.add(test.caller))
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			assert.Equal(t, test.wantStatus, response.StatusCode, "body: %s", body)
			assert.Equal(t, test.wantCreates, service.createCalls.Load(), "CreateCommunity calls")
			if test.wantError != "" {
				var errorBody struct {
					Error string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &errorBody), "body: %s", body)
				assert.Equal(t, test.wantError, errorBody.Error)
			}
		})
	}
}
