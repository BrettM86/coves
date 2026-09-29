package imageproxy

import (
	"io/fs"
	"syscall"
	"time"
)

// lastAccessTime returns the file's access time, which DiskCache.Get sets on
// every read and EvictLRU orders by.
func lastAccessTime(info fs.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Atim.Unix())
	}
	return info.ModTime()
}
