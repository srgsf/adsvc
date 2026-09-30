package admin

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/web"
)

const (
	testToken = "s3cret-admin"
	bobToken  = "bob-token"
)

func init() { loginDelay = time.Millisecond }

// fixture is an admin page over an in-memory store with a few ads, and a peer whose
// records were ingested.
type fixture struct {
	t       *testing.T
	st      *store.Store
	peer    *catalog.DB
	srv     *httptest.Server
	tokens  atomic.Pointer[[]string]
	reloads atomic.Int32
	cookie  *http.Cookie

	mine, theirs, other fingerprint.ID
}

func points(seed int32) ([]byte, fingerprint.ID) {
	var pts []fingerprint.Point
	for i := range int32(600) {
		pts = append(pts, fingerprint.Point{H: uint32((seed*7919 + i*104729) & (1<<fingerprint.HashBits - 1)), T: i / 6})
	}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		panic(err)
	}
	return b, fingerprint.IDOf(b)
}

func enroll(t *testing.T, db *catalog.DB, seed int32, label string) fingerprint.ID {
	t.Helper()
	b, id := points(seed)
	if _, err := db.AddAd(t.Context(), catalog.Ad{ID: id, FPVersion: fingerprint.Version, Label: label, DurationMs: 3200,
		NPoints: 600, Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	return id
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peer, err := catalog.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	if err := peer.SetIdentity(t.Context(), record.NewIdentity()); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, peer: peer}
	f.tokens.Store(&[]string{testToken})
	f.mine = enroll(t, st.DB, 1, "mine")
	f.theirs = enroll(t, peer, 2, "theirs")
	f.other = enroll(t, peer, 3, "other")
	if err := peer.PublishFileMap(t.Context(), &catalog.FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, DurationMs: 60000,
		Detections: []catalog.Detection{{Ad: f.theirs, StartMs: 1000, EndMs: 4200, Score: 90, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	es, err := peer.Log(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var recs []record.Record
	for _, e := range es {
		recs = append(recs, e.Record)
	}
	if _, err := st.DB.Ingest(t.Context(), recs, peer.Self()); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.PutFileMap(t.Context(), &catalog.FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, DurationMs: 60000,
		Analyzed: [][2]int32{{0, 30000}}, Detections: []catalog.Detection{{Ad: f.mine, StartMs: 10000, EndMs: 13200, Score: 60, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lib.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, err := New(t.Context(), Deps{
		DB: st.DB, Lib: st.Lib,
		Users: func() []User {
			tokens := *f.tokens.Load()
			if len(tokens) == 0 { // nobody has a token
				return []User{{Name: "alice", Admin: true}, {Name: "bob"}}
			}
			return []User{{Name: "alice", Tokens: tokens, Admin: true}, {Name: "bob", Tokens: []string{bobToken}}}
		},
		Config: func() ([]byte, error) {
			return []byte("listen: 127.0.0.1:8080\nauth:\n  users:\n    alice: {tokens: [<redacted>], admin: true}\n"), nil
		},
		Reload:   func() ([]string, error) { f.reloads.Add(1); return []string{"listen"}, nil },
		Peers:    func() []string { return []string{"friend"} },
		MinScore: func() int { return 20 },
		Version:  "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(a)
	t.Cleanup(f.srv.Close)
	return f
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request; hdr holds headers as "Name: value".
func (f *fixture) do(method, path string, form url.Values, hdr ...string) (*http.Response, string) {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (f *fixture) login() {
	f.t.Helper()
	resp, _ := f.do("POST", "/login", url.Values{"token": {testToken}, "next": {"/origins"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/origins" {
		f.t.Fatalf("login: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			f.cookie = c
		}
	}
	if f.cookie == nil || !f.cookie.HttpOnly || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Path != "/" {
		f.t.Fatalf("session cookie: %+v", f.cookie)
	}
}

// post sends an htmx form post, as the page does.
func (f *fixture) post(path string, form url.Values) (*http.Response, string) {
	f.t.Helper()
	return f.do("POST", path, form, "X-Adsvc-Admin: 1", "HX-Request: true", "Origin: "+f.srv.URL)
}

func TestLogin(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		name, path string
		hdr        []string
		code       int
		want       string // in Location, HX-Redirect or body
	}{
		{"page redirects", "/ads?q=x", nil, http.StatusSeeOther, "/login?next=%2Fads%3Fq%3Dx"},
		{"api is 401", "/api/ads", nil, http.StatusUnauthorized, `"error"`},
		{"htmx is told to go", "/ads", []string{"HX-Request: true"}, http.StatusUnauthorized, "/login"},
		{"login form", "/login", nil, http.StatusOK, `name="token"`},
		{"root", "/", nil, http.StatusSeeOther, "/login"},
	} {
		resp, body := f.do("GET", c.path, nil, c.hdr...)
		got := resp.Header.Get("Location") + resp.Header.Get("HX-Redirect") + body
		if resp.StatusCode != c.code || !strings.Contains(got, c.want) {
			t.Errorf("%s: %d %q, want %d with %q", c.name, resp.StatusCode, got, c.code, c.want)
		}
	}
	if resp, body := f.do("POST", "/login", url.Values{"token": {"wrong"}}); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(body, "Wrong token") || len(resp.Cookies()) != 0 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp, _ := f.do("POST", "/login", url.Values{"token": {testToken}}, "Origin: https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", resp.StatusCode)
	}
	for _, next := range []string{"//evil.example/admin/", "https://evil.example/admin/", "/\\evil", "/"} {
		resp, _ := f.do("POST", "/login", url.Values{"token": {testToken}, "next": {next}})
		if loc := resp.Header.Get("Location"); loc != "/ads" {
			t.Errorf("next %q: redirected to %q", next, loc)
		}
	}
	f.login()
	if resp, body := f.do("GET", "/ads", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, `class="version"`) ||
		!strings.Contains(body, ">1.2.3<") {
		t.Fatalf("logged in: %d", resp.StatusCode)
	}
	// A token taken out of the config ends its sessions.
	f.tokens.Store(&[]string{"another"})
	if resp, _ := f.do("GET", "/ads", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("session of a removed token: %d", resp.StatusCode)
	}
	f.tokens.Store(&[]string{testToken})
	if resp, _ := f.do("POST", "/logout", nil, "X-Adsvc-Admin: 1"); resp.StatusCode != http.StatusSeeOther ||
		len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout: %d %v", resp.StatusCode, resp.Cookies())
	}
	// No tokens: the page is off.
	f.tokens.Store(&[]string{})
	for _, p := range []string{"/ads", "/login"} {
		if resp, body := f.do("GET", p, nil); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "auth.users") {
			t.Errorf("%s without tokens: %d", p, resp.StatusCode)
		}
	}
}

func TestSessions(t *testing.T) {
	s, now := newSessions(), time.Now()
	tok := s.issue("k", now)
	for _, c := range []struct {
		name   string
		tok    string
		tokens []string
		at     time.Time
		ok     bool
	}{
		{"valid", tok, []string{"x", "k"}, now, true},
		{"valid, one of many", tok, []string{"k", "y"}, now, true},
		{"expired", tok, []string{"k"}, now.Add(sessionTTL), false},
		{"other token", tok, []string{"j"}, now, false},
		{"tampered", tok[:10] + map[bool]string{true: "A", false: "B"}[tok[10] != 'A'] + tok[11:], []string{"k"}, now, false},
		{"garbage", "!!", []string{"k"}, now, false},
		{"other secret", newSessions().issue("k", now), []string{"k"}, now, false},
	} {
		users := []User{{Name: "a", Tokens: c.tokens[:1]}, {Name: "b", Tokens: c.tokens[1:], Admin: true}}
		v, got := s.valid(c.tok, users, c.at)
		if got != c.ok || (got && (v.token != "k" || v.Admin != (c.tokens[1] == "k"))) {
			t.Errorf("%s: %v %+v, want %v", c.name, got, v, c.ok)
		}
	}
}

func TestCSRF(t *testing.T) {
	f := newFixture(t)
	f.login()
	path := "/ads/" + f.mine.String() + "/label"
	form := url.Values{"label": {"renamed"}}
	if resp, _ := f.do("POST", path, form); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without the header: %d", resp.StatusCode)
	}
	if resp, _ := f.do("POST", path, form, "X-Adsvc-Admin: 1", "Origin: https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	if resp, _ := f.do("POST", path, form, "X-Adsvc-Admin: 1", "Sec-Fetch-Site: cross-site"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site fetch: %d", resp.StatusCode)
	}
	if got := f.st.Lib.Get(f.mine).Label; got != "mine" {
		t.Fatalf("label changed to %q by a rejected request", got)
	}
	if resp, body := f.post(path, form); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Label saved") {
		t.Fatalf("same origin: %d %s", resp.StatusCode, body)
	}
	if got := f.st.Lib.Get(f.mine).Label; got != "renamed" {
		t.Fatalf("label %q after the edit (the index keeps labels too)", got)
	}
}

// untranslated finds translation keys left in a page.
var untranslated = regexp.MustCompile(`\b(nav|ads|ad|col|files|file|origins|origin|peers|tracking|config|quality|state|reason|kind|sort|login|error)\.[a-z_]+\b`)

func TestPages(t *testing.T) {
	f := newFixture(t)
	f.login()
	pages := []string{
		"/ads", "/ads?q=the&origin=a&pinned=no&fp=1&sort=label&page=9",
		"/ads/" + f.theirs.String(), "/ads/" + f.mine.String()[:8], "/ads/" + f.mine.String() + "/quality",
		"/files", "/files?q=abc", "/file?key=ih:abc/1",
		"/origins", "/peers", "/tracking", "/config",
	}
	for _, lang := range []string{"en", "ru-RU,ru;q=0.9"} {
		for _, p := range pages {
			for _, htmx := range []bool{false, true} {
				hdr := []string{"Accept-Language: " + lang}
				if htmx {
					hdr = append(hdr, "HX-Request: true")
				}
				resp, body := f.do("GET", p, nil, hdr...)
				if !htmx && strings.HasSuffix(p, "/quality") {
					if resp.StatusCode != http.StatusSeeOther {
						t.Errorf("%s without htmx: %d, want a redirect to the ad", p, resp.StatusCode)
					}
					continue
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s (%s, htmx %v): %d %s", p, lang, htmx, resp.StatusCode, body)
					continue
				}
				if m := untranslated.FindString(body); m != "" {
					t.Errorf("%s (%s): untranslated %q", p, lang, m)
				}
				if !htmx && !strings.Contains(body, "<nav>") {
					t.Errorf("%s: not a whole page", p)
				}
				if htmx && strings.Contains(body, "<html") && !strings.Contains(p, "/file?") && !strings.Contains(p, "/peers") {
					t.Errorf("%s: htmx got the whole page", p)
				}
			}
		}
	}
	_, body := f.do("GET", "/ads", nil, "Accept-Language: ru")
	if !strings.Contains(body, "Ролики") || !strings.Contains(body, `lang="ru"`) {
		t.Error("no Russian page for Accept-Language: ru")
	}
	_, body = f.do("GET", "/file?key=ih:abc/1", nil)
	if !strings.Contains(body, "not found here") { // the peer's ad lies in a range this node analysed
		t.Error("file page: the contradiction is not shown")
	}
	for _, c := range []struct{ path, want string }{
		{"/ads/00000000-0000-0000-0000-000000000001", "no such ad"},
		{"/file?key=c:nothing", "No map"},
		{"/file", "key is required"},
		{"/ads?fp=abc", "not a number"},
		{"/ads?sort=trust", "want created or label"},
	} {
		resp, body := f.do("GET", c.path, nil)
		if resp.StatusCode < 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want an error with %q", c.path, resp.StatusCode, c.want)
		}
	}
}

func TestJSON(t *testing.T) {
	f := newFixture(t)
	f.login()
	var ads struct {
		Ads []struct {
			ID      fingerprint.ID `json:"id"`
			Label   string         `json:"label"`
			Tracked bool           `json:"tracked"`
			Author  record.Origin  `json:"author"`
		} `json:"ads"`
		Total int `json:"total"`
	}
	resp, body := f.do("GET", "/api/ads?sort=label", nil)
	if resp.Header.Get("Content-Type") != "application/json" || json.Unmarshal([]byte(body), &ads) != nil {
		t.Fatalf("api/ads: %s %s", resp.Header.Get("Content-Type"), body)
	}
	if ads.Total != 3 || ads.Ads[0].Label != "mine" || !ads.Ads[0].Tracked || ads.Ads[1].Author != f.peer.Self() {
		t.Fatalf("api/ads: %+v", ads)
	}
	var q qualityView
	_, body = f.do("GET", "/api/ads/"+f.mine.String()+"/quality", nil)
	if json.Unmarshal([]byte(body), &q) != nil || q.SelfMin < 20 || q.MinScore != 20 {
		t.Fatalf("api quality: %s", body)
	}
	// Accept: application/json on a page route works as well.
	_, body = f.do("GET", "/peers", nil, "Accept: application/json")
	var peers peersPage
	if json.Unmarshal([]byte(body), &peers) != nil || len(peers.Peers) != 1 || peers.Peers[0].Name != "friend" || peers.Peers[0].Synced {
		t.Fatalf("peers: %s", body)
	}
	resp, body = f.do("POST", "/api/ads/"+f.mine.String()+"/vote", url.Values{"value": {"+2"}}, "X-Adsvc-Admin: 1")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `"error"`) {
		t.Fatalf("bad vote: %d %s", resp.StatusCode, body)
	}
}

func TestActions(t *testing.T) {
	f := newFixture(t)
	f.login()
	lib := f.st.Lib
	if lib.Get(f.theirs) != nil {
		t.Fatal("an unknown origin's ad is tracked")
	}
	ad := func(id fingerprint.ID) string { return "/ads/" + id.String() }

	// Votes and pins move ads in and out of the index at once.
	if resp, body := f.post(ad(f.theirs)+"/vote", url.Values{"value": {"+1"}, "reason": {"good"}}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "Vote recorded") {
		t.Fatalf("vote: %d %s", resp.StatusCode, body)
	}
	if lib.Get(f.theirs) == nil {
		t.Fatal("not tracked after this node's +1")
	}
	if resp, _ := f.post(ad(f.theirs)+"/vote", url.Values{"value": {"-1"}, "reason": {"not_ad"}}); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if lib.Get(f.theirs) != nil {
		t.Fatal("tracked after this node's −1")
	}
	if resp, _ := f.post(ad(f.theirs)+"/pin", url.Values{"on": {"1"}}); resp.StatusCode != http.StatusOK || lib.Get(f.theirs) == nil {
		t.Fatal("a pinned ad is not tracked")
	}
	// Bulk: unpin and vote up.
	resp, body := f.post("/ads/bulk", url.Values{"id": {f.theirs.String(), f.other.String()}, "action": {"unpin"}, "q": {""}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Done for 2 of 2") || lib.Get(f.theirs) != nil {
		t.Fatalf("bulk unpin: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.post("/ads/bulk", url.Values{"id": {f.other.String()}, "action": {"up"}}); resp.StatusCode != http.StatusOK || lib.Get(f.other) == nil {
		t.Fatal("bulk vote up")
	}
	if resp, _ := f.post("/ads/bulk", url.Values{"id": {"nonsense"}, "action": {"up"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bulk with a bad id: %d", resp.StatusCode)
	}
	// Dup, by id prefix: the duplicate leaves the index.
	if resp, body := f.post(ad(f.other)+"/dup", url.Values{"canonical": {f.theirs.String()[:8]}}); resp.StatusCode != http.StatusOK ||
		lib.Get(f.other) != nil {
		t.Fatalf("dup: %d %s", resp.StatusCode, body)
	}
	if resp, body := f.post(ad(f.other)+"/dup", url.Values{"canonical": {f.other.String()}}); resp.StatusCode != http.StatusUnprocessableEntity ||
		resp.Header.Get("HX-Retarget") != "#flash" {
		t.Fatalf("dup of itself: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.post(ad(f.mine)+"/label", url.Values{"label": {"see https://example.com"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a URL label: %d", resp.StatusCode)
	}

	// Origins: weight and block, but not config-managed ones.
	o := "/origins/" + f.peer.Self().String()
	if resp, body := f.post(o, url.Values{"name": {"friend"}, "weight": {"3"}, "action": {"save"}}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "friend") {
		t.Fatalf("set weight: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.post(ad(f.theirs)+"/vote", url.Values{"value": {"+1"}}); resp.StatusCode != http.StatusOK || lib.Get(f.theirs) == nil {
		t.Fatal("not tracked after voting +1 again")
	}
	if resp, _ := f.post(o, url.Values{"blocked": {"1"}, "action": {"save"}}); resp.StatusCode != http.StatusOK || lib.Get(f.theirs) != nil {
		t.Fatal("the ads of a blocked origin stay tracked")
	}
	if resp, _ := f.post(o, url.Values{"action": {"clear"}}); resp.StatusCode != http.StatusOK || lib.Get(f.theirs) == nil {
		t.Fatal("cleared policy")
	}
	if resp, _ := f.post(o, url.Values{"weight": {"-1"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("negative weight")
	}
	if resp, _ := f.post("/origins/"+f.st.DB.Self().String(), url.Values{"blocked": {"1"}}); resp.StatusCode != http.StatusConflict {
		t.Fatal("blocking this node")
	}
	if err := f.st.DB.SetOrigins(t.Context(), []catalog.OriginRule{{Origin: f.peer.Self(), Blocked: true}}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.post(o, url.Values{"weight": {"5"}}); resp.StatusCode != http.StatusConflict {
		t.Fatal("a config-managed origin changed")
	}

	// Tracking and config.
	if resp, body := f.post("/tracking/rebuild", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Rebuilt in") {
		t.Fatalf("rebuild: %d %s", resp.StatusCode, body)
	}
	if resp, body := f.post("/config/reload", nil); resp.StatusCode != http.StatusOK || f.reloads.Load() != 1 ||
		!strings.Contains(body, "need a restart: listen") {
		t.Fatalf("reload: %d %s", resp.StatusCode, body)
	}
}

func TestStatic(t *testing.T) {
	f := newFixture(t)
	a, _ := New(t.Context(), Deps{DB: f.st.DB, Lib: f.st.Lib, Users: func() []User { return nil }})
	h := a.assets["js/htmx.min.js"]
	for _, c := range []struct {
		path  string
		code  int
		cache string
	}{
		{"/static/js/htmx.min.js?v=" + h, http.StatusOK, "immutable"},
		{"/static/js/htmx.min.js", http.StatusOK, "no-cache"},
		{"/static/css/admin.css?v=old", http.StatusOK, "no-cache"},
		{"/static/js/", http.StatusNotFound, ""},
		{"/static/js/missing.js", http.StatusNotFound, ""},
		{"/static/templates/ads.tmpl", http.StatusNotFound, ""},
	} {
		resp, _ := f.do("GET", c.path, nil) // no session: assets are public
		if resp.StatusCode != c.code || !strings.Contains(resp.Header.Get("Cache-Control"), c.cache) {
			t.Errorf("%s: %d %q", c.path, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	resp, _ := f.do("GET", "/login", nil)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers: %v", resp.Header)
	}
}

func TestDetectLang(t *testing.T) {
	for h, want := range map[string]string{
		"": "en", "ru": "ru", "ru-RU,ru;q=0.9,en;q=0.8": "ru", "en-US,en;q=0.9,ru;q=0.8": "en",
		"de-DE,ru;q=0.5": "ru", "de,fr": "en", "ru;q=0,en;q=0.1": "en", "RU-ru": "ru", "ru;q=bogus,en;q=0.2": "en", "*": "en",
	} {
		if got := detectLang(h); got != want {
			t.Errorf("%q: %s, want %s", h, got, want)
		}
	}
}

// Every language has every key, and every key a template asks for exists.
func TestTranslations(t *testing.T) {
	keys := map[string][]string{}
	for _, l := range languages {
		b, err := fs.ReadFile(web.FS, "i18n/"+l+".json")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			keys[l] = append(keys[l], k)
		}
		slices.Sort(keys[l])
	}
	if !slices.Equal(keys["en"], keys["ru"]) {
		t.Fatalf("en and ru have different keys")
	}
	used := regexp.MustCompile(`\.Tf? "([a-z_.]+)"`)
	tmpls, _ := fs.Glob(web.FS, "templates/*.tmpl")
	// Keys the templates build (printf "state.%s" …) from these sets.
	dynamic := []string{"state.active"}
	for _, name := range tmpls {
		if n := strings.TrimSuffix(path.Base(name), ".tmpl"); n != "layout" {
			dynamic = append(dynamic, "nav."+n)
		}
	}
	for _, s := range []string{catalog.StateRetracted, catalog.StateBlocked, catalog.StateDup} {
		dynamic = append(dynamic, "state."+s)
	}
	for _, k := range []string{record.KindAdAdd, record.KindAdLabel, record.KindAdVote, record.KindAdDup, record.KindAdRetract, record.KindFileMap} {
		dynamic = append(dynamic, "kind."+k)
	}
	for _, r := range []string{record.ReasonGood, record.ReasonNotAd, record.ReasonBoundary, record.ReasonDup} {
		dynamic = append(dynamic, "reason."+r)
	}
	for _, s := range []string{catalog.SortCreated, catalog.SortLabel} {
		dynamic = append(dynamic, "sort."+s)
	}
	for _, k := range dynamic {
		if !slices.Contains(keys["en"], k) {
			t.Errorf("no translation for %q", k)
		}
	}
	for _, name := range tmpls {
		b, _ := fs.ReadFile(web.FS, name)
		for _, m := range used.FindAllStringSubmatch(string(b), -1) {
			if !slices.Contains(keys["en"], m[1]) {
				t.Errorf("%s: no translation for %q", name, m[1])
			}
		}
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/ads?q=1": "/ads?q=1", "/reports": "/reports", "": "", "/": "",
		"//evil/admin/": "", "javascript:alert(1)": "", "/\r\nx": "",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// loginAs logs in with token and returns the redirect target.
func (f *fixture) loginAs(token string) string {
	f.t.Helper()
	f.cookie = nil
	resp, _ := f.do("POST", "/login", url.Values{"token": {token}})
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			f.cookie = c
		}
	}
	if f.cookie == nil {
		f.t.Fatalf("login: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

func TestReports(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	add := func(user string, pos int32) int64 {
		t.Helper()
		id, err := f.st.DB.AddReport(ctx, catalog.Report{User: user, Norm: "h/" + user, URL: "http://u:pw@ts.lan:8090/stream/x.mkv?link=abc&play",
			Sel: "ih=abc&idx=2", PosMs: pos, Note: user + "'s note"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	bobs, alices := add("bob", 754_300), add("alice", 5_000)

	if home := f.loginAs(bobToken); home != "/reports" {
		t.Fatalf("a user starts at %q", home)
	}
	resp, body := f.do("GET", "/reports", nil)
	wantCmd := fmt.Sprintf("mpv --start=734 &#39;%s/s?u=%s&amp;ih=abc&amp;idx=2&amp;mark=1&#39;", f.srv.URL,
		url.QueryEscape("http://u:pw@ts.lan:8090/stream/x.mkv?link=abc&play"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "bob&#39;s note") || strings.Contains(body, "alice&#39;s note") ||
		!strings.Contains(body, wantCmd) || strings.Contains(body, `href="/ads"`) {
		t.Fatalf("bob's reports: %d\n%s\nwant %s", resp.StatusCode, body, wantCmd)
	}
	if strings.Contains(body, "u:pw@ts.lan:8090/stream/x.mkv?link=abc&amp;play</h2>") {
		t.Error("the shown source is not redacted")
	}
	for path, code := range map[string]int{"/ads": http.StatusSeeOther, "/api/ads": http.StatusForbidden, "/": http.StatusSeeOther} {
		if resp, _ := f.do("GET", path, nil); resp.StatusCode != code || (code == http.StatusSeeOther && resp.Header.Get("Location") != "/reports") {
			t.Errorf("bob at %s: %d to %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, _ := f.post(fmt.Sprintf("/reports/%d/delete", alices), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob deletes alice's report: %d", resp.StatusCode)
	}
	resp, body = f.do("GET", "/reports/adskip.lua", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `ads_token = "`+bobToken+`"`) ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "adskip.lua") {
		t.Fatalf("script: %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	if resp, body := f.post(fmt.Sprintf("/reports/%d/delete", bobs), nil); resp.StatusCode != http.StatusOK || strings.Contains(body, "bob&#39;s note") {
		t.Fatalf("bob deletes his report: %d %s", resp.StatusCode, body)
	}

	if home := f.loginAs(testToken); home != "/ads" {
		t.Fatalf("an admin starts at %q", home)
	}
	var d reportsPage
	resp, body = f.do("GET", "/api/reports", nil)
	if err := json.Unmarshal([]byte(body), &d); err != nil || resp.StatusCode != http.StatusOK || !d.All ||
		len(d.Files) != 1 || d.Files[0].Reports[0].User != "alice" {
		t.Fatalf("admin's reports: %d %s", resp.StatusCode, body)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	f.login()
	lib := f.st.Lib
	ad := "/ads/" + f.mine.String()
	if _, body := f.do("GET", ad, nil); !strings.Contains(body, ad+"/delete") {
		t.Fatal("no delete button on a tracked ad")
	}
	if resp, body := f.post(ad+"/delete", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Ad deleted") ||
		lib.Get(f.mine) != nil || strings.Contains(body, ad+"/delete") {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	d, err := f.st.DB.AdDetail(t.Context(), f.mine)
	if err != nil || d.State != "retracted" {
		t.Fatalf("own ad after delete: %+v %v", d.State, err)
	}

	// someone else's ad, in bulk: this node votes it down
	if resp, _ := f.post("/ads/"+f.other.String()+"/pin", url.Values{"on": {"1"}}); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if resp, body := f.post("/ads/bulk", url.Values{"id": {f.other.String()}, "action": {"delete"}}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "Done for 1 of 1") {
		t.Fatalf("bulk delete: %d %s", resp.StatusCode, body)
	}
	if err := lib.Refresh(t.Context()); err != nil || lib.Get(f.other) != nil {
		t.Fatalf("a deleted (pinned) ad is still tracked: %v", err)
	}
}

func TestPossibleDuplicates(t *testing.T) {
	f := newFixture(t)
	f.login()
	// the same ad enrolled twice, with one landmark less the second time
	b, _ := points(1)
	pts, err := fingerprint.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	b, err = fingerprint.Encode(pts[:len(pts)-1])
	if err != nil {
		t.Fatal(err)
	}
	twin := fingerprint.IDOf(b)
	if _, err := f.st.DB.AddAd(t.Context(), catalog.Ad{ID: twin, FPVersion: fingerprint.Version, Label: "twin", DurationMs: 3200,
		NPoints: len(pts) - 1, Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Lib.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// the list loads them after the table, as out-of-band swaps of the rows' dots
	_, body := f.do("GET", "/ads", nil)
	m := regexp.MustCompile(`hx-get="(/ads/dups\?[^"]+)"`).FindStringSubmatch(body)
	if m == nil || strings.Contains(body, `class="dot warn"`) {
		t.Fatalf("the list does not load its possible duplicates later:\n%s", body)
	}
	if _, dups := f.do("GET", html.UnescapeString(m[1]), nil, "HX-Request: true"); strings.Count(dups, `class="dot warn"`) != 2 ||
		strings.Count(dups, `hx-swap-oob="true"`) != 2 || !strings.Contains(dups, `id="dup-`+twin.String()+`"`) {
		t.Errorf("the duplicates fragment does not mark both twins:\n%s", dups)
	}
	var api []struct {
		ID          fingerprint.ID `json:"id"`
		PossibleDup *struct {
			ID fingerprint.ID `json:"id"`
		} `json:"possible_duplicate"`
	}
	if _, js := f.do("GET", "/api/ads/dups?id="+twin.String()+"&id="+f.other.String(), nil); json.Unmarshal([]byte(js), &api) != nil ||
		len(api) != 1 || api[0].ID != twin || api[0].PossibleDup == nil || api[0].PossibleDup.ID != f.mine {
		t.Errorf("/api/ads/dups: %s", js)
	}
	_, body = f.do("GET", "/ads/"+twin.String(), nil)
	if i, j := strings.Index(body, "Possible duplicate of"), strings.Index(body, "</h1>"); i < j ||
		!strings.Contains(body[i:], `href="/ads/`+f.mine.String()+`"`) {
		t.Errorf("no duplicate banner under the title, linking the other ad:\n%s", body)
	}
	// an ad of its own kind is not flagged
	if _, body := f.do("GET", "/ads/"+f.other.String(), nil); strings.Contains(body, "Possible duplicate of") {
		t.Error("a distinct ad is flagged as a duplicate")
	}
	// once marked as a duplicate, the banner says so instead
	if resp, _ := f.post("/ads/"+twin.String()+"/dup", url.Values{"canonical": {f.mine.String()}}); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if _, body := f.do("GET", "/ads/"+twin.String(), nil); !strings.Contains(body, "Marked as a duplicate of") ||
		strings.Contains(body, "Possible duplicate of") {
		t.Errorf("banner after marking the duplicate:\n%s", body)
	}
	// the pair is marked on both sides, in the list and on the other ad's page
	if _, body := f.do("GET", "/ads", nil); strings.Count(body, `class="dot bad"`) != 2 || strings.Contains(body, `class="dot warn"`) {
		t.Errorf("the list does not mark the known pair:\n%s", body)
	}
	if _, body := f.do("GET", "/ads/"+f.mine.String(), nil); !strings.Contains(body, "Has a duplicate:") ||
		!strings.Contains(body, `href="/ads/`+twin.String()+`"`) {
		t.Errorf("no banner on the ad that has a duplicate:\n%s", body)
	}
	// the quality section no longer repeats the warning
	if _, body := f.do("GET", "/ads/"+f.mine.String()+"/quality", nil, "HX-Request: true"); strings.Contains(body, "Collides") {
		t.Errorf("quality still warns at the bottom:\n%s", body)
	}
}
