package admin

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	cookieName  = "adsvc_session"
	sessionTTL  = 12 * time.Hour
	headerAdmin = "X-Adsvc-Admin"
	tokenLen    = 8 + 16 + sha256.Size
)

// loginDelay is what a wrong token costs; failed logins wait in line. A var for tests.
var loginDelay = time.Second

// sessions issues and checks session tokens: expiry (8 bytes) | token id (16) | MAC (32),
// base64url. The MAC is an HMAC-SHA256 under a secret made at start; the token id is a MAC
// of the user's token the session was opened with. So a restart ends every session, and
// taking a token out of the config ends the sessions opened with it. The token itself is
// never in the cookie.
type sessions struct{ secret []byte }

func newSessions() *sessions {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand does not fail
	return &sessions{secret: b}
}

func (s *sessions) mac(parts ...[]byte) []byte {
	m := hmac.New(sha256.New, s.secret)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func (s *sessions) tokenID(token string) []byte {
	return s.mac([]byte("token\x00"), []byte(token))[:16]
}

func (s *sessions) issue(token string, now time.Time) string {
	b := binary.BigEndian.AppendUint64(nil, uint64(now.Add(sessionTTL).Unix()))
	b = append(b, s.tokenID(token)...)
	b = append(b, s.mac([]byte("session\x00"), b)...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// viewer is the user a request comes from, and the token they logged in with (the
// Reports page writes it into the mpv script it hands out).
type viewer struct {
	Name  string
	Admin bool
	token string
}

type viewerKey struct{}

func viewerOf(r *http.Request) viewer {
	v, _ := r.Context().Value(viewerKey{}).(viewer)
	return v
}

// valid returns the user of tok, when it is an unexpired session opened with one of the
// users' tokens.
func (s *sessions) valid(tok string, users []User, now time.Time) (viewer, bool) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != tokenLen || !hmac.Equal(b[24:], s.mac([]byte("session\x00"), b[:24])) {
		return viewer{}, false
	}
	if now.Unix() >= int64(binary.BigEndian.Uint64(b)) {
		return viewer{}, false
	}
	var v viewer
	found := false
	for _, u := range users {
		for _, t := range u.Tokens {
			if hmac.Equal(b[8:24], s.tokenID(t)) && !found {
				v, found = viewer{Name: u.Name, Admin: u.Admin, token: t}, true
			}
		}
	}
	return v, found
}

// userOf returns the user a token belongs to. Every token is compared, in constant time.
func userOf(token string, users []User) (viewer, bool) {
	var v viewer
	found := false
	for _, u := range users {
		for _, t := range u.Tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 && !found {
				v, found = viewer{Name: u.Name, Admin: u.Admin, token: t}, true
			}
		}
	}
	return v, found && token != ""
}

// hasTokens reports whether any user can log in; without one the page is off.
func hasTokens(users []User) bool {
	for _, u := range users {
		if len(u.Tokens) > 0 {
			return true
		}
	}
	return false
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// guard lets in requests that carry a valid session, and only admins' when admin is set.
// Those that change something also need the X-Adsvc-Admin header, which a cross-site form
// cannot send, and must pass net/http's cross-origin protection (Sec-Fetch-Site, or
// Origin against Host).
func (a *Admin) guard(h http.HandlerFunc, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		users := a.d.Users()
		if !hasTokens(users) {
			a.disabled(w, r)
			return
		}
		c, err := r.Cookie(cookieName)
		var v viewer
		ok := err == nil
		if ok {
			v, ok = a.sess.valid(c.Value, users, time.Now())
		}
		if !ok {
			a.unauthorized(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), viewerKey{}, v))
		if admin && !v.Admin {
			if wantsJSON(r) || !safeMethod(r.Method) {
				a.fail(w, r, http.StatusForbidden, errors.New(a.translate(r, "error.admin_only")))
			} else {
				http.Redirect(w, r, home(v), http.StatusSeeOther)
			}
			return
		}
		if !safeMethod(r.Method) && r.Header.Get(headerAdmin) != "1" {
			a.fail(w, r, http.StatusForbidden, errors.New("missing header "+headerAdmin+": 1"))
			return
		}
		h(w, r)
	})
}

func (a *Admin) unauthorized(w http.ResponseWriter, r *http.Request) {
	switch {
	case wantsJSON(r):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "log in at /login"})
	case isHTMX(r):
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
	case safeMethod(r.Method):
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		http.Error(w, "log in at /login", http.StatusUnauthorized)
	}
}

func (a *Admin) disabled(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "disabled")
	p.Err = p.T("disabled.text")
	a.show(w, r, p, "", http.StatusNotFound)
}

func (a *Admin) loginForm(w http.ResponseWriter, r *http.Request) {
	if !hasTokens(a.d.Users()) {
		a.disabled(w, r)
		return
	}
	p := a.newPage(r, "login")
	p.Next = safeNext(r.URL.Query().Get("next"))
	a.show(w, r, p, "", http.StatusOK)
}

// login trades a user's token for a session cookie. A wrong token costs loginDelay, one
// attempt at a time.
func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	users := a.d.Users()
	if !hasTokens(users) {
		a.disabled(w, r)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	v, ok := userOf(token, users)
	if !ok {
		next := safeNext(r.PostFormValue("next"))
		a.loginMu.Lock()
		select {
		case <-time.After(loginDelay):
		case <-r.Context().Done():
		}
		a.loginMu.Unlock()
		p := a.newPage(r, "login")
		p.Next, p.Err = next, p.T("login.wrong")
		a.show(w, r, p, "", http.StatusUnauthorized)
		return
	}
	next := cmp.Or(safeNext(r.PostFormValue("next")), home(v))
	http.SetCookie(w, sessionCookie(r, a.sess.issue(token, time.Now()), int(sessionTTL/time.Second)))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookie(r, "", -1))
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// sessionCookie is the session cookie: HttpOnly, SameSite=Strict. It is
// Secure when the request came over TLS; adsvc mostly listens on plain HTTP on a LAN or
// loopback (TV boxes), where a Secure cookie would never be sent back.
func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, //nolint:gosec // G124: Secure follows the transport, see above
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode}
}

// safeNext is where to go after logging in: a page of this site, never another one; ""
// when s is none (the user's home then).
func safeNext(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") || u.Path == "/" ||
		strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n") {
		return ""
	}
	return u.RequestURI()
}
