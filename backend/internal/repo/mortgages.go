// Per-user mortgage repository (plan step 7.1, migration 0009): the
// entire mortgage spec (principal, annual_rate, amortization_years,
// start_date, payment_frequency, extra_payment_amount/frequency,
// extra_prepayment_type) is stored per plan §5 as ONE DEK-encrypted
// blob — the marshalled spec sealed as a single crypto.Seal pack in
// spec_encrypted, with NO plaintext spec columns. The amortisation
// engine (7.2) and the UI (7.3) decrypt in-app. account_id is
// immutable: a mortgage belongs to exactly one account. The store does
// NOT validate the account (existence/type checks live in the server
// layer); a bad account_id surfaces as a foreign-key violation.
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MortgageSpec is the entire mortgage spec as one struct: marshalled to
// JSON and sealed as ONE pack (plan §5). Amounts are canonical 2-decimal
// strings; AmortizationYears is a JSON integer.
type MortgageSpec struct {
	Principal             string `json:"principal"`
	AnnualRate            string `json:"annual_rate"`
	AmortizationYears     int    `json:"amortization_years"`
	StartDate             string `json:"start_date"`
	PaymentFrequency      string `json:"payment_frequency"`
	ExtraPaymentAmount    string `json:"extra_payment_amount"`
	ExtraPaymentFrequency string `json:"extra_payment_frequency"`
	ExtraPrepaymentType   string `json:"extra_prepayment_type"`
}

// Mortgage is one mortgages row with its spec opened. The wire DTOs
// omit created_at/updated_at; they stay on the row, not here.
type Mortgage struct {
	ID        string
	UserID    string
	AccountID string
	Spec      MortgageSpec
}

// MortgageIn is the Create input: the account plus the 8 spec fields.
type MortgageIn struct {
	AccountID             string
	Principal             string
	AnnualRate            string
	AmortizationYears     int
	StartDate             string
	PaymentFrequency      string
	ExtraPaymentAmount    string
	ExtraPaymentFrequency string
	ExtraPrepaymentType   string
}

// MortgagePatch is the Update input; nil = untouched (absent in the
// PATCH body). AccountID is immutable and NOT patchable.
type MortgagePatch struct {
	Principal             *string
	AnnualRate            *string
	AmortizationYears     *int
	StartDate             *string
	PaymentFrequency      *string
	ExtraPaymentAmount    *string
	ExtraPaymentFrequency *string
	ExtraPrepaymentType   *string
}

// MortgageStore persists mortgages. One store serves every user; each
// method takes a userID and scopes its SQL on it.
type MortgageStore struct {
	pool *pgxpool.Pool
}

// NewMortgageStore returns a MortgageStore backed by pool (nil for a
// nil pool, mirroring the NewBudgetStore constructor).
func NewMortgageStore(pool *pgxpool.Pool) *MortgageStore {
	if pool == nil {
		return nil
	}
	return &MortgageStore{pool: pool}
}

// sealSpec marshals the spec to JSON and seals it as ONE pack (the §5
// blob). json.Marshal cannot fail on this struct (strings + int only).
func sealSpec(dek []byte, spec MortgageSpec) ([]byte, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("repo: marshal mortgage spec: %w", err)
	}
	pack, err := crypto.Seal(dek, specJSON)
	if err != nil {
		return nil, fmt.Errorf("repo: seal mortgage spec: %w", err)
	}
	return pack, nil
}

// decrypt opens the spec pack and parses the JSON back into Spec. A
// pack that does not open is a key/data mismatch the API surfaces as
// 500 (the DEK came from the user's own wrapped_dek, so this is
// corruption, not a client error) — same rule as the other stores.
func (m *Mortgage) decrypt(pack []byte, dek []byte) error {
	specJSON, err := crypto.Open(dek, pack)
	if err != nil {
		return fmt.Errorf("repo: decrypt mortgage %s spec: %w", m.ID, err)
	}
	if err := json.Unmarshal(specJSON, &m.Spec); err != nil {
		return fmt.Errorf("repo: parse mortgage %s spec: %w", m.ID, err)
	}
	return nil
}

// List returns the user's mortgages ordered by created_at. dek
// decrypts the specs.
func (s *MortgageStore) List(ctx context.Context, userID string, dek []byte) ([]Mortgage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, spec_encrypted
		FROM mortgages
		WHERE user_id = $1
		ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list mortgages: %w", err)
	}
	defer rows.Close()
	out := make([]Mortgage, 0, 4)
	for rows.Next() {
		var (
			m    Mortgage
			pack []byte
		)
		if err := rows.Scan(&m.ID, &m.AccountID, &pack); err != nil {
			return nil, fmt.Errorf("repo: scan mortgage: %w", err)
		}
		m.UserID = userID
		if err := m.decrypt(pack, dek); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list mortgages: %w", err)
	}
	return out, nil
}

// Get returns one of the user's mortgages, or ErrMortgageNotFound when
// the id is not the user's (existence is not disclosed across users).
func (s *MortgageStore) Get(ctx context.Context, userID, id string, dek []byte) (Mortgage, error) {
	var (
		m    Mortgage
		pack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, spec_encrypted
		FROM mortgages
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&m.AccountID, &pack)
	if errors.Is(err, sql.ErrNoRows) {
		return Mortgage{}, ErrMortgageNotFound
	}
	if err != nil {
		return Mortgage{}, fmt.Errorf("repo: get mortgage: %w", err)
	}
	m.ID = id
	m.UserID = userID
	if err := m.decrypt(pack, dek); err != nil {
		return Mortgage{}, err
	}
	return m, nil
}

// Create inserts a mortgage for the user and returns it. The whole
// spec seals as ONE pack (plan §5). The store does not check that the
// account exists or is a mortgage account — the server does; a bad
// account_id surfaces as a foreign-key violation.
func (s *MortgageStore) Create(ctx context.Context, userID string, in MortgageIn, dek []byte) (Mortgage, error) {
	spec := MortgageSpec{
		Principal:             in.Principal,
		AnnualRate:            in.AnnualRate,
		AmortizationYears:     in.AmortizationYears,
		StartDate:             in.StartDate,
		PaymentFrequency:      in.PaymentFrequency,
		ExtraPaymentAmount:    in.ExtraPaymentAmount,
		ExtraPaymentFrequency: in.ExtraPaymentFrequency,
		ExtraPrepaymentType:   in.ExtraPrepaymentType,
	}
	pack, err := sealSpec(dek, spec)
	if err != nil {
		return Mortgage{}, err
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO mortgages (user_id, account_id, spec_encrypted)
		VALUES ($1, $2, $3)
		RETURNING id`,
		userID, in.AccountID, pack).Scan(&id)
	if err != nil {
		return Mortgage{}, fmt.Errorf("repo: create mortgage: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's mortgage and
// returns the updated row. The whole spec re-seals as ONE pack. An
// unchanged patch (all nil, or every present field equal to the
// current spec) is a no-op: the row comes back without an UPDATE, so
// updated_at stays put (the budgets/cards no-op rule). AccountID is
// immutable.
func (s *MortgageStore) Update(ctx context.Context, userID, id string, p MortgagePatch, dek []byte) (Mortgage, error) {
	var pack []byte
	err := s.pool.QueryRow(ctx, `
		SELECT spec_encrypted
		FROM mortgages
		WHERE user_id = $1 AND id = $2`, userID, id).Scan(&pack)
	if errors.Is(err, sql.ErrNoRows) {
		return Mortgage{}, ErrMortgageNotFound
	}
	if err != nil {
		return Mortgage{}, fmt.Errorf("repo: get mortgage: %w", err)
	}
	var cur MortgageSpec
	specJSON, err := crypto.Open(dek, pack)
	if err != nil {
		return Mortgage{}, fmt.Errorf("repo: decrypt mortgage %s spec: %w", id, err)
	}
	if err := json.Unmarshal(specJSON, &cur); err != nil {
		return Mortgage{}, fmt.Errorf("repo: parse mortgage %s spec: %w", id, err)
	}

	next := cur
	changed := false
	if p.Principal != nil && *p.Principal != cur.Principal {
		next.Principal = *p.Principal
		changed = true
	}
	if p.AnnualRate != nil && *p.AnnualRate != cur.AnnualRate {
		next.AnnualRate = *p.AnnualRate
		changed = true
	}
	if p.AmortizationYears != nil && *p.AmortizationYears != cur.AmortizationYears {
		next.AmortizationYears = *p.AmortizationYears
		changed = true
	}
	if p.StartDate != nil && *p.StartDate != cur.StartDate {
		next.StartDate = *p.StartDate
		changed = true
	}
	if p.PaymentFrequency != nil && *p.PaymentFrequency != cur.PaymentFrequency {
		next.PaymentFrequency = *p.PaymentFrequency
		changed = true
	}
	if p.ExtraPaymentAmount != nil && *p.ExtraPaymentAmount != cur.ExtraPaymentAmount {
		next.ExtraPaymentAmount = *p.ExtraPaymentAmount
		changed = true
	}
	if p.ExtraPaymentFrequency != nil && *p.ExtraPaymentFrequency != cur.ExtraPaymentFrequency {
		next.ExtraPaymentFrequency = *p.ExtraPaymentFrequency
		changed = true
	}
	if p.ExtraPrepaymentType != nil && *p.ExtraPrepaymentType != cur.ExtraPrepaymentType {
		next.ExtraPrepaymentType = *p.ExtraPrepaymentType
		changed = true
	}

	if !changed {
		// No-op patch: return the current row without an UPDATE.
		return s.Get(ctx, userID, id, dek)
	}

	newPack, err := sealSpec(dek, next)
	if err != nil {
		return Mortgage{}, err
	}
	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE mortgages SET spec_encrypted = $3, updated_at = now() "+
			"WHERE user_id = $1 AND id = $2",
		userID, id, newPack)
	if err != nil {
		return Mortgage{}, fmt.Errorf("repo: update mortgage: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Mortgage{}, ErrMortgageNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's mortgage (nothing references mortgages;
// 0009 has no inbound FKs). An unknown or other user's id is
// ErrMortgageNotFound; the row is left untouched.
func (s *MortgageStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM mortgages
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete mortgage: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrMortgageNotFound
	}
	return nil
}
