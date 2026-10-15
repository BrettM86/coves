//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}

// TestReconcile_UpdatesBridgedHandles is the acceptance case: a normal run
// moves a bridged user and a bridged community to their verified handles,
// carries the user's new handle into its OAuth session, purges the cache rows
// for the user's DID and old handle, and prints the totals line.
func TestReconcile_UpdatesBridgedHandles(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	userDID := uniqueDID(t, "user")
	communityDID := uniqueDID(t, "community")
	unrelatedDID := uniqueDID(t, "unrelated")

	seedUser(t, db, userDID, "oldalice.bridge.test", bridgeHost)
	seedOAuthSession(t, db, userDID, "oldalice.bridge.test", bridgeHost)
	seedCommunity(t, db, communityDID, "oldgarden.bridge.test", bridgeHost)
	seedIdentityCacheRow(t, db, userDID, userDID, "cachedalice.bridge.test")
	seedIdentityCacheRow(t, db, "oldalice.bridge.test", userDID, "oldalice.bridge.test")
	seedIdentityCacheRow(t, db, "unrelated.bridge.test", unrelatedDID, "unrelated.bridge.test")

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		userDID:      {handle: "newalice.bridge.test"},
		communityDID: {handle: "newgarden.bridge.test"},
	})
	var out bytes.Buffer
	report, err := reconcile(context.Background(), sweepOptions{Concurrency: 4}, newSweepDependencies(t, db, uncached), &out)

	assert.Equal(t, "newalice.bridge.test", storedUserHandle(t, db, userDID), "users row handle")
	assert.Equal(t, "newgarden.bridge.test", storedCommunityHandle(t, db, communityDID), "communities row handle")
	assert.Equal(t, "newalice.bridge.test", oauthSessionHandle(t, db, userDID), "OAuth session handle")
	assert.False(t, identityCacheHasRow(t, db, userDID), "identity_cache row for the user's DID")
	assert.False(t, identityCacheHasRow(t, db, "oldalice.bridge.test"), "identity_cache row for the user's old handle")
	assert.True(t, identityCacheHasRow(t, db, "unrelated.bridge.test"), "unrelated identity_cache row")

	require.NoError(t, err)
	assert.Equal(t, 2, report.Checked, "Checked")
	assert.Equal(t, 2, report.Updated, "Updated")
	assert.Equal(t, 0, report.Unchanged, "Unchanged")
	assert.Equal(t, 0, report.StillDiffering, "StillDiffering")
	assert.Equal(t, "checked=2 updated=2 unchanged=0 still_differing=0", lastLine(out.String()), "totals line")
	assert.Equal(t, 0, exitStatus(report, err), "exit status")
}

// TestReconcile_LeavesMatchingHandleUnchanged: a bridged user whose stored
// handle already equals its verified handle is counted unchanged.
func TestReconcile_LeavesMatchingHandleUnchanged(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	userDID := uniqueDID(t, "user")
	seedUser(t, db, userDID, "alice.bridge.test", bridgeHost)

	uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
		userDID: {handle: "alice.bridge.test"},
	})
	var out bytes.Buffer
	report, err := reconcile(context.Background(), sweepOptions{Concurrency: 4}, newSweepDependencies(t, db, uncached), &out)

	require.NoError(t, err)
	assert.Equal(t, "alice.bridge.test", storedUserHandle(t, db, userDID), "users row handle")
	assert.Equal(t, 1, report.Checked, "Checked")
	assert.Equal(t, 1, report.Unchanged, "Unchanged")
	assert.Equal(t, 0, report.Updated, "Updated")
	assert.Equal(t, 0, report.StillDiffering, "StillDiffering")
	assert.Equal(t, 0, exitStatus(report, err), "exit status")
}
