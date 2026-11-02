// Package server provides the HTTP API surface: routing, liveness and
// readiness probes, and Prometheus metrics (OpenMetrics-capable via
// Accept-header negotiation).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
	ready Readiness
	m     *httpMetrics
}

// New assembles the root router with cross-cutting middleware and the
// probe/metrics endpoints. Each call creates a fresh Prometheus registry
// (so tests can build independent servers) seeded with the standard
// Go and process collectors.
func New(ready Readiness, info BuildInfo) http.Handler {
	h := &handler{ready: ready, m: newHTTPMetrics()}

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

	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.readyz)
	r.Mount("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	return r
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
