// Plan limit overrides + room adjustments (round 51c, migration 0019).
//
// A limit override pins the user's annual contribution limit for one
// (plan_type, year), replacing the statutory default; the room engine
// (internal/plans) consumes these per user. A room adjustment is a
// signed manual correction of a plan-year's room (legacy room, one-off
// grants the engine cannot infer); there is no uniqueness — several
// adjustments may stack per plan-year.
//
// Both tables are owned per user: every query is scoped on user_id, and
// a missing row and a row owned by another user look identical
// (existence not disclosed). The override table has no encrypted
// column; the adjustment note is sealed under the owner's DEK (NULL
// when empty, same rule as snapshot/transaction notes).
package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"finance-tracker/internal/plans"
)

// LimitOverride is one decoded plan_limit_overrides row: the annual
// limit in canonical decimal ("33810.00").
type LimitOverride struct {
	ID                   string
	PlanType             string
	Year                 int
	AnnualLimit          string // canonical decimal, e.g. "33810.00"
	CreatedAt, UpdatedAt time.Time
}

// RoomAdjustment is one decoded plan_room_adjustments row: the signed
// adjustment in canonical decimal (negative removes room) and the note
// decrypted ("" when the column is NULL).
type RoomAdjustment struct {
	ID        string
	PlanType  string
	Year      int
	Amount    string // canonical decimal, signed (negative allowed)
	Note      string // decrypted; "" when NULL
	CreatedAt time.Time
}

// PlanStore persists plan_limit_overrides and plan_room_adjustments.
// One store serves every user; each method takes a userID and scopes
// its SQL on it (same shape as SnapshotStore).
type PlanStore struct {
	pool *pgxpool.Pool
}

// NewPlanStore returns a PlanStore over pool (nil for a nil pool,
// mirroring the SnapshotStore constructor).
func NewPlanStore(pool *pgxpool.Pool) *PlanStore {
	if pool == nil {
		return nil
	}
	return &PlanStore{pool: pool}
}

// validatePlanTypeYear checks the plan-type vocabulary (the plans
// package) and the plausible plan-year window 1900..2100.
func validatePlanTypeYear(planType string, year int) error {
	if !plans.IsPlanType(planType) {
		return fmt.Errorf("plan type %q is not a registered plan", planType)
	}
	if year < 1900 || year > 2100 {
		return fmt.Errorf("plan year %d must be between 1900 and 2100", year)
	}
	return nil
}

// validateAnnualLimit normalizes s to the canonicalAmount grammar (the
// same numeric(14,2) column as snapshots) and rejects negatives: an
// annual limit is >= 0. canonicalAmount accepts a leading '-', so the
// sign is checked on the canonical form (same rule as snapshot
// balances).
func validateAnnualLimit(s string) (string, error) {
	can, err := canonicalAmount(s)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(can, "-") {
		return "", fmt.Errorf("annual limit %q must not be negative", s)
	}
	return can, nil
}

// validateAdjustmentAmount normalizes s to the canonicalAmount grammar;
// the sign is meaningful (a negative adjustment removes room).
func validateAdjustmentAmount(s string) (string, error) {
	return canonicalAmount(s)
}

// limitOverrideColumns is the shared SELECT/RETURNING shape: the limit
// arrives as numeric text (re-canonicalized from cents — pgx's numeric
// -> string decode is not scale-preserving, same rule as snapshots).
const limitOverrideColumns = "id, plan_type, year, annual_limit, created_at, updated_at"

// roomAdjustmentColumns is the shared SELECT/RETURNING shape: the
// amount as numeric text (re-canonicalized from cents), the note as a
// nullable pack.
const roomAdjustmentColumns = "id, plan_type, year, amount, note_encrypted, created_at"

// decodeLimitOverride decodes one plan_limit_overrides row: the annual
// limit round-trips through integer cents (never float64). A stored
// limit that does not parse is corruption, surfaced as an error for the
// caller to wrap (same rule as a snapshot balance that will not parse).
func decodeLimitOverride(id, planType string, year int, limit string, ca, ua time.Time) (LimitOverride, error) {
	cents, err := parseNumericToCents(limit)
	if err != nil {
		return LimitOverride{}, fmt.Errorf("repo: limit override %s annual limit %q not canonical: %w", id, limit, err)
	}
	return LimitOverride{
		ID: id, PlanType: planType, Year: year,
		AnnualLimit: formatCents(cents),
		CreatedAt:   ca, UpdatedAt: ua,
	}, nil
}

// decodeRoomAdjustment decodes one plan_room_adjustments row: the
// amount round-trips through integer cents (signed — negatives render
// "-100.00"), the note pack opens (NULL -> "").
func decodeRoomAdjustment(id, planType string, year int, amount string, noteEnc *[]byte, ca time.Time, dek []byte) (RoomAdjustment, error) {
	cents, err := parseNumericToCents(amount)
	if err != nil {
		return RoomAdjustment{}, fmt.Errorf("repo: room adjustment %s amount %q not canonical: %w", id, amount, err)
	}
	note, err := openEncryptedPack(noteEnc, "room adjustment "+id+" note", dek)
	if err != nil {
		return RoomAdjustment{}, err
	}
	return RoomAdjustment{
		ID: id, PlanType: planType, Year: year,
		Amount: formatCents(cents), Note: note,
		CreatedAt: ca,
	}, nil
}

// ListLimitOverrides returns the user's limit overrides, optionally
// filtered by plan type and/or year ("" / 0 = any), ordered plan_type,
// year. No DEK: the table has no encrypted column.
func (s *PlanStore) ListLimitOverrides(ctx context.Context, userID, planType string, year int) ([]LimitOverride, error) {
	q := `
		SELECT ` + limitOverrideColumns + `
		FROM plan_limit_overrides
		WHERE user_id = $1`
	args := []any{userID}
	if planType != "" {
		args = append(args, planType)
		q += fmt.Sprintf(" AND plan_type = $%d", len(args))
	}
	if year != 0 {
		args = append(args, year)
		q += fmt.Sprintf(" AND year = $%d", len(args))
	}
	q += "\n\t\tORDER BY plan_type, year"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: list limit overrides: %w", err)
	}
	defer rows.Close()
	out := []LimitOverride{}
	for rows.Next() {
		var (
			id, rowType string
			rowYear     int
			limit       string
			ca, ua      time.Time
		)
		if err := rows.Scan(&id, &rowType, &rowYear, &limit, &ca, &ua); err != nil {
			return nil, fmt.Errorf("repo: scan limit override: %w", err)
		}
		ov, err := decodeLimitOverride(id, rowType, rowYear, limit, ca, ua)
		if err != nil {
			return nil, err
		}
		out = append(out, ov)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list limit overrides: %w", err)
	}
	return out, nil
}

// UpsertLimitOverride inserts or updates the user's (plan_type, year)
// limit override and returns the stored row (inserted on first call,
// limit + updated_at refreshed on subsequent ones).
func (s *PlanStore) UpsertLimitOverride(ctx context.Context, userID, planType string, year int, annualLimit string) (LimitOverride, error) {
	if err := validatePlanTypeYear(planType, year); err != nil {
		return LimitOverride{}, err
	}
	limit, err := validateAnnualLimit(annualLimit)
	if err != nil {
		return LimitOverride{}, err
	}
	var (
		id, rowType string
		rowYear     int
		rowLimit    string
		ca, ua      time.Time
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO plan_limit_overrides (user_id, plan_type, year, annual_limit)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, plan_type, year) DO UPDATE
		SET annual_limit = EXCLUDED.annual_limit,
		    updated_at = now()
		RETURNING `+limitOverrideColumns,
		userID, planType, year, limit).Scan(&id, &rowType, &rowYear, &rowLimit, &ca, &ua)
	if err != nil {
		return LimitOverride{}, fmt.Errorf("repo: upsert limit override: %w", err)
	}
	return decodeLimitOverride(id, rowType, rowYear, rowLimit, ca, ua)
}

// DeleteLimitOverride removes one override owned by the user;
// ErrLimitOverrideNotFound when it is absent OR owned by another user
// (existence not disclosed).
func (s *PlanStore) DeleteLimitOverride(ctx context.Context, userID, id string) error {
	cmd, err := s.pool.Exec(ctx, `
		DELETE FROM plan_limit_overrides
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete limit override: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrLimitOverrideNotFound
	}
	return nil
}

// ListRoomAdjustments returns the user's room adjustments, optionally
// filtered by plan type ("" = any), ordered plan_type, year. Notes are
// decrypted with the caller's DEK.
func (s *PlanStore) ListRoomAdjustments(ctx context.Context, userID, planType string, dek []byte) ([]RoomAdjustment, error) {
	q := `
		SELECT ` + roomAdjustmentColumns + `
		FROM plan_room_adjustments
		WHERE user_id = $1`
	args := []any{userID}
	if planType != "" {
		args = append(args, planType)
		q += fmt.Sprintf(" AND plan_type = $%d", len(args))
	}
	q += "\n\t\tORDER BY plan_type, year"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: list room adjustments: %w", err)
	}
	defer rows.Close()
	out := []RoomAdjustment{}
	for rows.Next() {
		var (
			id, rowType string
			rowYear     int
			amount      string
			noteEnc     *[]byte
			ca          time.Time
		)
		if err := rows.Scan(&id, &rowType, &rowYear, &amount, &noteEnc, &ca); err != nil {
			return nil, fmt.Errorf("repo: scan room adjustment: %w", err)
		}
		adj, err := decodeRoomAdjustment(id, rowType, rowYear, amount, noteEnc, ca, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, adj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list room adjustments: %w", err)
	}
	return out, nil
}

// CreateRoomAdjustment inserts one room adjustment and returns it. The
// amount is signed (negative removes room); the note is sealed ("" ->
// SQL NULL). No uniqueness: a (plan_type, year) may carry several
// adjustments over its life.
func (s *PlanStore) CreateRoomAdjustment(ctx context.Context, userID, planType string, year int, amount, note string, dek []byte) (RoomAdjustment, error) {
	if err := validatePlanTypeYear(planType, year); err != nil {
		return RoomAdjustment{}, err
	}
	can, err := validateAdjustmentAmount(amount)
	if err != nil {
		return RoomAdjustment{}, err
	}
	notePack, err := sealOpt(dek, note) // "" -> SQL NULL
	if err != nil {
		return RoomAdjustment{}, fmt.Errorf("repo: seal room adjustment note: %w", err)
	}
	var (
		id, rowType string
		rowYear     int
		rowAmount   string
		noteEnc     *[]byte
		ca          time.Time
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO plan_room_adjustments (user_id, plan_type, year, amount, note_encrypted)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+roomAdjustmentColumns,
		userID, planType, year, can, notePack).Scan(&id, &rowType, &rowYear, &rowAmount, &noteEnc, &ca)
	if err != nil {
		return RoomAdjustment{}, fmt.Errorf("repo: create room adjustment: %w", err)
	}
	return decodeRoomAdjustment(id, rowType, rowYear, rowAmount, noteEnc, ca, dek)
}

// DeleteRoomAdjustment removes one adjustment owned by the user;
// ErrAdjustmentNotFound when it is absent OR owned by another user
// (existence not disclosed).
func (s *PlanStore) DeleteRoomAdjustment(ctx context.Context, userID, id string) error {
	cmd, err := s.pool.Exec(ctx, `
		DELETE FROM plan_room_adjustments
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete room adjustment: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrAdjustmentNotFound
	}
	return nil
}
