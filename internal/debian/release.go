package debian

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Release describes one suite, as read from dists/<suite>/Release or from the
// clearsigned InRelease that apt prefers.
type Release struct {
	Origin        string
	Label         string
	Suite         string
	Codename      string
	Architectures []string
	Components    []string
	Date          time.Time
	// ValidUntil is zero when the field is absent. When set, it is the most
	// reliable freshness signal an operator has: once it passes, apt refuses
	// the repository outright, whatever the mirror thinks.
	ValidUntil    time.Time
	AcquireByHash bool

	// Files are the indices listed under SHA256, with paths relative to the
	// suite directory.
	Files []IndexFile
}

// IndexFile is one entry of a Release SHA256 listing.
type IndexFile struct {
	Path   string
	SHA256 string
	Size   int64
}

const (
	clearsignHeader    = "-----BEGIN PGP SIGNED MESSAGE-----"
	clearsignSignature = "-----BEGIN PGP SIGNATURE-----"
	sha256HexLen       = 64
)

// ParseRelease reads a Release or InRelease file. Clearsign armor is stripped
// but not verified: signature checking is apt's job on the client, and the
// master publishes what aptly signed, byte for byte.
func ParseRelease(r io.Reader) (*Release, error) {
	br := bufio.NewReader(r)

	body, err := stripClearsign(br)
	if err != nil {
		return nil, err
	}

	p, err := NewDecoder(body).Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errNoParagraph
		}
		return nil, err
	}

	rel := &Release{
		Origin:        p.Get("Origin"),
		Label:         p.Get("Label"),
		Suite:         p.Get("Suite"),
		Codename:      p.Get("Codename"),
		Architectures: strings.Fields(p.Get("Architectures")),
		Components:    strings.Fields(p.Get("Components")),
		AcquireByHash: strings.EqualFold(p.Get("Acquire-By-Hash"), "yes"),
	}

	if raw := p.Get("Date"); raw != "" {
		rel.Date, err = parseReleaseTime(raw)
		if err != nil {
			return nil, fmt.Errorf("debian: Date: %w", err)
		}
	}
	if raw := p.Get("Valid-Until"); raw != "" {
		rel.ValidUntil, err = parseReleaseTime(raw)
		if err != nil {
			return nil, fmt.Errorf("debian: Valid-Until: %w", err)
		}
	}

	if !p.Has("SHA256") {
		return nil, errors.New("debian: Release has no SHA256 field; Aquifer addresses everything by SHA-256")
	}
	rel.Files, err = parseDigestList(p.Get("SHA256"))
	if err != nil {
		return nil, err
	}
	return rel, nil
}

// parseDigestList reads the "<sha256> <size> <path>" lines of a Release
// checksum field.
func parseDigestList(value string) ([]IndexFile, error) {
	var out []IndexFile
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("debian: SHA256 entry %q has %d fields, want 3", line, len(fields))
		}
		digest, rawSize, path := fields[0], fields[1], fields[2]
		if err := validateDigest(digest); err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(rawSize, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("debian: SHA256 entry %q: malformed size: %w", line, err)
		}
		if size < 0 {
			return nil, fmt.Errorf("debian: SHA256 entry %q: negative size", line)
		}
		out = append(out, IndexFile{Path: path, SHA256: digest, Size: size})
	}
	return out, nil
}

// releaseTimeLayouts covers what Debian tooling emits in practice.
var releaseTimeLayouts = []string{
	time.RFC1123,  // Tue, 05 Aug 2026 12:00:00 UTC
	time.RFC1123Z, // Tue, 05 Aug 2026 14:00:00 +0200
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04:05 -0700",
}

func parseReleaseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range releaseTimeLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", value)
}

func validateDigest(digest string) error {
	if len(digest) != sha256HexLen {
		return fmt.Errorf("debian: digest %q is %d characters, want %d", digest, len(digest), sha256HexLen)
	}
	for i := range len(digest) {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("debian: digest %q is not lowercase hex", digest)
		}
	}
	return nil
}

// stripClearsign returns the signed body of an InRelease, or the input
// unchanged when it carries no armor.
func stripClearsign(br *bufio.Reader) (io.Reader, error) {
	prefix, err := br.Peek(len(clearsignHeader))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("debian: read: %w", err)
	}
	if !bytes.Equal(prefix, []byte(clearsignHeader)) {
		return br, nil
	}
	return &clearsignReader{br: br}, nil
}

// clearsignReader yields the body of a clearsigned message, undoing the dash
// escaping RFC 4880 applies to lines that start with a dash, and stopping at
// the signature block.
type clearsignReader struct {
	br      *bufio.Reader
	buf     bytes.Buffer
	started bool
	done    bool
	err     error
}

func (c *clearsignReader) Read(p []byte) (int, error) {
	for c.buf.Len() == 0 {
		if c.err != nil {
			return 0, c.err
		}
		if c.done {
			return 0, io.EOF
		}
		if err := c.fill(); err != nil {
			c.err = err
			if c.buf.Len() == 0 {
				return 0, err
			}
		}
	}
	return c.buf.Read(p)
}

// fill appends the next body line to the buffer.
func (c *clearsignReader) fill() error {
	if !c.started {
		if err := c.skipArmorHeaders(); err != nil {
			return err
		}
		c.started = true
	}

	line, err := c.br.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			// The body ended without a signature block, which means the file
			// is truncated. Accepting it would mean publishing a Release whose
			// index list may be incomplete.
			return errors.New("debian: clearsigned message ends without a signature block")
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("debian: read: %w", err)
		}
	}

	if strings.HasPrefix(line, clearsignSignature) {
		c.done = true
		return io.EOF
	}
	// RFC 4880 escapes a leading dash as "- ".
	body := strings.TrimPrefix(line, "- ")
	c.buf.WriteString(body)
	if err != nil { // a final line with no newline
		c.done = true
	}
	return nil
}

func (c *clearsignReader) skipArmorHeaders() error {
	// The first line is the header itself, then optional "Hash:" style
	// headers, then a blank line introducing the body.
	if _, err := c.br.ReadString('\n'); err != nil {
		return fmt.Errorf("debian: read: %w", err)
	}
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("debian: clearsigned message has no body: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			return nil
		}
	}
}
