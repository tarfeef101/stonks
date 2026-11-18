// Package plans implements the Canadian registered-plan room engine for
// finance-tracker (round 50).
//
// The engine is pure Go (stdlib only, no repo/crypto/DB imports) and works
// in INTEGER CENTS (int64) for all money. It is a computation oracle: the
// caller (the repo layer) decrypts the DEK-encrypted fields, pre-filters the
// transaction sums per the counting rule documented on YearSums, and passes
// plaintext values in. The engine never touches the database.
//
// Statutory tables are as of 2026, verified against canada.ca. Where the
// statute is indexed or only confirmed for a range of years, the table ends
// and the engine requires a user override (see the per-plan docs in room.go).
package plans

import (
	"math"
	"time"
)

// PlanType names one of the six Canadian registered plans the room engine
// understands. The values match the extended accounts.type vocabulary
// (migration 0019).
type PlanType string

const (
	PlanRRSP PlanType = "rrsp"
	PlanTFSA PlanType = "tfsa"
	PlanFHSA PlanType = "fhsa"
	PlanRRIF PlanType = "rrif"
	PlanRESP PlanType = "resp"
	PlanRDSP PlanType = "rdsp"
)

// AllPlanTypes is the canonical order of the six plans (also the sort order
// of Result.Cards by plan, then beneficiary).
var AllPlanTypes = []PlanType{PlanRRSP, PlanTFSA, PlanFHSA, PlanRRIF, PlanRESP, PlanRDSP}

// IsRegistered reports whether s is one of the six registered plan types.
// It is the membership test used for the RegisteredTotal rollup and for
// deciding which accounts get a room card.
func IsRegistered(s string) bool {
	switch PlanType(s) {
	case PlanRRSP, PlanTFSA, PlanFHSA, PlanRRIF, PlanRESP, PlanRDSP:
		return true
	}
	return false
}

// IsPlanType reports whether s is one of this package's PlanType constants.
// For the current vocabulary it coincides with IsRegistered; it is kept
// separate so callers can express intent (vocabulary check vs
// registered-plan rollup check) and so the two sets can diverge if a
// non-registered plan type is ever added.
func IsPlanType(s string) bool {
	return IsRegistered(s)
}

// TFSAStatutoryAnnualLimitCents returns the statutory TFSA annual limit for
// the year in cents:
//
//	2009-2012: $5,000   2013-2014: $5,500   2015: $10,000
//	2016-2018: $5,500   2019-2022: $6,000   2023: $6,500
//	2024-2026: $7,000
//
// 2027 and later are UNKNOWN (not yet announced; a user override is
// required) and the plan did not exist before 2009; both return false.
func TFSAStatutoryAnnualLimitCents(year int) (int64, bool) {
	var dollars int
	switch {
	case year >= 2009 && year <= 2012:
		dollars = 5000
	case year >= 2013 && year <= 2014:
		dollars = 5500
	case year == 2015:
		dollars = 10000
	case year >= 2016 && year <= 2018:
		dollars = 5500
	case year >= 2019 && year <= 2022:
		dollars = 6000
	case year == 2023:
		dollars = 6500
	case year >= 2024 && year <= 2026:
		dollars = 7000
	default:
		return 0, false
	}
	return int64(dollars) * 100, true
}

// RRSPStatutoryAnnualCapCents returns the statutory RRSP dollar limit (the
// CAP) for the year in cents:
//
//	2023: $31,570   2024: $32,790   2025: $32,490
//	2026: $33,810   2027: $35,390   2028: $36,390
//
// All other years are UNKNOWN (the cap is indexed and only confirmed to
// 2028); a user override is required. The cap is NOT the holder's personal
// limit: personal limit = min(18% of prior-year earned income, cap) −
// pension adjustment, which is why the room card always carries the
// rrsp_actual_limit note unless the user has overridden.
func RRSPStatutoryAnnualCapCents(year int) (int64, bool) {
	switch year {
	case 2023:
		return 3_157_000, true
	case 2024:
		return 3_279_000, true
	case 2025:
		return 3_249_000, true
	case 2026:
		return 3_381_000, true
	case 2027:
		return 3_539_000, true
	case 2028:
		return 3_639_000, true
	}
	return 0, false
}

// FHSA statutory parameters. The plan launched 2023-04-01; the $8,000
// annual limit is NOT indexed and is confirmed for 2023-2029 only (from
// 2030 an override per year is required). The $40,000 lifetime limit
// includes RRSP->FHSA transfers. The plan carries a 1%/month
// over-contribution penalty (reported as the over_limit warning).
const (
	FHSALaunchYear                 = 2023      // first year with a limit
	FHSAStatutoryLastConfirmedYear = 2029      // $8,000 confirmed for 2023-2029 only
	FHSAStatutoryAnnualLimitCents  = 800_000   // $8,000 / year
	FHSACarryForwardCapCents       = 800_000   // unused annual room carries forward, capped at $8,000/yr
	FHSALifetimeLimitCents         = 4_000_000 // $40,000 lifetime
	FHSAWindowYears                = 15        // account must be closed/emptied within 15 years of opening
)

// FHSAWindowOpen is the FHSA launch date; accounts opened before it are a
// data error (warning fhsa_pre_launch).
var FHSAWindowOpen = time.Date(2023, time.April, 1, 0, 0, 0, 0, time.UTC)

// RESP parameters. Lifetime limit is PER BENEFICIARY (grants excluded —
// they arrive as separate government_grant rows and never count).
// Contributions are allowed until the beneficiary is 31 (family plan) /
// 31 years after opening (individual plan); CESG stops after age 17.
// 1%/month over-contribution penalty.
const (
	RESPPerBeneficiaryLifetimeLimitCents = 5_000_000 // $50,000 per beneficiary
	RESPContributionCutoffAge            = 31
	RESPCESGCutoffAge                    = 17
)

// RDSP parameters. Lifetime limit is PER BENEFICIARY and INCLUDES
// rollovers. Contributions allowed until age 59. Not tax-deductible
// (note rdsp_not_deductible). CDSG stops after age 49.
const (
	RDSPPerBeneficiaryLifetimeLimitCents = 20_000_000 // $200,000 per beneficiary
	RDSPContributionCutoffAge            = 59
	RDSPCDSCutoffAge                     = 49
)

// rrifFactorBps71Plus is the fixed CRA minimum-withdrawal factor table for
// ages 71..94, stored as integer basis points (1 bp = 0.01%): 5.28% -> 528.
var rrifFactorBps71Plus = map[int]int64{
	71: 528, 72: 540, 73: 553, 74: 567, 75: 582, 76: 598, 77: 617, 78: 636,
	79: 658, 80: 682, 81: 708, 82: 738, 83: 771, 84: 808, 85: 851, 86: 899,
	87: 955, 88: 1021, 89: 1099, 90: 1192, 91: 1306, 92: 1449, 93: 1634,
	94: 1879,
}

// RRIFMinimumFactorBps returns the RRIF minimum-withdrawal factor for the
// holder's age as integer basis points (1 bp = 0.01%):
//
//   - age < 71:  1/(90-age)  (e.g. age 65 -> 400 bps = 4.00%)
//   - age 71-94: the fixed CRA table (age 71 -> 528 bps = 5.28%)
//   - age 95+:   20.00%      (2000 bps)
//
// The stored value is the factor ROUNDED to integer bps: 10000/(90-age) is
// not an integer for most ages under 71 (e.g. age 67 -> 434.78 bps, stored
// 435). The room engine computes the minimum withdrawal from the EXACT
// fraction fmv/(90-age), not from this rounded value (see room.go), so the
// rounding is display-only. Negative ages are invalid -> false.
func RRIFMinimumFactorBps(age int) (int64, bool) {
	if age < 0 {
		return 0, false
	}
	if age >= 71 {
		if age >= 95 {
			return 2000, true
		}
		return rrifFactorBps71Plus[age], true
	}
	return int64(math.Round(10000.0 / float64(90-age))), true
}

// RRSPConversionAge is the age by whose Dec 31 of the holder's birth year +
// this age an RRSP must have been converted to a RRIF (i.e. the year the
// holder turns 71).
const RRSPConversionAge = 71
