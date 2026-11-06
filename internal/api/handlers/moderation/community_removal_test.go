package moderation

import (
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
	removeCommunityPath     = "/xrpc/social.coves.moderation.removeCommunity"
	restoreCommunityPath    = "/xrpc/social.coves.moderation.restoreCommunity"
	removedCommunitySubject = "at://did:plc:removedcommunity"
	removeCommunityBody     = `{"subject":"did:plc:removedcommunity","expectedVersion":"v0","idempotencyKey":"remove-community-key","reason":"social.coves.moderation.defs#reasonSpam","privateNote":"private-note-secret"}`
	restoreCommunityBody    = `{"subject":"did:plc:removedcommunity","expectedVersion":"v1","idempotencyKey":"restore-community-key","reason":"social.coves.moderation.defs#reasonSpam","privateNote":"private-note-secret"}`
)

// communityRemovalResult is an applied community removal (or, with
// moderation.ActionRestore, its reversal) on removedCommunitySubject.
func communityRemovalResult(actionKind string) *moderation.MutationResult {
	result := &moderation.MutationResult{
		Outcome: moderation.OutcomeApplied,
		State: moderation.SubjectState{
			Subject: removedCommunitySubject, Version: "v1", RecordState: moderation.RecordStateUnavailable,
			Moderation:   moderation.ModerationView{State: moderation.ModerationStateRemoved},
			LocalRemoval: &moderation.ActionRef{ServiceDID: mutationInstanceDID, ActionID: "action-community-removal"},
		},
		Action: &moderation.Action{
			ID: "action-community-removal", ActorDID: mutationActorDID, AuthorityDID: mutationInstanceDID,
			ScopeKind: moderation.ScopeInstance, SubjectURI: removedCommunitySubject,
			Action: actionKind, Origin: moderation.OriginLocal, Reason: mutationReason,
			CreatedAt: time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC),
		},
	}
	if actionKind == moderation.ActionRestore {
		result.State.Moderation.State = moderation.ModerationStateClear
		result.State.LocalRemoval = nil
		result.Action.ReversesActionID = "action-previous-removal"
	}
	return result
}

// communityRemovalEndpoint drives one of the two handlers against a fake.
type communityRemovalEndpoint struct {
	name   string
	path   string
	body   string
	action string
	serve  func(*mutationServiceFake, http.ResponseWriter, *http.Request)
	calls  func(*mutationServiceFake) int
}

func communityRemovalEndpoints() []communityRemovalEndpoint {
	return []communityRemovalEndpoint{
		{
			name: "removeCommunity", path: removeCommunityPath, body: removeCommunityBody, action: moderation.ActionRemove,
			serve: func(fake *mutationServiceFake, w http.ResponseWriter, r *http.Request) {
				NewRemoveCommunityHandler(fake).HandleRemoveCommunity(w, r)
			},
			calls: func(fake *mutationServiceFake) int { return len(fake.removeCommunityCalls) },
		},
		{
			name: "restoreCommunity", path: restoreCommunityPath, body: restoreCommunityBody, action: moderation.ActionRestore,
			serve: func(fake *mutationServiceFake, w http.ResponseWriter, r *http.Request) {
				NewRestoreCommunityHandler(fake).HandleRestoreCommunity(w, r)
			},
			calls: func(fake *mutationServiceFake) int { return len(fake.restoreCommunityCalls) },
		},
	}
}

func TestRemoveCommunityHandlerPassesActorAndMapsRequest(t *testing.T) {
	fake := &mutationServiceFake{result: communityRemovalResult(moderation.ActionRemove)}
	response := httptest.NewRecorder()
	NewRemoveCommunityHandler(fake).HandleRemoveCommunity(response,
		mutationRequest(http.MethodPost, removeCommunityPath, removeCommunityBody, "application/json"))

	assert.Equal(t, []string{"did:plc:moderationadmin"}, fake.actors)
	assert.Equal(t, []moderation.RemoveCommunityRequest{{
		Subject: "did:plc:removedcommunity", ExpectedVersion: "v0", IdempotencyKey: "remove-community-key",
		Reason: "social.coves.moderation.defs#reasonSpam", PrivateNote: "private-note-secret",
	}}, fake.removeCommunityCalls)
	require.Equal(t, http.StatusOK, response.Code)
	body := decodeJSONObject(t, response)
	assert.Equal(t, "applied", body["outcome"])
	assert.Equal(t, "at://did:plc:removedcommunity", jsonObjectField(t, body, "state")["subject"])
}

func TestRestoreCommunityHandlerPassesActorAndMapsRequest(t *testing.T) {
	fake := &mutationServiceFake{result: communityRemovalResult(moderation.ActionRestore)}
	response := httptest.NewRecorder()
	NewRestoreCommunityHandler(fake).HandleRestoreCommunity(response,
		mutationRequest(http.MethodPost, restoreCommunityPath, restoreCommunityBody, "application/json"))

	assert.Equal(t, []string{"did:plc:moderationadmin"}, fake.actors)
	assert.Equal(t, []moderation.RestoreCommunityRequest{{
		Subject: "did:plc:removedcommunity", ExpectedVersion: "v1", IdempotencyKey: "restore-community-key",
		Reason: "social.coves.moderation.defs#reasonSpam", PrivateNote: "private-note-secret",
	}}, fake.restoreCommunityCalls)
	require.Equal(t, http.StatusOK, response.Code)
	body := decodeJSONObject(t, response)
	assert.Equal(t, "applied", body["outcome"])
	assert.Equal(t, "at://did:plc:removedcommunity", jsonObjectField(t, body, "state")["subject"])
}

func TestCommunityRemovalHandlersRequireSignedInCaller(t *testing.T) {
	for _, endpoint := range communityRemovalEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			fake := &mutationServiceFake{result: communityRemovalResult(endpoint.action)}
			request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			endpoint.serve(fake, response, request)
			assertMutationError(t, response, http.StatusUnauthorized, "AuthRequired")
			assert.Zero(t, endpoint.calls(fake), "a signed-out caller must not reach the service")
		})
	}
}

func TestCommunityRemovalHandlersMapWrappedServiceErrors(t *testing.T) {
	for _, endpoint := range communityRemovalEndpoints() {
		for _, test := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{"invalid subject", moderation.ErrInvalidSubject, http.StatusBadRequest, "InvalidSubject"},
			{"invalid request", moderation.ErrInvalidRequest, http.StatusBadRequest, "InvalidRequest"},
		} {
			t.Run(endpoint.name+"/"+test.name, func(t *testing.T) {
				fake := &mutationServiceFake{err: fmt.Errorf("community mutation failed: %w", test.err)}
				response := httptest.NewRecorder()
				endpoint.serve(fake, response,
					mutationRequest(http.MethodPost, endpoint.path, endpoint.body, "application/json"))
				assertMutationError(t, response, test.status, test.code)
				assert.Equal(t, 1, endpoint.calls(fake), "the request must reach the service once")
			})
		}
	}
}
