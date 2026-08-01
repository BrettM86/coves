package routes

import (
	"Coves/internal/core/users"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
}

func (s *stubUserService) ResolveHandleToDID(context.Context, string) (string, error) {
	return s.resolvedDID, s.resolveErr
}

func (s *stubUserService) GetProfile(context.Context, string) (*users.ProfileViewDetailed, error) {
	return s.profile, s.profileErr
}

func getProfile(t *testing.T, svc users.UserService, actor string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/xrpc/social.coves.actor.getProfile"
	if actor != "" {
		target += "?actor=" + actor
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
