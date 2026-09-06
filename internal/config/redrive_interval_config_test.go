package config

import (
	"strings"
	"testing"
	"time"
)

// TestRedriveIntervalConfig pins the REDRIVE_INTERVAL env var to
// cfg.Jetstream.RedriveInterval.
//
// Note the contrast with ACCEPTANCE_QUEUE_INTERVAL: there, a zero value is a
// legitimate way to disable the loop. Here it is not. The redriver is
// mandatory — there is no "off" — and time.NewTicker(0) panics, so a zero
// duration must be rejected at config-load time rather than at ticker
// construction time deep inside a running consumer.
func TestRedriveIntervalConfig(t *testing.T) {
	t.Run("defaults to five minutes when unset", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}

		if cfg.Jetstream.RedriveInterval != 5*time.Minute {
			t.Errorf("RedriveInterval = %v, want %v", cfg.Jetstream.RedriveInterval, 5*time.Minute)
		}
	})

	t.Run("parses an explicit duration", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		// The loader requires REDRIVE_INTERVAL to exceed IDENTITY_NEGATIVE_CACHE_TTL
		// (default 90s); a 5s interval is only valid with a shorter negative TTL.
		t.Setenv("IDENTITY_NEGATIVE_CACHE_TTL", "1s")
		t.Setenv("REDRIVE_INTERVAL", "5s")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}

		if cfg.Jetstream.RedriveInterval != 5*time.Second {
			t.Errorf("RedriveInterval = %v, want %v", cfg.Jetstream.RedriveInterval, 5*time.Second)
		}
	})

	t.Run("rejects a zero interval", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("REDRIVE_INTERVAL", "0s")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() returned nil error for REDRIVE_INTERVAL=0s, want a validation error")
		}

		if !strings.Contains(err.Error(), "REDRIVE_INTERVAL") {
			t.Errorf("Load() error = %q, want it to mention REDRIVE_INTERVAL", err.Error())
		}
	})
}
