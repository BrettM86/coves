package testkit

import (
	"sync"
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
	endpointsOnce = sync.OnceValue(loadEndpoints)
	t.Cleanup(func() { endpointsOnce = sync.OnceValue(loadEndpoints) })

	pg := Endpoints().Postgres
	// docker-compose.dev.yml publishes postgres-test here, and .env.ci's shared
	// network namespace puts it at the same address inside the hermetic stack.
	assert.Equal(t, "localhost", pg.Host)
	assert.Equal(t, "5434", pg.Port)
	assert.Equal(t, "test_user", pg.User)
	assert.Equal(t, "coves_test", pg.Database)
	assert.Equal(t, "disable", pg.SSLMode)
}

func TestEndpoints_ReadTheEnvironment(t *testing.T) {
	t.Setenv("POSTGRES_TEST_HOST", "db.example.internal")
	t.Setenv("POSTGRES_TEST_PORT", "6543")
	endpointsOnce = sync.OnceValue(loadEndpoints)
	t.Cleanup(func() { endpointsOnce = sync.OnceValue(loadEndpoints) })

	pg := Endpoints().Postgres
	assert.Equal(t, "db.example.internal", pg.Host)
	assert.Equal(t, "6543", pg.Port)
}

func TestEndpoints_MemoisedPerProcess(t *testing.T) {
	first := Endpoints()
	require.Equal(t, first, Endpoints(), "endpoints are read once; a mid-run change would be invisible anyway")
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
