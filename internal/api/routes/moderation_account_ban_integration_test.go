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
	moderationRouteBanAccountPath   = "/xrpc/social.coves.moderation.banAccount"
	moderationRouteUnbanAccountPath = "/xrpc/social.coves.moderation.unbanAccount"
)

func TestModerationBanAccountRouteWiring(t *testing.T) {
	db := testkit.DB(t)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			Communities: postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		},
	)
	userName := testkit.UniqueIDWithPrefix(t, "banroute")
	userDID := fixtures.DID(userName)
	fixtures.User(t, db, userName+".test", userDID)
	communityName := testkit.UniqueIDWithPrefix(t, "banroutecomm")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)

	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "banrouteadmin"))
	const token = "moderation-ban-route-session"
	admin := moderationRouteServer(t, service, token, adminDID, []string{adminDID})
	nonAdmin := moderationRouteServer(t, service, token, adminDID, nil)
	banBody := func(subject, key string) map[string]any {
		return map[string]any{"subject": subject, "expectedVersion": "v0", "idempotencyKey": key, "reason": moderationRouteReason}
	}

	for _, test := range []struct {
		name, server, subject, key string
		status                     int
		code                       string
	}{
		{name: "signed-in non-admin is forbidden", server: "nonadmin", subject: userDID, key: "ban-nonadmin", status: http.StatusForbidden, code: "Forbidden"},
		{name: "indexed community DID is not an account subject", server: "admin", subject: communityDID, key: "ban-community", status: http.StatusBadRequest, code: "InvalidSubject"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := admin
			if test.server == "nonadmin" {
				server = nonAdmin
			}
			response := moderationAcceptanceRequest(t, server.Client(), http.MethodPost, server.URL+moderationRouteBanAccountPath, token, banBody(test.subject, test.key))
			requireXRPCError(t, response, test.status, test.code)
			var actions, decisions, keys int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, "at://"+test.subject).Scan(&actions))
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1`, "at://"+test.subject).Scan(&decisions))
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE key = $1`, test.key).Scan(&keys))
			assert.Equal(t, []int{0, 0, 0}, []int{actions, decisions, keys}, "a refused ban writes no action, decision or idempotency key")
		})
	}

	t.Run("admin bans a user account", func(t *testing.T) {
		banned := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, admin.Client(), http.MethodPost, admin.URL+moderationRouteBanAccountPath, token, banBody(userDID, "ban-user")))
		assert.Equal(t, "applied", banned["outcome"])
		state := moderationAcceptanceObject(t, banned["state"])
		assert.Equal(t, "v1", state["version"])
		action := moderationAcceptanceObject(t, moderationAcceptanceObject(t, banned["action"])["action"])
		assert.Equal(t, "remove", action["action"])
		var actions int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1 AND subject_collection IS NULL`, "at://"+userDID).Scan(&actions))
		assert.Equal(t, 1, actions)
	})

	t.Run("signed-in non-admin cannot unban", func(t *testing.T) {
		activeBan := func() (actions int, activeActionID string) {
			t.Helper()
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, "at://"+userDID).Scan(&actions))
			require.NoError(t, db.QueryRowContext(t.Context(),
				`SELECT active_action_id FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'removal' AND active`,
				"at://"+userDID).Scan(&activeActionID))
			return actions, activeActionID
		}
		actionsBefore, activeBefore := activeBan()

		const key = "unban-nonadmin"
		response := moderationAcceptanceRequest(t, nonAdmin.Client(), http.MethodPost, nonAdmin.URL+moderationRouteUnbanAccountPath, token,
			map[string]any{"subject": userDID, "expectedVersion": "v1", "idempotencyKey": key, "reason": moderationRouteReason})
		requireXRPCError(t, response, http.StatusForbidden, "Forbidden")

		actionsAfter, activeAfter := activeBan()
		assert.Equal(t, actionsBefore, actionsAfter, "a refused unban writes no action")
		assert.Equal(t, activeBefore, activeAfter, "the ban stays in force under the same action")
		var keys int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE key = $1`, key).Scan(&keys))
		assert.Zero(t, keys, "a refused unban writes no idempotency key")
	})
}
