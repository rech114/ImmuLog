// SPDX-License-Identifier: Apache-2.0

// auth.go -- T2 in docs/DESIGN.md §7.11: the credential that decides who may
// talk to this port at all.
//
// It is deliberately **not** an identity. Identity is a signature (§4.1), and
// nothing in this project ever derives it from a request. So a stolen token
// buys someone the ability to read this node and to send messages **they
// signed** -- no more. That demotion (§7.9) is what makes the simplest possible
// mechanism the right one: one shared secret, no users, no sessions, no JWT.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

const (
	// cookieName is the one place the credential is stored.
	//
	// A cookie rather than an `Authorization` header because **EventSource
	// cannot set request headers**, and the SSE stream is the frontend's main
	// channel (§7.11). That is a constraint, not a preference.
	cookieName = "immulog_token"

	// unlockPage is the entry form, read from the same embedded tree as the
	// app so it inherits the same stylesheet and fonts.
	unlockPage = "unlock.html"

	// tokenBytes is the entropy behind a generated token.
	tokenBytes = 32

	// healthPath answers without the secret, so a liveness probe does not need
	// one. It answers with a single field; see guard.
	healthPath = "/api/health"
)

// NewToken generates a token for one run: 32 random bytes, hex encoded. Long
// enough that guessing is not a threat, short enough to paste.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("cannot generate a token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// guard gates the surface behind the token.
//
// An empty token means open mode. main.go only reaches that state through an
// explicit IMMULOG_OPEN=1 and says so loudly at startup, rather than leaving it
// implicit -- the same way it reports every other security property it does not
// have.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next.ServeHTTP(w, r)
			return
		}

		authed := s.authenticated(r)

		// A liveness probe has to answer without the secret or orchestrators
		// cannot use it. It answers with one field and nothing else: the real
		// payload names the feed, the head and every peer.
		if !authed && r.URL.Path == healthPath {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if !authed {
			s.challenge(w, r)
			return
		}
		// Arriving with ?token= **as a navigation** stores it and immediately
		// redirects to the same URL without it, so the credential does not
		// linger in history, in the address bar, or in any Referer.
		//
		// Only a navigation. A script that passes ?token= is served directly:
		// redirecting a POST would turn it into a GET and silently drop the
		// write, which is a far worse failure than a token in one's own shell
		// history.
		if isNavigation(r) && r.URL.Query().Has("token") {
			s.storeCookie(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isNavigation reports whether a request is a browser following a link rather
// than a script calling an API. It decides two things: whether a challenge is a
// page or a JSON error, and whether ?token= is retired into a cookie.
func isNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// authenticated checks every place a client may present the token.
//
// Three shapes, because there are three kinds of client: a browser (cookie,
// which it attaches by itself), a human following a link (query), and a script
// or a gossip peer (bearer header).
func (s *Server) authenticated(r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil && sameSecret(c.Value, s.token) {
		return true
	}
	if sameSecret(r.URL.Query().Get("token"), s.token) {
		return true
	}
	const bearer = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearer) {
		return sameSecret(strings.TrimPrefix(h, bearer), s.token)
	}
	return false
}

// sameSecret compares without leaking position or length through timing: a
// byte-by-byte early exit is a remote oracle for a secret that never expires.
func sameSecret(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// storeCookie writes the credential and redirects past it.
func (s *Server) storeCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.token,
		Path:     "/",
		HttpOnly: true, // a script must not be able to read it back out
		SameSite: http.SameSiteStrictMode,
		// r.TLS is nil behind a TLS-terminating proxy, which is the deployment
		// §7.11 recommends, so the standard signal has to be read as well. Get
		// this wrong one way and the cookie is dropped over HTTPS; wrong the
		// other way and it is sent over plain HTTP.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		// Session cookie, deliberately: it dies with the browser instead of
		// outliving the token in IMMULOG_TOKEN.
	})
	http.Redirect(w, r, withoutToken(r.URL), http.StatusSeeOther)
}

// withoutToken rebuilds a request URL with the credential removed.
func withoutToken(u *url.URL) string {
	q := u.Query()
	q.Del("token")
	target := u.Path
	if target == "" {
		target = "/"
	}
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return target
}

// challenge answers an unauthenticated request.
//
// A browser gets the entry page; an API or SSE client gets JSON, because an
// HTML page inside a fetch or an event stream is worse than an error.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	if !isNavigation(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	page, err := fs.ReadFile(s.files, unlockPage)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(page)
}
