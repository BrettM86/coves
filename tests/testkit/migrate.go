package testkit

import (
	"context"
	"database/sql"

	"Coves/internal/db/migrations"

	"github.com/pressly/goose/v3"
)

// Rolling a clone's schema forwards and backwards.
//
// WHY THIS IS IN THE KIT AND NOT IN A TEST BODY
//
// docs/TEST_ARCHITECTURE.md §3.3 states the rule plainly: goose survives in
// exactly one place — testkit's own provisioning, through NewProvider rather
// than the package globals — and in no test body. A migration's Down section is
// nonetheless a thing that must be TESTED, because a rollback that destroys
// rows is discovered in production or not at all, and the only honest way to
// test it is to run it. These two functions are how a test does that without
// importing goose, and they use the same provider construction migrateAndStamp
// uses, so there is still one notion of where migrations live.
//
// THEY ONLY MAKE SENSE ON A CLONE. Both take the *sql.DB that DB(t) returned:
// a private, per-test database that is dropped when the test ends. Pointing
// them at a shared database would migrate it out from under every other test,
// and pointing them at the TEMPLATE would corrupt the thing clones are made
// from — which is why neither function accepts a database name.

// MigrateDownOne rolls this test's database back by exactly one migration and
// returns the version it undid.
//
// The returned version is what lets a caller prove it rolled back the migration
// it meant to: a test that asserts on "the Down of 034" and silently gets 035's
// instead is testing nothing, and nothing else in the harness would notice.
func MigrateDownOne(t TestingT, db *sql.DB) int64 {
	t.Helper()

	provider := migrationProvider(t, db)
	if provider == nil {
		return 0
	}
	result, err := provider.Down(context.Background())
	if err != nil {
		t.Fatalf("testkit.MigrateDownOne: rolling back one migration: %v", err)
		return 0
	}
	if result == nil || result.Source == nil {
		t.Fatalf("testkit.MigrateDownOne: goose reported no migration to roll back")
		return 0
	}
	return result.Source.Version
}

// MigrateUp re-applies every pending migration to this test's database.
//
// Its normal use is the second half of a rollback test: undo a migration, assert
// what the Down section preserved, then put the schema back so the rest of the
// test — and any assertion about the migrated shape — runs against the real one.
func MigrateUp(t TestingT, db *sql.DB) {
	t.Helper()

	provider := migrationProvider(t, db)
	if provider == nil {
		return
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("testkit.MigrateUp: applying pending migrations: %v", err)
	}
}

// migrationProvider builds a goose provider over the embedded migrations for one
// database. Fatalf does not return, so callers still check for nil to satisfy
// the compiler and any TestingT implementation that is less abrupt than
// *testing.T.
func migrationProvider(t TestingT, db *sql.DB) *goose.Provider {
	t.Helper()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("testkit: configuring goose over the embedded migrations: %v", err)
		return nil
	}
	return provider
}
