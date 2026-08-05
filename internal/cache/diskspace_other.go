//go:build !unix

package cache

// availableBytes has no portable implementation outside Unix. Reporting
// "unknown" simply skips the startup sanity check.
func availableBytes(string) (int64, bool) { return 0, false }
