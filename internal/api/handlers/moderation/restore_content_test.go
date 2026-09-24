package moderation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/moderation"
	"Coves/internal/validation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	restorePath = "/xrpc/social.coves.moderation.restoreContent"
	restoreBody = `{"actionId":"action-previous","reviewedSubject":{"uri":"` + mutationSubjectURI + `","cid":"` + mutationSubjectCID + `"},"expectedVersion":"v0","idempotencyKey":"restore-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `"}`
)

func TestRestoreContentHandlerPassesContextActorAndExactlyMapsRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		reviewed *moderation.StrongRef
		note     string
	}{
		{"reviewed subject and private note", restoreBody, &moderation.StrongRef{URI: mutationSubjectURI, CID: mutationSubjectCID}, mutationNote},
		{"reviewed subject and private note omitted", `{"actionId":"action-previous","expectedVersion":"v0","idempotencyKey":"restore-key","reason":"` + mutationReason + `"}`, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := mutationResult(moderation.ActionRestore, test.note)
			fake := &mutationServiceFake{result: result}
			response := httptest.NewRecorder()
			NewRestoreContentHandler(fake).HandleRestoreContent(response,
				mutationRequest(http.MethodPost, restorePath, test.body, "application/json"))
			assert.Equal(t, []string{mutationActorDID}, fake.actors)
			assert.Equal(t, []moderation.RestoreContentRequest{{
				ActionID: "action-previous", ReviewedSubject: test.reviewed, ExpectedVersion: "v0",
				IdempotencyKey: "restore-key", Reason: mutationReason, PrivateNote: test.note,
			}}, fake.restoreCalls)
			assertMutationResponse(t, response, result)
		})
	}
}

func TestRestoreContentHandlerRejectsNonJSONInvalidJSONAndGET(t *testing.T) {
	for _, test := range []struct {
		name, method, body, contentType string
		status                          int
	}{
		{"text/plain", http.MethodPost, restoreBody, "text/plain", http.StatusBadRequest},
		{"form", http.MethodPost, restoreBody, "application/x-www-form-urlencoded", http.StatusBadRequest},
		{"missing content type", http.MethodPost, restoreBody, "", http.StatusBadRequest},
		{"malformed JSON", http.MethodPost, `{"actionId":`, "application/json", http.StatusBadRequest},
		{"GET", http.MethodGet, "", "", http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: mutationResult(moderation.ActionRestore, "")}
			response := httptest.NewRecorder()
			NewRestoreContentHandler(fake).HandleRestoreContent(response,
				mutationRequest(test.method, restorePath, test.body, test.contentType))
			assert.Equal(t, test.status, response.Code)
			if test.status == http.StatusBadRequest {
				assertMutationError(t, response, test.status, "InvalidRequest")
			}
			assert.Empty(t, fake.restoreCalls)
		})
	}
}

func TestRestoreContentHandlerMapsWrappedServiceErrors(t *testing.T) {
	for _, test := range mutationErrors {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{err: fmt.Errorf("mutation failed: %w", test.err)}
			response := httptest.NewRecorder()
			NewRestoreContentHandler(fake).HandleRestoreContent(response,
				mutationRequest(http.MethodPost, restorePath, restoreBody, "application/json"))
			assertMutationError(t, response, test.status, test.code)
			assert.Len(t, fake.restoreCalls, 1)
		})
	}
}

func TestRestoreContentHandlerOmitsCIDForUnavailableSubject(t *testing.T) {
	result := mutationResult(moderation.ActionRestore, "")
	result.State.RecordState = moderation.RecordStateUnavailable
	result.State.CurrentSubject = nil
	result.Action.ObservedCID = ""
	response := httptest.NewRecorder()
	NewRestoreContentHandler(&mutationServiceFake{result: result}).HandleRestoreContent(response,
		mutationRequest(http.MethodPost, restorePath, restoreBody, "application/json"))
	require.Equal(t, http.StatusOK, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	adminAction, ok := body["action"].(map[string]any)
	require.True(t, ok)
	action, ok := adminAction["action"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"uri": mutationSubjectURI}, action["subject"], "an unobserved subject has no CID to report")
	state, ok := body["state"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "unavailable", state["recordState"])
	assert.NotContains(t, state, "currentSubject")
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(catalog, decoded, "social.coves.moderation.defs#mutationResult", 0))
}
