package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const uiNotBuilt = `<!doctype html><meta charset="utf-8"><title>ekPanel</title>
<p>The web UI has not been built into this binary. Run <code>make ui</code>, then rebuild,
or use the Vite dev server (<code>make dev-ui</code>).</p>`

// uiHandler serves the single-page app: real files when they exist, index.html otherwise,
// so client-side routes like /servers/123 load the app.
func (s *Server) uiHandler() http.Handler {
	ui := s.opts.UI
	if ui != nil {
		if _, err := fs.Stat(ui, "index.html"); err != nil {
			ui = nil
		}
	}
	if ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(uiNotBuilt))
		})
	}
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(ui, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					// Vite puts a content hash in every asset name, so they never change.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, ui, "index.html")
	})
}
