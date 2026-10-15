//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bridgeHost is the trusted bridge PDS. Rows whose pds_url is this host are
// bridged; the trust list is given in the same full-URL form.
const bridgeHost = "https://bridge.test"

// scriptedAnswer is the uncached resolver's answer for one DID. subject is the
// DID the answer claims to be about; empty means the asked DID.
type scriptedAnswer struct {
	handle  string
	subject string
	err     error
}

// scriptedIdentityResolver is an identity.UncachedResolver answering each DID
// from a fixed script. It records how often each identifier was resolved, and
// runs onFirstResolve (when set before the sweep starts) once, on the first
// Resolve call, before answering it.
type scriptedIdentityResolver struct {
	onFirstResolve func(ctx context.Context, did string)

	mu           sync.Mutex
	answers      map[string]scriptedAnswer
	calls        map[string]int
	firstResolve sync.Once
}

func newScriptedIdentityResolver(answers map[string]scriptedAnswer) *scriptedIdentityResolver {
	return &scriptedIdentityResolver{answers: answers, calls: map[string]int{}}
}

func (r *scriptedIdentityResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	r.firstResolve.Do(func() {
		if r.onFirstResolve != nil {
			r.onFirstResolve(ctx, did)
		}
	})
	r.mu.Lock()
	r.calls[did]++
	answer, ok := r.answers[did]
	r.mu.Unlock()
	if !ok {
		return nil, &identity.ErrNotFound{Identifier: did, Reason: "no scripted answer"}
	}
	if answer.err != nil {
		return nil, answer.err
	}
	subject := answer.subject
	if subject == "" {
		subject = did
	}
	return &identity.Identity{
		DID: subject, Handle: answer.handle, PDSURL: bridgeHost,
		Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
	}, nil
}

func (r *scriptedIdentityResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", errors.New("scripted identity resolver: ResolveHandle is not scripted")
}

func (r *scriptedIdentityResolver) ResolveDID(context.Context, string) (*identity.DIDDocument, error) {
	return nil, errors.New("scripted identity resolver: ResolveDID is not scripted")
}

func (r *scriptedIdentityResolver) Purge(context.Context, string) error { return nil }

func (r *scriptedIdentityResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	r.mu.Lock()
	answer, ok := r.answers[did]
	r.mu.Unlock()
	if !ok {
		return nil, &identity.ErrNotFound{Identifier: did, Reason: "no scripted document"}
	}
	subject := answer.subject
	if subject == "" {
		subject = did
	}
	return &identity.DeclaredIdentity{DID: subject, DeclaredHandle: answer.handle, PDSURL: bridgeHost}, nil
}

// callCount is how many times did was resolved.
func (r *scriptedIdentityResolver) callCount(did string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[did]
}

// newFailingPLCCachingResolver is a real Postgres-backed caching resolver whose
// PLC directory is a loopback server that fails the test on any request. The
// sweep uses the caching resolver only to purge, so nothing should reach it.
func newFailingPLCCachingResolver(t *testing.T, db *sql.DB) identity.Resolver {
	t.Helper()
	plc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Failf(t, "PLC directory contacted", "the caching resolver requested %s %s", r.Method, r.URL.Path)
		http.Error(w, "test PLC directory refuses every request", http.StatusTeapot)
	}))
	t.Cleanup(plc.Close)
	// The hatch lets a stray request reach the failing server instead of being
	// refused by the SSRF guard, so it fails the test rather than an error path.
	config := identity.DefaultConfig(identity.WithPrivateHostsAllowed())
	config.PLCURL = plc.URL
	return identity.NewResolver(db, config)
}

// openSecondPool opens another pool to the same clone as db, tagged with
// applicationName. It is closed before the clone is dropped.
func openSecondPool(t *testing.T, db *sql.DB, applicationName string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var database string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database), "read clone name")
	pg := testkit.Endpoints().Postgres
	pool, err := sql.Open("postgres", pg.URL(database)+"&application_name="+url.QueryEscape(applicationName))
	require.NoError(t, err, "open pool %s on %s", applicationName, pg.Redacted(database))
	t.Cleanup(func() { _ = pool.Close() })
	require.NoError(t, pool.PingContext(ctx), "ping pool %s on %s", applicationName, pg.Redacted(database))
	return pool
}

// sweepLockApplicationName tags the sweep's lock pool, so pg_locks
// observations can tell its backends apart.
const sweepLockApplicationName = "reconcile-handles-lock"

// newSweepDependencies wires a sweep against db: a dedicated lock pool on the
// same clone, the failing-PLC caching resolver, the given uncached resolver,
// trust in bridgeHost only, and hostedBy verification skipped.
func newSweepDependencies(t *testing.T, db *sql.DB, uncached identity.UncachedResolver) sweepDependencies {
	t.Helper()
	return sweepDependencies{
		Database:                 db,
		LockDatabase:             openSecondPool(t, db, sweepLockApplicationName),
		CachingResolver:          newFailingPLCCachingResolver(t, db),
		UncachedResolver:         uncached,
		Trust:                    jetstream.NewBridgeTrust([]string{bridgeHost}),
		SkipHostedByVerification: true,
	}
}

// uniqueDID is a did:plc identifier unique to this test process.
func uniqueDID(t *testing.T, prefix string) string {
	t.Helper()
	return "did:plc:" + testkit.UniqueIDWithPrefix(t, prefix)
}

func seedUser(t *testing.T, db *sql.DB, did, handle, pdsURL string) {
	t.Helper()
	_, err := postgres.NewUserRepository(db).Create(context.Background(), &users.User{DID: did, Handle: handle, PDSURL: pdsURL})
	require.NoError(t, err, "seed user %s", did)
}

func seedCommunity(t *testing.T, db *sql.DB, did, handle, pdsURL string) {
	t.Helper()
	name, _, _ := strings.Cut(handle, ".")
	now := time.Now().UTC()
	_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).Create(context.Background(), &communities.Community{
		DID: did, Handle: handle, Name: name, DisplayName: name, PDSURL: pdsURL,
		OwnerDID: did, CreatedByDID: did, HostedByDID: "did:web:bridge.test",
		Visibility: "public", CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err, "seed community %s", did)
}

// seedIdentityCacheRow stores an unexpired identity_cache row keyed by identifier.
func seedIdentityCacheRow(t *testing.T, db *sql.DB, identifier, did, handle string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO identity_cache (identifier, did, handle, pds_url, resolved_at, resolution_method, expires_at)
		VALUES ($1, $2, $3, $4, NOW(), 'dns', NOW() + INTERVAL '1 day')`,
		identifier, did, handle, bridgeHost)
	require.NoError(t, err, "seed identity_cache row %s", identifier)
}

// seedOAuthSession stores an unexpired OAuth session for did carrying handle.
func seedOAuthSession(t *testing.T, db *sql.DB, did, handle, pdsURL string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO oauth_sessions (did, handle, pds_url, access_token, refresh_token, dpop_private_jwk, auth_server_iss, expires_at, session_id)
		VALUES ($1, $2, $3, 'test-access', 'test-refresh', '{}', 'https://auth.bridge.test', NOW() + INTERVAL '1 day', 'reconcile-session')`,
		did, handle, pdsURL)
	require.NoError(t, err, "seed OAuth session for %s", did)
}

func storedUserHandle(t *testing.T, db *sql.DB, did string) string {
	t.Helper()
	var handle string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT handle FROM users WHERE did = $1`, did).Scan(&handle),
		"read user %s", did)
	return handle
}

func storedCommunityHandle(t *testing.T, db *sql.DB, did string) string {
	t.Helper()
	var handle string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT handle FROM communities WHERE did = $1`, did).Scan(&handle),
		"read community %s", did)
	return handle
}

func oauthSessionHandle(t *testing.T, db *sql.DB, did string) string {
	t.Helper()
	var handle string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT handle FROM oauth_sessions WHERE did = $1`, did).Scan(&handle),
		"read OAuth session for %s", did)
	return handle
}

func identityCacheHasRow(t *testing.T, db *sql.DB, identifier string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM identity_cache WHERE identifier = $1)`, identifier).Scan(&exists),
		"read identity_cache row %s", identifier)
	return exists
}

// lastLine is the final non-empty line of a printed report.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return lines[len(lines)-1]
}

// recordingResolver is an identity.Resolver that records every identifier it
// is asked about and forwards to inner. With a nil inner it fails the test on
// any call.
type recordingResolver struct {
	t     *testing.T
	inner identity.Resolver

	mu          sync.Mutex
	identifiers []string
}

func (r *recordingResolver) record(method, identifier string) error {
	r.mu.Lock()
	r.identifiers = append(r.identifiers, identifier)
	r.mu.Unlock()
	if r.inner == nil {
		assert.Failf(r.t, "caching resolver called", "%s(%s)", method, identifier)
		return errors.New("recording resolver: no inner resolver")
	}
	return nil
}

func (r *recordingResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	if err := r.record("Resolve", identifier); err != nil {
		return nil, err
	}
	return r.inner.Resolve(ctx, identifier)
}

func (r *recordingResolver) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	if err := r.record("ResolveHandle", handle); err != nil {
		return "", "", err
	}
	return r.inner.ResolveHandle(ctx, handle)
}

func (r *recordingResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	if err := r.record("ResolveDID", did); err != nil {
		return nil, err
	}
	return r.inner.ResolveDID(ctx, did)
}

func (r *recordingResolver) Purge(ctx context.Context, identifier string) error {
	if err := r.record("Purge", identifier); err != nil {
		return err
	}
	return r.inner.Purge(ctx, identifier)
}

// seen is every identifier passed to any method, in call order.
func (r *recordingResolver) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.identifiers...)
}

func identityCacheRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM identity_cache`).Scan(&count),
		"count identity_cache rows")
	return count
}

// identityCacheRowContents is the did and handle stored under identifier.
func identityCacheRowContents(t *testing.T, db *sql.DB, identifier string) (did, handle string) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT did, handle FROM identity_cache WHERE identifier = $1`, identifier).Scan(&did, &handle),
		"read identity_cache row %s", identifier)
	return did, handle
}

// differsLineVerified finds the report line for one still-differing row and
// returns its verified field. found is false when no line matches.
func differsLineVerified(output, kind, did, stored, reason string) (verified string, found bool) {
	pattern := regexp.MustCompile(`(?m)^differs kind=` + regexp.QuoteMeta(kind) +
		` did=` + regexp.QuoteMeta(did) +
		` stored=` + regexp.QuoteMeta(stored) +
		` verified=(\S+) reason=` + regexp.QuoteMeta(reason) + `$`)
	match := pattern.FindStringSubmatch(output)
	if match == nil {
		return "", false
	}
	return match[1], true
}
