package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

const Version = "0.1.0"

type API struct {
	settings *db.Settings
	svc      *service.Service
	mcp      http.Handler
}

type handlerFunc func(a *API, r *http.Request, sheet, tab string) (int, any, error)

func New(settings *db.Settings, svc *service.Service) *API {
	a := &API{settings: settings, svc: svc}
	a.mcp = a.mcpHandler()
	return a
}

type ctxKey struct{}

// NewContext returns ctx carrying a, the API that serves requests made with ctx.
func NewContext(ctx context.Context, a *API) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// Mux serves a alone, without tenancy.
func (a *API) Mux() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), a)))
		})
	})
	Routes(r)
	return r
}

func Bearer(r *http.Request) string { return bearer(r) }

func WriteJSON(w http.ResponseWriter, status int, body any) { writeJSON(w, status, body) }

// Routes adds the REST endpoints, the OpenAPI document and the MCP endpoint to r. Each request is
// served by the API that middleware put in its context with NewContext.
func Routes(r chi.Router) {
	r.Method("GET", "/v1/{sheet}/{tab}", endpoint("read", (*API).list))
	r.Method("GET", "/v1/{sheet}/{tab}/{id}", endpoint("read", (*API).get))
	r.Method("POST", "/v1/{sheet}/{tab}", endpoint("create", (*API).create))
	r.Method("PATCH", "/v1/{sheet}/{tab}/{id}", endpoint("update", (*API).update))
	r.Method("DELETE", "/v1/{sheet}/{tab}/{id}", endpoint("delete", (*API).delete))
	r.Get("/openapi.json", serve((*API).openAPIHandler))
	r.Handle("/mcp", serve(func(a *API, w http.ResponseWriter, r *http.Request) { a.mcp.ServeHTTP(w, r) }))
}

// serve runs h with the request's API, or fails when no middleware supplied one.
func serve(h func(*API, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := r.Context().Value(ctxKey{}).(*API)
		if !ok {
			slog.Error("api: no API in the request context", "path", r.URL.Path)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		h(a, w, r)
	}
}

// pathValue returns the decoded path parameter name. chi matches on the escaped path when the
// request has one, such as a tab name with a slash in it, so its values can still be escaped.
func pathValue(r *http.Request, name string) string {
	v := chi.URLParam(r, name)
	if r.URL.RawPath != "" {
		if u, err := url.PathUnescape(v); err == nil {
			return u
		}
	}
	return v
}

// CORS lets browsers on origins call the API. Keys travel in the Authorization
// header, not cookies, so credentials mode is never needed.
func CORS(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(origins) > 0 {
			w.Header().Add("Vary", "Origin") // so caches don't reuse a response across origins
		}
		origin := r.Header.Get("Origin")
		if origin == "" || !slices.ContainsFunc(origins, func(o string) bool { return o == "*" || o == origin }) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		// so browser apps can page through a list and see their quota
		h.Set("Access-Control-Expose-Headers", "X-Total-Count, RateLimit, RateLimit-Policy")
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
func endpoint(op string, h handlerFunc) http.Handler {
	return serve(func(a *API, w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sheet, tab := pathValue(r, "sheet"), pathValue(r, "tab")
		key := a.settings.KeyFor(bearer(r))
		status, body, err := func() (int, any, error) {
			if key == nil {
				return 0, nil, &service.APIError{Status: http.StatusUnauthorized, Msg: "missing or invalid API key"}
			}
			r = r.WithContext(store.WithCaller(r.Context(), key.Hash))
			defer func() { setRoom(w, a.svc.Room(r.Context())) }() // after h's own Google calls
			if _, ok := a.settings.Rule(sheet, tab); !ok {
				return 0, nil, &service.APIError{Status: http.StatusNotFound, Msg: "unknown tab"}
			}
			if !a.settings.Can(key, sheet, tab, op) {
				return 0, nil, &service.APIError{Status: http.StatusForbidden, Msg: op + " not allowed on " + sheet + "/" + tab}
			}
			return h(a, r, sheet, tab)
		}()
		reason := ""
		if err != nil {
			status, body, reason = errorResponse(err)
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", retryAfter(err))
		}
		if p, ok := body.(page); ok {
			w.Header().Set("X-Total-Count", strconv.Itoa(p.total))
			body = p.rows
		}
		writeJSON(w, status, body)
		keyName := ""
		if key != nil {
			keyName = key.Name
		}
		slog.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "op", op, "key", keyName,
			"status", status, "reason", reason, "duration_ms", time.Since(start).Milliseconds())
	})
}

// errorResponse hides upstream (Google) error details from callers; they are only logged.
// Running out of Google quota, whether the server's limiter or Google says so, is 429 so clients
// know to back off. Google refusing the owner's sign-in is 503 until they reconnect.
func errorResponse(err error) (status int, body any, reason string) {
	var ae *service.APIError
	if errors.As(err, &ae) {
		return ae.Status, map[string]string{"error": ae.Msg}, ae.Msg
	}
	if rl, ok := errors.AsType[*store.RateLimitError](err); ok {
		return http.StatusTooManyRequests, map[string]string{"error": rl.Error()}, rl.Error()
	}
	if store.AccessLost(err) {
		return http.StatusServiceUnavailable, map[string]string{"error": "the key's owner must reconnect their Google account in the sheetserve dashboard"}, err.Error()
	}
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok && gerr.Code == http.StatusTooManyRequests {
		return http.StatusTooManyRequests, map[string]string{"error": "Google Sheets rate limit reached; retry in a minute"}, err.Error()
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
	rows, total, err := a.svc.List(r.Context(), sheet, tab, filter, sort, limit, offset)
	return http.StatusOK, page{rows, total}, err
}

// page is a list's rows, with how many rows matched before limit and offset, which the response
// sends as X-Total-Count.
type page struct {
	rows  []map[string]any
	total int
}

func (a *API) get(r *http.Request, sheet, tab string) (int, any, error) {
	row, err := a.svc.Get(r.Context(), sheet, tab, pathValue(r, "id"))
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
	row, err := a.svc.Update(r.Context(), sheet, tab, pathValue(r, "id"), fields)
	return http.StatusOK, row, err
}

func (a *API) delete(r *http.Request, sheet, tab string) (int, any, error) {
	id := pathValue(r, "id")
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
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "err", err)
	}
}

// setRoom sends the caller's Google quota as RateLimit-Policy and RateLimit headers, as the IETF
// draft has them: one "read" and one "write" item, each with q calls in a window of w seconds,
// r of them left, and t seconds until it's back to q.
func setRoom(w http.ResponseWriter, rooms []store.Room) {
	var policy, room []string
	for _, rm := range rooms {
		policy = append(policy, fmt.Sprintf("%q;q=%d;w=60", rm.Kind, rm.Limit))
		room = append(room, fmt.Sprintf("%q;r=%d;t=%d", rm.Kind, rm.Remaining, int(rm.Reset.Seconds())))
	}
	if len(rooms) > 0 {
		w.Header().Set("RateLimit-Policy", strings.Join(policy, ", "))
		w.Header().Set("RateLimit", strings.Join(room, ", "))
	}
}

// retryAfter returns the Retry-After seconds for a 429 caused by err: when the limiter has room
// again, rounded up, or a minute, the length of Google's quota window.
func retryAfter(err error) string {
	if rl, ok := errors.AsType[*store.RateLimitError](err); ok {
		return strconv.Itoa(rl.Seconds())
	}
	return "60"
}
