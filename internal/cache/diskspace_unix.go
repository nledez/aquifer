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
	return int64(st.Bavail) * int64(st.Bsize), true
}
