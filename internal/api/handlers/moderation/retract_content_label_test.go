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
	retractHandlerPath = "/xrpc/social.coves.moderation.retractContentLabel"
	retractHandlerBody = `{"actionId":"previous-label","reviewedSubject":{"uri":"` + labelHandlerURI + `","cid":"` + mutationSubjectCID + `"},"expectedVersion":"v1","idempotencyKey":"retract-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `"}`
)

func retractHandlerResult() *moderation.MutationResult {
	result := labelHandlerResult()
	result.State.Version = "v2"
	result.State.LocalLabels = nil
	result.State.Moderation.ContentLabels = nil
	result.Action.Action = moderation.ActionRetractLabel
	result.Action.ReversesActionID = "previous-label"
	return result
}

func TestRetractContentLabelHandlerPassesContextActorAndExactRequest(t *testing.T) {
	for _, test := range []struct {
		name, body string
		reviewed   *moderation.StrongRef
		reason     string
		note       string
	}{
		{"reviewed subject and note", retractHandlerBody, &moderation.StrongRef{URI: labelHandlerURI, CID: mutationSubjectCID}, mutationReason, mutationNote},
		{"optional fields omitted", `{"actionId":"previous-label","expectedVersion":"v1","idempotencyKey":"retract-key"}`, nil, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: retractHandlerResult()}
			response := httptest.NewRecorder()
			NewRetractContentLabelHandler(fake).HandleRetractContentLabel(response,
				mutationRequest(http.MethodPost, retractHandlerPath, test.body, "application/json"))
			require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
			require.NotEmpty(t, response.Body.Bytes(), "successful retractContentLabel must serialize a mutation result")
			assert.Equal(t, []string{mutationActorDID}, fake.actors)
			assert.Equal(t, []moderation.RetractContentLabelRequest{{
				ActionID: "previous-label", ReviewedSubject: test.reviewed, ExpectedVersion: "v1",
				IdempotencyKey: "retract-key", Reason: test.reason, PrivateNote: test.note,
			}}, fake.retractCalls)
			require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assertMutationLexicon(t, response)
			assert.Equal(t, moderation.OutcomeApplied, body["outcome"])
			state, ok := body["state"].(map[string]any)
			require.True(t, ok)
			assert.NotContains(t, state, "localLabels")
			assert.Equal(t, map[string]any{"state": moderation.ModerationStateClear}, state["moderation"])
			adminAction, ok := body["action"].(map[string]any)
			require.True(t, ok)
			action, ok := adminAction["action"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, moderation.ActionRetractLabel, action["action"])
			assert.Equal(t, moderation.LabelNSFW, action["labelValue"])
			assert.Equal(t, map[string]any{"serviceDid": mutationInstanceDID, "actionId": "previous-label"}, action["reverses"])
		})
	}
}

func TestRetractContentLabelHandlerRejectsInvalidHTTPAndMissingActor(t *testing.T) {
	for _, test := range []struct {
		name, method, body, contentType string
		actor                           bool
		status                          int
		code                            string
	}{
		{"GET", http.MethodGet, "", "", true, http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"missing content type", http.MethodPost, retractHandlerBody, "", true, http.StatusBadRequest, "InvalidRequest"},
		{"text/plain", http.MethodPost, retractHandlerBody, "text/plain", true, http.StatusBadRequest, "InvalidRequest"},
		{"malformed JSON", http.MethodPost, `{"actionId":`, "application/json", true, http.StatusBadRequest, "InvalidRequest"},
		{"no actor", http.MethodPost, retractHandlerBody, "application/json", false, http.StatusUnauthorized, "AuthRequired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: retractHandlerResult()}
			var request *http.Request
			if test.actor {
				request = mutationRequest(test.method, retractHandlerPath, test.body, test.contentType)
			} else {
				request = httptest.NewRequest(test.method, retractHandlerPath, strings.NewReader(test.body))
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			NewRetractContentLabelHandler(fake).HandleRetractContentLabel(response, request)
			require.Equalf(t, test.status, response.Code, "response: %s", response.Body.String())
			assertMutationError(t, response, test.status, test.code)
			assert.Empty(t, fake.retractCalls)
		})
	}
}

func TestRetractContentLabelHandlerMapsServiceErrors(t *testing.T) {
	for _, test := range mutationErrors {
		if test.err == moderation.ErrInvalidSubject || test.err == moderation.ErrSubjectNotFound {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{err: fmt.Errorf("mutation failed: %w", test.err)}
			response := httptest.NewRecorder()
			NewRetractContentLabelHandler(fake).HandleRetractContentLabel(response,
				mutationRequest(http.MethodPost, retractHandlerPath, retractHandlerBody, "application/json"))
			require.Equalf(t, test.status, response.Code, "response: %s", response.Body.String())
			assertMutationError(t, response, test.status, test.code)
			assert.Len(t, fake.retractCalls, 1)
		})
	}
}
