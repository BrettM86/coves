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
// THE RULE IS TRANSITIVE, and that is not a technicality — it decides the two
// biggest design questions in this package:
//
//   - internal/atproto/pds is a perfectly good PDS client, and testkit cannot
//     use it: it imports internal/core/blobs for its BlobRef type. Wrapping it
//     would make `go test ./internal/core/blobs` an import cycle. pds.go
//     therefore speaks XRPC over net/http directly, which is also what the
//     helpers it replaces did.
//   - internal/atproto/jetstream owns the JetstreamEvent structs, and testkit
//     cannot use those either: its consumers import communities, posts,
//     comments, users, votes, userblocks and aggregators. firehose.go declares
//     the four wire structs it needs (~30 lines) rather than dragging seven
//     domain packages into every test binary.
//
// Neither is a duplication testkit is free to remove later; both are load
// bearing. See the PACKAGE DOC in firehose.go for the field-drift risk this
// leaves and how it is contained.
//
// # Domain interfaces (the factory-adapter pattern)
//
// Several domain services take a PDS-client factory whose type is named by the
// domain itself — votes.PDSClientFactory, communities.PDSClientFactory, and so
// on — which are identical function types under four different names. testkit
// cannot return any of them, so instead of four adapters it exports one generic
// one, PasswordAuthFactory, which a call site instantiates with the domain's
// own constructor in a single line. See pds.go.
//
// # Layout
//
//	testkit.go   — endpoints, log silencing, the TestingT contract
//	db.go        — Postgres isolation: template-clone-per-test
//	wait.go      — the only waiting primitives tests are allowed to use
//	fixtures.go  — UniqueID, PNG/JPEG image bytes
//	pds.go       — accounts, sessions, records and blobs on the test PDS
//	firehose.go  — the one cursor-gated Jetstream subscriber
//	appview.go   — the XRPC client, and the AppView it talks to
//
// # The canonical TestMain
//
// Every package that uses this kit starts the same way, and Main is that
// starting point — log silencing plus an up-front probe of whatever
// infrastructure the package needs:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.RequirePostgres)) }
package testkit

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

// ServiceEndpoint is a plain HTTP service located by its base URL, with no
// trailing slash.
type ServiceEndpoint struct {
	BaseURL string
}

// PDSEndpoint locates the test PDS and names the domain it issues handles under.
type PDSEndpoint struct {
	BaseURL string
	// HandleDomain is the suffix every generated handle carries, without a
	// leading dot: "local.coves.dev". A handle outside the PDS' configured
	// service domains is rejected at account creation, so this is not
	// cosmetic — it is the difference between a working account and an
	// "InvalidHandle" that reads like a bug in the generator.
	HandleDomain string
}

// Handle renders a local label as a full handle on this PDS.
func (p PDSEndpoint) Handle(label string) string {
	return label + "." + p.HandleDomain
}

// JetstreamEndpoint locates the test Jetstream and the path its subscriptions
// are opened on.
type JetstreamEndpoint struct {
	BaseURL       string // ws://host:port
	SubscribePath string // /subscribe
}

// EndpointSet holds the address of every service the test stack provides.
//
// This is the only place test code reads infrastructure coordinates from the
// environment. Tests that build their own base URLs are how a suite ends up
// silently talking to a developer's dev stack — or to the public internet —
// so endpoint literals in tests are counted as violations by
// scripts/test-audit.sh.
type EndpointSet struct {
	Postgres  PostgresEndpoint
	PDS       PDSEndpoint
	PLC       ServiceEndpoint
	Jetstream JetstreamEndpoint
	AppView   ServiceEndpoint
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
		PDS: PDSEndpoint{
			BaseURL:      trimURL(envOr("PDS_URL", "http://localhost:3001")),
			HandleDomain: firstHandleDomain(os.Getenv("PDS_SERVICE_HANDLE_DOMAINS")),
		},
		PLC: ServiceEndpoint{
			BaseURL: trimURL(envOr("PLC_DIRECTORY_URL", "http://localhost:3002")),
		},
		Jetstream: JetstreamEndpoint{
			// JETSTREAM_TEST_URL rather than the JETSTREAM_FEEDS the server
			// reads: that variable is a list of key=url pairs for the AppView's
			// consumers, and a test subscribing to one endpoint should not have
			// to parse a consumer topology to find it.
			BaseURL:       trimURL(envOr("JETSTREAM_TEST_URL", "ws://localhost:6008")),
			SubscribePath: "/subscribe",
		},
		AppView: ServiceEndpoint{
			// APPVIEW_PUBLIC_URL is what the server itself publishes and what
			// .env.ci sets, so it is the honest second choice: if the AppView
			// believes it lives somewhere, that is where tests should call it.
			BaseURL: trimURL(envOr("APPVIEW_URL", envOr("APPVIEW_PUBLIC_URL", "http://localhost:8081"))),
		},
	}
}

// firstHandleDomain reads the PDS' service handle domains — the same
// ".local.coves.dev,.coves.social" the PDS container is configured with — and
// returns the first, without its leading dot.
//
// Sharing the variable with the container means a stack that changes its handle
// domain cannot leave the generator behind: handles would be rejected at the
// PDS with an error nobody would think to trace back to a test helper.
func firstHandleDomain(configured string) string {
	first, _, _ := strings.Cut(configured, ",")
	first = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "."))
	if first == "" {
		return "local.coves.dev"
	}
	return first
}

// trimURL drops a trailing slash, so joining a path onto a base URL never
// produces a double slash — which some routers treat as a different route.
func trimURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
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
// Call it from TestMain — or better, call Main, which does this and more. It
// mirrors the per-package TestMain blocks that already exist across the tree,
// and additionally silences slog, which those blocks miss: the server logs
// through slog, so silencing only the standard logger leaves most of the noise
// in place.
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

// Main is the canonical TestMain body. Every migrated package uses it:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.Main(m)) }
//
// It silences logs, then runs the tests. A package whose tests need
// infrastructure names it, and the probe runs ONCE, before any test:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.RequirePDS)) }
//
// The reason to probe up front rather than let the first test fail is
// attribution. Without it, a stack that is not running produces one confusing
// failure per test — thirty timeouts blaming thirty different features — and
// the reader has to notice they share a cause. With it, the package fails once
// with the address it could not reach and the command that starts it.
//
// Requirements are opt-in because most packages need none: a table-driven test
// of a pure function should not fail because Postgres is down.
func Main(m *testing.M, requires ...Requirement) int {
	SilenceLogs()
	for _, require := range requires {
		if err := require(); err != nil {
			fmt.Fprintf(os.Stderr, "\ntestkit: the test stack is not ready\n  %v\n\n"+
				"  start it with 'make dev-up', or run the whole suite through 'make ci'.\n\n", err)
			return 1
		}
	}
	return m.Run()
}

// A Requirement probes one service and explains what is missing.
type Requirement func() error

// RequirePostgres fails the package unless the test database answers.
func RequirePostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return WaitForPostgres(ctx, 10*time.Second)
}

// RequirePDS fails the package unless the PDS answers.
func RequirePDS() error { return probeHTTP("PDS", Endpoints().PDS.BaseURL) }

// RequireAppView fails the package unless the AppView answers.
func RequireAppView() error { return probeHTTP("AppView", Endpoints().AppView.BaseURL) }

// RequireJetstream fails the package unless a subscription can be opened.
//
// It dials rather than probing an HTTP port: the Jetstream image in this stack
// runs with its health endpoint disabled, so a TCP connect proves less than a
// handshake does.
func RequireJetstream() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := Endpoints().Jetstream
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, endpoint.BaseURL+endpoint.SubscribePath, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("Jetstream at %s did not accept a subscription: %w", endpoint.BaseURL, err)
	}
	_ = conn.Close()
	return nil
}

func probeHTTP(name, baseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewXRPCClient(baseURL).Health(ctx); err != nil {
		return fmt.Errorf("%s at %s did not answer its health endpoint: %w", name, baseURL, err)
	}
	return nil
}
