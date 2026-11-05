// Package server provides the HTTP API surface: routing, liveness and
// readiness probes, and Prometheus metrics (OpenMetrics-capable via
// Accept-header negotiation).
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/authn"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// apiPrefix is the path prefix for all business API routes. Auth (step
// 1.3) is enforced for exactly this prefix and everything under it.
const apiPrefix = "/api"

// prefixAuth returns middleware that requires a valid access token (via v)
// for requests whose path is prefix or lives under prefix/, and passes all
// other requests through untouched. It is installed on the root router —
// not the /api subrouter — so that unauthenticated requests get 401
// regardless of route matching: a subrouter answers 404 via its own
// NotFoundHandler for paths with no matching route (verified empirically
// against chi v5.5.0 in 1.3b, D-023), which would mask the auth failure.
func prefixAuth(prefix string, v *authn.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if p != prefix && !strings.HasPrefix(p, prefix+"/") {
				next.ServeHTTP(w, r)
				return
			}
			v.Middleware(next).ServeHTTP(w, r)
		})
	}
}

// Readiness reports whether the API is ready to serve traffic. It is
// satisfied by *pgxpool.Pool and by test fakes.
type Readiness interface {
	Ping(ctx context.Context) error
}

// BuildInfo is injected in the main packages via -ldflags.
type BuildInfo struct {
	Version string
	Commit  string
}

type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ftrk_http_requests_total",
			Help: "HTTP requests processed by the API, by method, status code and route.",
		}, []string{"method", "code", "route"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ftrk_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ftrk_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

type handler struct {
	ready         Readiness
	auth          *authn.Verifier
	bootstrap     *Bootstrap
	settings      SettingsStore
	accounts      AccountStore
	categories    CategoryStore
	transactions  TransactionStore
	budgets       BudgetStore
	notifications NotificationStore
	schemas       SchemaStore
	imports       ImportStore
	m             *httpMetrics
}

// New assembles the root router with cross-cutting middleware and the
// probe/metrics endpoints. Each call creates a fresh Prometheus registry
// (so tests can build independent servers) seeded with the standard
// Go and process collectors. auth guards every route under /api (step
// 1.3) and must not be nil. bootstrap performs first-login user creation
// (step 1.3c) and may be nil, in which case /api/v1/me and the CRUD
// routes answer 503. settings serves GET/PATCH /api/v1/settings (step
// 1.4a) and may be nil, in which case the settings routes answer 503.
// accounts and categories serve the CRUD routes (step 3.1) and may be
// nil, in which case those routes answer 503. transactions serves the
// transaction CRUD routes (step 3.2); budgets and transactions together
// with categories serve GET /api/v1/summary — any one nil and the
// affected routes answer 503. budgets serves the budget CRUD routes
// (step 3.3); notifications serves the notification list/mark-read/
// unread-count routes (step 3.3); schemas serves the import-schema
// routes (step 3.3: GET /api/v1/import-schemas; step 4.5: POST —
// save a mapping as a preset); imports serves the import job routes
// (step 4.1: POST /api/v1/imports plus the list and single-GET; step
// 4.5: the presets/analyze/preview/mapping pipeline) — any nil and
// the affected routes answer 503.
func New(ready Readiness, info BuildInfo, auth *authn.Verifier, bootstrap *Bootstrap, settings SettingsStore, accounts AccountStore, categories CategoryStore, transactions TransactionStore, budgets BudgetStore, notifications NotificationStore, schemas SchemaStore, imports ImportStore) http.Handler {
	if auth == nil {
		panic("server.New: auth verifier is required (step 1.3)")
	}
	h := &handler{
		ready:         ready,
		auth:          auth,
		bootstrap:     bootstrap,
		settings:      settings,
		accounts:      accounts,
		categories:    categories,
		transactions:  transactions,
		budgets:       budgets,
		notifications: notifications,
		schemas:       schemas,
		imports:       imports,
		m:             newHTTPMetrics(),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ftrk_build_info",
		Help: "Build information (version, commit); the value is always 1.",
	}, []string{"version", "commit"})
	reg.MustRegister(h.m.requests, h.m.duration, h.m.inflight, buildInfo)
	buildInfo.WithLabelValues(info.Version, info.Commit).Set(1)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.Recoverer)
	r.Use(h.metrics)
	// Auth (step 1.3): every request under /api requires a valid access
	// token. Enforced on the root router as a prefix middleware (not on the
	// /api subrouter) — see prefixAuth for why subrouter scoping would let
	// 404s mask 401s.
	r.Use(prefixAuth(apiPrefix, auth))

	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.readyz)
	r.Mount("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	// Business API routes (step 1.3+). Auth is enforced by the prefix
	// middleware above; this group holds the concrete routes.
	r.Route(apiPrefix, func(r chi.Router) {
		r.Get("/v1/me", h.me)
		// User settings (step 1.4a). The users-row settings columns are
		// the onboarding state; PATCH carries the one-way `onboarded`
		// completion flag. The error envelope ({error: {code, message}})
		// is plan 3.1's shape, adopted here early.
		r.Get("/v1/settings", h.getSettings)
		r.Patch("/v1/settings", h.patchSettings)
		// Accounts (step 3.1). No single-GET route: the repo surface has
		// no Get-by-id use case for the API, and a list is the only read.
		r.Get("/v1/accounts", h.accountsList)
		r.Post("/v1/accounts", h.accountsCreate)
		r.Patch("/v1/accounts/{id}", h.accountsUpdate)
		r.Delete("/v1/accounts/{id}", h.accountsDelete)
		// Categories (step 3.1), same shape as accounts.
		r.Get("/v1/categories", h.categoriesList)
		r.Post("/v1/categories", h.categoriesCreate)
		r.Patch("/v1/categories/{id}", h.categoriesUpdate)
		r.Delete("/v1/categories/{id}", h.categoriesDelete)
		// Transactions (step 3.2). POST is manual entry (the 4.7
		// importer shares the repo's Create); a dedup collision is a
		// 409. No single-GET route, for the same reason as accounts.
		r.Get("/v1/transactions", h.transactionsList)
		r.Post("/v1/transactions", h.transactionsCreate)
		r.Patch("/v1/transactions/{id}", h.transactionsUpdate)
		r.Delete("/v1/transactions/{id}", h.transactionsDelete)
		// Summary (step 3.2): per-period aggregation with the active
		// budget targets and category names joined in.
		r.Get("/v1/summary", h.summary)
		// Budgets (step 3.3). No single-GET route (the List covers it);
		// period is immutable (absent from the PATCH body).
		r.Get("/v1/budgets", h.budgetsList)
		r.Post("/v1/budgets", h.budgetsCreate)
		r.Patch("/v1/budgets/{id}", h.budgetsUpdate)
		r.Delete("/v1/budgets/{id}", h.budgetsDelete)
		// Notifications (step 3.3): list with unread/limit/offset
		// filters, idempotent mark-read, and the unread count the
		// browser badge polls.
		r.Get("/v1/notifications", h.notificationsList)
		r.Post("/v1/notifications/{id}/read", h.notificationsMarkRead)
		r.Get("/v1/notifications/unread-count", h.notificationsUnreadCount)
		// Import schemas (steps 3.3 + 4.5): the builtin presets plus
		// the caller's customs (list), and the "save mapping as
		// preset" create (the builtins stay read-only code).
		r.Get("/v1/import-schemas", h.schemasList)
		r.Post("/v1/import-schemas", h.schemasCreate)
		// Import jobs (steps 4.1 + 4.5 + 4.6): multipart statement
		// upload (10 MB cap), the job list and single-GET, the mapping
		// pipeline: the preset list, the per-job analyze / preview /
		// set-mapping, and the run, which persists the mapped rows to
		// the ledger with dedup. The static /presets segment wins over
		// /{id} (chi routing).
		r.Post("/v1/imports", h.postImport)
		r.Get("/v1/imports", h.importsList)
		r.Get("/v1/imports/presets", h.importPresets)
		r.Get("/v1/imports/{id}", h.importGet)
		r.Get("/v1/imports/{id}/analyze", h.importAnalyze)
		r.Post("/v1/imports/{id}/preview", h.importPreview)
		r.Post("/v1/imports/{id}/mapping", h.importSetMapping)
		r.Post("/v1/imports/{id}/run", h.importRun)
	})
	return r
}

// caller is the authenticated, bootstrapped user a handler acts for:
// the users row (its ID scopes every store call) and the DEK the row's
// wrapped_dek unwraps to (the repo layer encrypts names with it).
type caller struct {
	row UserRow
	dek []byte
}

// requireUser turns a verified request into a caller, writing the
// failure response itself. It runs for every authenticated business
// route (step 3.1): the prefix middleware already proved the token
// (401s never reach here, the Identity check is defensive), then the
// user row is ensured and the DEK unwrapped so handlers can call the
// repo stores without re-doing bootstrap work.
func (h *handler) requireUser(w http.ResponseWriter, r *http.Request) (caller, bool) {
	var c caller
	id, err := h.auth.Identity(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, codeUnauthorized, "invalid or missing access token")
		return c, false
	}
	if h.bootstrap == nil {
		notConfigured(w)
		return c, false
	}
	row, err := h.bootstrap.EnsureUser(r.Context(), id)
	if err != nil {
		slog.Error("user bootstrap failed", "sub", id.Sub, "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, codeInternal, "user bootstrap failed")
		return c, false
	}
	dek, err := h.bootstrap.UnwrapDEK(row)
	if err != nil {
		slog.Error("user dek unwrap failed", "sub", id.Sub, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "decryption failed")
		return c, false
	}
	return caller{row: row, dek: dek}, true
}

// me is the first business route (step 1.3c): it idempotently ensures the
// caller's users row (creating it with a fresh wrapped DEK on first login)
// and returns the stored identity. Responses come from the ROW, not the
// token, so profile-claim fallbacks are applied exactly once.
func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Sub         string `json:"sub"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}{c.row.Sub, c.row.Email, c.row.DisplayName})
}

func (h *handler) metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.m.inflight.Inc()
		defer h.m.inflight.Dec()
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		h.m.requests.WithLabelValues(r.Method, strconv.Itoa(sw.status), route).Inc()
		h.m.duration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// healthz is the liveness probe: process is up, no dependencies checked.
func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is the readiness probe: reports not-ready (503) until the
// database is configured and reachable.
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.ready == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "no database configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.ready.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
