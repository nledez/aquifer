//go:build unix

package cache

import "syscall"

// availableBytes reports the free space a non-privileged process can use on
// the filesystem holding dir.
func availableBytes(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	// Bsize is int64 on Linux and uint32 on darwin. Doing the arithmetic in
	// uint64 - the type Bavail has on both - converts once, at the end, rather
	// than writing a conversion that is redundant on one platform and required
	// on the other.
	return int64(st.Bavail * uint64(st.Bsize)), true
}
