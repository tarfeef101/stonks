package plans

import (
	"slices"
	"testing"
	"time"
)

// All money values in these tests are integer cents.

func ip(v int) *int { return &v }

func i64p(v int64) *int64 { return &v }

func ts(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	return &t
}

// getCard returns the card for (plan, beneficiary) or fails the test.
func getCard(t *testing.T, cards []Card, pt PlanType, ben string) Card {
	t.Helper()
	for _, c := range cards {
		if c.PlanType == pt && c.Beneficiary == ben {
			return c
		}
	}
	t.Fatalf("no card for %s %q in %v", pt, ben, cards)
	return Card{}
}

func hasCard(t *testing.T, cards []Card, pt PlanType, ben string) bool {
	t.Helper()
	for _, c := range cards {
		if c.PlanType == pt && c.Beneficiary == ben {
			return true
		}
	}
	return false
}

func checkI64(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", label, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", label, *got, want)
	}
}

func checkNilI64(t *testing.T, label string, got *int64) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %d, want nil", label, *got)
	}
}

// --- TFSA -------------------------------------------------------------------

// Basic 2024: $7,000 limit, $3,000 used, $1,500 withdrawn in 2023 re-creates
// room this year, $1,000 withdrawn this year re-creates room next year. Grant
// inflows never count.
func TestTFSA_RoomAndWithdrawalReCreation(t *testing.T) {
	in := Input{
		Year:            2024,
		HolderBirthYear: ip(1990),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
		Sums: []YearSums{
			{AccountID: "a1", Year: 2024, CountedContributions: 300_000, Withdrawals: 100_000, GrantInflows: 9_999_999},
			{AccountID: "a1", Year: 2023, Withdrawals: 150_000},
		},
	}
	c := getCard(t, Compute(in).Cards, PlanTFSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 700_000)
	if c.LimitSource != LimitSourceStatutory {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceStatutory)
	}
	if c.Used != 300_000 {
		t.Fatalf("Used = %d, want 300000 (grants must not count)", c.Used)
	}
	checkI64(t, "Room", c.Room, 700_000-300_000+150_000) // = 550_000
	if c.ReCreatedNextYear != 100_000 {
		t.Fatalf("ReCreatedNextYear = %d, want 100000", c.ReCreatedNextYear)
	}
	if !slices.Equal(c.Warnings, nil) || !slices.Equal(c.Notes, nil) {
		t.Fatalf("unexpected Warnings=%v Notes=%v", c.Warnings, c.Notes)
	}
}

func TestTFSA_OverLimit(t *testing.T) {
	in := Input{
		Year:            2024,
		HolderBirthYear: ip(1990),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
		Sums:            []YearSums{{AccountID: "a1", Year: 2024, CountedContributions: 800_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanTFSA, "")
	checkI64(t, "Room", c.Room, -100_000)
	if !slices.Equal(c.Warnings, []string{WarningOverLimit}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningOverLimit)
	}
}

// 2027 is not in the statutory table: no override -> limit unknown, no room.
func TestTFSA_LimitUnknown2027(t *testing.T) {
	in := Input{
		Year:            2027,
		HolderBirthYear: ip(1990),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
	}
	c := getCard(t, Compute(in).Cards, PlanTFSA, "")
	checkNilI64(t, "AnnualLimit", c.AnnualLimit)
	checkNilI64(t, "Room", c.Room)
	if c.LimitSource != LimitSourceNone {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceNone)
	}
	if !slices.Equal(c.Warnings, []string{WarningLimitUnknown}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningLimitUnknown)
	}
}

// An override beats the (missing) statutory value.
func TestTFSA_OverrideWins(t *testing.T) {
	in := Input{
		Year:            2027,
		HolderBirthYear: ip(1990),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
		Sums:            []YearSums{{AccountID: "a1", Year: 2027, CountedContributions: 150_000}},
		Overrides:       []Override{{PlanType: PlanTFSA, Year: 2027, AnnualLimit: 500_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanTFSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 500_000)
	if c.LimitSource != LimitSourceOverride {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceOverride)
	}
	checkI64(t, "Room", c.Room, 350_000)
	if !slices.Equal(c.Warnings, nil) {
		t.Fatalf("Warnings = %v, want none", c.Warnings)
	}
}

// Holder born 2007 is 17 in 2024 -> tfsa_min_age.
func TestTFSA_MinAge(t *testing.T) {
	in := Input{
		Year:            2024,
		HolderBirthYear: ip(2007),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
	}
	c := getCard(t, Compute(in).Cards, PlanTFSA, "")
	if !slices.Equal(c.Warnings, []string{WarningTFSAMinAge}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningTFSAMinAge)
	}
}

// --- RRSP --------------------------------------------------------------------

// 2026 cap $33,810; $10,000 used. Both notes fire (recontribution always,
// actual-limit because the statutory cap is not the personal limit).
func TestRRSP_Room2026(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1980),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
		Sums:            []YearSums{{AccountID: "a1", Year: 2026, CountedContributions: 1_000_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 3_381_000)
	if c.LimitSource != LimitSourceStatutory {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceStatutory)
	}
	checkI64(t, "Room", c.Room, 2_381_000)
	if !slices.Equal(c.Notes, []string{NoteRRSPRecontribution, NoteRRSPActualLimit}) {
		t.Fatalf("Notes = %v", c.Notes)
	}
	if !slices.Equal(c.Warnings, nil) {
		t.Fatalf("Warnings = %v, want none", c.Warnings)
	}
}

// An override removes the actual-limit note (the user has stated the real
// personal limit) and the room follows the override.
func TestRRSP_Override(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1980),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
		Sums:            []YearSums{{AccountID: "a1", Year: 2026, CountedContributions: 1_000_000}},
		Overrides:       []Override{{PlanType: PlanRRSP, Year: 2026, AnnualLimit: 2_500_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 2_500_000)
	if c.LimitSource != LimitSourceOverride {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceOverride)
	}
	checkI64(t, "Room", c.Room, 1_500_000)
	if !slices.Equal(c.Notes, []string{NoteRRSPRecontribution}) {
		t.Fatalf("Notes = %v, want [%s]", c.Notes, NoteRRSPRecontribution)
	}
}

// 2025 withdrawal re-creates 2026 room.
func TestRRSP_PriorYearWithdrawalReCreatesRoom(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1980),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
		Sums: []YearSums{
			{AccountID: "a1", Year: 2026, CountedContributions: 200_000},
			{AccountID: "a1", Year: 2025, Withdrawals: 100_000},
		},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	checkI64(t, "Room", c.Room, 3_381_000-200_000+100_000) // = 3_281_000
}

// 2022 is outside the confirmed cap range and there is no override.
func TestRRSP_Unknown2022(t *testing.T) {
	in := Input{
		Year:            2022,
		HolderBirthYear: ip(1980),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	checkNilI64(t, "AnnualLimit", c.AnnualLimit)
	checkNilI64(t, "Room", c.Room)
	if c.LimitSource != LimitSourceNone {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceNone)
	}
	if !slices.Equal(c.Warnings, []string{WarningLimitUnknown}) {
		t.Fatalf("Warnings = %v", c.Warnings)
	}
	if !slices.Equal(c.Notes, []string{NoteRRSPRecontribution, NoteRRSPActualLimit}) {
		t.Fatalf("Notes = %v", c.Notes)
	}
}

// Holder born 1955 turns 71 in 2026.
func TestRRSP_Age71(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1955),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	if !slices.Equal(c.Warnings, []string{WarningRRSPA71}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningRRSPA71)
	}
}

func TestRRSP_OverLimit(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1980),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRSP}},
		Sums:            []YearSums{{AccountID: "a1", Year: 2026, CountedContributions: 4_000_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRSP, "")
	checkI64(t, "Room", c.Room, -619_000)
	if !slices.Equal(c.Warnings, []string{WarningOverLimit}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningOverLimit)
	}
}

// --- FHSA --------------------------------------------------------------------

// $5,000 contributed in 2023 -> $3,000 of the $8,000 limit carries into 2024:
// effective 2024 limit $11,000.
func TestFHSA_CarryForward(t *testing.T) {
	in := Input{
		Year:     2024,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
		Sums:     []YearSums{{AccountID: "a1", Year: 2023, CountedContributions: 500_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 1_100_000)
	if c.LimitSource != LimitSourceStatutory {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceStatutory)
	}
	checkI64(t, "Room", c.Room, 1_100_000)
	checkI64(t, "LifetimeLimit", c.LifetimeLimit, 4_000_000)
	checkI64(t, "LifetimeUsed", c.LifetimeUsed, 500_000)
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 3_500_000)
	if !slices.Equal(c.Warnings, nil) || !slices.Equal(c.Notes, nil) {
		t.Fatalf("unexpected Warnings=%v Notes=%v", c.Warnings, c.Notes)
	}
}

// Nothing contributed in 2023 -> the full $8,000 cap carries: $16,000 in 2024.
func TestFHSA_FullCarryForward(t *testing.T) {
	in := Input{
		Year:     2024,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 1_600_000)
	checkI64(t, "Room", c.Room, 1_600_000)
}

// $30,000 contributed over the plan's life: lifetime room $10,000; the
// carry-forward floor clamps at zero, so the 2024 limit is the base $8,000.
func TestFHSA_Lifetime(t *testing.T) {
	in := Input{
		Year:     2024,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
		Sums:     []YearSums{{AccountID: "a1", Year: 2023, CountedContributions: 3_000_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 800_000)
	checkI64(t, "LifetimeUsed", c.LifetimeUsed, 3_000_000)
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 1_000_000)
}

// 15-year window closed: account opened 2023, target year 2038. The 2030-2038
// statutory gap is bridged by continuous overrides, so the chain stays known.
func TestFHSA_WindowNote(t *testing.T) {
	var ovs []Override
	for y := 2030; y <= 2038; y++ {
		ovs = append(ovs, Override{PlanType: PlanFHSA, Year: y, AnnualLimit: 800_000})
	}
	in := Input{
		Year:      2038,
		Accounts:  []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 15)}},
		Overrides: ovs,
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkI64(t, "AnnualLimit", c.AnnualLimit, 800_000)
	if c.LimitSource != LimitSourceOverride {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceOverride)
	}
	if !slices.Equal(c.Notes, []string{NoteFHSAWindow}) {
		t.Fatalf("Notes = %v, want [%s]", c.Notes, NoteFHSAWindow)
	}
	if !slices.Equal(c.Warnings, nil) {
		t.Fatalf("Warnings = %v, want none", c.Warnings)
	}
}

// Before the plan existed there is no card at all.
func TestFHSA_BeforeLaunchNoCard(t *testing.T) {
	in := Input{
		Year:     2022,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2022, time.June, 1)}},
	}
	if hasCard(t, Compute(in).Cards, PlanFHSA, "") {
		t.Fatal("FHSA card emitted for 2022; want none")
	}
}

// 2030 without an override: the confirmed chain stops -> limit unknown, but
// the lifetime fields still compute.
func TestFHSA_Unknown2030(t *testing.T) {
	in := Input{
		Year:     2030,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
		Sums:     []YearSums{{AccountID: "a1", Year: 2023, CountedContributions: 100_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkNilI64(t, "AnnualLimit", c.AnnualLimit)
	checkNilI64(t, "Room", c.Room)
	if c.LimitSource != LimitSourceNone {
		t.Fatalf("LimitSource = %q, want %q", c.LimitSource, LimitSourceNone)
	}
	if !slices.Equal(c.Warnings, []string{WarningLimitUnknown}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningLimitUnknown)
	}
	checkI64(t, "LifetimeLimit", c.LifetimeLimit, 4_000_000)
	checkI64(t, "LifetimeUsed", c.LifetimeUsed, 100_000)
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 3_900_000)
}

// An account opened before the 2023-04-01 launch is a data error.
func TestFHSA_PreLaunchOpened(t *testing.T) {
	in := Input{
		Year:     2024,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2022, time.January, 1)}},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	if !slices.Equal(c.Warnings, []string{WarningFHSAPreLaunch}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningFHSAPreLaunch)
	}
}

// $10,000 contributed in the first year: 2023 room is -$2,000 (over-limit
// penalty territory) and nothing carries into 2024.
func TestFHSA_OverLimitFirstYear(t *testing.T) {
	base := func(year int) Card {
		in := Input{
			Year:     year,
			Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
			Sums:     []YearSums{{AccountID: "a1", Year: 2023, CountedContributions: 1_000_000}},
		}
		return getCard(t, Compute(in).Cards, PlanFHSA, "")
	}
	c2023 := base(2023)
	checkI64(t, "2023 Room", c2023.Room, -200_000)
	if !slices.Equal(c2023.Warnings, []string{WarningOverLimit}) {
		t.Fatalf("2023 Warnings = %v, want [%s]", c2023.Warnings, WarningOverLimit)
	}
	c2024 := base(2024)
	checkI64(t, "2024 AnnualLimit", c2024.AnnualLimit, 800_000) // carry clamped at 0
	checkI64(t, "2024 Room", c2024.Room, 800_000)
}

// --- RESP --------------------------------------------------------------------

// Per-beneficiary lifetime limits: Alice ($15,000 of $50,000, grants
// excluded) and Bob ($60,000 -> $1,000 over). Ages 24 and 34 drive
// cesg_stops (both) and resp_age_31 (Bob only).
func TestRESP_PerBeneficiary(t *testing.T) {
	in := Input{
		Year: 2026,
		Accounts: []AccountInput{
			{ID: "a1", PlanType: PlanRESP, BeneficiaryName: "Alice", BeneficiaryBirthYear: ip(2002)},
			{ID: "a2", PlanType: PlanRESP, BeneficiaryName: "Bob", BeneficiaryBirthYear: ip(1992)},
		},
		Sums: []YearSums{
			{AccountID: "a1", Year: 2025, CountedContributions: 1_500_000, GrantInflows: 2_000_000},
			{AccountID: "a2", Year: 2024, CountedContributions: 3_000_000},
			{AccountID: "a2", Year: 2025, CountedContributions: 3_000_000},
		},
	}
	cards := Compute(in).Cards
	a := getCard(t, cards, PlanRESP, "Alice")
	checkI64(t, "Alice LifetimeLimit", a.LifetimeLimit, 5_000_000)
	checkI64(t, "Alice LifetimeUsed", a.LifetimeUsed, 1_500_000) // grants excluded
	checkI64(t, "Alice LifetimeRoom", a.LifetimeRoom, 3_500_000)
	if a.AnnualLimit != nil {
		t.Fatalf("Alice AnnualLimit = %d, want nil", *a.AnnualLimit)
	}
	if !slices.Equal(a.Warnings, nil) {
		t.Fatalf("Alice Warnings = %v, want none", a.Warnings)
	}
	if !slices.Equal(a.Notes, []string{NoteRESPCESGStops}) {
		t.Fatalf("Alice Notes = %v, want [%s]", a.Notes, NoteRESPCESGStops)
	}

	b := getCard(t, cards, PlanRESP, "Bob")
	checkI64(t, "Bob LifetimeUsed", b.LifetimeUsed, 6_000_000)
	checkI64(t, "Bob LifetimeRoom", b.LifetimeRoom, -1_000_000)
	if !slices.Equal(b.Warnings, []string{WarningLifetimeOverLimit, WarningRespAge31}) {
		t.Fatalf("Bob Warnings = %v", b.Warnings)
	}
	if !slices.Equal(b.Notes, []string{NoteRESPCESGStops}) {
		t.Fatalf("Bob Notes = %v", b.Notes)
	}
}

// RESP accounts without a beneficiary name group into one card.
func TestRESP_NoBeneficiaryGroupsToOneCard(t *testing.T) {
	in := Input{
		Year: 2026,
		Accounts: []AccountInput{
			{ID: "a1", PlanType: PlanRESP, BeneficiaryName: ""},
			{ID: "a2", PlanType: PlanRESP, BeneficiaryName: ""},
		},
		Sums: []YearSums{
			{AccountID: "a1", Year: 2025, CountedContributions: 100_000},
			{AccountID: "a2", Year: 2025, CountedContributions: 200_000},
		},
	}
	cards := Compute(in).Cards
	var respCards []Card
	for _, c := range cards {
		if c.PlanType == PlanRESP {
			respCards = append(respCards, c)
		}
	}
	if len(respCards) != 1 {
		t.Fatalf("got %d RESP cards, want 1: %v", len(respCards), respCards)
	}
	if respCards[0].Beneficiary != "" {
		t.Fatalf("Beneficiary = %q, want \"\"", respCards[0].Beneficiary)
	}
	checkI64(t, "LifetimeUsed", respCards[0].LifetimeUsed, 300_000)
}

// --- RDSP --------------------------------------------------------------------

// Beneficiary born 1965 is 61 in 2026 (past the age-59 cutoff); $210,000
// contributed puts the card $10,000 over the $200,000 lifetime limit.
func TestRDSP_Age59AndOverLimit(t *testing.T) {
	in := Input{
		Year: 2026,
		Accounts: []AccountInput{
			{ID: "a1", PlanType: PlanRDSP, BeneficiaryName: "C", BeneficiaryBirthYear: ip(1965)},
		},
		Sums: []YearSums{{AccountID: "a1", Year: 2025, CountedContributions: 21_000_000}},
	}
	c := getCard(t, Compute(in).Cards, PlanRDSP, "C")
	checkI64(t, "LifetimeLimit", c.LifetimeLimit, 20_000_000)
	checkI64(t, "LifetimeUsed", c.LifetimeUsed, 21_000_000)
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, -1_000_000)
	if !slices.Equal(c.Warnings, []string{WarningLifetimeOverLimit, WarningRDSPAge59}) {
		t.Fatalf("Warnings = %v", c.Warnings)
	}
	if !slices.Equal(c.Notes, []string{NoteRDSPNotDeductible, NoteRDSPCDSStops}) {
		t.Fatalf("Notes = %v", c.Notes)
	}
}

// Unknown beneficiary birth year: only the always-on not-deductible note.
func TestRDSP_UnknownBirthYear(t *testing.T) {
	in := Input{
		Year:     2026,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanRDSP, BeneficiaryName: "C"}},
	}
	c := getCard(t, Compute(in).Cards, PlanRDSP, "C")
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 20_000_000)
	if !slices.Equal(c.Warnings, nil) {
		t.Fatalf("Warnings = %v, want none", c.Warnings)
	}
	if !slices.Equal(c.Notes, []string{NoteRDSPNotDeductible}) {
		t.Fatalf("Notes = %v, want [%s]", c.Notes, NoteRDSPNotDeductible)
	}
}

// --- RRIF --------------------------------------------------------------------

// Holder born 1961 -> age 65 in 2026 -> factor 1/25 = 400 bps; minimum
// withdrawal fmv/25 = $400 on a $10,000 FMV.
func TestRRIF_Under71(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1961),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
		RRIFSnapshots:   map[string]int64{"a1": 1_000_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	if c.RRIFAge == nil || *c.RRIFAge != 65 {
		t.Fatalf("RRIFAge = %v, want 65", c.RRIFAge)
	}
	checkI64(t, "RRIFFactorBps", c.RRIFFactorBps, 400)
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 40_000)
	if !slices.Equal(c.Warnings, nil) || !slices.Equal(c.Notes, nil) {
		t.Fatalf("unexpected Warnings=%v Notes=%v", c.Warnings, c.Notes)
	}
}

// Age 70 -> 1/20 = 500 bps.
func TestRRIF_Age70(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1956),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
		RRIFSnapshots:   map[string]int64{"a1": 1_000_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFFactorBps", c.RRIFFactorBps, 500)
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 50_000)
}

// Age 71: fixed-table factor 5.28% = 528 bps; $100,000 FMV -> $5,280;
// conversion note fires.
func TestRRIF_Age71(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1955),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
		RRIFSnapshots:   map[string]int64{"a1": 10_000_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFFactorBps", c.RRIFFactorBps, 528)
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 528_000)
	if !slices.Equal(c.Notes, []string{NoteRRIFConversionDue}) {
		t.Fatalf("Notes = %v, want [%s]", c.Notes, NoteRRIFConversionDue)
	}
}

// Age 95 -> 20.00% = 2000 bps.
func TestRRIF_Age95(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1931),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
		RRIFSnapshots:   map[string]int64{"a1": 1_000_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFFactorBps", c.RRIFFactorBps, 2000)
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 200_000)
}

// An RRIF opened in the target year has no minimum for that year.
func TestRRIF_OpenedInTargetYear(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1961),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2026, time.March, 1)}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 0)
	if !slices.Equal(c.Notes, []string{NoteRRIFYearOpened}) {
		t.Fatalf("Notes = %v, want [%s]", c.Notes, NoteRRIFYearOpened)
	}
	if !slices.Equal(c.Warnings, nil) {
		t.Fatalf("Warnings = %v, want none", c.Warnings)
	}
}

// Missing snapshot for an old account -> rrif_no_fmv; minimum stays 0.
func TestRRIF_MissingSnapshot(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1961),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 0)
	if !slices.Equal(c.Warnings, []string{WarningRRIFNoFMV}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningRRIFNoFMV)
	}
}

// Two accounts: minimums are summed per account.
func TestRRIF_TwoAccountsSum(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1961), // age 65 -> /25
		Accounts: []AccountInput{
			{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2019, time.January, 1)},
			{ID: "a2", PlanType: PlanRRIF, OpenedAt: ts(2021, time.January, 1)},
		},
		RRIFSnapshots: map[string]int64{"a1": 1_000_000, "a2": 2_000_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFMinimum", c.RRIFMinimum, 40_000+80_000) // = 120_000
}

// Unknown holder birth year: no age, no factor, no minimum.
func TestRRIF_UnknownAge(t *testing.T) {
	in := Input{
		Year:     2026,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	if c.RRIFAge != nil || c.RRIFFactorBps != nil {
		t.Fatalf("RRIFAge=%v RRIFFactorBps=%v, want nil", c.RRIFAge, c.RRIFFactorBps)
	}
	checkNilI64(t, "RRIFMinimum", c.RRIFMinimum)
	if !slices.Equal(c.Warnings, []string{WarningRRIFAgeUnknown}) {
		t.Fatalf("Warnings = %v, want [%s]", c.Warnings, WarningRRIFAgeUnknown)
	}
}

// Age 67: the stored factor rounds to 435 bps but the minimum must come from
// the EXACT fraction fmv/(90-67). 2_300_000/23 = 100_000 exactly, while 435
// bps would give 100_050.
func TestRRIF_ExactFactorUnder71(t *testing.T) {
	in := Input{
		Year:            2026,
		HolderBirthYear: ip(1959),
		Accounts:        []AccountInput{{ID: "a1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)}},
		RRIFSnapshots:   map[string]int64{"a1": 2_300_000},
	}
	c := getCard(t, Compute(in).Cards, PlanRRIF, "")
	checkI64(t, "RRIFFactorBps (display)", c.RRIFFactorBps, 435)
	checkI64(t, "RRIFMinimum (exact fraction)", c.RRIFMinimum, 100_000)
}

// --- Adjustments ---------------------------------------------------------------

// Annual-plan adjustments apply to Room(Y) only for the target year.
func TestAdjustments_TFSA(t *testing.T) {
	for _, tc := range []struct {
		adj  int64
		room int64
	}{
		{+200_000, 900_000},
		{-200_000, 500_000},
	} {
		in := Input{
			Year:            2024,
			HolderBirthYear: ip(1990),
			Accounts:        []AccountInput{{ID: "a1", PlanType: PlanTFSA}},
			Adjustments:     []Adjustment{{PlanType: PlanTFSA, Year: 2024, Amount: tc.adj}},
		}
		c := getCard(t, Compute(in).Cards, PlanTFSA, "")
		checkI64(t, "Room", c.Room, tc.room)
	}
}

// FHSA is both annual and lifetime: adj(2024) enters Room(2024) only,
// adj(any year) enters LifetimeRoom.
func TestAdjustments_FHSA(t *testing.T) {
	in := Input{
		Year:     2024,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)}},
		Adjustments: []Adjustment{
			{PlanType: PlanFHSA, Year: 2023, Amount: +100_000},
			{PlanType: PlanFHSA, Year: 2024, Amount: +50_000},
		},
	}
	c := getCard(t, Compute(in).Cards, PlanFHSA, "")
	checkI64(t, "Room", c.Room, 1_600_000+50_000)          // only the 2024 adjustment
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 4_150_000) // both adjustments
}

// Lifetime-plan adjustments: a negative adjustment removes lifetime room.
func TestAdjustments_RESP(t *testing.T) {
	in := Input{
		Year:     2026,
		Accounts: []AccountInput{{ID: "a1", PlanType: PlanRESP, BeneficiaryName: "A"}},
		Adjustments: []Adjustment{
			{PlanType: PlanRESP, Year: 2024, Amount: -500_000},
		},
	}
	c := getCard(t, Compute(in).Cards, PlanRESP, "A")
	checkI64(t, "LifetimeRoom", c.LifetimeRoom, 4_500_000)
}

// --- Rollups and card order ------------------------------------------------------

// RegisteredTotal sums the latest balances over the input's plan accounts
// only; unlisted balances and non-registered accounts are ignored.
// InvestmentTotal is echoed from the input (caller pre-sums it).
func TestRollups(t *testing.T) {
	in := Input{
		Year: 2024,
		Accounts: []AccountInput{
			{ID: "a1", PlanType: PlanTFSA},
			{ID: "a2", PlanType: PlanRRSP},
			{ID: "a3", PlanType: PlanFHSA, OpenedAt: ts(2023, time.June, 1)},
			{ID: "a4", PlanType: PlanType("investment")}, // not one of the six: skipped
		},
		Balances: map[string]int64{
			"a1": 100_000, "a2": 50_000, "a3": 25_000,
			"a4": 999_999, // non-registered: not counted
			"a5": 777_777, // not an input account: not counted
		},
		InvestmentTotal: 123_456,
	}
	res := Compute(in)
	if res.RegisteredTotal != 175_000 {
		t.Fatalf("RegisteredTotal = %d, want 175000", res.RegisteredTotal)
	}
	if res.InvestmentTotal != 123_456 {
		t.Fatalf("InvestmentTotal = %d, want 123456", res.InvestmentTotal)
	}
}

// Cards are ordered by plan index in AllPlanTypes, then beneficiary name —
// independent of the input account order.
func TestCardOrder(t *testing.T) {
	in := Input{
		Year: 2026,
		Accounts: []AccountInput{
			{ID: "r1", PlanType: PlanRDSP, BeneficiaryName: "Z"},
			{ID: "t1", PlanType: PlanTFSA},
			{ID: "ra", PlanType: PlanRESP, BeneficiaryName: "Alice"},
			{ID: "f1", PlanType: PlanRRIF, OpenedAt: ts(2020, time.January, 1)},
			{ID: "p1", PlanType: PlanRRSP},
			{ID: "rb", PlanType: PlanRESP, BeneficiaryName: "Bob"},
		},
	}
	got := Compute(in).Cards
	want := []struct {
		pt  PlanType
		ben string
	}{
		{PlanRRSP, ""},
		{PlanTFSA, ""},
		{PlanRRIF, ""},
		{PlanRESP, "Alice"},
		{PlanRESP, "Bob"},
		{PlanRDSP, "Z"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cards, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].PlanType != want[i].pt || got[i].Beneficiary != want[i].ben {
			t.Fatalf("card %d = (%s, %q), want (%s, %q)", i, got[i].PlanType, got[i].Beneficiary, want[i].pt, want[i].ben)
		}
	}
}

// --- roundHalfUpDiv ---------------------------------------------------------------

func TestRoundHalfUpDiv(t *testing.T) {
	cases := []struct {
		num, den int64
		want     int64
	}{
		{5, 2, 3},  // 2.5 -> 3 (half up)
		{7, 2, 4},  // 3.5 -> 4
		{1, 2, 1},  // 0.5 -> 1
		{10, 3, 3}, // 3.333 -> 3
		{11, 3, 4}, // 3.667 -> 4
		{4, 2, 2},  // exact
		{0, 7, 0},
		{-5, 2, -2}, // -2.5 -> -2 (half toward +inf)
		{-1, 2, 0},  // -0.5 -> 0
		{-4, 3, -1}, // -1.333 -> -1 (floor of -0.833, not truncation toward zero)
	}
	for _, tc := range cases {
		if got := roundHalfUpDiv(tc.num, tc.den); got != tc.want {
			t.Fatalf("roundHalfUpDiv(%d, %d) = %d, want %d", tc.num, tc.den, got, tc.want)
		}
	}
}
