package config

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// validSealSecret is a well-formed OAUTH_SEAL_SECRET: base64 of exactly 32
// bytes, which is what oauth.NewOAuthClient requires.
var validSealSecret = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 32))

// prodEnv is the minimum set of variables a production configuration must
// provide. Tests start from this and mutate one thing at a time so each case
// asserts about exactly one rule.
func prodEnv(t *testing.T) {
	t.Helper()
	t.Setenv("IS_DEV_ENV", "false")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/coves?sslmode=disable")
	t.Setenv("OAUTH_SEAL_SECRET", validSealSecret)
	t.Setenv("CURSOR_SECRET", "a-real-cursor-secret-long-enough")
	t.Setenv("JETSTREAM_FEEDS", "bsky=wss://jetstream2.us-east.bsky.network")
	t.Setenv("INSTANCE_DID", "did:web:coves.social")
	t.Setenv("APPVIEW_PUBLIC_URL", "https://coves.social")
	t.Setenv("PDS_URL", "https://pds.coves.social")
}

// clearEnv delegates to the exported helper so there is exactly one list of
// the variables Load reads.
func clearEnv(t *testing.T) {
	t.Helper()
	ClearEnvForTest(t)
}

func TestLoad_DevDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if !cfg.IsDevEnv {
		t.Error("IsDevEnv should be true")
	}
	if cfg.Server.Port != "8080" {
		t.Errorf("Server.Port = %q, want %q", cfg.Server.Port, "8080")
	}
	if cfg.PDS.URL != "http://localhost:3001" {
		t.Errorf("PDS.URL = %q, want the local dev PDS", cfg.PDS.URL)
	}
	if cfg.Jetstream.FeedsSpec != "self=ws://localhost:6008" {
		t.Errorf("Jetstream.FeedsSpec = %q, want the local dev feed", cfg.Jetstream.FeedsSpec)
	}
	if cfg.CursorSecret != devCursorSecret {
		t.Errorf("CursorSecret = %q, want the dev placeholder", cfg.CursorSecret)
	}
	if !cfg.OAuth.SealSecretGenerated {
		t.Error("OAuth.SealSecretGenerated should be true when OAUTH_SEAL_SECRET is unset in dev")
	}
	if cfg.OAuth.SealSecret == "" {
		t.Error("OAuth.SealSecret should be generated in dev, not left empty")
	}
	if cfg.Instance.Domain != "coves.social" {
		t.Errorf("Instance.Domain = %q, want it derived from the default did:web", cfg.Instance.Domain)
	}
}

// The whole point of item 1: none of these may be zero, because a zero timeout
// in net/http means "wait forever".
func TestLoad_ServerTimeoutsAreNeverZero(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	timeouts := map[string]time.Duration{
		"ReadHeaderTimeout": cfg.Server.ReadHeaderTimeout,
		"ReadTimeout":       cfg.Server.ReadTimeout,
		"WriteTimeout":      cfg.Server.WriteTimeout,
		"IdleTimeout":       cfg.Server.IdleTimeout,
		"ShutdownTimeout":   cfg.Server.ShutdownTimeout,
	}
	for name, value := range timeouts {
		if value <= 0 {
			t.Errorf("Server.%s = %v, must be positive: zero means no timeout at all", name, value)
		}
	}

	// The image proxy may spend its full 30s fetch timeout on a remote PDS
	// before writing anything, so a shorter WriteTimeout would truncate
	// legitimate responses.
	if cfg.Server.WriteTimeout <= 30*time.Second {
		t.Errorf("Server.WriteTimeout = %v, must exceed the image proxy's 30s fetch timeout",
			cfg.Server.WriteTimeout)
	}
	if cfg.Server.ReadHeaderTimeout > cfg.Server.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) should not exceed ReadTimeout (%v)",
			cfg.Server.ReadHeaderTimeout, cfg.Server.ReadTimeout)
	}
}

func TestLoad_DatabasePoolDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Database.MaxOpenConns <= 0 {
		t.Errorf("MaxOpenConns = %d, must be bounded", cfg.Database.MaxOpenConns)
	}
	// PostgreSQL ships with max_connections = 100. Leave headroom for psql
	// and the cmd/ maintenance tools.
	if cfg.Database.MaxOpenConns > 50 {
		t.Errorf("MaxOpenConns = %d, too close to PostgreSQL's default max_connections of 100",
			cfg.Database.MaxOpenConns)
	}
	// database/sql defaults idle to 2, which makes the pool reconnect under
	// exactly the concurrency it exists to absorb.
	if cfg.Database.MaxIdleConns != cfg.Database.MaxOpenConns {
		t.Errorf("MaxIdleConns = %d, want it to match MaxOpenConns (%d) to avoid connection churn",
			cfg.Database.MaxIdleConns, cfg.Database.MaxOpenConns)
	}
	if cfg.Database.ConnMaxLifetime <= 0 {
		t.Error("ConnMaxLifetime must be set so the pool recovers from a PostgreSQL restart")
	}
	if cfg.Database.StatementTimeout <= 0 {
		t.Error("StatementTimeout must be set so a runaway query cannot pin a connection")
	}
}

// The single most important property in this package: an environment that
// says nothing about IS_DEV_ENV must be treated as production. If the default
// ever flipped, every production guard below would quietly stop applying while
// the rest of the suite stayed green.
func TestLoad_UnsetIsDevEnvMeansProduction(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/coves")
	// IS_DEV_ENV deliberately left blank.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with IS_DEV_ENV unset; an unset value must mean production")
	}
	for _, want := range []string{"OAUTH_SEAL_SECRET", "CURSOR_SECRET", "JETSTREAM_FEEDS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s; got:\n%s", want, err.Error())
		}
	}
}

// Validate's own guards must be reachable, not merely implied by the defaults.
// Every one of these values parses cleanly, so only Validate stands between it
// and a running server.
func TestLoad_RejectsExplicitlyDisabledGuards(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		wantText string
	}{
		{
			name: "zero read header timeout reopens slowloris",
			key:  "HTTP_READ_HEADER_TIMEOUT", value: "0s", wantText: "slowloris",
		},
		{
			name: "zero read timeout", key: "HTTP_READ_TIMEOUT", value: "0s",
			wantText: "HTTP_READ_TIMEOUT must be greater than 0",
		},
		{
			name: "zero write timeout", key: "HTTP_WRITE_TIMEOUT", value: "0s",
			wantText: "HTTP_WRITE_TIMEOUT must be greater than 0",
		},
		{
			name: "zero idle timeout", key: "HTTP_IDLE_TIMEOUT", value: "0s",
			wantText: "HTTP_IDLE_TIMEOUT must be greater than 0",
		},
		{
			// An already-expired shutdown deadline means nothing ever drains
			// and Jetstream cursors are never flushed.
			name: "zero shutdown timeout", key: "HTTP_SHUTDOWN_TIMEOUT", value: "0s",
			wantText: "HTTP_SHUTDOWN_TIMEOUT must be greater than 0",
		},
		{
			name: "zero max open conns leaves the pool unbounded",
			key:  "DB_MAX_OPEN_CONNS", value: "0",
			wantText: "DB_MAX_OPEN_CONNS must be greater than 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantText)
			}
		})
	}
}

// A ReadTimeout below ReadHeaderTimeout makes the slowloris guard unreachable:
// the whole-request deadline fires first.
func TestLoad_RejectsReadHeaderTimeoutExceedingReadTimeout(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("HTTP_READ_HEADER_TIMEOUT", "60s")
	t.Setenv("HTTP_READ_TIMEOUT", "30s")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a ReadHeaderTimeout larger than ReadTimeout")
	}
	if !strings.Contains(err.Error(), "HTTP_READ_HEADER_TIMEOUT") {
		t.Errorf("error = %q, want it to mention HTTP_READ_HEADER_TIMEOUT", err.Error())
	}
}

// The .env.prod.example placeholders are published in this repository, so a
// deployment still carrying one has no secret at all — the check must catch
// them, not just the dev constant.
func TestLoad_RejectsDocumentedPlaceholderSecrets(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"CURSOR_SECRET", "CHANGE_ME_CURSOR_SECRET"},
		{"OAUTH_SEAL_SECRET", "CHANGE_ME_BASE64_32_BYTES"},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted the shipped placeholder %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), "placeholder") {
				t.Errorf("error = %q, want it to name the value as a placeholder", err.Error())
			}
		})
	}
}

// Checked in config rather than left to oauth.NewOAuthClient, which runs only
// after schema migrations have been applied.
func TestLoad_ValidatesSealSecretShape(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantText string
	}{
		{"not base64", "not!valid!base64!", "must be base64"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("too short")), "must decode to 32 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("OAUTH_SEAL_SECRET", tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted OAUTH_SEAL_SECRET=%q", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantText)
			}
		})
	}
}

// A production deploy that never replaced the localhost defaults used to boot
// clean and then fail at first login — in the authorization server's logs, not
// ours.
func TestLoad_RejectsLocalhostURLsInProduction(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"APPVIEW_PUBLIC_URL", "http://localhost:8080"},
		{"APPVIEW_PUBLIC_URL", "http://127.0.0.1:8080"},
		{"PDS_URL", "http://localhost:3001"},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q in production", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error = %q, want it to mention %s", err.Error(), tc.key)
			}
		})
	}

	// Dev is where those defaults belong, so they must still be accepted.
	t.Run("allowed in dev", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		if _, err := Load(); err != nil {
			t.Fatalf("dev Load() rejected the localhost defaults: %v", err)
		}
	})
}

// INSTANCE_DID becomes the audience every aggregator service JWT is validated
// against, so a non-DID value fails every aggregator request at runtime.
func TestLoad_RejectsNonDIDInstanceDID(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("INSTANCE_DID", "coves.social")
	t.Setenv("INSTANCE_DOMAIN", "coves.social")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted an INSTANCE_DID that is not a DID")
	}
	if !strings.Contains(err.Error(), "INSTANCE_DID") {
		t.Errorf("error = %q, want it to mention INSTANCE_DID", err.Error())
	}
}

func TestLoad_ProductionRequiresSecrets(t *testing.T) {
	tests := []struct {
		name     string
		unset    string
		wantText string
	}{
		{"missing seal secret", "OAUTH_SEAL_SECRET", "OAUTH_SEAL_SECRET is required"},
		{"missing cursor secret", "CURSOR_SECRET", "CURSOR_SECRET is required"},
		{"missing jetstream feeds", "JETSTREAM_FEEDS", "JETSTREAM_FEEDS is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(tc.unset, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded without %s; production must fail closed", tc.unset)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantText)
			}
		})
	}
}

// The dev placeholder secret is in the repository, so accepting it in
// production would let anyone forge a signed pagination cursor.
func TestLoad_ProductionRejectsDevCursorSecret(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("CURSOR_SECRET", devCursorSecret)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted the dev cursor secret in production")
	}
	if !strings.Contains(err.Error(), "CURSOR_SECRET") {
		t.Errorf("error = %q, want it to mention CURSOR_SECRET", err.Error())
	}
}

func TestLoad_ProductionRejectsSkippedDIDWebVerification(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("SKIP_DID_WEB_VERIFICATION", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() allowed did:web verification to be skipped in production")
	}
	if !strings.Contains(err.Error(), "SKIP_DID_WEB_VERIFICATION") {
		t.Errorf("error = %q, want it to mention SKIP_DID_WEB_VERIFICATION", err.Error())
	}
}

func TestLoad_ProductionSucceedsWithFullConfig(t *testing.T) {
	clearEnv(t)
	prodEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error for a complete production config: %v", err)
	}
	if cfg.IsDevEnv {
		t.Error("IsDevEnv should be false")
	}
	if cfg.OAuth.SealSecretGenerated {
		t.Error("SealSecretGenerated should be false when OAUTH_SEAL_SECRET is provided")
	}
}

func TestLoad_LegacyJetstreamVarsAreRejected(t *testing.T) {
	for _, legacy := range legacyJetstreamVars {
		t.Run(legacy, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(legacy, "wss://jetstream2.us-east.bsky.network")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() ignored legacy variable %s", legacy)
			}
			if !strings.Contains(err.Error(), legacy) {
				t.Errorf("error = %q, want it to name %s", err.Error(), legacy)
			}
			if !strings.Contains(err.Error(), "JETSTREAM_FEEDS") {
				t.Errorf("error = %q, want it to point at JETSTREAM_FEEDS", err.Error())
			}
		})
	}
}

func TestLoad_InstanceDomain(t *testing.T) {
	tests := []struct {
		name       string
		did        string
		domain     string
		wantDomain string
		wantErr    bool
	}{
		{
			name:       "did:web derives its own domain",
			did:        "did:web:example.social",
			wantDomain: "example.social",
		},
		{
			// A did:web instance must not be able to mint handles under
			// someone else's domain, so INSTANCE_DOMAIN cannot override it.
			name:       "did:web ignores a conflicting INSTANCE_DOMAIN",
			did:        "did:web:example.social",
			domain:     "riotgames.com",
			wantDomain: "example.social",
		},
		{
			name:       "did:plc uses INSTANCE_DOMAIN",
			did:        "did:plc:abc123",
			domain:     "example.social",
			wantDomain: "example.social",
		},
		{
			name:    "did:plc without INSTANCE_DOMAIN is rejected",
			did:     "did:plc:abc123",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("INSTANCE_DID", tc.did)
			t.Setenv("INSTANCE_DOMAIN", tc.domain)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load() succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if cfg.Instance.Domain != tc.wantDomain {
				t.Errorf("Instance.Domain = %q, want %q", cfg.Instance.Domain, tc.wantDomain)
			}
		})
	}
}

func TestLoad_IdentityPLCResolution(t *testing.T) {
	t.Run("dev forces the resolver onto the local PLC", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("PLC_DIRECTORY_URL", "http://localhost:3002")
		// Must be ignored in dev: resolving against a different PLC than the
		// one registration writes to breaks end-to-end tests.
		t.Setenv("IDENTITY_PLC_URL", "https://plc.directory")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned error: %v", err)
		}
		if cfg.Identity.ResolverPLCURL != "http://localhost:3002" {
			t.Errorf("ResolverPLCURL = %q, want the local PLC in dev", cfg.Identity.ResolverPLCURL)
		}
	})

	t.Run("production honours IDENTITY_PLC_URL", func(t *testing.T) {
		clearEnv(t)
		prodEnv(t)
		t.Setenv("PLC_DIRECTORY_URL", "https://plc.directory")
		t.Setenv("IDENTITY_PLC_URL", "https://plc-mirror.internal")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned error: %v", err)
		}
		if cfg.Identity.ResolverPLCURL != "https://plc-mirror.internal" {
			t.Errorf("ResolverPLCURL = %q, want the configured mirror", cfg.Identity.ResolverPLCURL)
		}
		if cfg.Identity.PLCURL != "https://plc.directory" {
			t.Errorf("PLCURL = %q, want the primary directory", cfg.Identity.PLCURL)
		}
	})
}

func TestLoad_MalformedValuesAreRejected(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"IS_DEV_ENV", "yes"},
		{"DB_MAX_OPEN_CONNS", "lots"},
		{"DB_MAX_OPEN_CONNS", "-1"},
		{"HTTP_READ_TIMEOUT", "30"}, // missing a unit
		{"IDENTITY_CACHE_TTL", "forever"},
		{"SKIP_DID_WEB_VERIFICATION", "1.5"},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.value)
			}
			// Assert the reason, not just that something failed — otherwise a
			// Load() broken for an unrelated reason still passes this test.
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error = %q, want it to name %s", err.Error(), tc.key)
			}
		})
	}
}

func TestLoad_IdleConnsMayNotExceedOpenConns(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("DB_MAX_OPEN_CONNS", "10")
	t.Setenv("DB_MAX_IDLE_CONNS", "20")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted MaxIdleConns greater than MaxOpenConns")
	}
	if !strings.Contains(err.Error(), "DB_MAX_IDLE_CONNS") {
		t.Errorf("error = %q, want it to mention DB_MAX_IDLE_CONNS", err.Error())
	}
}

// Validate reports every problem at once so a misconfigured deployment can be
// fixed in one pass rather than one restart per mistake.
func TestValidate_ReportsAllProblems(t *testing.T) {
	cfg := &Config{
		IsDevEnv:     false,
		Database:     DatabaseConfig{URL: "postgres://u:p@db/coves", MaxOpenConns: 25, MaxIdleConns: 25},
		Server:       ServerConfig{Port: "8080", ReadHeaderTimeout: 10 * time.Second},
		Instance:     InstanceConfig{DID: "did:plc:abc"}, // no Domain
		CursorSecret: devCursorSecret,
		// no OAuth.SealSecret, no Jetstream.FeedsSpec
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil for an invalid production config")
	}
	for _, want := range []string{
		"INSTANCE_DOMAIN", "OAUTH_SEAL_SECRET", "CURSOR_SECRET", "JETSTREAM_FEEDS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s; got:\n%s", want, err.Error())
		}
	}
}

func TestLoad_CSVListsAreTrimmed(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("COMMUNITY_CREATORS", " did:plc:one , did:plc:two ,, ")
	t.Setenv("TRUSTED_BRIDGE_PDS_HOSTS", "bridge.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	want := []string{"did:plc:one", "did:plc:two"}
	got := cfg.Instance.AllowedCommunityCreators
	if len(got) != len(want) {
		t.Fatalf("AllowedCommunityCreators = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedCommunityCreators[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// nil vs empty matters: nil means "unrestricted", not "nobody".
	t.Setenv("COMMUNITY_CREATORS", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Instance.AllowedCommunityCreators != nil {
		t.Errorf("AllowedCommunityCreators = %v, want nil when unset (unrestricted)",
			cfg.Instance.AllowedCommunityCreators)
	}
}

func TestLoad_PortFallsBackToLegacyName(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("APPVIEW_PORT", "9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Server.Port != "9090" {
		t.Errorf("Server.Port = %q, want the APPVIEW_PORT fallback", cfg.Server.Port)
	}

	// PORT wins when both are set.
	t.Setenv("PORT", "8081")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Server.Port != "8081" {
		t.Errorf("Server.Port = %q, want PORT to take precedence", cfg.Server.Port)
	}
}

func TestAppDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		timeout time.Duration
		check   func(t *testing.T, got string)
	}{
		{
			name:    "url form gains statement_timeout in milliseconds",
			dsn:     "postgres://u:p@db:5432/coves?sslmode=disable",
			timeout: 30 * time.Second,
			check: func(t *testing.T, got string) {
				parsed, err := url.Parse(got)
				if err != nil {
					t.Fatalf("result is not a valid URL: %v", err)
				}
				if v := parsed.Query().Get("statement_timeout"); v != "30000" {
					t.Errorf("statement_timeout = %q, want %q", v, "30000")
				}
				if v := parsed.Query().Get("sslmode"); v != "disable" {
					t.Errorf("sslmode = %q, existing parameters must be preserved", v)
				}
			},
		},
		{
			name:    "an explicit statement_timeout is left alone",
			dsn:     "postgres://u:p@db:5432/coves?statement_timeout=5000",
			timeout: 30 * time.Second,
			check: func(t *testing.T, got string) {
				parsed, _ := url.Parse(got)
				if v := parsed.Query().Get("statement_timeout"); v != "5000" {
					t.Errorf("statement_timeout = %q, want the operator's %q", v, "5000")
				}
			},
		},
		{
			name:    "keyword form gains statement_timeout",
			dsn:     "host=db user=u dbname=coves sslmode=disable",
			timeout: 15 * time.Second,
			check: func(t *testing.T, got string) {
				if !strings.Contains(got, "statement_timeout=15000") {
					t.Errorf("result = %q, want statement_timeout=15000", got)
				}
				if !strings.Contains(got, "host=db") {
					t.Errorf("result = %q, existing keywords must be preserved", got)
				}
			},
		},
		{
			name:    "keyword form with an existing statement_timeout is left alone",
			dsn:     "host=db statement_timeout=2000",
			timeout: 15 * time.Second,
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "15000") {
					t.Errorf("result = %q, want the operator's 2000 preserved", got)
				}
			},
		},
		{
			name:    "zero timeout leaves the DSN untouched",
			dsn:     "postgres://u:p@db:5432/coves",
			timeout: 0,
			check: func(t *testing.T, got string) {
				if got != "postgres://u:p@db:5432/coves" {
					t.Errorf("result = %q, want the DSN unchanged", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := DatabaseConfig{URL: tc.dsn, StatementTimeout: tc.timeout}
			got, err := d.AppDSN()
			if err != nil {
				t.Fatalf("AppDSN() returned error: %v", err)
			}
			tc.check(t, got)
		})
	}
}

// Migrations must not inherit the request-path statement timeout: a CREATE
// INDEX killed halfway is worse than a slow one.
func TestMigrationDSN_HasNoStatementTimeout(t *testing.T) {
	d := DatabaseConfig{
		URL:              "postgres://u:p@db:5432/coves?sslmode=disable",
		StatementTimeout: 30 * time.Second,
	}
	got, err := d.MigrationDSN()
	if err != nil {
		t.Fatalf("MigrationDSN() returned error: %v", err)
	}
	if got != d.URL {
		t.Errorf("MigrationDSN() = %q, want the URL unchanged (%q)", got, d.URL)
	}

	// An operator-set statement_timeout must be stripped too: otherwise the
	// no-timeout guarantee holds only for the timeout this package adds.
	withOperatorTimeout := DatabaseConfig{
		URL:              "postgres://u:p@db:5432/coves?sslmode=disable&statement_timeout=5000",
		StatementTimeout: 30 * time.Second,
	}
	got, err = withOperatorTimeout.MigrationDSN()
	if err != nil {
		t.Fatalf("MigrationDSN() returned error: %v", err)
	}
	if strings.Contains(got, "statement_timeout") {
		t.Errorf("MigrationDSN() = %q, must not carry any statement timeout", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("MigrationDSN() = %q, other parameters must be preserved", got)
	}
}

func TestAppDSN_EmptyURL(t *testing.T) {
	// Both with and without a statement timeout: the early return for a
	// disabled timeout used to skip the emptiness check entirely, so
	// DatabaseConfig{} yielded ("", nil) and sql.Open then fell back to
	// PGHOST/PGUSER or the OS username.
	for _, timeout := range []time.Duration{time.Second, 0} {
		d := DatabaseConfig{URL: "  ", StatementTimeout: timeout}
		if _, err := d.AppDSN(); err == nil {
			t.Errorf("AppDSN() accepted an empty database URL (StatementTimeout=%v)", timeout)
		}
		if _, err := d.MigrationDSN(); err == nil {
			t.Errorf("MigrationDSN() accepted an empty database URL (StatementTimeout=%v)", timeout)
		}
	}
}

// PostgreSQL reads statement_timeout=0 as "no limit", so rounding a
// sub-millisecond setting down to zero would silently disable the bound
// instead of tightening it.
func TestAppDSN_SubMillisecondTimeoutClampsUp(t *testing.T) {
	d := DatabaseConfig{
		URL:              "postgres://u:p@db:5432/coves",
		StatementTimeout: 500 * time.Microsecond,
	}
	got, err := d.AppDSN()
	if err != nil {
		t.Fatalf("AppDSN() returned error: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %v", err)
	}
	if v := parsed.Query().Get("statement_timeout"); v != "1" {
		t.Errorf("statement_timeout = %q, want %q; 0 would disable the timeout entirely", v, "1")
	}
}

func TestAppDSN_MalformedURLIsRejected(t *testing.T) {
	d := DatabaseConfig{
		URL:              "postgres://u:p@db:notaport/coves",
		StatementTimeout: time.Second,
	}
	if _, err := d.AppDSN(); err == nil {
		t.Fatal("AppDSN() accepted a malformed URL instead of returning an error")
	}
}

// The URL is re-encoded when the parameter is added, so reserved characters
// must survive the round trip intact — a mangled password silently breaks
// authentication.
func TestAppDSN_PreservesEscapedCredentialsAndParams(t *testing.T) {
	d := DatabaseConfig{
		URL:              "postgres://user:p%40ss%2Fword@db:5432/coves?search_path=public,app&sslmode=require",
		StatementTimeout: 30 * time.Second,
	}
	got, err := d.AppDSN()
	if err != nil {
		t.Fatalf("AppDSN() returned error: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %v", err)
	}

	password, _ := parsed.User.Password()
	if password != "p@ss/word" {
		t.Errorf("password = %q, want %q", password, "p@ss/word")
	}
	if v := parsed.Query().Get("search_path"); v != "public,app" {
		t.Errorf("search_path = %q, want %q", v, "public,app")
	}
	if v := parsed.Query().Get("sslmode"); v != "require" {
		t.Errorf("sslmode = %q, want %q", v, "require")
	}
	if v := parsed.Query().Get("statement_timeout"); v != "30000" {
		t.Errorf("statement_timeout = %q, want %q", v, "30000")
	}
}

// libpq allows single-quoted values containing spaces. Splitting on
// whitespace alone turns "options='-c statement_timeout=1000'" into a field
// that parses as a bare statement_timeout keyword, so the parameter would be
// wrongly treated as already set.
func TestAppDSN_KeywordFormWithQuotedValue(t *testing.T) {
	d := DatabaseConfig{
		URL:              "host=db user=u options='-c default_transaction_read_only=on'",
		StatementTimeout: 15 * time.Second,
	}
	got, err := d.AppDSN()
	if err != nil {
		t.Fatalf("AppDSN() returned error: %v", err)
	}
	if !strings.Contains(got, "statement_timeout=15000") {
		t.Errorf("result = %q, want statement_timeout=15000 appended", got)
	}
	if !strings.Contains(got, "options='-c default_transaction_read_only=on'") {
		t.Errorf("result = %q, the quoted value must survive intact", got)
	}

	// And the inverse: a statement_timeout hidden inside a quoted options
	// value is not the connection parameter, so it must not suppress ours.
	hidden := DatabaseConfig{
		URL:              "host=db options='-c statement_timeout=1000'",
		StatementTimeout: 15 * time.Second,
	}
	got, err = hidden.AppDSN()
	if err != nil {
		t.Fatalf("AppDSN() returned error: %v", err)
	}
	if !strings.Contains(got, "statement_timeout=15000") {
		t.Errorf("result = %q, a quoted options value must not be mistaken for the parameter", got)
	}
}

func TestMigrationDSN_StripsKeywordFormTimeout(t *testing.T) {
	d := DatabaseConfig{URL: "host=db user=u statement_timeout=2000 sslmode=disable"}
	got, err := d.MigrationDSN()
	if err != nil {
		t.Fatalf("MigrationDSN() returned error: %v", err)
	}
	if strings.Contains(got, "statement_timeout") {
		t.Errorf("MigrationDSN() = %q, must not carry a statement timeout", got)
	}
	for _, want := range []string{"host=db", "user=u", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("MigrationDSN() = %q, must preserve %q", got, want)
		}
	}
}

func TestPDSConfig_HasInstanceCredentials(t *testing.T) {
	tests := []struct {
		name     string
		handle   string
		password string
		want     bool
	}{
		{"both set", "coves.social", "secret", true},
		{"handle only", "coves.social", "", false},
		{"password only", "", "secret", false},
		{"neither", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PDSConfig{InstanceHandle: tc.handle, InstancePassword: tc.password}
			if got := p.HasInstanceCredentials(); got != tc.want {
				t.Errorf("HasInstanceCredentials() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSignupConfig_TokenEndpointEnabled(t *testing.T) {
	tests := []struct {
		name          string
		secret        string
		adminPassword string
		want          bool
	}{
		{"both set", "turnstile-secret", "admin-password", true},
		{"captcha secret missing", "", "admin-password", false},
		{"admin password missing", "turnstile-secret", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := SignupConfig{TurnstileSecretKey: tc.secret}
			if got := s.TokenEndpointEnabled(tc.adminPassword); got != tc.want {
				t.Errorf("TokenEndpointEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoad_TrimsWhitespace(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	// Compose files and .env files routinely leave trailing spaces.
	t.Setenv("IS_DEV_ENV", " false ")
	t.Setenv("PDS_URL", "  http://pds.example.com  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.PDS.URL != "http://pds.example.com" {
		t.Errorf("PDS.URL = %q, want it trimmed", cfg.PDS.URL)
	}
	if cfg.IsDevEnv {
		t.Error("IsDevEnv should parse \" false \" as false")
	}
}
