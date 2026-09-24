package moderation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"Coves/internal/core/moderation"
	"Coves/internal/validation"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type subjectStateServiceFake struct {
	state    *moderation.SubjectState
	err      error
	subjects []string
}

func (service *subjectStateServiceFake) GetSubjectState(_ context.Context, subject string) (*moderation.SubjectState, error) {
	service.subjects = append(service.subjects, subject)
	return service.state, service.err
}

func requestGetSubjectState(service *subjectStateServiceFake, subject string, includeSubject bool) *httptest.ResponseRecorder {
	target := "/xrpc/social.coves.moderation.getSubjectState"
	if includeSubject {
		target += "?" + url.Values{"subject": {subject}}.Encode()
	}
	response := httptest.NewRecorder()
	NewGetSubjectStateHandler(service).HandleGetSubjectState(response, httptest.NewRequest(http.MethodGet, target, nil))
	return response
}

func assertSubjectStateError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assert.Equal(t, status, response.Code)
	if assert.NotEmpty(t, response.Body.Bytes(), "XRPC error must include a JSON body") {
		var body map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		assert.Equal(t, code, body["error"])
	}
}

func TestGetSubjectStateHandlerMissingSubject(t *testing.T) {
	service := &subjectStateServiceFake{}
	response := requestGetSubjectState(service, "", false)
	assertSubjectStateError(t, response, http.StatusBadRequest, "InvalidSubject")
	assert.Empty(t, service.subjects, "missing subject must be rejected before calling the service")
}

func TestGetSubjectStateHandlerServiceErrors(t *testing.T) {
	const subject = "at://did:plc:subject/social.coves.community.postv2/3kabc"
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid subject", err: fmt.Errorf("subject: %w", moderation.ErrInvalidSubject), status: http.StatusBadRequest, code: "InvalidSubject"},
		{name: "unavailable", err: fmt.Errorf("reading state: %w", moderation.ErrModerationUnavailable), status: http.StatusServiceUnavailable, code: "ModerationUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &subjectStateServiceFake{err: test.err}
			response := requestGetSubjectState(service, subject, true)
			assertSubjectStateError(t, response, test.status, test.code)
			assert.Equal(t, []string{subject}, service.subjects)
		})
	}
}

// TestGetSubjectStateHandlerLogsServerFailures swaps the process-global slog
// default, so it must not run in parallel.
func TestGetSubjectStateHandlerLogsServerFailures(t *testing.T) {
	const subject = "at://did:plc:subject/social.coves.community.postv2/3kabc"
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "unavailable",
			err:    fmt.Errorf("%w: %w", moderation.ErrModerationUnavailable, errors.New("pg: connection refused")),
			status: http.StatusServiceUnavailable, code: "ModerationUnavailable",
		},
		{
			name:   "unexpected",
			err:    fmt.Errorf("reading state: %w", errors.New("scan: column count mismatch")),
			status: http.StatusInternalServerError, code: "InternalServerError",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logged bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			response := requestGetSubjectState(&subjectStateServiceFake{err: test.err}, subject, true)

			assertSubjectStateError(t, response, test.status, test.code)
			assert.Contains(t, logged.String(), test.err.Error(), "the underlying error must reach the server log")
			assert.NotContains(t, response.Body.String(), test.err.Error(), "the underlying error must not reach the response body")
		})
	}
}

func TestGetSubjectStateHandlerExactLexiconOutput(t *testing.T) {
	const (
		subject = "at://did:plc:subject/social.coves.community.postv2/3kabc"
		cid     = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	)
	for _, test := range []struct {
		name        string
		recordState moderation.RecordState
		current     *moderation.StrongRef
		wantCurrent bool
	}{
		{name: "present", recordState: moderation.RecordStatePresent, current: &moderation.StrongRef{URI: subject, CID: cid}, wantCurrent: true},
		{name: "deleted", recordState: moderation.RecordStateDeleted},
		{name: "unavailable", recordState: moderation.RecordStateUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := lexicon.NewBaseCatalog()
			require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
			state := &moderation.SubjectState{
				Subject: subject, Version: "v0", Moderation: moderation.ModerationView{State: "clear"},
				RecordState: test.recordState, CurrentSubject: test.current,
			}
			service := &subjectStateServiceFake{state: state}
			response := requestGetSubjectState(service, subject, true)

			assert.Equal(t, []string{subject}, service.subjects, "handler must pass the subject through unchanged")
			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
			require.NotEmpty(t, response.Body.Bytes(), "successful query must serialize the subject state")

			var got map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
			wantState := map[string]any{
				"subject": subject, "version": "v0", "moderation": map[string]any{"state": "clear"},
				"recordState": string(test.recordState),
			}
			if test.wantCurrent {
				wantState["currentSubject"] = map[string]any{"uri": subject, "cid": cid}
			}
			assert.Equal(t, map[string]any{"state": wantState}, got, "extra fields and null optional fields are forbidden")

			decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
			require.NoError(t, err)
			assert.NoError(t, validation.ValidateData(catalog, decoded, "social.coves.moderation.getSubjectState#output", 0))
		})
	}
}

func TestGetSubjectStateHandlerPassesExactQuerySubject(t *testing.T) {
	const subject = "at://did:plc:verbatim/social.coves.community.comment/3kquery"
	service := &subjectStateServiceFake{}
	_ = requestGetSubjectState(service, subject, true)
	assert.Equal(t, []string{subject}, service.subjects)
}
