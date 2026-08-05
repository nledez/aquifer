// Package debian parses the control files of a Debian repository: the Release
// index that describes a suite, and the Packages and Sources indices that list
// what lives under pool/.
//
// Aquifer never computes a digest that a publication already carries. aptly has
// already hashed every file, and those digests are exactly what these indices
// record, so parsing them is cheaper and more truthful than re-reading 17 GiB
// from disk. A file that appears in no index at all is the only case that needs
// hashing, and that is the caller's problem, not this package's.
package debian

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Paragraph is one deb822 stanza: an ordered set of fields, looked up
// case-insensitively the way the format specifies.
type Paragraph struct {
	fields map[string]string
	order  []string
}

// Get returns a field's value, or the empty string when it is absent.
func (p Paragraph) Get(field string) string {
	return p.fields[strings.ToLower(field)]
}

// Has reports whether a field is present, which is not the same as non-empty:
// a multi-line digest field starts out with an empty first line.
func (p Paragraph) Has(field string) bool {
	_, ok := p.fields[strings.ToLower(field)]
	return ok
}

// Fields lists the field names in the order they appeared.
func (p Paragraph) Fields() []string { return p.order }

// Decoder reads deb822 paragraphs one at a time. A Packages index for a large
// suite is tens of megabytes, so nothing here holds more than one paragraph.
type Decoder struct {
	sc     *bufio.Scanner
	lineNo int
}

// maxFieldLen bounds one physical line. Real control files stay far below it.
const maxFieldLen = 1 << 20

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFieldLen)
	return &Decoder{sc: sc}
}

// Next returns the next paragraph, or io.EOF once the input is exhausted.
func (d *Decoder) Next() (Paragraph, error) {
	p := Paragraph{fields: map[string]string{}}
	var current string

	for d.sc.Scan() {
		d.lineNo++
		line := strings.TrimSuffix(d.sc.Text(), "\r")

		if strings.TrimSpace(line) == "" {
			if len(p.order) > 0 {
				return p, nil
			}
			continue // a run of blank lines between paragraphs
		}

		if line[0] == ' ' || line[0] == '\t' {
			if current == "" {
				return Paragraph{}, fmt.Errorf("debian: line %d: continuation with no field before it", d.lineNo)
			}
			content := strings.TrimLeft(line, " \t")
			if content == "." {
				content = "" // deb822 spells a blank body line as a lone dot
			}
			if p.fields[current] == "" {
				p.fields[current] = content
			} else {
				p.fields[current] += "\n" + content
			}
			continue
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return Paragraph{}, fmt.Errorf("debian: line %d: field line has no colon: %q", d.lineNo, line)
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t") {
			return Paragraph{}, fmt.Errorf("debian: line %d: malformed field name %q", d.lineNo, name)
		}
		current = strings.ToLower(name)
		if _, seen := p.fields[current]; !seen {
			p.order = append(p.order, name)
		}
		p.fields[current] = strings.TrimSpace(value)
	}

	if err := d.sc.Err(); err != nil {
		return Paragraph{}, fmt.Errorf("debian: read: %w", err)
	}
	if len(p.order) > 0 {
		return p, nil
	}
	return Paragraph{}, io.EOF
}

// errNoParagraph reports an input that held no stanza at all.
var errNoParagraph = errors.New("debian: no paragraph found")
