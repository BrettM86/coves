// Package testkit is the single test harness for the Coves suite: database
// isolation, waiting primitives, and fixture generators, shared by every tier
// (unit, integration, pipeline, live).
//
// # Dependency direction (hard rule)
//
// testkit imports NO domain package — nothing from internal/core/..., nothing
// from internal/api/.... It may import infrastructure only: the database
// driver, goose, the embedded migrations, and (later) the jetstream event
// types.
//
// The reason is not taste, it is compilability. Integration tests live
// in-package, next to the code they test: internal/core/communities has its own
// _test.go files, and those files import testkit. If testkit imported
// internal/core/communities, that would be an import cycle and neither package
// would build. The rule has to hold for every domain package, so the only safe
// version of it is "no domain imports at all".
//
// Domain-specific builders that need domain types belong either in that
// domain's own _test.go files or in a small leaf <domain>test package — never
// here.
//
// # Layout
//
//	testkit.go   — endpoints, log silencing, the TestingT contract
//	db.go        — Postgres isolation: template-clone-per-test
//	wait.go      — the only waiting primitives tests are allowed to use
//	fixtures.go  — UniqueID, PNG/JPEG image bytes
package testkit

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
)

// TestingT is the subset of *testing.T that testkit needs.
//
// It is an interface rather than *testing.T for one reason: testkit's own
// failure paths have to be testable. testing.TB cannot be implemented outside
// the standard library (it has an unexported method), so asserting that
// WaitFor fails with the right message would otherwise require a subprocess.
// *testing.T and *testing.B both satisfy this, so call sites are unaffected.
//
// Implementations must treat Fatalf as fatal — real *testing.T calls
// runtime.Goexit, and every testkit function relies on Fatalf not returning by
// also returning immediately afterwards.
type TestingT interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Name() string
}

// PostgresEndpoint locates a Postgres server and the credentials to reach it.
type PostgresEndpoint struct {
	Host string
	Port string
	User string
	// Password is a throwaway local credential. Never include it in a failure
	// message: use Redacted, not URL, when building error text.
	Password string
	// Database is the maintenance database: the one testkit connects to in
	// order to CREATE/DROP other databases and to hold advisory locks. It is
	// never the database a test writes to.
	Database string
	SSLMode  string
}

// URL renders a libpq connection string for the named database on this server.
func (p PostgresEndpoint) URL(database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		p.User, p.Password, p.Host, p.Port, database, p.SSLMode)
}

// Redacted renders the server address without credentials, for error messages.
func (p PostgresEndpoint) Redacted(database string) string {
	return fmt.Sprintf("postgres://%s@%s:%s/%s", p.User, p.Host, p.Port, database)
}

// EndpointSet holds the address of every service the test stack provides.
//
// This is the only place test code reads infrastructure coordinates from the
// environment. Tests that build their own base URLs are how a suite ends up
// silently talking to a developer's dev stack — or to the public internet —
// so endpoint literals in tests are counted as violations by
// scripts/test-audit.sh.
//
// Later phases extend this struct with the PDS, PLC, Jetstream and AppView
// addresses; the Postgres coordinates are all the database harness needs.
type EndpointSet struct {
	Postgres PostgresEndpoint
}

// Endpoints returns the test stack's coordinates, read from the environment
// once per process.
//
// Defaults match docker-compose.dev.yml's postgres-test service as published on
// the host, and .env.ci's values inside the hermetic stack — which share a
// network namespace, so "localhost:5434" is correct in both places.
//
// The memoised loader lives in db.go alongside the other process singletons,
// behind a lock, because testkit's own tests rebind it.
func Endpoints() EndpointSet {
	return loadedEndpoints()()
}

func loadEndpoints() EndpointSet {
	return EndpointSet{
		Postgres: PostgresEndpoint{
			Host:     envOr("POSTGRES_TEST_HOST", "localhost"),
			Port:     envOr("POSTGRES_TEST_PORT", "5434"),
			User:     envOr("POSTGRES_TEST_USER", "test_user"),
			Password: envOr("POSTGRES_TEST_PASSWORD", "test_password"),
			Database: envOr("POSTGRES_TEST_DB", "coves_test"),
			SSLMode:  envOr("POSTGRES_TEST_SSLMODE", "disable"),
		},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// SilenceLogs discards application log output when LOG_ENABLED=false, which is
// what the CI runner and `make test` set.
//
// Call it from TestMain. It mirrors the per-package TestMain blocks that
// already exist across the tree, and additionally silences slog, which those
// blocks miss — the server logs through slog, so silencing only the standard
// logger leaves most of the noise in place.
//
// This mutates process-global state, so it belongs in TestMain and nowhere
// else: a test calling it mid-run would race any test reading log output.
func SilenceLogs() {
	if os.Getenv("LOG_ENABLED") != "false" {
		return
	}
	log.SetOutput(io.Discard)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}
