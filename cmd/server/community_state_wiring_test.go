package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterXRPCRoutesServesGetCommunityState proves the application
// buildApplication wires mounts social.coves.moderation.getCommunityState: a
// signed-out request reaches the route's authentication gate and gets
// AuthRequired, not a 404.
func TestRegisterXRPCRoutesServesGetCommunityState(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	router := chi.NewRouter()
	registerXRPCRoutes(router, app)

	request := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.moderation.getCommunityState?subject=did:plc:communitystatewiring", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code, "body: %s", recorder.Body.String())
	var errorBody struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &errorBody), "body: %s", recorder.Body.String())
	assert.Equal(t, "AuthRequired", errorBody.Error)
	assert.Zero(t, connector.attempts.Load(), "a signed-out request must not reach the database")
}
