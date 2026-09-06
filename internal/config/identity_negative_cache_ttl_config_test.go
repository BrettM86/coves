package config

import (
	"strings"
	"testing"
	"time"
)

func TestIdentityNegativeCacheTTLConfig(t *testing.T) {
	t.Run("defaults to ninety seconds when unset", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}
		if cfg.Identity.NegativeCacheTTL != 90*time.Second {
			t.Errorf("Identity.NegativeCacheTTL = %v, want %v; the invariant needs an explicit value rather than zero",
				cfg.Identity.NegativeCacheTTL, 90*time.Second)
		}
	})

	t.Run("parses an explicit duration", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("IDENTITY_NEGATIVE_CACHE_TTL", "30s")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}
		if cfg.Identity.NegativeCacheTTL != 30*time.Second {
			t.Errorf("Identity.NegativeCacheTTL = %v, want %v", cfg.Identity.NegativeCacheTTL, 30*time.Second)
		}
	})

	for _, value := range []string{"0s", "-1s"} {
		t.Run("rejects "+value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("IDENTITY_NEGATIVE_CACHE_TTL", value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() returned nil error for IDENTITY_NEGATIVE_CACHE_TTL=%s, want a validation error", value)
			}
			if !strings.Contains(err.Error(), "IDENTITY_NEGATIVE_CACHE_TTL") {
				t.Errorf("Load() error = %q, want it to mention IDENTITY_NEGATIVE_CACHE_TTL", err.Error())
			}
		})
	}

	for _, tc := range []struct {
		name             string
		negativeCacheTTL string
		redriveInterval  string
	}{
		{name: "negative TTL exceeds redrive interval", negativeCacheTTL: "90s", redriveInterval: "60s"},
		{name: "negative TTL equals redrive interval", negativeCacheTTL: "90s", redriveInterval: "90s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("IDENTITY_NEGATIVE_CACHE_TTL", tc.negativeCacheTTL)
			t.Setenv("REDRIVE_INTERVAL", tc.redriveInterval)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() returned nil error for IDENTITY_NEGATIVE_CACHE_TTL=%s and REDRIVE_INTERVAL=%s; "+
					"a redrive served from negative cache burns one of only three attempts without touching the network",
					tc.negativeCacheTTL, tc.redriveInterval)
			}
			for _, variable := range []string{"IDENTITY_NEGATIVE_CACHE_TTL", "REDRIVE_INTERVAL"} {
				if !strings.Contains(err.Error(), variable) {
					t.Errorf("Load() error = %q, want it to mention both IDENTITY_NEGATIVE_CACHE_TTL and REDRIVE_INTERVAL", err.Error())
				}
			}
		})
	}

	t.Run("accepts a negative TTL below the redrive interval", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("IDENTITY_NEGATIVE_CACHE_TTL", "90s")
		t.Setenv("REDRIVE_INTERVAL", "5m")

		if _, err := Load(); err != nil {
			t.Fatalf("Load() returned error for a negative TTL below the redrive interval: %v", err)
		}
	})
}
