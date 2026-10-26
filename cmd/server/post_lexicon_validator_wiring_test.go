package main

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/validation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production post service must validate records against the lexicon, and
// the server runs from a directory with no schema files beside it. Building the
// application from an empty working directory proves the validator comes from
// schemas compiled into the binary and reaches the post service.
func TestBuildApplicationPostServiceCarriesLexiconValidator(t *testing.T) {
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

	t.Chdir(t.TempDir())
	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	postService, ok := app.postService.(interface {
		LexiconValidator() *validation.LexiconValidator
	})
	require.True(t, ok, "post service %T does not expose its lexicon validator", app.postService)
	assert.NotNil(t, postService.LexiconValidator())
}
