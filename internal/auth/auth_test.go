package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDisabledByDefault(t *testing.T) {
	a, err := New(context.Background(), Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Enabled() {
		t.Fatal("without IssuerURL, auth must be disabled")
	}

	called := false
	protected := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/power/current", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if !called {
		t.Fatal("with auth disabled, Protect should be a no-op and call the handler")
	}
}

func TestRequiresSecretKeyWhenEnabled(t *testing.T) {
	_, err := New(context.Background(), Config{IssuerURL: "https://example.invalid"})
	if err == nil {
		t.Fatal("expected an error without SESSION_SECRET_KEY when OIDC is configured")
	}
}

func TestSignAndVerifyRoundtrip(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	token := a.sign([]byte(`{"sub":"u1"}`))

	payload, ok := a.verify(token)
	if !ok {
		t.Fatal("verify rejected a correctly signed token")
	}
	if string(payload) != `{"sub":"u1"}` {
		t.Errorf("payload = %s", payload)
	}
}

func TestVerifyRejectsTamperedToken(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	token := a.sign([]byte(`{"sub":"u1"}`))

	// Tamper a character in the middle of the payload, not the token's last
	// character: a base64 block's last symbol can have unused padding bits,
	// so a change there isn't guaranteed to change the decoded bytes.
	mid := len(token) / 2
	other := byte('a')
	if token[mid] == 'a' {
		other = 'b'
	}
	tampered := token[:mid] + string(other) + token[mid+1:]

	if _, ok := a.verify(tampered); ok {
		t.Fatal("verify accepted a tampered token")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	a1 := &Auth{enabled: true, secretKey: []byte("key-one"), maxAge: time.Hour}
	a2 := &Auth{enabled: true, secretKey: []byte("key-two"), maxAge: time.Hour}

	token := a1.sign([]byte(`{"sub":"u1"}`))
	if _, ok := a2.verify(token); ok {
		t.Fatal("verify accepted a token signed with a different key")
	}
}

func TestProtectRedirectsToLoginWithoutSession(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	protected := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the handler must not be called without a session")
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect to /login)", rec.Code, http.StatusFound)
	}
}

func TestProtectReturns401JSONForAPIWithoutSession(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	protected := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the handler must not be called without a session")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/power/current", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an /api/* route", rec.Code)
	}
}

func TestProtectAllowsValidSessionAndSlidesExpiry(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	called := false
	protected := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/power/current", nil)
	rec0 := httptest.NewRecorder()
	a.setSessionCookie(rec0, sessionClaims{Sub: "u1", Exp: time.Now().Add(time.Hour).Unix()})
	req.AddCookie(rec0.Result().Cookies()[0])

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if !called {
		t.Fatal("with a valid session, the handler should be called")
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("expected a renewed session cookie in the response")
	}
}

func TestProtectAllowsLoginAndCallbackWithoutSession(t *testing.T) {
	a := &Auth{enabled: true, secretKey: []byte("test-secret"), maxAge: time.Hour}
	for _, path := range []string{"/login", "/callback"} {
		called := false
		protected := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if !called {
			t.Errorf("%s should stay reachable without a session", path)
		}
	}
}
