// SPDX-License-Identifier: Apache-2.0

// headers.go -- T3 in docs/DESIGN.md §7.11: keeping a served page from being
// turned against the person reading it.
//
// Every line has to be justifiable, because a header nobody can explain is a
// header the next person deletes.
package web

import "net/http"

// csp is the policy in one place, so it can be read as a whole.
//
// **`script-src 'self'` is the one that matters.** Every script in this app is a
// same-origin ES module, so an injected `<script>` -- inline or remote -- dies
// here. Injecting script is the most direct route to the message bodies, and
// therefore to the token, and it is the reason §7.11 lists T3 at all.
//
// `style-src` keeps `'unsafe-inline'` because Beer CSS's dynamic colour runtime
// builds the palette at run time. That is a real concession, stated rather than
// hidden: it costs little, because a style injection cannot read an HttpOnly
// cookie.
//
// ⚠️ `connect-src 'self'` is correct while the client reads one origin. §7.12's
// second backend widens it to the configured backends -- a change worth
// noticing, not a line to relax by accident.
const csp = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// securityHeaders wraps every response -- the app, the assets and the unlock
// page alike, since a policy that only covers some routes is a policy with a
// hole in it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		// An asset served with a type the browser disagrees about must not be
		// sniffed into something executable.
		h.Set("X-Content-Type-Options", "nosniff")
		// The token arrives in a URL once, before the redirect strips it;
		// Referer is the obvious way for it to escape.
		h.Set("Referrer-Policy", "no-referrer")
		// Nothing here is meant to be framed, and nothing here frames anything.
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
