package csvparse

import (
	"encoding/csv"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Parse decodes r, sniffs the delimiter, detects the header row and
// infers column types. The input is read fully into memory --
// statement files are capped at 10 MB by the upload step (4.1).
func Parse(r io.Reader) (*File, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	text, encoding := decode(raw)
	del := sniffDelimiter(text)
	rows, err := readRows(text, del)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrEmpty
	}
	f := &File{Delimiter: del, Encoding: encoding, Rows: rows}
	if header, data := detectHeader(rows); header != nil {
		f.HasHeader = true
		f.Header = header
		f.Rows = data
	}
	if len(f.Rows) == 0 {
		return nil, ErrEmpty // header-only files have nothing to import
	}
	f.Columns = analyzeColumns(f)
	return f, nil
}

// decode maps the raw bytes to a string, reporting the encoding label.
// Order matters: BOMs are checked before validity, because UTF-16
// bytes are never valid UTF-8. Files with a UTF-16 BOM that fail to
// decode (odd length, bad unit) fall through to the latin-1 escape
// hatch rather than erroring -- the goal is to never lose the user's
// file.
func decode(raw []byte) (text, encoding string) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		return string(raw[3:]), "utf-8"
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		if r, ok := decodeUTF16(raw[2:], true); ok {
			return r, "utf-16le"
		}
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		if r, ok := decodeUTF16(raw[2:], false); ok {
			return r, "utf-16be"
		}
	case utf8.Valid(raw):
		return string(raw), "utf-8"
	}
	// latin-1: every byte maps to the rune of the same value,
	// re-encoded as UTF-8 so the result is a valid Go string (and a
	// valid value for the downstream JSON/Postgres layers).
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}
	return string(runes), "latin-1"
}

// decodeUTF16 converts raw little/big-endian UTF-16 bytes to a string.
// It reports false only for odd-length payloads (a truncated BOM);
// bad code units surface as U+FFFD, which is the right outcome for a
// viewer-grade parser -- never lose the file over one bad unit.
func decodeUTF16(raw []byte, little bool) (string, bool) {
	if len(raw)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		b0, b1 := raw[2*i], raw[2*i+1]
		if little {
			units[i] = uint16(b0) | uint16(b1)<<8 // first byte is low
		} else {
			units[i] = uint16(b0)<<8 | uint16(b1) // first byte is high
		}
	}
	return string(utf16.Decode(units)), true
}

// sniffDelimiter picks from ',' ';' '\t' by looking at the first five
// non-empty lines. A candidate that splits no line at all is a
// fallback: it would mean a single-column file, and a non-delimiter
// always looks "perfectly consistent" (one column per line), so it
// must not beat a real delimiter just because the file is ragged.
// Among candidates that do split something, the winner has the fewest
// distinct column counts across the sample lines, then the larger
// maximum count, then the fixed priority order.
func sniffDelimiter(text string) rune {
	lines := firstNonEmptyLines(text, 5)
	if len(lines) == 0 {
		return ','
	}
	candidates := []rune{',', ';', '\t'}
	type score struct {
		distinct, max, priority int
	}
	scores := make([]score, len(candidates))
	for p, r := range candidates {
		seen := map[int]bool{}
		max := 0
		for _, ln := range lines {
			c := countColumns(ln, r)
			seen[c] = true
			if c > max {
				max = c
			}
		}
		scores[p] = score{len(seen), max, p}
	}
	best := -1
	for p, s := range scores {
		if s.max < 2 { // splits nothing: keep as fallback only
			continue
		}
		if best == -1 ||
			s.distinct < scores[best].distinct ||
			(s.distinct == scores[best].distinct && s.max > scores[best].max) ||
			(s.distinct == scores[best].distinct && s.max == scores[best].max && s.priority < scores[best].priority) {
			best = p
		}
	}
	if best == -1 {
		return ',' // single-column file
	}
	return candidates[best]
}

// firstNonEmptyLines returns up to n lines of text that are not blank
// after trimming CR/whitespace.
func firstNonEmptyLines(text string, n int) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if ln == "" {
			continue
		}
		out = append(out, ln)
		if len(out) == n {
			break
		}
	}
	return out
}

// countColumns counts the CSV fields in line for delimiter r,
// respecting double quotes so a delimiter inside a quoted field does
// not split it. Escaped quotes ("" inside a quoted field) are skipped
// as a pair; a stray unbalanced quote degrades to a best-effort count,
// which is all a sniffer needs.
func countColumns(line string, r rune) int {
	n := 1
	inQuotes := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			if inQuotes && i+1 < len(line) && line[i+1] == '"' {
				i++ // escaped quote; stay in quotes
			} else {
				inQuotes = !inQuotes
			}
		case byte(r):
			if !inQuotes {
				n++
			}
		}
	}
	return n
}

// readRows runs the stdlib csv reader over the decoded text.
// FieldsPerRecord -1 tolerates ragged rows (trailing blank cells are
// trimmed per record); LazyQuotes tolerates the stray quotes bank
// exports are full of. Records that still fail to parse are skipped
// (the reader recovers on the next line) so one bad line never loses
// the whole file; all-empty records are dropped. More than MaxRows
// raw records (the header and all-empty records count too) is an
// error: the cap bounds memory independently of the upload step's
// byte cap (T-P0-3).
func readRows(text string, del rune) ([][]string, error) {
	rd := csv.NewReader(strings.NewReader(text))
	rd.Comma = del
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	rd.TrimLeadingSpace = true
	var rows [][]string
	var seen int
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		seen++
		if seen > MaxRows {
			return nil, ErrTooManyRows
		}
		rec = trimTrailingEmpty(rec)
		if isAllEmpty(rec) {
			continue
		}
		rows = append(rows, rec)
	}
	return rows, nil
}

// trimTrailingEmpty drops trailing empty cells so a row like
// "a,b," does not inflate the column count with a phantom column.
func trimTrailingEmpty(rec []string) []string {
	i := len(rec)
	for i > 0 && strings.TrimSpace(rec[i-1]) == "" {
		i--
	}
	return rec[:i]
}

func isAllEmpty(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// detectHeader decides whether the first row is a header. A row that
// looks like data -- its first non-empty cell parses as a date, or it
// carries at least one date cell AND at least one amount cell -- means
// the file is headerless and every row is data. Otherwise the first
// row is the (trimmed) header.
func detectHeader(rows [][]string) (header []string, data [][]string) {
	if len(rows) == 0 {
		return nil, rows
	}
	first := rows[0]
	dates, amounts := 0, 0
	firstCellDate := false
	firstSeen := false
	for _, cell := range first {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			continue
		}
		_, _, _, isDate := parseDateCell(cell)
		if !firstSeen {
			firstSeen = true
			firstCellDate = isDate
		}
		if isDate {
			dates++
		} else if _, ok := ParseAmount(cell); ok {
			amounts++
		}
	}
	if firstCellDate || (dates >= 1 && amounts >= 1) {
		return nil, rows
	}
	header = make([]string, len(first))
	for i, c := range first {
		header[i] = strings.TrimSpace(c)
	}
	return header, rows[1:]
}
