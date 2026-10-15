//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nativeHost is a PDS that is not a trusted bridge, so rows on it are native.
const nativeHost = "https://native.test"

// TestRun_ClearsIdentityCacheBeforeResolving: with -clear-identity-cache,
// every identity_cache row is gone before the first fresh resolution, and the
// number deleted is reported.
func TestRun_ClearsIdentityCacheBeforeResolving(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	userDID := uniqueDID(t, "user")
	seedUser(t, db, userDID, "alice.bridge.test", bridgeHost)
	seedIdentityCacheRow(t, db, "first.unrelated.test", uniqueDID(t, "first"), "first.unrelated.test")
	seedIdentityCacheRow(t, db, "second.unrelated.test", uniqueDID(t, "second"), "second.unrelated.test")
	seedIdentityCacheRow(t, db, "third.unrelated.test", uniqueDID(t, "third"), "third.unrelated.test")

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		userDID: {handle: "alice.bridge.test"},
	})
	cacheRowsAtFirstResolve := -1
	uncached.onFirstResolve = func(ctx context.Context, _ string) {
		assert.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_cache`).Scan(&cacheRowsAtFirstResolve),
			"count identity_cache rows at first resolve")
	}
	dependencies := newSweepDependencies(t, db, uncached)
	open := func(context.Context, func(string) string, int) (sweepDependencies, func(), error) {
		return dependencies, func() {}, nil
	}
	getenv := func(string) string { return "" }
	var stdout, stderr bytes.Buffer

	status := run(context.Background(),
		[]string{"-clear-identity-cache", "-concurrency", "4", "-trusted-hosts", bridgeHost},
		getenv, &stdout, &stderr, open)

	assert.Equal(t, 0, cacheRowsAtFirstResolve, "identity_cache rows when the first resolution started")
	assert.Contains(t, strings.Split(stdout.String(), "\n"), "identity_cache_deleted=3", "report lines")
	assert.Equal(t, "checked=1 updated=0 unchanged=1 still_differing=0", lastLine(stdout.String()), "totals line")
	assert.Equal(t, 0, status, "exit status; stderr: %s", stderr.String())
}

// TestRun_TrustedHostsFlagReplacesEnvironment: -trusted-hosts replaces
// TRUSTED_BRIDGE_PDS_HOSTS, so rows on the environment's host are native and
// never resolved, while rows on the flag's host are reconciled.
func TestRun_TrustedHostsFlagReplacesEnvironment(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	nativeUserDID := uniqueDID(t, "native")
	nativeCommunityDID := uniqueDID(t, "nativecomm")
	bridgedDID := uniqueDID(t, "bridged")
	seedUser(t, db, nativeUserDID, "nora.native.test", nativeHost)
	seedCommunity(t, db, nativeCommunityDID, "pond.native.test", nativeHost)
	seedUser(t, db, bridgedDID, "oldalice.bridge.test", bridgeHost)

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		nativeUserDID:      {handle: "newnora.native.test"},
		nativeCommunityDID: {handle: "newpond.native.test"},
		bridgedDID:         {handle: "newalice.bridge.test"},
	})
	dependencies := newSweepDependencies(t, db, uncached)
	caching := &recordingResolver{t: t, inner: dependencies.CachingResolver}
	dependencies.CachingResolver = caching
	dependencies.Trust = nil
	open := func(context.Context, func(string) string, int) (sweepDependencies, func(), error) {
		return dependencies, func() {}, nil
	}
	environment := map[string]string{"TRUSTED_BRIDGE_PDS_HOSTS": nativeHost}
	getenv := func(key string) string { return environment[key] }
	var stdout, stderr bytes.Buffer

	status := run(context.Background(), []string{"-trusted-hosts", bridgeHost}, getenv, &stdout, &stderr, open)

	assert.Equal(t, 0, uncached.callCount(nativeUserDID), "native user resolutions")
	assert.Equal(t, 0, uncached.callCount(nativeCommunityDID), "native community resolutions")
	assert.NotContains(t, caching.seen(), nativeUserDID, "caching resolver identifiers")
	assert.NotContains(t, caching.seen(), nativeCommunityDID, "caching resolver identifiers")
	assert.Equal(t, "nora.native.test", storedUserHandle(t, db, nativeUserDID), "native users row handle")
	assert.Equal(t, "pond.native.test", storedCommunityHandle(t, db, nativeCommunityDID), "native communities row handle")
	assert.Equal(t, "newalice.bridge.test", storedUserHandle(t, db, bridgedDID), "bridged users row handle")
	assert.Equal(t, "checked=1 updated=1 unchanged=0 still_differing=0", lastLine(stdout.String()), "totals line")
	assert.Equal(t, 0, status, "exit status; stderr: %s", stderr.String())
}

// holdIdentityRefreshLock holds did's per-DID refresh lock from a separate
// pool until the returned release runs (also at cleanup). It returns once the
// lock is held.
func holdIdentityRefreshLock(t *testing.T, db *sql.DB, did string) (release func()) {
	t.Helper()
	lock := jetstream.NewPostgresIdentityRefreshLock(openSecondPool(t, db, "reconcile-handles-holder"), 5*time.Second)
	acquired := make(chan struct{})
	released := make(chan struct{})
	holderDone := make(chan struct{})
	var holderErr error
	go func() {
		defer close(holderDone)
		holderErr = lock.WithDIDLock(context.Background(), did, func(context.Context) error {
			close(acquired)
			<-released
			return nil
		})
	}()
	releaseOnce := sync.OnceFunc(func() {
		close(released)
		<-holderDone
		assert.NoError(t, holderErr, "holder of %s's refresh lock", did)
	})
	// Registered after the holder pool, so it runs before that pool closes.
	t.Cleanup(releaseOnce)
	select {
	case <-acquired:
	case <-holderDone:
		require.Failf(t, "hold refresh lock", "the holder returned before acquiring %s's lock: %v", did, holderErr)
	}
	return releaseOnce
}

// TestReconcile_ReportsEachReasonARowStillDiffers: one sweep over rows that
// each fail for a different reason, plus one that updates. Every failed row
// keeps its stored handle and is reported with its reason code.
func TestReconcile_ReportsEachReasonARowStillDiffers(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	type differing struct {
		kind, did, stored, reason string
	}
	resolveErrorDID := uniqueDID(t, "resolveerr")
	unverifiedDID := uniqueDID(t, "unverified")
	mismatchDID := uniqueDID(t, "mismatch")
	conflictDID := uniqueDID(t, "conflict")
	invalidDID := uniqueDID(t, "invalid")
	busyDID := uniqueDID(t, "busy")
	hostedByDID := uniqueDID(t, "hostedby")
	updatableDID := uniqueDID(t, "updatable")
	nativeDID := uniqueDID(t, "native")

	rows := []differing{
		{kind: "user", did: resolveErrorDID, stored: "anna.bridge.test", reason: "resolve-error"},
		{kind: "user", did: unverifiedDID, stored: "ben.bridge.test", reason: "unverified"},
		{kind: "user", did: mismatchDID, stored: "cleo.bridge.test", reason: "subject-mismatch"},
		{kind: "user", did: conflictDID, stored: "dora.bridge.test", reason: "handle-conflict"},
		{kind: "user", did: invalidDID, stored: "erin.bridge.test", reason: "invalid-handle"},
		{kind: "user", did: busyDID, stored: "finn.bridge.test", reason: "lock-busy"},
		{kind: "community", did: hostedByDID, stored: "oldgarden.bridge.test", reason: "hostedby-mismatch"},
	}
	for _, row := range rows {
		if row.kind == "community" {
			seedCommunity(t, db, row.did, row.stored, bridgeHost)
		} else {
			seedUser(t, db, row.did, row.stored, bridgeHost)
		}
	}
	seedUser(t, db, updatableDID, "gina.bridge.test", bridgeHost)
	seedUser(t, db, nativeDID, "newdora.bridge.test", nativeHost)

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		resolveErrorDID: {err: errors.New("scripted resolution failure")},
		unverifiedDID:   {handle: "handle.invalid"},
		mismatchDID:     {handle: "newcleo.bridge.test", subject: uniqueDID(t, "other")},
		conflictDID:     {handle: "newdora.bridge.test"},
		invalidDID:      {handle: "new_erin.bridge.test"},
		busyDID:         {handle: "newfinn.bridge.test"},
		hostedByDID:     {handle: "newgarden.elsewhere.test"},
		updatableDID:    {handle: "newgina.bridge.test"},
	})
	dependencies := newSweepDependencies(t, db, uncached)
	dependencies.SkipHostedByVerification = false
	dependencies.LockWaitTimeout = 300 * time.Millisecond
	releaseBusyLock := holdIdentityRefreshLock(t, db, busyDID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	report, err := reconcile(ctx, sweepOptions{Concurrency: 4}, dependencies, &out)
	releaseBusyLock()

	verifiedByDID := map[string]string{}
	for _, row := range rows {
		if row.kind == "community" {
			assert.Equal(t, row.stored, storedCommunityHandle(t, db, row.did), "%s communities row handle", row.reason)
		} else {
			assert.Equal(t, row.stored, storedUserHandle(t, db, row.did), "%s users row handle", row.reason)
		}
		verified, found := differsLineVerified(out.String(), row.kind, row.did, row.stored, row.reason)
		assert.True(t, found, "differs line for the %s row in report:\n%s", row.reason, out.String())
		verifiedByDID[row.did] = verified
	}
	assert.Equal(t, "-", verifiedByDID[resolveErrorDID], "verified field of the resolve-error line")
	assert.Equal(t, "-", verifiedByDID[unverifiedDID], "verified field of the unverified line")
	assert.Equal(t, "newdora.bridge.test", verifiedByDID[conflictDID], "verified field of the handle-conflict line")
	assert.Equal(t, "newgina.bridge.test", storedUserHandle(t, db, updatableDID), "updatable users row handle")
	assert.Equal(t, "newdora.bridge.test", storedUserHandle(t, db, nativeDID), "native users row handle")

	assert.Equal(t, 8, report.Checked, "Checked")
	assert.Equal(t, 1, report.Updated, "Updated")
	assert.Equal(t, 7, report.StillDiffering, "StillDiffering")
	wantDiffering := map[string]differing{}
	for _, row := range rows {
		wantDiffering[row.did] = row
	}
	gotDiffering := map[string]differing{}
	for _, row := range report.Differing {
		gotDiffering[row.DID] = differing{kind: row.Kind, did: row.DID, stored: row.StoredHandle, reason: row.Reason}
	}
	assert.Equal(t, wantDiffering, gotDiffering, "Differing rows")
	assert.NotZero(t, exitStatus(report, err), "exit status")
}

// TestRun_DryRunWritesNothing: -dry-run reports what it would change, lists
// every row that differs or fails, writes nothing (no row, cache entry or
// session changes, and the caching resolver is never touched) and exits
// non-zero because rows still differ.
func TestRun_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	staleUserDID := uniqueDID(t, "staleuser")
	staleCommunityDID := uniqueDID(t, "stalecomm")
	matchingDID := uniqueDID(t, "matching")
	failingDID := uniqueDID(t, "failing")
	seedUser(t, db, staleUserDID, "oldalice.bridge.test", bridgeHost)
	seedIdentityCacheRow(t, db, staleUserDID, staleUserDID, "oldalice.bridge.test")
	seedOAuthSession(t, db, staleUserDID, "oldalice.bridge.test", bridgeHost)
	seedCommunity(t, db, staleCommunityDID, "oldgarden.bridge.test", bridgeHost)
	seedUser(t, db, matchingDID, "bob.bridge.test", bridgeHost)
	seedUser(t, db, failingDID, "carl.bridge.test", bridgeHost)

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		staleUserDID:      {handle: "newalice.bridge.test"},
		staleCommunityDID: {handle: "newgarden.bridge.test"},
		matchingDID:       {handle: "bob.bridge.test"},
		failingDID:        {err: errors.New("scripted resolution failure")},
	})
	dependencies := newSweepDependencies(t, db, uncached)
	caching := &recordingResolver{t: t}
	dependencies.CachingResolver = caching
	open := func(context.Context, func(string) string, int) (sweepDependencies, func(), error) {
		return dependencies, func() {}, nil
	}
	getenv := func(string) string { return "" }
	var stdout, stderr bytes.Buffer

	status := run(context.Background(), []string{"-dry-run", "-concurrency", "4", "-trusted-hosts", bridgeHost},
		getenv, &stdout, &stderr, open)

	assert.Equal(t, "oldalice.bridge.test", storedUserHandle(t, db, staleUserDID), "stale users row handle")
	assert.Equal(t, "oldgarden.bridge.test", storedCommunityHandle(t, db, staleCommunityDID), "stale communities row handle")
	assert.Equal(t, "bob.bridge.test", storedUserHandle(t, db, matchingDID), "matching users row handle")
	assert.Equal(t, "carl.bridge.test", storedUserHandle(t, db, failingDID), "failing users row handle")
	assert.Equal(t, 1, identityCacheRowCount(t, db), "identity_cache rows")
	cachedDID, cachedHandle := identityCacheRowContents(t, db, staleUserDID)
	assert.Equal(t, staleUserDID, cachedDID, "identity_cache row did")
	assert.Equal(t, "oldalice.bridge.test", cachedHandle, "identity_cache row handle")
	assert.Equal(t, "oldalice.bridge.test", oauthSessionHandle(t, db, staleUserDID), "OAuth session handle")
	assert.Empty(t, caching.seen(), "caching resolver identifiers")

	assert.NotContains(t, stdout.String(), " reason=error\n", "report lines (a reason=error row means the sweep returned an error)")
	assert.Contains(t, strings.Split(stdout.String(), "\n"),
		"differs kind=user did="+staleUserDID+" stored=oldalice.bridge.test verified=newalice.bridge.test reason=-",
		"report lines")
	assert.Equal(t, "checked=4 would_update=2 unchanged=1 still_differing=3", lastLine(stdout.String()), "totals line")
	assert.NotEqual(t, 0, status, "exit status; stderr: %s", stderr.String())
}
