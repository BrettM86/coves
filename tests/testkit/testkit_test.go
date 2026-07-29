package testkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpoints_DefaultsMatchTheTestStack(t *testing.T) {
	for _, key := range []string{
		"POSTGRES_TEST_HOST", "POSTGRES_TEST_PORT", "POSTGRES_TEST_USER",
		"POSTGRES_TEST_PASSWORD", "POSTGRES_TEST_DB", "POSTGRES_TEST_SSLMODE",
	} {
		t.Setenv(key, "")
	}
	swapEndpoints(t)

	pg := Endpoints().Postgres
	// docker-compose.dev.yml publishes postgres-test here, and .env.ci's shared
	// network namespace puts it at the same address inside the hermetic stack.
	assert.Equal(t, "localhost", pg.Host)
	assert.Equal(t, "5434", pg.Port)
	assert.Equal(t, "test_user", pg.User)
	assert.Equal(t, "coves_test", pg.Database)
	assert.Equal(t, "disable", pg.SSLMode)
}

func TestEndpoints_ServiceDefaultsMatchTheTestStack(t *testing.T) {
	for _, key := range []string{
		"PDS_URL", "PDS_SERVICE_HANDLE_DOMAINS", "PLC_DIRECTORY_URL",
		"JETSTREAM_TEST_URL", "APPVIEW_URL", "APPVIEW_PUBLIC_URL",
	} {
		t.Setenv(key, "")
	}
	swapEndpoints(t)

	endpoints := Endpoints()
	// The ports docker-compose.dev.yml publishes on the host, which .env.ci
	// reproduces inside the hermetic stack's shared network namespace.
	assert.Equal(t, "http://localhost:3001", endpoints.PDS.BaseURL)
	assert.Equal(t, "local.coves.dev", endpoints.PDS.HandleDomain)
	assert.Equal(t, "http://localhost:3002", endpoints.PLC.BaseURL)
	assert.Equal(t, "ws://localhost:6008", endpoints.Jetstream.BaseURL)
	assert.Equal(t, "/subscribe", endpoints.Jetstream.SubscribePath)
	assert.Equal(t, "http://localhost:8081", endpoints.AppView.BaseURL)
}

func TestEndpoints_AppViewFallsBackToThePublishedURL(t *testing.T) {
	t.Setenv("APPVIEW_URL", "")
	// What the server itself publishes, and what .env.ci sets. If the AppView
	// believes it lives somewhere, that is where tests should call it.
	t.Setenv("APPVIEW_PUBLIC_URL", "http://127.0.0.1:8081")
	swapEndpoints(t)

	assert.Equal(t, "http://127.0.0.1:8081", Endpoints().AppView.BaseURL)
}

func TestEndpoints_HandleDomainComesFromThePDSConfiguration(t *testing.T) {
	// Verbatim from .env.dev and .env.ci: leading dots, more than one domain.
	t.Setenv("PDS_SERVICE_HANDLE_DOMAINS", ".local.coves.dev,.coves.social")
	swapEndpoints(t)

	pds := Endpoints().PDS
	assert.Equal(t, "local.coves.dev", pds.HandleDomain)
	assert.Equal(t, "alice.local.coves.dev", pds.Handle("alice"))
}

func TestEndpoints_TrimTrailingSlashes(t *testing.T) {
	t.Setenv("PDS_URL", "http://pds.test:3001/")
	t.Setenv("APPVIEW_URL", "http://appview.test:8081//")
	swapEndpoints(t)

	endpoints := Endpoints()
	// Joining "/xrpc/..." onto a base ending in a slash yields "//xrpc/...",
	// which some routers treat as a different path entirely.
	assert.Equal(t, "http://pds.test:3001", endpoints.PDS.BaseURL)
	assert.Equal(t, "http://appview.test:8081", endpoints.AppView.BaseURL)
}

func TestEndpoints_ReadTheEnvironment(t *testing.T) {
	t.Setenv("POSTGRES_TEST_HOST", "db.example.internal")
	t.Setenv("POSTGRES_TEST_PORT", "6543")
	swapEndpoints(t)

	pg := Endpoints().Postgres
	assert.Equal(t, "db.example.internal", pg.Host)
	assert.Equal(t, "6543", pg.Port)
}

func TestEndpoints_MemoisedPerProcess(t *testing.T) {
	swapEndpoints(t)
	t.Setenv("PDS_URL", "http://first.test:3001")
	first := Endpoints()
	require.Equal(t, "http://first.test:3001", first.PDS.BaseURL)

	// Changing the environment after the first read must not change the answer.
	// Asserting only that two consecutive calls agree would pass against a
	// function that re-read the environment every time, which is the property
	// this test exists to rule out: a suite whose endpoints could change halfway
	// through a run would have tests talking to two different stacks.
	t.Setenv("PDS_URL", "http://second.test:3001")

	assert.Equal(t, first, Endpoints())
	assert.Equal(t, "http://first.test:3001", Endpoints().PDS.BaseURL)
}

func TestPostgresEndpoint_RedactedOmitsTheCredential(t *testing.T) {
	pg := PostgresEndpoint{
		Host: "localhost", Port: "5434", User: "test_user",
		Password: "hunter2", Database: "coves_test", SSLMode: "disable",
	}

	assert.Equal(t, "postgres://test_user:hunter2@localhost:5434/coves_test?sslmode=disable",
		pg.URL(pg.Database))

	redacted := pg.Redacted("some_clone")
	assert.NotContains(t, redacted, "hunter2", "error messages must not leak the password")
	assert.Contains(t, redacted, "localhost:5434")
	assert.Contains(t, redacted, "some_clone")
}
