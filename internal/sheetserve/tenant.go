package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// multiTenant serves hosted mode: users sign in with Google, and each user's API keys reach an
// *API built from that user's rows in the database, calling Google with that user's credentials.
type multiTenant struct {
	global      *Config // listen, cache_ttl and cors_origins; spreadsheets and keys come from db
	db          *DB
	oauth       *oauth2.Config
	userinfoURL string
	secure      bool   // set Secure on cookies (base URL is https)
	pickerKey   string // browser API key for Google Picker
	appID       string // Cloud project number, so picked files are granted to this app
	newStore    func(userID int64) (Store, error)

	mu      sync.Mutex
	tenants map[int64]*tenant
	locks   map[int64]*sync.Map // per-user tab write locks; outlive tenant rebuilds
}

type tenant struct {
	h   http.Handler
	svc *Service
}

func newMultiTenant(global *Config, db *DB, oauth *oauth2.Config) *multiTenant {
	m := &multiTenant{global: global, db: db, oauth: oauth, tenants: map[int64]*tenant{}, locks: map[int64]*sync.Map{},
		userinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		secure:      strings.HasPrefix(oauth.RedirectURL, "https://")}
	m.newStore = func(userID int64) (Store, error) {
		ts, err := m.tokenSource(userID)
		if err != nil {
			return nil, err
		}
		return newGoogleStore(context.Background(), option.WithTokenSource(ts))
	}
	return m
}

func (m *multiTenant) tokenSource(userID int64) (oauth2.TokenSource, error) {
	rt, err := m.db.refreshToken(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	return m.oauth.TokenSource(context.Background(), &oauth2.Token{RefreshToken: rt}), nil
}

func (m *multiTenant) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", m.login)
	mux.HandleFunc("GET /auth/callback", m.callback)
	api := http.HandlerFunc(m.serveAPI)
	mux.Handle("/v1/", api)
	mux.Handle("/mcp", api)
	mux.Handle("GET /openapi.json", api)
	mux.Handle("/", http.NewCrossOriginProtection().Handler(m.web()))
	return (&API{cfg: m.global}).cors(mux)
}

// serveAPI hands the request to the API of the user who owns the bearer key.
func (m *multiTenant) serveAPI(w http.ResponseWriter, r *http.Request) {
	userID, ok, err := m.db.keyUser(r.Context(), hashKey(bearer(r)))
	var t *tenant
	if err == nil && ok {
		t, err = m.tenant(r.Context(), userID)
	}
	switch {
	case err != nil:
		slog.Error("tenant", "user", userID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream error"})
	case !ok:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"})
	default:
		t.h.ServeHTTP(w, r)
	}
}

// ponytail: tenants are cached in-process; a second replica needs shared invalidation.
func (m *multiTenant) tenant(ctx context.Context, userID int64) (*tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tenants[userID]; ok {
		return t, nil
	}
	var cfg *Config
	err := m.db.snapshot(ctx, func(tx *sql.Tx) (err error) {
		cfg, err = userConfig(ctx, tx, userID, m.global)
		return err
	})
	if err != nil {
		return nil, err
	}
	store, err := m.newStore(userID)
	if err != nil {
		return nil, err
	}
	svc := newService(cfg, store)
	// Requests still running on a replaced tenant hold its tab locks, so every rebuild shares them.
	if m.locks[userID] == nil {
		m.locks[userID] = &sync.Map{}
	}
	svc.locks = m.locks[userID]
	t := &tenant{(&API{cfg: cfg, svc: svc}).mux(), svc}
	m.tenants[userID] = t
	return t, nil
}

// invalidate drops userID's cached API so the next request sees their latest settings and token.
func (m *multiTenant) invalidate(userID int64) {
	m.mu.Lock()
	delete(m.tenants, userID)
	m.mu.Unlock()
}

func (m *multiTenant) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode}
}

func (m *multiTenant) login(w http.ResponseWriter, r *http.Request) {
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	http.SetCookie(w, m.cookie("oauth_state", state, 600))
	http.SetCookie(w, m.cookie("oauth_verifier", verifier, 600))
	// prompt=consent makes Google return a refresh token on every sign-in, not just the first.
	http.Redirect(w, r, m.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent")), http.StatusFound)
}

func (m *multiTenant) callback(w http.ResponseWriter, r *http.Request) {
	state, err1 := r.Cookie("oauth_state")
	verifier, err2 := r.Cookie("oauth_verifier")
	http.SetCookie(w, m.cookie("oauth_state", "", -1))
	http.SetCookie(w, m.cookie("oauth_verifier", "", -1))
	if err1 != nil || err2 != nil || r.FormValue("state") != state.Value {
		http.Error(w, "invalid sign-in state; try again", http.StatusBadRequest)
		return
	}
	if e := r.FormValue("error"); e != "" {
		http.Error(w, "sign-in failed: "+e, http.StatusBadRequest)
		return
	}
	userID, err := m.signIn(r.Context(), r.FormValue("code"), verifier.Value)
	if err != nil {
		slog.Error("sign-in", "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	sess, err := m.db.newSession(r.Context(), userID)
	if err != nil {
		slog.Error("session", "err", err)
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	m.invalidate(userID) // pick up the new refresh token
	http.SetCookie(w, m.cookie("session", sess, int(sessionTTL.Seconds())))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signIn exchanges the code, identifies the Google account, and stores its refresh token.
func (m *multiTenant) signIn(ctx context.Context, code, verifier string) (int64, error) {
	tok, err := m.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return 0, err
	}
	resp, err := m.oauth.Client(ctx, tok).Get(m.userinfoURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var info struct{ Sub, Email string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != http.StatusOK || info.Sub == "" {
		return 0, fmt.Errorf("userinfo: status %d, err %v", resp.StatusCode, err)
	}
	return m.db.upsertUser(ctx, info.Sub, info.Email, tok.RefreshToken)
}

// currentUser returns the signed-in user, or ok=false.
func (m *multiTenant) currentUser(r *http.Request) (id int64, email string, ok bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return 0, "", false
	}
	id, email, ok, err = m.db.sessionUser(r.Context(), c.Value)
	if err != nil {
		slog.Error("session", "err", err)
	}
	return id, email, ok
}

func (m *multiTenant) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		if err := m.db.deleteSession(r.Context(), c.Value); err != nil {
			slog.Error("logout", "err", err)
		}
	}
	http.SetCookie(w, m.cookie("session", "", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
