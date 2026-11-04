// User settings (plan step 1.4a): GET/PATCH /api/v1/settings over the
// users-row settings columns (migration 0001) plus the onboarding
// completion marker (migration 0004). All settings columns are plaintext
// by plan §5 — no DEK involved here. The handlers bootstrap the user row
// first (same pattern as /v1/me) so a token that has never hit /me still
// gets a row and, therefore, settings.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSettingsNotFound is returned by the settings store when no users row
// exists for the sub. The handlers bootstrap the row before calling the
// store, so this path is defensive only (it maps to 404 not_found).
var ErrSettingsNotFound = errors.New("server: settings not found")

// Settings is one user's settings as served by GET /api/v1/settings.
// It is the users-row settings columns; OnboardedAt is nil until the
// onboarding form is submitted (the 1.4 gate the frontend routes on).
type Settings struct {
	Email           string     `json:"email"`
	DisplayName     string     `json:"display_name"`
	Language        string     `json:"language"`
	DefaultCurrency string     `json:"default_currency"`
	DefaultPeriod   string     `json:"default_period"`
	NotifyMonthly   bool       `json:"notify_monthly"`
	NotifyQuarterly bool       `json:"notify_quarterly"`
	NotifyAnnual    bool       `json:"notify_annual"`
	OnboardedAt     *time.Time `json:"onboarded_at"`
}

// SettingsPatch is the PATCH /api/v1/settings body. Every field is a
// pointer: nil = leave the stored value untouched. Onboarded is the
// one-way completion flag (true sets onboarded_at only-if-unset; false is
// rejected — un-onboarding is not a supported operation).
type SettingsPatch struct {
	Email           *string `json:"email"`
	DisplayName     *string `json:"display_name"`
	Language        *string `json:"language"`
	DefaultCurrency *string `json:"default_currency"`
	DefaultPeriod   *string `json:"default_period"`
	NotifyMonthly   *bool   `json:"notify_monthly"`
	NotifyQuarterly *bool   `json:"notify_quarterly"`
	NotifyAnnual    *bool   `json:"notify_annual"`
	Onboarded       *bool   `json:"onboarded"`
}

// SettingsStore persists user settings (the users-row settings columns).
// Implementations are keyed by the verified token sub.
type SettingsStore interface {
	Get(ctx context.Context, sub string) (Settings, error)
	Update(ctx context.Context, sub string, p SettingsPatch) (Settings, error)
}

// pgSettingsStore is the production SettingsStore backed by the users
// table. Update applies the non-nil patch fields, bumps updated_at, and
// returns the full updated row in one transaction so the response always
// reflects this write.
type pgSettingsStore struct {
	pool *pgxpool.Pool
}

// NewPGSettingsStore returns a SettingsStore backed by pool (nil for a
// nil pool; the settings routes then answer 503 like /v1/me).
func NewPGSettingsStore(pool *pgxpool.Pool) SettingsStore {
	if pool == nil {
		return nil
	}
	return &pgSettingsStore{pool: pool}
}

const settingsSelect = `
	SELECT email, display_name, language, default_currency, default_period,
	       notify_monthly, notify_quarterly, notify_annual, onboarded_at
	FROM users`

func (s *pgSettingsStore) Get(ctx context.Context, sub string) (Settings, error) {
	var st Settings
	err := s.pool.QueryRow(ctx, settingsSelect+` WHERE auth0_sub = $1`, sub).
		Scan(&st.Email, &st.DisplayName, &st.Language, &st.DefaultCurrency,
			&st.DefaultPeriod, &st.NotifyMonthly, &st.NotifyQuarterly, &st.NotifyAnnual,
			&st.OnboardedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, ErrSettingsNotFound
	}
	if err != nil {
		return Settings{}, fmt.Errorf("server: fetch settings %q: %w", sub, err)
	}
	return st, nil
}

func (s *pgSettingsStore) Update(ctx context.Context, sub string, p SettingsPatch) (Settings, error) {
	var (
		sets []string
		args []any
	)
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Email != nil {
		add("email", *p.Email)
	}
	if p.DisplayName != nil {
		add("display_name", *p.DisplayName)
	}
	if p.Language != nil {
		add("language", *p.Language)
	}
	if p.DefaultCurrency != nil {
		add("default_currency", *p.DefaultCurrency)
	}
	if p.DefaultPeriod != nil {
		add("default_period", *p.DefaultPeriod)
	}
	if p.NotifyMonthly != nil {
		add("notify_monthly", *p.NotifyMonthly)
	}
	if p.NotifyQuarterly != nil {
		add("notify_quarterly", *p.NotifyQuarterly)
	}
	if p.NotifyAnnual != nil {
		add("notify_annual", *p.NotifyAnnual)
	}
	// One-way completion: set only-if-unset, never clear, never overwrite.
	if p.Onboarded != nil {
		sets = append(sets, "onboarded_at = COALESCE(onboarded_at, now())")
	}
	sets = append(sets, "updated_at = now()")

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("server: begin settings update %q: %w", sub, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit
	args = append(args, sub)
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"UPDATE users SET %s WHERE auth0_sub = $%d",
		strings.Join(sets, ", "), len(args)), args...); err != nil {
		return Settings{}, fmt.Errorf("server: update settings %q: %w", sub, err)
	}
	// auth0_sub is UNIQUE, so at most one row; QueryRow keeps the
	// connection free for the commit that follows (a tx.Query row held
	// open across the commit is "conn busy").
	var st Settings
	err = tx.QueryRow(ctx, settingsSelect+` WHERE auth0_sub = $1`, sub).
		Scan(&st.Email, &st.DisplayName, &st.Language, &st.DefaultCurrency,
			&st.DefaultPeriod, &st.NotifyMonthly, &st.NotifyQuarterly, &st.NotifyAnnual,
			&st.OnboardedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, ErrSettingsNotFound
	}
	if err != nil {
		return Settings{}, fmt.Errorf("server: fetch settings %q: %w", sub, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Settings{}, fmt.Errorf("server: commit settings update %q: %w", sub, err)
	}
	return st, nil
}

// validatePatch checks the fields present in p and reports every
// violation at once (the onboarding form submits one round trip; surfacing
// all problems up front saves the user another cycle). The period set
// mirrors the users_default_period_check in 0001 so a bad value 400s
// instead of tripping the DB constraint (500).
func validatePatch(p SettingsPatch) error {
	empty := p.Email == nil && p.DisplayName == nil && p.Language == nil &&
		p.DefaultCurrency == nil && p.DefaultPeriod == nil &&
		p.NotifyMonthly == nil && p.NotifyQuarterly == nil && p.NotifyAnnual == nil &&
		p.Onboarded == nil
	if empty {
		return errors.New("empty patch: provide at least one field")
	}
	var problems []string
	if p.Email != nil {
		if *p.Email == "" {
			problems = append(problems, "email must not be empty")
		} else if len([]rune(*p.Email)) > 254 {
			problems = append(problems, "email must be at most 254 characters")
		}
	}
	if p.DisplayName != nil {
		if *p.DisplayName == "" {
			problems = append(problems, "display_name must not be empty")
		} else if len([]rune(*p.DisplayName)) > 255 {
			problems = append(problems, "display_name must be at most 255 characters")
		}
	}
	if p.Language != nil {
		if *p.Language == "" {
			problems = append(problems, "language must not be empty")
		} else if len([]rune(*p.Language)) > 16 {
			problems = append(problems, "language must be at most 16 characters")
		}
	}
	if p.DefaultCurrency != nil && !iso4217Re.MatchString(*p.DefaultCurrency) {
		problems = append(problems, `default_currency must be a 3-letter ISO 4217 code (e.g. "CAD")`)
	}
	if p.DefaultPeriod != nil && !validPeriods[*p.DefaultPeriod] {
		problems = append(problems, "default_period must be one of month|quarter|year")
	}
	if p.Onboarded != nil && !*p.Onboarded {
		problems = append(problems, "onboarded must be true (completion is one-way)")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

var (
	iso4217Re    = regexp.MustCompile(`^[A-Z]{3}$`)
	validPeriods = map[string]bool{"month": true, "quarter": true, "year": true}
)

// settingsPrecheck runs the guards shared by both settings routes and
// returns the verified identity. It writes the failure response itself.
func (h *handler) settingsPrecheck(w http.ResponseWriter, r *http.Request) (string, bool) {
	sub, err := h.auth.Identity(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, codeUnauthorized, "invalid or missing access token")
		return "", false
	}
	if h.bootstrap == nil || h.settings == nil {
		notConfigured(w)
		return "", false
	}
	if _, err := h.bootstrap.EnsureUser(r.Context(), sub); err != nil {
		slog.Error("user bootstrap failed", "sub", sub.Sub, "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, codeInternal, "user bootstrap failed")
		return "", false
	}
	return sub.Sub, true
}

func (h *handler) getSettings(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.settingsPrecheck(w, r)
	if !ok {
		return
	}
	st, err := h.settings.Get(r.Context(), sub)
	if err != nil {
		h.settingsError(w, sub, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *handler) patchSettings(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.settingsPrecheck(w, r)
	if !ok {
		return
	}
	var patch SettingsPatch
	if err := decodeBody(r, &patch); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest,
			"body must be a JSON object: "+err.Error())
		return
	}
	if err := validatePatch(patch); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	st, err := h.settings.Update(r.Context(), sub, patch)
	if err != nil {
		h.settingsError(w, sub, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// settingsError maps store errors to responses: missing row → 404
// not_found (defensive — the row is bootstrapped first), anything else →
// 500 internal with the details only in the server log.
func (h *handler) settingsError(w http.ResponseWriter, sub, op string, err error) {
	if errors.Is(err, ErrSettingsNotFound) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "settings not found")
		return
	}
	slog.Error("settings operation failed", "op", op, "sub", sub, "error", err)
	writeAPIError(w, http.StatusInternalServerError, codeInternal, "settings operation failed")
}
