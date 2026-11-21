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

// The server's XRPC route table serves describeFeedGenerator from the
// application's builtin registry: the instance DID and the four production
// builtins in registration order, without touching the database.
func TestRegisterXRPCRoutesDescribeFeedGeneratorServesBuiltinRegistry(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Instance.DID)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	router := chi.NewRouter()
	registerXRPCRoutes(router, app)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.describeFeedGenerator", nil))
	require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())

	var body struct {
		DID   string `json:"did"`
		Feeds []struct {
			URI string `json:"uri"`
		} `json:"feeds"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "body: %s", recorder.Body.String())
	assert.Equal(t, cfg.Instance.DID, body.DID)

	prefix := "at://" + cfg.Instance.DID + "/social.coves.feed.generator/"
	gotURIs := make([]string, 0, len(body.Feeds))
	for _, feed := range body.Feeds {
		gotURIs = append(gotURIs, feed.URI)
	}
	assert.Equal(t, []string{
		prefix + "hot", prefix + "new", prefix + "top", prefix + "discover-hot",
		prefix + "comments-hot", prefix + "comments-top", prefix + "comments-new",
	}, gotURIs)
	assert.Zero(t, connector.attempts.Load(), "describeFeedGenerator must not connect to the database")
}
