// Package manifest reads and writes the per-revision listing that maps a
// serving path to the content-addressed blob behind it.
//
// The format is a zstd-compressed TSV: header lines starting with '#', then
// one "<path>\t<sha256-hex>\t<size>" record per entry, sorted by path. At the
// scale this project works at - a few hundred entries per revision, tens of
// kilobytes compressed - lookup is never the bottleneck, so a text format
// wins on every other axis: it greps, it diffs between two revisions, and it
// stays readable in ten years without a tool. A database would charge a whole
// SQL engine for what is a map[string]Entry.
package manifest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// FormatVersion is the only manifest layout this build understands.
const FormatVersion = 1

// ErrUnsupportedVersion reports a manifest written by a different format.
var ErrUnsupportedVersion = errors.New("manifest: unsupported format version")

const (
	sha256HexLen = 64
	// maxLineLen bounds a single record. Debian paths are nowhere near this,
	// but an unbounded scanner on untrusted input is a bad habit.
	maxLineLen = 64 * 1024
)

// Entry is one file of a publication.
type Entry struct {
	Path   string
	SHA256 string
	Size   int64
}

// Meta is the header block of a manifest.
type Meta struct {
	Repo      string
	Revision  string
	CreatedAt time.Time
}

// Manifest is one immutable revision of one repo, held in memory.
type Manifest struct {
	Meta

	entries map[string]Entry
	order   []string
	bytes   int64
}

// Len reports how many entries the revision holds.
func (m *Manifest) Len() int { return len(m.entries) }

// Bytes reports the total size of the entries, uncompressed.
func (m *Manifest) Bytes() int64 { return m.bytes }

// Lookup resolves a serving path to its blob.
func (m *Manifest) Lookup(path string) (Entry, bool) {
	e, ok := m.entries[path]
	return e, ok
}

// LookupDigest finds an entry by digest rather than by path, which is what a
// by-hash request needs.
//
// It scans rather than keeping a reverse index: a few hundred entries per
// revision make the scan free, while a second map per revision would cost real
// heap across every retained revision of every repo.
func (m *Manifest) LookupDigest(hash string) (Entry, bool) {
	for _, path := range m.order {
		if e := m.entries[path]; e.SHA256 == hash {
			return e, true
		}
	}
	return Entry{}, false
}

// All iterates the entries in path order.
func (m *Manifest) All() iter.Seq[Entry] {
	return func(yield func(Entry) bool) {
		for _, path := range m.order {
			if !yield(m.entries[path]) {
				return
			}
		}
	}
}

// IsMetadata reports whether a serving path lives under a dists/ tree.
//
// Metadata is the only mutable part of a Debian repository, which is why it
// resolves strictly against the current revision while pool paths resolve
// against the whole retained window.
func IsMetadata(path string) bool {
	return strings.HasPrefix(path, "dists/") || strings.Contains(path, "/dists/")
}

// Read parses a manifest, rejecting it whole rather than accepting part of it.
// A truncated, unsorted or otherwise damaged manifest must fail at load time:
// a partially applied revision would serve 404s for paths that do exist.
func Read(r io.Reader) (*Manifest, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("manifest: zstd: %w", err)
	}
	defer dec.Close()

	m := &Manifest{entries: map[string]Entry{}}
	var (
		sawVersion bool
		sawRecord  bool
		prevPath   string
		lineNo     int
	)

	scanner := bufio.NewScanner(dec)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineLen)
	for scanner.Scan() {
		line := scanner.Text()
		lineNo++
		if line == "" {
			return nil, fmt.Errorf("manifest: line %d: empty line", lineNo)
		}

		if strings.HasPrefix(line, "#") {
			if sawRecord {
				return nil, fmt.Errorf("manifest: line %d: header after the first record", lineNo)
			}
			if err := m.parseHeader(line, &sawVersion); err != nil {
				return nil, fmt.Errorf("manifest: line %d: %w", lineNo, err)
			}
			continue
		}

		if !sawVersion {
			return nil, fmt.Errorf("manifest: line %d: %w: missing format_version header",
				lineNo, ErrUnsupportedVersion)
		}

		entry, err := parseRecord(line)
		if err != nil {
			return nil, fmt.Errorf("manifest: line %d: %w", lineNo, err)
		}
		if sawRecord && entry.Path <= prevPath {
			return nil, fmt.Errorf("manifest: line %d: path %q is not after %q",
				lineNo, entry.Path, prevPath)
		}

		m.entries[entry.Path] = entry
		m.order = append(m.order, entry.Path)
		m.bytes += entry.Size
		prevPath = entry.Path
		sawRecord = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("manifest: read: %w", err)
	}
	if !sawVersion {
		return nil, fmt.Errorf("%w: missing format_version header", ErrUnsupportedVersion)
	}
	return m, nil
}

func (m *Manifest) parseHeader(line string, sawVersion *bool) error {
	key, value, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), " ")
	value = strings.TrimSpace(value)
	if key == "" {
		return errors.New("malformed header")
	}

	switch key {
	case "format_version":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%w: %q", ErrUnsupportedVersion, value)
		}
		if v != FormatVersion {
			return fmt.Errorf("%w: %d", ErrUnsupportedVersion, v)
		}
		*sawVersion = true
	case "repo":
		m.Repo = value
	case "revision":
		m.Revision = value
	case "created_at":
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return fmt.Errorf("malformed created_at %q: %w", value, err)
		}
		m.CreatedAt = t
	default:
		// Unknown headers are ignored so that a future writer can add
		// informational fields without breaking older readers. The format
		// version guards anything that changes meaning.
	}
	return nil
}

func parseRecord(line string) (Entry, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 3 {
		return Entry{}, fmt.Errorf("got %d columns, want 3", len(fields))
	}
	path, digest, rawSize := fields[0], fields[1], fields[2]

	if path == "" {
		return Entry{}, errors.New("empty path")
	}
	if err := validateDigest(digest); err != nil {
		return Entry{}, err
	}
	size, err := strconv.ParseInt(rawSize, 10, 64)
	if err != nil {
		return Entry{}, fmt.Errorf("malformed size %q: %w", rawSize, err)
	}
	if size < 0 {
		return Entry{}, fmt.Errorf("negative size %d", size)
	}
	return Entry{Path: path, SHA256: digest, Size: size}, nil
}

// validateDigest insists on lowercase hex so that the manifest, the blob key
// and the ETag are the same string everywhere.
func validateDigest(digest string) error {
	if len(digest) != sha256HexLen {
		return fmt.Errorf("digest %q is %d characters, want %d", digest, len(digest), sha256HexLen)
	}
	for i := range len(digest) {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("digest %q is not lowercase hex", digest)
		}
	}
	return nil
}
