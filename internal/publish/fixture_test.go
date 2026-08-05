package publish_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// publication builds an archive tree shaped like what "aptly publish" leaves
// behind, with digests that genuinely match the files on disk. Anything less
// would let a parser bug hide behind a fixture that agrees with it.
type publication struct {
	dir   string
	suite string

	// pool maps an archive-relative path to its content.
	pool map[string][]byte
	// extras are files no index mentions, such as an exported signing key.
	extras map[string][]byte

	// byHash adds stale by-hash copies of the binary index.
	byHash bool
	// validUntil, when non-zero, is written into the Release.
	validUntil time.Time
	// signed writes an InRelease instead of a plain Release.
	signed bool
}

func newPublication(t *testing.T) *publication {
	t.Helper()
	return &publication{
		dir:    t.TempDir(),
		suite:  "bookworm",
		pool:   map[string][]byte{},
		extras: map[string][]byte{},
	}
}

func (p *publication) addPackage(name string, body []byte) string {
	path := fmt.Sprintf("pool/main/%s/%s/%s_1.0_amd64.deb", name[:1], name, name)
	p.pool[path] = body
	return path
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// write lays the tree out on disk and returns the archive root.
func (p *publication) write(t *testing.T) string {
	t.Helper()

	for path, body := range p.pool {
		writeFile(t, filepath.Join(p.dir, path), body)
	}
	for path, body := range p.extras {
		writeFile(t, filepath.Join(p.dir, path), body)
	}

	packages := p.packagesIndex()
	indexDir := filepath.Join("dists", p.suite, "main", "binary-amd64")
	writeFile(t, filepath.Join(p.dir, indexDir, "Packages"), packages)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(packages); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	writeFile(t, filepath.Join(p.dir, indexDir, "Packages.gz"), gz.Bytes())

	if p.byHash {
		// aptly keeps superseded index versions under by-hash so that a
		// client holding a slightly stale Release can still fetch them.
		writeFile(t, filepath.Join(p.dir, indexDir, "by-hash", "SHA256", digest(packages)), packages)
		stale := append([]byte("Package: stale\n"), packages...)
		writeFile(t, filepath.Join(p.dir, indexDir, "by-hash", "SHA256", digest(stale)), stale)
	}

	release := p.release(map[string][]byte{
		"main/binary-amd64/Packages":    packages,
		"main/binary-amd64/Packages.gz": gz.Bytes(),
	})
	name := "Release"
	if p.signed {
		name = "InRelease"
		release = clearsign(release)
	}
	writeFile(t, filepath.Join(p.dir, "dists", p.suite, name), release)

	return p.dir
}

// packagesIndex renders a binary index carrying the digests of the pool files.
func (p *publication) packagesIndex() []byte {
	paths := slices.Sorted(maps(p.pool))

	var b strings.Builder
	for _, path := range paths {
		body := p.pool[path]
		name := strings.TrimSuffix(filepath.Base(path), "_1.0_amd64.deb")
		fmt.Fprintf(&b, "Package: %s\nVersion: 1.0\nArchitecture: amd64\n"+
			"Filename: %s\nSize: %d\nSHA256: %s\n\n",
			name, path, len(body), digest(body))
	}
	return []byte(b.String())
}

func (p *publication) release(indices map[string][]byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Origin: Test\nLabel: Test\nSuite: %s\nCodename: %s\n", p.suite, p.suite)
	fmt.Fprintf(&b, "Date: %s\n", time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC1123))
	if !p.validUntil.IsZero() {
		fmt.Fprintf(&b, "Valid-Until: %s\n", p.validUntil.UTC().Format(time.RFC1123))
	}
	b.WriteString("Architectures: amd64\nComponents: main\n")
	b.WriteString("SHA256:\n")
	for _, name := range slices.Sorted(maps(indices)) {
		fmt.Fprintf(&b, " %s %d %s\n", digest(indices[name]), len(indices[name]), name)
	}
	return []byte(b.String())
}

func clearsign(body []byte) []byte {
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		string(body) +
		"-----BEGIN PGP SIGNATURE-----\n\nnot-a-real-signature\n-----END PGP SIGNATURE-----\n")
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// maps yields a map's keys; slices.Sorted needs an iterator.
func maps[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
