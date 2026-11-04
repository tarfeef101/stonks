package csvparse

import (
	"strings"
	"testing"
)

// top returns the best suggestion for field, failing the test when the
// field has no candidates at all.
func top(t *testing.T, f *File, field Field) ScoredColumn {
	t.Helper()
	for _, s := range Suggest(f) {
		if s.Field == field {
			if len(s.Columns) == 0 {
				t.Fatalf("no candidates for %s", field)
			}
			return s.Columns[0]
		}
	}
	t.Fatalf("no suggestion for field %s", field)
	return ScoredColumn{}
}

func expectTop(t *testing.T, f *File, field Field, index int, heading string) {
	t.Helper()
	c := top(t, f, field)
	if c.Index != index || c.Heading != heading {
		t.Errorf("top(%s) = (%d, %q, %v), want (%d, %q)", field, c.Index, c.Heading, c.Score, index, heading)
	}
}

// Synthetic bank-style samples for the 4.2 done-when. These are
// style fixtures, not verified exports: the real per-bank headers and
// formats are captured in step 4.4.

func parseRBC(t *testing.T) *File {
	t.Helper()
	return mustParse(t, strings.Join([]string{
		"Transaction Date,Description,Amount",
		`01/05/2026,"Grocery Store, Ltd.",-$85.40`,
		"01/06/2026,Payroll Deposit,$2500.00",
		"01/08/2026,Coffee,(12.50)",
		"01/09/2026,Utility Bill,-$89.99",
	}, "\n"))
}

func TestSuggestRBCStyle(t *testing.T) {
	f := parseRBC(t)
	if f.Delimiter != ',' {
		t.Fatalf("Delimiter = %q", f.Delimiter)
	}
	if f.Columns[0].Kind != KindDate || f.Columns[0].DateFormat != "mm-dd-yyyy" {
		t.Fatalf("date column = %+v", f.Columns[0])
	}
	if f.Rows[0][1] != `Grocery Store, Ltd.` {
		t.Fatalf("quoted row = %v", f.Rows[0])
	}
	if f.Columns[2].Kind != KindAmount {
		t.Fatalf("amount column = %+v", f.Columns[2])
	}
	expectTop(t, f, FieldDatePosted, 0, "Transaction Date")
	expectTop(t, f, FieldDateExecuted, 0, "Transaction Date")
	expectTop(t, f, FieldAmount, 2, "Amount")
	expectTop(t, f, FieldDescription, 1, "Description")
	expectTop(t, f, FieldCategory, 1, "Description")
	expectTop(t, f, FieldRunningBalance, 2, "Amount")
}

func parseTD(t *testing.T) *File {
	t.Helper()
	return mustParse(t, strings.Join([]string{
		"Date,Description,Amount,Balance",
		"2026-01-05,Grocery,-85.40,914.60",
		"2026-01-06,Payroll,2500.00,3414.60",
		"2026-01-08,Coffee,(12.50),3402.10",
		"2026-01-09,Utility,-89.99,3312.11",
	}, "\n"))
}

func TestSuggestTDStyle(t *testing.T) {
	f := parseTD(t)
	if f.Columns[0].DateFormat != "iso8601" {
		t.Fatalf("date format = %q", f.Columns[0].DateFormat)
	}
	if !f.Columns[3].RunningBalance || f.Columns[2].RunningBalance {
		t.Fatalf("running balance flags: %+v", f.Columns)
	}
	expectTop(t, f, FieldDatePosted, 0, "Date")
	expectTop(t, f, FieldAmount, 2, "Amount")
	expectTop(t, f, FieldRunningBalance, 3, "Balance")
	expectTop(t, f, FieldDescription, 1, "Description")
	expectTop(t, f, FieldCategory, 1, "Description")
	// Within the amount field, the plain amount column must outrank
	// the flagged balance column (3.0+2.0 vs 1.0).
	var amtScore, balScore float64
	for _, s := range Suggest(f) {
		if s.Field != FieldAmount {
			continue
		}
		for _, c := range s.Columns {
			switch c.Index {
			case 2:
				amtScore = c.Score
			case 3:
				balScore = c.Score
			}
		}
	}
	if !(amtScore > balScore) {
		t.Errorf("amount field: col2 score %v should exceed balance col3 score %v", amtScore, balScore)
	}
}

func parseScotia(t *testing.T) *File {
	t.Helper()
	return mustParse(t, strings.Join([]string{
		"Date;Description;Montant;Solde",
		"25/01/2026;Épicerie du coin;-85.40;914.60",
		"26/01/2026;Virement salariale;2500.00;3414.60",
		"31/01/2026;Café;(12.50);3402.10",
		"01/02/2026;Facture électricité;-89.99;3312.11",
	}, "\n"))
}

func TestSuggestScotiaStyle(t *testing.T) {
	f := parseScotia(t)
	if f.Delimiter != ';' {
		t.Fatalf("Delimiter = %q", f.Delimiter)
	}
	if f.Columns[0].DateFormat != "dd-mm-yyyy" {
		t.Fatalf("date format = %q, want dd-mm-yyyy", f.Columns[0].DateFormat)
	}
	if f.Rows[0][1] != "Épicerie du coin" {
		t.Fatalf("accented cell = %q", f.Rows[0][1])
	}
	if !f.Columns[3].RunningBalance {
		t.Fatalf("Solde not flagged: %+v", f.Columns[3])
	}
	expectTop(t, f, FieldDatePosted, 0, "Date")
	expectTop(t, f, FieldAmount, 2, "Montant")
	expectTop(t, f, FieldRunningBalance, 3, "Solde")
	expectTop(t, f, FieldDescription, 1, "Description")
}

func parseCIBC(t *testing.T) *File {
	t.Helper()
	return mustParse(t, strings.Join([]string{
		"Transaction Date\tDescription\tAmount",
		"01/05/2026\tDirect Deposit\t1500.00",
		"01/06/2026\tLunch at Cafe\t-23.45",
		"01/07/2026\tATM Withdrawal\t-100.00",
		"01/08/2026\tPhone Bill\t-60.00",
	}, "\n"))
}

func TestSuggestCIBCStyle(t *testing.T) {
	f := parseCIBC(t)
	if f.Delimiter != '\t' {
		t.Fatalf("Delimiter = %q, want tab", f.Delimiter)
	}
	expectTop(t, f, FieldDatePosted, 0, "Transaction Date")
	expectTop(t, f, FieldAmount, 2, "Amount")
	expectTop(t, f, FieldDescription, 1, "Description")
}

func parseBMO(t *testing.T) *File {
	t.Helper()
	return mustParse(t, strings.Join([]string{
		"2026-01-05,Coffee shop,-4.50",
		"2026-01-06,Salary,1800.00",
		"2026-01-07,Rent,-1200.00",
		"2026-01-08,Groceries,-75.25",
	}, "\n"))
}

func TestSuggestBMOStyle(t *testing.T) {
	f := parseBMO(t)
	if f.HasHeader || f.Header != nil {
		t.Fatalf("HasHeader = %v, Header = %v", f.HasHeader, f.Header)
	}
	if len(f.Rows) != 4 {
		t.Fatalf("Rows = %v", f.Rows)
	}
	expectTop(t, f, FieldDatePosted, 0, "")
	expectTop(t, f, FieldAmount, 2, "")
	expectTop(t, f, FieldDescription, 1, "")
	expectTop(t, f, FieldRunningBalance, 2, "")
}

func parseDesjardins(t *testing.T) *File {
	t.Helper()
	body := strings.Join([]string{
		"Date,Libellé,Montant",
		"01/25/2026,Épicerie,C$-85.40",
		"01/26/2026,Salaire,C$2500.00",
		"01/27/2026,Café,C$-12.50",
		"01/28/2026,Fourniture électricité,C$-89.99",
	}, "\n")
	return mustParse(t, "\ufeff"+body)
}

func TestSuggestDesjardinsStyle(t *testing.T) {
	f := parseDesjardins(t)
	if f.Encoding != "utf-8" {
		t.Fatalf("Encoding = %q", f.Encoding)
	}
	if f.Header[0] != "Date" {
		t.Fatalf("BOM leaked into header: %q", f.Header[0])
	}
	if f.Columns[0].DateFormat != "mm-dd-yyyy" {
		t.Fatalf("date format = %q", f.Columns[0].DateFormat)
	}
	if f.Columns[2].Kind != KindAmount {
		t.Fatalf("C$ amounts not recognized: %+v", f.Columns[2])
	}
	expectTop(t, f, FieldDatePosted, 0, "Date")
	expectTop(t, f, FieldAmount, 2, "Montant")
	expectTop(t, f, FieldDescription, 1, "Libellé")
}

func TestSuggestHeadingBoosts(t *testing.T) {
	in := strings.Join([]string{
		"Posting Date,Description,Amount,Category",
		"01/05/2026,Coffee,-4.50,Food",
		"01/06/2026,Salary,1800.00,Income",
		"01/07/2026,Rent,-1200.00,Housing",
	}, "\n")
	f := mustParse(t, in)
	// "Posting Date" beats a plain "Date" for date_posted (post +2.5).
	c := top(t, f, FieldDatePosted)
	if c.Score < 5.5 {
		t.Errorf("Posting Date score = %v, want >= 5.5", c.Score)
	}
	// The explicit Category column outranks the generic description.
	expectTop(t, f, FieldCategory, 3, "Category")
}
