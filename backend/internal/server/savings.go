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

// --- savings limit overrides and room adjustments (52a2) ---------------------

// limitOverrideDTO is the wire form of repo.LimitOverride.
type limitOverrideDTO struct {
	ID          string    `json:"id"`
	PlanType    string    `json:"plan_type"`
	Year        int       `json:"year"`
	AnnualLimit string    `json:"annual_limit"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// roomAdjustmentDTO is the wire form of repo.RoomAdjustment.
type roomAdjustmentDTO struct {
	ID        string    `json:"id"`
	PlanType  string    `json:"plan_type"`
	Year      int       `json:"year"`
	Amount    string    `json:"amount"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

func toLimitOverrideDTO(lo repo.LimitOverride) limitOverrideDTO {
	return limitOverrideDTO{
		ID:          lo.ID,
		PlanType:    lo.PlanType,
		Year:        lo.Year,
		AnnualLimit: lo.AnnualLimit,
		CreatedAt:   lo.CreatedAt.UTC(),
		UpdatedAt:   lo.UpdatedAt.UTC(),
	}
}

func toRoomAdjustmentDTO(a repo.RoomAdjustment) roomAdjustmentDTO {
	return roomAdjustmentDTO{
		ID:        a.ID,
		PlanType:  a.PlanType,
		Year:      a.Year,
		Amount:    a.Amount,
		Note:      a.Note,
		CreatedAt: a.CreatedAt.UTC(),
	}
}

// parseYearQuery parses a query-string year value as a base-10 integer;
// callers apply the range policy (1900..2100) where it applies.
func parseYearQuery(s string) (int, bool) {
	y, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return y, true
}

// pathID returns the last path segment (the chi {id}) of r.
func pathID(r *http.Request) string {
	i := strings.LastIndex(r.URL.Path, "/")
	if i < 0 || i == len(r.URL.Path)-1 {
		return ""
	}
	return r.URL.Path[i+1:]
}

// savingsLimitsList serves GET /api/v1/savings/limits: the caller's
// annual limit overrides, optionally filtered by plan_type and year
// (the store treats ""/0 as "any").
func (h *handler) savingsLimitsList(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	planType := q.Get("plan_type")
	if planType != "" && !plans.IsPlanType(planType) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "plan_type must be a known plan type")
		return
	}
	year := 0
	if s := q.Get("year"); s != "" {
		y, ok := parseYearQuery(s)
		if !ok || y < 1900 || y > 2100 {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "year must be an integer between 1900 and 2100")
			return
		}
		year = y
	}
	los, err := h.plans.ListLimitOverrides(r.Context(), c.row.ID, planType, year)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "listing limit overrides failed")
		return
	}
	out := make([]limitOverrideDTO, 0, len(los))
	for _, lo := range los {
		out = append(out, toLimitOverrideDTO(lo))
	}
	writeJSON(w, http.StatusOK, out)
}

// limitOverrideIn is the PUT /api/v1/savings/limits body.
type limitOverrideIn struct {
	PlanType    string `json:"plan_type"`
	Year        int    `json:"year"`
	AnnualLimit string `json:"annual_limit"`
}

// savingsLimitsUpsert serves PUT /api/v1/savings/limits: create or update
// the caller's annual limit override for (plan_type, year). Only rrsp,
// tfsa and fhsa plans have an annual limit (resp/rdsp have none, rrif
// has a minimum instead).
func (h *handler) savingsLimitsUpsert(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in limitOverrideIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	pt := plans.PlanType(in.PlanType)
	if pt != plans.PlanRRSP && pt != plans.PlanTFSA && pt != plans.PlanFHSA {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "annual limits are only supported for rrsp, tfsa and fhsa plans")
		return
	}
	if in.Year < 1900 || in.Year > 2100 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "year must be between 1900 and 2100")
		return
	}
	cents, ok := toCents(in.AnnualLimit)
	if !ok || cents < 0 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "annual_limit must be a decimal amount of at least 0.00")
		return
	}
	lo, err := h.plans.UpsertLimitOverride(r.Context(), c.row.ID, in.PlanType, in.Year, centsToString(cents))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "saving limit override failed")
		return
	}
	writeJSON(w, http.StatusOK, toLimitOverrideDTO(lo))
}

// savingsLimitsDelete serves DELETE /api/v1/savings/limits/{id} and
// answers 204 (no content) on success.
func (h *handler) savingsLimitsDelete(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if err := h.plans.DeleteLimitOverride(r.Context(), c.row.ID, id); err != nil {
		if errors.Is(err, repo.ErrLimitOverrideNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "limit override not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "deleting limit override failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// savingsAdjustmentsList serves GET /api/v1/savings/adjustments: the
// caller's room adjustments, optionally filtered by plan_type (store)
// and year (in memory; the store has no year parameter).
func (h *handler) savingsAdjustmentsList(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	planType := q.Get("plan_type")
	if planType != "" && !plans.IsPlanType(planType) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "plan_type must be a known plan type")
		return
	}
	year := 0
	haveYear := false
	if s := q.Get("year"); s != "" {
		y, ok := parseYearQuery(s)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "year must be an integer")
			return
		}
		year = y
		haveYear = true
	}
	ras, err := h.plans.ListRoomAdjustments(r.Context(), c.row.ID, planType, c.dek)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "listing room adjustments failed")
		return
	}
	out := make([]roomAdjustmentDTO, 0, len(ras))
	for _, a := range ras {
		if haveYear && a.Year != year {
			continue
		}
		out = append(out, toRoomAdjustmentDTO(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// roomAdjustmentIn is the POST /api/v1/savings/adjustments body.
type roomAdjustmentIn struct {
	PlanType string `json:"plan_type"`
	Year     int    `json:"year"`
	Amount   string `json:"amount"`
	Note     string `json:"note"`
}

// maxAdjustmentNoteLen bounds room adjustments.note.
const maxAdjustmentNoteLen = 500

// savingsAdjustmentsCreate serves POST /api/v1/savings/adjustments:
// record a manual adjustment to a plan's room for a year (e.g. room
// reclaimed from an under-contribution year).
func (h *handler) savingsAdjustmentsCreate(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in roomAdjustmentIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	if !plans.IsPlanType(in.PlanType) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "plan_type must be a known plan type")
		return
	}
	if in.Year < 1900 || in.Year > 2100 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "year must be between 1900 and 2100")
		return
	}
	cents, ok := toCents(in.Amount)
	if !ok || cents == 0 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "amount must be a non-zero decimal amount")
		return
	}
	if len(in.Note) > maxAdjustmentNoteLen {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("note must be at most %d characters", maxAdjustmentNoteLen))
		return
	}
	a, err := h.plans.CreateRoomAdjustment(r.Context(), c.row.ID, in.PlanType, in.Year, centsToString(cents), in.Note, c.dek)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "saving room adjustment failed")
		return
	}
	writeJSON(w, http.StatusCreated, toRoomAdjustmentDTO(a))
}

// savingsAdjustmentsDelete serves DELETE /api/v1/savings/adjustments/{id}
// and answers 204 (no content) on success.
func (h *handler) savingsAdjustmentsDelete(w http.ResponseWriter, r *http.Request) {
	if h.plans == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if err := h.plans.DeleteRoomAdjustment(r.Context(), c.row.ID, id); err != nil {
		if errors.Is(err, repo.ErrAdjustmentNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "room adjustment not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "deleting room adjustment failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- savings snapshots and equity series (52a3) ------------------------------

// maxSnapshotNoteLen bounds savings snapshots.note.
const maxSnapshotNoteLen = 500

// snapshotDTO is the wire form of repo.Snapshot. AsOf is a UTC calendar
// date; the timestamps are RFC3339 (UTC).
type snapshotDTO struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	AsOf      string    `json:"as_of"`
	Balance   string    `json:"balance"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSnapshotDTO(s repo.Snapshot) snapshotDTO {
	return snapshotDTO{
		ID:        s.ID,
		AccountID: s.AccountID,
		AsOf:      s.AsOf.UTC().Format("2006-01-02"),
		Balance:   s.Balance,
		Note:      s.Note,
		CreatedAt: s.CreatedAt.UTC(),
		UpdatedAt: s.UpdatedAt.UTC(),
	}
}

// equityPointDTO is one point of the GET /api/v1/savings/equity response.
type equityPointDTO struct {
	AsOf             string `json:"as_of"`
	Balance          string `json:"balance"`
	NetContributions string `json:"net_contributions"`
}

// parseSnapshotDate parses a strict "YYYY-MM-DD" value at UTC midnight
// (time.Parse rejects unzero-padded and garbage input) and requires the
// year to be 1900..2100. It cannot reuse the shared parseDateParam: that
// helper takes a field-name parameter and returns an error, so it would
// redeclare the name in this package.
func parseSnapshotDate(s string) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	if y := d.Year(); y < 1900 || y > 2100 {
		return time.Time{}, false
	}
	return d, true
}

// probeSavingsAccount enforces that accountID belongs to the caller.
// It answers 404 for a missing/foreign account and 500 for store
// failures, and reports whether the handler may continue.
func (h *handler) probeSavingsAccount(w http.ResponseWriter, ctx context.Context, callerID string, dek []byte, accountID string) bool {
	if _, err := h.accounts.Get(ctx, callerID, accountID, dek); err != nil {
		if errors.Is(err, repo.ErrAccountNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
			return false
		}
		slog.Error("savings snapshots: account probe failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return false
	}
	return true
}

// savingsSnapshotsList serves GET /api/v1/savings/snapshots: the
// caller's snapshots for one account, newest first.
func (h *handler) savingsSnapshotsList(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	acctID := r.URL.Query().Get("account_id")
	if acctID == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id is required")
		return
	}
	if !h.probeSavingsAccount(w, ctx, c.row.ID, c.dek, acctID) {
		return
	}
	ss, err := h.snapshots.ListSnapshots(ctx, c.row.ID, acctID, c.dek)
	if err != nil {
		slog.Error("savings snapshots: list failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	out := make([]snapshotDTO, 0, len(ss))
	for _, s := range ss {
		out = append(out, toSnapshotDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// snapshotIn is the POST /api/v1/savings/snapshots body.
type snapshotIn struct {
	AccountID string `json:"account_id"`
	AsOf      string `json:"as_of"`
	Balance   string `json:"balance"`
	Note      string `json:"note"`
}

// savingsSnapshotsCreate serves POST /api/v1/savings/snapshots: record
// a manual balance for an account as of a date that is not in the
// future. A duplicate (account, as_of) is a 409 conflict.
func (h *handler) savingsSnapshotsCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var in snapshotIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	if in.AccountID == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id is required")
		return
	}
	if !h.probeSavingsAccount(w, ctx, c.row.ID, c.dek, in.AccountID) {
		return
	}
	asOf, ok := parseSnapshotDate(in.AsOf)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "as_of must be a YYYY-MM-DD date between 1900-01-01 and 2100-12-31")
		return
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if asOf.After(today) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "as_of must not be in the future")
		return
	}
	cents, ok := toCents(in.Balance)
	if !ok || cents < 0 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "balance must be a decimal amount of at least 0.00")
		return
	}
	if len(in.Note) > maxSnapshotNoteLen {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("note must be at most %d characters", maxSnapshotNoteLen))
		return
	}
	s, err := h.snapshots.CreateSnapshot(ctx, c.row.ID, repo.SnapshotIn{
		AccountID: in.AccountID,
		AsOf:      asOf,
		Balance:   centsToString(cents),
		Note:      in.Note,
	}, c.dek)
	if err != nil {
		if errors.Is(err, repo.ErrSnapshotConflict) {
			writeAPIError(w, http.StatusConflict, codeConflict, "a snapshot already exists for this account on this date")
			return
		}
		slog.Error("savings snapshots: create failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toSnapshotDTO(s))
}

// savingsSnapshotsPatch serves PATCH /api/v1/savings/snapshots/{id}
// with the envelope tri-state: an absent field is untouched, JSON null
// clears note (balance cannot be null), a string sets the value.
func (h *handler) savingsSnapshotsPatch(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := pathID(r)
	var body struct {
		Balance triStateString `json:"balance"`
		Note    triStateString `json:"note"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request body")
		return
	}
	// balance: absent -> nil; present null -> 400; present value -> the
	// canonical decimal string (a pointer, since "" is a legal value).
	var balance *string
	if body.Balance.set {
		if body.Balance.value == nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "balance cannot be null")
			return
		}
		cents, ok := toCents(*body.Balance.value)
		if !ok || cents < 0 {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "balance must be a decimal amount of at least 0.00")
			return
		}
		s := centsToString(cents)
		balance = &s
	}
	// note: absent -> nil; present null -> &"" (clear); present value ->
	// the value (bounded to 500 bytes).
	var note *string
	if body.Note.set {
		if body.Note.value != nil && len(*body.Note.value) > maxSnapshotNoteLen {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("note must be at most %d characters", maxSnapshotNoteLen))
			return
		}
		if body.Note.value == nil {
			empty := ""
			note = &empty
		} else {
			note = body.Note.value
		}
	}
	s, err := h.snapshots.UpdateSnapshot(ctx, c.row.ID, id, repo.SnapshotPatch{Balance: balance, Note: note}, c.dek)
	if err != nil {
		if errors.Is(err, repo.ErrSnapshotNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "snapshot not found")
			return
		}
		slog.Error("savings snapshots: update failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toSnapshotDTO(s))
}

// savingsSnapshotsDelete serves DELETE /api/v1/savings/snapshots/{id}
// and answers 204 (no content) on success.
func (h *handler) savingsSnapshotsDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := pathID(r)
	if err := h.snapshots.DeleteSnapshot(ctx, c.row.ID, id); err != nil {
		if errors.Is(err, repo.ErrSnapshotNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "snapshot not found")
			return
		}
		slog.Error("savings snapshots: delete failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// savingsEquity serves GET /api/v1/savings/equity: the account's
// balance and net-contribution series over a date range (default: the
// last ten years through today, UTC).
func (h *handler) savingsEquity(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	acctID := q.Get("account_id")
	if acctID == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id is required")
		return
	}
	now := time.Now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if s := q.Get("to"); s != "" {
		d, ok := parseSnapshotDate(s)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "to must be a YYYY-MM-DD date between 1900-01-01 and 2100-12-31")
			return
		}
		to = d
	}
	from := to.AddDate(-10, 0, 0)
	if s := q.Get("from"); s != "" {
		d, ok := parseSnapshotDate(s)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "from must be a YYYY-MM-DD date between 1900-01-01 and 2100-12-31")
			return
		}
		from = d
	}
	if from.After(to) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "from must not be after to")
		return
	}
	pts, err := h.snapshots.EquitySeries(ctx, c.row.ID, acctID, from, to)
	if err != nil {
		if errors.Is(err, repo.ErrAccountNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
			return
		}
		slog.Error("savings equity: equity series failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	out := make([]equityPointDTO, 0, len(pts))
	for _, p := range pts {
		out = append(out, equityPointDTO{
			AsOf:             p.AsOf.UTC().Format("2006-01-02"),
			Balance:          centsToString(p.Balance),
			NetContributions: centsToString(p.NetContributions),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Points []equityPointDTO `json:"points"`
	}{out})
}
