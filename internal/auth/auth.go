// Package auth implements OIDC login (Authelia) integrated into the app,
// the same pattern as hp-bios-webui: off by default (no breakage for those
// already running a forward-auth), on by setting OIDC_ISSUER_URL. When on,
// every route requires a valid session, no exception for read-only ones.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config are the OIDC parameters, read from the environment by the caller.
type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	SecretKey     string        // SESSION_SECRET_KEY, signs the session and OAuth state cookies
	SessionMaxAge time.Duration // default 1h if zero
}

// Auth is the middleware. If not Enabled(), Protect is a no-op: identical
// behavior to before auth was added (for those already using a forward-auth).
type Auth struct {
	enabled      bool
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	secretKey    []byte
	maxAge       time.Duration
}

func New(ctx context.Context, cfg Config) (*Auth, error) {
	if cfg.IssuerURL == "" {
		return &Auth{enabled: false}, nil
	}
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("SESSION_SECRET_KEY is required when OIDC_ISSUER_URL is set")
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discovering OIDC provider (%s): %w", cfg.IssuerURL, err)
	}

	maxAge := cfg.SessionMaxAge
	if maxAge == 0 {
		maxAge = time.Hour
	}

	return &Auth{
		enabled: true,
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier:  provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		secretKey: []byte(cfg.SecretKey),
		maxAge:    maxAge,
	}, nil
}

func (a *Auth) Enabled() bool { return a.enabled }

// --- session cookie: signed/httponly/Secure, stateless (HMAC, no
// server-side store), sliding expiry renewed on every valid request.

const sessionCookieName = "wattson_session"

type sessionClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
}

func (a *Auth) sign(payload []byte) string {
	mac := hmac.New(sha256.New, a.secretKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) verify(token string) ([]byte, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, a.secretKey)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}

func (a *Auth) setSessionCookie(w http.ResponseWriter, claims sessionClaims) {
	data, _ := json.Marshal(claims)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: a.sign(data), Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(claims.Exp, 0),
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}

func (a *Auth) sessionFromRequest(r *http.Request) (sessionClaims, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return sessionClaims{}, false
	}
	payload, ok := a.verify(c.Value)
	if !ok {
		return sessionClaims{}, false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil || time.Now().Unix() > claims.Exp {
		return sessionClaims{}, false
	}
	return claims, true
}

// --- login/callback: authorization code + PKCE, state/verifier in a
// short-lived (10 minute) signed cookie, no server-side store.

const stateCookieName = "wattson_oauth_state"

type stateClaims struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Redirect string `json:"redirect"`
	Exp      int64  `json:"exp"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *Auth) LoginHandler(w http.ResponseWriter, r *http.Request) {
	verifier := oauth2.GenerateVerifier()
	state, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirect := r.URL.Query().Get("redirect")
	if redirect == "" || !strings.HasPrefix(redirect, "/") {
		redirect = "/"
	}

	data, _ := json.Marshal(stateClaims{State: state, Verifier: verifier, Redirect: redirect, Exp: time.Now().Add(10 * time.Minute).Unix()})
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: a.sign(data), Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})

	http.Redirect(w, r, a.oauth2Config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *Auth) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing or expired state, please log in again", http.StatusBadRequest)
		return
	}
	payload, ok := a.verify(c.Value)
	if !ok {
		http.Error(w, "invalid state, please log in again", http.StatusBadRequest)
		return
	}
	var sc stateClaims
	if err := json.Unmarshal(payload, &sc); err != nil || time.Now().Unix() > sc.Exp {
		http.Error(w, "expired state, please log in again", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, stateCookieName)

	if r.URL.Query().Get("state") != sc.State {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	token, err := a.oauth2Config.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(sc.Verifier))
	if err != nil {
		log.Println("auth: code exchange failed:", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "provider response has no id_token", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		log.Println("auth: id_token verification failed:", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email string `json:"email"`
	}
	_ = idToken.Claims(&claims)

	a.setSessionCookie(w, sessionClaims{Sub: idToken.Subject, Email: claims.Email, Exp: time.Now().Add(a.maxAge).Unix()})
	http.Redirect(w, r, sc.Redirect, http.StatusFound)
}

func (a *Auth) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Protect requires a valid session on every route (frontend and /api/*,
// including read-only ones), no exceptions. /login and /callback stay
// reachable so login can complete. A no-op if auth is off.
func (a *Auth) Protect(next http.Handler) http.Handler {
	if !a.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || r.URL.Path == "/callback" {
			next.ServeHTTP(w, r)
			return
		}

		claims, ok := a.sessionFromRequest(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			http.Redirect(w, r, "/login?redirect="+url.QueryEscape(r.URL.Path), http.StatusFound)
			return
		}

		// sliding idle timeout: renew the expiry on every valid request
		a.setSessionCookie(w, sessionClaims{Sub: claims.Sub, Email: claims.Email, Exp: time.Now().Add(a.maxAge).Unix()})
		next.ServeHTTP(w, r)
	})
}
