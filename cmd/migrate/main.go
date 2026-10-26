// Command migrate applies or rolls back the AppView's embedded goose
// migrations against a database.
//
// The stock goose CLI cannot run this repository's migrations: Go migrations
// (such as 062_backfill_content_blob_references.go) register themselves with
// goose.AddMigrationContext in init() and only exist inside a binary that
// imports Coves/internal/db/migrations. This command uses the same
// goose.SetBaseFS(migrations.FS) path as the server's startup migrations, but
// it does not run ReencryptLegacyCredentials, which the server runs before
// goose.
//
// Usage:
//
//	go run ./cmd/migrate [-dsn DSN] up|down
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/pressly/goose/v3"

	"Coves/internal/db/migrations"

	"github.com/lib/pq"
)

const developmentDSN = "postgresql://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"

func main() {
	// The default is applied after parsing so usage output never prints a
	// connection string with its password.
	dsn := flag.String("dsn", "", "PostgreSQL connection string (default: the local development database)")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "usage: migrate [-dsn DSN] up|down")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *dsn == "" {
		*dsn = developmentDSN
	}

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *dsn, flag.Arg(0)); err != nil {
		slog.Error("migration failed", "error", err)
		stop()
		os.Exit(1)
	}
}

// errInvalidDSN replaces lib/pq's connection-settings parse errors, which can
// quote the connection string and with it the password.
var errInvalidDSN = errors.New("invalid DSN: the connection string or a PG* environment variable could not be parsed")

func run(ctx context.Context, dsn, command string) error {
	if command != "up" && command != "down" {
		return fmt.Errorf("unknown command %q: want up or down", command)
	}

	config, err := pq.NewConfig(dsn)
	if err != nil {
		return errInvalidDSN
	}
	return redactPassword(migrate(ctx, config, command), config.Password)
}

// redactPassword removes password from err's message. No error path below
// is known to include it; this keeps any future one from reaching the log.
func redactPassword(err error, password string) error {
	if err == nil || password == "" || !strings.Contains(err.Error(), password) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "xxxxx"))
}

func migrate(ctx context.Context, config pq.Config, command string) (err error) {
	connector, err := pq.NewConnectorConfig(config)
	if err != nil {
		return fmt.Errorf("creating migration connector: %w", err)
	}
	db := sql.OpenDB(connector)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing migration connection: %w", closeErr))
		}
	}()
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging database for migrations: %w", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	goose.SetBaseFS(migrations.FS)

	switch command {
	case "up":
		if err := goose.UpContext(ctx, db, "."); err != nil {
			return fmt.Errorf("running migrations up: %w", err)
		}
	case "down":
		if err := goose.DownContext(ctx, db, "."); err != nil {
			return fmt.Errorf("rolling back last migration: %w", err)
		}
	}
	return nil
}
