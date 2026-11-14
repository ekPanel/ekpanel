package api

import (
	"html"
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
	index, _ := fs.ReadFile(ui, "index.html")
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(s.brandedIndex(r, index))
	})
}

// brandedIndex puts the brand of the domain the panel was opened at into the page (its
// title, icon and color), so the first paint already looks right. The app takes over from
// there, with the signed-in account's brand.
func (s *Server) brandedIndex(r *http.Request, index []byte) []byte {
	e, err := s.hostBrand(r)
	if err != nil || !e.Custom {
		return index
	}
	page := strings.Replace(string(index), "<title>ekPanel</title>", "<title>"+html.EscapeString(e.Name)+"</title>", 1)
	if e.LogoID != "" {
		page = strings.Replace(page, `<link rel="icon" type="image/svg+xml" href="/favicon.svg" />`,
			`<link rel="icon" type="image/png" href="`+html.EscapeString(logoURL(e.LogoID, e.LogoVersion))+`" />`, 1)
	}
	if e.Color != "" {
		page = strings.Replace(page, "</title>", "</title>\n    <meta name=\"ekpanel-brand-color\" content=\""+html.EscapeString(e.Color)+"\" />", 1)
	}
	return []byte(page)
}
