//go:build integration

package routes_test

import (
	"net/http"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderationRouteRemoveCommunityPath  = "/xrpc/social.coves.moderation.removeCommunity"
	moderationRouteRestoreCommunityPath = "/xrpc/social.coves.moderation.restoreCommunity"
)

func TestModerationRemoveCommunityRouteWiring(t *testing.T) {
	db := testkit.DB(t)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			Communities: postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		},
	)
	userName := testkit.UniqueIDWithPrefix(t, "removeroute")
	userDID := fixtures.DID(userName)
	fixtures.User(t, db, userName+".test", userDID)
	communityName := testkit.UniqueIDWithPrefix(t, "removeroutecomm")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)

	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "removerouteadmin"))
	const token = "moderation-remove-community-route-session"
	admin := moderationRouteServer(t, service, token, adminDID, []string{adminDID})
	nonAdmin := moderationRouteServer(t, service, token, adminDID, nil)
	body := func(subject, key string) map[string]any {
		return map[string]any{"subject": subject, "expectedVersion": "v0", "idempotencyKey": key, "reason": moderationRouteReason}
	}

	for _, test := range []struct {
		name, server, path, subject, key string
		status                           int
		code                             string
	}{
		{name: "signed-in non-admin cannot remove", server: "nonadmin", path: moderationRouteRemoveCommunityPath, subject: communityDID, key: "remove-nonadmin", status: http.StatusForbidden, code: "Forbidden"},
		{name: "signed-in non-admin cannot restore", server: "nonadmin", path: moderationRouteRestoreCommunityPath, subject: communityDID, key: "restore-nonadmin", status: http.StatusForbidden, code: "Forbidden"},
		{name: "a user DID is not a community", server: "admin", path: moderationRouteRemoveCommunityPath, subject: userDID, key: "remove-user", status: http.StatusBadRequest, code: "InvalidSubject"},
		{name: "a malformed subject is an invalid request", server: "admin", path: moderationRouteRemoveCommunityPath, subject: "not-a-did", key: "remove-malformed", status: http.StatusBadRequest, code: "InvalidRequest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := admin
			if test.server == "nonadmin" {
				server = nonAdmin
			}
			response := moderationAcceptanceRequest(t, server.Client(), http.MethodPost, server.URL+test.path, token, body(test.subject, test.key))
			requireXRPCError(t, response, test.status, test.code)
			var actions, decisions, keys int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions`).Scan(&actions))
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_decisions`).Scan(&decisions))
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE key = $1`, test.key).Scan(&keys))
			assert.Equal(t, []int{0, 0, 0}, []int{actions, decisions, keys}, "a refused community mutation writes no action, decision or idempotency key")
		})
	}

	t.Run("admin removes an indexed community", func(t *testing.T) {
		removed := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, admin.Client(), http.MethodPost, admin.URL+moderationRouteRemoveCommunityPath, token, body(communityDID, "remove-community")))
		assert.Equal(t, "applied", removed["outcome"])
		state := moderationAcceptanceObject(t, removed["state"])
		assert.Equal(t, "v1", state["version"])
		action := moderationAcceptanceObject(t, moderationAcceptanceObject(t, removed["action"])["action"])
		assert.Equal(t, "remove", action["action"])
		var actions int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1 AND subject_collection IS NULL`, "at://"+communityDID).Scan(&actions))
		assert.Equal(t, 1, actions)
	})
}
