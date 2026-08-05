package manifest_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nledez/aquifer/internal/manifest"
)

func testMeta() manifest.Meta {
	return manifest.Meta{
		Repo:      "debian/bookworm",
		Revision:  "1754400000-a1b2c3d4",
		CreatedAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
}

func entry(path, hash string, size int64) manifest.Entry {
	return manifest.Entry{Path: path, SHA256: hash, Size: size}
}

// hashOf builds a syntactically valid but recognisable digest.
func hashOf(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}
	return hex(b)
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func sampleEntries() []manifest.Entry {
	return []manifest.Entry{
		entry("debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", hashOf(0x11), 1_234_567),
		entry("debian/bookworm/dists/bookworm/InRelease", hashOf(0x22), 4096),
		entry("debian/bookworm/dists/bookworm/main/binary-amd64/Packages.gz", hashOf(0x33), 98_765),
		entry("debian/bookworm/pool/main/a/apt/apt_2.6.1_amd64.deb", hashOf(0x44), 90_000_000),
	}
}

func writeManifest(t *testing.T, meta manifest.Meta, entries []manifest.Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := manifest.Write(&buf, meta, entries); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// decompress exposes the plain TSV so tests can assert on the wire format,
// which is the whole reason for choosing a text format over a database.
func decompress(t *testing.T, compressed []byte) string {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()
	plain, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	return string(plain)
}

// compress turns a hand-written TSV into what Read expects, so that tests can
// feed deliberately malformed manifests.
func compress(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := enc.Write([]byte(plain)); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()

	meta := testMeta()
	entries := sampleEntries()
	m, err := manifest.Read(bytes.NewReader(writeManifest(t, meta, entries)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if m.Repo != meta.Repo || m.Revision != meta.Revision {
		t.Fatalf("meta = %q/%q, want %q/%q", m.Repo, m.Revision, meta.Repo, meta.Revision)
	}
	if !m.CreatedAt.Equal(meta.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want %v", m.CreatedAt, meta.CreatedAt)
	}
	if m.Len() != len(entries) {
		t.Fatalf("Len = %d, want %d", m.Len(), len(entries))
	}

	for _, want := range entries {
		got, ok := m.Lookup(want.Path)
		if !ok {
			t.Fatalf("missing entry %q", want.Path)
		}
		if got != want {
			t.Fatalf("entry %q = %+v, want %+v", want.Path, got, want)
		}
	}
	if _, ok := m.Lookup("debian/bookworm/pool/main/z/zzz/absent.deb"); ok {
		t.Fatal("Lookup returned an entry that was never written")
	}
}

// The manifest must be byte-for-byte reproducible: two publications of the
// same content produce the same file, whatever order the entries arrive in.
func TestWriteIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	t.Parallel()

	entries := sampleEntries()
	shuffled := []manifest.Entry{entries[2], entries[0], entries[3], entries[1]}

	first := writeManifest(t, testMeta(), entries)
	second := writeManifest(t, testMeta(), shuffled)

	if !bytes.Equal(first, second) {
		t.Fatal("the same content produced two different manifests")
	}
}

func TestWriteSortsEntriesAndEmitsHeaders(t *testing.T) {
	t.Parallel()

	plain := decompress(t, writeManifest(t, testMeta(), sampleEntries()))
	lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")

	var headers, records []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			headers = append(headers, line)
			continue
		}
		records = append(records, line)
	}

	wantHeaders := []string{
		"# format_version 1",
		"# repo debian/bookworm",
		"# revision 1754400000-a1b2c3d4",
		"# created_at 2026-08-05T12:00:00Z",
	}
	if len(headers) != len(wantHeaders) {
		t.Fatalf("got %d headers, want %d: %q", len(headers), len(wantHeaders), headers)
	}
	for i, want := range wantHeaders {
		if headers[i] != want {
			t.Fatalf("header %d = %q, want %q", i, headers[i], want)
		}
	}

	if len(records) != 4 {
		t.Fatalf("got %d records, want 4", len(records))
	}
	for i := 1; i < len(records); i++ {
		if records[i-1] >= records[i] {
			t.Fatalf("records are not sorted: %q then %q", records[i-1], records[i])
		}
	}

	wantFirst := "debian/bookworm/dists/bookworm/InRelease\t" + hashOf(0x22) + "\t4096"
	if records[0] != wantFirst {
		t.Fatalf("first record = %q, want %q", records[0], wantFirst)
	}
}

// A corrupt manifest must fail at load time rather than being half accepted.
func TestReadRejectsMalformedManifests(t *testing.T) {
	t.Parallel()

	valid := "# format_version 1\n" +
		"# repo debian/bookworm\n" +
		"# revision 1754400000-a1b2c3d4\n" +
		"# created_at 2026-08-05T12:00:00Z\n" +
		"a/one\t" + hashOf(0x11) + "\t10\n" +
		"b/two\t" + hashOf(0x22) + "\t20\n"

	cases := []struct {
		name  string
		plain string
	}{
		{
			name:  "unknown format version",
			plain: strings.Replace(valid, "format_version 1", "format_version 2", 1),
		},
		{
			name:  "missing format version",
			plain: strings.Replace(valid, "# format_version 1\n", "", 1),
		},
		{
			name:  "too few columns",
			plain: valid + "c/three\t" + hashOf(0x33) + "\n",
		},
		{
			name:  "too many columns",
			plain: valid + "c/three\t" + hashOf(0x33) + "\t30\textra\n",
		},
		{
			name:  "non numeric size",
			plain: valid + "c/three\t" + hashOf(0x33) + "\tbig\n",
		},
		{
			name:  "negative size",
			plain: valid + "c/three\t" + hashOf(0x33) + "\t-1\n",
		},
		{
			name:  "digest is not hex",
			plain: valid + "c/three\tzzzz\t30\n",
		},
		{
			name:  "digest has the wrong length",
			plain: valid + "c/three\tabcdef\t30\n",
		},
		{
			// hashOf(0xab) yields letters, so uppercasing actually differs.
			name:  "uppercase digest",
			plain: valid + "c/three\t" + strings.ToUpper(hashOf(0xab)) + "\t30\n",
		},
		{
			name:  "paths out of order",
			plain: valid + "a/zero\t" + hashOf(0x33) + "\t30\n",
		},
		{
			name:  "duplicate path",
			plain: valid + "b/two\t" + hashOf(0x33) + "\t30\n",
		},
		{
			name:  "empty path",
			plain: valid + "\t" + hashOf(0x33) + "\t30\n",
		},
		{
			name:  "header after records",
			plain: valid + "# repo sneaky\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := manifest.Read(bytes.NewReader(compress(t, tc.plain))); err == nil {
				t.Fatalf("Read accepted a manifest with %s", tc.name)
			}
		})
	}
}

func TestReadRejectsUnknownFormatVersionWithATypedError(t *testing.T) {
	t.Parallel()

	plain := "# format_version 99\n# repo r\n# revision 1-a\n# created_at 2026-08-05T12:00:00Z\n"
	_, err := manifest.Read(bytes.NewReader(compress(t, plain)))
	if !errors.Is(err, manifest.ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := manifest.Read(bytes.NewReader([]byte("not zstd at all"))); err == nil {
		t.Fatal("Read accepted data that is not a zstd stream")
	}
}

func TestWriterRejectsOutOfOrderAdds(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w, err := manifest.NewWriter(&buf, testMeta())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Add(entry("b/two", hashOf(0x11), 1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Add(entry("a/one", hashOf(0x22), 2)); err == nil {
		t.Fatal("Add accepted an entry that breaks the sort order")
	}
	_ = w.Close()
}

func TestManifestReportsTotalSize(t *testing.T) {
	t.Parallel()

	m, err := manifest.Read(bytes.NewReader(writeManifest(t, testMeta(), sampleEntries())))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var want int64
	for _, e := range sampleEntries() {
		want += e.Size
	}
	if m.Bytes() != want {
		t.Fatalf("Bytes = %d, want %d", m.Bytes(), want)
	}
}

func TestAllIteratesInSortedOrder(t *testing.T) {
	t.Parallel()

	m, err := manifest.Read(bytes.NewReader(writeManifest(t, testMeta(), sampleEntries())))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var paths []string
	for e := range m.All() {
		paths = append(paths, e.Path)
	}
	if len(paths) != 4 {
		t.Fatalf("iterated %d entries, want 4", len(paths))
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] >= paths[i] {
			t.Fatalf("All is not sorted: %q then %q", paths[i-1], paths[i])
		}
	}
}

// --- revisions --------------------------------------------------------------

func TestRevisionsSortLexicographicallyByTime(t *testing.T) {
	t.Parallel()

	older := manifest.NewRevision(time.Unix(1754400000, 0))
	newer := manifest.NewRevision(time.Unix(1754400001, 0))

	if !(older < newer) {
		t.Fatalf("revision %q should sort before %q", older, newer)
	}
	if older == manifest.NewRevision(time.Unix(1754400000, 0)) {
		t.Fatal("two revisions minted at the same second must still differ")
	}
	if !manifest.ValidRevision(older) {
		t.Fatalf("NewRevision produced an invalid revision %q", older)
	}
}

func TestValidRevisionRejectsJunk(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "nope", "-abc", "1754400000-", "17544/0000-abc", "1754400000_abc"} {
		if manifest.ValidRevision(bad) {
			t.Fatalf("ValidRevision(%q) = true, want false", bad)
		}
	}
}

// --- revision window --------------------------------------------------------

func revManifest(t *testing.T, repo, revision string, entries []manifest.Entry) *manifest.Manifest {
	t.Helper()
	meta := manifest.Meta{Repo: repo, Revision: revision, CreatedAt: time.Unix(0, 0).UTC()}
	m, err := manifest.Read(bytes.NewReader(writeManifest(t, meta, entries)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return m
}

// SPEC section 5: pool paths resolve against the union of retained revisions,
// so that "apt update" on one edge and "apt install" on another do not 404
// during a switchover.
func TestWindowResolvesPoolPathsAgainstRetainedRevisions(t *testing.T) {
	t.Parallel()

	old := revManifest(t, "debian/bookworm", "1754400000-aaaa", []manifest.Entry{
		entry("debian/bookworm/dists/bookworm/InRelease", hashOf(0x01), 100),
		entry("debian/bookworm/pool/main/a/apt/apt_1.0_amd64.deb", hashOf(0x02), 200),
	})
	current := revManifest(t, "debian/bookworm", "1754400100-bbbb", []manifest.Entry{
		entry("debian/bookworm/dists/bookworm/InRelease", hashOf(0x03), 300),
		entry("debian/bookworm/pool/main/a/apt/apt_2.0_amd64.deb", hashOf(0x04), 400),
	})

	w := manifest.NewWindow(5)
	w.Push(old)
	w.Push(current)

	// The superseded package is still reachable.
	got, ok := w.Lookup("debian/bookworm/pool/main/a/apt/apt_1.0_amd64.deb")
	if !ok {
		t.Fatal("a pool path from a retained revision must still resolve")
	}
	if got.SHA256 != hashOf(0x02) {
		t.Fatalf("got digest %s, want %s", got.SHA256, hashOf(0x02))
	}

	if _, ok := w.Lookup("debian/bookworm/pool/main/a/apt/apt_2.0_amd64.deb"); !ok {
		t.Fatal("the current revision's pool path must resolve")
	}
}

// SPEC section 5: metadata resolves strictly against the current revision, so
// a stale index can never be served.
func TestWindowResolvesMetadataAgainstTheCurrentRevisionOnly(t *testing.T) {
	t.Parallel()

	old := revManifest(t, "debian/bookworm", "1754400000-aaaa", []manifest.Entry{
		entry("debian/bookworm/dists/bookworm/InRelease", hashOf(0x01), 100),
		entry("debian/bookworm/dists/bookworm/Retired", hashOf(0x05), 500),
	})
	current := revManifest(t, "debian/bookworm", "1754400100-bbbb", []manifest.Entry{
		entry("debian/bookworm/dists/bookworm/InRelease", hashOf(0x03), 300),
	})

	w := manifest.NewWindow(5)
	w.Push(old)
	w.Push(current)

	got, ok := w.Lookup("debian/bookworm/dists/bookworm/InRelease")
	if !ok {
		t.Fatal("current metadata must resolve")
	}
	if got.SHA256 != hashOf(0x03) {
		t.Fatalf("served digest %s, want the current revision's %s", got.SHA256, hashOf(0x03))
	}

	if _, ok := w.Lookup("debian/bookworm/dists/bookworm/Retired"); ok {
		t.Fatal("metadata dropped by the current revision must not resolve")
	}
}

func TestWindowKeepsAtMostKRevisions(t *testing.T) {
	t.Parallel()

	w := manifest.NewWindow(2)
	revisions := []string{"1754400000-aaaa", "1754400100-bbbb", "1754400200-cccc"}
	for i, rev := range revisions {
		w.Push(revManifest(t, "debian/bookworm", rev, []manifest.Entry{
			entry("debian/bookworm/pool/main/p/pkg/pkg_"+rev+".deb", hashOf(byte(i+1)), 10),
		}))
	}

	if got := len(w.Retained()); got != 2 {
		t.Fatalf("retained %d revisions, want 2", got)
	}
	if _, ok := w.Lookup("debian/bookworm/pool/main/p/pkg/pkg_1754400000-aaaa.deb"); ok {
		t.Fatal("a revision pushed out of the window must stop resolving")
	}
	if _, ok := w.Lookup("debian/bookworm/pool/main/p/pkg/pkg_1754400100-bbbb.deb"); !ok {
		t.Fatal("a retained revision must keep resolving")
	}
	if w.Current().Revision != "1754400200-cccc" {
		t.Fatalf("Current = %q, want the newest revision", w.Current().Revision)
	}
}

func TestEmptyWindowResolvesNothing(t *testing.T) {
	t.Parallel()

	w := manifest.NewWindow(5)
	if w.Current() != nil {
		t.Fatal("an empty window has no current revision")
	}
	if _, ok := w.Lookup("anything"); ok {
		t.Fatal("an empty window must not resolve any path")
	}
}

func TestIsMetadataClassifiesPaths(t *testing.T) {
	t.Parallel()

	metadata := []string{
		"dists/bookworm/InRelease",
		"debian/bookworm/dists/bookworm/main/binary-amd64/Packages",
		"ubuntu/jammy/dists/jammy/Release.gpg",
	}
	for _, p := range metadata {
		if !manifest.IsMetadata(p) {
			t.Fatalf("IsMetadata(%q) = false, want true", p)
		}
	}

	pool := []string{
		"pool/main/a/apt/apt_2.0_amd64.deb",
		"debian/bookworm/pool/main/n/nginx/nginx.deb",
		"keys/archive.gpg",
		"distsfoo/bar",
		"a/redists/bar",
	}
	for _, p := range pool {
		if manifest.IsMetadata(p) {
			t.Fatalf("IsMetadata(%q) = true, want false", p)
		}
	}
}
