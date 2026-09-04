package community

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
)

// TestCommunityErrorCodes pins the wire contract, exercising every sentinel
// WRAPPED — which is how it actually arrives, since the service adds context on
// the way up. The mapper this replaced compared with ==, so a wrapped sentinel
// fell through; these rows are the behavior that changed.
//
// NOTE: ErrHandleTaken is a deliberate contract change. The old switch reached
// NameTaken only via `err == communities.ErrHandleTaken`, and the service wraps
// it ("failed to persist community with credentials: %w"), so the real create
// path always answered AlreadyExists. errors.Is now matches, so it answers
// NameTaken — the code the old switch plainly intended. Clients keying on
// AlreadyExists for a handle collision need updating.
func TestCommunityErrorCodes(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "handle collision reaches NameTaken through a wrapper",
			err:        fmt.Errorf("failed to persist community: %w", communities.ErrHandleTaken),
			wantStatus: http.StatusConflict,
			wantCode:   "NameTaken",
		},
		{
			name:       "other conflicts stay AlreadyExists",
			err:        fmt.Errorf("service: %w", communities.ErrCommunityAlreadyExists),
			wantStatus: http.StatusConflict,
			wantCode:   "AlreadyExists",
		},
		{
			name:       "appview authorization refusal",
			err:        fmt.Errorf("service: %w", communities.ErrUnauthorized),
			wantStatus: http.StatusForbidden,
			wantCode:   "Forbidden",
		},
		{
			name:       "stored session predates community block scope",
			err:        fmt.Errorf("service: %w", communities.ErrCommunityBlockScopeRequired),
			wantStatus: http.StatusForbidden,
			wantCode:   "OAuthScopeRequired",
		},
		{
			name:       "banned member",
			err:        fmt.Errorf("service: %w", communities.ErrMemberBanned),
			wantStatus: http.StatusForbidden,
			wantCode:   "Blocked",
		},
		{
			name:       "ambiguous name@origin is neither missing nor malformed",
			err:        fmt.Errorf("resolving scoped identifier !comicstrips@lemmy.world: %w", communities.ErrAmbiguousCommunity),
			wantStatus: http.StatusConflict,
			wantCode:   "AmbiguousCommunity",
		},
		{
			name:       "missing community",
			err:        fmt.Errorf("service: %w", communities.ErrCommunityNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   "NotFound",
		},
		{
			name:       "invalid input",
			err:        fmt.Errorf("service: %w", communities.ErrInvalidInput),
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidRequest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, tt.err)
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// Subscribe and block write to the user's own repo, so a dead session here must
// reach the client as 401 — this package's whole reason for having PDS rules.
func TestCommunityPDSErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "expired token behind the domain sentinel",
			err: fmt.Errorf("%w: %w", communities.ErrUnauthorized,
				fmt.Errorf("CreateRecord: %w", pds.ErrUnauthorized)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name: "missing scope keeps the domain's 403",
			err: fmt.Errorf("%w: %w", communities.ErrUnauthorized,
				fmt.Errorf("CreateRecord: %w", pds.ErrForbidden)),
			wantStatus: http.StatusForbidden,
			wantCode:   "Forbidden",
		},
		{
			// Previously 500: this package's hand-written switch omitted 429/413.
			name:       "pds rate limit",
			err:        fmt.Errorf("CreateRecord: %w", pds.ErrRateLimited),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "RateLimitExceeded",
		},
		{
			name:       "pds payload too large",
			err:        fmt.Errorf("CreateRecord: %w", pds.ErrPayloadTooLarge),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "PayloadTooLarge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, tt.err)
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestCommunityBlockScopeErrorIsActionable(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, communities.ErrCommunityBlockScopeRequired)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	var body xrpc.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Error != "OAuthScopeRequired" {
		t.Errorf("code = %q, want OAuthScopeRequired", body.Error)
	}
	if !strings.Contains(body.Message, "Sign out and back in") {
		t.Errorf("message %q is not actionable; an existing session cannot acquire a new scope without reauthorization", body.Message)
	}
}

func TestUnmappedErrorIsGeneric500(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("pq: connection refused to 10.0.0.4:5432"))

	assertXRPCError(t, rec, http.StatusInternalServerError, "InternalServerError")
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.4") {
		t.Errorf("internal detail leaked to the client: %s", body)
	}
}

func assertXRPCError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d", rec.Code, wantStatus)
	}
	var body xrpc.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Error != wantCode {
		t.Errorf("code = %q, want %q", body.Error, wantCode)
	}
}
