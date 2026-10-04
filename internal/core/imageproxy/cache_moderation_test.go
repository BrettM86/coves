package imageproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiskCache_PurgeOwnerAndCIDAcrossPresets(t *testing.T) {
	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	const (
		ownerA   = "did:plc:cacheownera"
		ownerB   = "did:plc:cacheownerb"
		cid      = "bafyreimoderatedimage"
		otherCID = "bafyreiunrelatedimage"
	)
	for _, entry := range []struct{ preset, did, cid string }{
		{"avatar", ownerA, cid}, {"banner", ownerA, cid},
		{"avatar", ownerB, cid}, {"banner", ownerB, cid},
		{"avatar", ownerA, otherCID},
	} {
		require.NoError(t, cache.Set(entry.preset, entry.did, entry.cid, []byte("image")))
	}
	assertEntry := func(preset, did, cid string, want bool) {
		t.Helper()
		_, found, err := cache.Get(preset, did, cid)
		require.NoError(t, err)
		assert.Equal(t, want, found, "%s %s %s", preset, did, cid)
	}

	require.NoError(t, cache.DeleteOwner(ownerA, cid))
	assertEntry("avatar", ownerA, cid, false)
	assertEntry("banner", ownerA, cid, false)
	assertEntry("avatar", ownerB, cid, true)
	assertEntry("banner", ownerB, cid, true)
	assertEntry("avatar", ownerA, otherCID, true)
	require.NoError(t, cache.DeleteOwner(ownerA, cid), "deletion is idempotent")

	require.NoError(t, cache.Set("banner", ownerA, cid, []byte("reinserted")))
	require.NoError(t, cache.DeleteCID(cid))
	assertEntry("banner", ownerA, cid, false)
	assertEntry("avatar", ownerB, cid, false)
	assertEntry("banner", ownerB, cid, false)
	assertEntry("avatar", ownerA, otherCID, true)
	require.NoError(t, cache.DeleteCID(cid), "deletion is idempotent")
}

// blockPurgeTarget makes the cache entry path a non-empty directory, which
// os.Remove cannot delete, so a purge sees a real filesystem error there.
func blockPurgeTarget(t *testing.T, cache *DiskCache, preset, did, cid string) {
	t.Helper()
	path := cache.cachePath(preset, did, cid)
	require.NoError(t, os.MkdirAll(filepath.Join(path, "occupied"), 0o755))
}

func TestDiskCache_DeleteCIDContinuesPastFailedEntries(t *testing.T) {
	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	const (
		ownerA = "did:plc:cacheownera"
		ownerB = "did:plc:cacheownerb"
		cid    = "bafyreimoderatedimage"
	)
	// ReadDir sorts entries, so the failing entry is visited first.
	blockPurgeTarget(t, cache, "avatar", ownerA, cid)
	require.NoError(t, cache.Set("avatar", ownerB, cid, []byte("image")))
	require.NoError(t, cache.Set("banner", ownerA, cid, []byte("image")))

	err = cache.DeleteCID(cid)
	require.Error(t, err, "the entry that could not be removed must be reported")
	for _, entry := range []struct{ preset, did string }{{"avatar", ownerB}, {"banner", ownerA}} {
		_, found, getErr := cache.Get(entry.preset, entry.did, cid)
		require.NoError(t, getErr)
		assert.False(t, found, "%s %s must still be purged after an earlier failure", entry.preset, entry.did)
	}
}

func TestDiskCache_DeleteOwnerContinuesPastFailedEntries(t *testing.T) {
	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	const (
		owner = "did:plc:cacheownera"
		cid   = "bafyreimoderatedimage"
	)
	blockPurgeTarget(t, cache, "avatar", owner, cid)
	require.NoError(t, cache.Set("banner", owner, cid, []byte("image")))

	err = cache.DeleteOwner(owner, cid)
	require.Error(t, err, "the entry that could not be removed must be reported")
	_, found, err := cache.Get("banner", owner, cid)
	require.NoError(t, err)
	assert.False(t, found, "later presets must still be purged after an earlier failure")
}

func TestDiskCache_PurgeRemovesInterruptedTemporaryFiles(t *testing.T) {
	const (
		owner = "did:plc:cacheownera"
		cid   = "bafyreimoderatedimage"
	)
	for name, purge := range map[string]func(*DiskCache) error{
		"owner": func(cache *DiskCache) error { return cache.DeleteOwner(owner, cid) },
		"cid":   func(cache *DiskCache) error { return cache.DeleteCID(cid) },
	} {
		t.Run(name, func(t *testing.T) {
			cache, err := NewDiskCache(t.TempDir(), 1, 0)
			require.NoError(t, err)
			require.NoError(t, cache.Set("avatar", owner, cid, []byte("image")))
			temporary := cache.cachePath("avatar", owner, cid) + ".tmp"
			require.NoError(t, os.WriteFile(temporary, []byte("partial image"), 0o644))

			require.NoError(t, purge(cache))
			_, statErr := os.Stat(temporary)
			assert.True(t, os.IsNotExist(statErr), "an interrupted write's bytes must be purged too")
		})
	}
}
