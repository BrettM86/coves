package vote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"
)

// TestExpiredSessionAnswers401 covers the reported bug: voting with an expired
// OAuth session answered 500 InternalServerError, so clients had no signal to
// re-authenticate and users saw "an internal error occurred" until they signed
// out by hand.
//
// The service reports its own ErrNotAuthorized with the PDS cause attached, and
// the mapper has to see past the former to the latter.
func TestExpiredSessionAnswers401(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "pds rejected the token",
			err: fmt.Errorf("%w: %w", votes.ErrNotAuthorized,
				fmt.Errorf("CreateRecord: %w: expired token", pds.ErrUnauthorized)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name: "session could not be resumed",
			err: fmt.Errorf("failed to create PDS client: %w",
				fmt.Errorf("failed to resume OAuth session: %w: revoked", pds.ErrSessionExpired)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			// A scope gap is not a dead session and must not sign the user out.
			name: "missing scope stays 403",
			err: fmt.Errorf("%w: %w", votes.ErrNotAuthorized,
				fmt.Errorf("CreateRecord: %w", pds.ErrForbidden)),
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
		},
		{
			// An appview-level refusal has no PDS cause and keeps its own answer.
			name:       "domain refusal stays 403",
			err:        votes.ErrNotAuthorized,
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
		},
		{
			name:       "pds rate limit is no longer a 500",
			err:        fmt.Errorf("CreateRecord: %w", pds.ErrRateLimited),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "RateLimitExceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, tt.err)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var body XRPCError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if body.Error != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error, tt.wantCode)
			}
		})
	}
}

// The vote error codes are a client contract; this pins them so a mapper
// refactor cannot quietly rename one. The wrapped forms are the ones that
// changed: errors.Is now matches where == did not.
func TestVoteErrorCodes(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{votes.ErrVoteNotFound, http.StatusNotFound, "VoteNotFound"},
		{votes.ErrInvalidDirection, http.StatusBadRequest, "InvalidRequest"},
		{votes.ErrInvalidSubject, http.StatusBadRequest, "InvalidSubject"},
		{votes.ErrVoteAlreadyExists, http.StatusConflict, "AlreadyExists"},
		{votes.ErrNotAuthorized, http.StatusForbidden, "NotAuthorized"},
		{votes.ErrBanned, http.StatusForbidden, "NotAuthorized"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode+"/"+tt.err.Error(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			// Wrapped, because service layers add context on the way up.
			handleServiceError(rec, fmt.Errorf("service: %w", tt.err))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var body XRPCError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if body.Error != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error, tt.wantCode)
			}
		})
	}
}
