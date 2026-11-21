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
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	invitationCommunityDID = "did:plc:rosteralpha"
	invitationOtherDID     = "did:plc:rosterbeta"
	invitationOwnerDID     = "did:plc:rosterowner"
	invitationModeratorDID = "did:plc:troublemaker456"
	invitationInviteeDID   = "did:plc:rosterinvitee"
	invitationBystanderDID = "did:plc:rosterbystander"
)

// The invitations the fake service serves, and their wire views.
var (
	moderatorInvitation = communities.Invitation{
		ID: "invitation-1", CommunityDID: invitationCommunityDID, InviteeDID: invitationInviteeDID,
		Role: communities.RoleModerator, InviterDID: invitationOwnerDID, State: communities.InvitationOpen,
		CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC),
	}
	moderatorInvitationView = `{"id":"invitation-1","community":"did:plc:rosteralpha","subject":"did:plc:rosterinvitee",` +
		`"role":"moderator","inviter":"did:plc:rosterowner","createdAt":"2026-11-14T10:00:00Z","expiresAt":"2026-11-28T10:00:00Z"}`

	ownershipOffer = communities.Invitation{
		ID: "invitation-2", CommunityDID: invitationCommunityDID, InviteeDID: invitationModeratorDID,
		Role: communities.RoleOwner, InviterDID: invitationOwnerDID, State: communities.InvitationOpen,
		CreatedAt: time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC),
	}
	ownershipOfferView = `{"id":"invitation-2","community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456",` +
		`"role":"owner","inviter":"did:plc:rosterowner","createdAt":"2026-11-13T09:30:00Z","expiresAt":"2026-11-27T09:30:00Z"}`

	otherCommunityInvitation = communities.Invitation{
		ID: "invitation-3", CommunityDID: invitationOtherDID, InviteeDID: invitationInviteeDID,
		Role: communities.RoleModerator, InviterDID: "did:plc:instanceadmin", InviterAdmin: true,
		State:     communities.InvitationOpen,
		CreatedAt: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC),
	}
	otherCommunityInvitationView = `{"id":"invitation-3","community":"did:plc:rosterbeta","subject":"did:plc:rosterinvitee",` +
		`"role":"moderator","inviter":"did:plc:instanceadmin","createdAt":"2026-11-12T08:00:00Z","expiresAt":"2026-11-26T08:00:00Z"}`

	bystanderInvitation = communities.Invitation{
		ID: "invitation-4", CommunityDID: invitationOtherDID, InviteeDID: invitationBystanderDID,
		Role: communities.RoleModerator, InviterDID: invitationOwnerDID, State: communities.InvitationOpen,
		CreatedAt: time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC),
	}
	bystanderInvitationView = `{"id":"invitation-4","community":"did:plc:rosterbeta","subject":"did:plc:rosterbystander",` +
		`"role":"moderator","inviter":"did:plc:rosterowner","createdAt":"2026-11-11T07:00:00Z","expiresAt":"2026-11-25T07:00:00Z"}`
)

// listCommunityInvitationsCall is one ListCommunityInvitations call.
type listCommunityInvitationsCall struct{ actorDID, communityDID string }

// communityInvitationService answers every invitation procedure and query
// with one configured result or error, serves fixed invitations, roster
// versions and per-viewer inboxes, and records the requests it received.
type communityInvitationService struct {
	mu     sync.Mutex
	result *moderation.CommunityActionResult
	err    error

	byID              map[string]communities.Invitation
	rosterVersions    map[string]int64
	communityList     []communities.Invitation
	viewerInvitations map[string][]communities.Invitation

	inviteRequests  []moderation.InviteRequest
	offerRequests   []moderation.InviteRequest
	acceptRequests  []moderation.AcceptInvitationRequest
	declineRequests []moderation.DeclineInvitationRequest
	listCommunity   []listCommunityInvitationsCall
	listViewer      []string
}

func newCommunityInvitationService() *communityInvitationService {
	return &communityInvitationService{
		byID: map[string]communities.Invitation{
			moderatorInvitation.ID: moderatorInvitation,
			ownershipOffer.ID:      ownershipOffer,
		},
		rosterVersions: map[string]int64{invitationCommunityDID: 7, invitationOtherDID: 3},
		communityList:  []communities.Invitation{moderatorInvitation, ownershipOffer},
		viewerInvitations: map[string][]communities.Invitation{
			invitationInviteeDID:   {moderatorInvitation, otherCommunityInvitation},
			invitationBystanderDID: {bystanderInvitation},
		},
	}
}

func (s *communityInvitationService) actionResult() (*moderation.CommunityActionResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *communityInvitationService) InviteModerator(_ context.Context, req moderation.InviteRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inviteRequests = append(s.inviteRequests, req)
	return s.actionResult()
}

func (s *communityInvitationService) OfferOwnership(_ context.Context, req moderation.InviteRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offerRequests = append(s.offerRequests, req)
	return s.actionResult()
}

func (s *communityInvitationService) AcceptInvitation(_ context.Context, req moderation.AcceptInvitationRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acceptRequests = append(s.acceptRequests, req)
	return s.actionResult()
}

func (s *communityInvitationService) DeclineInvitation(_ context.Context, req moderation.DeclineInvitationRequest) (*moderation.CommunityActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declineRequests = append(s.declineRequests, req)
	return s.actionResult()
}

func (s *communityInvitationService) ListCommunityInvitations(_ context.Context, actorDID, communityDID string) ([]communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCommunity = append(s.listCommunity, listCommunityInvitationsCall{actorDID: actorDID, communityDID: communityDID})
	if s.err != nil {
		return nil, s.err
	}
	return s.communityList, nil
}

func (s *communityInvitationService) ListViewerInvitations(_ context.Context, viewerDID string) ([]communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listViewer = append(s.listViewer, viewerDID)
	if s.err != nil {
		return nil, s.err
	}
	return s.viewerInvitations[viewerDID], nil
}

func (s *communityInvitationService) Invitation(_ context.Context, invitationID string) (*communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	invitation, ok := s.byID[invitationID]
	if !ok {
		return nil, fmt.Errorf("fake: no invitation %q", invitationID)
	}
	return &invitation, nil
}

func (s *communityInvitationService) RosterVersion(_ context.Context, communityDID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.rosterVersions[communityDID]
	if !ok {
		return 0, fmt.Errorf("fake: no roster version for %q", communityDID)
	}
	return version, nil
}

func (s *communityInvitationService) set(result *moderation.CommunityActionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result, s.err = result, err
	s.inviteRequests, s.offerRequests, s.acceptRequests, s.declineRequests = nil, nil, nil, nil
	s.listCommunity, s.listViewer = nil, nil
}

// invitationRouteClient sends authenticated or signed-out requests to the
// invitation routes registered on a test server.
type invitationRouteClient struct {
	server   *httptest.Server
	sessions *viewerAuthoritySessions
}

func newInvitationRouteClient(t *testing.T, service *communityInvitationService) *invitationRouteClient {
	t.Helper()
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:instanceadmin"})
	mux := chi.NewRouter()
	routes.RegisterCommunityInvitationRoutes(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), service)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &invitationRouteClient{server: server, sessions: sessions}
}

// send issues a POST (body non-empty) or a GET (query appended to the NSID)
// as caller; an empty caller is signed out.
func (c *invitationRouteClient) send(t *testing.T, method, nsid, body, caller string) (int, string) {
	t.Helper()
	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	target := c.server.URL + "/xrpc/" + nsid
	if method == http.MethodGet && body != "" {
		target += "?" + body
	}
	request, err := http.NewRequestWithContext(t.Context(), method, target, reader)
	require.NoError(t, err)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if caller != "" {
		request.Header.Set("Authorization", "Bearer "+c.sessions.add(caller))
	}
	response, err := c.server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(responseBody)
}

// TestCommunityInvitationRoutes drives the six invitation endpoints through
// their route registration: authentication, the request the service
// receives (the actor is always the authenticated caller), the success body
// and the mapping of service refusals.
func TestCommunityInvitationRoutes(t *testing.T) {
	service := newCommunityInvitationService()
	client := newInvitationRouteClient(t, service)

	endpoints := []struct {
		name   string
		method string
		nsid   string
		caller string
		// body is the JSON input of a procedure or the query string of a query.
		body string
		// result is what an action method returns; wantBody is the response.
		result   *moderation.CommunityActionResult
		wantBody string
		// received checks the one request the service received.
		received func(t *testing.T)
	}{
		{
			name: "inviteModerator", method: http.MethodPost, nsid: "social.coves.community.inviteModerator",
			caller: invitationOwnerDID,
			body:   `{"community":"did:plc:rosteralpha","subject":"invitee.coves.social","expectedVersion":7,"idempotencyKey":"invite-1","actor":"did:plc:spoofedactor"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: invitationCommunityDID, SubjectDID: invitationInviteeDID,
				InvitationID: moderatorInvitation.ID, Version: 7,
			},
			wantBody: `{"invitation":` + moderatorInvitationView + `,"version":7}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.InviteRequest{{
					ActorDID: invitationOwnerDID, CommunityDID: invitationCommunityDID,
					Invitee: "invitee.coves.social", ExpectedVersion: 7, IdempotencyKey: "invite-1",
				}}, service.inviteRequests, "the authenticated caller is the actor; the handle reaches the service unresolved")
				assert.Empty(t, service.offerRequests)
			},
		},
		{
			name: "offerOwnership", method: http.MethodPost, nsid: "social.coves.community.offerOwnership",
			caller: invitationOwnerDID,
			body:   `{"community":"did:plc:rosteralpha","subject":"did:plc:troublemaker456","expectedVersion":7,"idempotencyKey":"offer-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: invitationCommunityDID, SubjectDID: invitationModeratorDID,
				InvitationID: ownershipOffer.ID, Version: 7,
			},
			wantBody: `{"invitation":` + ownershipOfferView + `,"version":7}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.InviteRequest{{
					ActorDID: invitationOwnerDID, CommunityDID: invitationCommunityDID,
					Invitee: invitationModeratorDID, ExpectedVersion: 7, IdempotencyKey: "offer-1",
				}}, service.offerRequests, "the authenticated caller is the actor")
				assert.Empty(t, service.inviteRequests)
			},
		},
		{
			name: "acceptInvitation", method: http.MethodPost, nsid: "social.coves.community.acceptInvitation",
			caller: invitationInviteeDID,
			body:   `{"invitation":"invitation-1","expectedVersion":7,"idempotencyKey":"accept-1","actor":"did:plc:rosterowner"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: invitationCommunityDID, SubjectDID: invitationInviteeDID,
				RecordURI: "at://did:plc:rosteralpha/social.coves.community.moderator/accept",
				ActionID:  "community-action-1", InvitationID: moderatorInvitation.ID, Version: 8,
			},
			wantBody: `{"community":"did:plc:rosteralpha","subject":"did:plc:rosterinvitee","version":8}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.AcceptInvitationRequest{{
					ActorDID: invitationInviteeDID, InvitationID: moderatorInvitation.ID,
					ExpectedVersion: 7, IdempotencyKey: "accept-1",
				}}, service.acceptRequests, "the authenticated invitee is the actor, never a body field")
			},
		},
		{
			name: "declineInvitation", method: http.MethodPost, nsid: "social.coves.community.declineInvitation",
			caller: invitationInviteeDID,
			body:   `{"invitation":"invitation-1","expectedVersion":7,"idempotencyKey":"decline-1"}`,
			result: &moderation.CommunityActionResult{
				CommunityDID: invitationCommunityDID, SubjectDID: invitationInviteeDID,
				InvitationID: moderatorInvitation.ID, Version: 7,
			},
			wantBody: `{"invitation":"invitation-1"}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []moderation.DeclineInvitationRequest{{
					ActorDID: invitationInviteeDID, InvitationID: moderatorInvitation.ID,
					ExpectedVersion: 7, IdempotencyKey: "decline-1",
				}}, service.declineRequests, "the authenticated invitee is the actor")
			},
		},
		{
			name: "listInvitations", method: http.MethodGet, nsid: "social.coves.community.listInvitations",
			caller:   invitationOwnerDID,
			body:     "community=did:plc:rosteralpha",
			wantBody: `{"invitations":[` + moderatorInvitationView + `,` + ownershipOfferView + `]}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []listCommunityInvitationsCall{{actorDID: invitationOwnerDID, communityDID: invitationCommunityDID}},
					service.listCommunity, "the authenticated caller is the actor the service authorizes")
			},
		},
		{
			name: "listViewerInvitations", method: http.MethodGet, nsid: "social.coves.community.listViewerInvitations",
			caller: invitationInviteeDID,
			wantBody: `{"invitations":[` +
				`{"invitation":` + moderatorInvitationView + `,"rosterVersion":7},` +
				`{"invitation":` + otherCommunityInvitationView + `,"rosterVersion":3}]}`,
			received: func(t *testing.T) {
				service.mu.Lock()
				defer service.mu.Unlock()
				assert.Equal(t, []string{invitationInviteeDID}, service.listViewer)
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
		{err: moderation.ErrInviteeHasRole, wantStatus: http.StatusBadRequest, wantError: "InviteeHasRole"},
		{err: moderation.ErrInviteeNotModerator, wantStatus: http.StatusBadRequest, wantError: "InviteeNotModerator"},
		{err: moderation.ErrInviteeBanned, wantStatus: http.StatusBadRequest, wantError: "InviteeBanned"},
		{err: moderation.ErrInvitationNotFound, wantStatus: http.StatusNotFound, wantError: "InvitationNotFound"},
		{err: moderation.ErrInvitationLapsed, wantStatus: http.StatusBadRequest, wantError: "InvitationLapsed"},
		{err: moderation.ErrInvitationNotOpen, wantStatus: http.StatusBadRequest, wantError: "InvitationNotOpen"},
		{err: moderation.ErrInvitationStale, wantStatus: http.StatusBadRequest, wantError: "InvitationStale"},
		{err: moderation.ErrAccountNotFound, wantStatus: http.StatusNotFound, wantError: "AccountNotFound"},
		{err: moderation.ErrModerationUnavailable, wantStatus: http.StatusServiceUnavailable, wantError: "ModerationUnavailable"},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			t.Run("unauthenticated", func(t *testing.T) {
				service.set(endpoint.result, nil)

				status, body := client.send(t, endpoint.method, endpoint.nsid, endpoint.body, "")

				assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)
				assert.Equal(t, "AuthRequired", xrpcErrorName(t, body))
			})

			t.Run("success", func(t *testing.T) {
				service.set(endpoint.result, nil)

				status, body := client.send(t, endpoint.method, endpoint.nsid, endpoint.body, endpoint.caller)

				require.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.JSONEq(t, endpoint.wantBody, body)
				endpoint.received(t)
			})

			// A listInvitations caller the service refuses (a moderator or an
			// unrelated account) is the ErrForbidden row of this table.
			for _, refusal := range refusals {
				t.Run(refusal.wantError, func(t *testing.T) {
					service.set(nil, fmt.Errorf("%w: refused by the service", refusal.err))

					status, body := client.send(t, endpoint.method, endpoint.nsid, endpoint.body, endpoint.caller)

					assert.Equal(t, refusal.wantStatus, status, "body: %s", body)
					assert.Equal(t, refusal.wantError, xrpcErrorName(t, body))
				})
			}
		})
	}
}

// TestCommunityInvitationRoutesExpectedVersion pins that the four invitation
// procedures require expectedVersion: an omitted or null value is refused
// with InvalidRequest before the service runs, while an explicit 0 is a valid
// version passed through unchanged.
func TestCommunityInvitationRoutesExpectedVersion(t *testing.T) {
	service := newCommunityInvitationService()
	client := newInvitationRouteClient(t, service)
	inviteResult := &moderation.CommunityActionResult{
		CommunityDID: invitationCommunityDID, SubjectDID: invitationInviteeDID,
		InvitationID: moderatorInvitation.ID, Version: 0,
	}

	versionsOfInvites := func(requests []moderation.InviteRequest) []int64 {
		versions := []int64{}
		for _, request := range requests {
			versions = append(versions, request.ExpectedVersion)
		}
		return versions
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
			name: "inviteModerator", nsid: "social.coves.community.inviteModerator", caller: invitationOwnerDID,
			fields: `"community":"did:plc:rosteralpha","subject":"did:plc:rosterinvitee","idempotencyKey":"invite-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				return versionsOfInvites(service.inviteRequests)
			},
		},
		{
			name: "offerOwnership", nsid: "social.coves.community.offerOwnership", caller: invitationOwnerDID,
			fields: `"community":"did:plc:rosteralpha","subject":"did:plc:rosterinvitee","idempotencyKey":"offer-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				return versionsOfInvites(service.offerRequests)
			},
		},
		{
			name: "acceptInvitation", nsid: "social.coves.community.acceptInvitation", caller: invitationInviteeDID,
			fields: `"invitation":"invitation-1","idempotencyKey":"accept-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				versions := []int64{}
				for _, request := range service.acceptRequests {
					versions = append(versions, request.ExpectedVersion)
				}
				return versions
			},
		},
		{
			name: "declineInvitation", nsid: "social.coves.community.declineInvitation", caller: invitationInviteeDID,
			fields: `"invitation":"invitation-1","idempotencyKey":"decline-version"`,
			receivedVersions: func() []int64 {
				service.mu.Lock()
				defer service.mu.Unlock()
				versions := []int64{}
				for _, request := range service.declineRequests {
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
					service.set(inviteResult, nil)

					status, body := client.send(t, http.MethodPost, procedure.nsid, missing.body, procedure.caller)

					assert.Equal(t, http.StatusBadRequest, status, "body: %s", body)
					assert.Equal(t, "InvalidRequest", xrpcErrorName(t, body))
					assert.Empty(t, procedure.receivedVersions(), "the service must not run without expectedVersion")
				})
			}

			t.Run("explicit zero", func(t *testing.T) {
				service.set(inviteResult, nil)

				status, body := client.send(t, http.MethodPost, procedure.nsid, "{"+procedure.fields+`,"expectedVersion":0}`, procedure.caller)

				assert.Equal(t, http.StatusOK, status, "body: %s", body)
				assert.Equal(t, []int64{0}, procedure.receivedVersions())
			})
		})
	}
}

// TestListViewerInvitationsServesOnlyTheCallersInbox pins that the
// authenticated viewer's DID alone chooses the inbox: a query parameter
// naming another account does not change whose invitations are served.
func TestListViewerInvitationsServesOnlyTheCallersInbox(t *testing.T) {
	service := newCommunityInvitationService()
	client := newInvitationRouteClient(t, service)

	for _, test := range []struct {
		name     string
		viewer   string
		query    string
		wantBody string
	}{
		{
			name: "bystander naming the invitee as actor", viewer: invitationBystanderDID,
			query:    "actor=did:plc:rosterinvitee",
			wantBody: `{"invitations":[{"invitation":` + bystanderInvitationView + `,"rosterVersion":3}]}`,
		},
		{
			name: "bystander naming the invitee as subject", viewer: invitationBystanderDID,
			query:    "subject=did:plc:rosterinvitee",
			wantBody: `{"invitations":[{"invitation":` + bystanderInvitationView + `,"rosterVersion":3}]}`,
		},
		{
			name: "invitee naming the bystander as actor and subject", viewer: invitationInviteeDID,
			query: "actor=did:plc:rosterbystander&subject=did:plc:rosterbystander",
			wantBody: `{"invitations":[` +
				`{"invitation":` + moderatorInvitationView + `,"rosterVersion":7},` +
				`{"invitation":` + otherCommunityInvitationView + `,"rosterVersion":3}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.set(nil, nil)

			status, body := client.send(t, http.MethodGet, "social.coves.community.listViewerInvitations", test.query, test.viewer)

			require.Equal(t, http.StatusOK, status, "body: %s", body)
			assert.JSONEq(t, test.wantBody, body)
			service.mu.Lock()
			defer service.mu.Unlock()
			assert.Equal(t, []string{test.viewer}, service.listViewer, "the inbox is the authenticated viewer's")
		})
	}
}
