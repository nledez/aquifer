package debian

import (
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"iter"
	"path"
	"strconv"
	"strings"
)

// PoolFile is one addressable file of a publication, with the digest the index
// already carries.
type PoolFile struct {
	// Path is relative to the archive root, the way a client requests it.
	Path   string
	SHA256 string
	Size   int64
}

// Packages walks a binary index, yielding one entry per package. Iteration
// stops at the first malformed stanza: an index we cannot fully understand
// would silently publish an incomplete set of packages.
func Packages(r io.Reader) iter.Seq2[PoolFile, error] {
	return func(yield func(PoolFile, error) bool) {
		dec := NewDecoder(r)
		for {
			p, err := dec.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(PoolFile{}, err)
				return
			}

			pf, err := poolFileFromPackage(p)
			if err != nil {
				yield(PoolFile{}, err)
				return
			}
			if !yield(pf, nil) {
				return
			}
		}
	}
}

func poolFileFromPackage(p Paragraph) (PoolFile, error) {
	name := p.Get("Package")
	filename := p.Get("Filename")
	if filename == "" {
		return PoolFile{}, fmt.Errorf("debian: package %q has no Filename", name)
	}
	digest := p.Get("SHA256")
	if digest == "" {
		return PoolFile{}, fmt.Errorf("debian: package %q has no SHA256", name)
	}
	if err := validateDigest(digest); err != nil {
		return PoolFile{}, fmt.Errorf("debian: package %q: %w", name, err)
	}
	rawSize := p.Get("Size")
	if rawSize == "" {
		return PoolFile{}, fmt.Errorf("debian: package %q has no Size", name)
	}
	size, err := strconv.ParseInt(rawSize, 10, 64)
	if err != nil {
		return PoolFile{}, fmt.Errorf("debian: package %q: malformed Size: %w", name, err)
	}
	if size < 0 {
		return PoolFile{}, fmt.Errorf("debian: package %q: negative Size", name)
	}
	return PoolFile{Path: path.Clean(filename), SHA256: digest, Size: size}, nil
}

// Sources walks a source index, yielding one entry per file of each source
// package. Paths are the stanza's Directory joined with each file name.
func Sources(r io.Reader) iter.Seq2[PoolFile, error] {
	return func(yield func(PoolFile, error) bool) {
		dec := NewDecoder(r)
		for {
			p, err := dec.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(PoolFile{}, err)
				return
			}

			files, err := poolFilesFromSource(p)
			if err != nil {
				yield(PoolFile{}, err)
				return
			}
			for _, pf := range files {
				if !yield(pf, nil) {
					return
				}
			}
		}
	}
}

func poolFilesFromSource(p Paragraph) ([]PoolFile, error) {
	name := p.Get("Package")
	dir := p.Get("Directory")
	if dir == "" {
		return nil, fmt.Errorf("debian: source %q has no Directory", name)
	}
	if !p.Has("Checksums-Sha256") {
		return nil, fmt.Errorf("debian: source %q has no Checksums-Sha256", name)
	}

	var out []PoolFile
	for _, line := range strings.Split(p.Get("Checksums-Sha256"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("debian: source %q: checksum entry %q has %d fields, want 3",
				name, line, len(fields))
		}
		digest, rawSize, file := fields[0], fields[1], fields[2]
		if err := validateDigest(digest); err != nil {
			return nil, fmt.Errorf("debian: source %q: %w", name, err)
		}
		size, err := strconv.ParseInt(rawSize, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("debian: source %q: malformed size in %q: %w", name, line, err)
		}
		if size < 0 {
			return nil, fmt.Errorf("debian: source %q: negative size in %q", name, line)
		}
		out = append(out, PoolFile{
			Path:   path.Join(dir, file),
			SHA256: digest,
			Size:   size,
		})
	}
	return out, nil
}

// OpenIndex wraps r in whatever decompressor the index's name calls for.
//
// Unknown compression is an error rather than a silent pass-through: handing
// xz bytes to the deb822 decoder would produce a confusing parse failure far
// from the cause.
func OpenIndex(name string, r io.Reader) (io.ReadCloser, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("debian: %s: %w", name, err)
		}
		return zr, nil
	case strings.HasSuffix(name, ".bz2"):
		return io.NopCloser(bzip2.NewReader(r)), nil
	case strings.HasSuffix(name, ".xz"),
		strings.HasSuffix(name, ".lzma"),
		strings.HasSuffix(name, ".zst"),
		strings.HasSuffix(name, ".zstd"):
		return nil, fmt.Errorf("debian: %s: unsupported compression; publish an uncompressed, "+
			".gz or .bz2 variant of this index", name)
	default:
		return io.NopCloser(r), nil
	}
}

// IndexVariants lists the names of an index, best first, so that a caller can
// pick the cheapest variant a publication actually contains.
func IndexVariants(base string) []string {
	return []string{base, base + ".gz", base + ".bz2"}
}

