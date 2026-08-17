// Package httpapi exposes the lognorm service over HTTP.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"task045-lognorm/internal/lognorm"
)

// API wires a lognorm.Service to HTTP endpoints.
type API struct {
	svc *lognorm.Service
}

// New creates an API bound to the given service.
func New(svc *lognorm.Service) *API { return &API{svc: svc} }

// Handler returns the HTTP handler for all routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("POST /ingest", a.ingest)
	mux.HandleFunc("GET /logs", a.logs)
	mux.HandleFunc("GET /stats", a.stats)
	mux.HandleFunc("GET /logs/{id}", a.getLog)
	return mux
}

// writeJSON encodes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps a service error to the right status code and encodes it.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, lognorm.ErrLinesRequired),
		errors.Is(err, lognorm.ErrTooManyLines),
		errors.Is(err, lognorm.ErrLevelInvalid),
		errors.Is(err, lognorm.ErrLineTooLong):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"error": err.Error(), "status": status})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ingest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Lines []string `json:"lines"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON", "status": http.StatusBadRequest})
		return
	}
	if len(req.Lines) == 0 {
		writeError(w, lognorm.ErrLinesRequired)
		return
	}
	if len(req.Lines) > lognorm.MaxLines {
		writeError(w, lognorm.ErrTooManyLines)
		return
	}
	res := a.svc.Ingest(req.Lines, time.Now().UTC())
	writeJSON(w, http.StatusOK, res)
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := lognorm.Query{Limit: 100}

	if v := strings.TrimSpace(q.Get("min_level")); v != "" {
		v = strings.ToUpper(v)
		if !lognorm.IsValidLevel(v) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "min_level 非法", "status": http.StatusBadRequest})
			return
		}
		query.MinLevel = v
	}
	if v := q.Get("since"); v != "" {
		t, ok := parseQueryTime(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "since 非法", "status": http.StatusBadRequest})
			return
		}
		query.Since = t
		query.SinceSet = true
	}
	if v := q.Get("until"); v != "" {
		t, ok := parseQueryTime(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "until 非法", "status": http.StatusBadRequest})
			return
		}
		query.Until = t
		query.UntilSet = true
	}
	if v := q.Get("format"); v != "" {
		if !lognorm.IsValidFormat(v) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format 非法", "status": http.StatusBadRequest})
			return
		}
		query.Format = v
	}
	query.Q = q.Get("q")
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit 非法", "status": http.StatusBadRequest})
			return
		}
		if n > 1000 {
			n = 1000
		}
		query.Limit = n
	}

	recs := a.svc.Query(query)
	writeJSON(w, http.StatusOK, map[string]any{"logs": recs, "total": len(recs)})
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.Stats())
}

func (a *API) getLog(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.svc.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "日志不存在", "status": http.StatusNotFound})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// parseQueryTime parses a strict RFC3339 timestamp for query parameters.
func parseQueryTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
