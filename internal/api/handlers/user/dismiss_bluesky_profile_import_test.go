package user

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDismissBlueskyProfileImportHandler(t *testing.T) {
	tests := []struct {
		name         string
		dismissErr   error
		wantStatus   int
		wantEmptyRes bool
	}{
		{
			name:         "authenticated POST dismisses and returns an empty 200",
			wantStatus:   http.StatusOK,
			wantEmptyRes: true,
		},
		{
			name:       "dismisser error returns 500",
			dismissErr: errors.New("database unavailable"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dismisser := &recordingDismisser{err: tt.dismissErr}
			handler := NewDismissBlueskyProfileImportHandler(dismisser)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.dismissBlueskyProfileImport", nil)
			req = setTestOAuthSession(req, importTestDID, createTestOAuthSession(importTestDID))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, []string{importTestDID}, dismisser.dids, "the dismisser must be called once with the user's DID")
			assert.Equal(t, tt.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tt.wantEmptyRes {
				assert.Empty(t, rec.Body.String())
			}
		})
	}
}

// A client that goes away after asking must not lose the dismissal, and the
// write stays bounded because nobody is waiting on it.
func TestDismissBlueskyProfileImportHandler_SurvivesRequestCancel(t *testing.T) {
	dismisser := &recordingDismisser{}
	handler := NewDismissBlueskyProfileImportHandler(dismisser)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.dismissBlueskyProfileImport", nil)
	req = setTestOAuthSession(req, importTestDID, createTestOAuthSession(importTestDID))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	handler.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

	require.Equal(t, []string{importTestDID}, dismisser.dids)
	assert.Equal(t, []error{nil}, dismisser.ctxErrs, "the dismissal runs on a context the disconnect does not cancel")
	assertBoundedDeadline(t, dismisser.deadlines[0])
}
