package hosted

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
)

// multiTenant serves the app: users sign in with Google and manage their spreadsheets and keys on
// the dashboard, and tenants serves each user's API to their keys.
type multiTenant struct {
	global      *config.Config // listen, cache_ttl and cors_origins; spreadsheets and keys come from db
	db          *db.DB
	oauth       *oauth2.Config
	userinfoURL string
	secure      bool   // set Secure on cookies (base URL is https)
	pickerKey   string // browser API key for Google Picker
	appID       string // Cloud project number, so picked files are granted to this app
	tenants     *tenancy.Tenants
	saEmail     string // the address users share spreadsheets with for tenants.SAStore
}

func newMultiTenant(global *config.Config, d *db.DB, oauth *oauth2.Config) *multiTenant {
	m := &multiTenant{
		global: global, db: d, oauth: oauth, tenants: &tenancy.Tenants{DB: d, Global: global},
		userinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		secure:      strings.HasPrefix(oauth.RedirectURL, "https://"),
	}
	m.tenants.NewStore = func(userID int64) (store.Store, error) {
		ts, err := m.tokenSource(userID)
		if err != nil {
			return nil, err
		}
		return store.NewGoogleStore(context.Background(), option.WithTokenSource(ts))
	}
	return m
}

func (m *multiTenant) tokenSource(userID int64) (oauth2.TokenSource, error) {
	rt, err := m.db.RefreshToken(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	return m.oauth.TokenSource(context.Background(), &oauth2.Token{RefreshToken: rt}), nil
}

func (m *multiTenant) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", m.login)
	mux.HandleFunc("GET /auth/callback", m.callback)
	mux.Handle("/v1/", m.tenants)
	mux.Handle("/mcp", m.tenants)
	mux.Handle("GET /openapi.json", m.tenants)
	mux.Handle("/", http.NewCrossOriginProtection().Handler(m.web()))
	return api.CORS(m.global, mux)
}

func (m *multiTenant) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	}
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
	sess, err := m.db.NewSession(r.Context(), userID)
	if err != nil {
		slog.Error("session", "err", err)
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	m.tenants.Invalidate(userID) // pick up the new refresh token
	http.SetCookie(w, m.cookie("session", sess, int(db.SessionTTL.Seconds())))
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
	defer func() { _ = resp.Body.Close() }()
	var info struct{ Sub, Email string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != http.StatusOK || info.Sub == "" {
		return 0, fmt.Errorf("userinfo: status %d, err %v", resp.StatusCode, err)
	}
	return m.db.UpsertUser(ctx, info.Sub, info.Email, tok.RefreshToken)
}

// currentUser returns the signed-in user, or ok=false.
func (m *multiTenant) currentUser(r *http.Request) (id int64, email string, ok bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return 0, "", false
	}
	id, email, ok, err = m.db.SessionUser(r.Context(), c.Value)
	if err != nil {
		slog.Error("session", "err", err)
	}
	return id, email, ok
}

func (m *multiTenant) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		if err := m.db.DeleteSession(r.Context(), c.Value); err != nil {
			slog.Error("logout", "err", err)
		}
	}
	http.SetCookie(w, m.cookie("session", "", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
