package xrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	coreerrors "Coves/internal/core/errors"
)

// Stand-ins for a domain package's sentinels.
var (
	errDomainNotAuthorized = errors.New("not authorized")
	errDomainNotFound      = errors.New("thing not found")
)

func newTestMapper() *xrpc.Mapper {
	return xrpc.NewMapper("test",
		xrpc.Sentinel(errDomainNotAuthorized, http.StatusForbidden,
			"NotAuthorized", "You may not do that"),
		xrpc.Sentinel(errDomainNotFound, http.StatusNotFound,
			"ThingNotFound", "Thing not found"),
	)
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMatch  bool
	}{
		{
			name:       "domain sentinel",
			err:        errDomainNotAuthorized,
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
			wantMatch:  true,
		},
		{
			// The regression this package exists to prevent: a wrapped sentinel
			// must still match. An == comparison would drop to 500 here.
			name:       "domain sentinel wrapped with %w",
			err:        fmt.Errorf("service call failed: %w", errDomainNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   "ThingNotFound",
			wantMatch:  true,
		},
		{
			name:       "pds 401 with no domain sentinel",
			err:        fmt.Errorf("CreateRecord: %w: token expired", pds.ErrUnauthorized),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
			wantMatch:  true,
		},
		{
			// A session that could not be resumed never reaches the PDS to earn
			// a 401, but has the same remedy.
			name:       "session expired",
			err:        fmt.Errorf("failed to resume: %w: gone", pds.ErrSessionExpired),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
			wantMatch:  true,
		},
		{
			// 403 is a scope problem, not a dead session; it must not answer 401
			// or clients would sign the user out over a permissions gap.
			name:       "pds 403 stays 403",
			err:        fmt.Errorf("CreateRecord: %w: no scope", pds.ErrForbidden),
			wantStatus: http.StatusForbidden,
			wantCode:   "PermissionDenied",
			wantMatch:  true,
		},
		{
			name:       "pds 429 inherited",
			err:        fmt.Errorf("CreateRecord: %w", pds.ErrRateLimited),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "RateLimitExceeded",
			wantMatch:  true,
		},
		{
			name:       "pds 413 inherited",
			err:        fmt.Errorf("UploadBlob: %w", pds.ErrPayloadTooLarge),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "PayloadTooLarge",
			wantMatch:  true,
		},
		{
			// A lost swap arrives from a live PDS as HTTP 400, so without its
			// own rule it would be indistinguishable from a malformed request —
			// or worse, fall to 500. It must answer 409: a retryable conflict.
			name:       "pds swap conflict is 409 not 400",
			err:        pds.ErrSwapConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "Conflict",
			wantMatch:  true,
		},
		{
			name:       "pds swap conflict wrapped with %w",
			err:        fmt.Errorf("applyWrites: %w: CID mismatch", pds.ErrSwapConflict),
			wantStatus: http.StatusConflict,
			wantCode:   "Conflict",
			wantMatch:  true,
		},
		{
			// A 409 InvalidSwap wraps ErrConflict and ErrSwapConflict at once;
			// both mean 409 Conflict, whichever rule wins.
			name:       "pds 409 swap wraps both conflict sentinels",
			err:        fmt.Errorf("applyWrites: %w: %w: stale", pds.ErrConflict, pds.ErrSwapConflict),
			wantStatus: http.StatusConflict,
			wantCode:   "Conflict",
			wantMatch:  true,
		},
		{
			// A PDS 5xx is a classified upstream failure: 502, not the
			// content-free 500 reserved for errors nothing recognized.
			name:       "pds server error is 502 upstream failure",
			err:        pds.ErrServerError,
			wantStatus: http.StatusBadGateway,
			wantCode:   "UpstreamFailure",
			wantMatch:  true,
		},
		{
			name:       "pds server error wrapped with %w",
			err:        fmt.Errorf("applyWrites: %w: Internal Server Error", pds.ErrServerError),
			wantStatus: http.StatusBadGateway,
			wantCode:   "UpstreamFailure",
			wantMatch:  true,
		},
		{
			name:       "shared typed validation error",
			err:        coreerrors.NewValidationError("handle", "is required"),
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidRequest",
			wantMatch:  true,
		},
		{
			name:       "shared typed not found",
			err:        coreerrors.NewNotFoundError("post", "at://x"),
			wantStatus: http.StatusNotFound,
			wantCode:   "NotFound",
			wantMatch:  true,
		},
		{
			name:       "shared typed conflict",
			err:        coreerrors.NewConflictError("community", "handle", "!go"),
			wantStatus: http.StatusConflict,
			wantCode:   "AlreadyExists",
			wantMatch:  true,
		},
		{
			name:       "deadline exceeded",
			err:        fmt.Errorf("query: %w", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "Timeout",
			wantMatch:  true,
		},
		{
			name:       "canceled",
			err:        fmt.Errorf("query: %w", context.Canceled),
			wantStatus: http.StatusBadRequest,
			wantCode:   "RequestCanceled",
			wantMatch:  true,
		},
		{
			name:       "unmapped falls through and reports no match",
			err:        errors.New("something nobody anticipated"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "InternalServerError",
			wantMatch:  false,
		},
	}

	mapper := newTestMapper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := mapper.Resolve(tt.err)
			if got.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tt.wantStatus)
			}
			if got.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tt.wantCode)
			}
			if matched != tt.wantMatch {
				t.Errorf("matched = %v, want %v", matched, tt.wantMatch)
			}
		})
	}
}

// TestReauthBeatsDomainSentinel is the fix for the bug that motivated this
// package. Services translate a PDS auth failure into their own sentinel, which
// maps to 403. If that were consulted first, a user whose session expired would
// get 403 forever with no signal to sign in again.
func TestReauthBeatsDomainSentinel(t *testing.T) {
	mapper := newTestMapper()

	// What a service now returns: its own sentinel, with the cause preserved.
	expired := fmt.Errorf("%w: %w",
		errDomainNotAuthorized,
		fmt.Errorf("CreateRecord: %w: token expired", pds.ErrUnauthorized))

	// Both are matchable...
	if !errors.Is(expired, errDomainNotAuthorized) {
		t.Fatal("domain sentinel should still match")
	}
	if !errors.Is(expired, pds.ErrUnauthorized) {
		t.Fatal("pds cause should still match")
	}

	// ...and the dead session wins.
	got, matched := mapper.Resolve(expired)
	if !matched {
		t.Fatal("expected a match")
	}
	if got.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 so the client re-authenticates", got.Status)
	}
	if got.Code != "AuthRequired" {
		t.Errorf("code = %q, want AuthRequired", got.Code)
	}

	// A 403 layered the same way keeps the domain's own answer, since both mean
	// "not allowed" and neither is fixed by signing in again.
	forbidden := fmt.Errorf("%w: %w",
		errDomainNotAuthorized,
		fmt.Errorf("CreateRecord: %w", pds.ErrForbidden))
	got, _ = mapper.Resolve(forbidden)
	if got.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got.Status)
	}
	if got.Code != "NotAuthorized" {
		t.Errorf("code = %q, want the domain code NotAuthorized", got.Code)
	}
}

func TestRuleOrderIsFirstMatchWins(t *testing.T) {
	specific := errors.New("specific")
	mapper := xrpc.NewMapper("test",
		xrpc.Sentinel(specific, http.StatusNotFound, "Specific", "specific"),
		xrpc.Match(func(error) bool { return true }, http.StatusBadRequest, "Broad", "broad"),
	)

	if got, _ := mapper.Resolve(specific); got.Code != "Specific" {
		t.Errorf("code = %q, want Specific: an earlier rule must win", got.Code)
	}
	if got, _ := mapper.Resolve(errors.New("other")); got.Code != "Broad" {
		t.Errorf("code = %q, want Broad", got.Code)
	}
}

func TestWithDerivesWithoutMutatingParent(t *testing.T) {
	parent := newTestMapper()
	derived := parent.With(
		xrpc.Sentinel(errDomainNotFound, http.StatusNotFound, "Narrower", "narrower"),
	)

	if got, _ := derived.Resolve(errDomainNotFound); got.Code != "Narrower" {
		t.Errorf("derived code = %q, want Narrower", got.Code)
	}
	if got, _ := parent.Resolve(errDomainNotFound); got.Code != "ThingNotFound" {
		t.Errorf("parent code = %q, want ThingNotFound: With must not mutate the parent", got.Code)
	}

	// Two siblings must not share a backing array.
	siblingA := parent.With(xrpc.Sentinel(errDomainNotFound, http.StatusNotFound, "A", "a"))
	siblingB := parent.With(xrpc.Sentinel(errDomainNotFound, http.StatusNotFound, "B", "b"))
	if got, _ := siblingA.Resolve(errDomainNotFound); got.Code != "A" {
		t.Errorf("siblingA code = %q, want A", got.Code)
	}
	if got, _ := siblingB.Resolve(errDomainNotFound); got.Code != "B" {
		t.Errorf("siblingB code = %q, want B", got.Code)
	}
}

// A derived mapper's extra rules must not be able to displace the
// re-authentication check.
func TestWithCannotDisplaceReauth(t *testing.T) {
	mapper := newTestMapper().With(
		xrpc.Match(func(error) bool { return true }, http.StatusTeapot, "Greedy", "greedy"),
	)

	got, _ := mapper.Resolve(fmt.Errorf("x: %w", pds.ErrUnauthorized))
	if got.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 even behind a catch-all rule", got.Status)
	}
}

func TestWithReauthAndWithFallback(t *testing.T) {
	mapper := newTestMapper().
		WithReauth("AuthExpired", "Please re-authenticate.").
		WithFallback(http.StatusUnauthorized, "SessionError", "Sign in again.")

	got, _ := mapper.Resolve(fmt.Errorf("x: %w", pds.ErrUnauthorized))
	if got.Code != "AuthExpired" || got.Status != http.StatusUnauthorized {
		t.Errorf("reauth = %d/%q, want 401/AuthExpired", got.Status, got.Code)
	}

	got, matched := mapper.Resolve(errors.New("unclassifiable"))
	if got.Code != "SessionError" || got.Status != http.StatusUnauthorized {
		t.Errorf("fallback = %d/%q, want 401/SessionError", got.Status, got.Code)
	}
	if matched {
		t.Error("a fallback answer must still report matched=false so the caller logs it")
	}
}

func TestAsReadsMessageOffTypedErrorNotTheChain(t *testing.T) {
	mapper := xrpc.NewMapper("test")
	wrapped := fmt.Errorf("internal detail nobody should see: %w",
		coreerrors.NewValidationError("handle", "is required"))

	got, _ := mapper.Resolve(wrapped)
	if got.Message != "handle: is required" {
		t.Errorf("message = %q, want just the typed error's text", got.Message)
	}
}

func TestWriteProducesXRPCJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestMapper().Write(rec, errDomainNotAuthorized)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var body xrpc.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Error != "NotAuthorized" {
		t.Errorf("error = %q, want NotAuthorized", body.Error)
	}
	if body.Message != "You may not do that" {
		t.Errorf("message = %q", body.Message)
	}
}

// A nil error means the handler took its error path without an error. Answering
// 500 is wrong, but a silent return would leave the client with an empty 200.
func TestWriteNilErrorStillTerminatesTheRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestMapper().Write(rec, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("expected a body")
	}
}
