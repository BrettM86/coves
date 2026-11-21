package routes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// communityStateService answers getCommunityState from fixed results keyed by
// subject and records the requests it received.
type communityStateService struct {
	mu       sync.Mutex
	states   map[string]*moderation.CommunityState
	errs     map[string]error
	requests []moderation.CommunityStateRequest
}

func (s *communityStateService) GetCommunityState(_ context.Context, req moderation.CommunityStateRequest) (*moderation.CommunityState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if err, ok := s.errs[req.Subject]; ok {
		return nil, err
	}
	if state, ok := s.states[req.Subject]; ok {
		return state, nil
	}
	return nil, fmt.Errorf("communityStateService: no state seeded for %s", req.Subject)
}

// TestGetCommunityStateRoute drives social.coves.moderation.getCommunityState
// through its route registration: authentication, the response for each
// subject kind and the mapping of service refusals.
func TestGetCommunityStateRoute(t *testing.T) {
	const (
		viewerDID    = "did:plc:alphamoderator"
		communityDID = "did:plc:communityalpha"
		postURI      = "at://did:plc:authoralice/social.coves.community.postv2/3kabcalphapost"
		commentURI   = "at://did:plc:authorbob/social.coves.community.comment/3kabcalphacomment"
		bannedDID    = "did:plc:troublemaker456"
		unbannedDID  = "did:plc:wellbehaved789"
		forbiddenURI = "at://did:plc:authoralice/social.coves.community.postv2/3kabcforbidden"
		invalidDID   = "did:plc:invalidrequest"
		invalidURI   = "at://did:plc:authoralice/app.bsky.feed.post/3kabcforeign"
		failingURI   = "at://did:plc:authoralice/social.coves.community.postv2/3kabcunavailable"
	)
	banExpiry := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	service := &communityStateService{
		states: map[string]*moderation.CommunityState{
			postURI: {
				CommunityDID: communityDID, Subject: postURI, Kind: "post", Version: 3,
				AdmissionStatus: "accepted",
			},
			commentURI: {
				CommunityDID: communityDID, Subject: commentURI, Kind: "comment", Version: 2,
				CommentRemoved: true,
			},
			bannedDID: {
				CommunityDID: communityDID, Subject: bannedDID, Kind: "account", Version: 4,
				Ban: &communities.CommunityBan{
					CommunityDID: communityDID,
					SubjectDID:   bannedDID,
					RecordKey:    "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
					RecordCID:    "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy",
					Reason:       "spam",
					ExpiresAt:    &banExpiry,
				},
			},
			unbannedDID: {
				CommunityDID: communityDID, Subject: unbannedDID, Kind: "account", Version: 0,
			},
			communityDID: {
				CommunityDID: communityDID, Subject: communityDID, Kind: "roster", Version: 5,
				Roster: &communities.Roster{
					Owner: &communities.CommunityRole{
						CommunityDID: communityDID, SubjectDID: "did:plc:alphaowner", Role: communities.RoleOwner,
					},
					Moderators: []communities.CommunityRole{{
						CommunityDID: communityDID, SubjectDID: viewerDID, Role: communities.RoleModerator,
					}},
				},
			},
		},
		errs: map[string]error{
			forbiddenURI: fmt.Errorf("%w: not a moderator", moderation.ErrForbidden),
			invalidDID:   fmt.Errorf("%w: community is required", moderation.ErrInvalidRequest),
			invalidURI:   fmt.Errorf("%w: unknown collection", moderation.ErrInvalidSubject),
			failingURI:   fmt.Errorf("%w: reading the community state: connection reset", moderation.ErrModerationUnavailable),
		},
	}
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityStateRoute(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	get := func(t *testing.T, subject, community string, authenticated bool) (int, string) {
		t.Helper()
		query := url.Values{"subject": {subject}}
		if community != "" {
			query.Set("community", community)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			server.URL+"/xrpc/social.coves.moderation.getCommunityState?"+query.Encode(), nil)
		require.NoError(t, err)
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+sessions.add(viewerDID))
		}
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, string(body)
	}

	t.Run("unauthenticated", func(t *testing.T) {
		status, body := get(t, postURI, "", false)

		assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
		assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
	})

	for _, test := range []struct {
		name      string
		subject   string
		community string
		wantBody  string
	}{
		{
			name:     "post",
			subject:  postURI,
			wantBody: `{"community":"did:plc:communityalpha","subject":"at://did:plc:authoralice/social.coves.community.postv2/3kabcalphapost","kind":"post","version":3,"post":{"status":"accepted"}}`,
		},
		{
			name:     "comment",
			subject:  commentURI,
			wantBody: `{"community":"did:plc:communityalpha","subject":"at://did:plc:authorbob/social.coves.community.comment/3kabcalphacomment","kind":"comment","version":2,"comment":{"removed":true}}`,
		},
		{
			name:      "banned account",
			subject:   bannedDID,
			community: communityDID,
			wantBody:  `{"community":"did:plc:communityalpha","subject":"did:plc:troublemaker456","kind":"account","version":4,"ban":{"reason":"spam","expiresAt":"2099-01-01T00:00:00Z","recordUri":"at://did:plc:communityalpha/social.coves.moderation.ban/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"}}`,
		},
		{
			name:      "account without a ban",
			subject:   unbannedDID,
			community: communityDID,
			wantBody:  `{"community":"did:plc:communityalpha","subject":"did:plc:wellbehaved789","kind":"account","version":0}`,
		},
		{
			name:      "roster",
			subject:   communityDID,
			community: communityDID,
			wantBody:  `{"community":"did:plc:communityalpha","subject":"did:plc:communityalpha","kind":"roster","version":5,"roster":{"owner":{"did":"did:plc:alphaowner"},"moderators":[{"did":"did:plc:alphamoderator"}]}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := get(t, test.subject, test.community, true)

			assert.Equal(t, http.StatusOK, status, "body: %s", body)
			assert.JSONEq(t, test.wantBody, body)
			service.mu.Lock()
			defer service.mu.Unlock()
			require.NotEmpty(t, service.requests)
			assert.Equal(t, moderation.CommunityStateRequest{
				ViewerDID: viewerDID, Subject: test.subject, CommunityDID: test.community,
			}, service.requests[len(service.requests)-1])
		})
	}

	for _, test := range []struct {
		name       string
		subject    string
		community  string
		wantStatus int
		wantError  string
	}{
		{name: "forbidden", subject: forbiddenURI, wantStatus: http.StatusForbidden, wantError: "Forbidden"},
		{name: "invalid request", subject: invalidDID, wantStatus: http.StatusBadRequest, wantError: "InvalidRequest"},
		{name: "invalid subject", subject: invalidURI, wantStatus: http.StatusBadRequest, wantError: "InvalidSubject"},
		{name: "moderation unavailable", subject: failingURI, wantStatus: http.StatusServiceUnavailable, wantError: "ModerationUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := get(t, test.subject, test.community, true)

			assert.Equal(t, test.wantStatus, status, "body: %s", body)
			assert.Equal(t, test.wantError, xrpcErrorName(t, body))
		})
	}
}

// xrpcErrorName is the error name of an XRPC error body.
func xrpcErrorName(t *testing.T, body string) string {
	t.Helper()
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return "<not an XRPC error body: " + body + ">"
	}
	return decoded.Error
}
