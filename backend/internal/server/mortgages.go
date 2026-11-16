// Mortgage endpoints (plan step 7.1, migration 0009):
// GET/POST /api/v1/mortgages, GET/PATCH/DELETE /api/v1/mortgages/{id}
// over the repo.MortgageStore. The whole spec is DEK-encrypted at rest
// by the repo layer; the handlers only ever see plaintext in and out.
// Every store call is scoped to the caller's users.id, so a token can
// only ever touch its own rows. The account existence/type checks live
// here (the repo does not validate the account): a create consults
// h.accounts, and account_id is immutable (absent from the PATCH DTO).
package server

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

type MortgageStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Mortgage, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Mortgage, error)
	Create(ctx context.Context, userID string, in repo.MortgageIn, dek []byte) (repo.Mortgage, error)
	Update(ctx context.Context, userID, id string, p repo.MortgagePatch, dek []byte) (repo.Mortgage, error)
	Delete(ctx context.Context, userID, id string) error
}

// rateEvent is the wire shape of one stored rate change (round 23):
// served on the GET/POST bodies and accepted by the PATCH body, where a
// non-nil array FULLY replaces the stored events (an empty array clears
// them; nil = untouched). annual_rate is the canonical 2-decimal percent
// string, like the base spec's annual_rate.
type rateEvent struct {
	EffectiveDate string `json:"effective_date"`
	AnnualRate    string `json:"annual_rate"`
	RateType      string `json:"rate_type"`
	Action        string `json:"action"`
}

// mortgageOut is the wire DTO (RAW, no envelope, NO timestamps — the 3.1
// convention), the account plus the 8 spec fields in the pinned field
// order, then the term/renewal metadata (round 23, appended LAST).
// Amounts are canonical 2-decimal strings; annual_rate is a
// percent string (e.g. "4.99"); amortization_years and term_years are
// JSON integers. rate_events is [] when the spec carries none.
type mortgageOut struct {
	ID                    string      `json:"id"`
	AccountID             string      `json:"account_id"`
	Principal             string      `json:"principal"`
	AnnualRate            string      `json:"annual_rate"`
	AmortizationYears     int         `json:"amortization_years"`
	StartDate             string      `json:"start_date"`
	PaymentFrequency      string      `json:"payment_frequency"`
	ExtraPaymentAmount    string      `json:"extra_payment_amount"`
	ExtraPaymentFrequency string      `json:"extra_payment_frequency"`
	ExtraPrepaymentType   string      `json:"extra_prepayment_type"`
	TermYears             int         `json:"term_years"`
	RateType              string      `json:"rate_type"`
	RateEvents            []rateEvent `json:"rate_events"`
}

func toMortgageOut(m repo.Mortgage) mortgageOut {
	events := make([]rateEvent, 0, len(m.Spec.RateEvents))
	for _, e := range m.Spec.RateEvents {
		events = append(events, rateEvent{
			EffectiveDate: e.EffectiveDate,
			AnnualRate:    e.AnnualRate,
			RateType:      e.RateType,
			Action:        e.Action,
		})
	}
	return mortgageOut{
		ID:                    m.ID,
		AccountID:             m.AccountID,
		Principal:             m.Spec.Principal,
		AnnualRate:            m.Spec.AnnualRate,
		AmortizationYears:     m.Spec.AmortizationYears,
		StartDate:             m.Spec.StartDate,
		PaymentFrequency:      m.Spec.PaymentFrequency,
		ExtraPaymentAmount:    m.Spec.ExtraPaymentAmount,
		ExtraPaymentFrequency: m.Spec.ExtraPaymentFrequency,
		ExtraPrepaymentType:   m.Spec.ExtraPrepaymentType,
		TermYears:             m.Spec.TermYears,
		RateType:              m.Spec.RateType,
		RateEvents:            events,
	}
}

// mortgageIn is the POST body: pointer fields, nil = absent. The 9
// spec fields are required (a nil field is "<name> is required");
// term_years and rate_type are optional metadata (absent = 0 / "fixed").
// Unknown JSON fields are ignored by the decoder. Rate events are NOT
// settable on a create — they arrive later via the PATCH path.
type mortgageIn struct {
	AccountID             *string `json:"account_id"`
	Principal             *string `json:"principal"`
	AnnualRate            *string `json:"annual_rate"`
	AmortizationYears     *int    `json:"amortization_years"`
	StartDate             *string `json:"start_date"`
	PaymentFrequency      *string `json:"payment_frequency"`
	ExtraPaymentAmount    *string `json:"extra_payment_amount"`
	ExtraPaymentFrequency *string `json:"extra_payment_frequency"`
	ExtraPrepaymentType   *string `json:"extra_prepayment_type"`
	TermYears             *int    `json:"term_years"`
	RateType              *string `json:"rate_type"`
}

// mortgagePatch is the PATCH body: the 8 spec fields plus the term
// metadata (round 23), all optional (nil = untouched), at least one
// present. rate_events, when non-nil, FULLY replaces the stored events
// (an empty array clears them); payment_classifications (round 33) does
// the same for the stored aberrant-payment classifications — the only
// way to clear or correct a mis-answered item, since the classify
// endpoint upserts and never deletes. account_id is immutable and NOT
// in the patch DTO — a client-sent account_id is silently ignored by
// the decoder.
type mortgagePatch struct {
	Principal              *string                 `json:"principal"`
	AnnualRate             *string                 `json:"annual_rate"`
	AmortizationYears      *int                    `json:"amortization_years"`
	StartDate              *string                 `json:"start_date"`
	PaymentFrequency       *string                 `json:"payment_frequency"`
	ExtraPaymentAmount     *string                 `json:"extra_payment_amount"`
	ExtraPaymentFrequency  *string                 `json:"extra_payment_frequency"`
	ExtraPrepaymentType    *string                 `json:"extra_prepayment_type"`
	TermYears              *int                    `json:"term_years"`
	RateType               *string                 `json:"rate_type"`
	RateEvents             []rateEvent             `json:"rate_events"`
	PaymentClassifications []paymentClassification `json:"payment_classifications"`
}

// paymentClassification is one element of the patch's
// payment_classifications array (round 33): the same field set and
// rules as the classify endpoint's body (classifyIn), so an element
// that could not have been recorded by classify is rejected here too.
// The three rate fields are only meaningful for a rate_change; a
// one_off and a rejected ignore them.
type paymentClassification struct {
	Date           string `json:"date"`
	Kind           string `json:"kind"`
	Classification string `json:"classification"`
	AnnualRate     string `json:"annual_rate"`
	RateType       string `json:"rate_type"`
	Action         string `json:"action"`
}

// todayUTCMidnight is the server's current calendar date at UTC
// midnight. time.Parse("2006-01-02", s) yields exactly that shape for a
// date string, so the .After comparison between the two compares
// calendar dates (server local "today", via time.Now()'s date fields).
func todayUTCMidnight() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// specFieldProblems validates the PRESENT (non-nil) spec fields; nil =
// absent = skipped. The caller adds the account_id problems (shape
// validation for create; the existence/type checks run after, in the
// handler, only when account_id was shape-valid). rateEvents (round 23)
// is validated per event and as a strictly ascending sequence; each
// event's effective_date must also be after eventStart (a zero eventStart
// skips that check — the start_date shape problem already 400s).
func specFieldProblems(
	principal, annualRate, startDate, paymentFrequency,
	extraPaymentAmount, extraPaymentFrequency, extraPrepaymentType *string,
	amortizationYears, termYears *int,
	rateType *string,
	rateEvents []rateEvent,
	eventStart time.Time,
) []string {
	var problems []string
	if principal != nil {
		if c, err := repo.CanonicalAmount(*principal); err != nil {
			problems = append(problems, err.Error())
		} else if v, _ := strconv.ParseFloat(c, 64); v <= 0 {
			problems = append(problems, "principal must be greater than zero")
		}
	}
	if annualRate != nil {
		v, err := strconv.ParseFloat(*annualRate, 64)
		switch {
		case err != nil:
			problems = append(problems, "annual_rate must be a number")
		case math.IsNaN(v) || v <= 0 || v > 100:
			problems = append(problems, "annual_rate must be greater than 0 and at most 100")
		}
	}
	if amortizationYears != nil && (*amortizationYears < 1 || *amortizationYears > 40) {
		problems = append(problems, "amortization_years must be between 1 and 40")
	}
	if startDate != nil {
		d, err := time.Parse("2006-01-02", *startDate)
		switch {
		case err != nil:
			problems = append(problems, "start_date must be a date in YYYY-MM-DD form")
		case d.After(todayUTCMidnight()):
			problems = append(problems, "start_date must not be in the future")
		}
	}
	if paymentFrequency != nil {
		switch *paymentFrequency {
		case "monthly", "semi_monthly", "biweekly":
		default:
			problems = append(problems, "payment_frequency must be monthly, semi_monthly, or biweekly")
		}
	}
	if extraPaymentAmount != nil {
		if c, err := repo.CanonicalAmount(*extraPaymentAmount); err != nil {
			problems = append(problems, err.Error())
		} else if v, _ := strconv.ParseFloat(c, 64); v < 0 {
			problems = append(problems, "extra_payment_amount must not be negative")
		}
	}
	if extraPaymentFrequency != nil {
		switch *extraPaymentFrequency {
		case "monthly", "quarterly", "yearly":
		default:
			problems = append(problems, "extra_payment_frequency must be monthly, quarterly, or yearly")
		}
	}
	if extraPrepaymentType != nil {
		switch *extraPrepaymentType {
		case "term", "principal":
		default:
			problems = append(problems, "extra_prepayment_type must be term or principal")
		}
	}
	if termYears != nil && (*termYears < 0 || *termYears > 40) {
		problems = append(problems, "term_years must be between 0 and 40")
	}
	if rateType != nil {
		switch *rateType {
		case "fixed", "variable":
		default:
			problems = append(problems, "rate_type must be fixed or variable")
		}
	}
	for i, e := range rateEvents {
		d, derr := time.Parse("2006-01-02", e.EffectiveDate)
		if derr != nil {
			problems = append(problems, "rate event effective_date must be a date in YYYY-MM-DD form")
		} else {
			if !eventStart.IsZero() && !d.After(eventStart) {
				problems = append(problems, "rate event effective_date must be after start_date")
			}
			if i > 0 {
				// Strictly ascending across the array: compare against the
				// previous event's date (skipped when it did not parse —
				// that already reported its own form problem).
				prev, perr := time.Parse("2006-01-02", rateEvents[i-1].EffectiveDate)
				if perr == nil && !d.After(prev) {
					problems = append(problems, "rate event effective_dates must be strictly ascending")
				}
			}
		}
		v, rerr := strconv.ParseFloat(e.AnnualRate, 64)
		switch {
		case rerr != nil:
			problems = append(problems, "rate event annual_rate must be a number")
		case math.IsNaN(v) || v <= 0 || v > 100:
			problems = append(problems, "rate event annual_rate must be greater than 0 and at most 100")
		}
		switch e.RateType {
		case "fixed", "variable":
		default:
			problems = append(problems, "rate event rate_type must be fixed or variable")
		}
		switch e.Action {
		case "re_amortize", "keep_payment":
		default:
			problems = append(problems, "rate event action must be re_amortize or keep_payment")
		}
	}
	return problems
}

// validateMortgageIn collects EVERY create problem (all 9 fields
// required) and joins them with "; " (the settings convention, like
// validateBudgetIn).
func (h *handler) validateMortgageIn(in *mortgageIn) error {
	var problems []string
	switch {
	case in.AccountID == nil:
		problems = append(problems, "account_id is required")
	case !uuidShapeOK(*in.AccountID):
		problems = append(problems, "account_id must be a valid UUID")
	}
	// All 8 spec fields are required on a create (a nil field is
	// "<name> is required"); present fields are then checked in
	// specFieldProblems below, in wire order.
	if in.Principal == nil {
		problems = append(problems, "principal is required")
	}
	if in.AnnualRate == nil {
		problems = append(problems, "annual_rate is required")
	}
	if in.AmortizationYears == nil {
		problems = append(problems, "amortization_years is required")
	}
	if in.StartDate == nil {
		problems = append(problems, "start_date is required")
	}
	if in.PaymentFrequency == nil {
		problems = append(problems, "payment_frequency is required")
	}
	if in.ExtraPaymentAmount == nil {
		problems = append(problems, "extra_payment_amount is required")
	}
	if in.ExtraPaymentFrequency == nil {
		problems = append(problems, "extra_payment_frequency is required")
	}
	if in.ExtraPrepaymentType == nil {
		problems = append(problems, "extra_prepayment_type is required")
	}
	// Create takes the term metadata (term_years, rate_type) but NO rate
	// events (they arrive via PATCH), so the events arg is nil and the
	// after-start check is skipped (zero eventStart).
	problems = append(problems, specFieldProblems(
		in.Principal, in.AnnualRate, in.StartDate, in.PaymentFrequency,
		in.ExtraPaymentAmount, in.ExtraPaymentFrequency, in.ExtraPrepaymentType,
		in.AmortizationYears, in.TermYears, in.RateType, nil, time.Time{},
	)...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateMortgagePatch checks the present spec fields only (absent =
// untouched; no "… is required" on a patch). The rate events are checked
// against the EFFECTIVE start date: the patch's start_date when present,
// else the stored one (a patch that rewrites start_date and adds events
// in the same call is checked against the NEW start).
func (h *handler) validateMortgagePatch(p *mortgagePatch, stored *repo.Mortgage) error {
	var eventStart time.Time
	if p.StartDate != nil {
		eventStart, _ = time.Parse("2006-01-02", *p.StartDate)
	} else if stored != nil {
		eventStart, _ = time.Parse("2006-01-02", stored.Spec.StartDate)
	}
	problems := specFieldProblems(
		p.Principal, p.AnnualRate, p.StartDate, p.PaymentFrequency,
		p.ExtraPaymentAmount, p.ExtraPaymentFrequency, p.ExtraPrepaymentType,
		p.AmortizationYears, p.TermYears, p.RateType, p.RateEvents, eventStart,
	)
	// payment_classifications (round 33): each element is validated with
	// the classify endpoint's own rules (classifyProblems — no drift),
	// prefixed with the element's (kind, date) so several bad elements
	// stay readable when joined; the stored array keys on that pair, so
	// a duplicate is a 400 (classify's upsert would silently pick one).
	// No check against the CURRENT matching report: a classification may
	// outlive its item (the data can change), and the replacement is a
	// statement of intent, not a per-item action.
	seen := make(map[[2]string]bool, len(p.PaymentClassifications))
	for _, pc := range p.PaymentClassifications {
		key := [2]string{pc.Kind, pc.Date}
		if seen[key] {
			problems = append(problems, "payment classification "+pc.Kind+" on "+pc.Date+" is duplicated")
		}
		seen[key] = true
		b := classifyIn{Date: pc.Date, Kind: pc.Kind, Classification: pc.Classification,
			AnnualRate: pc.AnnualRate, RateType: pc.RateType, Action: pc.Action}
		for _, pr := range classifyProblems(b) {
			problems = append(problems, "payment classification "+pc.Kind+" on "+pc.Date+": "+pr)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// mortgageError maps store errors to responses. The sentinel mapping is
// what the frontend keys off; everything else is a 500 with the details
// only in the server log.
func (h *handler) mortgageError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrMortgageNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "mortgage not found")
	case errors.Is(err, repo.ErrAccountNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
	default:
		slog.Error("mortgage operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
	}
}

// mortgagesList serves GET /api/v1/mortgages: the caller's mortgages in
// the repo's order ([] when empty).
func (h *handler) mortgagesList(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ms, err := h.mortgages.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.mortgageError(w, "list", err)
		return
	}
	out := make([]mortgageOut, 0, len(ms))
	for _, m := range ms {
		out = append(out, toMortgageOut(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// mortgagesGet serves GET /api/v1/mortgages/{id}. A malformed id answers
// 404 (not_found) instead of reaching the store; a well-formed id the
// caller does not own is also 404 (existence is not disclosed).
func (h *handler) mortgagesGet(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "mortgage not found")
		return
	}
	m, err := h.mortgages.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.mortgageError(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, toMortgageOut(m))
}

// mortgagesCreate serves POST /api/v1/mortgages and answers 201 with the
// created row. The checks run in this order (the 7.1 pin): shape
// validation of ALL fields first (every problem "; "-joined, one 400),
// then the account existence/type checks — only when account_id was
// shape-valid.
func (h *handler) mortgagesCreate(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in mortgageIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateMortgageIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if in.AccountID != nil && uuidShapeOK(*in.AccountID) {
		acc, err := h.accounts.Get(r.Context(), c.row.ID, *in.AccountID, c.dek)
		if err != nil {
			h.mortgageError(w, "create", err)
			return
		}
		if acc.Type != "mortgage" {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account is not a mortgage account")
			return
		}
	}
	// Validation has already passed: canonicalize the amounts to the
	// 2-decimal string form and annual_rate to a percent string.
	principal, err := repo.CanonicalAmount(*in.Principal)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	extraPayment, err := repo.CanonicalAmount(*in.ExtraPaymentAmount)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	annualRate, err := strconv.ParseFloat(*in.AnnualRate, 64)
	if err != nil || math.IsNaN(annualRate) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "annual_rate must be a number")
		return
	}
	// term_years and rate_type are optional metadata: absent = 0 (no term
	// on the spec) and "fixed".
	termYears := 0
	if in.TermYears != nil {
		termYears = *in.TermYears
	}
	rateType := "fixed"
	if in.RateType != nil {
		rateType = *in.RateType
	}
	m, err := h.mortgages.Create(r.Context(), c.row.ID, repo.MortgageIn{
		AccountID:             *in.AccountID,
		Principal:             principal,
		AnnualRate:            strconv.FormatFloat(annualRate, 'f', 2, 64),
		AmortizationYears:     *in.AmortizationYears,
		StartDate:             *in.StartDate,
		PaymentFrequency:      *in.PaymentFrequency,
		ExtraPaymentAmount:    extraPayment,
		ExtraPaymentFrequency: *in.ExtraPaymentFrequency,
		ExtraPrepaymentType:   *in.ExtraPrepaymentType,
		TermYears:             termYears,
		RateType:              rateType,
	}, c.dek)
	if err != nil {
		h.mortgageError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toMortgageOut(m))
}

// mortgagesUpdate serves PATCH /api/v1/mortgages/{id} (same id rules as
// mortgagesGet). A patch with no field set is an empty patch; the
// present fields are validated with the same per-field rules as create.
func (h *handler) mortgagesUpdate(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "mortgage not found")
		return
	}
	var p mortgagePatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Principal == nil && p.AnnualRate == nil && p.AmortizationYears == nil &&
		p.StartDate == nil && p.PaymentFrequency == nil && p.ExtraPaymentAmount == nil &&
		p.ExtraPaymentFrequency == nil && p.ExtraPrepaymentType == nil &&
		p.TermYears == nil && p.RateType == nil && p.RateEvents == nil &&
		p.PaymentClassifications == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	// The row is fetched BEFORE field validation: the rate events are
	// checked against the stored start date when the patch carries none,
	// and an unknown id 404s even with a malformed body (the 7.1 house
	// order is otherwise unchanged; the empty patch still 400s first).
	m, err := h.mortgages.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.mortgageError(w, "update", err)
		return
	}
	if err := h.validateMortgagePatch(&p, &m); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	// Validation has already passed: canonicalize the present amounts so
	// the stored row stays canonical.
	patch := repo.MortgagePatch{
		AmortizationYears:     p.AmortizationYears,
		StartDate:             p.StartDate,
		PaymentFrequency:      p.PaymentFrequency,
		ExtraPaymentFrequency: p.ExtraPaymentFrequency,
		ExtraPrepaymentType:   p.ExtraPrepaymentType,
		TermYears:             p.TermYears,
		RateType:              p.RateType,
	}
	if p.Principal != nil {
		principal, err := repo.CanonicalAmount(*p.Principal)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		patch.Principal = &principal
	}
	if p.AnnualRate != nil {
		annualRate, err := strconv.ParseFloat(*p.AnnualRate, 64)
		if err != nil || math.IsNaN(annualRate) {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "annual_rate must be a number")
			return
		}
		rate := strconv.FormatFloat(annualRate, 'f', 2, 64)
		patch.AnnualRate = &rate
	}
	if p.ExtraPaymentAmount != nil {
		extraPayment, err := repo.CanonicalAmount(*p.ExtraPaymentAmount)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		patch.ExtraPaymentAmount = &extraPayment
	}
	if p.RateEvents != nil {
		// Full-array replacement: the events are canonicalized (rate to
		// the 2-decimal percent string) and stored verbatim, dates and
		// the rest as validated. A non-nil empty slice clears them.
		events := make([]repo.RateEvent, 0, len(p.RateEvents))
		for _, e := range p.RateEvents {
			rate, err := strconv.ParseFloat(e.AnnualRate, 64)
			if err != nil || math.IsNaN(rate) {
				writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "rate event annual_rate must be a number")
				return
			}
			events = append(events, repo.RateEvent{
				EffectiveDate: e.EffectiveDate,
				AnnualRate:    strconv.FormatFloat(rate, 'f', 2, 64),
				RateType:      e.RateType,
				Action:        e.Action,
			})
		}
		patch.RateEvents = events
	}
	if p.PaymentClassifications != nil {
		// Full-array replacement (round 33): the elements take the
		// stored shape of the classify endpoint — the rate fields are
		// set only for a rate_change (canonical 2-decimal rate), nil
		// for a one_off and a rejected. A non-nil empty slice clears
		// the stored classifications. Rate EVENTS are not touched here:
		// they stay under rate_events (and the classify endpoint).
		cls := make([]repo.PaymentClassification, 0, len(p.PaymentClassifications))
		for _, pc := range p.PaymentClassifications {
			out := repo.PaymentClassification{Date: pc.Date, Kind: pc.Kind, Classification: pc.Classification}
			if pc.Classification == "rate_change" {
				rate, _ := repo.CanonicalAmount(pc.AnnualRate) // validated above
				rateType, action := pc.RateType, pc.Action
				out.AnnualRate = &rate
				out.RateType = &rateType
				out.Action = &action
			}
			cls = append(cls, out)
		}
		patch.PaymentClassifications = cls
	}
	m, err = h.mortgages.Update(r.Context(), c.row.ID, id, patch, c.dek)
	if err != nil {
		h.mortgageError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toMortgageOut(m))
}

// mortgagesDelete serves DELETE /api/v1/mortgages/{id} and answers 204
// (no content) on success.
func (h *handler) mortgagesDelete(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "mortgage not found")
		return
	}
	if err := h.mortgages.Delete(r.Context(), c.row.ID, id); err != nil {
		h.mortgageError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
