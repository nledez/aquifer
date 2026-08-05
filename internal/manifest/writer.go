package manifest

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Writer streams a manifest out. Entries must be added in path order; the
// sorting itself belongs to the caller, or to Write, which does it for you.
//
// Streaming matters less for the 800 entries a real publication holds than
// determinism does: the same content must always produce the same bytes, so
// that two publications of an unchanged repository are indistinguishable.
type Writer struct {
	enc      *zstd.Encoder
	w        io.Writer
	prevPath string
	started  bool
	closed   bool
}

// NewWriter starts a manifest with the given header block.
func NewWriter(w io.Writer, meta Meta) (*Writer, error) {
	// Fixed encoder settings, single-threaded: concurrency changes how blocks
	// are split, and that would leak into the output bytes.
	enc, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("manifest: zstd: %w", err)
	}

	mw := &Writer{enc: enc, w: w}
	header := fmt.Sprintf("# format_version %d\n# repo %s\n# revision %s\n# created_at %s\n",
		FormatVersion, meta.Repo, meta.Revision, meta.CreatedAt.UTC().Format(rfc3339Seconds))
	if _, err := io.WriteString(enc, header); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("manifest: write header: %w", err)
	}
	return mw, nil
}

// rfc3339Seconds drops sub-second precision so that a manifest never carries
// more timestamp resolution than it can round-trip meaningfully.
const rfc3339Seconds = "2006-01-02T15:04:05Z07:00"

// Add appends one entry. It refuses anything that would break the sort order,
// because an unsorted manifest is rejected at load time and would only be
// discovered on the edge, after publication.
func (w *Writer) Add(e Entry) error {
	if w.closed {
		return errors.New("manifest: writer is closed")
	}
	if e.Path == "" {
		return errors.New("manifest: empty path")
	}
	if strings.ContainsAny(e.Path, "\t\n") {
		return fmt.Errorf("manifest: path %q contains a tab or newline", e.Path)
	}
	if err := validateDigest(e.SHA256); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if e.Size < 0 {
		return fmt.Errorf("manifest: negative size %d for %q", e.Size, e.Path)
	}
	if w.started && e.Path <= w.prevPath {
		return fmt.Errorf("manifest: path %q is not after %q", e.Path, w.prevPath)
	}

	var b strings.Builder
	b.Grow(len(e.Path) + sha256HexLen + 24)
	b.WriteString(e.Path)
	b.WriteByte('\t')
	b.WriteString(e.SHA256)
	b.WriteByte('\t')
	b.WriteString(strconv.FormatInt(e.Size, 10))
	b.WriteByte('\n')

	if _, err := io.WriteString(w.enc, b.String()); err != nil {
		return fmt.Errorf("manifest: write entry: %w", err)
	}
	w.prevPath = e.Path
	w.started = true
	return nil
}

// Close flushes the compressed stream.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.enc.Close(); err != nil {
		return fmt.Errorf("manifest: close: %w", err)
	}
	return nil
}

// Write sorts entries and writes a complete manifest. This is the entry point
// publish uses; the result depends only on the content, never on the order the
// caller happened to discover it in.
func Write(w io.Writer, meta Meta, entries []Entry) error {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })

	mw, err := NewWriter(w, meta)
	if err != nil {
		return err
	}
	for _, e := range sorted {
		if err := mw.Add(e); err != nil {
			_ = mw.Close()
			return err
		}
	}
	return mw.Close()
}
