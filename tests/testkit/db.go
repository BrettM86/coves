package testkit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"Coves/internal/db/migrations"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// Database isolation: one throwaway clone of a migrated template per test.
//
// WHY NOT A SHARED DATABASE
//
// The suite this replaces shared one database across every test and cleaned up
// by issuing unscoped DELETEs — 331 of them at last count. That made `-p 1`
// mandatory (packages running concurrently deleted each other's fixtures) and
// made every test's correctness depend on every other test's cleanup
// discipline. Cleanup discipline is exactly what failed.
//
// Cloning a pre-migrated template costs ~30ms and makes isolation a property of
// the construction rather than of the test author's care.
//
// THE THREE-PARTY PROTOCOL
//
// Three things provision the template, and they must not race:
//
//  1. scripts/test-db-prepare.sh, run by `make test` and scripts/ci-runner.sh
//     before any test binary starts. This is the primary path.
//  2. This file's EnsureTemplate, as a fallback for ad-hoc `go test ./...`
//     invocations that never went through the Makefile.
//  3. Other test binaries doing (2) concurrently — `go test ./...` runs each
//     package as its own process, several at a time.
//
// A Postgres advisory lock serialises them. Provisioning takes it exclusively;
// cloning takes it shared. A clone therefore cannot start while the template is
// being dropped and rebuilt, and any number of clones can proceed at once.
//
// Advisory locks are scoped to the database the session is connected to, so
// every participant must connect to the same maintenance database
// (POSTGRES_TEST_DB, default coves_test) — which is also the only database from
// which CREATE DATABASE / DROP DATABASE are issued.

const (
	// defaultTemplateDB is the migrated database every test clone is copied
	// from. Override with POSTGRES_TEST_TEMPLATE — subject to
	// validateTemplateName, because this name is an argument to DROP DATABASE.
	defaultTemplateDB = "coves_test_template"

	// ClonePrefix marks a database as a testkit clone. Nothing outside this
	// prefix (and the validated template name) is ever dropped by testkit or by
	// scripts/test-db-prepare.sh — the sweep runs against a live Postgres that
	// also holds the dev database, so "drop anything that looks unused" is not
	// an acceptable rule.
	ClonePrefix = "tkclone_"

	// PrivateTemplatePrefix marks a throwaway template database created by
	// testkit's own tests, which need to drop and rebuild a template without
	// pulling the shared one out from under another test binary.
	//
	// It is a SEPARATE prefix rather than a clone name because the two are
	// dropped under different rules — and it is a sweepable one because a
	// killed run would otherwise leave a fully migrated database behind
	// forever: nothing else names it, and the clone sweep would not look at it.
	// It contains "test" so that validateTemplateName accepts it.
	PrivateTemplatePrefix = "tktmpl_test_"

	// Advisory lock coordinates. The class id spells "COVE" in ASCII; object
	// ids are allocated here as testkit grows more cluster-wide critical
	// sections. Both halves are needed because Postgres advisory locks are
	// identified by the key alone — any other tool using key (1129530437, 1)
	// against the same database would contend with template provisioning.
	advisoryLockClassID       = 1129530437 // 'C','O','V','E'
	advisoryLockTemplateObjID = 1          // template provisioning / cloning

	// lockAcquireTimeout bounds how long a caller waits for the template lock.
	//
	// Unbounded would be worse than it sounds: a process killed mid-migration
	// releases its lock when its backend exits, but a *wedged* one does not, and
	// every other test binary would then block forever with no output at all.
	// Two minutes comfortably exceeds a full provisioning run (~300ms) while
	// still failing inside any sane test timeout.
	lockAcquireTimeout = 2 * time.Minute

	// clonePoolMaxOpen bounds the connections a single test's pool may open.
	//
	// This is the connection budget from the spec: with a clone per test and
	// tests running in parallel, pools multiply. ConcurrencyBudget derives the
	// safe -parallel value from this and the server's max_connections, and the
	// compose files raise max_connections to match.
	clonePoolMaxOpen = 3
	clonePoolMaxIdle = 2

	// adminPoolMaxOpen bounds the coordination pool. These sessions only take
	// advisory locks and issue CREATE/DROP DATABASE; they run no application
	// SQL, so a small pool is right — and every connection spent here is one
	// unavailable to an actual test.
	adminPoolMaxOpen = 3

	// stampTable records which migration set the template was built from, so a
	// stale template is detected instead of silently used.
	stampTable = "testkit_template_stamp"

	// sqlstateObjectInUse is Postgres' "source database is being accessed by
	// other users" — see cloneTemplate for why it is retried.
	sqlstateObjectInUse = "55006"
)

// cloneNamePattern is the safety rail: every destructive statement testkit
// issues checks its target against this first.
var cloneNamePattern = regexp.MustCompile(`^` + ClonePrefix + `[a-z0-9_]+$`)

var privateTemplateNamePattern = regexp.MustCompile(`^` + PrivateTemplatePrefix + `[a-z0-9_]+$`)

// sweepableFamilies are the database name families testkit creates and may drop
// unattended. Both share the layout newSweepableName writes, so both can be
// aged by name.
var sweepableFamilies = []struct {
	prefix  string
	pattern *regexp.Regexp
}{
	{ClonePrefix, cloneNamePattern},
	{PrivateTemplatePrefix, privateTemplateNamePattern},
}

// templateNamePattern constrains what may be used as a template database name.
var templateNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ---------------------------------------------------------------------------
// Template naming (and the rail around it)
// ---------------------------------------------------------------------------

// templateNameOnce is a process singleton like the others below, and is read
// through resolveTemplateName so testkit's own tests can rebind it safely.
var templateNameOnce = sync.OnceValues(loadTemplateName)

// resolveTemplateName returns the validated template database name.
func resolveTemplateName() (string, error) {
	singletonMu.RLock()
	fn := templateNameOnce
	singletonMu.RUnlock()
	return fn()
}

// TemplateName returns the name of the migrated template database.
//
// It panics if POSTGRES_TEST_TEMPLATE names something testkit must not touch.
// A panic rather than a returned error because this is a configuration mistake
// with a destructive blast radius: the name reaches DROP DATABASE, and the one
// thing that must not happen is quietly carrying on with it.
func TemplateName() string {
	name, err := resolveTemplateName()
	if err != nil {
		panic("testkit: " + err.Error())
	}
	return name
}

func loadTemplateName() (string, error) {
	name := envOr("POSTGRES_TEST_TEMPLATE", defaultTemplateDB)
	if err := validateTemplateName(name, Endpoints().Postgres.Database); err != nil {
		return "", err
	}
	return name, nil
}

// validateTemplateName rejects any template name that testkit must not drop.
//
// The template is the one database outside the clone prefix that testkit
// destroys, and its name comes from the environment. A typo — or a copied
// .env line — setting POSTGRES_TEST_TEMPLATE=coves_dev would otherwise hand
// DROP DATABASE ... WITH (FORCE) the development database, which is precisely
// what the clone-prefix rail exists to make impossible everywhere else.
//
// The "test" substring requirement is the load-bearing part: it confines the
// template to a namespace no real database occupies. The explicit denylist
// below is redundant with it for coves_dev and postgres, and kept anyway
// because a rail worth having is worth stating twice.
func validateTemplateName(name, maintenanceDB string) error {
	switch {
	case name == "":
		return errors.New("template database name is empty")
	case len(name) > 63:
		return fmt.Errorf("template database name %q exceeds Postgres' 63-byte identifier limit", name)
	case !templateNamePattern.MatchString(name):
		return fmt.Errorf("template database name %q must be lowercase alphanumeric with underscores, starting with a letter", name)
	case !strings.Contains(name, "test"):
		return fmt.Errorf("refusing to use %q as the template database: the name must contain \"test\"; "+
			"testkit drops and recreates this database, so it must live in a namespace no real database occupies", name)
	case strings.HasPrefix(name, ClonePrefix):
		return fmt.Errorf("refusing to use %q as the template database: %q is the per-test clone prefix, "+
			"and the orphan sweep would delete it", name, ClonePrefix)
	case name == maintenanceDB:
		return fmt.Errorf("refusing to use %q as the template database: it is the maintenance database "+
			"testkit connects to in order to drop and create databases", name)
	case slices.Contains([]string{"postgres", "template0", "template1", "coves_dev", "coves_prod"}, name):
		return fmt.Errorf("refusing to use %q as the template database: testkit drops and recreates it", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Process singletons
// ---------------------------------------------------------------------------

// The memoised singletons are swapped out by testkit's own tests (to point at
// an unreachable server, to re-read the environment), so they are read through
// a lock rather than directly. Without it `go test -shuffle=on -race` reports a
// data race between a test rebinding one and another goroutine reading it.
var (
	singletonMu   sync.RWMutex
	endpointsOnce = sync.OnceValue(loadEndpoints)
	adminDBOnce   = sync.OnceValues(openAdminDB)
)

func loadedEndpoints() func() EndpointSet {
	singletonMu.RLock()
	defer singletonMu.RUnlock()
	return endpointsOnce
}

func openAdminDB() (*sql.DB, error) {
	pg := Endpoints().Postgres
	db, err := sql.Open("postgres", pg.URL(pg.Database))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", pg.Redacted(pg.Database), err)
	}
	db.SetMaxOpenConns(adminPoolMaxOpen)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func adminDB() (*sql.DB, error) {
	singletonMu.RLock()
	fn := adminDBOnce
	singletonMu.RUnlock()
	return fn()
}

// WaitForPostgres blocks until the maintenance database answers, or the timeout
// expires. Used by scripts/test-db-prepare.sh, which may be invoked moments
// after `docker compose up` returns.
func WaitForPostgres(ctx context.Context, timeout time.Duration) error {
	db, err := adminDB()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = db.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			pg := Endpoints().Postgres
			return fmt.Errorf("test database %s unreachable after %s: %w",
				pg.Redacted(pg.Database), timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Advisory locking
// ---------------------------------------------------------------------------

// withAdvisoryLock runs fn while holding the testkit template lock, shared or
// exclusive, on a dedicated session.
//
// THE SESSION IS DESTROYED, NOT POOLED, ON ANY LOCKING ERROR.
//
// database/sql returns connections to the pool without resetting session state,
// and advisory locks are session-scoped. Two failure modes follow, and both end
// with a pooled connection silently holding a lock that nothing will ever
// release — which manifests, minutes later, as every other test binary blocking
// on the template lock:
//
//   - The acquire appears to fail. Postgres query cancellation is asynchronous,
//     so a context deadline reached while waiting for the lock can return an
//     error to Go after the backend has already been granted the lock.
//   - The release fails, or reports false. pg_advisory_unlock returns a boolean;
//     false means this session did not hold the lock it tried to release, which
//     means the bookkeeping here and in the server have diverged.
//
// Returning driver.ErrBadConn from Conn.Raw discards the connection instead of
// pooling it, which turns both cases into "one wasted connection" — the backend
// exits, and Postgres releases every advisory lock it held.
func withAdvisoryLock(ctx context.Context, shared bool, fn func(context.Context, *sql.Conn) error) error {
	db, err := adminDB()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring admin connection: %w", err)
	}

	lockFn, unlockFn := "pg_advisory_lock", "pg_advisory_unlock"
	if shared {
		lockFn, unlockFn = lockFn+"_shared", unlockFn+"_shared"
	}

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, lockAcquireTimeout)
	_, err = conn.ExecContext(acquireCtx,
		fmt.Sprintf("SELECT %s($1, $2)", lockFn),
		advisoryLockClassID, advisoryLockTemplateObjID)
	cancelAcquire()
	if err != nil {
		discardConn(conn)
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf(
				"timed out after %s waiting for the %s testkit template lock (%d, %d) on %s%s",
				lockAcquireTimeout, lockName(shared),
				advisoryLockClassID, advisoryLockTemplateObjID,
				Endpoints().Postgres.Redacted(Endpoints().Postgres.Database),
				describeLockHolders(ctx))
		}
		return fmt.Errorf("acquiring %s advisory lock: %w", lockName(shared), err)
	}

	var poisoned bool
	defer func() {
		// A fresh context: if ctx was cancelled we still have to release.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		var released bool
		relErr := conn.QueryRowContext(releaseCtx,
			fmt.Sprintf("SELECT %s($1, $2)", unlockFn),
			advisoryLockClassID, advisoryLockTemplateObjID).Scan(&released)
		if relErr != nil || !released || poisoned {
			discardConn(conn)
			return
		}
		_ = conn.Close()
	}()

	if err := fn(ctx, conn); err != nil {
		// The callback runs its statements on this same session; if it failed
		// mid-statement the session's state is not something to hand to the
		// next caller.
		poisoned = true
		return err
	}
	return nil
}

// discardConn removes a session from the pool rather than returning it, so any
// advisory lock it may still hold dies with its backend.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// describeLockHolders makes a lock timeout diagnosable by naming who is holding
// it. Best-effort: it returns "" rather than an error, because it runs on a
// path that is already failing and must not fail differently.
func describeLockHolders(ctx context.Context) string {
	db, err := adminDB()
	if err != nil {
		return ""
	}
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(queryCtx, `
		SELECT a.pid, a.application_name, a.state, coalesce(a.query, '')
		FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = $2 AND l.granted`,
		advisoryLockClassID, advisoryLockTemplateObjID)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()

	var holders []string
	for rows.Next() {
		var pid int
		var app, state, query string
		if err := rows.Scan(&pid, &app, &state, &query); err != nil {
			return ""
		}
		if len(query) > 120 {
			query = query[:120] + "..."
		}
		holders = append(holders, fmt.Sprintf("pid %d (%s, %s): %s", pid, app, state, query))
	}
	if len(holders) == 0 {
		return ""
	}
	return "\n  currently held by:\n    " + strings.Join(holders, "\n    ")
}

func lockName(shared bool) string {
	if shared {
		return "shared"
	}
	return "exclusive"
}

// ---------------------------------------------------------------------------
// Migration fingerprint
// ---------------------------------------------------------------------------

var migrationsHashOnce = sync.OnceValues(func() (string, error) {
	return HashMigrations(migrations.FS)
})

// MigrationsHash fingerprints the AppView's embedded migration set.
//
// The template is stamped with this value, and a mismatch means the template
// predates a migration and must be rebuilt. Hashing the embedded FS rather than
// a directory on disk means the answer is identical from the host, from the CI
// runner, and from any working directory.
func MigrationsHash() (string, error) {
	return migrationsHashOnce()
}

// HashMigrations fingerprints the .sql files in fsys.
//
// Any change to the set — a file's content, its name, one added, one removed —
// must change the digest, because the digest is the only thing standing between
// a changed schema and a stale template being cloned into every test.
func HashMigrations(fsys fs.FS) (string, error) {
	sum := sha256.New()
	// WalkDir visits lexically, so the digest is order-stable.
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		content, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		// Length-prefixed so that moving a byte across the boundary between two
		// files cannot produce the same digest.
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(content)))
		_, _ = io.WriteString(sum, path)
		_, _ = sum.Write(length[:])
		_, _ = sum.Write(content)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hashing migrations: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// Template provisioning
// ---------------------------------------------------------------------------

// ProvisionAction says what a provisioning run did, so callers can report it.
type ProvisionAction string

const (
	ProvisionUpToDate ProvisionAction = "up to date"
	ProvisionCreated  ProvisionAction = "created"
	ProvisionRebuilt  ProvisionAction = "rebuilt (migrations changed)"
	ProvisionForced   ProvisionAction = "rebuilt (forced)"
)

var (
	templateMu       sync.Mutex
	templateVerified bool
)

// EnsureTemplate guarantees that a template database matching the current
// migration set exists, provisioning it if it does not.
//
// It is verified at most once per process — the check costs a round trip and a
// connection to the template, and re-running it per test would serialise the
// suite behind the very lock it takes. Cross-process safety comes from the
// advisory lock, not from this flag.
func EnsureTemplate(ctx context.Context) error {
	templateMu.Lock()
	defer templateMu.Unlock()
	if templateVerified {
		return nil
	}
	if _, err := provisionTemplateLocked(ctx, false); err != nil {
		return err
	}
	templateVerified = true
	return nil
}

// ProvisionTemplate creates or rebuilds the template database and reports what
// it did. With force, the template is rebuilt even if its stamp already matches.
//
// The decision and the action are one operation under one lock acquisition:
// asking whether the template is current, releasing the lock, and then acting on
// the answer would act on a fact that another process may have invalidated in
// between. This is what scripts/test-db-prepare.sh calls.
func ProvisionTemplate(ctx context.Context, force bool) (ProvisionAction, error) {
	templateMu.Lock()
	defer templateMu.Unlock()
	action, err := provisionTemplateLocked(ctx, force)
	if err != nil {
		return action, err
	}
	templateVerified = true
	return action, nil
}

func provisionTemplateLocked(ctx context.Context, force bool) (ProvisionAction, error) {
	want, err := MigrationsHash()
	if err != nil {
		return "", err
	}
	// Re-validated here rather than trusted from the memoised accessor: this is
	// the function that issues DROP DATABASE, and a rail checked at the point
	// of use cannot be bypassed by a future caller that reaches it another way.
	template, err := resolveTemplateName()
	if err != nil {
		return "", err
	}
	if err := validateTemplateName(template, Endpoints().Postgres.Database); err != nil {
		return "", err
	}

	action := ProvisionUpToDate
	lockErr := withAdvisoryLock(ctx, false, func(ctx context.Context, conn *sql.Conn) error {
		exists, current, err := templateState(ctx, conn, template, want)
		if err != nil {
			return err
		}
		switch {
		case force:
			action = ProvisionForced
		case !exists:
			action = ProvisionCreated
		case !current:
			action = ProvisionRebuilt
		default:
			action = ProvisionUpToDate
			return nil
		}

		// Rebuild rather than migrate-in-place: a template whose stamp does not
		// match may have been built from a migration set that no longer exists
		// (a rewritten migration during pre-production development is normal
		// here), and goose cannot undo what it cannot see. Dropping is cheap
		// and always correct.
		//
		// WITH (FORCE) terminates any session still connected to the template.
		// Without it, one leaked connection from a crashed run wedges every
		// subsequent run.
		if _, err := conn.ExecContext(ctx,
			"DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(template)+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("dropping stale template %q: %w", template, err)
		}
		if _, err := conn.ExecContext(ctx,
			"CREATE DATABASE "+pq.QuoteIdentifier(template)); err != nil {
			return fmt.Errorf("creating template %q: %w", template, err)
		}
		return migrateAndStamp(ctx, template, want)
	})
	if lockErr != nil {
		return "", lockErr
	}
	return action, nil
}

// TemplateStatus reports whether the template database exists and whether its
// stamp matches the current migration set.
//
// It takes the lock EXCLUSIVELY even though it only reads: checking the stamp
// means connecting to the template, and CREATE DATABASE ... TEMPLATE refuses to
// run while any session is connected to its source. A shared lock here would
// make a concurrent clone fail with "source database is being accessed by other
// users".
//
// Callers deciding whether to provision should use ProvisionTemplate, which
// decides and acts under one lock acquisition. This exists for reporting.
func TemplateStatus(ctx context.Context) (exists, current bool, err error) {
	want, err := MigrationsHash()
	if err != nil {
		return false, false, err
	}
	template, err := resolveTemplateName()
	if err != nil {
		return false, false, err
	}
	err = withAdvisoryLock(ctx, false, func(ctx context.Context, conn *sql.Conn) error {
		exists, current, err = templateState(ctx, conn, template, want)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return exists, current, nil
}

// templateState reports whether the template exists and carries a stamp
// matching the current migration set. The caller must hold the exclusive lock.
func templateState(ctx context.Context, conn *sql.Conn, template, wantHash string) (exists, current bool, err error) {
	if err := conn.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", template,
	).Scan(&exists); err != nil {
		return false, false, fmt.Errorf("looking up template %q: %w", template, err)
	}
	if !exists {
		return false, false, nil
	}

	// The connection to the template is deliberately short-lived: CREATE
	// DATABASE ... TEMPLATE refuses to run while any session is connected to
	// the source, so holding this open would break every concurrent clone.
	tdb, err := openDatabase(template, 1)
	if err != nil {
		return true, false, err
	}
	defer func() { _ = tdb.Close() }()

	var got string
	scanErr := tdb.QueryRowContext(ctx,
		"SELECT migrations_hash FROM "+pq.QuoteIdentifier(stampTable)+" LIMIT 1").Scan(&got)
	switch {
	case scanErr == nil:
		return true, got == wantHash, nil
	case errors.Is(scanErr, sql.ErrNoRows):
		return true, false, nil
	default:
		// Most likely the stamp table does not exist: a template from before
		// stamping, or a half-provisioned one. Either way, rebuild.
		return true, false, nil
	}
}

// migrateAndStamp runs every migration against a freshly created template and
// records the fingerprint it was built from.
func migrateAndStamp(ctx context.Context, template, hash string) error {
	db, err := openDatabase(template, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// goose.NewProvider rather than the package-level goose.Up: the package
	// API keeps the dialect and base filesystem in global variables, and the
	// legacy tests/integration path still calls goose.Up with a relative
	// directory. Two callers with different notions of where migrations live,
	// sharing one global, is a bug waiting for the first test binary that does
	// both.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("configuring goose for template %q: %w", template, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrating template %q: %w", template, err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE `+pq.QuoteIdentifier(stampTable)+` (
			id             boolean PRIMARY KEY DEFAULT true CHECK (id),
			migrations_hash text        NOT NULL,
			stamped_at      timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("creating stamp table in %q: %w", template, err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO "+pq.QuoteIdentifier(stampTable)+" (migrations_hash) VALUES ($1)", hash); err != nil {
		return fmt.Errorf("stamping template %q: %w", template, err)
	}
	return nil
}

func openDatabase(name string, maxOpen int) (*sql.DB, error) {
	pg := Endpoints().Postgres
	db, err := sql.Open("postgres", pg.URL(name))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", pg.Redacted(name), err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(min(maxOpen, clonePoolMaxIdle))
	return db, nil
}

// ---------------------------------------------------------------------------
// Connection budget
// ---------------------------------------------------------------------------

// connectionHeadroom is the number of connections the budget refuses to spend
// on tests: Postgres' superuser reserve, a developer's psql, a concurrent
// test-db-prepare, and slack for the burst between a clone's pool opening and
// the previous test's pool closing.
//
// It does NOT cover the AppView, which talks to a different server (coves_dev
// on :5435) in every configuration this repository ships.
const connectionHeadroom = 40

// packageParallelism is the `go test -p` value the budget is divided across.
//
// -p and -parallel multiply: N test binaries each running M parallel tests hold
// N × M clone pools at once. The server's max_connections therefore buys a
// fixed number of concurrent test databases, and these two flags only decide
// how that total is split between the dimensions.
//
// It is 1, and the reason is NOT the one that used to be written on `-p 1` in
// the Makefile and the CI runner. That reason — tests/integration wiping shared
// tables — is gone: every test owns a private clone and packages cannot corrupt
// each other's data any more.
//
// The reason now is the ONE resource that stayed shared, and that no amount of
// database isolation can partition: the Jetstream firehose. Running two test
// binaries at once means tests/testkit's firehose tests subscribe while
// tests/integration is creating PDS accounts 25 at a time — and Jetstream's
// wantedCollections filter does not apply to account/identity events, so those
// tests get a stream that is almost entirely other tests' account churn and
// time out waiting for their own commit. Measured, not assumed: on a dev stack
// with an accumulated Jetstream store, the full tagged run failed 2 of 4 times
// at -p 2 and 0 of 4 times at -p 1, with the same -parallel.
//
// So the budget goes to within-package parallelism, which is where this tree's
// wall clock lives anyway — tests/integration alone is most of it. Raise this
// when Phase 4 of docs/TEST_ARCHITECTURE.md deletes tests/integration and its
// ten hand-rolled subscribers, leaving testkit the only firehose consumer.
const packageParallelism = 1

// maxParallelPerPackage keeps a very large max_connections from producing a
// -parallel value far past what the machine can usefully run.
const maxParallelPerPackage = 64

// nestedClonePools is the most testkit.DB clones a SINGLE test may hold open at
// the same time, and therefore the multiplier the budget has to apply to every
// parallel slot.
//
// One is the norm, but a test that calls testkit.DB and then calls it again in
// a subtest holds two pools at once — the outer clone outlives the inner one.
// TestPasswordSecurity in tests/integration does exactly that today. Budgeting
// one pool per slot made the model understate the true peak, so the formula
// could hand out a -parallel whose worst case sat above the ceiling it had just
// computed.
//
// Two costs nothing here, which is why it is the conservative choice rather
// than a tuned one: measured on this tree, -parallel 26 and -parallel 52 finish
// the tagged run within noise of each other (91-95s vs 89-94s), because the
// wall clock is dominated by the serial firehose block, not by how many clones
// run at once. Raise it only if some test starts holding three.
const nestedClonePools = 2

// A Budget is the pair of `go test` concurrency flags a server can support.
type Budget struct {
	Packages int // -p:        test binaries running at once
	Parallel int // -parallel: t.Parallel() tests at once within each binary
}

// Flags renders the budget as the flags themselves, so a caller can splice it
// into a command line without knowing which flag is which.
func (b Budget) Flags() string {
	return fmt.Sprintf("-p %d -parallel %d", b.Packages, b.Parallel)
}

// ConcurrencyBudget returns the largest `go test` concurrency the server's
// max_connections can support, given that every parallel test holds its own
// clone pool and every test binary holds an admin pool.
//
// Without this the connection budget is an unwritten assumption, and the way it
// surfaces is a run that fails with "sorry, too many clients already" in
// whichever test happened to be unlucky — a failure that reads like a bug in
// that test.
func ConcurrencyBudget(ctx context.Context) (Budget, error) {
	db, err := adminDB()
	if err != nil {
		return Budget{}, err
	}
	var raw string
	if err := db.QueryRowContext(ctx, "SHOW max_connections").Scan(&raw); err != nil {
		return Budget{}, fmt.Errorf("reading max_connections: %w", err)
	}
	maxConns, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return Budget{}, fmt.Errorf("parsing max_connections %q: %w", raw, err)
	}
	return budgetFor(maxConns)
}

// budgetFor is the arithmetic on its own, so it can be tested at the sizes that
// matter — including the ones no development server is configured with.
func budgetFor(maxConns int) (Budget, error) {
	// More test binaries than the machine has processors would divide the
	// budget without buying any overlap, so GOMAXPROCS is the ceiling whatever
	// packageParallelism asks for.
	//
	// CAVEAT, latent while packageParallelism is 1: this GOMAXPROCS is the
	// PREPARE process's, and the flags it prints are consumed by a separate
	// `go test` process. They agree today because both run in the same
	// container with the same cgroup limits, and because a ceiling of 1 makes
	// GOMAXPROCS irrelevant. The day packageParallelism rises, a prepare step
	// running somewhere narrower than the test run (or wider) would size the
	// budget for the wrong machine. Read it in the test process — or pass it in
	// — before raising the constant.
	packages := max(1, min(packageParallelism, runtime.GOMAXPROCS(0)))
	available := maxConns - connectionHeadroom - packages*adminPoolMaxOpen
	parallel := available / (packages * nestedClonePools * clonePoolMaxOpen)

	// No floor of 1 here. Clamping an impossible budget up to one would report
	// a limit this server cannot honour and then fail later as "sorry, too many
	// clients already" somewhere unrelated — the exact failure this function
	// exists to prevent. A server too small to run one test says so.
	if parallel < 1 {
		return Budget{}, fmt.Errorf(
			"max_connections=%d cannot support even one test: %d packages need %d admin connections "+
				"plus %d for a single test's clones, and %d are reserved as headroom; "+
				"raise max_connections (the compose files set 200)",
			maxConns, packages, packages*adminPoolMaxOpen,
			nestedClonePools*clonePoolMaxOpen, connectionHeadroom)
	}

	return Budget{
		Packages: packages,
		Parallel: min(parallel, maxParallelPerPackage),
	}, nil
}

// ---------------------------------------------------------------------------
// Per-test clones
// ---------------------------------------------------------------------------

var sweepableCounter = newCounter()

// newSweepableName builds a collision-free, sweepable database name.
//
// Shape: <prefix><created-unix-seconds base36>_<run prefix>_<counter base36>.
// The run prefix is per-process random, so two concurrent `go test` processes
// (or two developers against one Postgres) cannot collide. The timestamp is
// there for the orphan sweep: pg_database records no creation time, so age
// has to be written into the name.
func newSweepableName(prefix string) string {
	return fmt.Sprintf("%s%s_%s_%s",
		prefix,
		strconv.FormatInt(time.Now().Unix(), 36),
		RunPrefix(),
		strconv.FormatUint(sweepableCounter.next(), 36),
	)
}

func newCloneName() string { return newSweepableName(ClonePrefix) }

// DB returns a private, fully migrated Postgres database for this test.
//
// The database is a clone of the migrated template, so it starts with the
// production schema and exactly the rows the migrations seed — which is not
// none: 006 and 025 insert encryption keys, and code that decrypts community
// or aggregator credentials depends on them being there. "Empty" means no rows
// from any other test. It is dropped when the test finishes; nothing
// the test writes is visible to any other test, and no cleanup code is needed
// (or wanted — an explicit DELETE in a test body is a sign the test is still
// assuming a shared database).
//
// A missing or unreachable Postgres fails the test. Tests do not skip on absent
// infrastructure: if the suite was invoked, the infrastructure was requested.
func DB(t TestingT) *sql.DB {
	t.Helper()
	ctx := context.Background()

	if err := EnsureTemplate(ctx); err != nil {
		pg := Endpoints().Postgres
		// resolveTemplateName, not TemplateName: one of the ways EnsureTemplate
		// fails is that the configured template name is unsafe, and TemplateName
		// panics on exactly that. Rendering the failure must not become a
		// second, louder failure that buries the first.
		template, nameErr := resolveTemplateName()
		if nameErr != nil {
			template = "<invalid POSTGRES_TEST_TEMPLATE>"
		}
		t.Fatalf("testkit.DB: no usable template database at %s: %v\n"+
			"  the test database is expected at %s — start it with 'make test' or 'make dev-up'",
			pg.Redacted(template), err, pg.Redacted(pg.Database))
		return nil
	}

	name := newCloneName()
	if err := cloneTemplate(ctx, name); err != nil {
		t.Fatalf("testkit.DB: %v", err)
		return nil
	}

	db, err := openDatabase(name, clonePoolMaxOpen)
	if err != nil {
		_ = dropDatabase(ctx, name)
		t.Fatalf("testkit.DB: %v", err)
		return nil
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_ = dropDatabase(ctx, name)
		t.Fatalf("testkit.DB: pinging clone %q: %v", name, err)
		return nil
	}

	t.Cleanup(func() {
		// DROP FIRST, CLOSE SECOND. sql.DB.Close waits for in-flight queries to
		// finish, so closing first means one goroutine still running a slow
		// query — or one connection a test forgot to release — blocks teardown
		// and the drop never happens. DROP ... WITH (FORCE) terminates the
		// clone's backends outright, after which Close finds only dead
		// connections and returns immediately.
		//
		// Reported, not ignored: a clone that survives its test is a leak, and
		// leaks nobody sees accumulate until Postgres runs out of something.
		// The sweep in test-db-prepare.sh is the backstop for processes that
		// died before reaching here, not a licence to leak.
		if err := dropDatabase(context.Background(), name); err != nil {
			t.Errorf("testkit.DB: leaked test database %q: %v", name, err)
		}
		_ = db.Close()
	})

	return db
}

// cloneTemplate copies the template under a shared advisory lock, so a
// concurrent provisioning run cannot pull the template out from under it.
//
// The retry is not defensive padding. Postgres terminates a backend
// asynchronously, so the short-lived stamp-checking connection that
// templateState opens is still visible in pg_stat_activity for a few
// milliseconds after Close returns — and CREATE DATABASE ... TEMPLATE refuses
// to run while any session is connected to its source. Without this, the first
// testkit.DB call in a package would intermittently fail with SQLSTATE 55006,
// more often on a loaded machine.
func cloneTemplate(ctx context.Context, name string) error {
	if !cloneNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to create %q: not a testkit clone name", name)
	}
	template, err := resolveTemplateName()
	if err != nil {
		return err
	}

	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = withAdvisoryLock(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
			_, execErr := conn.ExecContext(ctx,
				"CREATE DATABASE "+pq.QuoteIdentifier(name)+" TEMPLATE "+pq.QuoteIdentifier(template))
			return execErr
		})
		if lastErr == nil {
			return nil
		}
		if !isSourceInUse(lastErr) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("cloning template %q into %q: %w", template, name, lastErr)
}

// isSourceInUse reports whether err is Postgres declining to copy a database
// because something is connected to it.
func isSourceInUse(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && string(pqErr.Code) == sqlstateObjectInUse
}

// dropDatabase force-drops a testkit clone.
//
// WITH (FORCE) rather than a plain DROP: a test that leaves a connection open —
// a *sql.DB it never closed, a goroutine still querying — would otherwise wedge
// its own teardown, and the failure would surface as a confusing timeout in an
// unrelated later test. The orphan sweep deliberately does NOT use FORCE; see
// SweepOrphanClones.
func dropDatabase(ctx context.Context, name string) error {
	if !cloneNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: not a testkit clone name", name)
	}
	db, err := adminDB()
	if err != nil {
		return err
	}
	dropCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(dropCtx,
		"DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %q: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Orphan sweep
// ---------------------------------------------------------------------------

// SweepResult reports what a sweep did. Skipped and Failed are warnings: a
// sweep is opportunistic cleanup, and neither justifies failing a test run.
type SweepResult struct {
	Dropped []string
	// Skipped holds clones that were old enough to look orphaned but still had
	// sessions connected.
	Skipped []string
	// Failed holds clones whose DROP returned an error, keyed by name.
	Failed map[string]error
}

// SweepOrphanClones drops testkit-created databases that are older than maxAge
// and have no sessions connected: per-test clones AND the private templates
// testkit's own tests build.
//
// Both families are swept because both are created by a process that intends
// to drop them and may not survive to do it. A leaked private template is the
// worse leak of the two — it is a fully migrated database that nothing else
// names, so without this it would sit there until someone noticed by hand.
//
// Orphans come from processes that died between CREATE DATABASE and their
// cleanup: a panicking test binary, a killed `go test`, a docker stop.
//
// TWO RULES MAKE THIS SAFE TO RUN WHILE ANOTHER SUITE IS MID-FLIGHT.
//
// Age is necessary but not sufficient: a long E2E test legitimately holds a
// clone for more than an hour, and killing its database mid-run would produce a
// baffling failure in a suite this one has no business touching. So the sweep
// also requires the clone to be idle, and it does NOT pass WITH (FORCE) —
// FORCE would terminate exactly the sessions whose existence is the evidence
// that the clone is still in use.
//
// The second rule is that one stubborn database must not fail the run. Errors
// are collected and reported; the sweep continues.
func SweepOrphanClones(ctx context.Context, maxAge time.Duration) (SweepResult, error) {
	result := SweepResult{Failed: map[string]error{}}

	db, err := adminDB()
	if err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, family := range sweepableFamilies {
		names, err := databaseNamesWithPrefix(ctx, db, family.prefix)
		if err != nil {
			return result, err
		}
		for _, name := range names {
			created, ok := sweepableCreatedAt(family.prefix, family.pattern, name)
			if !ok {
				// A name that matches the prefix but not the layout is not
				// something testkit created, so it is not something testkit
				// drops.
				continue
			}
			if created.After(cutoff) {
				continue
			}

			busy, err := databaseHasSessions(ctx, db, name)
			if err != nil {
				result.Failed[name] = err
				continue
			}
			if busy {
				result.Skipped = append(result.Skipped, name)
				continue
			}

			if err := dropDatabaseIfIdle(ctx, db, family.pattern, name); err != nil {
				result.Failed[name] = err
				continue
			}
			result.Dropped = append(result.Dropped, name)
		}
	}
	return result, nil
}

func databaseNamesWithPrefix(ctx context.Context, db *sql.DB, prefix string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY datname`,
		prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("listing %s databases: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func databaseHasSessions(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, name).Scan(&count); err != nil {
		return false, fmt.Errorf("checking sessions on %q: %w", name, err)
	}
	return count > 0, nil
}

// dropDatabaseIfIdle drops a clone WITHOUT FORCE, so a session that connected
// between the idle check and here makes the drop fail rather than be killed.
func dropDatabaseIfIdle(ctx context.Context, db *sql.DB, pattern *regexp.Regexp, name string) error {
	if !pattern.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: not a sweepable testkit database name", name)
	}
	dropCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(dropCtx,
		"DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(name)); err != nil {
		return fmt.Errorf("dropping %q: %w", name, err)
	}
	return nil
}

// sweepableCreatedAt recovers the creation time newSweepableName encoded into
// the name.
func sweepableCreatedAt(prefix string, pattern *regexp.Regexp, name string) (time.Time, bool) {
	if !pattern.MatchString(name) {
		return time.Time{}, false
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "_")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(parts[0], 36, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}
