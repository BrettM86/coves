package imageproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestDiskCache_GetUpdatesMtime(t *testing.T) {
	tmpDir := t.TempDir()
	cache := mustNewDiskCache(t, tmpDir, 1)

	data := []byte("test data")
	if err := cache.Set("avatar", "did:plc:test", "cid1", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	path := cache.cachePath("avatar", "did:plc:test", "cid1")

	// Set an old mtime
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	// Get the file - this should update mtime
	_, found, err := cache.Get("avatar", "did:plc:test", "cid1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !found {
		t.Fatal("Expected to find entry")
	}

	// Check that mtime was updated
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	// Mtime should be recent (within last minute)
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("Expected mtime to be updated to now, but it's %v old", time.Since(info.ModTime()))
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

	// Wait for at least one cleanup cycle
	time.Sleep(100 * time.Millisecond)

	// Expired file should be gone
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
	cache := mustNewDiskCache(t, tmpDir, 1)

	// Start cleanup job
	cancel := cache.StartCleanupJob(10 * time.Millisecond)

	// Let it run briefly
	time.Sleep(30 * time.Millisecond)

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
