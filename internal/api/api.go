package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/service"
)

const Version = "0.1.0"

type API struct {
	cfg *config.Config
	svc *service.Service
}

type handlerFunc func(r *http.Request, sheet, tab string) (int, any, error)

func New(cfg *config.Config, svc *service.Service) *API { return &API{cfg: cfg, svc: svc} }

func (a *API) Routes() http.Handler { return CORS(a.cfg, a.mux()) }

func (a *API) Mux() http.Handler { return a.mux() }

func CORS(cfg *config.Config, next http.Handler) http.Handler { return (&API{cfg: cfg}).cors(next) }

func Bearer(r *http.Request) string { return bearer(r) }

func WriteJSON(w http.ResponseWriter, status int, body any) { writeJSON(w, status, body) }

func (a *API) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /v1/{sheet}/{tab}", a.endpoint("read", a.list))
	mux.Handle("GET /v1/{sheet}/{tab}/{id}", a.endpoint("read", a.get))
	mux.Handle("POST /v1/{sheet}/{tab}", a.endpoint("create", a.create))
	mux.Handle("PATCH /v1/{sheet}/{tab}/{id}", a.endpoint("update", a.update))
	mux.Handle("DELETE /v1/{sheet}/{tab}/{id}", a.endpoint("delete", a.delete))
	mux.HandleFunc("GET /openapi.json", a.openAPIHandler)
	mux.Handle("/mcp", a.mcpHandler())
	return mux
}

// cors lets browsers on the configured origins call the API. Keys travel in the Authorization
// header, not cookies, so credentials mode is never needed.
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.cfg.CORSOrigins) > 0 {
			w.Header().Add("Vary", "Origin") // so caches don't reuse a response across origins
		}
		origin := r.Header.Get("Origin")
		if origin == "" || !slices.ContainsFunc(a.cfg.CORSOrigins, func(o string) bool { return o == "*" || o == origin }) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// endpoint authenticates, authorizes, runs h, and logs the outcome (including refusal reasons).
func (a *API) endpoint(op string, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sheet, tab := r.PathValue("sheet"), r.PathValue("tab")
		key := a.cfg.KeyFor(bearer(r))
		status, body, err := func() (int, any, error) {
			if key == nil {
				return 0, nil, &service.APIError{Status: http.StatusUnauthorized, Msg: "missing or invalid API key"}
			}
			if _, ok := a.cfg.Rule(sheet, tab); !ok {
				return 0, nil, &service.APIError{Status: http.StatusNotFound, Msg: "unknown tab"}
			}
			if !a.cfg.Can(key, sheet, tab, op) {
				return 0, nil, &service.APIError{Status: http.StatusForbidden, Msg: op + " not allowed on " + sheet + "/" + tab}
			}
			return h(r, sheet, tab)
		}()
		reason := ""
		if err != nil {
			status, body, reason = errorResponse(err)
		}
		writeJSON(w, status, body)
		keyName := ""
		if key != nil {
			keyName = key.Name
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "op", op, "key", keyName,
			"status", status, "reason", reason, "duration_ms", time.Since(start).Milliseconds())
	})
}

// errorResponse hides upstream (Google) error details from callers; they are only logged.
func errorResponse(err error) (status int, body any, reason string) {
	var ae *service.APIError
	if errors.As(err, &ae) {
		return ae.Status, map[string]string{"error": ae.Msg}, ae.Msg
	}
	return http.StatusBadGateway, map[string]string{"error": "upstream error"}, err.Error()
}

func (a *API) list(r *http.Request, sheet, tab string) (int, any, error) {
	q := r.URL.Query()
	limit, offset, sort := 0, 0, ""
	filter := map[string]string{}
	for k := range q {
		var err error
		switch k {
		case "limit":
			limit, err = strconv.Atoi(q.Get(k))
		case "offset":
			offset, err = strconv.Atoi(q.Get(k))
		case "sort":
			sort = q.Get(k)
		default:
			filter[k] = q.Get(k)
		}
		if err != nil || limit < 0 || offset < 0 {
			return 0, nil, service.BadRequest("invalid %s", k)
		}
	}
	rows, err := a.svc.List(r.Context(), sheet, tab, filter, sort, limit, offset)
	return http.StatusOK, rows, err
}

func (a *API) get(r *http.Request, sheet, tab string) (int, any, error) {
	row, err := a.svc.Get(r.Context(), sheet, tab, r.PathValue("id"))
	return http.StatusOK, row, err
}

func (a *API) create(r *http.Request, sheet, tab string) (int, any, error) {
	fields, err := decodeBody(r)
	if err != nil {
		return 0, nil, err
	}
	row, err := a.svc.Create(r.Context(), sheet, tab, fields)
	return http.StatusCreated, row, err
}

func (a *API) update(r *http.Request, sheet, tab string) (int, any, error) {
	fields, err := decodeBody(r)
	if err != nil {
		return 0, nil, err
	}
	row, err := a.svc.Update(r.Context(), sheet, tab, r.PathValue("id"), fields)
	return http.StatusOK, row, err
}

func (a *API) delete(r *http.Request, sheet, tab string) (int, any, error) {
	id := r.PathValue("id")
	return http.StatusOK, map[string]string{"deleted": id}, a.svc.Delete(r.Context(), sheet, tab, id)
}

func decodeBody(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.UseNumber() // keep large integers exact
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, service.BadRequest("body must be a JSON object")
	}
	return fields, nil
}

func bearer(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
