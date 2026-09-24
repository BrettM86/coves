package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadModerationIdempotencySettings(t *testing.T) {
	for _, test := range []struct {
		name      string
		retention string
		maxKeys   string
		wantTime  time.Duration
		wantKeys  int
	}{
		{name: "defaults", wantTime: 24 * time.Hour, wantKeys: 1000},
		{name: "configured limits", retention: "2h", maxKeys: "5", wantTime: 2 * time.Hour, wantKeys: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("MODERATION_IDEMPOTENCY_RETENTION", test.retention)
			t.Setenv("MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS", test.maxKeys)
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, test.wantTime, cfg.Moderation.IdempotencyRetention)
			assert.Equal(t, test.wantKeys, cfg.Moderation.MaxLiveIdempotencyKeys)
		})
	}
}

func TestLoadModerationIdempotencyRejectsInvalidSettings(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"MODERATION_IDEMPOTENCY_RETENTION", "0s"},
		{"MODERATION_IDEMPOTENCY_RETENTION", "-1h"},
		{"MODERATION_IDEMPOTENCY_RETENTION", "banana"},
		{"MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS", "0"},
		{"MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS", "-1"},
		{"MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS", "x"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(test.name, test.value)
			_, err := Load()
			require.Error(t, err)
			assert.ErrorContains(t, err, test.name)
		})
	}
}

func TestClearEnvForTestClearsModerationIdempotencySettings(t *testing.T) {
	for _, name := range []string{"MODERATION_IDEMPOTENCY_RETENTION", "MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS"} {
		t.Setenv(name, "invalid")
	}
	ClearEnvForTest(t)
	for _, name := range []string{"MODERATION_IDEMPOTENCY_RETENTION", "MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS"} {
		assert.Empty(t, os.Getenv(name), "%s must be in loadedEnvVars", name)
	}
	t.Setenv("IS_DEV_ENV", "true")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, cfg.Moderation.IdempotencyRetention)
	assert.Equal(t, 1000, cfg.Moderation.MaxLiveIdempotencyKeys)
}
