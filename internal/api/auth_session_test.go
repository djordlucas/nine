package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
)

func sessionTestServer(token string) (*Server, http.Handler) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{AuthToken: token},
		Version:   "test",
		StartTime: time.Now(),
	})
	return s, s.createHandler()
}

type authBody struct {
	AuthRequired  bool    `json:"auth_required"`
	Authenticated bool    `json:"authenticated"`
	CSRFToken     *string `json:"csrf_token"`
}

func sendAuth(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, authBody) {
	t.Helper()
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var b authBody
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return w, b
}

// signIn logs in and returns the session cookie and CSRF token.
func signIn(t *testing.T, h http.Handler, token string) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest("POST", authSessionPath, strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w, b := sendAuth(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sign in: status %d, body %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("sign in set cookies %v, want one %s", cookies, sessionCookieName)
	}
	if b.CSRFToken == nil || *b.CSRFToken == "" {
		t.Fatal("sign in returned no CSRF token")
	}
	return cookies[0], *b.CSRFToken
}

func TestAuthSession_SignInSetsAHardenedCookie(t *testing.T) {
	_, h := sessionTestServer("secret")
	c, _ := signIn(t, h, "secret")
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, Path=/", c)
	}
	if c.Secure {
		t.Error("cookie is Secure on a plain-HTTP request; the browser would drop it")
	}
	if strings.Contains(c.Value, "secret") {
		t.Error("cookie carries the API token")
	}
}

func TestAuthSession_WrongTokenIsRefused(t *testing.T) {
	_, h := sessionTestServer("secret")
	req := httptest.NewRequest("POST", authSessionPath, strings.NewReader(`{"token":"guess"}`))
	req.Header.Set("Content-Type", "application/json")
	w, _ := sendAuth(t, h, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a refused sign-in set a cookie")
	}
}

func TestAuthSession_GetReportsState(t *testing.T) {
	_, h := sessionTestServer("secret")

	_, b := sendAuth(t, h, httptest.NewRequest("GET", authSessionPath, nil))
	if !b.AuthRequired || b.Authenticated || b.CSRFToken != nil {
		t.Errorf("anonymous: %+v, want required, not authenticated, no CSRF token", b)
	}

	c, csrf := signIn(t, h, "secret")
	req := httptest.NewRequest("GET", authSessionPath, nil)
	req.AddCookie(c)
	_, b = sendAuth(t, h, req)
	if !b.Authenticated || b.CSRFToken == nil || *b.CSRFToken != csrf {
		t.Errorf("signed in: %+v, want authenticated with the session's CSRF token", b)
	}

	req = httptest.NewRequest("GET", authSessionPath, nil)
	req.Header.Set("Authorization", "Bearer secret")
	_, b = sendAuth(t, h, req)
	if !b.Authenticated || b.CSRFToken != nil {
		t.Errorf("bearer: %+v, want authenticated, no CSRF token", b)
	}
}

func TestAuthSession_NoTokenConfigured(t *testing.T) {
	_, h := sessionTestServer("")
	_, b := sendAuth(t, h, httptest.NewRequest("GET", authSessionPath, nil))
	if b.AuthRequired || !b.Authenticated {
		t.Errorf("%+v, want not required and authenticated", b)
	}
	req := httptest.NewRequest("POST", authSessionPath, strings.NewReader(`{"token":"anything"}`))
	req.Header.Set("Content-Type", "application/json")
	w, _ := sendAuth(t, h, req)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Errorf("sign in with no token configured: status %d, cookies %v; want 200 and none", w.Code, w.Result().Cookies())
	}
}

func TestAuthSession_CookieAuthenticatesReads(t *testing.T) {
	_, h := sessionTestServer("secret")
	c, _ := signIn(t, h, "secret")

	req := httptest.NewRequest("GET", "/api/v1/docs", nil)
	req.AddCookie(c)
	if w, _ := sendAuth(t, h, req); w.Code != http.StatusOK {
		t.Errorf("cookie read: status %d, want 200", w.Code)
	}

	req = httptest.NewRequest("GET", "/api/v1/docs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "forged"})
	if w, _ := sendAuth(t, h, req); w.Code != http.StatusUnauthorized {
		t.Errorf("unknown session: status %d, want 401", w.Code)
	}
}

// A cookie write passes only from this origin and with the CSRF token. The
// handler behind it needs a daemon, so "passed auth" is any status but 401/403.
func TestAuthSession_CookieWritesNeedOriginAndCSRF(t *testing.T) {
	_, h := sessionTestServer("secret")
	c, csrf := signIn(t, h, "secret")

	cases := []struct {
		name, origin, csrf string
		want               int // 0: anything but 401/403
	}{
		{"no origin", "", csrf, http.StatusForbidden},
		{"other origin", "https://evil.example", csrf, http.StatusForbidden},
		{"no csrf", "http://example.com", "", http.StatusForbidden},
		{"wrong csrf", "http://example.com", "nope", http.StatusForbidden},
		{"same origin with csrf", "http://example.com", csrf, 0},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/api/v1/plugins/reload", nil)
		req.Host = "example.com"
		req.AddCookie(c)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.csrf != "" {
			req.Header.Set(csrfHeader, tc.csrf)
		}
		w, _ := sendAuth(t, h, req)
		switch {
		case tc.want == 0 && (w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden):
			t.Errorf("%s: status %d, want it past auth", tc.name, w.Code)
		case tc.want != 0 && w.Code != tc.want:
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

// A WebSocket handshake is a GET that opens a channel for turns: with a cookie
// it must come from this origin.
func TestAuthSession_CookieWebSocketNeedsOrigin(t *testing.T) {
	_, h := sessionTestServer("secret")
	c, _ := signIn(t, h, "secret")
	req := httptest.NewRequest("GET", "/ws/v1/conversations/abc/messages", nil)
	req.Host = "example.com"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(c)
	if w, _ := sendAuth(t, h, req); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin cookie handshake: status %d, want 403", w.Code)
	}
}

func TestAuthSession_SignOutEndsTheSession(t *testing.T) {
	_, h := sessionTestServer("secret")
	c, _ := signIn(t, h, "secret")

	req := httptest.NewRequest("DELETE", authSessionPath, nil)
	req.AddCookie(c)
	w, _ := sendAuth(t, h, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign out: status %d, want 204", w.Code)
	}
	if cs := w.Result().Cookies(); len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Errorf("sign out cookies %v, want one expired cookie", cs)
	}

	req = httptest.NewRequest("GET", "/api/v1/docs", nil)
	req.AddCookie(c)
	if w, _ := sendAuth(t, h, req); w.Code != http.StatusUnauthorized {
		t.Errorf("after sign out: status %d, want 401", w.Code)
	}
}

func TestSessionStore_ExpiresIdleAndCaps(t *testing.T) {
	st := newSessionStore()
	now := time.Now()
	st.now = func() time.Time { return now }

	id, _ := st.create()
	now = now.Add(sessionIdle + time.Minute)
	if _, ok := st.lookup(id); ok {
		t.Error("an idle session outlived sessionIdle")
	}

	first, _ := st.create()
	for range maxSessions {
		now = now.Add(time.Second)
		st.create()
	}
	if _, ok := st.lookup(first); ok {
		t.Error("the least recently used session survived past maxSessions")
	}
	if len(st.m) > maxSessions {
		t.Errorf("store holds %d sessions, cap %d", len(st.m), maxSessions)
	}
}

func TestSecureCookieBehindTrustedProxy(t *testing.T) {
	s := serverWithTrustedProxies(t, "10.0.0.0/8")
	req := httptest.NewRequest("POST", authSessionPath, nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-Proto", "https")
	if !s.secureRequest(req) {
		t.Error("HTTPS through a trusted proxy is not treated as secure")
	}
	req.RemoteAddr = "203.0.113.9:5555"
	if s.secureRequest(req) {
		t.Error("X-Forwarded-Proto from an untrusted peer was believed")
	}
}

func TestUIServedBesideTheAPI(t *testing.T) {
	_, h := sessionTestServer("secret")

	w, _ := sendAuth(t, h, httptest.NewRequest("GET", "/processes", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("UI route: status %d type %q, want the app without auth", w.Code, w.Header().Get("Content-Type"))
	}
	if w, _ := sendAuth(t, h, httptest.NewRequest("GET", "/api/v1/docs", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("API path: status %d, want 401 — the UI must not open the API", w.Code)
	}

	off := false
	s := NewServer(Config{APIConfig: config.APIConfig{UI: &off}, Version: "test", StartTime: time.Now()})
	if w, _ := sendAuth(t, s.createHandler(), httptest.NewRequest("GET", "/processes", nil)); w.Code != http.StatusNotFound {
		t.Errorf("ui = false: status %d, want 404", w.Code)
	}
}
