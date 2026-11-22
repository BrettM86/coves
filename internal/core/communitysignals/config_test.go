package communitysignals_test

import (
	"os"
	"testing"
	"time"

	"Coves/internal/core/communitysignals"
)

var configKeys = []string{
	"COMMUNITY_SIGNAL_ALERTS_ENABLED",
	"COMMUNITY_SIGNAL_SWEEP_INTERVAL",
	"COMMUNITY_SIGNAL_ATTEMPT_LIMIT",
	"COMMUNITY_SIGNAL_RETRY_BASE_DELAY",
	"COMMUNITY_SIGNAL_DIGEST_MIN_POSTS",
	"COMMUNITY_SIGNAL_DIGEST_RISE_POINTS",
}

// clearConfigEnv unsets every key; t.Setenv registers the restore first.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range configKeys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	clearConfigEnv(t)

	got, err := communitysignals.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := communitysignals.Config{
		AlertsEnabled:       false,
		SweepInterval:       5 * time.Minute,
		AttemptLimit:        8,
		RetryBaseDelay:      time.Minute,
		DigestMinPosts:      10,
		DigestRiseThreshold: 20,
	}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("COMMUNITY_SIGNAL_ALERTS_ENABLED", "true")
	t.Setenv("COMMUNITY_SIGNAL_SWEEP_INTERVAL", "10m")
	t.Setenv("COMMUNITY_SIGNAL_ATTEMPT_LIMIT", "3")
	t.Setenv("COMMUNITY_SIGNAL_RETRY_BASE_DELAY", "30s")
	t.Setenv("COMMUNITY_SIGNAL_DIGEST_MIN_POSTS", "25")
	t.Setenv("COMMUNITY_SIGNAL_DIGEST_RISE_POINTS", "12.5")

	got, err := communitysignals.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := communitysignals.Config{
		AlertsEnabled:       true,
		SweepInterval:       10 * time.Minute,
		AttemptLimit:        3,
		RetryBaseDelay:      30 * time.Second,
		DigestMinPosts:      25,
		DigestRiseThreshold: 12.5,
	}
	if got != want {
		t.Errorf("overrides = %+v, want %+v", got, want)
	}
}

func TestConfigFromEnvInvalid(t *testing.T) {
	cases := []struct {
		name, key, value string
	}{
		{name: "non-numeric attempt limit", key: "COMMUNITY_SIGNAL_ATTEMPT_LIMIT", value: "many"},
		{name: "zero sweep interval", key: "COMMUNITY_SIGNAL_SWEEP_INTERVAL", value: "0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)

			if _, err := communitysignals.ConfigFromEnv(); err == nil {
				t.Errorf("ConfigFromEnv with %s=%q returned nil error", tc.key, tc.value)
			}
		})
	}
}
