//go:build !linux && !darwin

package imageproxy

import (
	"io/fs"
	"time"
)

// lastAccessTime falls back to the modification time, which is the entry's
// creation time, on platforms the cache is not deployed or developed on. LRU
// eviction there degrades to evicting the oldest entries first.
func lastAccessTime(info fs.FileInfo) time.Time {
	return info.ModTime()
}
