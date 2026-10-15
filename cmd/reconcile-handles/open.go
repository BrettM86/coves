package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"Coves/internal/atproto/identity"
	"Coves/internal/config"

	_ "github.com/lib/pq"
)

// openProduction loads the AppView's configuration and opens what the sweep
// runs against: the application pool, a separate lock pool, and the caching
// and uncached identity resolvers built the way cmd/server's buildIdentity
// builds them.
//
// config.Load reads the process environment, which is what getenv reads in
// production. DATABASE_URL is required here even though config.Load would
// fall back to the local dev database: an operator tool must not pick a
// database nobody named.
func openProduction(ctx context.Context, getenv func(string) string, concurrency int) (sweepDependencies, func(), error) {
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		return sweepDependencies{}, nil, errors.New("DATABASE_URL is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return sweepDependencies{}, nil, fmt.Errorf("load configuration: %w", err)
	}
	dsn, err := cfg.Database.AppDSN()
	if err != nil {
		return sweepDependencies{}, nil, fmt.Errorf("build database DSN: %w", err)
	}

	db, err := openPool(ctx, dsn, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns, cfg.Database)
	if err != nil {
		return sweepDependencies{}, nil, fmt.Errorf("open database: %w", err)
	}
	// Lock waiters must not share a capped pool with the writes that release
	// them: every refresh holds one lock connection while it waits or holds,
	// so this pool has one connection per concurrent refresh.
	lockDB, err := openPool(ctx, dsn, concurrency, concurrency, cfg.Database)
	if err != nil {
		_ = db.Close()
		return sweepDependencies{}, nil, fmt.Errorf("open identity refresh lock database: %w", err)
	}

	allowPrivateHosts := cfg.IsDevEnv
	identityOptions := identity.PrivateHostOptions(allowPrivateHosts)
	if len(cfg.Identity.WellKnownHosts) > 0 {
		identityOptions = append(identityOptions, identity.WithWellKnownHosts(cfg.Identity.WellKnownHosts))
	}
	identityConfig := identity.DefaultConfig(identityOptions...)
	identityConfig.PLCURL = cfg.Identity.ResolverPLCURL
	if cfg.Identity.CacheTTL > 0 {
		identityConfig.CacheTTL = cfg.Identity.CacheTTL
	}
	identityConfig.NegativeCacheTTL = cfg.Identity.NegativeCacheTTL

	dependencies := sweepDependencies{
		Database:                 db,
		LockDatabase:             lockDB,
		CachingResolver:          identity.NewResolver(db, identityConfig),
		UncachedResolver:         identity.NewUncachedResolver(identityConfig),
		SkipHostedByVerification: cfg.Instance.SkipDIDWebVerification,
		InstanceDomain:           cfg.Instance.Domain,
		PLCURL:                   identityConfig.PLCURL,
		AllowPrivateHosts:        allowPrivateHosts,
	}
	closeAll := func() {
		_ = lockDB.Close()
		_ = db.Close()
	}
	return dependencies, closeAll, nil
}

// openPool opens and pings a bounded pool on dsn.
func openPool(ctx context.Context, dsn string, maxOpen, maxIdle int, cfg config.DatabaseConfig) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
