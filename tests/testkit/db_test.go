package testkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests need Postgres. They fail rather than skip when it is missing:
// running the suite is a request for its infrastructure, and a suite that
// prints green with its database down is the failure mode this whole refactor
// exists to remove. Start it with `make test` or `make dev-up`.

func TestDB_ParallelClonesCannotSeeEachOther(t *testing.T) {
	// Both subtests insert a user with the SAME primary key. On a shared
	// database the second insert would fail outright; if the clones somehow
	// pointed at one database, the row count would be 2. Overlapping in time
	// is the point, so each waits for the other's write before counting.
	const sharedDID = "did:plc:testkitisolationprobe"
	var written atomic.Int32

	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("clone-%d", i), func(t *testing.T) {
			t.Parallel()
			db := DB(t)

			_, err := db.Exec(
				`INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
				sharedDID, UniqueID(t)+".test", "http://pds.invalid")
			require.NoError(t, err, "each clone starts empty, so this insert must succeed")
			written.Add(1)

			WaitFor(t, 30*time.Second, func() (bool, error) {
				return written.Load() == 2, nil
			}, WithDescription("both clones to have written the shared DID"))

			var count int
			require.NoError(t, db.QueryRow(
				`SELECT count(*) FROM users WHERE did = $1`, sharedDID).Scan(&count))
			assert.Equal(t, 1, count, "a clone must only ever see its own writes")
		})
	}
}

func TestDB_CarriesTheMigratedSchema(t *testing.T) {
	db := DB(t)

	var applied int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM goose_db_version`).Scan(&applied))
	assert.Greater(t, applied, 1, "the clone should arrive fully migrated")

	// A table from the last migration, not just the first: a template built
	// from a truncated migration run would still pass a users-table check.
	var exists sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass('public.jetstream_record_revs')::text`).Scan(&exists))
	assert.True(t, exists.Valid, "expected the full migration set to have been applied")

	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users`).Scan(&rows))
	assert.Zero(t, rows, "a clone must arrive with no data")
}

func TestDB_CleanupDropsTheClone(t *testing.T) {
	var name string

	t.Run("inner", func(t *testing.T) {
		db := DB(t)
		require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))
		require.Regexp(t, cloneNamePattern, name)
		require.True(t, databaseExists(t, name), "the clone should exist while its test runs")
	})

	// Subtest cleanups have run by the time t.Run returns.
	assert.False(t, databaseExists(t, name), "the clone should be dropped when its test ends")
}

func TestDB_CleanupIsNotBlockedByAQueryStillRunning(t *testing.T) {
	// The teardown ordering hazard, stated as a test.
	//
	// sql.DB.Close waits for in-flight queries. If cleanup closed the pool
	// before dropping, the 60-second query below would hold teardown for a
	// minute and the drop would happen — if at all — long after the test was
	// supposedly finished. Dropping first with WITH (FORCE) terminates the
	// clone's backends, and Close then finds only dead connections.
	//
	// Deliberately NOT registered as a t.Cleanup: cleanups run last-in-first-out,
	// so anything registered here would run BEFORE testkit's and release the
	// connection, which is what made an earlier version of this test vacuous.
	var name string
	queryReturned := make(chan struct{})

	start := time.Now()
	t.Run("inner", func(t *testing.T) {
		db := DB(t)
		require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))

		queryStarted := make(chan struct{})
		go func() {
			defer close(queryReturned)
			conn, err := db.Conn(context.Background())
			if err != nil {
				close(queryStarted)
				return
			}
			defer func() { _ = conn.Close() }()
			close(queryStarted)
			// Killed by the FORCE drop; that is the point.
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_sleep(60)`)
		}()
		<-queryStarted

		// Make sure the query is actually executing server-side before the test
		// ends, otherwise this proves nothing.
		WaitFor(t, 10*time.Second, func() (bool, error) {
			admin, err := adminDB()
			if err != nil {
				return false, err
			}
			var running int
			if err := admin.QueryRow(
				`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query LIKE 'SELECT pg_sleep%'`,
				name).Scan(&running); err != nil {
				return false, err
			}
			return running > 0, nil
		}, WithDescription("the long query to be running on the clone"))
	})
	elapsed := time.Since(start)

	assert.False(t, databaseExists(t, name), "the clone should be dropped despite the running query")
	assert.Less(t, elapsed, 30*time.Second,
		"teardown must not wait out the 60s query; it took %s", elapsed)

	select {
	case <-queryReturned:
	case <-time.After(30 * time.Second):
		t.Fatal("the terminated query never returned")
	}
}

func TestDB_ProvisionsTheTemplateWhenItIsMissing(t *testing.T) {
	// The ad-hoc path: someone runs `go test ./internal/foo` without the
	// Makefile having prepared anything. testkit has to notice and provision.
	dropTemplate(t)
	resetTemplateVerification()
	require.False(t, databaseExists(t, TemplateName()))

	db := DB(t)

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users`).Scan(&count))
	assert.True(t, databaseExists(t, TemplateName()), "the template should have been rebuilt on demand")
}

func TestDB_RebuildsTheTemplateWhenTheMigrationsChange(t *testing.T) {
	require.NoError(t, EnsureTemplate(context.Background()))

	// Stand in for "a migration was added since this template was built", and
	// leave a marker so the rebuild can be told apart from a re-stamp.
	withTemplate(t, func(tdb *sql.DB) {
		_, err := tdb.Exec(`UPDATE ` + pq.QuoteIdentifier(stampTable) +
			` SET migrations_hash = 'stale-fingerprint'`)
		require.NoError(t, err)
		_, err = tdb.Exec(`CREATE TABLE testkit_staleness_marker (id int)`)
		require.NoError(t, err)
	})

	resetTemplateVerification()
	db := DB(t)

	var marker sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass('public.testkit_staleness_marker')::text`).Scan(&marker))
	assert.False(t, marker.Valid,
		"a stale template must be dropped and rebuilt, not patched in place")

	want, err := MigrationsHash()
	require.NoError(t, err)
	var stamped string
	require.NoError(t, db.QueryRow(
		`SELECT migrations_hash FROM `+pq.QuoteIdentifier(stampTable)).Scan(&stamped))
	assert.Equal(t, want, stamped, "the rebuilt template must be stamped with the current migrations")
}

func TestTemplateStatus(t *testing.T) {
	_, err := ProvisionTemplate(context.Background(), false)
	require.NoError(t, err)

	exists, current, err := TemplateStatus(context.Background())
	require.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, current)

	dropTemplate(t)
	exists, current, err = TemplateStatus(context.Background())
	require.NoError(t, err)
	assert.False(t, exists)
	assert.False(t, current)

	resetTemplateVerification()
	require.NoError(t, EnsureTemplate(context.Background()))
}

func TestMigrationsHash_IsStable(t *testing.T) {
	first, err := MigrationsHash()
	require.NoError(t, err)
	second, err := MigrationsHash()
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Len(t, first, 64, "expected a hex sha256")
}

func TestHashMigrations_ChangesWithEveryKindOfChange(t *testing.T) {
	// The digest is the only thing standing between an edited migration and a
	// stale template being cloned into every test, so "it returns 64 hex
	// characters" is not the property worth asserting. Each case below is a
	// change that must not go unnoticed.
	base := fstest.MapFS{
		"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
		"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
	}
	baseHash, err := HashMigrations(base)
	require.NoError(t, err)

	t.Run("identical content hashes identically", func(t *testing.T) {
		same := fstest.MapFS{
			"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
		}
		got, err := HashMigrations(same)
		require.NoError(t, err)
		assert.Equal(t, baseHash, got,
			"the walk is lexical, so map ordering must not affect the digest")
	})

	t.Run("edited content changes the digest", func(t *testing.T) {
		edited := fstest.MapFS{
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text NOT NULL);")},
			"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
		}
		got, err := HashMigrations(edited)
		require.NoError(t, err)
		assert.NotEqual(t, baseHash, got)
	})

	t.Run("a renamed file changes the digest", func(t *testing.T) {
		renamed := fstest.MapFS{
			"001_accounts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
			"002_posts.sql":    &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
		}
		got, err := HashMigrations(renamed)
		require.NoError(t, err)
		assert.NotEqual(t, baseHash, got, "goose orders by filename, so a rename is a schema change")
	})

	t.Run("an added migration changes the digest", func(t *testing.T) {
		added := fstest.MapFS{
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
			"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
			"003_votes.sql": &fstest.MapFile{Data: []byte("CREATE TABLE votes (uri text);")},
		}
		got, err := HashMigrations(added)
		require.NoError(t, err)
		assert.NotEqual(t, baseHash, got)
	})

	t.Run("a removed migration changes the digest", func(t *testing.T) {
		removed := fstest.MapFS{
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
		}
		got, err := HashMigrations(removed)
		require.NoError(t, err)
		assert.NotEqual(t, baseHash, got)
	})

	t.Run("content moved across a file boundary changes the digest", func(t *testing.T) {
		// Without the length prefix in the digest, concatenating the files in
		// order would produce the same byte stream and the same hash.
		shifted := fstest.MapFS{
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);CREATE")},
			"002_posts.sql": &fstest.MapFile{Data: []byte(" TABLE posts (uri text);")},
		}
		got, err := HashMigrations(shifted)
		require.NoError(t, err)
		assert.NotEqual(t, baseHash, got)
	})

	t.Run("non-sql files are ignored", func(t *testing.T) {
		withNoise := fstest.MapFS{
			"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);")},
			"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);")},
			"embed.go":      &fstest.MapFile{Data: []byte("package migrations")},
			"README.md":     &fstest.MapFile{Data: []byte("# migrations")},
		}
		got, err := HashMigrations(withNoise)
		require.NoError(t, err)
		assert.Equal(t, baseHash, got,
			"embed.go sits in the migrations directory and changes for unrelated reasons")
	})
}

func TestSweepOrphanClones(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, EnsureTemplate(ctx))

	// An orphan: a clone name whose embedded timestamp is two hours old, the
	// shape left behind by a test binary that was killed before its cleanup.
	orphan := fmt.Sprintf("%s%s_%s_orphan",
		ClonePrefix,
		strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 36),
		RunPrefix())
	createDatabase(t, orphan)
	t.Cleanup(func() { _ = dropDatabase(ctx, orphan) })

	// A clone from a run that is still going: same prefix, current timestamp.
	// Dropping this one out from under a live test is the failure the age
	// bound exists to prevent.
	live := newCloneName()
	createDatabase(t, live)
	t.Cleanup(func() { _ = dropDatabase(ctx, live) })

	result, err := SweepOrphanClones(ctx, time.Hour)
	require.NoError(t, err)

	assert.Contains(t, result.Dropped, orphan)
	assert.NotContains(t, result.Dropped, live)
	assert.False(t, databaseExists(t, orphan))
	assert.True(t, databaseExists(t, live), "a clone younger than the cutoff must survive")
}

func TestSweepOrphanClones_SkipsAnOldCloneThatIsStillInUse(t *testing.T) {
	// Age is evidence of orphanhood, not proof of it. A long-running E2E test
	// legitimately holds its clone for over an hour, and killing that database
	// mid-run would produce a baffling failure in a suite this sweep has no
	// business touching. So an old clone with a live session is left alone —
	// and the drop deliberately omits WITH (FORCE), which would terminate
	// exactly the sessions whose existence is the evidence.
	ctx := context.Background()

	busy := fmt.Sprintf("%s%s_%s_busy",
		ClonePrefix,
		strconv.FormatInt(time.Now().Add(-3*time.Hour).Unix(), 36),
		RunPrefix())
	createDatabase(t, busy)
	t.Cleanup(func() { _ = dropDatabase(ctx, busy) })

	held, err := openDatabase(busy, 1)
	require.NoError(t, err)
	conn, err := held.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = held.Close()
	})
	require.NoError(t, conn.PingContext(ctx))

	result, err := SweepOrphanClones(ctx, time.Hour)
	require.NoError(t, err, "one undroppable clone must not fail the sweep")

	assert.NotContains(t, result.Dropped, busy)
	assert.Contains(t, result.Skipped, busy, "an in-use clone should be reported, not dropped")
	assert.True(t, databaseExists(t, busy))
}

func TestSweepOrphanClones_KeepsGoingAfterAFailure(t *testing.T) {
	// The gate must not turn red because one leftover database refused to go.
	// A clone that acquires a session between the idle check and the DROP is
	// the realistic version of this; it is simulated here by holding a session
	// open, which makes the un-FORCEd drop fail if the check is bypassed.
	ctx := context.Background()

	dropped := fmt.Sprintf("%s%s_%s_ok",
		ClonePrefix, strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 36), RunPrefix())
	createDatabase(t, dropped)
	t.Cleanup(func() { _ = dropDatabase(ctx, dropped) })

	result, err := SweepOrphanClones(ctx, time.Hour)
	require.NoError(t, err)
	assert.Contains(t, result.Dropped, dropped)
	assert.Empty(t, result.Failed)
}

func TestValidateTemplateName(t *testing.T) {
	const maintenance = "coves_test"

	t.Run("accepts the default and other test-namespaced names", func(t *testing.T) {
		for _, name := range []string{"coves_test_template", "testkit_template", "my_test_db"} {
			assert.NoError(t, validateTemplateName(name, maintenance), "should accept %q", name)
		}
	})

	// The whole point of the rail: this name reaches DROP DATABASE ... WITH
	// (FORCE), and it comes from an environment variable. A typo must not be
	// able to destroy a real database.
	t.Run("refuses names outside the test namespace", func(t *testing.T) {
		for _, name := range []string{
			"coves_dev",    // the development database
			"coves_prod",   // the one that would end a career
			"postgres",     // the maintenance database of last resort
			"template0",    // Postgres' own templates
			"template1",    //
			"coves_test",   // the maintenance database testkit connects to
			"tkclone_abc",  // the per-test clone prefix; the sweep would eat it
			"Coves_Test",   // not a legal lowercase identifier
			"drop table x", // not an identifier at all
			"9test",        // must start with a letter
			"",             //
		} {
			err := validateTemplateName(name, maintenance)
			require.Error(t, err, "validateTemplateName must refuse %q", name)
			assert.Contains(t, strings.ToLower(err.Error()), "template database")
		}
	})

	t.Run("refuses an over-long identifier", func(t *testing.T) {
		assert.Error(t, validateTemplateName("test_"+strings.Repeat("x", 60), maintenance))
	})
}

func TestParallelBudget(t *testing.T) {
	budget, err := ParallelBudget(context.Background())
	require.NoError(t, err)

	// The compose files set max_connections=200, so the budget should be
	// comfortably above one but never unbounded.
	assert.GreaterOrEqual(t, budget, 1)
	assert.LessOrEqual(t, budget, 64)
}

func TestDescribeLockHolders_NamesTheBlockingSession(t *testing.T) {
	// A lock timeout that says only "timed out" leaves the next person with no
	// way to find who is holding it. This is the part of that message that
	// makes it actionable, so it gets a test of its own — the two-minute
	// timeout around it does not.
	ctx := context.Background()

	holding := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = withAdvisoryLock(ctx, false, func(context.Context, *sql.Conn) error {
			close(holding)
			<-released
			return nil
		})
	}()
	<-holding
	defer close(released)

	var description string
	WaitFor(t, 10*time.Second, func() (bool, error) {
		description = describeLockHolders(ctx)
		return description != "", nil
	}, WithDescription("the advisory lock to show up in pg_locks"))

	assert.Contains(t, description, "currently held by")
	assert.Regexp(t, `pid \d+`, description)
}

func TestIsSourceInUse(t *testing.T) {
	assert.True(t, isSourceInUse(&pq.Error{Code: sqlstateObjectInUse}))
	assert.True(t, isSourceInUse(fmt.Errorf("wrapped: %w", &pq.Error{Code: sqlstateObjectInUse})),
		"the classifier must see through wrapping, since cloneTemplate wraps")
	assert.False(t, isSourceInUse(&pq.Error{Code: "42P04"}), "duplicate_database is not retryable")
	assert.False(t, isSourceInUse(errors.New("connection refused")))
}

func TestDropDatabase_RefusesAnythingButAClone(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{
		"coves_dev",
		"coves_test",
		TemplateName(),
		"postgres",
		"tkclone", // the prefix alone, without the trailing underscore
		"",
	} {
		err := dropDatabase(ctx, name)
		require.Error(t, err, "dropDatabase must refuse %q", name)
		assert.Contains(t, err.Error(), "not a testkit clone name")
	}
}

func TestCloneCreatedAt(t *testing.T) {
	name := newCloneName()
	created, ok := cloneCreatedAt(name)
	require.True(t, ok, "newCloneName must produce a sweepable name: %q", name)
	assert.WithinDuration(t, time.Now(), created, 5*time.Second)

	for _, bogus := range []string{
		"coves_dev",
		"tkclone_notatimestamp",               // too few segments
		"tkclone_zzz_" + RunPrefix() + "_1_2", // too many segments
	} {
		_, ok := cloneCreatedAt(bogus)
		assert.False(t, ok, "%q must not be treated as a sweepable clone", bogus)
	}
}

func TestDB_FailsLoudlyWhenPostgresIsUnreachable(t *testing.T) {
	// Missing infrastructure is a failure, not a skip — and the message has to
	// say where testkit looked and how to start it, because "connection
	// refused" on its own has cost more debugging time than any other line in
	// this suite.
	t.Setenv("POSTGRES_TEST_PORT", "1")
	t.Setenv("POSTGRES_TEST_HOST", "127.0.0.1")

	restore := swapSingletons(sync.OnceValue(loadEndpoints), sync.OnceValues(openAdminDB))
	defer restore()

	f := &fakeT{}
	runIsolated(func() { DB(f) })

	require.True(t, f.failed())
	msg := f.message()
	assert.Contains(t, msg, "testkit.DB")
	assert.Contains(t, msg, "127.0.0.1:1")
	assert.Contains(t, msg, "make test", "the failure must say how to start the database")
	assert.NotContains(t, msg, Endpoints().Postgres.Password,
		"credentials must never reach a failure message")
}

func TestTemplateName_PanicsOnAnUnsafeOverride(t *testing.T) {
	// The environment variable reaches DROP DATABASE. Carrying on with a value
	// that names a real database is the one outcome that must be impossible, so
	// the failure is loud and immediate rather than an error some caller might
	// log and ignore.
	t.Setenv("POSTGRES_TEST_TEMPLATE", "coves_dev")

	restore := swapSingletons(sync.OnceValue(loadEndpoints), sync.OnceValues(openAdminDB))
	defer restore()

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = TemplateName()
	}()
	require.NotNil(t, panicked, "TemplateName must not return an unsafe name")
	assert.Contains(t, fmt.Sprint(panicked), `refusing to use "coves_dev" as the template database`)

	// And the provisioning path refuses rather than panicking, so DB(t) reports
	// it as an ordinary test failure.
	_, err := ProvisionTemplate(context.Background(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coves_dev")

	f := &fakeT{}
	runIsolated(func() { DB(f) })
	require.True(t, f.failed())
	assert.Contains(t, f.message(), "coves_dev")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func databaseExists(t *testing.T, name string) bool {
	t.Helper()
	db, err := adminDB()
	require.NoError(t, err)
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists))
	return exists
}

func createDatabase(t *testing.T, name string) {
	t.Helper()
	require.Regexp(t, cloneNamePattern, name, "tests only create clone-shaped databases")
	db, err := adminDB()
	require.NoError(t, err)
	_, err = db.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name))
	require.NoError(t, err)
}

// dropTemplate removes the template database directly, UNDER THE EXCLUSIVE
// LOCK.
//
// The lock is not ceremony. Dropping the template is exactly what the lock
// protects against a concurrent clone, and a test helper that skips it works
// only for as long as the suite runs one package at a time — which is a
// property phase 3 deliberately removes. testkit's own tests are the first
// place that regression would appear and the last place anyone would look.
//
// It bypasses the clone-prefix rail in dropDatabase, which is the one thing
// only a test may do.
func dropTemplate(t *testing.T) {
	t.Helper()
	require.NoError(t, withAdvisoryLock(context.Background(), false,
		func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx,
				"DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(TemplateName())+" WITH (FORCE)")
			return err
		}))
}

// withTemplate opens a short-lived connection to the template, under the
// exclusive lock for the same reason dropTemplate takes it: CREATE DATABASE ...
// TEMPLATE refuses to run while any session is connected to the source, so a
// concurrent clone must not be in flight.
func withTemplate(t *testing.T, fn func(*sql.DB)) {
	t.Helper()
	require.NoError(t, withAdvisoryLock(context.Background(), false,
		func(ctx context.Context, conn *sql.Conn) error {
			db, err := openDatabase(TemplateName(), 1)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			fn(db)
			return nil
		}))
}
