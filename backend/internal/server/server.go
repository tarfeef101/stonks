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
	"finance-tracker/internal/mail"
	"finance-tracker/internal/repo"

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
	jobs          *repo.CategoriseJobStore
	review        ReviewStore
	reprompt      *Reprompter
	llmCreds      *LLMCredentials
	dashboards    DashboardStore
	widgets       DashboardWidgetStore
	mortgages     MortgageStore
	mailer        mail.Sender
	mailEnabled   bool
	institutions  InstitutionStore
	fiCredentials FICredentialStore
	offsets       OffsetStore
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
// the affected routes answer 503. jobs serves the categorise routes
// (plan 5.4: POST /api/v1/categorise, GET /api/v1/categorise/{id});
// nil and those routes answer 503. review serves the review-queue
// routes (plan 5.5: GET /api/v1/review, the accept and accept-all
// writes); nil and those routes answer 503. reprompt serves
// POST /api/v1/review/{id}/reprompt (re-run the LLM on one row); nil
// and that route answers 503. llmCreds serves the LLM credential routes
// (plan 5.6: GET /api/v1/llm-credentials, PUT and DELETE
// /api/v1/llm-credentials/{provider}, POST /api/v1/llm-credentials/test);
// nil and those routes answer 503. dashboards and widgets serve the
// dashboard routes (round 6 rework, migration 0013: GET/POST
// /api/v1/dashboards, PATCH/DELETE /api/v1/dashboards/{id},
// GET/POST /api/v1/dashboards/{id}/widgets, PATCH/DELETE
// /api/v1/dashboards/{id}/widgets/{wid}, PUT
// /api/v1/dashboards/{id}/widgets/order); either nil and the affected
// routes answer 503. mortgages serves the mortgage routes
// (plan 7.1: GET/POST /api/v1/mortgages, GET/PATCH/DELETE
// /api/v1/mortgages/{id}); nil and those routes answer 503.
// institutions serves the FI institution registry routes (plan 9.1:
// GET/POST /api/v1/fi/institutions — global, no user scoping); nil
// and those routes answer 503. fiCredentials serves the FI credential
// routes (plan 9.1: GET/POST /api/v1/fi/credentials, DELETE
// /api/v1/fi/credentials/{id}); nil and those routes answer 503.
// offsets serves the offset (net-to-zero) routes (migration 0012:
// POST /api/v1/offsets/scan, GET /api/v1/offsets/suggestions, the
// suggestion accept/dismiss writes, POST/GET /api/v1/offsets/groups
// and DELETE /api/v1/offsets/groups/{id}); nil and those routes answer
// 503.
// mailer
// (may be nil = no email; the settings routes then skip the verification
// send) delivers the transactional emails (step 8.1); mailEnabled
// reflects it in the settings envelope (mail_enabled).
func New(ready Readiness, info BuildInfo, auth *authn.Verifier, bootstrap *Bootstrap, settings SettingsStore, accounts AccountStore, categories CategoryStore, transactions TransactionStore, budgets BudgetStore, notifications NotificationStore, schemas SchemaStore, imports ImportStore, jobs *repo.CategoriseJobStore, review ReviewStore, reprompt *Reprompter, llmCreds *LLMCredentials, dashboards DashboardStore, widgets DashboardWidgetStore, mortgages MortgageStore, mailer mail.Sender, mailEnabled bool, institutions InstitutionStore, fiCredentials FICredentialStore, offsets OffsetStore) http.Handler {
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
		jobs:          jobs,
		review:        review,
		reprompt:      reprompt,
		llmCreds:      llmCreds,
		dashboards:    dashboards,
		widgets:       widgets,
		mortgages:     mortgages,
		mailer:        mailer,
		mailEnabled:   mailEnabled,
		institutions:  institutions,
		fiCredentials: fiCredentials,
		offsets:       offsets,
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
		// Dashboards (round 6 rework, migration 0013): named
		// dashboards of widgets, each widget a saved Explore view or a
		// markdown note. The /widgets segment and the /widgets/order
		// static tail win over /{wid} (chi routing); the order endpoint
		// re-sorts a dashboard's whole widget set in one write.
		r.Get("/v1/dashboards", h.dashboardsList)
		r.Post("/v1/dashboards", h.dashboardsCreate)
		r.Patch("/v1/dashboards/{id}", h.dashboardsUpdate)
		r.Delete("/v1/dashboards/{id}", h.dashboardsDelete)
		r.Get("/v1/dashboards/{id}/widgets", h.widgetsList)
		r.Post("/v1/dashboards/{id}/widgets", h.widgetsCreate)
		r.Patch("/v1/dashboards/{id}/widgets/{wid}", h.widgetsUpdate)
		r.Delete("/v1/dashboards/{id}/widgets/{wid}", h.widgetsDelete)
		r.Put("/v1/dashboards/{id}/widgets/order", h.widgetsSetOrder)
		// Mortgages (plan 7.1, migration 0009): the mortgage CRUD over a
		// single mortgage-typed account (existence/type checked in the
		// handler). No PUT; account_id is immutable (absent from the
		// PATCH body).
		r.Get("/v1/mortgages", h.mortgagesList)
		r.Post("/v1/mortgages", h.mortgagesCreate)
		r.Get("/v1/mortgages/{id}", h.mortgagesGet)
		r.Patch("/v1/mortgages/{id}", h.mortgagesUpdate)
		r.Delete("/v1/mortgages/{id}", h.mortgagesDelete)
		// Mortgage amortization + what-if (plan 7.3): read-only
		// schedule computation over the stored spec, plus a
		// stateless what-if (no second row is written).
		r.Get("/v1/mortgages/{id}/amortization", h.mortgageAmortizationGet)
		r.Post("/v1/mortgages/{id}/amortization/what-if", h.mortgageWhatIfPost)
		// FI institutions + credentials (plan 9.1, migration 0010):
		// the GLOBAL institution registry (auth required, no user
		// scoping) and the per-user tokens (the secrets are
		// DEK-encrypted in the repo layer; the DTOs carry none of
		// them). No institution-delete route (9.1 scope: institutions
		// are shared vocabulary).
		r.Get("/v1/fi/institutions", h.fiInstitutionsList)
		r.Post("/v1/fi/institutions", h.fiInstitutionsCreate)
		r.Get("/v1/fi/credentials", h.fiCredentialsList)
		r.Post("/v1/fi/credentials", h.fiCredentialsCreate)
		r.Delete("/v1/fi/credentials/{id}", h.fiCredentialsDelete)
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
		// Categorise (plan 5.4): the manual trigger (202, the job row)
		// and the status read the SPA polls; the worker (cmd/worker)
		// claims and drives the jobs.
		r.Post("/v1/categorise", h.postCategorise)
		r.Get("/v1/categorise/{id}", h.getCategorise)
		// Review queue (plan 5.5): the rows the categorise worker
		// flagged for review, the per-row accept, the accept-all bulk
		// action (the static segment, distinct depth from /{id}/accept),
		// and the single-row reprompt (re-run the LLM on one row).
		r.Get("/v1/review", h.reviewList)
		r.Post("/v1/review/accept-all", h.reviewAcceptAll)
		r.Post("/v1/review/{id}/accept", h.reviewAccept)
		r.Post("/v1/review/{id}/reprompt", h.reviewReprompt)
		// Offsets (migration 0012): the scan that proposes net-to-zero
		// pairs, the pending suggestions and their accept/dismiss
		// writes, and the hand-picked groups (create, list, dissolve).
		r.Post("/v1/offsets/scan", h.offsetsScan)
		r.Get("/v1/offsets/suggestions", h.offsetsSuggestionList)
		r.Post("/v1/offsets/suggestions/{id}/accept", h.offsetsSuggestionAccept)
		r.Post("/v1/offsets/suggestions/{id}/dismiss", h.offsetsSuggestionDismiss)
		r.Post("/v1/offsets/groups", h.offsetsGroupCreate)
		r.Get("/v1/offsets/groups", h.offsetsGroupList)
		r.Delete("/v1/offsets/groups/{id}", h.offsetsGroupClear)
		// LLM credentials (plan 5.6): the caller's per-provider LLM
		// endpoint and encrypted API key (key masked on read), plus the
		// test-connection probe. The static /test segment wins over
		// /{provider} (chi routing); it is POST-only, so a PUT to it is
		// just the {provider} route with provider "test" (400).
		r.Get("/v1/llm-credentials", h.llmCredsList)
		r.Put("/v1/llm-credentials/{provider}", h.llmCredsPut)
		r.Delete("/v1/llm-credentials/{provider}", h.llmCredsDelete)
		r.Post("/v1/llm-credentials/test", h.llmCredsTest)
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
