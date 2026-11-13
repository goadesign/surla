// This file serves the views page: the fixed browser shell that holds no
// project data and talks only to the address it was loaded from.
package controlplane

import (
	_ "embed"
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
)

// viewsPagePolicy is the views page's Content-Security-Policy. The page may
// run only its own inline script and styles, send requests only to the
// address it was loaded from, never be shown inside another page, submit no
// form anywhere, and never change its base address.
const viewsPagePolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"connect-src 'self'; frame-ancestors 'none'; form-action 'none'; base-uri 'none'"

// viewsPageHTML is the views page built from ui/checkpoint into checkpoint.html.
// It is the same for every project and caller: data arrives only through the
// requests the page sends with the session it obtains.
//
//go:embed checkpoint.html
var viewsPageHTML string

// ViewsPage returns the handler for the views page. The composition root
// mounts it at exact GET /views without a credential, behind the project
// server's address and browser-mark checks, which also mark every response
// as no-sniff and no-store. The page's security policy travels as a response
// header so it covers the document before any of it is parsed.
func ViewsPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", viewsPagePolicy)
		if _, err := io.WriteString(w, viewsPageHTML); err != nil {
			otel.Handle(err)
		}
	})
}
