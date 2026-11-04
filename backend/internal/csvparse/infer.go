package csvparse

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

const (
	// sampleCap bounds how many data rows the column inference scans.
	// Statements run to a few hundred rows at most; a few hundred
	// sampled rows are plenty for stable type ratios and keep Parse
	// fast on the 10 MB upload cap.
	sampleCap = 500

	// balanceTolerance is half a cent: statement balances are
	// 2-decimal, so the running-balance recurrence holds to within
	// rounding.
	balanceTolerance = 0.005
)

var (
	isoDateRe   = regexp.MustCompile(`^(\d{4})[-/](\d{1,2})[-/](\d{1,2})$`)
	slashDateRe = regexp.MustCompile(`^(\d{1,2})[-/](\d{1,2})[-/](\d{4})$`)

	amountIntRe   = regexp.MustCompile(`^\d+$`)
	amountDecRe   = regexp.MustCompile(`^\d+\.\d{1,4}$`)
	amountGroupRe = regexp.MustCompile(`^\d{1,3}(,\d{3})+(\.\d{1,4})?$`)
)

// parseDateCell classifies s as a date. It reports whether the cell is
// ISO-shaped (year first) and, for the other shape, the two numeric
// parts before the year (first/second). MM-DD-YYYY vs DD-MM-YYYY
// ambiguity is resolved at column level by classifyDateFormat (any
// part > 12 decides; the default is mm-dd-yyyy, the US convention).
func parseDateCell(s string) (isISO bool, first, second int, ok bool) {
	if m := isoDateRe.FindStringSubmatch(s); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if year < 1900 || year > 2099 || month < 1 || month > 12 || day < 1 || day > 31 {
			return false, 0, 0, false
		}
		return true, 0, 0, true
	}
	if m := slashDateRe.FindStringSubmatch(s); m != nil {
		first, _ := strconv.Atoi(m[1])
		second, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if year < 1900 || year > 2099 {
			return false, 0, 0, false
		}
		if first < 1 || first > 31 || second < 1 || second > 31 {
			return false, 0, 0, false
		}
		if first > 12 && second > 12 {
			return false, 0, 0, false
		}
		return false, first, second, true
	}
	return false, 0, 0, false
}

// ParseAmount recognizes the amount spellings bank exports use: an
// optional currency marker (C$ / $), an optional sign (leading -,
// trailing -, or parentheses for negatives) and a body that is a bare
// integer, a decimal with up to four fractional digits, or a
// US-grouped number (1,234.56). It reports false for anything else.
func ParseAmount(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		neg = true
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	// Currency and sign appear in either order ("C$-42.10", "-$42.10",
	// "42.10-"); strip them until only the body is left. Each
	// iteration shrinks s, so this terminates.
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(strings.ToLower(s), "c$"):
			s = strings.TrimSpace(s[2:])
		case strings.HasPrefix(s, "$"):
			s = strings.TrimSpace(s[1:])
		case strings.HasPrefix(s, "-"):
			neg = true
			s = strings.TrimSpace(s[1:])
		case strings.HasSuffix(s, "-"):
			neg = true
			s = strings.TrimSpace(s[:len(s)-1])
		default:
			goto done
		}
	}
done:
	switch {
	case amountIntRe.MatchString(s), amountDecRe.MatchString(s), amountGroupRe.MatchString(s):
	default:
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// analyzeColumns infers each column's kind (and date format /
// running-balance flag) from the first sampleCap data rows. Date is
// tried before amount per cell; the two are disjoint, so a date cell
// never counts as an amount.
func analyzeColumns(f *File) []Column {
	width := len(f.Header)
	for _, row := range f.Rows {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return nil
	}
	cols := make([]Column, width)
	for i := range cols {
		cols[i] = Column{Index: i}
		if f.HasHeader && i < len(f.Header) {
			cols[i].Heading = strings.TrimSpace(f.Header[i])
		}
	}

	n := len(f.Rows)
	if n > sampleCap {
		n = sampleCap
	}
	ne := make([]int, width)
	dateCnt := make([]int, width)
	amtCnt := make([]int, width)
	isoCnt := make([]int, width)
	firstGT := make([]bool, width)
	secondGT := make([]bool, width)
	vals := make([][]float64, width)
	for i := range vals {
		vals[i] = make([]float64, 0, n)
	}

	for r := 0; r < n; r++ {
		row := f.Rows[r]
		for i := 0; i < width; i++ {
			var cell string
			if i < len(row) {
				cell = strings.TrimSpace(row[i])
			}
			if cell == "" {
				vals[i] = append(vals[i], math.NaN())
				continue
			}
			ne[i]++
			if isISO, first, second, ok := parseDateCell(cell); ok {
				dateCnt[i]++
				if isISO {
					isoCnt[i]++
				}
				if first > 12 {
					firstGT[i] = true
				}
				if second > 12 {
					secondGT[i] = true
				}
				vals[i] = append(vals[i], math.NaN())
			} else if v, ok := ParseAmount(cell); ok {
				amtCnt[i]++
				vals[i] = append(vals[i], v)
			} else {
				vals[i] = append(vals[i], math.NaN())
			}
		}
	}

	for i := range cols {
		switch {
		case dateCnt[i]*2 > ne[i]:
			cols[i].Kind = KindDate
			cols[i].DateFormat = classifyDateFormat(isoCnt[i], dateCnt[i], firstGT[i], secondGT[i])
		case amtCnt[i]*2 > ne[i]:
			cols[i].Kind = KindAmount
		case ne[i] == 0:
			cols[i].Kind = KindUnknown
		default:
			cols[i].Kind = KindText
		}
	}
	markRunningBalances(cols, vals)
	return cols
}

// classifyDateFormat pins a date column's layout. A part > 12 is
// decisive (a month or day can never exceed 12). Otherwise an ISO
// plurality wins; the default is the US convention.
func classifyDateFormat(isoCount, dateCount int, firstGT, secondGT bool) string {
	if firstGT {
		return "dd-mm-yyyy"
	}
	if secondGT {
		return "mm-dd-yyyy"
	}
	if isoCount*2 > dateCount {
		return "iso8601"
	}
	return "mm-dd-yyyy"
}

// markRunningBalances flags amount columns that track a cumulative
// statement balance: for an ordered pair (amount a, balance b), most
// consecutive rows must satisfy b - bPrev = +/- a within half a cent,
// with one sign dominant (>= 70%) so a coincidental fit does not pass.
func markRunningBalances(cols []Column, vals [][]float64) {
	for i, a := range cols {
		if a.Kind != KindAmount {
			continue
		}
		for j, b := range cols {
			if i == j || b.Kind != KindAmount {
				continue
			}
			if looksBalanced(vals[i], vals[j]) {
				cols[j].RunningBalance = true
			}
		}
	}
}

func looksBalanced(a, b []float64) bool {
	matched, plus, minus := 0, 0, 0
	lastB := math.NaN()
	for r := range a {
		bv := b[r]
		if math.IsNaN(bv) {
			continue
		}
		if av := a[r]; !math.IsNaN(av) && av != 0 {
			if !math.IsNaN(lastB) {
				d := bv - lastB
				switch {
				case math.Abs(d-av) <= balanceTolerance:
					matched++
					plus++
				case math.Abs(d+av) <= balanceTolerance:
					matched++
					minus++
				}
			}
		}
		lastB = bv
	}
	if matched < 3 {
		return false
	}
	return float64(plus) >= 0.7*float64(matched) || float64(minus) >= 0.7*float64(matched)
}
