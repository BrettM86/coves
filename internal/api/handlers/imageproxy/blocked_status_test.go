package imageproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/imageproxy"
)

// The external half of the ErrPDSBlocked contract.
//
// The sentinel exists so that in-process callers can tell "the guard refused
// this destination" from "the fetch failed" — see the fetcher's own guard tests.
// Outside the process it must be invisible. A stranger who names an internal
// address in a DID document has to get back exactly what they would get for a
// PDS that is simply unreachable, because the point of this behavior is
// removing the response-status oracle that currently maps 404 to "the port
// answered", 502 to "refused" and 504 to "filtered".
//
// A new status for blocked addresses would be the same oracle wearing a new
// number, and a strictly better one: it would say "this address is internal"
// rather than merely "something happened here".
//
// handleServiceError's `default` branch serves 500, so an ErrPDSBlocked that
// nobody adds to that switch does not fall back to something harmless — it
// becomes a THIRD distinguishable answer.

// blockedStatusRequest drives the handler once with a service that fails with
// the given error, and returns the recorder.
func blockedStatusRequest(t *testing.T, serviceErr error) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(
		&mockService{
			getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
				return nil, serviceErr
			},
		},
		resolverForPDS("https://pds.example.com"),
	)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)
	return w
}

// TestHandler_HandleImage_BlockedIsIndistinguishableFromAFailedFetch asserts the
// two responses are the same response — status and body both.
//
// Asserting 502 alone would not be enough. A body of "blocked: private address"
// under a 502 leaks the classification just as effectively as a distinct status
// would, to anyone reading the response rather than the status line.
func TestHandler_HandleImage_BlockedIsIndistinguishableFromAFailedFetch(t *testing.T) {
	t.Parallel()

	blocked := blockedStatusRequest(t, imageproxy.ErrPDSBlocked)
	failed := blockedStatusRequest(t, imageproxy.ErrPDSFetchFailed)

	require.Equalf(t, http.StatusBadGateway, blocked.Code,
		"a guard refusal was served as %d rather than 502. handleServiceError's default branch is 500, "+
			"so an ErrPDSBlocked that was never added to the switch does not fail safe — it becomes a "+
			"third distinguishable answer, and a stranger probing addresses through a DID document "+
			"reads it as 'this one is internal'. Body: %s", blocked.Code, blocked.Body.String())

	assert.Equalf(t, failed.Code, blocked.Code,
		"a blocked address answers %d while an ordinary fetch failure answers %d. The internal "+
			"distinction is deliberate and must not reach the wire: two statuses is the port-scan "+
			"oracle this behavior exists to remove", blocked.Code, failed.Code)

	assert.Equalf(t, failed.Body.String(), blocked.Body.String(),
		"the response bodies differ (%q vs %q). Same status with a different body is the same oracle, "+
			"read one line further down", blocked.Body.String(), failed.Body.String())

	assert.Equalf(t, "no-store", blocked.Header().Get("Cache-Control"),
		"a refusal must stay uncacheable like every other error on this route: it sits behind a CDN "+
			"whose success responses advertise a one-year immutable lifetime")
}
