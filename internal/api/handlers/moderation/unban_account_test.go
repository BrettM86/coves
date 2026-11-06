package moderation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	unbanAccountPath = "/xrpc/social.coves.moderation.unbanAccount"
	unbanAccountBody = `{"subject":"` + bannedAccountDID + `","expectedVersion":"v1","idempotencyKey":"unban-key","reason":"` + mutationReason + `","privateNote":"` + mutationNote + `"}`
)

func TestUnbanAccountHandlerPassesActorAndMapsRequest(t *testing.T) {
	result := accountBanResult(nil)
	result.State.Moderation.State = moderation.ModerationStateClear
	result.State.LocalRemoval = nil
	result.Action.Action = moderation.ActionRestore
	result.Action.ReversesActionID = "action-previous-ban"
	fake := &mutationServiceFake{result: result}
	response := httptest.NewRecorder()
	NewUnbanAccountHandler(fake).HandleUnbanAccount(response,
		mutationRequest(http.MethodPost, unbanAccountPath, unbanAccountBody, "application/json"))

	assert.Equal(t, []string{mutationActorDID}, fake.actors)
	assert.Equal(t, []moderation.UnbanAccountRequest{{
		Subject: bannedAccountDID, ExpectedVersion: "v1", IdempotencyKey: "unban-key",
		Reason: mutationReason, PrivateNote: mutationNote,
	}}, fake.unbanCalls)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "applied", decodeJSONObject(t, response)["outcome"])
}

func TestUnbanAccountHandlerRequiresSignedInCaller(t *testing.T) {
	fake := &mutationServiceFake{}
	request := httptest.NewRequest(http.MethodPost, unbanAccountPath, strings.NewReader(unbanAccountBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewUnbanAccountHandler(fake).HandleUnbanAccount(response, request)
	assertMutationError(t, response, http.StatusUnauthorized, "AuthRequired")
	assert.Empty(t, fake.unbanCalls)
}
