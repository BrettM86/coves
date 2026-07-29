//go:build live

// Package live holds the opt-in tier of tests that deliberately reach the
// public internet: real Bluesky handles and posts, the production PLC
// directory, and third-party unfurl targets (Streamable, YouTube, Reddit,
// Wikipedia, Kagi Kite).
//
// Everything here is excluded from the merge gate by the `live` build tag.
// `make ci` runs an egress-blocked stack (docker-compose.ci.yml declares its
// network `internal: true`), so these tests could not pass there even if they
// were compiled in — which is the point. Reality checks against third parties
// belong in a run someone chooses to make, not in the gate that decides whether
// a change merges.
//
// Run them with:
//
//	make test-live
//
// They need the test database on localhost:5434 (`make dev-up` provides it) and
// working internet access. When the network is unavailable these tests are
// expected to fail rather than pass quietly.
package live

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// setupTestDB connects to the test database, runs migrations, and clears the
// tables these tests write to. Copied from tests/integration so the live tier
// stands alone; the two converge again when the suite moves to testkit.DB(t).
func setupTestDB(t *testing.T) *sql.DB {
	testUser := os.Getenv("POSTGRES_TEST_USER")
	testPassword := os.Getenv("POSTGRES_TEST_PASSWORD")
	testPort := os.Getenv("POSTGRES_TEST_PORT")
	testDB := os.Getenv("POSTGRES_TEST_DB")

	if testUser == "" {
		testUser = "test_user"
	}
	if testPassword == "" {
		testPassword = "test_password"
	}
	if testPort == "" {
		testPort = "5434"
	}
	if testDB == "" {
		testDB = "coves_test"
	}

	dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		testUser, testPassword, testPort, testDB)

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	if pingErr := db.Ping(); pingErr != nil {
		t.Fatalf("Failed to ping test database: %v", pingErr)
	}

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}

	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Order matters: foreign keys point from subscriptions → communities/users,
	// comments → posts, posts → communities.
	for _, stmt := range []string{
		"DELETE FROM community_subscriptions",
		"DELETE FROM comments",
		"DELETE FROM posts",
		"DELETE FROM communities",
		"DELETE FROM users WHERE handle LIKE '%.test'",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Logf("Warning: cleanup %q failed: %v", stmt, err)
		}
	}

	return db
}

// generateTestDID builds a valid did:plc-shaped string for fixtures that never
// need PLC registration.
func generateTestDID(suffix string) string {
	return fmt.Sprintf("did:plc:test%s", suffix)
}
