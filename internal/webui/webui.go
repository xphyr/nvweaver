// Package webui is the browser interface, embedded in the binary. It's static files that use the public API from the browser,
// with no build step and no dependencies, so it can do nothing that the API can't, and holds no secrets of its own.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Path is where the interface is served.
const Path = "/ui/"

// contentSecurityPolicy allows only this service's own files and API, so that if a name from the API ever reached the page
// as markup, it couldn't run anything or send anything elsewhere.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the interface under Path. It needs no token: these files are the same for everyone, and the API
// itself decides what a browser may do.
func Handler() http.Handler {
	root, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // The embedded directory is there, or this wouldn't have compiled.
	}
	files := http.StripPrefix(Path, http.FileServerFS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Embedded files have no modification time, and an upgrade should take effect at once.
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
