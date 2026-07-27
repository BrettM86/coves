package comments

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/comments"
)

// Commenting write-forwards to the user's repo, so an expired session used to
// answer 500 here with no signal for the client to re-authenticate.
func TestExpiredSessionAnswers401(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "pds rejected the token",
			err: fmt.Errorf("%w: %w", comments.ErrNotAuthorized,
				fmt.Errorf("CreateRecord: %w: expired", pds.ErrUnauthorized)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name: "session could not be resumed",
			err: fmt.Errorf("failed to create PDS client: %w",
				fmt.Errorf("resume: %w", pds.ErrSessionExpired)),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AuthRequired",
		},
		{
			name: "missing scope stays 403",
			err: fmt.Errorf("%w: %w", comments.ErrNotAuthorized,
				fmt.Errorf("CreateRecord: %w", pds.ErrForbidden)),
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
		},
		{
			name:       "appview refusal stays 403",
			err:        comments.ErrNotAuthorized,
			wantStatus: http.StatusForbidden,
			wantCode:   "NotAuthorized",
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

// A concurrent edit loses the optimistic-locking race on PutRecord and the
// service reports ErrConcurrentModification. The old switch dropped that case
// as "dead code" it was not, so every lost race answered 500.
func TestConcurrentModificationIs409(t *testing.T) {
	rec := httptest.NewRecorder()
	handleServiceError(rec, fmt.Errorf("%w: %w", comments.ErrConcurrentModification,
		fmt.Errorf("PutRecord: %w", pds.ErrConflict)))
	assertXRPCError(t, rec, http.StatusConflict, "ConcurrentModification")
}

// The comment error codes are a client contract; this pins them.
func TestCommentErrorCodesUnchanged(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{comments.ErrCommentNotFound, http.StatusNotFound, "CommentNotFound"},
		{comments.ErrParentNotFound, http.StatusNotFound, "ParentNotFound"},
		{comments.ErrRootNotFound, http.StatusNotFound, "RootNotFound"},
		{comments.ErrInvalidReply, http.StatusBadRequest, "InvalidReply"},
		{comments.ErrContentTooLong, http.StatusBadRequest, "ContentTooLong"},
		{comments.ErrContentEmpty, http.StatusBadRequest, "ContentEmpty"},
		{comments.ErrInvalidFacets, http.StatusBadRequest, "InvalidFacets"},
		{comments.ErrInvalidCursor, http.StatusBadRequest, "InvalidRequest"},
		{comments.ErrBanned, http.StatusForbidden, "Banned"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode+"/"+tt.err.Error(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleServiceError(rec, fmt.Errorf("service: %w", tt.err))
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
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
