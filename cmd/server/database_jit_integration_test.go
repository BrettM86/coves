//go:build integration

package main

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The planner's cost estimate for the notification list query triggers JIT
// compilation, which cost seconds per request against a millisecond execution.
// Every pool the server opens from AppDSN must run with JIT off, whatever the
// server or database default is.
func TestApplicationPoolConnectionsRunWithJITOff(t *testing.T) {
	ctx := t.Context()
	clone := testkit.DB(t)
	var name string
	require.NoError(t, clone.QueryRowContext(ctx, "SELECT current_database()").Scan(&name))

	// Force the database default on, so the assertion below cannot pass on
	// the server's own default.
	_, err := clone.ExecContext(ctx, `ALTER DATABASE "`+name+`" SET jit = on`)
	require.NoError(t, err)

	databaseURL := testkit.Endpoints().Postgres.URL(name)
	plain, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = plain.Close() })
	var plainJIT string
	require.NoError(t, plain.QueryRowContext(ctx, "SHOW jit").Scan(&plainJIT))
	require.Equal(t, "on", plainJIT, "precondition: a plain connection to the clone must start with jit on")

	// The smallest opener that goes through AppDSN without running
	// migrations; every application pool, openDatabase's included, uses AppDSN.
	pool, err := openIdentityRefreshLockDatabase(ctx, config.DatabaseConfig{
		URL:              databaseURL,
		StatementTimeout: 30 * time.Second,
		ConnMaxLifetime:  time.Minute,
		ConnMaxIdleTime:  time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })

	var poolJIT string
	require.NoError(t, pool.QueryRowContext(ctx, "SHOW jit").Scan(&poolJIT))
	require.Equal(t, "off", poolJIT, "application pool connections must run with jit off")
}
