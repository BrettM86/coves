package imageproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"Coves/tests/testkit"
)

// entryIsGone reports whether a cache file has been removed from disk. It is
// the probe the cleanup-job tests wait on. It stats the file rather than
// calling DiskCache.Get so that polling observes the entry without reading it
// or recording an access.
func entryIsGone(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		// Anything other than "not there" is a broken probe, not a delay.
		return false, err
	}
	return false, nil
}

// mustNewDiskCache is a test helper that creates a DiskCache or fails the test
// Uses 0 for TTL (disabled) by default for backward compatibility
func mustNewDiskCache(t *testing.T, basePath string, maxSizeGB int) *DiskCache {
	t.Helper()
	cache, err := NewDiskCache(basePath, maxSizeGB, 0)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}
	return cache
}

func TestDiskCache_SetAndGet(t *testing.T) {
	// Create a temporary directory for the cache
	tmpDir := t.TempDir()

	cache := mustNewDiskCache(t, tmpDir, 1)

	testData := []byte("test image data")
	preset := "thumb"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"

	// Set the data
	err := cache.Set(preset, did, cid, testData)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Get the data back
	data, found, err := cache.Get(preset, did, cid)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !found {
		t.Fatal("Expected data to be found in cache")
	}
	if string(data) != string(testData) {
		t.Errorf("Get returned %q, want %q", string(data), string(testData))
	}
}

func TestDiskCache_GetMissingKey(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	data, found, err := cache.Get("thumb", "did:plc:notexist", "bafynotexist")
	if err != nil {
		t.Fatalf("Get should not error for missing key: %v", err)
	}
	if found {
		t.Error("Expected found to be false for missing key")
	}
	if data != nil {
		t.Error("Expected data to be nil for missing key")
	}
}

func TestDiskCache_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	testData := []byte("data to delete")
	preset := "medium"
	did := "did:plc:todelete"
	cid := "bafyreitodelete"

	// Set data
	err := cache.Set(preset, did, cid, testData)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Verify it exists
	_, found, _ := cache.Get(preset, did, cid)
	if !found {
		t.Fatal("Expected data to exist before delete")
	}

	// Delete
	err = cache.Delete(preset, did, cid)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify it's gone
	_, found, _ = cache.Get(preset, did, cid)
	if found {
		t.Error("Expected data to be gone after delete")
	}
}

func TestDiskCache_DeleteNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	// Deleting a non-existent key should not error
	err := cache.Delete("thumb", "did:plc:notexist", "bafynotexist")
	if err != nil {
		t.Errorf("Delete of non-existent key should not error: %v", err)
	}
}

func TestDiskCache_PathConstruction(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	testData := []byte("path test data")
	preset := "thumb"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"

	err := cache.Set(preset, did, cid, testData)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Verify the path structure: {basePath}/{preset}/{did_safe}/{cid}
	// did_safe should have colons replaced with underscores
	expectedPath := filepath.Join(tmpDir, preset, "did_plc_abc123", cid)
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("Expected cache file at %s to exist", expectedPath)
	}
}

func TestDiskCache_HandlesSpecialCharactersInDID(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	tests := []struct {
		name    string
		did     string
		wantDir string
	}{
		{
			name:    "plc DID with colons",
			did:     "did:plc:abc123",
			wantDir: "did_plc_abc123",
		},
		{
			name:    "web DID with multiple colons",
			did:     "did:web:example.com:user",
			wantDir: "did_web_example.com_user",
		},
		{
			name:    "DID with many segments",
			did:     "did:plc:a:b:c:d",
			wantDir: "did_plc_a_b_c_d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testData := []byte("test data for " + tt.name)
			preset := "thumb"
			cid := "bafytest123"

			err := cache.Set(preset, tt.did, cid, testData)
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}

			expectedPath := filepath.Join(tmpDir, preset, tt.wantDir, cid)
			if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
				t.Errorf("Expected cache file at %s to exist for DID %s", expectedPath, tt.did)
			}

			// Also verify we can read it back
			data, found, err := cache.Get(preset, tt.did, cid)
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}
			if !found {
				t.Error("Expected to find cached data")
			}
			if string(data) != string(testData) {
				t.Errorf("Get returned %q, want %q", string(data), string(testData))
			}
		})
	}
}

func TestDiskCache_DifferentPresetsAreSeparate(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	did := "did:plc:same"
	cid := "bafysame"
	thumbData := []byte("thumbnail data")
	fullData := []byte("full size data")

	// Set different data for different presets
	err := cache.Set("thumb", did, cid, thumbData)
	if err != nil {
		t.Fatalf("Set thumb failed: %v", err)
	}

	err = cache.Set("full", did, cid, fullData)
	if err != nil {
		t.Fatalf("Set full failed: %v", err)
	}

	// Verify they're separate
	data, found, _ := cache.Get("thumb", did, cid)
	if !found {
		t.Fatal("Expected thumb data to be found")
	}
	if string(data) != string(thumbData) {
		t.Errorf("thumb preset returned wrong data: got %q, want %q", string(data), string(thumbData))
	}

	data, found, _ = cache.Get("full", did, cid)
	if !found {
		t.Fatal("Expected full data to be found")
	}
	if string(data) != string(fullData) {
		t.Errorf("full preset returned wrong data: got %q, want %q", string(data), string(fullData))
	}
}

func TestDiskCache_EmptyParametersHandled(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	// Empty preset
	err := cache.Set("", "did:plc:abc", "bafytest", []byte("data"))
	if err == nil {
		t.Error("Expected error when preset is empty")
	}

	// Empty DID
	err = cache.Set("thumb", "", "bafytest", []byte("data"))
	if err == nil {
		t.Error("Expected error when DID is empty")
	}

	// Empty CID
	err = cache.Set("thumb", "did:plc:abc", "", []byte("data"))
	if err == nil {
		t.Error("Expected error when CID is empty")
	}
}

func TestNewDiskCache(t *testing.T) {
	cache, err := NewDiskCache("/some/path", 5, 30)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	if cache == nil {
		t.Fatal("NewDiskCache returned nil")
	}
	if cache.basePath != "/some/path" {
		t.Errorf("basePath = %q, want %q", cache.basePath, "/some/path")
	}
	if cache.maxSizeGB != 5 {
		t.Errorf("maxSizeGB = %d, want %d", cache.maxSizeGB, 5)
	}
	if cache.ttlDays != 30 {
		t.Errorf("ttlDays = %d, want %d", cache.ttlDays, 30)
	}
}

func TestNewDiskCache_Errors(t *testing.T) {
	t.Run("empty base path", func(t *testing.T) {
		_, err := NewDiskCache("", 5, 0)
		if !errors.Is(err, ErrInvalidCacheBasePath) {
			t.Errorf("expected ErrInvalidCacheBasePath, got: %v", err)
		}
	})

	t.Run("zero max size", func(t *testing.T) {
		_, err := NewDiskCache("/some/path", 0, 0)
		if !errors.Is(err, ErrInvalidCacheMaxSize) {
			t.Errorf("expected ErrInvalidCacheMaxSize, got: %v", err)
		}
	})

	t.Run("negative max size", func(t *testing.T) {
		_, err := NewDiskCache("/some/path", -1, 0)
		if !errors.Is(err, ErrInvalidCacheMaxSize) {
			t.Errorf("expected ErrInvalidCacheMaxSize, got: %v", err)
		}
	})

	t.Run("negative TTL", func(t *testing.T) {
		_, err := NewDiskCache("/some/path", 5, -1)
		if err == nil {
			t.Error("expected error for negative TTL")
		}
	})
}

func TestCache_InterfaceImplementation(t *testing.T) {
	// Compile-time check that DiskCache implements Cache
	var _ Cache = (*DiskCache)(nil)
}

func TestDiskCache_GetCacheSize(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	// Empty cache should be 0
	size, err := cache.GetCacheSize()
	if err != nil {
		t.Fatalf("GetCacheSize failed: %v", err)
	}
	if size != 0 {
		t.Errorf("Expected 0 for empty cache, got %d", size)
	}

	// Add some data
	data := make([]byte, 1000) // 1KB
	if err := cache.Set("avatar", "did:plc:test1", "cid1", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := cache.Set("avatar", "did:plc:test2", "cid2", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	size, err = cache.GetCacheSize()
	if err != nil {
		t.Fatalf("GetCacheSize failed: %v", err)
	}
	if size != 2000 {
		t.Errorf("Expected 2000 bytes, got %d", size)
	}
}

func TestDiskCache_EvictLRU(t *testing.T) {
	tmpDir := t.TempDir()
	// Use a very small max size (1 byte) so any data triggers eviction
	cache, err := NewDiskCache(tmpDir, 1, 0) // 1GB but we'll add more than that won't fit
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	// Add some files with different modification times
	data := make([]byte, 100)

	// Create old file
	if err := cache.Set("avatar", "did:plc:old", "cid_old", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	oldPath := cache.cachePath("avatar", "did:plc:old", "cid_old")
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Create new file
	if err := cache.Set("avatar", "did:plc:new", "cid_new", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Cache is under 1GB so eviction shouldn't remove anything
	removed, err := cache.EvictLRU()
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("Expected 0 entries removed (under limit), got %d", removed)
	}

	// Both files should still exist
	if _, found, _ := cache.Get("avatar", "did:plc:old", "cid_old"); !found {
		t.Error("Old entry should still exist")
	}
	if _, found, _ := cache.Get("avatar", "did:plc:new", "cid_new"); !found {
		t.Error("New entry should still exist")
	}
}

func TestDiskCache_CleanExpired(t *testing.T) {
	tmpDir := t.TempDir()
	// TTL of 1 day
	cache, err := NewDiskCache(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := make([]byte, 100)

	// Create fresh file
	if err := cache.Set("avatar", "did:plc:fresh", "cid_fresh", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Create expired file (manually set old mtime)
	if err := cache.Set("avatar", "did:plc:expired", "cid_expired", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	expiredPath := cache.cachePath("avatar", "did:plc:expired", "cid_expired")
	oldTime := time.Now().Add(-48 * time.Hour) // 2 days old, TTL is 1 day
	if err := os.Chtimes(expiredPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Clean expired entries
	removed, err := cache.CleanExpired()
	if err != nil {
		t.Fatalf("CleanExpired failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("Expected 1 expired entry removed, got %d", removed)
	}

	// Fresh file should still exist
	if _, found, _ := cache.Get("avatar", "did:plc:fresh", "cid_fresh"); !found {
		t.Error("Fresh entry should still exist")
	}

	// Expired file should be gone
	if _, found, _ := cache.Get("avatar", "did:plc:expired", "cid_expired"); found {
		t.Error("Expired entry should be removed")
	}
}

func TestDiskCache_CleanExpired_TTLDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	// TTL of 0 = disabled
	cache, err := NewDiskCache(tmpDir, 1, 0)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := make([]byte, 100)

	// Create a file with old mtime
	if err := cache.Set("avatar", "did:plc:old", "cid_old", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	path := cache.cachePath("avatar", "did:plc:old", "cid_old")
	oldTime := time.Now().Add(-365 * 24 * time.Hour) // 1 year old
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Clean expired should do nothing when TTL is disabled
	removed, err := cache.CleanExpired()
	if err != nil {
		t.Fatalf("CleanExpired failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("Expected 0 removed with TTL disabled, got %d", removed)
	}

	// File should still exist
	if _, found, _ := cache.Get("avatar", "did:plc:old", "cid_old"); !found {
		t.Error("Entry should still exist when TTL is disabled")
	}
}

func TestDiskCache_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	// TTL of 1 day
	cache, err := NewDiskCache(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := make([]byte, 100)

	// Create fresh file
	if err := cache.Set("avatar", "did:plc:fresh", "cid_fresh", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Create expired file
	if err := cache.Set("avatar", "did:plc:expired", "cid_expired", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	expiredPath := cache.cachePath("avatar", "did:plc:expired", "cid_expired")
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expiredPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Cleanup should remove expired entry
	removed, err := cache.Cleanup()
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("Expected 1 entry removed, got %d", removed)
	}

	// Fresh file should still exist
	if _, found, _ := cache.Get("avatar", "did:plc:fresh", "cid_fresh"); !found {
		t.Error("Fresh entry should still exist")
	}
}

// statTimes returns the entry's creation time (its modification time) and its
// last access time, as the cache reads them.
func statTimes(t *testing.T, path string) (createdAt, lastAccessedAt time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	return info.ModTime(), lastAccessTime(info)
}

// setEntryTimes backdates an entry: createdAt becomes its modification time and
// lastAccessedAt its access time.
func setEntryTimes(t *testing.T, path string, createdAt, lastAccessedAt time.Time) {
	t.Helper()
	if err := os.Chtimes(path, lastAccessedAt, createdAt); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}
}

func TestDiskCache_GetRecordsAccessWithoutChangingCreationTime(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	if err := cache.Set("avatar", "did:plc:test", "cid1", []byte("test data")); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	path := cache.cachePath("avatar", "did:plc:test", "cid1")
	createdAt := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	setEntryTimes(t, path, createdAt, createdAt)

	if _, found, err := cache.Get("avatar", "did:plc:test", "cid1"); err != nil || !found {
		t.Fatalf("Get: found=%v err=%v, want a hit", found, err)
	}

	gotCreatedAt, gotLastAccessedAt := statTimes(t, path)
	if !gotCreatedAt.Equal(createdAt) {
		t.Errorf("Get changed the creation time the TTL counts from: got %v, want %v", gotCreatedAt, createdAt)
	}
	if time.Since(gotLastAccessedAt) > time.Minute {
		t.Errorf("Get did not record the read: access time is %v old", time.Since(gotLastAccessedAt))
	}
}

// Reading an entry must not extend its life: the TTL counts from when Set
// wrote it, so a deleted account's images stop being served once it passes,
// however often they are requested.
func TestDiskCache_CleanExpired_CountsFromCreationNotLastRead(t *testing.T) {
	tmpDir := t.TempDir()
	const ttlDays = 30
	cache, err := NewDiskCache(tmpDir, 1, ttlDays)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := []byte("image bytes")
	now := time.Now()
	entries := []struct {
		cid       string
		createdAt time.Time
		wantKept  bool
	}{
		{cid: "cid_past_ttl", createdAt: now.AddDate(0, 0, -(ttlDays + 1)), wantKept: false},
		{cid: "cid_within_ttl", createdAt: now.AddDate(0, 0, -(ttlDays - 1)), wantKept: true},
	}
	for _, entry := range entries {
		if err := cache.Set("avatar", "did:plc:reader", entry.cid, data); err != nil {
			t.Fatalf("Set %s failed: %v", entry.cid, err)
		}
		setEntryTimes(t, cache.cachePath("avatar", "did:plc:reader", entry.cid), entry.createdAt, entry.createdAt)

		// Read repeatedly, as a popular image would be.
		for read := 0; read < 3; read++ {
			if _, found, err := cache.Get("avatar", "did:plc:reader", entry.cid); err != nil || !found {
				t.Fatalf("Get %s (read %d): found=%v err=%v, want a hit", entry.cid, read+1, found, err)
			}
		}
	}

	removed, err := cache.CleanExpired()
	if err != nil {
		t.Fatalf("CleanExpired failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("CleanExpired removed %d entries, want 1", removed)
	}

	for _, entry := range entries {
		_, statErr := os.Stat(cache.cachePath("avatar", "did:plc:reader", entry.cid))
		kept := statErr == nil
		if kept != entry.wantKept {
			t.Errorf("%s created %v ago and read just now: kept=%v, want kept=%v",
				entry.cid, now.Sub(entry.createdAt).Round(time.Hour), kept, entry.wantKept)
		}
	}
}

// Eviction removes the least recently READ entry, not the oldest one: the
// entry created first but read just now must survive, and the entry created
// last but not read since must go.
func TestDiskCache_EvictLRU_EvictsLeastRecentlyRead(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1) // 1GB limit

	// Three 400MB entries make 1.2GB, so evicting exactly one brings the cache
	// under the limit. Truncate extends each file sparsely: its reported size
	// is 400MB without writing 400MB to disk.
	const entrySize = 400 * 1024 * 1024
	now := time.Now()
	entries := []struct {
		cid            string
		createdAt      time.Time
		lastAccessedAt time.Time
	}{
		// Oldest entry; Get below reads it now.
		{cid: "cid_created_first_read_now", createdAt: now.Add(-72 * time.Hour), lastAccessedAt: now.Add(-72 * time.Hour)},
		{cid: "cid_read_half_hour_ago", createdAt: now.Add(-48 * time.Hour), lastAccessedAt: now.Add(-30 * time.Minute)},
		// Newest entry, never read since it was written.
		{cid: "cid_created_last_never_read", createdAt: now.Add(-1 * time.Hour), lastAccessedAt: now.Add(-1 * time.Hour)},
	}
	for _, entry := range entries {
		if err := cache.Set("avatar", "did:plc:lru", entry.cid, []byte("image bytes")); err != nil {
			t.Fatalf("Set %s failed: %v", entry.cid, err)
		}
		setEntryTimes(t, cache.cachePath("avatar", "did:plc:lru", entry.cid), entry.createdAt, entry.lastAccessedAt)
	}

	// Read while the entry is still small: Get reads the whole file into
	// memory, and 400MB of it would be a 400MB allocation.
	if _, found, err := cache.Get("avatar", "did:plc:lru", "cid_created_first_read_now"); err != nil || !found {
		t.Fatalf("Get: found=%v err=%v, want a hit", found, err)
	}
	readInfo, err := os.Stat(cache.cachePath("avatar", "did:plc:lru", "cid_created_first_read_now"))
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	readAt := lastAccessTime(readInfo)

	// Truncate resets mtime, so each entry's times are restored after it is
	// enlarged, keeping the access time Get recorded.
	for _, entry := range entries {
		path := cache.cachePath("avatar", "did:plc:lru", entry.cid)
		if err := os.Truncate(path, entrySize); err != nil {
			t.Fatalf("Truncate %s failed: %v", entry.cid, err)
		}
		lastAccessedAt := entry.lastAccessedAt
		if entry.cid == "cid_created_first_read_now" {
			lastAccessedAt = readAt
		}
		setEntryTimes(t, path, entry.createdAt, lastAccessedAt)
	}

	removed, err := cache.EvictLRU()
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("EvictLRU removed %d entries, want 1", removed)
	}

	for _, entry := range entries {
		_, statErr := os.Stat(cache.cachePath("avatar", "did:plc:lru", entry.cid))
		kept := statErr == nil
		wantKept := entry.cid != "cid_created_last_never_read"
		if kept != wantKept {
			t.Errorf("%s: kept=%v, want kept=%v", entry.cid, kept, wantKept)
		}
	}
}

func TestDiskCache_StartCleanupJob(t *testing.T) {
	tmpDir := t.TempDir()
	// Create cache with 1 day TTL
	cache, err := NewDiskCache(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := make([]byte, 100)

	// Create an expired file
	if err := cache.Set("avatar", "did:plc:expired", "cid_expired", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	expiredPath := cache.cachePath("avatar", "did:plc:expired", "cid_expired")
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expiredPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Start cleanup job with very short interval
	cancel := cache.StartCleanupJob(50 * time.Millisecond)
	defer cancel()

	// The eviction is work the background goroutine does, so wait for the
	// eviction rather than for a duration guessed to contain a cycle.
	//
	// The probe stats the file instead of calling Get, so polling observes the
	// entry without reading it or recording an access.
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		return entryIsGone(expiredPath)
	}, testkit.WithDescription("the background cleanup job to evict the expired entry"))

	// And it is gone through the cache API too, not merely off the disk.
	if _, found, _ := cache.Get("avatar", "did:plc:expired", "cid_expired"); found {
		t.Error("Expired entry should have been cleaned up by background job")
	}
}

func TestDiskCache_StartCleanupJob_ZeroInterval(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	// Starting with 0 interval should return a no-op cancel
	cancel := cache.StartCleanupJob(0)
	defer cancel()

	// Should not panic when called
	cancel()
	cancel() // Multiple calls should be safe
}

func TestDiskCache_StartCleanupJob_GracefulShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	// 1-day TTL, so the entry seeded below is expirable and the job's first
	// cycle has something observable to do.
	cache, err := NewDiskCache(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("NewDiskCache failed: %v", err)
	}

	data := make([]byte, 100)
	if err := cache.Set("avatar", "did:plc:expired", "cid_expired", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	expiredPath := cache.cachePath("avatar", "did:plc:expired", "cid_expired")
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expiredPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Start cleanup job
	cancel := cache.StartCleanupJob(10 * time.Millisecond)

	// Cancelling a job that never started running would prove nothing, so wait
	// until a cycle has demonstrably run — the expired entry disappearing is
	// that evidence — and only then shut it down mid-flight. See the note in
	// TestDiskCache_StartCleanupJob for why the probe stats rather than Gets.
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		return entryIsGone(expiredPath)
	}, testkit.WithDescription("the cleanup job to complete a cycle before it is cancelled"))

	// Cancel should not hang or panic
	done := make(chan struct{})
	go func() {
		cancel()
		close(done)
	}()

	select {
	case <-done:
		// Good, cancel returned
	case <-time.After(1 * time.Second):
		t.Error("Cancel took too long, may be stuck")
	}
}
