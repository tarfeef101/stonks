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

// mortgageOut is the wire DTO (RAW, no envelope, NO timestamps — the 3.1
// convention), the account plus the 8 spec fields in the pinned field
// order. Amounts are canonical 2-decimal strings; annual_rate is a
// percent string (e.g. "4.99"); amortization_years is a JSON integer.
type mortgageOut struct {
	ID                    string `json:"id"`
	AccountID             string `json:"account_id"`
	Principal             string `json:"principal"`
	AnnualRate            string `json:"annual_rate"`
	AmortizationYears     int    `json:"amortization_years"`
	StartDate             string `json:"start_date"`
	PaymentFrequency      string `json:"payment_frequency"`
	ExtraPaymentAmount    string `json:"extra_payment_amount"`
	ExtraPaymentFrequency string `json:"extra_payment_frequency"`
	ExtraPrepaymentType   string `json:"extra_prepayment_type"`
}

func toMortgageOut(m repo.Mortgage) mortgageOut {
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
	}
}

// mortgageIn is the POST body: pointer fields, nil = absent. All 9 are
// required (a nil field is "<name> is required"). Unknown JSON fields
// are ignored by the decoder.
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
}

// mortgagePatch is the PATCH body: the 8 spec fields only, all optional
// (nil = untouched), at least one present. account_id is immutable and
// NOT in the patch DTO — a client-sent account_id is silently ignored by
// the decoder.
type mortgagePatch struct {
	Principal             *string `json:"principal"`
	AnnualRate            *string `json:"annual_rate"`
	AmortizationYears     *int    `json:"amortization_years"`
	StartDate             *string `json:"start_date"`
	PaymentFrequency      *string `json:"payment_frequency"`
	ExtraPaymentAmount    *string `json:"extra_payment_amount"`
	ExtraPaymentFrequency *string `json:"extra_payment_frequency"`
	ExtraPrepaymentType   *string `json:"extra_prepayment_type"`
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
// handler, only when account_id was shape-valid).
func specFieldProblems(
	principal, annualRate, startDate, paymentFrequency,
	extraPaymentAmount, extraPaymentFrequency, extraPrepaymentType *string,
	amortizationYears *int,
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
	problems = append(problems, specFieldProblems(
		in.Principal, in.AnnualRate, in.StartDate, in.PaymentFrequency,
		in.ExtraPaymentAmount, in.ExtraPaymentFrequency, in.ExtraPrepaymentType,
		in.AmortizationYears,
	)...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateMortgagePatch checks the present spec fields only (absent =
// untouched; no "… is required" on a patch).
func (h *handler) validateMortgagePatch(p *mortgagePatch) error {
	problems := specFieldProblems(
		p.Principal, p.AnnualRate, p.StartDate, p.PaymentFrequency,
		p.ExtraPaymentAmount, p.ExtraPaymentFrequency, p.ExtraPrepaymentType,
		p.AmortizationYears,
	)
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
		p.ExtraPaymentFrequency == nil && p.ExtraPrepaymentType == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := h.validateMortgagePatch(&p); err != nil {
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
	m, err := h.mortgages.Update(r.Context(), c.row.ID, id, patch, c.dek)
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
