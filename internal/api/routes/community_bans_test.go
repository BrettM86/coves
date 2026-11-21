package routes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityBanModeratorDID = "did:plc:banmoderator1"
	communityBanCommunityDID = "did:plc:bancommunity1"
	communityBanSubjectDID   = "did:plc:bansubject1"
	communityBanRecordURI    = "at://did:plc:bancommunity1/social.coves.moderation.ban/4wp3kb2ytwnxjfr4bwgk7xcmvl2ysx2a4ta4ofeu46axolswyxra"
)

// communityBanService answers the three ban endpoints with one configured
// result or error and records the requests it received.
type communityBanService struct {
	mu            sync.Mutex
	result        *moderation.CommunityActionResult
	page          *moderation.CommunityBanPage
	err           error
	banRequests   []moderation.BanCommunityAccountRequest
	unbanRequests []moderation.UnbanCommunityAccountRequest
	listRequests  []moderation.ListCommunityBansRequest
}

func (s *communityBanService) BanCommunityAccount(_ context.Context, req moderation.BanCommunityAccountRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.banRequests = append(s.banRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityBanService) UnbanCommunityAccount(_ context.Context, req moderation.UnbanCommunityAccountRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unbanRequests = append(s.unbanRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityBanService) ListCommunityBans(_ context.Context, req moderation.ListCommunityBansRequest) (*moderation.CommunityBanPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listRequests = append(s.listRequests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.page, nil
}

func (s *communityBanService) set(result *moderation.CommunityActionResult, page *moderation.CommunityBanPage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result, s.page, s.err = result, page, err
	s.banRequests, s.unbanRequests, s.listRequests = nil, nil, nil
}

func (s *communityBanService) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.banRequests) + len(s.unbanRequests) + len(s.listRequests)
}

// newCommunityBanRouteClient serves the three ban endpoints through their
// route registration.
func newCommunityBanRouteClient(t *testing.T, service *communityBanService) *invitationRouteClient {
	t.Helper()
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityBanRoutes(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &invitationRouteClient{server: server, sessions: sessions}
}

// TestCommunityBanRoutes drives social.coves.community.banAccount and
// unbanAccount through their route registration: the request the service
// receives (the actor is the authenticated caller, never the body's), the
// success body with alreadyApplied, and authentication.
func TestCommunityBanRoutes(t *testing.T) {
	service := &communityBanService{}
	client := newCommunityBanRouteClient(t, service)

	procedures := []struct {
		name     string
		nsid     string
		body     string
		result   *moderation.CommunityActionResult
		wantBody string
		received func(t *testing.T)
	}{
		{
			name: "banAccount",
			nsid: "social.coves.community.banAccount",
			body: `{"actor":"did:plc:banimposter1","community":"did:plc:bancommunity1","subject":"did:plc:bansubject1",` +
				`"reason":"Spamming invite links","expiresAt":"2026-11-20T12:00:00Z","expectedVersion":4,"idempotencyKey":"ban-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
				RecordURI: communityBanRecordURI, ActionID: "community-action-3", Version: 5,
			},
			wantBody: `{"uri":"` + communityBanRecordURI + `","version":5,"actionId":"community-action-3","alreadyApplied":false}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				require.Len(t, service.banRequests, 1)
				got := service.banRequests[0]
				require.NotNil(t, got.ExpiresAt, "expiresAt reaches the service")
				assert.True(t, got.ExpiresAt.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)), "expiresAt is %s", got.ExpiresAt)
				got.ExpiresAt = nil
				assert.Equal(t, moderation.BanCommunityAccountRequest{
					ActorDID: communityBanModeratorDID, CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
					Reason: "Spamming invite links", ExpectedVersion: 4, IdempotencyKey: "ban-1",
				}, got, "the authenticated caller is the actor; the body's actor is ignored")
			},
		},
		{
			name: "banAccount already applied",
			nsid: "social.coves.community.banAccount",
			body: `{"community":"did:plc:bancommunity1","subject":"did:plc:bansubject1","expectedVersion":4,"idempotencyKey":"ban-2"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
				RecordURI: communityBanRecordURI, Version: 4, AlreadyApplied: true,
			},
			// No action was written, so actionId is absent.
			wantBody: `{"uri":"` + communityBanRecordURI + `","version":4,"alreadyApplied":true}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.BanCommunityAccountRequest{{
					ActorDID: communityBanModeratorDID, CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
					ExpectedVersion: 4, IdempotencyKey: "ban-2",
				}}, service.banRequests, "no reason and no expiry reach the service as empty")
			},
		},
		{
			name: "unbanAccount",
			nsid: "social.coves.community.unbanAccount",
			body: `{"actor":"did:plc:banimposter1","community":"did:plc:bancommunity1","subject":"did:plc:bansubject1",` +
				`"expectedVersion":5,"idempotencyKey":"unban-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
				RecordURI: communityBanRecordURI, ActionID: "community-action-4", Version: 6,
			},
			wantBody: `{"uri":"` + communityBanRecordURI + `","version":6,"actionId":"community-action-4","alreadyApplied":false}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.UnbanCommunityAccountRequest{{
					ActorDID: communityBanModeratorDID, CommunityDID: communityBanCommunityDID, SubjectDID: communityBanSubjectDID,
					ExpectedVersion: 5, IdempotencyKey: "unban-1",
				}}, service.unbanRequests, "the authenticated caller is the actor; the body's actor is ignored")
			},
		},
	}

	for _, procedure := range procedures {
		t.Run(procedure.name, func(t *testing.T) {
			t.Run("success", func(t *testing.T) {
				service.set(procedure.result, nil, nil)

				status, body := client.send(t, http.MethodPost, procedure.nsid, procedure.body, communityBanModeratorDID)

				assert.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.JSONEq(t, procedure.wantBody, body)
				procedure.received(t)
			})

			t.Run("unauthenticated", func(t *testing.T) {
				service.set(procedure.result, nil, nil)

				status, body := client.send(t, http.MethodPost, procedure.nsid, procedure.body, "")

				assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
				assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
				assert.Zero(t, service.calls(), "the service must not run without authentication")
			})
		})
	}
}

// TestCommunityBanListRoute drives social.coves.community.listBans: the
// caller is the actor, and each ban is served with its reason, expiry and
// whether it is in force, plus the next cursor.
func TestCommunityBanListRoute(t *testing.T) {
	service := &communityBanService{}
	client := newCommunityBanRouteClient(t, service)
	expires := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	lapsed := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	service.set(nil, &moderation.CommunityBanPage{
		Bans: []moderation.CommunityBanEntry{
			{
				SubjectDID: communityBanSubjectDID, URI: communityBanRecordURI,
				CID: "bafyreiabaninforceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reason: "Spamming invite links",
				ExpiresAt: &expires, CreatedAt: &created, IndexedAt: time.Date(2026, 10, 25, 8, 0, 5, 0, time.UTC), Active: true,
			},
			{
				SubjectDID: "did:plc:bansubject2",
				URI:        "at://did:plc:bancommunity1/social.coves.moderation.ban/lapsedbankey",
				CID:        "bafyreiabanlapsedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				ExpiresAt:  &lapsed, IndexedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), Active: false,
			},
		},
		Cursor: "next-page-1",
	}, nil)

	status, body := client.send(t, http.MethodGet, "social.coves.community.listBans",
		"community=did:plc:bancommunity1&limit=10&cursor=page-1", communityBanModeratorDID)

	require.Equal(t, http.StatusOK, status, "body: %s", body)
	var output struct {
		Bans []struct {
			Subject   string     `json:"subject"`
			URI       string     `json:"uri"`
			CID       string     `json:"cid"`
			Reason    *string    `json:"reason"`
			ExpiresAt *time.Time `json:"expiresAt"`
			CreatedAt *time.Time `json:"createdAt"`
			IndexedAt time.Time  `json:"indexedAt"`
			Active    *bool      `json:"active"`
		} `json:"bans"`
		Cursor string `json:"cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &output), "body: %s", body)
	assert.Equal(t, "next-page-1", output.Cursor)
	require.Len(t, output.Bans, 2)

	inForce := output.Bans[0]
	assert.Equal(t, communityBanSubjectDID, inForce.Subject)
	assert.Equal(t, communityBanRecordURI, inForce.URI)
	assert.Equal(t, "bafyreiabaninforceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", inForce.CID)
	require.NotNil(t, inForce.Reason)
	assert.Equal(t, "Spamming invite links", *inForce.Reason)
	require.NotNil(t, inForce.ExpiresAt)
	assert.True(t, inForce.ExpiresAt.Equal(expires), "expiresAt is %s", inForce.ExpiresAt)
	require.NotNil(t, inForce.CreatedAt)
	assert.True(t, inForce.CreatedAt.Equal(created), "createdAt is %s", inForce.CreatedAt)
	assert.True(t, inForce.IndexedAt.Equal(time.Date(2026, 10, 25, 8, 0, 5, 0, time.UTC)), "indexedAt is %s", inForce.IndexedAt)
	require.NotNil(t, inForce.Active)
	assert.True(t, *inForce.Active)

	lapsedBan := output.Bans[1]
	assert.Equal(t, "did:plc:bansubject2", lapsedBan.Subject)
	assert.Nil(t, lapsedBan.Reason, "a ban without a reason carries no reason")
	assert.Nil(t, lapsedBan.CreatedAt, "a ban without a createdAt carries none")
	require.NotNil(t, lapsedBan.ExpiresAt)
	assert.True(t, lapsedBan.ExpiresAt.Equal(lapsed), "expiresAt is %s", lapsedBan.ExpiresAt)
	require.NotNil(t, lapsedBan.Active, "active is always present")
	assert.False(t, *lapsedBan.Active)

	service.mu.Lock()
	assert.Equal(t, []moderation.ListCommunityBansRequest{{
		ActorDID: communityBanModeratorDID, CommunityDID: communityBanCommunityDID, Limit: 10, Cursor: "page-1",
	}}, service.listRequests, "the authenticated caller is the actor the service authorizes")
	service.mu.Unlock()

	t.Run("unauthenticated", func(t *testing.T) {
		service.set(nil, &moderation.CommunityBanPage{}, nil)

		status, body := client.send(t, http.MethodGet, "social.coves.community.listBans", "community=did:plc:bancommunity1", "")

		assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
		assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
		assert.Zero(t, service.calls(), "the service must not run without authentication")
	})
}

// TestCommunityBanRoutesMapRefusals pins the XRPC error for each service
// refusal on every ban endpoint (a non-moderator's listBans is the Forbidden
// row), and that a missing expectedVersion is refused before the service
// runs.
func TestCommunityBanRoutesMapRefusals(t *testing.T) {
	service := &communityBanService{}
	client := newCommunityBanRouteClient(t, service)

	refusals := []struct {
		err        error
		wantStatus int
		wantError  string
		mutation   bool // only the procedures can answer it
	}{
		{err: moderation.ErrSubjectHoldsRole, wantStatus: http.StatusConflict, wantError: "SubjectHoldsRole", mutation: true},
		{err: moderation.ErrStateConflict, wantStatus: http.StatusConflict, wantError: "StateConflict", mutation: true},
		{err: moderation.ErrIdempotencyConflict, wantStatus: http.StatusConflict, wantError: "IdempotencyConflict", mutation: true},
		{err: moderation.ErrRateLimited, wantStatus: http.StatusTooManyRequests, wantError: "RateLimitExceeded", mutation: true},
		{err: moderation.ErrInvalidRequest, wantStatus: http.StatusBadRequest, wantError: "InvalidRequest"},
		{err: moderation.ErrForbidden, wantStatus: http.StatusForbidden, wantError: "Forbidden"},
		{err: moderation.ErrModerationUnavailable, wantStatus: http.StatusServiceUnavailable, wantError: "ModerationUnavailable"},
	}

	for _, endpoint := range []struct {
		name, method, nsid, body string
		// withoutVersion is body with expectedVersion omitted; "" for the
		// query.
		withoutVersion string
	}{
		{
			name: "banAccount", method: http.MethodPost, nsid: "social.coves.community.banAccount",
			body:           `{"community":"did:plc:bancommunity1","subject":"did:plc:bansubject1","expectedVersion":4,"idempotencyKey":"ban-3"}`,
			withoutVersion: `{"community":"did:plc:bancommunity1","subject":"did:plc:bansubject1","idempotencyKey":"ban-3"}`,
		},
		{
			name: "unbanAccount", method: http.MethodPost, nsid: "social.coves.community.unbanAccount",
			body:           `{"community":"did:plc:bancommunity1","subject":"did:plc:bansubject1","expectedVersion":5,"idempotencyKey":"unban-3"}`,
			withoutVersion: `{"community":"did:plc:bancommunity1","subject":"did:plc:bansubject1","idempotencyKey":"unban-3"}`,
		},
		{
			name: "listBans", method: http.MethodGet, nsid: "social.coves.community.listBans",
			body: "community=did:plc:bancommunity1",
		},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, refusal := range refusals {
				if refusal.mutation && endpoint.method == http.MethodGet {
					continue
				}
				t.Run(refusal.wantError, func(t *testing.T) {
					service.set(nil, nil, fmt.Errorf("%w: refused by the service", refusal.err))

					status, body := client.send(t, endpoint.method, endpoint.nsid, endpoint.body, communityBanModeratorDID)

					assert.Equal(t, refusal.wantStatus, status, "body: %s", body)
					assert.Equal(t, refusal.wantError, xrpcErrorName(t, body))
				})
			}

			if endpoint.withoutVersion == "" {
				return
			}
			t.Run("missing expectedVersion", func(t *testing.T) {
				service.set(&moderation.CommunityActionResult{Version: 1}, nil, nil)

				status, body := client.send(t, endpoint.method, endpoint.nsid, endpoint.withoutVersion, communityBanModeratorDID)

				assert.Equal(t, http.StatusBadRequest, status, "body: %s", body)
				assert.Equal(t, "InvalidRequest", xrpcErrorName(t, body))
				assert.Zero(t, service.calls(), "the service must not run without expectedVersion")
			})
		})
	}
}
