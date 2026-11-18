package plans

import (
	"testing"
)

func TestIsRegistered(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"rrsp", true},
		{"tfsa", true},
		{"fhsa", true},
		{"rrif", true},
		{"resp", true},
		{"rdsp", true},
		{"bank", false},
		{"cash", false},
		{"credit_card", false},
		{"mortgage", false},
		{"investment", false},
		{"other", false},
		{"RRSP", false}, // case-sensitive: matches accounts.type exactly
		{"tfsa ", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsRegistered(c.in); got != c.want {
			t.Errorf("IsRegistered(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsPlanType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"rrsp", true},
		{"tfsa", true},
		{"fhsa", true},
		{"rrif", true},
		{"resp", true},
		{"rdsp", true},
		{"investment", false},
		{"savings", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsPlanType(c.in); got != c.want {
			t.Errorf("IsPlanType(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAllPlanTypes(t *testing.T) {
	if len(AllPlanTypes) != 6 {
		t.Fatalf("len(AllPlanTypes) = %d, want 6", len(AllPlanTypes))
	}
	seen := map[PlanType]bool{}
	for _, p := range AllPlanTypes {
		if seen[p] {
			t.Errorf("duplicate plan type %q in AllPlanTypes", p)
		}
		seen[p] = true
		if !IsRegistered(string(p)) {
			t.Errorf("AllPlanTypes contains non-registered %q", p)
		}
	}
}

func TestTFSAStatutoryAnnualLimit(t *testing.T) {
	cases := []struct {
		year   int
		wantC  int64 // cents
		wantOK bool
	}{
		{2008, 0, false},
		{2009, 500_000, true},
		{2012, 500_000, true},
		{2013, 550_000, true},
		{2014, 550_000, true},
		{2015, 1_000_000, true},
		{2016, 550_000, true},
		{2018, 550_000, true},
		{2019, 600_000, true},
		{2022, 600_000, true},
		{2023, 650_000, true},
		{2024, 700_000, true},
		{2026, 700_000, true},
		{2027, 0, false}, // UNKNOWN: no pre-fill, override required
		{2028, 0, false},
	}
	for _, c := range cases {
		got, ok := TFSAStatutoryAnnualLimitCents(c.year)
		if ok != c.wantOK || (ok && got != c.wantC) {
			t.Errorf("TFSAStatutoryAnnualLimitCents(%d) = (%d, %v), want (%d, %v)",
				c.year, got, ok, c.wantC, c.wantOK)
		}
	}
}

func TestRRSPStatutoryAnnualCap(t *testing.T) {
	cases := []struct {
		year   int
		wantC  int64
		wantOK bool
	}{
		{2022, 0, false},
		{2023, 3_157_000, true},
		{2024, 3_279_000, true},
		{2025, 3_249_000, true},
		{2026, 3_381_000, true},
		{2027, 3_539_000, true},
		{2028, 3_639_000, true},
		{2029, 0, false},
	}
	for _, c := range cases {
		got, ok := RRSPStatutoryAnnualCapCents(c.year)
		if ok != c.wantOK || (ok && got != c.wantC) {
			t.Errorf("RRSPStatutoryAnnualCapCents(%d) = (%d, %v), want (%d, %v)",
				c.year, got, ok, c.wantC, c.wantOK)
		}
	}
}

func TestRRIFFactorBps(t *testing.T) {
	cases := []struct {
		age  int
		want int64
		ok   bool
	}{
		{-1, 0, false},
		{0, 111, true},  // 10000/90 = 111.11 -> 111 (display rounding; exact math used in the engine)
		{18, 139, true}, // 10000/72 = 138.89 -> 139
		{65, 400, true}, // 1/25
		{67, 435, true}, // 10000/23 = 434.78 -> 435
		{70, 500, true}, // 1/20
		{71, 528, true},
		{72, 540, true},
		{73, 553, true},
		{74, 567, true},
		{75, 582, true},
		{76, 598, true},
		{77, 617, true},
		{78, 636, true},
		{79, 658, true},
		{80, 682, true},
		{81, 708, true},
		{82, 738, true},
		{83, 771, true},
		{84, 808, true},
		{85, 851, true},
		{86, 899, true},
		{87, 955, true},
		{88, 1021, true},
		{89, 1099, true},
		{90, 1192, true},
		{91, 1306, true},
		{92, 1449, true},
		{93, 1634, true},
		{94, 1879, true},
		{95, 2000, true},
		{100, 2000, true},
	}
	for _, c := range cases {
		got, ok := RRIFMinimumFactorBps(c.age)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("RRIFMinimumFactorBps(%d) = (%d, %v), want (%d, %v)",
				c.age, got, ok, c.want, c.ok)
		}
	}
}
