// Savings routes (later step): the narrow store interfaces the
// handlers program against. The signatures are copied verbatim from
// the repo implementations, so the repo types satisfy them directly.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/plans"
	"finance-tracker/internal/repo"
)

// SavingsUserStore is the user-row surface the savings routes need:
// the row read plus the birth-year setter.
type SavingsUserStore interface {
	Get(ctx context.Context, id string, dek []byte) (repo.User, error)
	SetBirthYear(ctx context.Context, id string, birthYear *string, dek []byte) error
}

// SnapshotStore is the account-snapshot CRUD plus the equity series
// the savings appreciation chart reads.
type SnapshotStore interface {
	CreateSnapshot(ctx context.Context, userID string, in repo.SnapshotIn, dek []byte) (repo.Snapshot, error)
	ListSnapshots(ctx context.Context, userID, accountID string, dek []byte) ([]repo.Snapshot, error)
	GetSnapshot(ctx context.Context, userID, id string, dek []byte) (repo.Snapshot, error)
	UpdateSnapshot(ctx context.Context, userID, id string, p repo.SnapshotPatch, dek []byte) (repo.Snapshot, error)
	DeleteSnapshot(ctx context.Context, userID, id string) error
	EquitySeries(ctx context.Context, userID, accountID string, from, to time.Time) ([]repo.EquityPoint, error)
}

// PlanStore is the plan limit-override and room-adjustment surface
// the savings plan routes read and write.
type PlanStore interface {
	ListLimitOverrides(ctx context.Context, userID, planType string, year int) ([]repo.LimitOverride, error)
	UpsertLimitOverride(ctx context.Context, userID, planType string, year int, annualLimit string) (repo.LimitOverride, error)
	DeleteLimitOverride(ctx context.Context, userID, id string) error
	ListRoomAdjustments(ctx context.Context, userID, planType string, dek []byte) ([]repo.RoomAdjustment, error)
	CreateRoomAdjustment(ctx context.Context, userID, planType string, year int, amount, note string, dek []byte) (repo.RoomAdjustment, error)
	DeleteRoomAdjustment(ctx context.Context, userID, id string) error
}

// savingsRoomDTO is the GET /api/v1/savings/room response. Amounts are
// canonical decimal strings; the engine works in integer cents (no
// float64 anywhere in this file).
type savingsRoomDTO struct {
	Year            int              `json:"year"`
	BirthYear       *int             `json:"birth_year"`
	Cards           []savingsCardDTO `json:"cards"`
	RegisteredTotal string           `json:"registered_total"`
	InvestmentTotal string           `json:"investment_total"`
}

// savingsCardDTO is one plan card. Nullable amounts marshal as null when
// the engine has no value; warnings/notes never marshal as null.
type savingsCardDTO struct {
	PlanType          string   `json:"plan_type"`
	Beneficiary       string   `json:"beneficiary"`
	AnnualLimit       *string  `json:"annual_limit"`
	LimitSource       string   `json:"limit_source"`
	Used              string   `json:"used"`
	Room              *string  `json:"room"`
	ReCreatedNextYear string   `json:"re_created_next_year"`
	LifetimeLimit     *string  `json:"lifetime_limit"`
	LifetimeUsed      *string  `json:"lifetime_used"`
	LifetimeRoom      *string  `json:"lifetime_room"`
	RRIFMinimum       *string  `json:"rrif_minimum"`
	RRIFFactorBps     *int64   `json:"rrif_factor_bps"`
	RRIFAge           *int     `json:"rrif_age"`
	Warnings          []string `json:"warnings"`
	Notes             []string `json:"notes"`
}

// savingsRoom answers GET /api/v1/savings/room: per-plan room cards for
// the requested (default: current) calendar year plus the registered and
// investment rollups.
func (h *handler) savingsRoom(w http.ResponseWriter, r *http.Request) {
	year := time.Now().UTC().Year()
	if ys := r.URL.Query().Get("year"); ys != "" {
		n, err := strconv.Atoi(ys)
		if err != nil || n < 1900 || n > 2100 {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "year must be an integer between 1900 and 2100")
			return
		}
		year = n
	}

	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	user, err := h.users.Get(ctx, c.row.ID, c.dek)
	if err != nil {
		if errors.Is(err, repo.ErrUserNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "user not found")
			return
		}
		slog.Error("savings room: user lookup failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	accounts, err := h.accounts.List(ctx, c.row.ID, c.dek)
	if err != nil {
		slog.Error("savings room: account list failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	var planAccts, invAccts []repo.Account
	for _, a := range accounts {
		switch {
		case plans.IsPlanType(a.Type):
			planAccts = append(planAccts, a)
		case a.Type == "investment":
			invAccts = append(invAccts, a)
		}
	}

	sums, err := h.transactions.SavingsSums(ctx, c.row.ID)
	if err != nil {
		slog.Error("savings room: savings sums failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	var yearSums []plans.YearSums
	for _, s := range sums {
		counted := s.Contribution + s.Employer + s.Other
		yearSums = append(yearSums, plans.YearSums{
			AccountID:            s.AccountID,
			Year:                 s.Year,
			CountedContributions: max(0, counted),
			Withdrawals:          max(0, -counted),
			GrantInflows:         max(0, s.GovernmentGrant+s.Inherited),
		})
	}

	ovrs, err := h.plans.ListLimitOverrides(ctx, c.row.ID, "", 0)
	if err != nil {
		slog.Error("savings room: limit overrides failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	var overrides []plans.Override
	for _, o := range ovrs {
		limit, ok := toCents(o.AnnualLimit)
		if !ok {
			slog.Error("savings room: unparsable override limit", "id", o.ID, "annual_limit", o.AnnualLimit)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
		overrides = append(overrides, plans.Override{PlanType: plans.PlanType(o.PlanType), Year: o.Year, AnnualLimit: limit})
	}

	adjs, err := h.plans.ListRoomAdjustments(ctx, c.row.ID, "", c.dek)
	if err != nil {
		slog.Error("savings room: room adjustments failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	var adjustments []plans.Adjustment
	for _, a := range adjs {
		amount, ok := toCents(a.Amount)
		if !ok {
			slog.Error("savings room: unparsable adjustment amount", "id", a.ID, "amount", a.Amount)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
		adjustments = append(adjustments, plans.Adjustment{PlanType: plans.PlanType(a.PlanType), Year: a.Year, Amount: amount})
	}

	from := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Now().UTC()

	balances := map[string]int64{}
	for _, a := range planAccts {
		pts, err := h.snapshots.EquitySeries(ctx, c.row.ID, a.ID, from, to)
		if err != nil {
			slog.Error("savings room: equity series failed", "account", a.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
		if len(pts) > 0 {
			balances[a.ID] = pts[len(pts)-1].Balance
		}
	}

	// The RRIF minimum keys off the latest snapshot on or before Jan 1 of
	// the requested year, not the live balance.
	rrifSnaps := map[string]int64{}
	cutoff := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, a := range accounts {
		if a.Type != "rrif" {
			continue
		}
		ss, err := h.snapshots.ListSnapshots(ctx, c.row.ID, a.ID, c.dek)
		if err != nil {
			slog.Error("savings room: rrif snapshots failed", "account", a.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
		for _, s := range ss { // DESC by AsOf: first hit wins
			if !s.AsOf.After(cutoff) {
				cents, ok := toCents(s.Balance)
				if !ok {
					slog.Error("savings room: unparsable rrif snapshot balance", "id", s.ID, "balance", s.Balance)
					writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
					return
				}
				rrifSnaps[a.ID] = cents
				break
			}
		}
	}

	investmentTotal := int64(0)
	for _, a := range invAccts {
		pts, err := h.snapshots.EquitySeries(ctx, c.row.ID, a.ID, from, to)
		if err != nil {
			slog.Error("savings room: equity series failed", "account", a.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
		if len(pts) > 0 {
			investmentTotal += pts[len(pts)-1].Balance
		}
	}

	acctInputs := make([]plans.AccountInput, 0, len(planAccts))
	for _, a := range planAccts {
		acctInputs = append(acctInputs, plans.AccountInput{
			ID:                   a.ID,
			PlanType:             plans.PlanType(a.Type),
			BeneficiaryName:      a.BeneficiaryName,
			BeneficiaryBirthYear: a.BeneficiaryBirthYear,
			HolderBirthYear:      user.BirthYear,
		})
	}

	res := plans.Compute(plans.Input{
		Year:            year,
		Accounts:        acctInputs,
		Sums:            yearSums,
		Overrides:       overrides,
		Adjustments:     adjustments,
		HolderBirthYear: user.BirthYear,
		Balances:        balances,
		InvestmentTotal: investmentTotal,
		RRIFSnapshots:   rrifSnaps,
	})

	cards := make([]savingsCardDTO, 0, len(res.Cards))
	for _, cd := range res.Cards {
		cards = append(cards, savingsCardDTO{
			PlanType:          string(cd.PlanType),
			Beneficiary:       cd.Beneficiary,
			AnnualLimit:       centsOrNull(cd.AnnualLimit),
			LimitSource:       cd.LimitSource,
			Used:              centsToString(cd.Used),
			Room:              centsOrNull(cd.Room),
			ReCreatedNextYear: centsToString(cd.ReCreatedNextYear),
			LifetimeLimit:     centsOrNull(cd.LifetimeLimit),
			LifetimeUsed:      centsOrNull(cd.LifetimeUsed),
			LifetimeRoom:      centsOrNull(cd.LifetimeRoom),
			RRIFMinimum:       centsOrNull(cd.RRIFMinimum),
			RRIFFactorBps:     cd.RRIFFactorBps,
			RRIFAge:           cd.RRIFAge,
			Warnings:          nonNilStrings(cd.Warnings),
			Notes:             nonNilStrings(cd.Notes),
		})
	}

	writeJSON(w, http.StatusOK, savingsRoomDTO{
		Year:            year,
		BirthYear:       user.BirthYear,
		Cards:           cards,
		RegisteredTotal: centsToString(res.RegisteredTotal),
		InvestmentTotal: centsToString(res.InvestmentTotal),
	})
}

// patchBirthYear answers PATCH /api/v1/me/birth-year with the envelope
// tri-state: absent = no-op, "" = clear to NULL, "YYYY" = set. The repo
// rejects anything but a 4-digit year in 1900..current; any failure is a
// 400 (user-supplied value, not corruption).
func (h *handler) patchBirthYear(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		BirthYear triStateString `json:"birth_year"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	// The tri-state decodes to the envelope **string idiom; the store
	// speaks *string (nil = no-op, &"" = clear), so unwrap one level.
	var birthYear *string
	if body.BirthYear.set {
		birthYear = body.BirthYear.value
	}
	if err := h.users.SetBirthYear(r.Context(), c.row.ID, birthYear, c.dek); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "birth_year must be a 4-digit year between 1900 and the current year, or an empty string to clear")
		return
	}
	user, err := h.users.Get(r.Context(), c.row.ID, c.dek)
	if err != nil {
		if errors.Is(err, repo.ErrUserNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "user not found")
			return
		}
		slog.Error("patch birth year: user lookup failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		BirthYear *int `json:"birth_year"`
	}{user.BirthYear})
}

// toCents parses a canonical decimal amount (optional sign, digits,
// optional '.' + up to two fraction digits) into integer cents. No
// float64: the integer and fraction parts are parsed separately.
func toCents(s string) (int64, bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	rest := s[i:]
	intPart, fracPart := rest, ""
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		intPart, fracPart = rest[:dot], rest[dot+1:]
	}
	if intPart == "" || len(fracPart) > 2 || (len(intPart) > 1 && intPart[0] == '0') {
		return 0, false
	}
	for _, part := range []string{intPart, fracPart} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 63)
	if err != nil {
		return 0, false
	}
	var frac int64
	switch len(fracPart) {
	case 1:
		frac = int64(fracPart[0]-'0') * 10
	case 2:
		frac = int64(fracPart[0]-'0')*10 + int64(fracPart[1]-'0')
	}
	if whole > (math.MaxInt64-frac)/100 {
		return 0, false
	}
	cents := whole*100 + frac
	if neg {
		cents = -cents
	}
	return cents, true
}

// centsToString renders integer cents as a canonical decimal string:
// 113636 -> "1136.36", -200000 -> "-2000.00", 0 -> "0.00".
func centsToString(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := fmt.Sprintf("%d.%02d", c/100, c%100)
	if neg {
		s = "-" + s
	}
	return s
}

// centsOrNull maps a nullable cents amount to a nullable decimal string.
func centsOrNull(c *int64) *string {
	if c == nil {
		return nil
	}
	s := centsToString(*c)
	return &s
}

// nonNilStrings keeps the card warning/note arrays non-null in JSON.
func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}
