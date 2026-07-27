package pds

import (
	"context"
	"errors"
	"fmt"
	"testing"

	covesoauth "Coves/internal/atproto/oauth"
)

// TestIsReauthRequired pins which conditions mean "the user must sign in again".
//
// This predicate is the first thing the API error mapper consults, and it is the
// only thing standing between a real expired session and a spurious sign-out.
// The ErrForbidden row is the important one: a missing OAuth scope is not a dead
// session, and answering 401 for it would sign users out over a permissions gap
// that signing in again does not obviously fix.
func TestIsReauthRequired(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unauthorized", ErrUnauthorized, true},
		{"session expired", ErrSessionExpired, true},
		{"forbidden is NOT reauth", ErrForbidden, false},
		{"not found", ErrNotFound, false},
		{"bad request", ErrBadRequest, false},
		{"rate limited", ErrRateLimited, false},
		{"unrelated", errors.New("boom"), false},
		{"wrapped unauthorized", fmt.Errorf("CreateRecord: %w: expired", ErrUnauthorized), true},
		{"wrapped session expired", fmt.Errorf("resume: %w", ErrSessionExpired), true},
		{"double-wrapped behind a domain sentinel",
			fmt.Errorf("%w: %w", errors.New("not authorized"), ErrUnauthorized), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReauthRequired(tt.err); got != tt.want {
				t.Errorf("IsReauthRequired(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsAuthErrorSpansForbidden guards the distinction IsAuthError deliberately
// blurs, so nobody "simplifies" the two predicates back into one.
func TestIsAuthErrorSpansForbidden(t *testing.T) {
	if !IsAuthError(ErrForbidden) {
		t.Error("IsAuthError must cover 403")
	}
	if IsReauthRequired(ErrForbidden) {
		t.Error("IsReauthRequired must NOT cover 403 — that is the whole point of having both")
	}
	if !IsAuthError(ErrSessionExpired) {
		t.Error("IsAuthError must cover a dead session")
	}
}

// TestNewFromOAuthSessionClassifiesResumeFailures is the test whose absence made
// every "expired session returns 401" test elsewhere tautological: they all
// hand-constructed an ErrSessionExpired-tagged error rather than proving the
// factory produces one.
//
// It also pins the narrowing that matters operationally. ResumeSession is a
// session-store read, so a store outage surfaces here identically to a missing
// row — and because the mapper checks re-auth first, tagging both would answer
// 401 to every in-flight request during a database blip.
func TestNewFromOAuthSessionClassifiesResumeFailures(t *testing.T) {
	tests := []struct {
		name          string
		storeErr      error
		wantReauth    bool
		wantCauseKept bool
	}{
		{
			name:          "missing or expired session is terminal",
			storeErr:      covesoauth.ErrSessionNotFound,
			wantReauth:    true,
			wantCauseKept: true,
		},
		{
			name:          "wrapped missing session is still terminal",
			storeErr:      fmt.Errorf("lookup: %w", covesoauth.ErrSessionNotFound),
			wantReauth:    true,
			wantCauseKept: true,
		},
		{
			name:       "store outage must NOT read as an expired session",
			storeErr:   errors.New("failed to get session: connection reset by peer"),
			wantReauth: false,
		},
		{
			name:       "cancelled request must NOT read as an expired session",
			storeErr:   context.Canceled,
			wantReauth: false,
		},
		{
			name:       "deadline must NOT read as an expired session",
			storeErr:   context.DeadlineExceeded,
			wantReauth: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyResumeFailure(tt.storeErr, "did:plc:test", "session-1")

			if got := IsReauthRequired(err); got != tt.wantReauth {
				t.Errorf("IsReauthRequired = %v, want %v (err: %v)", got, tt.wantReauth, err)
			}
			// The cause must stay reachable either way — it is what gets logged.
			if !errors.Is(err, tt.storeErr) {
				t.Errorf("underlying cause is no longer matchable: %v", err)
			}
			if tt.wantCauseKept && !errors.Is(err, ErrSessionExpired) {
				t.Error("expected ErrSessionExpired to be matchable alongside the cause")
			}
		})
	}
}

// Non-terminal failures must also stay clear of the context rules being
// pre-empted: a cancelled request should still look cancelled at the boundary.
func TestResumeFailureLeavesContextErrorsIntact(t *testing.T) {
	err := classifyResumeFailure(context.Canceled, "did:plc:test", "session-1")

	if !errors.Is(err, context.Canceled) {
		t.Error("context.Canceled must remain matchable so the boundary can answer 400, not 401")
	}
	if errors.Is(err, ErrSessionExpired) {
		t.Error("a cancelled request is not an expired session")
	}
}
