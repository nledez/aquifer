package manifest

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// timestampWidth zero-pads the seconds so revisions keep sorting
// lexicographically in the same order as chronologically.
const timestampWidth = 10

// NewRevision mints a revision identifier for a publication.
//
// The "<unix-seconds>-<short-uuid>" shape sorts lexicographically by time,
// which is what lets the object store list revisions in order without any
// index, while the random suffix keeps two publications made within the same
// second distinct.
func NewRevision(now time.Time) string {
	return fmt.Sprintf("%0*d-%s", timestampWidth, now.Unix(), uuid.NewString()[:8])
}

// ValidRevision reports whether s has the shape NewRevision produces.
func ValidRevision(s string) bool {
	stamp, suffix, ok := strings.Cut(s, "-")
	if !ok || len(stamp) < timestampWidth || suffix == "" {
		return false
	}
	for i := range len(stamp) {
		if stamp[i] < '0' || stamp[i] > '9' {
			return false
		}
	}
	for i := range len(suffix) {
		c := suffix[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
