// Package ofxparse extracts STMTTRN transactions from OFX and QFX
// (Quicken Financial Exchange) statement files (plan 4.3). Both are
// the same tagged-text format: lines of "TAG:value", records closed
// by a lone "^" or a closing tag. QFX files (the BMO / RBC export)
// reuse the <STMTTRN> tag for BOTH the wrapping section and each
// record (records "^"-closed); spec-conformant OFX wraps records in
// <BANKTRANLIST> instead (records closed by </STMTTRN>). Either way,
// a record starts at <STMTTRN. Values are kept RAW -- step 4.6
// validates and normalizes
// dates and amounts, exactly like csvparse rows. Stdlib only: no
// DB, no routes.
package ofxparse

import (
	"errors"
	"io"
	"strings"
)

// ErrEmpty is returned by Parse when the payload contains no
// non-empty STMTTRN record.
var ErrEmpty = errors.New("ofxparse: no STMTTRN records")

// maxParseBytes bounds how much of the file Parse reads (T-P0-3).
// The upload step caps statements at 10 MB; this mirrors that bound
// inside the parser so a larger payload can never reach the line
// loop, and reading one extra byte keeps "exactly the cap"
// distinguishable from "over it". Tests may lower it.
var maxParseBytes int64 = 10 << 20

// ErrTooLarge is returned by Parse when the file exceeds
// maxParseBytes.
var ErrTooLarge = errors.New("ofxparse: document too large")

// Transaction is one parsed STMTTRN record. All fields are the raw
// tag values from the file (missing tag -> ""); 4.6 decides what is
// fatal (an empty DatePosted or Amount becomes a row error there,
// not a parse error here).
type Transaction struct {
	DatePosted string // DTPOSTED: YYYYMMDD[HHMMSS[[-+]ZZZZ]]
	Amount     string // TRNAMT: plain decimal, sign = direction
	Name       string // NAME (or PAYEE): merchant / memo
	RefNum     string // REFNUM (or CHECKNUM), "" when absent
}

// Parse extracts every non-empty STMTTRN record from an OFX or QFX
// payload, in file order. It is deliberately viewer-grade: envelope
// junk (Signon, ACCTINQ, CLOSINGBATCH, ...), section tags and
// unknown record tags are skipped; a record missing a field comes
// back with "" rather than an error. A completely empty record --
// none of the four fields present -- is dropped; in QFX files the
// section opener is itself a <STMTTRN line, so it shows up as
// exactly such a record and the rule doubles as the section/record
// disambiguator.
func Parse(r io.Reader) ([]Transaction, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxParseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxParseBytes {
		return nil, ErrTooLarge
	}
	text := string(raw)
	if strings.HasPrefix(text, "\uFEFF") { // BOM, harmless to strip
		text = text[len("\uFEFF"):]
	}
	var (
		out   []Transaction
		cur   *Transaction
		inTrn bool
	)
	flush := func() {
		if cur != nil && (cur.DatePosted != "" || cur.Amount != "" ||
			cur.Name != "" || cur.RefNum != "") {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case line == "":
			continue
		case strings.EqualFold(line, "<STMTTRN"):
			flush() // closes a prior record that lacked its "^"
			cur = &Transaction{}
			inTrn = true
		case strings.EqualFold(line, "</STMTTRN"):
			flush()
			inTrn = false
		case line == "^":
			if inTrn {
				flush()
			}
		default:
			if !inTrn {
				continue
			}
			tag, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = unescape(value)
			switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tag), "<")) {
			case "dtposted":
				cur.DatePosted = value
			case "trnamt":
				cur.Amount = value
			case "name", "payee":
				cur.Name = value
			case "refnum", "checknum":
				cur.RefNum = value
			}
		}
	}
	flush() // unterminated last record: keep it rather than lose it
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// unescape decodes OFX value escapes: a backslash before ~ , : ; ^
// becomes the literal character, ~Uhhhh is a four-hex-digit Unicode
// rune (FIs such as Desjardins use it for accented merchant names)
// and ~N is a newline. Unknown backslash/tilde sequences are kept
// verbatim -- viewer-grade, never lose data.
func unescape(s string) string {
	if !strings.ContainsAny(s, "\\~") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '~' && i+6 <= len(s) && s[i+1] == 'U' && hexOK(s[i+2:i+6]):
			b.WriteRune(hexRune(s[i+2 : i+6]))
			i += 5
		case c == '~' && i+1 < len(s) && s[i+1] == 'N':
			i++
			b.WriteByte('\n')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hexOK reports whether h is exactly four hex digits.
func hexOK(h string) bool {
	if len(h) != 4 {
		return false
	}
	for _, c := range h {
		if hexVal(byte(c)) < 0 {
			return false
		}
	}
	return true
}

// hexRune parses four hex digits as a rune. Callers must hexOK first.
func hexRune(h string) rune {
	var r rune
	for _, c := range h {
		r = r<<4 | rune(hexVal(byte(c)))
	}
	return r
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
