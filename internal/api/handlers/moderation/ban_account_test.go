package moderation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	banAccountPath       = "/xrpc/social.coves.moderation.banAccount"
	bannedAccountDID     = "did:plc:bannedaccount"
	bannedAccountSubject = "at://did:plc:bannedaccount"
	banAccountBody       = `{"subject":"` + bannedAccountDID + `","expectedVersion":"v0","idempotencyKey":"ban-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `","expiresAt":"2030-01-01T00:00:00.123Z"}`
)

// accountBanExpiry is the ban expiry the account fixtures serve: 2030-01-01T00:00:00.123Z.
func accountBanExpiry() *time.Time {
	expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 123_000_000, time.UTC)
	return &expiresAt
}

// accountBanResult is an applied account ban on bannedAccountSubject expiring at expiresAt.
func accountBanResult(expiresAt *time.Time) *moderation.MutationResult {
	return &moderation.MutationResult{
		Outcome: moderation.OutcomeApplied,
		State: moderation.SubjectState{
			Subject: bannedAccountSubject, Version: "v1", RecordState: moderation.RecordStateUnavailable,
			Moderation:   moderation.ModerationView{State: moderation.ModerationStateRemoved},
			LocalRemoval: &moderation.ActionRef{ServiceDID: mutationInstanceDID, ActionID: "action-ban", ExpiresAt: expiresAt},
		},
		Action: &moderation.Action{
			ID: "action-ban", ActorDID: mutationActorDID, AuthorityDID: mutationInstanceDID,
			ScopeKind: moderation.ScopeInstance, SubjectURI: bannedAccountSubject,
			Action: moderation.ActionRemove, Origin: moderation.OriginLocal, Reason: mutationReason,
			CreatedAt: time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC), ExpiresAt: expiresAt,
		},
	}
}

func decodeJSONObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoErrorf(t, json.Unmarshal(response.Body.Bytes(), &body), "response body: %s", response.Body.String())
	return body
}

func jsonObjectField(t *testing.T, object map[string]any, name string) map[string]any {
	t.Helper()
	field, ok := object[name].(map[string]any)
	require.Truef(t, ok, "%q must be a JSON object in %v", name, object)
	return field
}

func TestBanAccountHandlerPassesActorAndMapsRequest(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          string
		note          string
		wantExpiresAt string
	}{
		{name: "UTC expiresAt with milliseconds", body: banAccountBody, note: mutationNote, wantExpiresAt: "2030-01-01T00:00:00.123Z"},
		{
			name: "offset expiresAt is the same instant",
			body: strings.Replace(banAccountBody, "2030-01-01T00:00:00.123Z", "2030-01-01T09:00:00.123+09:00", 1),
			note: mutationNote, wantExpiresAt: "2030-01-01T00:00:00.123Z",
		},
		{
			name: "expiresAt and private note omitted",
			body: `{"subject":"` + bannedAccountDID + `","expectedVersion":"v0","idempotencyKey":"ban-key","reason":"` + mutationReason + `"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var served *time.Time
			if test.wantExpiresAt != "" {
				served = accountBanExpiry()
			}
			fake := &mutationServiceFake{result: accountBanResult(served)}
			response := httptest.NewRecorder()
			NewBanAccountHandler(fake).HandleBanAccount(response,
				mutationRequest(http.MethodPost, banAccountPath, test.body, "application/json"))

			assert.Equal(t, []string{mutationActorDID}, fake.actors)
			require.Len(t, fake.banCalls, 1, "the request must reach the service once")
			call := fake.banCalls[0]
			assert.Equal(t, bannedAccountDID, call.Subject)
			assert.Equal(t, "v0", call.ExpectedVersion)
			assert.Equal(t, "ban-key", call.IdempotencyKey)
			assert.Equal(t, mutationReason, call.Reason)
			assert.Equal(t, test.note, call.PrivateNote)
			if test.wantExpiresAt == "" {
				assert.Nil(t, call.ExpiresAt, "an omitted expiresAt is a permanent ban")
			} else {
				require.NotNil(t, call.ExpiresAt)
				assert.Equal(t, test.wantExpiresAt, call.ExpiresAt.UTC().Format(time.RFC3339Nano))
			}

			require.Equal(t, http.StatusOK, response.Code)
			body := decodeJSONObject(t, response)
			assert.Equal(t, "applied", body["outcome"])
			localRemoval := jsonObjectField(t, jsonObjectField(t, body, "state"), "localRemoval")
			action := jsonObjectField(t, jsonObjectField(t, body, "action"), "action")
			if test.wantExpiresAt == "" {
				assert.NotContains(t, localRemoval, "expiresAt")
				assert.NotContains(t, action, "expiresAt")
			} else {
				assert.Equal(t, "2030-01-01T00:00:00.123Z", localRemoval["expiresAt"])
				assert.Equal(t, "2030-01-01T00:00:00.123Z", action["expiresAt"])
			}
		})
	}
}

func TestBanAccountHandlerRejectsBeforeCallingService(t *testing.T) {
	for _, test := range []struct {
		name, method, body, contentType string
		signedIn                        bool
		status                          int
		code                            string
	}{
		{
			name: "malformed expiresAt", method: http.MethodPost,
			body:        strings.Replace(banAccountBody, "2030-01-01T00:00:00.123Z", "tomorrow", 1),
			contentType: "application/json", signedIn: true, status: http.StatusBadRequest, code: "InvalidRequest",
		},
		{
			name: "malformed JSON", method: http.MethodPost, body: `{"subject":`, contentType: "application/json",
			signedIn: true, status: http.StatusBadRequest, code: "InvalidRequest",
		},
		{
			name: "text/plain", method: http.MethodPost, body: banAccountBody, contentType: "text/plain",
			signedIn: true, status: http.StatusBadRequest, code: "InvalidRequest",
		},
		{name: "GET", method: http.MethodGet, signedIn: true, status: http.StatusMethodNotAllowed, code: "MethodNotAllowed"},
		{
			name: "signed out", method: http.MethodPost, body: banAccountBody, contentType: "application/json",
			status: http.StatusUnauthorized, code: "AuthRequired",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: accountBanResult(accountBanExpiry())}
			request := mutationRequest(test.method, banAccountPath, test.body, test.contentType)
			if !test.signedIn {
				request = httptest.NewRequest(test.method, banAccountPath, strings.NewReader(test.body))
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			NewBanAccountHandler(fake).HandleBanAccount(response, request)
			assertMutationError(t, response, test.status, test.code)
			assert.Empty(t, fake.banCalls)
		})
	}
}

func TestBanAccountHandlerMapsWrappedServiceErrors(t *testing.T) {
	for _, test := range mutationErrors {
		t.Run(test.name, func(t *testing.T) {
			fake := &mutationServiceFake{err: fmt.Errorf("ban failed: %w", test.err)}
			response := httptest.NewRecorder()
			NewBanAccountHandler(fake).HandleBanAccount(response,
				mutationRequest(http.MethodPost, banAccountPath, banAccountBody, "application/json"))
			assertMutationError(t, response, test.status, test.code)
			assert.Len(t, fake.banCalls, 1)
		})
	}
}

func TestGetSubjectStateHandlerServesAccountBanExpiry(t *testing.T) {
	state := accountBanResult(accountBanExpiry()).State
	service := &subjectStateServiceFake{state: &state}
	response := requestGetSubjectState(service, bannedAccountSubject, true)
	require.Equal(t, http.StatusOK, response.Code)
	localRemoval := jsonObjectField(t, jsonObjectField(t, decodeJSONObject(t, response), "state"), "localRemoval")
	assert.Equal(t, map[string]any{
		"serviceDid": mutationInstanceDID, "actionId": "action-ban", "expiresAt": "2030-01-01T00:00:00.123Z",
	}, localRemoval)
}

func TestActionListHandlersServeAccountBanExpiry(t *testing.T) {
	action := *accountBanResult(accountBanExpiry()).Action
	action.SubjectAccess = moderation.SubjectAccessPublic
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			fake := &actionListServiceFake{
				publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{moderation.NewActionView(action)}},
				adminPage:  &moderation.AdminActionPage{Actions: []moderation.AdminActionView{moderation.NewAdminActionView(action)}},
			}
			response := requestActionList(fake, admin, "", "", true)
			require.Equal(t, http.StatusOK, response.Code)
			actions, ok := decodeJSONObject(t, response)["actions"].([]any)
			require.True(t, ok)
			require.Len(t, actions, 1)
			view, ok := actions[0].(map[string]any)
			require.True(t, ok)
			if admin {
				view = jsonObjectField(t, view, "action")
			}
			assert.Equal(t, "2030-01-01T00:00:00.123Z", view["expiresAt"])
			assert.Equal(t, map[string]any{"uri": bannedAccountSubject}, view["subject"])
		})
	}
}
