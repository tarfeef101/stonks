package plans

import (
	"sort"
	"time"
)

// Warning and note codes on Card. Warnings are stable machine-readable
// codes the UI can act on (or surface with help text); notes are
// informational codes for the same purpose.
const (
	// WarningLimitUnknown: the annual limit for the target year is not
	// known to the statutory tables and no user override covers it.
	WarningLimitUnknown = "limit_unknown"
	// WarningOverLimit: room for the target year is negative (over-
	// contribution; 1%/month CRA penalty applies for TFSA/RRSP/FHSA/RESP).
	WarningOverLimit = "over_limit"
	// WarningTFSAMinAge: the holder was under 18 in the target year
	// (TFSA contributions require age 18+ and Canadian residency).
	WarningTFSAMinAge = "tfsa_min_age"
	// WarningRRSPA71: the holder turns 71 in the target year (RRSP
	// contributions are allowed until Dec 31 of that year).
	WarningRRSPA71 = "rrsp_age_71"
	// WarningFHSAPreLaunch: an FHSA account's OpenedAt predates the
	// plan's 2023-04-01 launch (data error).
	WarningFHSAPreLaunch = "fhsa_pre_launch"
	// WarningLifetimeOverLimit: lifetime room is negative (RESP/RDSP).
	WarningLifetimeOverLimit = "lifetime_over_limit"
	// WarningRespAge31: the beneficiary turns 31+ in the target year;
	// contributions must stop (family plan: age 31; individual plan:
	// 31 years from opening).
	WarningRespAge31 = "resp_age_31"
	// WarningRDSPAge59: the beneficiary turns 59+ in the target year;
	// contributions must stop.
	WarningRDSPAge59 = "rdsp_age_59"
	// WarningRRIFAgeUnknown: the holder's birth year is unknown, so the
	// RRIF minimum withdrawal cannot be computed.
	WarningRRIFAgeUnknown = "rrif_age_unknown"
	// WarningRRIFNoFMV: an RRIF account (not opened in the target year)
	// has no snapshot on/before Jan 1 of the target year, so its minimum
	// contribution cannot be computed.
	WarningRRIFNoFMV = "rrif_no_fmv"

	// NoteRRSPActualLimit: the statutory figure is the CAP, not the
	// holder's personal limit (min(18% earned income, cap) − PA); an
	// override with the actual limit is expected.
	NoteRRSPActualLimit = "rrsp_actual_limit"
	// NoteRRSPRecontribution: RRSP withdrawals may be re-contributed at
	// any time within 36 months without using room (in addition to the
	// TFSA-style withdrawal room counted in Room).
	NoteRRSPRecontribution = "rrsp_recontribution"
	// NoteFHSAWindow: the 15-year window from account opening has
	// closed; the account must be closed and funds withdrawn.
	NoteFHSAWindow = "fhsa_window"
	// NoteRESPCESGStops: CESG is no longer eligible (beneficiary age 17+).
	NoteRESPCESGStops = "resp_cesg_stops"
	// NoteRDSPCDSStops: CDSG is no longer eligible (beneficiary age 49+).
	NoteRDSPCDSStops = "rdsp_cds_stops"
	// NoteRDSPNotDeductible: RDSP contributions are not tax-deductible.
	NoteRDSPNotDeductible = "rdsp_not_deductible"
	// NoteRRIFYearOpened: an RRIF account was opened in the target year,
	// so it has no minimum withdrawal for that year.
	NoteRRIFYearOpened = "rrif_year_opened"
	// NoteRRIFConversionDue: the holder turns 71+ in the target year;
	// the RRSP must have been converted to a RRIF by Dec 31 of the year
	// the holder turns 71.
	NoteRRIFConversionDue = "rrif_conversion_due"
)

// LimitSource values on Card.LimitSource.
const (
	LimitSourceStatutory = "statutory"
	LimitSourceOverride  = "override"
	LimitSourceNone      = "none"
)

// AccountInput is one registered-plan account as seen by the engine. The
// caller has already decrypted the name/birth-year fields and pre-filtered
// the transactions; the engine receives plaintext values and pre-computed
// sums. BeneficiaryName is only meaningful for RESP/RDSP ("" groups
// beneficiary-less accounts into one card).
type AccountInput struct {
	ID                   string
	PlanType             PlanType // one of the six
	BeneficiaryName      string   // "" unless RESP/RDSP
	BeneficiaryBirthYear *int     // optional (decrypted by caller)
	// HolderBirthYear is optional and exists for per-account callers;
	// Compute prefers Input.HolderBirthYear and falls back to the first
	// non-nil per-account value (single-user context: one holder).
	HolderBirthYear *int
	OpenedAt        *time.Time // optional
}

// YearSums is the caller's pre-filtered per-account, per-year sums, in
// cents, for ALL years (the engine selects what it needs).
//
// Counting rule (enforced by the caller, NOT the engine): a counted
// contribution is a POSITIVE-amount transaction row whose external_origin
// is NULL, 'employer' or 'other'. 'government_grant' and 'inherited' rows
// NEVER count toward room — grants and inherited money are not the user's
// own contribution and affect the balance only. Employer matches DO count
// toward RRSP room (an employer match consumes the holder's RRSP room).
type YearSums struct {
	AccountID string
	Year      int
	// CountedContributions: positive rows per the counting rule above.
	CountedContributions int64
	// Withdrawals: positive magnitude of negative rows.
	Withdrawals int64
	// GrantInflows is informational only (grants/inherited received); it
	// NEVER counts toward room.
	GrantInflows int64
}

// Override is a user-specified annual limit (in cents) for one plan-year.
// Overrides beat the statutory tables.
type Override struct {
	PlanType    PlanType
	Year        int
	AnnualLimit int64 // cents
}

// Adjustment is a signed room adjustment (in cents) for one plan-year:
// positive adds room, negative removes it. How it applies is documented on
// Compute: annual-limit plans add adj(plan, Y) to Room(Y); lifetime plans
// add adj(plan, any year) to LifetimeRoom (FHSA is both).
type Adjustment struct {
	PlanType PlanType
	Year     int
	Amount   int64 // cents, signed
}

// Card is the room result for one (plan type, beneficiary) pair. All money
// fields are in cents; nil pointers mean "not applicable / not known".
type Card struct {
	PlanType    PlanType
	Beneficiary string // "" for non-beneficiary plans
	// AnnualLimit: nil = the plan has no annual limit (resp/rdsp/rrif) or
	// the limit is not known (see WarningLimitUnknown).
	AnnualLimit *int64
	// LimitSource: "statutory" | "override" | "none".
	LimitSource string
	// Used: counted contributions in the target year.
	Used int64
	// Room: AnnualLimit − Used + prior-year withdrawals (tfsa/rrsp) +
	// adjustments, when the annual limit is known; nil otherwise.
	Room *int64
	// ReCreatedNextYear: tfsa/rrsp withdrawals in the target year, which
	// re-create room for NEXT year; 0 otherwise.
	ReCreatedNextYear int64
	// LifetimeLimit/LifetimeUsed/LifetimeRoom: fhsa/resp/rdsp only.
	LifetimeLimit *int64
	LifetimeUsed  *int64
	LifetimeRoom  *int64
	// RRIF fields (rrif only; nil when the holder's birth year is
	// unknown): the minimum total withdrawal for the target year, the
	// minimum-withdrawal factor in basis points, and the holder's
	// approximate age at Jan 1 of the target year.
	RRIFMinimum   *int64
	RRIFFactorBps *int64
	RRIFAge       *int
	// Warnings: stable machine-readable codes (see the constants above),
	// appended in deterministic order.
	Warnings []string
	// Notes: informational codes, appended in deterministic order.
	Notes []string
}

// Input is everything Compute needs for one target year. All money is in
// integer cents.
type Input struct {
	Year     int
	Accounts []AccountInput // the user's registered-plan accounts (the six)
	// Sums: all years; the engine selects what it needs.
	Sums        []YearSums
	Overrides   []Override
	Adjustments []Adjustment
	// HolderBirthYear: the holder's birth year (nil = unknown). When nil,
	// Compute falls back to the first non-nil per-account HolderBirthYear.
	HolderBirthYear *int
	// Balances: accountID -> latest known balance in cents, for rollups.
	// RegisteredTotal sums this over the input's plan accounts.
	Balances map[string]int64
	// InvestmentTotal: the caller's pre-summed latest known balance over
	// the user's 'investment'-type accounts (cents).
	InvestmentTotal int64
	// RRIFSnapshots: accountID -> balance of the latest snapshot on/before
	// Jan 1 of the target year (cents), when any.
	RRIFSnapshots map[string]int64
}

// Result is the engine output: one card per (plan type, beneficiary) pair
// with at least one account of that type, in stable order (plan index in
// AllPlanTypes, then beneficiary), plus the balance rollups.
type Result struct {
	Year  int
	Cards []Card
	// RegisteredTotal: sum of the latest known balances over the user's
	// registered-plan accounts (one of the six).
	RegisteredTotal int64
	// InvestmentTotal: latest known total over 'investment' accounts.
	InvestmentTotal int64
}

// Compute runs the room engine for in.Year and returns the per-plan cards
// plus rollups. It is a pure function: no I/O, no globals mutated.
func Compute(in Input) Result {
	res := Result{Year: in.Year, InvestmentTotal: in.InvestmentTotal}
	y := in.Year

	// Holder birth year: top level, else the first non-nil per-account.
	holderYear := in.HolderBirthYear
	if holderYear == nil {
		for i := range in.Accounts {
			if in.Accounts[i].HolderBirthYear != nil {
				holderYear = in.Accounts[i].HolderBirthYear
				break
			}
		}
	}

	// Group plan accounts by (plan, beneficiary); non-registered plan
	// types are ignored by contract (defensive skip, not an error).
	type groupKey struct {
		plan        PlanType
		beneficiary string
	}
	groups := map[groupKey][]AccountInput{}
	var order []groupKey
	for _, a := range in.Accounts {
		if !IsRegistered(string(a.PlanType)) {
			continue
		}
		key := groupKey{a.PlanType, a.BeneficiaryName}
		if a.PlanType != PlanRESP && a.PlanType != PlanRDSP {
			key.beneficiary = "" // beneficiary only meaningful for resp/rdsp
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], a)
	}
	planIdx := map[PlanType]int{}
	for i, p := range AllPlanTypes {
		planIdx[p] = i
	}
	sort.Slice(order, func(i, j int) bool {
		if planIdx[order[i].plan] != planIdx[order[j].plan] {
			return planIdx[order[i].plan] < planIdx[order[j].plan]
		}
		return order[i].beneficiary < order[j].beneficiary
	})

	idx := newIndex(in.Sums)

	overrideFor := func(p PlanType, year int) (int64, bool) {
		for _, o := range in.Overrides {
			if o.PlanType == p && o.Year == year {
				return o.AnnualLimit, true
			}
		}
		return 0, false
	}
	// adjFor(p, year): sum of adjustments for plan p in year; year < 0
	// means ANY year (lifetime plans).
	adjFor := func(p PlanType, year int) int64 {
		var t int64
		for _, a := range in.Adjustments {
			if a.PlanType != p {
				continue
			}
			if year >= 0 && a.Year != year {
				continue
			}
			t += a.Amount
		}
		return t
	}

	var registered int64
	for _, key := range order {
		accts := groups[key]
		for _, a := range accts {
			registered += in.Balances[a.ID]
		}
		switch key.plan {
		case PlanTFSA:
			res.Cards = append(res.Cards, tfsaCard(accts, y, holderYear, idx, overrideFor, adjFor))
		case PlanRRSP:
			res.Cards = append(res.Cards, rrspCard(accts, y, holderYear, idx, overrideFor, adjFor))
		case PlanFHSA:
			if c, ok := fhsaCard(accts, y, idx, overrideFor, adjFor); ok {
				res.Cards = append(res.Cards, c)
			}
		case PlanRESP:
			res.Cards = append(res.Cards, respCard(accts, y, idx, adjFor))
		case PlanRDSP:
			res.Cards = append(res.Cards, rdspCard(accts, y, idx, adjFor))
		case PlanRRIF:
			res.Cards = append(res.Cards, rrifCard(accts, y, holderYear, in.RRIFSnapshots, idx))
		}
	}
	res.RegisteredTotal = registered
	return res
}

// tfsaCard: one card, beneficiary "". Room(Y) = limit(Y) − used(Y) +
// withdrawals(Y−1) + adj(tfsa, Y). Withdrawals re-create room for the NEXT
// year, so they enter through Y−1.
func tfsaCard(accts []AccountInput, y int, holderYear *int, idx sumIndex,
	overrideFor func(PlanType, int) (int64, bool), adjFor func(PlanType, int) int64) Card {
	ids := acctsIDs(accts)
	used := idx.contrib(ids, y)
	card := Card{PlanType: PlanTFSA, Used: used, ReCreatedNextYear: idx.withdrawals(ids, y)}
	setAnnualLimit(&card,
		func() (int64, bool) { return overrideFor(PlanTFSA, y) },
		func() (int64, bool) { return TFSAStatutoryAnnualLimitCents(y) })
	if card.AnnualLimit != nil {
		room := *card.AnnualLimit - used + idx.withdrawals(ids, y-1) + adjFor(PlanTFSA, y)
		card.Room = &room
		if room < 0 {
			addUnique(&card.Warnings, WarningOverLimit)
		}
	}
	if holderYear != nil && y-*holderYear < 18 {
		addUnique(&card.Warnings, WarningTFSAMinAge)
	}
	return card
}

// rrspCard: limit(Y) = override(Y) else the statutory CAP (outside
// 2023-2028 the cap is unknown -> override required). Room(Y) = limit(Y) −
// used(Y) + withdrawals(Y−1) + adj(rrsp, Y). Note rrsp_actual_limit: the
// effective personal limit is min(18% earned income, cap) − PA, so the user
// is expected to override. Note rrsp_recontribution: withdrawals may be
// re-contributed at any time within 36 months without using room (on top
// of the TFSA-style withdrawal room already counted).
func rrspCard(accts []AccountInput, y int, holderYear *int, idx sumIndex,
	overrideFor func(PlanType, int) (int64, bool), adjFor func(PlanType, int) int64) Card {
	ids := acctsIDs(accts)
	used := idx.contrib(ids, y)
	card := Card{PlanType: PlanRRSP, Used: used, ReCreatedNextYear: idx.withdrawals(ids, y),
		Notes: []string{NoteRRSPRecontribution}}
	setAnnualLimit(&card,
		func() (int64, bool) { return overrideFor(PlanRRSP, y) },
		func() (int64, bool) { return RRSPStatutoryAnnualCapCents(y) })
	if card.LimitSource != LimitSourceOverride {
		addUnique(&card.Notes, NoteRRSPActualLimit)
	}
	if card.AnnualLimit != nil {
		room := *card.AnnualLimit - used + idx.withdrawals(ids, y-1) + adjFor(PlanRRSP, y)
		card.Room = &room
		if room < 0 {
			addUnique(&card.Warnings, WarningOverLimit)
		}
	}
	if holderYear != nil && y-*holderYear >= RRSPConversionAge {
		addUnique(&card.Warnings, WarningRRSPA71)
	}
	return card
}

// fhsaCard: no card at all for Y < 2023 (the plan did not exist; the
// launch-date check is the only handling — there is deliberately no
// fhsa_pre_2023 warning code).
//
// The effective annual limit is computed iteratively from 2023:
// eff(2022) = 0; for y' = 2023..Y: override(y') -> eff = override; else
// (y' <= 2029) eff = 8000 + min(8000, max(0, eff(y'−1) − used(y'−1)))
// (cents throughout); else (y' >= 2030, no override) the statutory chain
// stops — $8,000 is confirmed for 2023-2029 only, so 2030+ REQUIRES an
// override (the chain must be covered continuously; an override for 2035
// without a 2030 override still stops at 2030).
//
// Room(Y) = eff(Y) − used(Y) + adj(fhsa, Y). Lifetime = 40000 −
// all-year used + adj(fhsa, any year).
func fhsaCard(accts []AccountInput, y int, idx sumIndex,
	overrideFor func(PlanType, int) (int64, bool), adjFor func(PlanType, int) int64) (Card, bool) {
	if y < FHSALaunchYear {
		return Card{}, false // plan did not exist
	}
	ids := acctsIDs(accts)
	used := idx.contrib(ids, y)
	card := Card{PlanType: PlanFHSA, Used: used}

	eff := int64(0) // eff(2022)
	known, source := true, LimitSourceStatutory
	for y2 := FHSALaunchYear; y2 <= y; y2++ {
		if ov, ok := overrideFor(PlanFHSA, y2); ok {
			eff = ov
			source = LimitSourceOverride
			continue
		}
		if y2 > FHSAStatutoryLastConfirmedYear {
			known = false // 2030+ without an override: unknown
			break
		}
		carry := eff - idx.contrib(ids, y2-1)
		if carry < 0 {
			carry = 0
		}
		if carry > FHSACarryForwardCapCents {
			carry = FHSACarryForwardCapCents
		}
		eff = FHSAStatutoryAnnualLimitCents + carry
		source = LimitSourceStatutory
	}
	if !known {
		card.LimitSource = LimitSourceNone
		addUnique(&card.Warnings, WarningLimitUnknown)
	} else {
		card.AnnualLimit = &eff
		card.LimitSource = source
		room := eff - used + adjFor(PlanFHSA, y)
		card.Room = &room
		if room < 0 {
			addUnique(&card.Warnings, WarningOverLimit)
		}
	}

	lifetimeUsed := idx.contribAll(ids)
	limit := int64(FHSALifetimeLimitCents)
	lr := limit - lifetimeUsed + adjFor(PlanFHSA, -1) // lifetime: ANY year
	card.LifetimeLimit = &limit
	card.LifetimeUsed = &lifetimeUsed
	card.LifetimeRoom = &lr

	for _, a := range accts {
		if a.OpenedAt == nil {
			continue
		}
		if beforeDate(*a.OpenedAt, FHSAWindowOpen) {
			addUnique(&card.Warnings, WarningFHSAPreLaunch)
		}
		if y >= a.OpenedAt.Year()+FHSAWindowYears {
			addUnique(&card.Notes, NoteFHSAWindow)
		}
	}
	return card, true
}

// respCard: one card PER BENEFICIARY NAME ("" groups beneficiary-less
// accounts). No annual limit. Lifetime = 50000 − all-year counted
// contributions for that beneficiary, per beneficiary. Grant inflows are
// NEVER counted: they arrive as separate rows with
// external_origin='government_grant', which the caller's counting rule
// excludes from CountedContributions (YearSums.GrantInflows is
// informational only), so the lifetime math holds as long as the caller's
// filter is right.
func respCard(accts []AccountInput, y int, idx sumIndex, adjFor func(PlanType, int) int64) Card {
	ids := acctsIDs(accts)
	by := firstNonNilBirthYear(accts)
	card := Card{PlanType: PlanRESP, Beneficiary: accts[0].BeneficiaryName, Used: idx.contrib(ids, y),
		LimitSource: LimitSourceNone}
	limit := int64(RESPPerBeneficiaryLifetimeLimitCents)
	lifetimeUsed := idx.contribAll(ids)
	lr := limit - lifetimeUsed + adjFor(PlanRESP, -1) // lifetime: ANY year
	card.LifetimeLimit = &limit
	card.LifetimeUsed = &lifetimeUsed
	card.LifetimeRoom = &lr
	if lr < 0 {
		addUnique(&card.Warnings, WarningLifetimeOverLimit)
	}
	if by != nil {
		age := y - *by
		if age >= RESPContributionCutoffAge {
			addUnique(&card.Warnings, WarningRespAge31)
		}
		if age >= RESPCESGCutoffAge {
			addUnique(&card.Notes, NoteRESPCESGStops)
		}
	}
	return card
}

// rdspCard: one card per beneficiary. Lifetime = 200000 − all-year counted
// contributions (includes rollovers). Contributions until age 59; CDSG
// stops after 49; contributions are NOT tax-deductible.
func rdspCard(accts []AccountInput, y int, idx sumIndex, adjFor func(PlanType, int) int64) Card {
	ids := acctsIDs(accts)
	by := firstNonNilBirthYear(accts)
	card := Card{PlanType: PlanRDSP, Beneficiary: accts[0].BeneficiaryName, Used: idx.contrib(ids, y),
		LimitSource: LimitSourceNone, Notes: []string{NoteRDSPNotDeductible}}
	limit := int64(RDSPPerBeneficiaryLifetimeLimitCents)
	lifetimeUsed := idx.contribAll(ids)
	lr := limit - lifetimeUsed + adjFor(PlanRDSP, -1) // lifetime: ANY year
	card.LifetimeLimit = &limit
	card.LifetimeUsed = &lifetimeUsed
	card.LifetimeRoom = &lr
	if lr < 0 {
		addUnique(&card.Warnings, WarningLifetimeOverLimit)
	}
	if by != nil {
		age := y - *by
		if age >= RDSPContributionCutoffAge {
			addUnique(&card.Warnings, WarningRDSPAge59)
		}
		if age >= RDSPCDSCutoffAge {
			addUnique(&card.Notes, NoteRDSPCDSStops)
		}
	}
	return card
}

// rrifCard: one card, beneficiary "". No contribution limit.
//
// Age approximation: only the birth YEAR is stored, so the age at Jan 1 of
// the target year is approximated as Y − birthYear. This can OVERSTATE the
// age by up to 1 year (a holder who turns N later in the year), which
// overstates the minimum withdrawal — the safe direction (never understates
// a required withdrawal).
//
// Per account: an account opened in the target year has no minimum (note
// rrif_year_opened); otherwise it needs a snapshot on/before Jan 1 of the
// target year (warning rrif_no_fmv when missing). The minimum for an
// account is computed from the EXACT factor:
//
//   - age < 71:  fmv / (90 − age), round-half-up to the cent
//     (10000/(90-age) bps is not integral for most ages; using the exact
//     fraction avoids rounding the factor first)
//   - age 71+:   fmv * factorBps / 10000, round-half-up
//
// RRIFFactorBps reports the (possibly rounded) factor in bps.
func rrifCard(accts []AccountInput, y int, holderYear *int,
	snapshots map[string]int64, idx sumIndex) Card {
	ids := acctsIDs(accts)
	card := Card{PlanType: PlanRRIF, Used: idx.contrib(ids, y)}
	if holderYear == nil {
		card.LimitSource = LimitSourceNone
		addUnique(&card.Warnings, WarningRRIFAgeUnknown)
		return card
	}
	age := y - *holderYear
	factorBps, _ := RRIFMinimumFactorBps(age)
	card.LimitSource = LimitSourceNone
	card.RRIFAge = &age
	card.RRIFFactorBps = &factorBps

	var min int64
	for _, a := range accts {
		if a.OpenedAt != nil && a.OpenedAt.Year() == y {
			addUnique(&card.Notes, NoteRRIFYearOpened)
			continue // no minimum for the year the RRIF is opened
		}
		fmv, ok := snapshots[a.ID]
		if !ok {
			addUnique(&card.Warnings, WarningRRIFNoFMV)
			continue
		}
		min += rrifAccountMinimum(fmv, age)
	}
	card.RRIFMinimum = &min
	if age >= RRSPConversionAge {
		addUnique(&card.Notes, NoteRRIFConversionDue)
	}
	return card
}

// rrifAccountMinimum is the round-half-up minimum withdrawal for one RRIF
// account with fair market value fmvCents, from the exact factor.
func rrifAccountMinimum(fmvCents int64, age int) int64 {
	if age >= 71 {
		bps := rrifFactorBps71Plus[age]
		if age >= 95 {
			bps = 2000
		}
		return roundHalfUpDiv(fmvCents*bps, 10000)
	}
	return roundHalfUpDiv(fmvCents, int64(90-age))
}

// setAnnualLimit sets card.AnnualLimit/LimitSource: a user override beats
// the statutory table; when neither exists the source is "none" and the
// limit_unknown warning is appended.
func setAnnualLimit(card *Card, override, statutory func() (int64, bool)) {
	if ov, ok := override(); ok {
		card.AnnualLimit = &ov
		card.LimitSource = LimitSourceOverride
		return
	}
	if v, ok := statutory(); ok {
		card.AnnualLimit = &v
		card.LimitSource = LimitSourceStatutory
		return
	}
	card.LimitSource = LimitSourceNone
	addUnique(&card.Warnings, WarningLimitUnknown)
}

// sumIndex indexes YearSums by (account, year), merging duplicates.
type sumIndex struct {
	byAcctYear map[sumKey]YearSums
}

type sumKey struct {
	acct string
	year int
}

func newIndex(sums []YearSums) sumIndex {
	idx := sumIndex{byAcctYear: make(map[sumKey]YearSums, len(sums))}
	for _, s := range sums {
		k := sumKey{s.AccountID, s.Year}
		if m, ok := idx.byAcctYear[k]; ok {
			m.CountedContributions += s.CountedContributions
			m.Withdrawals += s.Withdrawals
			m.GrantInflows += s.GrantInflows
			idx.byAcctYear[k] = m
		} else {
			idx.byAcctYear[k] = s
		}
	}
	return idx
}

func (s sumIndex) contrib(acctIDs []string, year int) int64 {
	var t int64
	for _, id := range acctIDs {
		t += s.byAcctYear[sumKey{id, year}].CountedContributions
	}
	return t
}

func (s sumIndex) withdrawals(acctIDs []string, year int) int64 {
	var t int64
	for _, id := range acctIDs {
		t += s.byAcctYear[sumKey{id, year}].Withdrawals
	}
	return t
}

// contribAll sums counted contributions over ALL years.
func (s sumIndex) contribAll(acctIDs []string) int64 {
	var t int64
	for k, v := range s.byAcctYear {
		for _, id := range acctIDs {
			if k.acct == id {
				t += v.CountedContributions
			}
		}
	}
	return t
}

func acctsIDs(accts []AccountInput) []string {
	ids := make([]string, 0, len(accts))
	for _, a := range accts {
		ids = append(ids, a.ID)
	}
	return ids
}

// firstNonNilBirthYear returns the first non-nil BeneficiaryBirthYear in
// the group (accounts sharing a beneficiary name are expected to agree;
// the first known value wins).
func firstNonNilBirthYear(accts []AccountInput) *int {
	for _, a := range accts {
		if a.BeneficiaryBirthYear != nil {
			return a.BeneficiaryBirthYear
		}
	}
	return nil
}

// beforeDate reports whether t's calendar date (UTC) is strictly before d's.
func beforeDate(t, d time.Time) bool {
	y1, m1, day1 := t.UTC().Date()
	y2, m2, day2 := d.UTC().Date()
	if y1 != y2 {
		return y1 < y2
	}
	if m1 != m2 {
		return m1 < m2
	}
	return day1 < day2
}

// roundHalfUpDiv returns num/den rounded half UP (toward +inf on exact
// halves), exactly, for den > 0: it is floor(num/den + 1/2) =
// floor((2*num + den) / (2*den)), with no intermediate rounding. Sums are
// non-negative by contract; negative num is supported for symmetry
// (e.g. -5/2 -> -2, not -1 or -3).
func roundHalfUpDiv(num, den int64) int64 {
	q := (2*num + den) / (2 * den)
	if rem := (2*num + den) % (2 * den); rem < 0 {
		q-- // Go's / truncates toward zero; floor is one less on a negative remainder
	}
	return q
}

// addUnique appends v to *s unless it is already present: warning/note
// codes may fire per account but are reported once per card.
func addUnique(s *[]string, v string) {
	for _, x := range *s {
		if x == v {
			return
		}
	}
	*s = append(*s, v)
}
