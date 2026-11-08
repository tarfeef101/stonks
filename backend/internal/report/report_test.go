package report

import (
	"context"
	"fmt"
	"html"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	catG = "11111111-1111-1111-1111-111111111111"
	catD = "22222222-2222-2222-2222-222222222222"
	catS = "33333333-3333-3333-3333-333333333333"
)

// fixData is the golden fixture: monthly, as of 2026-10-15 UTC
// (October 2026, prior September 2026). Four categories (one
// uncategorized), budget targets on two (one over, one within), 12
// merchants with ties at every count, and a prior period. Dining out's
// spend is deliberately non-canonical ("180.1") to pin the
// re-canonicalization.
func fixData() Data {
	return Data{
		Period: PeriodMonthly,
		AsOf:   time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		CategorySpend: []CategorySpend{
			{CategoryID: catG, Spend: "612.30", Income: "0.00", Net: "-612.30"},
			{CategoryID: catD, Spend: "180.1", Income: "0.00", Net: "-180.1"},
			{CategoryID: catS, Spend: "0.00", Income: "4500.00", Net: "4500.00"},
			{CategoryID: "", Spend: "123.45", Income: "500.00", Net: "376.55"},
		},
		Merchants: fixMerchants(),
		PriorCategorySpend: []CategorySpend{
			{CategoryID: catG, Spend: "600.00", Income: "0.00", Net: "-600.00"},
			{CategoryID: catD, Spend: "400.00", Income: "0.00", Net: "-400.00"},
			{CategoryID: catS, Spend: "0.00", Income: "4500.00", Net: "4500.00"},
		},
		BudgetTargets: map[string]string{catG: "500.00", catD: "200.00"},
		CategoryNames: map[string]string{catG: "Groceries", catD: "Dining out", catS: "Salary"},
	}
}

// fixMerchants is the fixture top-merchant list, already ordered by
// count DESC, Name ASC — the shape TopMerchants produces.
func fixMerchants() []MerchantCount {
	return []MerchantCount{
		{Name: "Starbucks", Count: 5, Total: "68.25"},
		{Name: "Tim Hortons", Count: 4, Total: "52.10"},
		{Name: "Uber Eats", Count: 4, Total: "120.40"},
		{Name: "Metro", Count: 3, Total: "95.75"},
		{Name: "Shell", Count: 3, Total: "86.20"},
		{Name: "Walmart", Count: 3, Total: "41.00"},
		{Name: "Costco", Count: 2, Total: "210.15"},
		{Name: "Netflix", Count: 2, Total: "21.96"},
		{Name: "Spotify", Count: 2, Total: "10.99"},
		{Name: "Apple.com", Count: 1, Total: "34.99"},
		{Name: "Aurora Pharmacy", Count: 1, Total: "28.40"},
		{Name: "Cheezburger", Count: 1, Total: "15.00"},
	}
}

// fixReport is the exact Report Generate(fixData()) must produce.
func fixReport() Report {
	over, within := false, true
	return Report{
		Label:  "October 2026",
		From:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Income: "5000.00",
		Spend:  "915.85",
		Net:    "4084.15",
		ByCategory: []CategoryLine{
			{CategoryID: catG, Name: "Groceries", Spend: "612.30", Income: "0.00", Net: "-612.30", BudgetTarget: "500.00", WithinBudget: &over},
			{CategoryID: catD, Name: "Dining out", Spend: "180.10", Income: "0.00", Net: "-180.10", BudgetTarget: "200.00", WithinBudget: &within},
			{CategoryID: catS, Name: "Salary", Spend: "0.00", Income: "4500.00", Net: "4500.00"},
			{CategoryID: "", Name: "", Spend: "123.45", Income: "500.00", Net: "376.55"},
		},
		TopMerchants: fixMerchants(),
		Prior:        &PeriodTotals{Label: "September 2026", Income: "4500.00", Spend: "1000.00", Net: "3500.00"},
	}
}

func TestGenerateFixture(t *testing.T) {
	t.Run("fixture", func(t *testing.T) {
		want := fixReport()
		got := Generate(fixData())
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("Generate:\n got %+v\nwant %+v", got, want)
		}
	})
	t.Run("nil prior renders no comparison", func(t *testing.T) {
		d := fixData()
		d.PriorCategorySpend = nil
		want := fixReport()
		want.Prior = nil
		if got := Generate(d); !reflect.DeepEqual(want, got) {
			t.Fatalf("got %+v\nwant %+v", got, want)
		}
	})
	t.Run("empty non-nil prior renders zero totals", func(t *testing.T) {
		d := fixData()
		d.PriorCategorySpend = []CategorySpend{}
		want := fixReport()
		want.Prior = &PeriodTotals{Label: "September 2026", Income: "0.00", Spend: "0.00", Net: "0.00"}
		if got := Generate(d); !reflect.DeepEqual(want, got) {
			t.Fatalf("got %+v\nwant %+v", got, want)
		}
	})
}

func TestTopMerchants(t *testing.T) {
	rows := []Row{
		{Description: "B", Amount: "-1.00"},
		{Description: "A", Amount: "-2.00"},
		{Description: "A", Amount: "-3.00"},
		{Description: "", Amount: "-9.99"},  // empty description: skipped
		{Description: "A", Amount: "+5.00"}, // income: not a spend
		{Description: "C", Amount: "-0.50"},
		{Description: "B", Amount: "-4.00"},
	}
	want := []MerchantCount{
		{Name: "A", Count: 2, Total: "5.00"},
		{Name: "B", Count: 2, Total: "5.00"},
		{Name: "C", Count: 1, Total: "0.50"},
	}
	t.Run("ordering spend-only skip ties", func(t *testing.T) {
		if got := TopMerchants(rows, 10); !reflect.DeepEqual(want, got) {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	})
	t.Run("truncate to n", func(t *testing.T) {
		wantShort := want[:2]
		if got := TopMerchants(rows, 2); !reflect.DeepEqual(wantShort, got) {
			t.Fatalf("got %v\nwant %v", got, wantShort)
		}
	})
	t.Run("default n is 10", func(t *testing.T) {
		many := make([]Row, 0, 12)
		for i := 1; i <= 12; i++ {
			many = append(many, Row{Description: fmt.Sprintf("m%02d", i), Amount: "-1.00"})
		}
		got := TopMerchants(many, 0)
		if len(got) != 10 {
			t.Fatalf("len = %d, want 10: %v", len(got), got)
		}
		for i, m := range got {
			if m.Name != fmt.Sprintf("m%02d", i+1) {
				t.Fatalf("got %v, want m01..m10", got)
			}
		}
	})
	t.Run("empty input", func(t *testing.T) {
		if got := TopMerchants(nil, 10); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
	t.Run("income only", func(t *testing.T) {
		got := TopMerchants([]Row{{Description: "X", Amount: "+1.00"}}, 10)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

func TestRange(t *testing.T) {
	u := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return tm
	}
	d := time.Date // shorthand
	cases := []struct {
		name     string
		asOf     time.Time
		p        Period
		from, to time.Time
		label    string
	}{
		{"monthly mid-month", u("2026-01-15T12:00:00Z"), PeriodMonthly, d(2026, 1, 1, 0, 0, 0, 0, time.UTC), d(2026, 1, 31, 0, 0, 0, 0, time.UTC), "January 2026"},
		{"monthly leap Feb", u("2024-02-10T00:00:00Z"), PeriodMonthly, d(2024, 2, 1, 0, 0, 0, 0, time.UTC), d(2024, 2, 29, 0, 0, 0, 0, time.UTC), "February 2024"},
		{"monthly non-leap Feb", u("2026-02-28T23:59:59Z"), PeriodMonthly, d(2026, 2, 1, 0, 0, 0, 0, time.UTC), d(2026, 2, 28, 0, 0, 0, 0, time.UTC), "February 2026"},
		{"monthly year end", u("2026-12-31T23:59:59Z"), PeriodMonthly, d(2026, 12, 1, 0, 0, 0, 0, time.UTC), d(2026, 12, 31, 0, 0, 0, 0, time.UTC), "December 2026"},
		{"monthly UTC wins over local zone", u("2026-01-01T04:30:00Z").In(time.FixedZone("", -5*3600)), PeriodMonthly, d(2026, 1, 1, 0, 0, 0, 0, time.UTC), d(2026, 1, 31, 0, 0, 0, 0, time.UTC), "January 2026"},
		{"quarter Dec 31", u("2026-12-31T00:00:00Z"), PeriodQuarterly, d(2026, 10, 1, 0, 0, 0, 0, time.UTC), d(2026, 12, 31, 0, 0, 0, 0, time.UTC), "Q4 2026"},
		{"quarter Jan 1", u("2026-01-01T00:00:00Z"), PeriodQuarterly, d(2026, 1, 1, 0, 0, 0, 0, time.UTC), d(2026, 3, 31, 0, 0, 0, 0, time.UTC), "Q1 2026"},
		{"quarter Mar 31", u("2026-03-31T18:00:00Z"), PeriodQuarterly, d(2026, 1, 1, 0, 0, 0, 0, time.UTC), d(2026, 3, 31, 0, 0, 0, 0, time.UTC), "Q1 2026"},
		{"quarter Apr 1", u("2026-04-01T00:00:00Z"), PeriodQuarterly, d(2026, 4, 1, 0, 0, 0, 0, time.UTC), d(2026, 6, 30, 0, 0, 0, 0, time.UTC), "Q2 2026"},
		{"quarter Jul 15", u("2026-07-15T09:00:00Z"), PeriodQuarterly, d(2026, 7, 1, 0, 0, 0, 0, time.UTC), d(2026, 9, 30, 0, 0, 0, 0, time.UTC), "Q3 2026"},
		{"annual mid-year", u("2026-06-15T00:00:00Z"), PeriodAnnual, d(2026, 1, 1, 0, 0, 0, 0, time.UTC), d(2026, 12, 31, 0, 0, 0, 0, time.UTC), "2026"},
		{"annual millennium edge", u("2000-01-01T00:00:00Z"), PeriodAnnual, d(2000, 1, 1, 0, 0, 0, 0, time.UTC), d(2000, 12, 31, 0, 0, 0, 0, time.UTC), "2000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, label := Range(tc.asOf, tc.p)
			if !from.Equal(tc.from) || !to.Equal(tc.to) || label != tc.label {
				t.Fatalf("Range(%v, %q) = (%v, %v, %q), want (%v, %v, %q)",
					tc.asOf, tc.p, from, to, label, tc.from, tc.to, tc.label)
			}
			if from.Location() != time.UTC || to.Location() != time.UTC {
				t.Fatalf("from/to must be UTC, got %v / %v", from.Location(), to.Location())
			}
		})
	}
}

func TestRenderFixture(t *testing.T) {
	r := Generate(fixData())

	txt, err := RenderText(r)
	if err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	for _, want := range []string{
		"Personal Finance Report",
		"October 2026",
		"from 2026-10-01 to 2026-10-31",
		"Totals",
		"+5000.00",
		"-915.85",
		"+4084.15",
		"By category",
		"Groceries",
		"Dining out",
		"Salary",
		"Uncategorized",
		"over by 112.30",
		"within budget",
		"Top merchants",
		"Starbucks",
		"Aurora Pharmacy",
		"vs September 2026",
		"+584.15",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("text missing %q:\n%s", want, txt)
		}
	}

	h, err := RenderHTML(r)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	for _, want := range []string{
		"<!DOCTYPE html>",
		"max-width:560px",
		"October 2026",
		"from 2026-10-01 to 2026-10-31",
		"Groceries",
		"Dining out",
		"Salary",
		"Uncategorized",
		"over by 112.30",
		"within budget",
		"Starbucks",
		"Aurora Pharmacy",
		"vs September 2026",
		"+500.00",
		"-84.15",
		"+584.15",
		"Personal Finance Tracker",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("html missing %q", want)
		}
	}
	if strings.Contains(h, "<style") {
		t.Errorf("inline CSS only: html must not contain a <style> element")
	}

	dir := "/tmp/opencode/e2e_82"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(dir+"/sample_report.txt", []byte(txt), 0o644); err != nil {
		t.Fatalf("write sample_report.txt: %v", err)
	}
	if err := os.WriteFile(dir+"/sample_report.html", []byte(h), 0o644); err != nil {
		t.Fatalf("write sample_report.html: %v", err)
	}
}

// fakeTx is a scriptable TxSource that records every call so Collect's
// page walk and window choices can be asserted.
type fakeTx struct {
	current   []CategorySpend
	prior     []CategorySpend
	rows      []Row
	aggCalls  []aggCall
	listCalls []ListFilter
}

type aggCall struct {
	from, to time.Time
	accounts []string
}

func (f *fakeTx) Aggregate(_ context.Context, _ string, from, to time.Time, accountIDs []string) ([]CategorySpend, error) {
	f.aggCalls = append(f.aggCalls, aggCall{from: from, to: to, accounts: accountIDs})
	if from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		return f.current, nil
	}
	return f.prior, nil
}

func (f *fakeTx) List(_ context.Context, _ string, fl ListFilter, _ []byte) ([]Row, error) {
	f.listCalls = append(f.listCalls, fl)
	end := fl.Offset + txPageSize
	if end > len(f.rows) {
		end = len(f.rows)
	}
	if fl.Offset >= len(f.rows) {
		return nil, nil
	}
	return f.rows[fl.Offset:end], nil
}

type fakeBudgets struct{ budgets []Budget }

func (f fakeBudgets) List(_ context.Context, _ string, _ []byte) ([]Budget, error) {
	return f.budgets, nil
}

type fakeCats struct{ cats []Name }

func (f fakeCats) List(_ context.Context, _ string, _ []byte) ([]Name, error) {
	return f.cats, nil
}

func TestCollect(t *testing.T) {
	asOf := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	oct31 := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sep30 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	current := []CategorySpend{
		{CategoryID: catG, Spend: "612.30", Income: "0.00", Net: "-612.30"},
		{CategoryID: catS, Spend: "0.00", Income: "4500.00", Net: "4500.00"},
	}
	prior := []CategorySpend{
		{CategoryID: catG, Spend: "600.00", Income: "0.00", Net: "-600.00"},
	}
	// 250 rows: 3 merchant descriptions, interleaved income and
	// empty-description rows; pages of 100 -> 100/100/50.
	merch := []string{"Alpha Market", "Beta Coffee", "Gamma Fuel"}
	rows := make([]Row, 0, 250)
	for i := 0; i < 250; i++ {
		switch {
		case i%7 == 0:
			rows = append(rows, Row{Description: "", Amount: "-1.00"})
		case i%3 == 0:
			rows = append(rows, Row{Description: "Payroll deposit", Amount: "+25.00"})
		default:
			rows = append(rows, Row{Description: merch[i%3], Amount: fmt.Sprintf("-%d.%02d", 1+i%9, (i*7)%100)})
		}
	}
	budgets := []Budget{
		{Period: "month", Active: false, Targets: map[string]string{catG: "1.00"}},
		{Period: "quarter", Active: true, Targets: map[string]string{catD: "2.00"}},
		{Period: "month", Active: true, Targets: map[string]string{catG: "500.00", catD: "200.00"}},
	}
	cats := []Name{{ID: catG, Name: "Groceries"}, {ID: catD, Name: "Dining out"}}

	t.Run("walks pages and wires data", func(t *testing.T) {
		tx := &fakeTx{current: current, prior: prior, rows: rows}
		col := &Collector{Tx: tx, Budget: fakeBudgets{budgets: budgets}, Category: fakeCats{cats: cats}}
		data, err := col.Collect(context.Background(), "user-1", PeriodMonthly, asOf, []byte("dek"))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if data.Period != PeriodMonthly || !data.AsOf.Equal(asOf) {
			t.Errorf("Period/AsOf = %v/%v", data.Period, data.AsOf)
		}
		if !reflect.DeepEqual(data.CategorySpend, current) {
			t.Errorf("CategorySpend = %v, want %v", data.CategorySpend, current)
		}
		if !reflect.DeepEqual(data.PriorCategorySpend, prior) {
			t.Errorf("PriorCategorySpend = %v, want %v", data.PriorCategorySpend, prior)
		}
		if want := TopMerchants(rows, 10); !reflect.DeepEqual(data.Merchants, want) {
			t.Errorf("Merchants = %v, want %v", data.Merchants, want)
		}
		if len(data.Merchants) > 10 {
			t.Errorf("Merchants has %d entries, want <= 10", len(data.Merchants))
		}
		wantTargets := map[string]string{catG: "500.00", catD: "200.00"}
		if !reflect.DeepEqual(data.BudgetTargets, wantTargets) {
			t.Errorf("BudgetTargets = %v, want %v (first active month match)", data.BudgetTargets, wantTargets)
		}
		wantNames := map[string]string{catG: "Groceries", catD: "Dining out"}
		if !reflect.DeepEqual(data.CategoryNames, wantNames) {
			t.Errorf("CategoryNames = %v, want %v", data.CategoryNames, wantNames)
		}

		if len(tx.aggCalls) != 2 {
			t.Fatalf("Aggregate called %d times, want 2", len(tx.aggCalls))
		}
		if !tx.aggCalls[0].from.Equal(oct1) || !tx.aggCalls[0].to.Equal(oct31) || tx.aggCalls[0].accounts != nil {
			t.Errorf("first Aggregate = (%v, %v, %v), want (Oct 1, Oct 31, nil)",
				tx.aggCalls[0].from, tx.aggCalls[0].to, tx.aggCalls[0].accounts)
		}
		if !tx.aggCalls[1].from.Equal(sep1) || !tx.aggCalls[1].to.Equal(sep30) {
			t.Errorf("second Aggregate = (%v, %v), want (Sep 1, Sep 30)",
				tx.aggCalls[1].from, tx.aggCalls[1].to)
		}
		if len(tx.listCalls) != 3 {
			t.Fatalf("List called %d times, want 3 (250 rows / page 100)", len(tx.listCalls))
		}
		for i, wantOff := range []int{0, 100, 200} {
			if !tx.listCalls[i].From.Equal(oct1) || !tx.listCalls[i].To.Equal(oct31) || tx.listCalls[i].Offset != wantOff {
				t.Errorf("List[%d] = %v, want From=Oct 1 To=Oct 31 Offset=%d", i, tx.listCalls[i], wantOff)
			}
		}
	})

	t.Run("empty prior aggregate normalizes to nil", func(t *testing.T) {
		tx := &fakeTx{current: current, prior: []CategorySpend{}, rows: rows[:10]}
		col := &Collector{Tx: tx, Budget: fakeBudgets{budgets: budgets}, Category: fakeCats{cats: cats}}
		data, err := col.Collect(context.Background(), "user-1", PeriodMonthly, asOf, []byte("dek"))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if data.PriorCategorySpend != nil {
			t.Errorf("PriorCategorySpend = %v, want nil", data.PriorCategorySpend)
		}
	})

	t.Run("no matching active budget gives empty map", func(t *testing.T) {
		tx := &fakeTx{current: current, prior: prior, rows: rows[:10]}
		other := []Budget{
			{Period: "month", Active: false, Targets: map[string]string{catG: "1.00"}},
			{Period: "year", Active: true, Targets: map[string]string{catG: "9.99"}},
		}
		col := &Collector{Tx: tx, Budget: fakeBudgets{budgets: other}, Category: fakeCats{cats: cats}}
		data, err := col.Collect(context.Background(), "user-1", PeriodMonthly, asOf, []byte("dek"))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if data.BudgetTargets == nil || len(data.BudgetTargets) != 0 {
			t.Errorf("BudgetTargets = %v, want empty non-nil map", data.BudgetTargets)
		}
	})

	t.Run("unknown period is an error", func(t *testing.T) {
		tx := &fakeTx{current: current, prior: prior, rows: rows[:10]}
		col := &Collector{Tx: tx, Budget: fakeBudgets{}, Category: fakeCats{}}
		if _, err := col.Collect(context.Background(), "user-1", Period("weekly"), asOf, nil); err == nil {
			t.Fatal("want error for unknown period")
		} else if !strings.HasPrefix(err.Error(), "report: ") {
			t.Fatalf("error %q must carry the report: prefix", err)
		}
	})
}

func TestRenderEscape(t *testing.T) {
	r := Report{
		Label:  "October 2026",
		From:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Income: "0.00",
		Spend:  "5.00",
		Net:    "-5.00",
		ByCategory: []CategoryLine{
			{CategoryID: "c1", Name: "<b>Evil</b> & Co", Spend: "5.00", Income: "0.00", Net: "-5.00"},
		},
		TopMerchants: []MerchantCount{
			{Name: `A & B <script>alert(1)</script>`, Count: 1, Total: "5.00"},
		},
	}
	h, err := RenderHTML(r)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	escapedName := html.EscapeString("<b>Evil</b> & Co")
	escapedMerch := html.EscapeString(`A & B <script>alert(1)</script>`)
	if !strings.Contains(h, escapedName) {
		t.Errorf("html missing escaped category name %q", escapedName)
	}
	if !strings.Contains(h, escapedMerch) {
		t.Errorf("html missing escaped merchant name %q", escapedMerch)
	}
	if strings.Contains(h, "<b>Evil</b>") || strings.Contains(h, "<script>") {
		t.Errorf("html contains an unescaped user string:\n%s", h)
	}

	txt, err := RenderText(r)
	if err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	if !strings.Contains(txt, "<b>Evil</b> & Co") {
		t.Errorf("text must carry the raw (unescaped) name:\n%s", txt)
	}
}

func TestRenderInvalidMoney(t *testing.T) {
	r := Report{
		Label:  "October 2026",
		From:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Income: "abc",
		Spend:  "0.00",
		Net:    "0.00",
	}
	if _, err := RenderText(r); err == nil || !strings.HasPrefix(err.Error(), "report: ") {
		t.Errorf("RenderText: want report: error, got %v", err)
	}
	if _, err := RenderHTML(r); err == nil || !strings.HasPrefix(err.Error(), "report: ") {
		t.Errorf("RenderHTML: want report: error, got %v", err)
	}
}
