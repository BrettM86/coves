package moderation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/core/moderation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	labelHandlerPath = "/xrpc/social.coves.moderation.labelContent"
	labelHandlerURI  = "at://did:plc:postauthor/social.coves.community.postv2/3kabc"
	labelHandlerBody = `{"subject":{"uri":"` + labelHandlerURI + `","cid":"` + mutationSubjectCID + `"},"labelValue":"nsfw","expectedVersion":"v0","idempotencyKey":"label-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `"}`
)

func labelHandlerResult() *moderation.MutationResult {
	result := mutationResult(moderation.ActionLabel, mutationNote)
	result.State.Subject = labelHandlerURI
	result.State.CurrentSubject.URI = labelHandlerURI
	result.State.Moderation = moderation.ModerationView{
		State: moderation.ModerationStateClear,
		ContentLabels: []moderation.ContentLabel{{
			Value:   moderation.LabelNSFW,
			Sources: []moderation.DecisionSource{{AuthorityDID: mutationInstanceDID, ScopeKind: moderation.ScopeInstance}},
		}},
	}
	result.State.LocalRemoval = nil
	result.State.LocalLabels = []moderation.LocalLabel{{
		Value: moderation.LabelNSFW, Action: moderation.ActionRef{ServiceDID: mutationInstanceDID, ActionID: result.Action.ID},
	}}
	result.Action.SubjectURI = labelHandlerURI
	result.Action.SubjectCollection = moderation.PostV2Collection
	result.Action.LabelValue = moderation.LabelNSFW
	return result
}

func requireLabelMutationResponse(t *testing.T, response *httptest.ResponseRecorder, actionKind string) map[string]any {
	t.Helper()
	require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assertMutationLexicon(t, response)
	assert.Equal(t, moderation.OutcomeApplied, body["outcome"])
	state, ok := body["state"].(map[string]any)
	require.True(t, ok, "state must be an object: %s", response.Body.String())
	view, ok := state["moderation"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"state": moderation.ModerationStateClear, "contentLabels": []any{map[string]any{
		"value":   moderation.LabelNSFW,
		"sources": []any{map[string]any{"authorityDid": mutationInstanceDID, "scope": map[string]any{"kind": moderation.ScopeInstance}}},
	}}}, view)
	labels, ok := state["localLabels"].([]any)
	require.True(t, ok)
	require.Len(t, labels, 1)
	label, ok := labels[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, moderation.LabelNSFW, label["value"])
	ref, ok := label["action"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"serviceDid": mutationInstanceDID, "actionId": "action-1"}, ref)
	adminAction, ok := body["action"].(map[string]any)
	require.True(t, ok)
	action, ok := adminAction["action"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, actionKind, action["action"])
	assert.Equal(t, moderation.LabelNSFW, action["labelValue"])
	return action
}

func TestLabelContentHandlerPassesContextActorAndExactRequest(t *testing.T) {
	fake := &mutationServiceFake{result: labelHandlerResult()}
	response := httptest.NewRecorder()
	NewLabelContentHandler(fake).HandleLabelContent(response,
		mutationRequest(http.MethodPost, labelHandlerPath, labelHandlerBody, "application/json"))
	require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
	require.NotEmpty(t, response.Body.Bytes(), "successful labelContent must serialize a mutation result")
	assert.Equal(t, []string{mutationActorDID}, fake.actors)
	assert.Equal(t, []moderation.LabelContentRequest{{
		Subject:    moderation.StrongRef{URI: labelHandlerURI, CID: mutationSubjectCID},
		LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "label-key",
		Reason: mutationReason, PrivateNote: mutationNote,
	}}, fake.labelCalls)
	_ = requireLabelMutationResponse(t, response, moderation.ActionLabel)
}

func TestLabelContentHandlerRejectsInvalidHTTPAndMissingActor(t *testing.T) {
	for _, test := range []struct {
		name, method, body, contentType string
		actor                           bool
		status                          int
		code                            string
	}{
		{"GET", http.MethodGet, "", "", true, http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"missing content type", http.MethodPost, labelHandlerBody, "", true, http.StatusBadRequest, "InvalidRequest"},
		{"text/plain", http.MethodPost, labelHandlerBody, "text/plain", true, http.StatusBadRequest, "InvalidRequest"},
		{"malformed JSON", http.MethodPost, `{"subject":`, "application/json", true, http.StatusBadRequest, "InvalidRequest"},
		{"no actor", http.MethodPost, labelHandlerBody, "application/json", false, http.StatusUnauthorized, "AuthRequired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: labelHandlerResult()}
			var request *http.Request
			if test.actor {
				request = mutationRequest(test.method, labelHandlerPath, test.body, test.contentType)
			} else {
				request = httptest.NewRequest(test.method, labelHandlerPath, strings.NewReader(test.body))
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			NewLabelContentHandler(fake).HandleLabelContent(response, request)
			require.Equalf(t, test.status, response.Code, "response: %s", response.Body.String())
			assertMutationError(t, response, test.status, test.code)
			assert.Empty(t, fake.labelCalls)
		})
	}
}

func TestLabelContentHandlerMapsServiceErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid request", moderation.ErrInvalidRequest, http.StatusBadRequest, "InvalidRequest"},
		{"invalid subject", moderation.ErrInvalidSubject, http.StatusBadRequest, "InvalidSubject"},
		{"subject not found", moderation.ErrSubjectNotFound, http.StatusBadRequest, "SubjectNotFound"},
		{"content changed", moderation.ErrContentChanged, http.StatusBadRequest, "ContentChanged"},
		{"state conflict", moderation.ErrStateConflict, http.StatusBadRequest, "StateConflict"},
		{"idempotency conflict", moderation.ErrIdempotencyConflict, http.StatusBadRequest, "IdempotencyConflict"},
		{"unsupported reason", moderation.ErrUnsupportedReason, http.StatusBadRequest, "UnsupportedReason"},
		{"unsupported label", moderation.ErrUnsupportedLabel, http.StatusBadRequest, "UnsupportedLabel"},
		{"unavailable", moderation.ErrModerationUnavailable, http.StatusServiceUnavailable, "ModerationUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{err: fmt.Errorf("mutation failed: %w", test.err)}
			response := httptest.NewRecorder()
			NewLabelContentHandler(fake).HandleLabelContent(response,
				mutationRequest(http.MethodPost, labelHandlerPath, labelHandlerBody, "application/json"))
			require.Equalf(t, test.status, response.Code, "response: %s", response.Body.String())
			assertMutationError(t, response, test.status, test.code)
			assert.Len(t, fake.labelCalls, 1)
		})
	}
}
