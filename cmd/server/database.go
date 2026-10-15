package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher"
	"Coves/internal/db/migrations"
	postgresRepo "Coves/internal/db/postgres"

	"github.com/pressly/goose/v3"
)

// openDatabase runs schema migrations and returns the configured application
// connection pool.
//
// Migrations and application queries deliberately use separate connections.
// The application pool carries a statement_timeout so a runaway query cannot
// pin a connection indefinitely; migrations must not inherit that bound,
// because a CREATE INDEX or a backfill can legitimately run longer than any
// request handler should, and a migration cancelled halfway is far worse than
// a slow one.
//
// The caller owns the returned pool and must Close it.
func openDatabase(
	ctx context.Context,
	cfg config.DatabaseConfig,
	cipher *credentialcipher.Cipher,
	keyIsEphemeral bool,
) (*sql.DB, error) {
	if err := runMigrations(ctx, cfg, cipher, keyIsEphemeral); err != nil {
		return nil, err
	}

	dsn, err := cfg.AppDSN()
	if err != nil {
		return nil, fmt.Errorf("building application DSN: %w", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening AppView database: %w", err)
	}

	// Bound the pool. database/sql defaults MaxOpenConns to unlimited, so a
	// traffic spike can open connections until PostgreSQL's max_connections
	// is exhausted — which locks out every other client, including psql and
	// the maintenance commands under cmd/.
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		// PingContext has already opened a connection and started the pool's
		// background goroutines, so the pool must be closed even though it is
		// unusable.
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close database pool after failed ping", "error", closeErr)
		}
		return nil, fmt.Errorf("pinging AppView database: %w", err)
	}

	slog.Info("connected to AppView database",
		"max_open_conns", cfg.MaxOpenConns,
		"max_idle_conns", cfg.MaxIdleConns,
		"conn_max_lifetime", cfg.ConnMaxLifetime,
		"conn_max_idle_time", cfg.ConnMaxIdleTime,
		"statement_timeout", cfg.StatementTimeout,
	)
	return db, nil
}

// openSessionCoordinationDatabase returns the small dedicated pool that OAuth
// session operations hold connections from. Each authenticated PDS write pins
// one of these connections, under a per-session advisory lock, for its whole
// PDS round trip, and same-session waiters block on that lock with their own
// connection. Isolating that from the request pool keeps session lookups and
// feed queries flowing while PDS writes are slow.
//
// The caller owns the returned pool and must Close it.
func openSessionCoordinationDatabase(ctx context.Context, cfg config.DatabaseConfig) (*sql.DB, error) {
	dsn, err := cfg.AppDSN()
	if err != nil {
		return nil, fmt.Errorf("building session coordination DSN: %w", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening session coordination database: %w", err)
	}
	db.SetMaxOpenConns(cfg.SessionCoordinationMaxOpenConns)
	db.SetMaxIdleConns(cfg.SessionCoordinationMaxOpenConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close session coordination pool after failed ping", "error", closeErr)
		}
		return nil, fmt.Errorf("pinging session coordination database: %w", err)
	}

	slog.Info("connected session coordination pool",
		"max_open_conns", cfg.SessionCoordinationMaxOpenConns,
	)
	return db, nil
}

// identityRefreshLockMaxOpenConns caps the identity refresh lock pool. Each
// in-flight identity refresh holds one connection from it while it waits for
// or holds its DID's lock, and a process runs at most a few refreshes at once
// (one per feed's consumer, plus the dead letter redriver). When it is full,
// further refreshes queue for a connection under their own context.
const identityRefreshLockMaxOpenConns = 8

// openIdentityRefreshLockDatabase returns the small dedicated pool that
// identity refreshes hold their per-DID advisory locks on. Keeping the waiting
// and holding connections off the application pool means refreshes queued on
// one DID cannot starve the handle write that would release them. Each lock
// wait is bounded by the lock_timeout the lock sets on its connection
// (jetstream.IdentityRefreshLockWaitTimeout).
//
// The caller owns the returned pool and must Close it.
func openIdentityRefreshLockDatabase(ctx context.Context, cfg config.DatabaseConfig) (*sql.DB, error) {
	dsn, err := cfg.AppDSN()
	if err != nil {
		return nil, fmt.Errorf("building identity refresh lock DSN: %w", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening identity refresh lock database: %w", err)
	}
	db.SetMaxOpenConns(identityRefreshLockMaxOpenConns)
	db.SetMaxIdleConns(identityRefreshLockMaxOpenConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close identity refresh lock pool after failed ping", "error", closeErr)
		}
		return nil, fmt.Errorf("pinging identity refresh lock database: %w", err)
	}

	slog.Info("connected identity refresh lock pool",
		"max_open_conns", identityRefreshLockMaxOpenConns,
	)
	return db, nil
}

// runMigrations applies pending migrations on a short-lived connection that
// carries no statement timeout. Before goose runs, legacy credentials are
// resealed with the application cipher because migration 046 refuses to drop
// their database-held key while any remain. On a fresh database the legacy key
// table does not exist yet, so the pass is a no-op. Migrations are embedded in the
// binary, so this does not depend on the process's working directory.
func runMigrations(
	ctx context.Context,
	cfg config.DatabaseConfig,
	cipher *credentialcipher.Cipher,
	keyIsEphemeral bool,
) error {
	dsn, err := cfg.MigrationDSN()
	if err != nil {
		return fmt.Errorf("building migration DSN: %w", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close migration connection", "error", closeErr)
		}
	}()

	// Migrations are a single serial operation; one connection is enough and
	// keeps this out of the way of the application pool's budget.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging database for migrations: %w", err)
	}

	report, err := postgresRepo.ReencryptLegacyCredentials(ctx, db, cipher, keyIsEphemeral)
	if err != nil {
		return fmt.Errorf("re-encrypting legacy database credentials: %w", err)
	}
	if report.CommunitiesRewritten > 0 || report.AggregatorsRewritten > 0 {
		slog.Info("legacy database credentials re-encrypted with the application cipher",
			"communities_rewritten", report.CommunitiesRewritten,
			"aggregators_rewritten", report.AggregatorsRewritten,
		)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	goose.SetBaseFS(migrations.FS)

	// "." is the root of the embedded filesystem, which contains only the
	// migration files themselves.
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	slog.Info("database migrations applied")
	return nil
}
