package csvparse

import (
	"math"
	"testing"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"$1,234.56", 1234.56, true},
		{"C$1,234.56", 1234.56, true},
		{"c$1,234.56", 1234.56, true},
		{"(1,234.56)", -1234.56, true},
		{"($85.00)", -85.00, true},
		{"C$-42.10", -42.10, true},
		{"-$5.00", -5.00, true},
		{"-45.25", -45.25, true},
		{"45-", -45, true},
		{"1234", 1234, true},
		{"1,234", 1234, true},
		{"0.00", 0, true},
		{"12.345", 12.345, true},
		{"1,234.5678", 1234.5678, true},
		{"  $7.99  ", 7.99, true},
		{"", 0, false},
		{"$", 0, false},
		{"C$", 0, false},
		{"abc", 0, false},
		{"1,23", 0, false},
		{"12,34,567", 0, false},
		{"1.23456", 0, false},
		{"1-2", 0, false},
		{"(1234", 0, false},
		{"1,234.56,78", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseAmount(c.in)
		if ok != c.ok {
			t.Errorf("ParseAmount(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if c.ok && math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ParseAmount(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDateCell(t *testing.T) {
	cases := []struct {
		in     string
		isISO  bool
		first  int
		second int
		ok     bool
	}{
		{"2026-01-05", true, 0, 0, true},
		{"2026/01/05", true, 0, 0, true},
		{"2025-1-5", true, 0, 0, true},
		{"2099-12-31", true, 0, 0, true},
		{"1900-01-01", true, 0, 0, true},
		{"2026-13-05", false, 0, 0, false},
		{"2026-01-32", false, 0, 0, false},
		{"1899-12-31", false, 0, 0, false},
		{"01/05/2026", false, 1, 5, true},
		{"05/13/2026", false, 5, 13, true},
		{"13/05/2026", false, 13, 5, true},
		{"31/12/2026", false, 31, 12, true},
		{"1/2/2026", false, 1, 2, true},
		{"01-05-2026", false, 1, 5, true},
		{"13/14/2026", false, 0, 0, false},
		{"00/01/2026", false, 0, 0, false},
		{"01/00/2026", false, 0, 0, false},
		{"2026", false, 0, 0, false},
		{"2026-01", false, 0, 0, false},
		{"2026-01-05 ", false, 0, 0, false},
		{"hello", false, 0, 0, false},
	}
	for _, c := range cases {
		isISO, first, second, ok := parseDateCell(c.in)
		if ok != c.ok || isISO != c.isISO || first != c.first || second != c.second {
			t.Errorf("parseDateCell(%q) = (%v, %d, %d, %v), want (%v, %d, %d, %v)",
				c.in, isISO, first, second, ok, c.isISO, c.first, c.second, c.ok)
		}
	}
}

func TestColumnKinds(t *testing.T) {
	in := "Date,Amount,Description,,Mixed\n" +
		"01/05/2026,-85.40,Coffee,,01/06/2026\n" +
		"01/06/2026,1200.00,Payroll,,42.00\n" +
		"01/07/2026,(12.00),Fee,,lunch\n"
	f := mustParse(t, in)
	want := []ColumnKind{KindDate, KindAmount, KindText, KindUnknown, KindText}
	if len(f.Columns) != len(want) {
		t.Fatalf("Columns = %+v", f.Columns)
	}
	for i, k := range want {
		if f.Columns[i].Kind != k {
			t.Errorf("column %d kind = %v, want %v", i, f.Columns[i].Kind, k)
		}
	}
}

func TestDateFormatClassification(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dd-mm decisive", "D\n13/05/2026\n14/05/2026\n", "dd-mm-yyyy"},
		{"mm-dd decisive", "D\n05/13/2026\n06/13/2026\n", "mm-dd-yyyy"},
		{"iso plurality", "D\n2026-01-05\n2026-01-06\n", "iso8601"},
		{"ambiguous default", "D\n01/05/2026\n01/06/2026\n", "mm-dd-yyyy"},
		{"iso beats slash plurality", "D\n2026-01-05\n2026-01-06\n01/07/2026\n", "iso8601"},
		{"slash beats iso plurality", "D\n01/05/2026\n01/06/2026\n2026-01-07\n", "mm-dd-yyyy"},
	}
	for _, c := range cases {
		f := mustParse(t, c.in)
		if f.Columns[0].Kind != KindDate {
			t.Fatalf("%s: kind = %v, want date", c.name, f.Columns[0].Kind)
		}
		if f.Columns[0].DateFormat != c.want {
			t.Errorf("%s: DateFormat = %q, want %q", c.name, f.Columns[0].DateFormat, c.want)
		}
	}
}

func TestRunningBalanceDetection(t *testing.T) {
	// Consistent: balance = previous balance + amount.
	cons := "Date,Amount,Balance\n" +
		"2026-01-05,-85.40,914.60\n" +
		"2026-01-06,2500.00,3414.60\n" +
		"2026-01-08,(12.50),3402.10\n" +
		"2026-01-09,-89.99,3312.11\n"
	f := mustParse(t, cons)
	if !f.Columns[2].RunningBalance {
		t.Errorf("balanced column not flagged: %+v", f.Columns[2])
	}
	if f.Columns[1].RunningBalance {
		t.Errorf("amount column wrongly flagged: %+v", f.Columns[1])
	}

	// Inverted convention: balance = previous balance - amount.
	inv := "Date,Amount,Balance\n" +
		"2026-01-05,85.40,914.60\n" +
		"2026-01-06,-2500.00,3414.60\n" +
		"2026-01-08,12.50,3402.10\n" +
		"2026-01-09,89.99,3312.11\n"
	f = mustParse(t, inv)
	if !f.Columns[2].RunningBalance {
		t.Errorf("inverted balance not flagged: %+v", f.Columns[2])
	}

	// Random: no recurrence.
	rand := "Date,Amount,Balance\n" +
		"2026-01-05,-85.40,100.00\n" +
		"2026-01-06,2500.00,777.77\n" +
		"2026-01-08,(12.50),321.00\n" +
		"2026-01-09,-89.99,55.55\n"
	f = mustParse(t, rand)
	if f.Columns[2].RunningBalance || f.Columns[1].RunningBalance {
		t.Errorf("random data flagged: %+v", f.Columns)
	}

	// Too short: fewer than three matched pairs.
	short := "Date,Amount,Balance\n2026-01-05,-85.40,914.60\n2026-01-06,2500.00,3414.60\n"
	f = mustParse(t, short)
	if f.Columns[2].RunningBalance {
		t.Errorf("2-row file flagged: %+v", f.Columns[2])
	}
}
