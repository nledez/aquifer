package debian_test

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nledez/aquifer/internal/debian"
)

// --- deb822 ------------------------------------------------------------------

func TestDecoderReadsParagraphsAndContinuations(t *testing.T) {
	t.Parallel()

	const input = "Package: nginx\n" +
		"Version: 1.24.0-1\n" +
		"Description: small web server\n" +
		" It also does reverse proxying.\n" +
		"\tand this line is folded with a tab\n" +
		"\n" +
		"Package: apt\n" +
		"Version: 2.6.1\n"

	dec := debian.NewDecoder(strings.NewReader(input))

	first, err := dec.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if got := first.Get("Package"); got != "nginx" {
		t.Fatalf("Package = %q, want nginx", got)
	}
	wantDesc := "small web server\nIt also does reverse proxying.\nand this line is folded with a tab"
	if got := first.Get("Description"); got != wantDesc {
		t.Fatalf("Description = %q, want %q", got, wantDesc)
	}

	second, err := dec.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if got := second.Get("Package"); got != "apt" {
		t.Fatalf("Package = %q, want apt", got)
	}

	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("third Next: got %v, want io.EOF", err)
	}
}

func TestDecoderLooksUpFieldsCaseInsensitively(t *testing.T) {
	t.Parallel()

	dec := debian.NewDecoder(strings.NewReader("SHA256:\n abc 1 x\nFilename: pool/x\n"))
	p, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := p.Get("filename"); got != "pool/x" {
		t.Fatalf("Get(\"filename\") = %q, want pool/x", got)
	}
	if got := p.Get("FILENAME"); got != "pool/x" {
		t.Fatalf("Get(\"FILENAME\") = %q, want pool/x", got)
	}
}

func TestDecoderToleratesBlankRunsAndCRLF(t *testing.T) {
	t.Parallel()

	const input = "\r\n\r\nPackage: a\r\nVersion: 1\r\n\r\n\r\nPackage: b\r\nVersion: 2\r\n\r\n"
	dec := debian.NewDecoder(strings.NewReader(input))

	var names []string
	for {
		p, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		names = append(names, p.Get("Package"))
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("paragraphs = %v, want [a b]", names)
	}
}

func TestDecoderRejectsAContinuationWithoutAField(t *testing.T) {
	t.Parallel()

	dec := debian.NewDecoder(strings.NewReader(" orphaned continuation\nPackage: a\n"))
	if _, err := dec.Next(); err == nil {
		t.Fatal("Next accepted a continuation line with no field before it")
	}
}

func TestDecoderRejectsALineWithoutAColon(t *testing.T) {
	t.Parallel()

	dec := debian.NewDecoder(strings.NewReader("Package: a\nthis line has no colon\n"))
	if _, err := dec.Next(); err == nil {
		t.Fatal("Next accepted a field line with no colon")
	}
}

// --- Release -----------------------------------------------------------------

const releaseBody = `Origin: Example
Label: Example
Suite: bookworm
Codename: bookworm
Date: Tue, 05 Aug 2026 12:00:00 UTC
Valid-Until: Tue, 12 Aug 2026 12:00:00 UTC
Architectures: amd64 arm64
Components: main contrib
Acquire-By-Hash: yes
MD5Sum:
 d41d8cd98f00b204e9800998ecf8427e            1234 main/binary-amd64/Packages
SHA256:
 1111111111111111111111111111111111111111111111111111111111111111            1234 main/binary-amd64/Packages
 2222222222222222222222222222222222222222222222222222222222222222             567 main/binary-amd64/Packages.gz
 3333333333333333333333333333333333333333333333333333333333333333            8901 main/source/Sources
`

func TestParseReleaseReadsMetadataAndIndexDigests(t *testing.T) {
	t.Parallel()

	rel, err := debian.ParseRelease(strings.NewReader(releaseBody))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}

	if rel.Suite != "bookworm" || rel.Codename != "bookworm" {
		t.Fatalf("suite/codename = %q/%q", rel.Suite, rel.Codename)
	}
	if len(rel.Architectures) != 2 || rel.Architectures[0] != "amd64" {
		t.Fatalf("Architectures = %v", rel.Architectures)
	}
	if len(rel.Components) != 2 || rel.Components[1] != "contrib" {
		t.Fatalf("Components = %v", rel.Components)
	}
	if !rel.AcquireByHash {
		t.Fatal("Acquire-By-Hash: yes was not picked up")
	}

	wantValid := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if !rel.ValidUntil.Equal(wantValid) {
		t.Fatalf("ValidUntil = %v, want %v", rel.ValidUntil, wantValid)
	}
	wantDate := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	if !rel.Date.Equal(wantDate) {
		t.Fatalf("Date = %v, want %v", rel.Date, wantDate)
	}

	// Only SHA256 is kept: it is the digest the whole system addresses by.
	if len(rel.Files) != 3 {
		t.Fatalf("got %d index files, want 3", len(rel.Files))
	}
	first := rel.Files[0]
	if first.Path != "main/binary-amd64/Packages" ||
		first.SHA256 != strings.Repeat("1", 64) ||
		first.Size != 1234 {
		t.Fatalf("first index file = %+v", first)
	}
}

func TestParseReleaseWithoutValidUntilLeavesItZero(t *testing.T) {
	t.Parallel()

	body := strings.Replace(releaseBody, "Valid-Until: Tue, 12 Aug 2026 12:00:00 UTC\n", "", 1)
	rel, err := debian.ParseRelease(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if !rel.ValidUntil.IsZero() {
		t.Fatalf("ValidUntil = %v, want the zero time", rel.ValidUntil)
	}
}

func TestParseReleaseAcceptsNumericTimezones(t *testing.T) {
	t.Parallel()

	body := strings.Replace(releaseBody,
		"Valid-Until: Tue, 12 Aug 2026 12:00:00 UTC",
		"Valid-Until: Tue, 12 Aug 2026 14:00:00 +0200", 1)
	rel, err := debian.ParseRelease(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if !rel.ValidUntil.Equal(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("ValidUntil = %v", rel.ValidUntil.UTC())
	}
}

func TestParseReleaseRejectsAReleaseWithoutSHA256(t *testing.T) {
	t.Parallel()

	body := "Suite: bookworm\nCodename: bookworm\n" +
		"MD5Sum:\n d41d8cd98f00b204e9800998ecf8427e 1234 main/binary-amd64/Packages\n"
	if _, err := debian.ParseRelease(strings.NewReader(body)); err == nil {
		t.Fatal("ParseRelease accepted a Release with no SHA256 field")
	}
}

func TestParseReleaseRejectsMalformedDigestLines(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"two fields":     " 1111111111111111111111111111111111111111111111111111111111111111 1234\n",
		"bad size":       " 1111111111111111111111111111111111111111111111111111111111111111 big p\n",
		"short digest":   " 11 1234 p\n",
		"uppercase hash": " " + strings.Repeat("A", 64) + " 1234 p\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := "Suite: s\nSHA256:\n" + line
			if _, err := debian.ParseRelease(strings.NewReader(body)); err == nil {
				t.Fatalf("ParseRelease accepted %s", name)
			}
		})
	}
}

// InRelease is the clearsigned form apt actually prefers.
func TestParseReleaseStripsClearsignArmor(t *testing.T) {
	t.Parallel()

	signed := "-----BEGIN PGP SIGNED MESSAGE-----\n" +
		"Hash: SHA512\n" +
		"\n" +
		releaseBody +
		"-----BEGIN PGP SIGNATURE-----\n" +
		"\n" +
		"iQIzBAEBCgAdFiEEexample\n" +
		"-----END PGP SIGNATURE-----\n"

	rel, err := debian.ParseRelease(strings.NewReader(signed))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if rel.Suite != "bookworm" {
		t.Fatalf("Suite = %q, want bookworm", rel.Suite)
	}
	if len(rel.Files) != 3 {
		t.Fatalf("got %d index files, want 3", len(rel.Files))
	}
}

// RFC 4880 escapes any body line starting with a dash.
func TestParseReleaseUndoesDashEscaping(t *testing.T) {
	t.Parallel()

	signed := "-----BEGIN PGP SIGNED MESSAGE-----\n" +
		"Hash: SHA512\n" +
		"\n" +
		"- Suite: bookworm\n" +
		"SHA256:\n" +
		" " + strings.Repeat("1", 64) + " 10 main/binary-amd64/Packages\n" +
		"-----BEGIN PGP SIGNATURE-----\n" +
		"\n" +
		"sig\n" +
		"-----END PGP SIGNATURE-----\n"

	rel, err := debian.ParseRelease(strings.NewReader(signed))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if rel.Suite != "bookworm" {
		t.Fatalf("Suite = %q, want bookworm", rel.Suite)
	}
}

func TestParseReleaseRejectsAnUnterminatedSignedMessage(t *testing.T) {
	t.Parallel()

	signed := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nSuite: bookworm\n"
	if _, err := debian.ParseRelease(strings.NewReader(signed)); err == nil {
		t.Fatal("ParseRelease accepted a clearsigned message with no signature block")
	}
}

// --- Packages ----------------------------------------------------------------

const packagesBody = `Package: nginx
Version: 1.24.0-1
Architecture: amd64
Filename: pool/main/n/nginx/nginx_1.24.0-1_amd64.deb
Size: 1234567
SHA256: 4444444444444444444444444444444444444444444444444444444444444444

Package: apt
Version: 2.6.1
Architecture: amd64
Filename: pool/main/a/apt/apt_2.6.1_amd64.deb
Size: 90000000
SHA256: 5555555555555555555555555555555555555555555555555555555555555555
`

func TestParsePackagesYieldsPoolEntries(t *testing.T) {
	t.Parallel()

	var got []debian.PoolFile
	for pf, err := range debian.Packages(strings.NewReader(packagesBody)) {
		if err != nil {
			t.Fatalf("Packages: %v", err)
		}
		got = append(got, pf)
	}

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Path != "pool/main/n/nginx/nginx_1.24.0-1_amd64.deb" {
		t.Fatalf("first path = %q", got[0].Path)
	}
	if got[0].SHA256 != strings.Repeat("4", 64) || got[0].Size != 1234567 {
		t.Fatalf("first entry = %+v", got[0])
	}
	if got[1].Size != 90000000 {
		t.Fatalf("second size = %d", got[1].Size)
	}
}

func TestParsePackagesRejectsEntriesWeCannotAddress(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"no SHA256":   "Package: x\nFilename: pool/x\nSize: 1\n",
		"no Filename": "Package: x\nSize: 1\nSHA256: " + strings.Repeat("4", 64) + "\n",
		"no Size":     "Package: x\nFilename: pool/x\nSHA256: " + strings.Repeat("4", 64) + "\n",
		"bad SHA256":  "Package: x\nFilename: pool/x\nSize: 1\nSHA256: nope\n",
		"bad Size":    "Package: x\nFilename: pool/x\nSize: huge\nSHA256: " + strings.Repeat("4", 64) + "\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var sawErr bool
			for _, err := range debian.Packages(strings.NewReader(body)) {
				if err != nil {
					sawErr = true
				}
			}
			if !sawErr {
				t.Fatalf("Packages accepted an entry with %s", name)
			}
		})
	}
}

func TestPackagesStopsWhenTheCallerBreaks(t *testing.T) {
	t.Parallel()

	var seen int
	for range debian.Packages(strings.NewReader(packagesBody)) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("iterated %d entries after break, want 1", seen)
	}
}

// --- Sources -----------------------------------------------------------------

const sourcesBody = `Package: nginx
Version: 1.24.0-1
Directory: pool/main/n/nginx
Checksums-Sha256:
 6666666666666666666666666666666666666666666666666666666666666666 4321 nginx_1.24.0-1.dsc
 7777777777777777777777777777777777777777777777777777777777777777 987654 nginx_1.24.0.orig.tar.gz
`

func TestParseSourcesJoinsDirectoryAndFilename(t *testing.T) {
	t.Parallel()

	var got []debian.PoolFile
	for pf, err := range debian.Sources(strings.NewReader(sourcesBody)) {
		if err != nil {
			t.Fatalf("Sources: %v", err)
		}
		got = append(got, pf)
	}

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Path != "pool/main/n/nginx/nginx_1.24.0-1.dsc" {
		t.Fatalf("first path = %q", got[0].Path)
	}
	if got[1].Path != "pool/main/n/nginx/nginx_1.24.0.orig.tar.gz" || got[1].Size != 987654 {
		t.Fatalf("second entry = %+v", got[1])
	}
}

func TestParseSourcesRejectsAStanzaWithoutADirectory(t *testing.T) {
	t.Parallel()

	body := "Package: x\nChecksums-Sha256:\n " + strings.Repeat("6", 64) + " 1 x.dsc\n"
	var sawErr bool
	for _, err := range debian.Sources(strings.NewReader(body)) {
		if err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("Sources accepted a stanza with no Directory")
	}
}

// --- index decompression ------------------------------------------------------

func TestOpenIndexHandlesTheCompressionAptlyWrites(t *testing.T) {
	t.Parallel()

	const plain = "Package: nginx\n"

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"Packages", []byte(plain)},
		{"Packages.gz", gz.Bytes()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc, err := debian.OpenIndex(tc.name, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("OpenIndex: %v", err)
			}
			defer func() { _ = rc.Close() }()

			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != plain {
				t.Fatalf("content = %q, want %q", got, plain)
			}
		})
	}
}

func TestOpenIndexReadsBzip2(t *testing.T) {
	t.Parallel()

	// compress/bzip2 is decompression only, so verify against a known stream:
	// the bzip2 encoding of "Package: nginx\n" produced by the bzip2 tool.
	// Round-tripping through the stdlib is impossible, so assert that the
	// reader is wired up by decoding a stream we can also decode directly.
	plain := []byte("Package: nginx\n")
	compressed := bzip2Fixture

	if got, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(compressed))); err != nil {
		t.Fatalf("fixture is not valid bzip2: %v", err)
	} else if !bytes.Equal(got, plain) {
		t.Fatalf("fixture decodes to %q, want %q", got, plain)
	}

	rc, err := debian.OpenIndex("Packages.bz2", bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("content = %q, want %q", got, plain)
	}
}

func TestOpenIndexRejectsUnknownCompression(t *testing.T) {
	t.Parallel()

	if _, err := debian.OpenIndex("Packages.xz", strings.NewReader("")); err == nil {
		t.Fatal("OpenIndex accepted a compression format it cannot read")
	}
}
