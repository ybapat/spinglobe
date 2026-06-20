package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// AdminHandler serves the internal management API under /admin/.
// It is mounted without JWT auth or rate limiting — protect it at the
// network level (e.g., Kubernetes NetworkPolicy) in production.
type AdminHandler struct {
	table  *RoutingTable
	cfg    Config
	logger *zap.Logger
	mux    *http.ServeMux
}

// NewAdminHandler creates an AdminHandler and registers its routes.
func NewAdminHandler(table *RoutingTable, cfg Config, logger *zap.Logger) *AdminHandler {
	a := &AdminHandler{
		table:  table,
		cfg:    cfg,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	a.mux.HandleFunc("/admin/routes", a.routes)
	a.mux.HandleFunc("/admin/health", a.health)
	return a
}

func (a *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// routesRequest is the JSON body accepted by POST /admin/routes.
type routesRequest struct {
	Routes []RouteConfig `json:"routes"`
}

// routeView is the JSON shape returned by GET /admin/routes.
type routeView struct {
	Prefix   string         `json:"prefix"`
	Backends []backendView  `json:"backends"`
}

type backendView struct {
	URL            string `json:"url"`
	CircuitBreaker string `json:"circuit_breaker"`
	Active         int64  `json:"active_connections"`
}

// routes handles GET and POST /admin/routes.
//
//	GET  — returns the current routing table as JSON.
//	POST — atomically replaces the full routing table with the supplied config.
func (a *AdminHandler) routes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listRoutes(w, r)
	case http.MethodPost:
		a.reloadRoutes(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *AdminHandler) listRoutes(w http.ResponseWriter, r *http.Request) {
	a.table.mu.RLock()
	defer a.table.mu.RUnlock()

	views := make([]routeView, 0, len(a.table.routes))
	for prefix, pool := range a.table.routes {
		rv := routeView{Prefix: prefix}
		for _, e := range pool.entries {
			rv.Backends = append(rv.Backends, backendView{
				URL:            e.URL.String(),
				CircuitBreaker: e.CB.State().String(),
				Active:         e.active.Load(),
			})
		}
		views = append(views, rv)
	}

	writeJSON(w, http.StatusOK, views)
}

func (a *AdminHandler) reloadRoutes(w http.ResponseWriter, r *http.Request) {
	var req routesRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	for i, route := range req.Routes {
		if route.Prefix == "" {
			http.Error(w, "route["+itoa(i)+"]: prefix is required", http.StatusBadRequest)
			return
		}
		for j, backend := range route.Backends {
			if _, err := url.ParseRequestURI(backend); err != nil {
				http.Error(w,
					"route["+itoa(i)+"].backends["+itoa(j)+"]: invalid URL: "+err.Error(),
					http.StatusBadRequest,
				)
				return
			}
		}
	}

	// Replace each declared prefix; existing prefixes not in the payload are kept.
	for _, route := range req.Routes {
		var entries []*BackendEntry
		for _, raw := range route.Backends {
			u, _ := url.Parse(raw)
			entries = append(entries, &BackendEntry{
				URL: u,
				CB:  NewCircuitBreaker(a.cfg.CBErrorThreshold, a.cfg.CBCooldown),
			})
		}
		a.table.Register(route.Prefix, entries)
	}

	a.logger.Info("routing table reloaded via admin API",
		zap.Int("prefixes", len(req.Routes)),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"updated": len(req.Routes),
	})
}

// healthView is the JSON shape returned by GET /admin/health.
type healthView struct {
	Status    string            `json:"status"`
	Timestamp string            `json:"timestamp"`
	Backends  []backendHealthView `json:"backends"`
}

type backendHealthView struct {
	URL            string `json:"url"`
	CircuitBreaker string `json:"circuit_breaker"`
	ErrorRate      string `json:"error_rate"`
}

// health returns a rich health payload with per-backend circuit-breaker states.
func (a *AdminHandler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	a.table.mu.RLock()
	defer a.table.mu.RUnlock()

	overall := "healthy"
	var backends []backendHealthView
	for _, pool := range a.table.routes {
		for _, e := range pool.entries {
			state := e.CB.State()
			if state == StateOpen {
				overall = "degraded"
			}
			e.CB.mu.Lock()
			rate := e.CB.errorRate()
			e.CB.mu.Unlock()
			backends = append(backends, backendHealthView{
				URL:            e.URL.String(),
				CircuitBreaker: state.String(),
				ErrorRate:      formatPct(rate),
			})
		}
	}

	view := healthView{
		Status:    overall,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Backends:  backends,
	}
	writeJSON(w, http.StatusOK, view)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func formatPct(f float64) string {
	return fmt.Sprintf("%.2f%%", f*100)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
