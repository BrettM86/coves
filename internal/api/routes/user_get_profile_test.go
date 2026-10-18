package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"Coves/internal/core/users"
)

// stubUserService implements only the two methods GetProfile calls. The
// embedded nil interface supplies the rest: any other call panics, which is the
// behaviour we want if this handler grows a dependency the test does not model.
type stubUserService struct {
	users.UserService

	resolvedDID string
	resolveErr  error
	profile     *users.ProfileViewDetailed
	profileErr  error

	// actorResolutions, when set, answers ResolveActorIdentifierToDID per
	// identifier. When nil that call answers like ResolveHandleToDID.
	actorResolutions map[string]stubResolution
	// profilesByDID, when set, answers GetProfile per DID; a DID missing from
	// it is ErrUserNotFound.
	profilesByDID map[string]*users.ProfileViewDetailed
}

type stubResolution struct {
	did string
	err error
}

func (s *stubUserService) ResolveHandleToDID(context.Context, string) (string, error) {
	return s.resolvedDID, s.resolveErr
}

func (s *stubUserService) ResolveActorIdentifierToDID(_ context.Context, identifier string) (string, error) {
	if s.actorResolutions == nil {
		return s.resolvedDID, s.resolveErr
	}
	resolution, ok := s.actorResolutions[identifier]
	if !ok {
		return "", fmt.Errorf("stub: unmodeled actor identifier %q", identifier)
	}
	return resolution.did, resolution.err
}

func (s *stubUserService) GetProfile(_ context.Context, did string) (*users.ProfileViewDetailed, error) {
	if s.profilesByDID != nil {
		profile, ok := s.profilesByDID[did]
		if !ok {
			return nil, users.ErrUserNotFound
		}
		return profile, nil
	}
	return s.profile, s.profileErr
}

func getProfile(t *testing.T, svc users.UserService, actor string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/xrpc/social.coves.actor.getProfile"
	if actor != "" {
		target += "?actor=" + url.QueryEscape(actor)
	}
	w := httptest.NewRecorder()
	NewUserHandler(svc).GetProfile(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// TestGetProfile_ResolutionErrorMapping covers the split this handler makes on
// purpose: a handle that provably does not exist is the caller's problem (404),
// while a resolver that could not answer is ours (500). Collapsing the second
// into the first would tell users their handle is wrong during a DNS or PLC
// outage, and would hide that outage from anyone watching 5xx rates.
//
// It lives here because the integration test that used to cover it now passes a
// DID — proving an unregistered *handle* does not exist requires public DNS,
// which the hermetic stack deliberately cannot reach. A stub resolver covers
// both branches with no network at all.
func TestGetProfile_ResolutionErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		resolveErr error
		wantStatus int
		wantError  string
	}{
		{
			name:       "handle does not exist is a 404",
			resolveErr: users.ErrUserNotFound,
			wantStatus: http.StatusNotFound,
			wantError:  "ProfileNotFound",
		},
		{
			name:       "resolver outage is a 500, not a 404",
			resolveErr: errors.New("lookup _atproto.example.test: server misbehaving"),
			wantStatus: http.StatusInternalServerError,
			wantError:  "InternalError",
		},
		{
			name:       "malformed handle is a 400",
			resolveErr: &users.InvalidHandleError{Handle: "not a handle", Reason: "contains a space"},
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidRequest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := getProfile(t, &stubUserService{resolveErr: tc.resolveErr}, "someone.test")

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			var body struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error != tc.wantError {
				t.Errorf("error = %q, want %q", body.Error, tc.wantError)
			}
		})
	}
}

// TestGetProfile_UnknownDIDIsNotFound is the other half: an actor that is
// already a DID skips resolution entirely, so "not in the index" reaches the
// caller as a 404 rather than as a resolver failure.
func TestGetProfile_UnknownDIDIsNotFound(t *testing.T) {
	w := getProfile(t, &stubUserService{profileErr: users.ErrUserNotFound}, "did:plc:notindexed")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// TestGetProfile_ProfileLookupFailureIs500 keeps the same distinction on the
// second lookup: a database failure must not read as a missing user.
func TestGetProfile_ProfileLookupFailureIs500(t *testing.T) {
	w := getProfile(t, &stubUserService{profileErr: errors.New("connection refused")}, "did:plc:indexed")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestGetProfile_MissingActorIsBadRequest(t *testing.T) {
	w := getProfile(t, &stubUserService{}, "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestGetProfile_ResolvesScopedActorIdentifier covers getProfile's use of the
// actor-identifier resolver: a name@host actor reaches the profile lookup as
// the resolved DID, and the resolver's ambiguity, not-found and validation
// errors keep distinct answers. The legacy handle resolver answers a different
// DID so a handler that never switched shows up as a wrong profile.
func TestGetProfile_ResolvesScopedActorIdentifier(t *testing.T) {
	const (
		aliceDID  = "did:plc:alicebridged000000000000"
		legacyDID = "did:plc:legacypath000000000000"
	)
	svc := &stubUserService{
		resolvedDID: legacyDID,
		actorResolutions: map[string]stubResolution{
			"Alice_B@lemmy.world":  {did: aliceDID},
			"Carol@example.social": {err: fmt.Errorf("resolve Carol@example.social: %w", users.ErrAmbiguousUser)},
			"nobody@lemmy.world":   {err: fmt.Errorf("resolve nobody@lemmy.world: %w", users.ErrUserNotFound)},
			"alice@not a host":     {err: &users.InvalidHandleError{Handle: "alice@not a host", Reason: "invalid host"}},
		},
		profilesByDID: map[string]*users.ProfileViewDetailed{
			aliceDID:  {DID: aliceDID, Handle: "alice-b.lemmy-world.ap.example"},
			legacyDID: {DID: legacyDID, Handle: "legacy.example"},
		},
	}

	tests := []struct {
		name       string
		actor      string
		wantStatus int
		wantError  string
		wantDID    string
	}{
		{
			name:       "name@host answers the resolved user's profile",
			actor:      "Alice_B@lemmy.world",
			wantStatus: http.StatusOK,
			wantDID:    "did:plc:alicebridged000000000000",
		},
		{
			name:       "ambiguous address is a 409 AmbiguousActor",
			actor:      "Carol@example.social",
			wantStatus: http.StatusConflict,
			wantError:  "AmbiguousActor",
		},
		{
			name:       "unknown address is a 404 ProfileNotFound",
			actor:      "nobody@lemmy.world",
			wantStatus: http.StatusNotFound,
			wantError:  "ProfileNotFound",
		},
		{
			name:       "invalid address is a 400 InvalidRequest",
			actor:      "alice@not a host",
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidRequest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := getProfile(t, svc, tc.actor)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			var body struct {
				DID   string `json:"did"`
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != tc.wantError {
				t.Errorf("error = %q, want %q", body.Error, tc.wantError)
			}
			if body.DID != tc.wantDID {
				t.Errorf("did = %q, want %q", body.DID, tc.wantDID)
			}
		})
	}
}
