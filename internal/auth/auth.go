// Package auth signs users in with Google and keeps their sessions in the database.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// Google handles sign-in with Google, the session cookie, and each user's Google credentials.
type Google struct {
	DB          *db.DB
	OAuth       *oauth2.Config
	UserinfoURL string
	// OnSignIn runs after a sign-in stores a fresh refresh token, so cached clients can pick it up.
	OnSignIn func(userID int64)
}

// New returns a Google that signs in with oauth and stores users and sessions in d.
func New(d *db.DB, oauth *oauth2.Config) *Google {
	return &Google{DB: d, OAuth: oauth, UserinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		OnSignIn: func(int64) {}}
}

// TokenSource returns userID's Google credentials, from the refresh token they signed in with.
func (g *Google) TokenSource(userID int64) (oauth2.TokenSource, error) {
	rt, err := g.DB.RefreshToken(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	return g.OAuth.TokenSource(context.Background(), &oauth2.Token{RefreshToken: rt}), nil
}

func (g *Google) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: strings.HasPrefix(g.OAuth.RedirectURL, "https://"), SameSite: http.SameSiteLaxMode,
	}
}

// Login sends the browser to Google's consent screen.
func (g *Google) Login(w http.ResponseWriter, r *http.Request) {
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	http.SetCookie(w, g.cookie("oauth_state", state, 600))
	http.SetCookie(w, g.cookie("oauth_verifier", verifier, 600))
	// prompt=consent makes Google return a refresh token on every sign-in, not just the first.
	http.Redirect(w, r, g.OAuth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent")), http.StatusFound)
}

// Callback finishes sign-in when Google redirects back, then starts a session.
func (g *Google) Callback(w http.ResponseWriter, r *http.Request) {
	state, err1 := r.Cookie("oauth_state")
	verifier, err2 := r.Cookie("oauth_verifier")
	http.SetCookie(w, g.cookie("oauth_state", "", -1))
	http.SetCookie(w, g.cookie("oauth_verifier", "", -1))
	if err1 != nil || err2 != nil || r.FormValue("state") != state.Value {
		http.Error(w, "invalid sign-in state; try again", http.StatusBadRequest)
		return
	}
	if e := r.FormValue("error"); e != "" {
		http.Error(w, "sign-in failed: "+e, http.StatusBadRequest)
		return
	}
	userID, err := g.signIn(r.Context(), r.FormValue("code"), verifier.Value)
	if err != nil {
		slog.Error("sign-in", "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	sess, err := g.DB.NewSession(r.Context(), userID)
	if err != nil {
		slog.Error("session", "err", err)
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	g.OnSignIn(userID)
	http.SetCookie(w, g.cookie("session", sess, int(db.SessionTTL.Seconds())))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signIn exchanges the code, identifies the Google account, and stores its refresh token.
func (g *Google) signIn(ctx context.Context, code, verifier string) (int64, error) {
	tok, err := g.OAuth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return 0, err
	}
	resp, err := g.OAuth.Client(ctx, tok).Get(g.UserinfoURL)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct{ Sub, Email string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != http.StatusOK || info.Sub == "" {
		return 0, fmt.Errorf("userinfo: status %d, err %v", resp.StatusCode, err)
	}
	return g.DB.UpsertUser(ctx, info.Sub, info.Email, tok.RefreshToken)
}

// CurrentUser returns the signed-in user, or ok=false.
func (g *Google) CurrentUser(r *http.Request) (u db.User, ok bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return db.User{}, false
	}
	u, ok, err = g.DB.SessionUser(r.Context(), c.Value)
	if err != nil {
		slog.Error("session", "err", err)
	}
	return u, ok
}

// Logout ends the session and returns to the home page.
func (g *Google) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		if err := g.DB.DeleteSession(r.Context(), c.Value); err != nil {
			slog.Error("logout", "err", err)
		}
	}
	http.SetCookie(w, g.cookie("session", "", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
