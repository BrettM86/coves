package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"
	"Coves/internal/validation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mutationActorDID    = "did:plc:moderationadmin"
	mutationInstanceDID = "did:web:moderation.test"
	mutationSubjectURI  = "at://did:plc:commentauthor/social.coves.community.comment/3kabc"
	mutationSubjectCID  = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	mutationReason      = "social.coves.moderation.defs#reasonSpam"
	mutationNote        = "private-note-secret"
	removeBody          = `{"subject":{"uri":"` + mutationSubjectURI + `","cid":"` + mutationSubjectCID + `"},"expectedVersion":"v0","idempotencyKey":"remove-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `"}`
)

type mutationServiceFake struct {
	moderation.Service
	removeCalls  []moderation.RemoveContentRequest
	restoreCalls []moderation.RestoreContentRequest
	actors       []string
	result       *moderation.MutationResult
	err          error
}

func (fake *mutationServiceFake) RemoveContent(_ context.Context, actorDID string, request moderation.RemoveContentRequest) (*moderation.MutationResult, error) {
	fake.actors = append(fake.actors, actorDID)
	fake.removeCalls = append(fake.removeCalls, request)
	return fake.result, fake.err
}

func (fake *mutationServiceFake) RestoreContent(_ context.Context, actorDID string, request moderation.RestoreContentRequest) (*moderation.MutationResult, error) {
	fake.actors = append(fake.actors, actorDID)
	fake.restoreCalls = append(fake.restoreCalls, request)
	return fake.result, fake.err
}

func mutationRequest(method, path, body, contentType string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return request.WithContext(context.WithValue(request.Context(), middleware.UserDIDKey, mutationActorDID))
}

func mutationResult(actionKind string, privateNote string) *moderation.MutationResult {
	state := moderation.SubjectState{
		Subject: mutationSubjectURI, Version: "v1", RecordState: moderation.RecordStatePresent,
		CurrentSubject: &moderation.StrongRef{URI: mutationSubjectURI, CID: mutationSubjectCID},
	}
	action := &moderation.Action{
		ID: "action-1", ActorDID: mutationActorDID, AuthorityDID: mutationInstanceDID,
		ScopeKind: moderation.ScopeInstance, SubjectURI: mutationSubjectURI, ObservedCID: mutationSubjectCID,
		Action: actionKind, Origin: moderation.OriginLocal, Reason: mutationReason,
		PrivateNote: privateNote, CreatedAt: time.Date(2026, time.September, 24, 12, 30, 0, 0, time.UTC),
	}
	if actionKind == moderation.ActionRestore {
		state.Moderation.State = moderation.ModerationStateClear
		action.ReversesActionID = "action-previous"
	} else {
		state.Moderation.State = moderation.ModerationStateRemoved
		state.LocalRemoval = &moderation.ActionRef{ServiceDID: mutationInstanceDID, ActionID: action.ID}
	}
	return &moderation.MutationResult{Outcome: moderation.OutcomeApplied, State: state, Action: action}
}

func assertMutationResponse(t *testing.T, response *httptest.ResponseRecorder, result *moderation.MutationResult) {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	state := map[string]any{
		"subject": mutationSubjectURI, "version": result.State.Version,
		"moderation":  map[string]any{"state": result.State.Moderation.State},
		"recordState": "present", "currentSubject": map[string]any{"uri": mutationSubjectURI, "cid": mutationSubjectCID},
	}
	if result.State.LocalRemoval != nil {
		state["localRemoval"] = map[string]any{"serviceDid": mutationInstanceDID, "actionId": result.State.LocalRemoval.ActionID}
	}
	want := map[string]any{"outcome": result.Outcome, "state": state}
	if result.Action != nil {
		action := result.Action
		projection := map[string]any{
			"ref":    map[string]any{"serviceDid": action.AuthorityDID, "actionId": action.ID},
			"action": action.Action, "authorityDid": action.AuthorityDID,
			"scope": map[string]any{"kind": action.ScopeKind}, "createdAt": action.CreatedAt.Format(time.RFC3339),
			"origin": action.Origin, "subject": map[string]any{"uri": action.SubjectURI, "cid": action.ObservedCID},
			"reason": action.Reason, "actor": map[string]any{"did": action.ActorDID},
		}
		if action.ReversesActionID != "" {
			projection["reverses"] = map[string]any{"serviceDid": action.AuthorityDID, "actionId": action.ReversesActionID}
		}
		adminAction := map[string]any{"action": projection, "actorDid": action.ActorDID}
		if action.PrivateNote != "" {
			adminAction["privateNote"] = action.PrivateNote
		}
		want["action"] = adminAction
	}
	assert.Equal(t, want, body, "response must omit null optional fields and expose the exact action projection")
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(catalog, decoded, "social.coves.moderation.defs#mutationResult", 0))
}

func assertMutationError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assert.Equal(t, status, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, code, body["error"])
	assert.NotEmpty(t, body["message"])
	assert.Len(t, body, 2, "errors expose only error and message")
	assert.NotContains(t, response.Body.String(), mutationNote)
}

func TestRemoveContentHandlerPassesContextActorAndExactlyMapsRequest(t *testing.T) {
	result := mutationResult(moderation.ActionRemove, mutationNote)
	fake := &mutationServiceFake{result: result}
	response := httptest.NewRecorder()
	request := mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", removeBody, "application/json")
	NewRemoveContentHandler(fake).HandleRemoveContent(response, request)
	assert.Equal(t, []string{mutationActorDID}, fake.actors)
	assert.Equal(t, []moderation.RemoveContentRequest{{
		Subject:         moderation.StrongRef{URI: mutationSubjectURI, CID: mutationSubjectCID},
		ExpectedVersion: "v0", IdempotencyKey: "remove-key", Reason: mutationReason, PrivateNote: mutationNote,
	}}, fake.removeCalls)
	assertMutationResponse(t, response, result)
}

func TestRemoveContentHandlerOmitsActionForUnchanged(t *testing.T) {
	result := mutationResult(moderation.ActionRemove, "")
	result.Outcome = moderation.OutcomeUnchanged
	result.Action = nil
	fake := &mutationServiceFake{result: result}
	response := httptest.NewRecorder()
	NewRemoveContentHandler(fake).HandleRemoveContent(response,
		mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", removeBody, "application/json"))
	assertMutationResponse(t, response, result)
}

func TestRemoveContentHandlerRejectsNonJSONInvalidJSONAndGET(t *testing.T) {
	for _, test := range []struct {
		name, method, body, contentType string
		status                          int
	}{
		{"text/plain", http.MethodPost, removeBody, "text/plain", http.StatusBadRequest},
		{"form", http.MethodPost, removeBody, "application/x-www-form-urlencoded", http.StatusBadRequest},
		{"missing content type", http.MethodPost, removeBody, "", http.StatusBadRequest},
		{"malformed JSON", http.MethodPost, `{"subject":`, "application/json", http.StatusBadRequest},
		{"GET", http.MethodGet, "", "", http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: mutationResult(moderation.ActionRemove, "")}
			response := httptest.NewRecorder()
			NewRemoveContentHandler(fake).HandleRemoveContent(response,
				mutationRequest(test.method, "/xrpc/social.coves.moderation.removeContent", test.body, test.contentType))
			assert.Equal(t, test.status, response.Code)
			if test.status == http.StatusBadRequest {
				assertMutationError(t, response, test.status, "InvalidRequest")
			}
			assert.Empty(t, fake.removeCalls)
		})
	}
}

var mutationErrors = []struct {
	name   string
	err    error
	status int
	code   string
}{
	{"invalid request", moderation.ErrInvalidRequest, http.StatusBadRequest, "InvalidRequest"},
	{"invalid subject", moderation.ErrInvalidSubject, http.StatusBadRequest, "InvalidSubject"},
	{"subject not found", moderation.ErrSubjectNotFound, http.StatusBadRequest, "SubjectNotFound"},
	{"decision not found", moderation.ErrDecisionNotFound, http.StatusBadRequest, "DecisionNotFound"},
	{"invalid decision", moderation.ErrInvalidDecision, http.StatusBadRequest, "InvalidDecision"},
	{"content changed", moderation.ErrContentChanged, http.StatusBadRequest, "ContentChanged"},
	{"state conflict", moderation.ErrStateConflict, http.StatusBadRequest, "StateConflict"},
	{"idempotency conflict", moderation.ErrIdempotencyConflict, http.StatusBadRequest, "IdempotencyConflict"},
	{"unsupported reason", moderation.ErrUnsupportedReason, http.StatusBadRequest, "UnsupportedReason"},
	{"unavailable", moderation.ErrModerationUnavailable, http.StatusServiceUnavailable, "ModerationUnavailable"},
	{"unknown", errors.New("unexpected storage failure"), http.StatusInternalServerError, "InternalServerError"},
}

func TestRemoveContentHandlerMapsWrappedServiceErrors(t *testing.T) {
	for _, test := range mutationErrors {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{err: fmt.Errorf("mutation failed: %w", test.err)}
			response := httptest.NewRecorder()
			NewRemoveContentHandler(fake).HandleRemoveContent(response,
				mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", removeBody, "application/json"))
			assertMutationError(t, response, test.status, test.code)
			assert.Len(t, fake.removeCalls, 1)
		})
	}
}

func mutationErrorMessage(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	message, ok := body["message"].(string)
	require.Truef(t, ok, "message must be a string: %s", response.Body.String())
	return message
}

func TestRemoveContentHandlerRejectsUnsupportedCollectionBeforeStoreAccess(t *testing.T) {
	service := moderation.NewService(nil, nil, moderation.Config{
		InstanceDID: mutationInstanceDID, IdempotencyRetention: time.Hour, MaxLiveIdempotencyKeys: 1,
	})
	unsupportedBody := strings.Replace(removeBody, moderation.CommentCollection, "app.bsky.feed.post", 1)
	response := httptest.NewRecorder()
	NewRemoveContentHandler(service).HandleRemoveContent(response,
		mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", unsupportedBody, "application/json"))
	assertMutationError(t, response, http.StatusBadRequest, "InvalidSubject")
	assert.Equal(t, "invalid subject: unsupported subject collection", mutationErrorMessage(t, response))
}

func TestMutationHandlersWriteRuleDetailButKeepUnavailableGeneric(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"live key cap", fmt.Errorf("%w: live idempotency key limit %d reached", moderation.ErrInvalidRequest, 3),
			http.StatusBadRequest, "InvalidRequest", "invalid request: live idempotency key limit 3 reached"},
		{"bare rule sentinel", moderation.ErrStateConflict, http.StatusBadRequest, "StateConflict", "state conflict"},
		{"unavailable with storage detail", fmt.Errorf("%w: %w", moderation.ErrModerationUnavailable, errors.New("dial tcp 10.0.0.9:5432: connection refused")),
			http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable"},
		{"unknown with detail", errors.New("dial tcp 10.0.0.9:5432: connection refused"),
			http.StatusInternalServerError, "InternalServerError", "An internal error occurred"},
	} {
		t.Run(test.name, func(t *testing.T) {
			removeResponse := httptest.NewRecorder()
			NewRemoveContentHandler(&mutationServiceFake{err: test.err}).HandleRemoveContent(removeResponse,
				mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", removeBody, "application/json"))
			assertMutationError(t, removeResponse, test.status, test.code)
			assert.Equal(t, test.message, mutationErrorMessage(t, removeResponse))
			restoreResponse := httptest.NewRecorder()
			NewRestoreContentHandler(&mutationServiceFake{err: test.err}).HandleRestoreContent(restoreResponse,
				mutationRequest(http.MethodPost, restorePath, restoreBody, "application/json"))
			assertMutationError(t, restoreResponse, test.status, test.code)
			assert.Equal(t, test.message, mutationErrorMessage(t, restoreResponse))
		})
	}
}

func assertHiddenMutationAction(t *testing.T, response *httptest.ResponseRecorder, result *moderation.MutationResult) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	adminAction, ok := body["action"].(map[string]any)
	require.True(t, ok, "mutation must include an admin action: %s", response.Body.String())
	publicAction, ok := adminAction["action"].(map[string]any)
	require.True(t, ok, "mutation must include the public action projection: %s", response.Body.String())
	assert.NotContains(t, publicAction, "subject", "hidden subjects must not be exposed in the public projection")
	assert.Equal(t, map[string]any{"uri": result.Action.SubjectURI, "cid": result.Action.ObservedCID}, adminAction["privateSubject"])
	assert.Equal(t, result.Action.ActorDID, adminAction["actorDid"])
	assert.Equal(t, result.Action.PrivateNote, adminAction["privateNote"])
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(catalog, decoded, "social.coves.moderation.defs#mutationResult", 0))
	return publicAction
}

func TestRemoveContentHandlerKeepsHiddenSubjectOnlyInPrivateProjection(t *testing.T) {
	for _, reason := range []string{
		"social.coves.moderation.defs#reasonIllegalContent",
		"social.coves.moderation.defs#reasonDoxing",
	} {
		t.Run(reason, func(t *testing.T) {
			result := mutationResult(moderation.ActionRemove, mutationNote)
			result.Action.Reason = reason
			fake := &mutationServiceFake{result: result}
			response := httptest.NewRecorder()
			NewRemoveContentHandler(fake).HandleRemoveContent(response,
				mutationRequest(http.MethodPost, "/xrpc/social.coves.moderation.removeContent", removeBody, "application/json"))
			require.Len(t, fake.removeCalls, 1)
			publicAction := assertHiddenMutationAction(t, response, result)
			assert.Equal(t, reason, publicAction["reason"])
		})
	}
}
