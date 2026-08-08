package post

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/posts"
)

// Deleting a post can fail with any PDS error. This package mapped none of
// them, so an expired session answered 500 and clients never learned to
// re-authenticate.
//
// The errors here are the ones raised against the CALLER's session. Posts live
// in the community's repo, so a failure of the community's own service
// credentials must not land in this set — see
// TestCommunityCredentialFailureIsNot401.
func TestDeleteMapsPDSErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "expired token",
			err:        fmt.Errorf("DeleteRecord: %w: expired", pds.ErrUnauthorized),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name: "session could not be resumed",
			err: fmt.Errorf("failed to create PDS client: %w",
				fmt.Errorf("resume: %w: revoked", pds.ErrSessionExpired)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name:       "missing scope",
			err:        fmt.Errorf("DeleteRecord: %w", pds.ErrForbidden),
			wantStatus: http.StatusForbidden,
			wantCode:   "PermissionDenied",
		},
		{
			name:       "pds rate limit",
			err:        fmt.Errorf("DeleteRecord: %w", pds.ErrRateLimited),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "RateLimitExceeded",
		},
		{
			// Delete keeps its own more specific codes.
			name:       "missing post",
			err:        fmt.Errorf("service: %w", posts.ErrNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   "PostNotFound",
		},
		{
			name:       "not the author",
			err:        fmt.Errorf("service: %w", posts.ErrNotAuthorized),
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleDeleteError(rec, tt.err)
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// TestCommunityCredentialCannotTriggerReauth guards the one place reauth-first
// could misfire. Posts live in the community's repo, so a delete authenticates
// with the community's service token; if a rejection of THAT credential reached
// the mapper carrying pds.ErrUnauthorized, the boundary would read it as the
// caller's session being dead and answer 401 — telling a user with a healthy
// session to sign in over a server-side problem, and hiding the outage from 5xx.
//
// posts.DeletePost strips the sentinel before returning, so the error arrives
// unclassified. This asserts the boundary behaviour that depends on it.
func TestCommunityCredentialCannotTriggerReauth(t *testing.T) {
	// Shaped as posts.communityCredentialFailure builds it: %v, not %w.
	err := fmt.Errorf("community PDS credentials rejected during delete post for %s: %v",
		"did:plc:community", fmt.Errorf("DeleteRecord: %w: bad token", pds.ErrUnauthorized))

	rec := httptest.NewRecorder()
	handleDeleteError(rec, err)

	body := assertXRPCError(t, rec, http.StatusInternalServerError, "InternalServerError")
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a community credential failure must never tell the caller to re-authenticate")
	}
	if strings.Contains(body.Message, "did:plc:community") {
		t.Errorf("internal detail leaked: %q", body.Message)
	}
}

// The post error codes are a client contract; this pins them. Note that the
// wrapped forms below newly reach these codes at all: the switch this replaced
// compared with ==, so a wrapped sentinel fell through to 500.
func TestPostErrorCodes(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{posts.ErrCommunityNotFound, http.StatusNotFound, "CommunityNotFound"},
		{posts.ErrNotAuthorized, http.StatusForbidden, "NotAuthorized"},
		{posts.ErrBanned, http.StatusForbidden, "Banned"},
		{posts.ErrNotFound, http.StatusNotFound, "NotFound"},
		{posts.ErrRateLimitExceeded, http.StatusTooManyRequests, "RateLimitExceeded"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, fmt.Errorf("service: %w", tt.err))
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// The other domain whose errors reach this mapper. An aggregator's refusal
// comes out of posts.CreatePost wrapped in its own sentence — `fmt.Errorf(
// "aggregator not authorized: %w", err)` (service.go step 5) — and the rules
// that catch it are predicates over the aggregators package's sentinels rather
// than over anything in posts.
//
// Both codes are load-bearing for a machine client: 403 says stop asking, 429
// says ask later. A mapper that knew only the posts package would answer 500 to
// each, and a well-behaved aggregator would retry a permanent refusal forever.
func TestAggregatorErrorCodes(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{aggregators.ErrNotAuthorized, http.StatusForbidden, "NotAuthorized"},
		{aggregators.ErrRateLimitExceeded, http.StatusTooManyRequests, "RateLimitExceeded"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, fmt.Errorf("aggregator not authorized: %w", tt.err))
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// A submission refused as a repeat is a 409, and it must not be confused with
// anything else.
//
// Two client behaviours depend on the distinction. A 409 says "your post
// already exists, stop retrying and go look for it", which is exactly what a
// client whose response was lost needs to hear; a 429 says "wait", and a client
// told to wait would resend the same content on a timer forever. And the code
// must be its own — folding it into the generic AlreadyExists that
// coreerrors.ConflictError produces would leave a client unable to tell a
// refused submission from a record the indexer already holds.
func TestDuplicateSubmissionIsItsOwnConflict(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("createPost: %w", posts.ErrDuplicateSubmission))

	body := assertXRPCError(t, rec, http.StatusConflict, "DuplicateSubmission")
	if strings.Contains(body.Message, "createPost") {
		t.Errorf("wrapper context leaked into the client message: %q", body.Message)
	}
}

// posts.ErrCommunityNotFound must keep beating the generic not-found rule that
// also matches it.
func TestCommunityNotFoundBeatsGenericNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("service: %w", posts.ErrCommunityNotFound))
	assertXRPCError(t, rec, http.StatusNotFound, "CommunityNotFound")
}

// Typed errors carry their own client-facing text; wrapper context added by the
// service on the way up must not ride along into the response.
func TestTypedPostErrorsUseTheirOwnMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("createPost: %w",
		posts.NewContentRuleViolation("requireText", "posts must have text")))
	body := assertXRPCError(t, rec, http.StatusBadRequest, "ContentRuleViolation")
	if strings.Contains(body.Message, "createPost") {
		t.Errorf("wrapper context leaked into the client message: %q", body.Message)
	}

	rec = httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("createPost: %w", posts.NewValidationError("uri", "is malformed")))
	body = assertXRPCError(t, rec, http.StatusBadRequest, "InvalidRequest")
	if body.Message != "uri: is malformed" {
		t.Errorf("message = %q, want just the typed error's text", body.Message)
	}
}

// The sentinel rules replaced err.Error() echoes with fixed strings, so an
// internal wrapper can no longer reach the client.
func TestSentinelMessagesAreFixed(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("repo query against 10.0.0.4 failed: %w", posts.ErrNotFound))

	body := assertXRPCError(t, rec, http.StatusNotFound, "NotFound")
	if body.Message != "Post not found" {
		t.Errorf("message = %q, want the fixed string", body.Message)
	}
	if strings.Contains(body.Message, "10.0.0.4") {
		t.Errorf("internal detail leaked: %q", body.Message)
	}
}

func assertXRPCError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) xrpc.Error {
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
	return body
}
