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

// The production comment service must validate comment records against the
// lexicon, from schemas compiled into the binary: the build runs from an empty
// working directory with no schema files beside it.
func TestBuildApplicationCommentServiceCarriesLexiconValidator(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	db := sql.OpenDB(&unreachableDatabaseConnector{})
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	t.Chdir(t.TempDir())
	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	commentService, ok := app.commentService.(interface {
		LexiconValidator() *validation.LexiconValidator
	})
	require.True(t, ok, "comment service %T does not expose its lexicon validator", app.commentService)
	assert.NotNil(t, commentService.LexiconValidator())
}
