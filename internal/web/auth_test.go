// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"immulog/core/feed"
	"immulog/core/gitx"
)

// testToken stands in for the secret main.go generates at startup.
const testToken = "0123456789abcdef0123456789abcdef"

// ── Fixtures ────────────────────────────────────────────────────

func guardedServer(t *testing.T, token string) (*httptest.Server, *feed.Store) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "", "config", "user.name", "alice")
	rawGit(t, dir, "", "config", "user.email", "alice@example.com")

	repo := gitx.Open(dir)
	store, err := feed.New(context.Background(), repo, feed.FeedID("alice"))
	if err != nil {
		t.Fatal(err)
	}

	files := fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><title>ImmuLog</title>")},
		"unlock.html": &fstest.MapFile{Data: []byte(`<!doctype html><form method="get" action="/"><input name="token"></form>`)},
	}
	srv := httptest.NewServer(New(Config{
		Store: store, Hub: NewHub(), Repo: repo, Files: files, Token: token,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

// cred is how a client presents the token: any one of the three shapes §7.11
// allows, or none.
type cred struct {
	query   string
	cookie  string
	bearer  string
	accept  string
	headers map[string]string
}

// request sends one request. Redirects are not followed: the whole point of the
// ?token= path is what it redirects *to*.
func request(t *testing.T, base, method, path string, c cred) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	accept := c.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c.cookie})
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── The gate ────────────────────────────────────────────────────

// Every entry point that can read, write or destroy is behind the token.
func TestAnonymousRequestsAreChallenged(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/snapshot"},
		{http.MethodGet, "/api/stream"},
		{http.MethodPost, "/api/commit"},
		{http.MethodPost, "/api/rotate"},
		{http.MethodPost, "/api/epoch"},
		{http.MethodPost, "/api/shred"},
	}
	for _, c := range cases {
		resp := request(t, srv.URL, c.method, c.path, cred{})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401, got %d", c.method, c.path, resp.StatusCode)
		}
		if !strings.Contains(bodyOf(t, resp), "unauthorized") {
			t.Fatalf("%s %s: an API client should get JSON, not a page", c.method, c.path)
		}
	}
}

// A browser gets something it can act on; a fetch or an event stream does not.
func TestTheUnlockPageIsServedOnlyToBrowsers(t *testing.T) {
	srv, _ := guardedServer(t, testToken)

	browser := request(t, srv.URL, http.MethodGet, "/", cred{accept: "text/html,application/xhtml+xml"})
	if browser.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", browser.StatusCode)
	}
	if ct := browser.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("a browser must get the entry page, got %q", ct)
	}
	if !strings.Contains(bodyOf(t, browser), "<form") {
		t.Fatal("the entry page must actually carry the form")
	}

	api := request(t, srv.URL, http.MethodGet, "/", cred{})
	if ct := api.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Fatalf("a non-browser client must not be handed HTML, got %q", ct)
	}
}

// A liveness probe has to answer without the secret. It answers with one field,
// because the full payload names the feed, the head and every peer.
func TestHealthAnswersAnonymousButSaysNothing(t *testing.T) {
	srv, _ := guardedServer(t, testToken)

	anon := request(t, srv.URL, http.MethodGet, "/api/health", cred{})
	if anon.StatusCode != http.StatusOK {
		t.Fatalf("a probe must succeed, got %d", anon.StatusCode)
	}
	body := bodyOf(t, anon)
	for _, leaked := range []string{`"feed"`, `"head"`, `"peers"`, `"remotes"`, `"signed"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("the anonymous health payload leaks %s: %s", leaked, body)
		}
	}

	full := request(t, srv.URL, http.MethodGet, "/api/health", cred{bearer: testToken})
	complete := bodyOf(t, full)
	for _, want := range []string{`"feed"`, `"head"`, `"signed"`} {
		if !strings.Contains(complete, want) {
			t.Fatalf("the authenticated payload should carry %s: %s", want, complete)
		}
	}
}

// ── Getting in ──────────────────────────────────────────────────

// The credential arrives in the URL exactly once, is stored, and is immediately
// redirected out of the address bar.
func TestQueryTokenSetsACookieAndRedirectsPastIt(t *testing.T) {
	srv, _ := guardedServer(t, testToken)

	resp := request(t, srv.URL, http.MethodGet, "/?token="+testToken, cred{accept: "text/html"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("the redirect must drop the token, got %q", loc)
	}

	cookie := resp.Header.Get("Set-Cookie")
	for _, attr := range []string{cookieName + "=" + testToken, "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, attr) {
			t.Fatalf("the cookie is missing %s: %q", attr, cookie)
		}
	}
}

func TestQueryTokenKeepsTheRestOfTheQuery(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	resp := request(t, srv.URL, http.MethodGet, "/?token="+testToken+"&room=abc", cred{accept: "text/html"})
	if loc := resp.Header.Get("Location"); loc != "/?room=abc" {
		t.Fatalf("want /?room=abc, got %q", loc)
	}
}

func TestEveryCredentialShapeAuthenticates(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	cases := map[string]cred{
		"cookie": {cookie: testToken},
		"bearer": {bearer: testToken},
		"query":  {query: "?token=" + testToken},
	}
	for name, c := range cases {
		path := "/api/snapshot" + c.query
		resp := request(t, srv.URL, http.MethodGet, path, c)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", name, resp.StatusCode)
		}
		if !strings.Contains(bodyOf(t, resp), `"digest"`) {
			t.Fatalf("%s: the surface really is reachable with it", name)
		}
	}
}

// Only a navigation retires the token into a cookie. A script passing ?token=
// is served directly, because redirecting a POST would turn it into a GET and
// drop the write.
func TestOnlyANavigationRetiresTheQueryToken(t *testing.T) {
	srv, _ := guardedServer(t, testToken)

	api := request(t, srv.URL, http.MethodGet, "/api/snapshot?token="+testToken, cred{})
	if api.StatusCode != http.StatusOK {
		t.Fatalf("an API call with ?token= must be served, got %d", api.StatusCode)
	}
	if api.Header.Get("Set-Cookie") != "" {
		t.Fatal("a non-navigation must not be handed a cookie")
	}

	post, err := http.NewRequest(http.MethodPost, srv.URL+"/api/commit?token="+testToken,
		strings.NewReader(`{"kind":"msg","body":"via query"}`))
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a POST carrying ?token= must not be redirected, got %d", resp.StatusCode)
	}
}

func TestAWrongTokenIsRejectedInEveryShape(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	wrong := "not-the-token"
	for name, c := range map[string]cred{
		"cookie": {cookie: wrong},
		"bearer": {bearer: wrong},
		"query":  {query: "?token=" + wrong},
		// A prefix must not pass either.
		"truncated": {bearer: testToken[:len(testToken)-1]},
	} {
		resp := request(t, srv.URL, http.MethodGet, "/api/snapshot"+c.query, c)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", name, resp.StatusCode)
		}
	}
}

// Secure is only set when the request really is over TLS: behind the proxy §7.11
// recommends, r.TLS is nil and X-Forwarded-Proto is the signal. Getting this
// wrong the other way sends the cookie over plain HTTP.
func TestTheCookieIsSecureOnlyOverTLS(t *testing.T) {
	srv, _ := guardedServer(t, testToken)

	proxied := request(t, srv.URL, http.MethodGet, "/?token="+testToken, cred{
		accept:  "text/html",
		headers: map[string]string{"X-Forwarded-Proto": "https"},
	})
	if !strings.Contains(proxied.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("behind HTTPS the cookie must be Secure: %q", proxied.Header.Get("Set-Cookie"))
	}

	plain := request(t, srv.URL, http.MethodGet, "/?token="+testToken, cred{accept: "text/html"})
	if strings.Contains(plain.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("over plain HTTP a Secure cookie would never come back: %q", plain.Header.Get("Set-Cookie"))
	}
}

// ── The point of the whole thing ────────────────────────────────

// The gate must stop the write, not merely answer 401. This is the test that
// says the node is no longer writable by anyone who can reach the port.
func TestAnonymousWritesNeverReachTheRepository(t *testing.T) {
	srv, store := guardedServer(t, testToken)
	ctx := context.Background()

	resp := request(t, srv.URL, http.MethodPost, "/api/commit", cred{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	msgs, err := store.History(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("an anonymous POST reached the chain: %+v", msgs)
	}

	// And the same request with the credential still works, or the gate is
	// simply broken in the other direction.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/commit",
		strings.NewReader(`{"kind":"msg","body":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	ok, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("an authenticated POST should land, got %d", ok.StatusCode)
	}
	if msgs, err = store.History(ctx, 10); err != nil || len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d (%v)", len(msgs), err)
	}
}

// ── T3: headers ─────────────────────────────────────────────────

func TestSecurityHeadersAreOnEveryResponse(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	paths := []struct {
		path string
		c    cred
	}{
		{"/", cred{accept: "text/html"}},
		{"/", cred{accept: "text/html", cookie: testToken}},
		{"/api/health", cred{}},
		{"/api/health", cred{bearer: testToken}},
		{"/api/snapshot", cred{bearer: testToken}},
	}
	for _, p := range paths {
		resp := request(t, srv.URL, http.MethodGet, p.path, p.c)
		for _, h := range []string{
			"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy",
			"X-Frame-Options", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		} {
			if resp.Header.Get(h) == "" {
				t.Fatalf("%s: missing %s (a policy with a hole is not a policy)", p.path, h)
			}
		}
	}
}

// The policy's one valuable line is that scripts may only come from this origin.
// A regression here would be silent, so it is asserted directly.
func TestCSPForbidsAnythingButSameOriginScript(t *testing.T) {
	srv, _ := guardedServer(t, testToken)
	resp := request(t, srv.URL, http.MethodGet, "/", cred{accept: "text/html"})
	csp := resp.Header.Get("Content-Security-Policy")

	if !strings.Contains(csp, "script-src 'self';") {
		t.Fatalf("script-src must be exactly 'self': %q", csp)
	}
	// 'unsafe-inline' is granted to styles only -- see headers.go for why it is
	// granted at all. It must never appear in the directive that runs code.
	if before, _, ok := strings.Cut(csp, "style-src"); ok && strings.Contains(before, "'unsafe-inline'") {
		t.Fatalf("'unsafe-inline' leaked outside style-src: %q", csp)
	}
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline';") {
		t.Fatalf("style-src should carry the documented concession: %q", csp)
	}
	if !strings.Contains(csp, "base-uri 'none'") {
		t.Fatalf("base-uri must be pinned: every URL in this app is relative: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("frame-ancestors must be none: %q", csp)
	}
}

// ── The opt-out ─────────────────────────────────────────────────

// Open mode exists, is reachable only by asking for it in main.go, and behaves
// exactly like the node did before this feature. It is asserted so that
// "accidentally open" would look like a decision rather than a default.
func TestOpenModeIsAnExplicitChoice(t *testing.T) {
	srv, _ := guardedServer(t, "")

	resp := request(t, srv.URL, http.MethodGet, "/api/health", cred{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), `"feed"`) {
		t.Fatal("with no token the full payload comes back -- that is what open mode means")
	}
}
