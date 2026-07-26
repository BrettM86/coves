package main

import (
	"Coves/internal/config"
	"Coves/internal/db/migrations"
	"context"
	"database/sql"
	"fmt"
	"log/slog"

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
func openDatabase(ctx context.Context, cfg config.DatabaseConfig) (*sql.DB, error) {
	if err := runMigrations(ctx, cfg); err != nil {
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

// runMigrations applies pending migrations on a short-lived connection that
// carries no statement timeout. Migrations are embedded in the binary, so this
// does not depend on the process's working directory.
func runMigrations(ctx context.Context, cfg config.DatabaseConfig) error {
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
