package testkit

import (
	"context"
	"database/sql"
	"strings"

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
// from — which is why neither function accepts a database name, and why both
// refuse outright any database whose name does not carry ClonePrefix.

// MigrateDownOne rolls this test's database back by exactly one migration —
// after proving that the schema is currently AT expectedCurrentVersion — and
// returns the version it undid.
//
// The precondition is the point. A rollback test written against "the Down of
// 034" keeps passing after migration 035 lands, silently asserting on 035's
// Down instead — the strongest wrong answer a green test can give, because
// 034's Down (the thing the test exists to prove non-destructive) stops being
// run at all. Failing loudly here turns "a future migration landed" into a
// named remedy at the call site instead of a quietly retargeted assertion.
// The returned version is the same proof after the fact: the caller can pin
// which migration's Down actually ran.
func MigrateDownOne(t TestingT, db *sql.DB, expectedCurrentVersion int64) int64 {
	t.Helper()

	requireClone(t, db, "MigrateDownOne")
	provider := migrationProvider(t, db)
	if provider == nil {
		return 0
	}

	current, err := provider.GetDBVersion(context.Background())
	if err != nil {
		t.Fatalf("testkit.MigrateDownOne: reading the current migration version: %v", err)
		return 0
	}
	if current != expectedCurrentVersion {
		t.Fatalf("testkit.MigrateDownOne: the database is at migration %d, not %d — a migration has landed on top of the one this test rolls back. "+
			"Rolling back now would exercise %d's Down section and silently stop testing the one the assertions are about. "+
			"Remedy: update the call site to the new current version and re-check what that migration's Down preserves, "+
			"or roll back through the newer migrations explicitly, one asserted step at a time.",
			current, expectedCurrentVersion, current)
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

	requireClone(t, db, "MigrateUp")
	provider := migrationProvider(t, db)
	if provider == nil {
		return
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("testkit.MigrateUp: applying pending migrations: %v", err)
	}
}

// requireClone refuses to run migrations against anything but a per-test
// clone. The *sql.DB handed in is supposed to be the one DB(t) returned, but
// nothing in the type system says so — a handle to the template or to a shared
// dev database satisfies the signature just as well, and migrating THOSE
// corrupts every other test (or the developer's data) instead of one clone.
// current_database() is authoritative for where this pool actually points, and
// the clone prefix is the same rail every destructive statement in db.go rides.
func requireClone(t TestingT, db *sql.DB, operation string) {
	t.Helper()

	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("testkit.%s: identifying the connected database: %v", operation, err)
		return
	}
	if !strings.HasPrefix(name, ClonePrefix) {
		t.Fatalf("testkit.%s: refusing to migrate %q: only per-test clones (prefix %q, from testkit.DB) may be rolled forwards and backwards — anything else is the template or shared state",
			operation, name, ClonePrefix)
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
