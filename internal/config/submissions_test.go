package config

import (
	"strings"
	"testing"
	"time"
)

// The per-author submission quota of docs/PRD_AUTHOR_OWNED_POSTS.md §8 is
// configuration, and configuration that goes missing must stop the process.
//
// The failure this guards against is specific: an operator who never sets
// POST_SUBMISSIONS_MAX_PER_COMMUNITY gets a zero, a limit check written as
// `count >= limit` then refuses everything (or, written the other way, admits
// everything), and either way the behaviour is decided by an omission rather
// than by a decision. §8's quotas exist to absorb the fact that anyone can
// write unlimited records naming any community — so "unset" cannot be allowed
// to mean "unlimited", and validating at startup is the only place the answer
// is cheap.

func TestLoad_SubmissionQuotaHasWorkingDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Submissions.MaxPerAuthorPerCommunity <= 0 {
		t.Errorf("Submissions.MaxPerAuthorPerCommunity = %d, want a positive default; "+
			"a zero here is a quota decided by omission",
			cfg.Submissions.MaxPerAuthorPerCommunity)
	}
	if cfg.Submissions.Window <= 0 {
		t.Errorf("Submissions.Window = %s, want a positive rolling window", cfg.Submissions.Window)
	}
	if cfg.Submissions.DedupeWindow <= 0 {
		t.Errorf("Submissions.DedupeWindow = %s, want a positive dedupe window", cfg.Submissions.DedupeWindow)
	}
}

func TestLoad_SubmissionQuotaIsReadFromTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("POST_SUBMISSIONS_MAX_PER_COMMUNITY", "7")
	t.Setenv("POST_SUBMISSIONS_WINDOW", "30m")
	t.Setenv("POST_SUBMISSIONS_DEDUPE_WINDOW", "10m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Submissions.MaxPerAuthorPerCommunity != 7 {
		t.Errorf("MaxPerAuthorPerCommunity = %d, want 7", cfg.Submissions.MaxPerAuthorPerCommunity)
	}
	if cfg.Submissions.Window != 30*time.Minute {
		t.Errorf("Window = %s, want 30m", cfg.Submissions.Window)
	}
	if cfg.Submissions.DedupeWindow != 10*time.Minute {
		t.Errorf("DedupeWindow = %s, want 10m", cfg.Submissions.DedupeWindow)
	}
}

// A config assembled with the quota left at its zero value must not validate.
// This is the assertion that makes "unset means unlimited" unrepresentable
// rather than merely discouraged.
func TestValidate_RejectsAnUnsetSubmissionQuota(t *testing.T) {
	base := func() *Config {
		return &Config{
			IsDevEnv:     true,
			Database:     DatabaseConfig{URL: "postgres://u:p@db/coves", MaxOpenConns: 25, MaxIdleConns: 25},
			Server:       ServerConfig{Port: "8080", ReadHeaderTimeout: time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, ShutdownTimeout: 15 * time.Second},
			Instance:     InstanceConfig{DID: "did:web:coves.social", Domain: "coves.social"},
			CursorSecret: devCursorSecret,
			Jetstream:    JetstreamConfig{FeedsSpec: "self=ws://localhost:6008"}, // coves:allow-host-literal: a non-empty spec so Validate's unrelated JETSTREAM_FEEDS rule is satisfied; parsing lives elsewhere and nothing here dials it
			Submissions: SubmissionsConfig{
				MaxPerAuthorPerCommunity: 10,
				Window:                   time.Hour,
				DedupeWindow:             time.Hour,
			},
		}
	}

	// The control: the fully-specified config validates, so a failure below is
	// about the field that was cleared and not about the fixture.
	if err := base().Validate(); err != nil {
		t.Fatalf("the fully-specified config must validate; got: %v", err)
	}

	for _, tc := range []struct {
		name  string
		clear func(*Config)
		want  string
	}{
		{"no per-community limit", func(c *Config) { c.Submissions.MaxPerAuthorPerCommunity = 0 }, "POST_SUBMISSIONS_MAX_PER_COMMUNITY"},
		{"no window", func(c *Config) { c.Submissions.Window = 0 }, "POST_SUBMISSIONS_WINDOW"},
		{"no dedupe window", func(c *Config) { c.Submissions.DedupeWindow = 0 }, "POST_SUBMISSIONS_DEDUPE_WINDOW"},
		{"a negative limit", func(c *Config) { c.Submissions.MaxPerAuthorPerCommunity = -1 }, "POST_SUBMISSIONS_MAX_PER_COMMUNITY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.clear(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() accepted a submission quota that is not a quota; the process would start with abuse limits silently disabled")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %s so an operator can fix it; got:\n%s", tc.want, err.Error())
			}
		})
	}
}
