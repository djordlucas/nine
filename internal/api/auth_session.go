package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nine/internal/api/apigen"
)

// Browser sessions (spec/contracts/api.md API-HTTP-2a).
//
// A browser cannot put an Authorization header on a WebSocket, so the web UI
// exchanges the API token once for a cookie. The cookie holds a random session
// id, never the token: a leaked cookie dies with its session, and the token
// stays where the operator put it. Sessions live in this process's memory, so a
// restart of the API server signs every browser out.

const (
	// sessionCookieName is the cookie POST /auth/session sets.
	sessionCookieName = "nine_session"

	// csrfHeader carries a cookie session's CSRF token on writes. A cross-site
	// page can make a browser send the cookie only if SameSite fails it; it can
	// never read the token, so a write carrying it came from the UI.
	csrfHeader = "X-Nine-CSRF"

	// sessionIdle is how long an unused session lives.
	sessionIdle = 7 * 24 * time.Hour

	// maxSessions caps the store: past it the least recently used session goes.
	// Logins are rate limited, so this bounds memory rather than a real workload.
	maxSessions = 1000

	authSessionPath = apiBasePath + "/auth/session"
)

type browserSession struct {
	csrf string
	seen time.Time
}

// sessionStore holds the signed-in browser sessions.
type sessionStore struct {
	mu  sync.Mutex
	m   map[string]*browserSession
	now func() time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: make(map[string]*browserSession), now: time.Now}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// create starts a session and returns its id and CSRF token.
func (st *sessionStore) create() (id, csrf string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.m) >= maxSessions {
		var oldest string
		var at time.Time
		for k, v := range st.m {
			if oldest == "" || v.seen.Before(at) {
				oldest, at = k, v.seen
			}
		}
		delete(st.m, oldest)
	}
	id, csrf = randomHex(32), randomHex(32)
	st.m[id] = &browserSession{csrf: csrf, seen: st.now()}
	return id, csrf
}

// lookup returns a live session and marks it used. An expired one is removed.
func (st *sessionStore) lookup(id string) (*browserSession, bool) {
	if id == "" {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.m[id]
	if !ok {
		return nil, false
	}
	if st.now().Sub(s.seen) > sessionIdle {
		delete(st.m, id)
		return nil, false
	}
	s.seen = st.now()
	return s, true
}

func (st *sessionStore) remove(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.m, id)
}

// sessionFromRequest returns the request's cookie session, if it has a live one.
func (s *Server) sessionFromRequest(r *http.Request) (*browserSession, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, false
	}
	return s.sessions.lookup(c.Value)
}

// validBearer reports whether the request carries the configured token.
func (s *Server) validBearer(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.config.AuthToken)) == 1
}

// sameOrigin reports whether a browser request came from a page this server
// served. Browsers always send Origin on a WebSocket handshake and on a
// cross-origin write, so a cookie request without one is refused too.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// safeMethod reports a method that changes nothing, which needs no CSRF token.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// isWebSocketUpgrade reports a WebSocket handshake. It is a GET, but it opens a
// channel that sends turns, so it is checked as a write.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// checkCookieRequest applies the rules a cookie-authenticated request must
// meet beyond carrying a live session: a write or a WebSocket handshake comes
// from this server's own origin, and a write carries the session's CSRF token.
// It returns the reason for a refusal, or "".
func checkCookieRequest(r *http.Request, sess *browserSession) string {
	upgrade := isWebSocketUpgrade(r)
	if safeMethod(r.Method) && !upgrade {
		return ""
	}
	if !sameOrigin(r) {
		return "a cookie-authenticated write must come from this server's origin"
	}
	if upgrade {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(sess.csrf)) != 1 {
		return "missing or invalid " + csrfHeader + " header"
	}
	return ""
}

// secureRequest reports whether the browser reached us over HTTPS, directly or
// through a trusted proxy, which decides the cookie's Secure flag.
func (s *Server) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer, ok := peerAddr(r)
	return ok && s.trustsProxy(peer) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) string {
	c := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.secureRequest(r),
	}
	return c.String()
}

// requestKey carries the *http.Request into the strict handlers that need it:
// the session operations read and set cookies, which the typed request objects
// do not expose.
type requestKey struct{}

func withHTTPRequest(f apigen.StrictHandlerFunc, _ string) apigen.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		return f(context.WithValue(ctx, requestKey{}, r), w, r, req)
	}
}

func httpRequest(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

// GetAuthSession reports whether a token is required and whether this request
// is signed in.
func (s *Server) GetAuthSession(ctx context.Context, _ apigen.GetAuthSessionRequestObject) (apigen.GetAuthSessionResponseObject, error) {
	r := httpRequest(ctx)
	resp := apigen.GetAuthSession200JSONResponse{AuthRequired: s.config.AuthToken != ""}
	if !resp.AuthRequired {
		resp.Authenticated = true
		return resp, nil
	}
	if sess, ok := s.sessionFromRequest(r); ok {
		resp.Authenticated = true
		resp.CsrfToken = &sess.csrf
		return resp, nil
	}
	resp.Authenticated = s.validBearer(r)
	return resp, nil
}

// CreateAuthSession exchanges the API token for a session cookie.
func (s *Server) CreateAuthSession(ctx context.Context, req apigen.CreateAuthSessionRequestObject) (apigen.CreateAuthSessionResponseObject, error) {
	if s.config.AuthToken == "" {
		return apigen.CreateAuthSession200JSONResponse{
			Body: apigen.AuthSessionResponse{AuthRequired: false, Authenticated: true},
		}, nil
	}
	if req.Body == nil || subtle.ConstantTimeCompare([]byte(req.Body.Token), []byte(s.config.AuthToken)) != 1 {
		return apigen.CreateAuthSession401JSONResponse{UnauthorizedJSONResponse: apigen.UnauthorizedJSONResponse(
			errorBody("unauthorized", "invalid token", nil))}, nil
	}
	id, csrf := s.sessions.create()
	cookie := s.sessionCookie(httpRequest(ctx), id, int(sessionIdle.Seconds()))
	return apigen.CreateAuthSession200JSONResponse{
		Body:    apigen.AuthSessionResponse{AuthRequired: true, Authenticated: true, CsrfToken: &csrf},
		Headers: apigen.CreateAuthSession200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// DeleteAuthSession ends the request's session, if any, and expires its cookie.
func (s *Server) DeleteAuthSession(ctx context.Context, _ apigen.DeleteAuthSessionRequestObject) (apigen.DeleteAuthSessionResponseObject, error) {
	r := httpRequest(ctx)
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.remove(c.Value)
	}
	cookie := s.sessionCookie(r, "", -1)
	return apigen.DeleteAuthSession204Response{
		Headers: apigen.DeleteAuthSession204ResponseHeaders{SetCookie: &cookie},
	}, nil
}
