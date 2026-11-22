package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const classifierTestToken = "cf-workers-ai-token-SENTINEL-5b2e"

// classifierEnv blanks every classifier variable so the developer's shell
// cannot leak into a case, then applies the given values.
func classifierEnv(t *testing.T, values map[string]string) {
	t.Helper()
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	for _, name := range []string{
		"CLASSIFIER_ENABLED", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_WORKERS_AI_API_TOKEN",
		"CLASSIFIER_TEXT_MODEL_ID", "CLASSIFIER_IMAGE_MODEL_ID", "CLASSIFIER_DAILY_REQUEST_CAP",
		"CLASSIFIER_WORKER_INTERVAL", "CLASSIFIER_CONCURRENCY", "CLASSIFIER_TEXT_CHAR_LIMIT",
		"CLASSIFIER_ATTEMPT_LIMIT", "CLASSIFIER_RETRY_BASE_DELAY", "CLASSIFIER_PENDING_DEFERRAL",
		"CLASSIFIER_SCORE_ALERTS", "CLASSIFIER_SCORE_ALERT_ATTEMPT_LIMIT", "CLASSIFIER_SCORE_ALERT_RETRY_BASE_DELAY",
		"CLASSIFIER_SCORE_ALERT_SWEEP_INTERVAL",
	} {
		t.Setenv(name, "")
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
}

// TestLoadClassifierConfig pins the classifier settings: credentials are
// required only when enabled, the defaults, and token redaction (B5).
func TestLoadClassifierConfig(t *testing.T) {
	t.Run("enabled without token names the token key", func(t *testing.T) {
		classifierEnv(t, map[string]string{
			"CLASSIFIER_ENABLED":    "true",
			"CLOUDFLARE_ACCOUNT_ID": "acct-123",
		})
		_, err := Load()
		require.Error(t, err)
		assert.ErrorContains(t, err, "CLOUDFLARE_WORKERS_AI_API_TOKEN")
	})

	t.Run("enabled without account ID names the account key", func(t *testing.T) {
		classifierEnv(t, map[string]string{
			"CLASSIFIER_ENABLED":              "true",
			"CLOUDFLARE_WORKERS_AI_API_TOKEN": classifierTestToken,
		})
		_, err := Load()
		require.Error(t, err)
		assert.ErrorContains(t, err, "CLOUDFLARE_ACCOUNT_ID")
		assert.NotContains(t, err.Error(), classifierTestToken)
	})

	t.Run("disabled needs nothing", func(t *testing.T) {
		classifierEnv(t, nil)
		cfg, err := Load()
		require.NoError(t, err)
		assert.False(t, cfg.Classifier.Enabled)
	})

	t.Run("enabled with credentials loads the defaults", func(t *testing.T) {
		classifierEnv(t, map[string]string{
			"CLASSIFIER_ENABLED":              "true",
			"CLOUDFLARE_ACCOUNT_ID":           "acct-123",
			"CLOUDFLARE_WORKERS_AI_API_TOKEN": classifierTestToken,
		})
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, ClassifierConfig{
			Enabled:         true,
			AccountID:       "acct-123",
			APIToken:        classifierTestToken,
			TextModelID:     "@cf/cloudflare/clef-flash",
			ImageModelID:    "@cf/cloudflare/clef",
			DailyRequestCap: 5000,
			WorkerInterval:  30 * time.Second,
			Concurrency:     2,
			TextCharLimit:   8000,
			AttemptLimit:    5,
			RetryBaseDelay:  time.Minute,
			PendingDeferral: 10 * time.Minute,

			ScoreAlerts:              "sexual_minors:0.3",
			ScoreAlertAttemptLimit:   8,
			ScoreAlertRetryBaseDelay: time.Minute,
			ScoreAlertSweepInterval:  5 * time.Minute,
		}, cfg.Classifier)

		assert.NotContains(t, fmt.Sprintf("%v", cfg), classifierTestToken)
		assert.NotContains(t, fmt.Sprintf("%+v", cfg), classifierTestToken)
	})

	for _, key := range []string{
		"CLASSIFIER_DAILY_REQUEST_CAP",
		"CLASSIFIER_RETRY_BASE_DELAY",
		"CLASSIFIER_PENDING_DEFERRAL",
	} {
		t.Run("enabled with "+key+" set to 0 names the key", func(t *testing.T) {
			zero := "0"
			if key != "CLASSIFIER_DAILY_REQUEST_CAP" {
				zero = "0s"
			}
			classifierEnv(t, map[string]string{
				"CLASSIFIER_ENABLED":              "true",
				"CLOUDFLARE_ACCOUNT_ID":           "acct-123",
				"CLOUDFLARE_WORKERS_AI_API_TOKEN": classifierTestToken,
				key:                               zero,
			})
			_, err := Load()
			require.Error(t, err)
			assert.ErrorContains(t, err, key+" must be greater than 0")
			assert.NotContains(t, err.Error(), classifierTestToken)
		})
	}
}
