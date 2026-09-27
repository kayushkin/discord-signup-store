package discordsignup

import (
	"embed"
	"io/fs"
	"net/http"
)

// The drawings the web pages show, painted by art/render.mjs from the code in
// art/drawings and built into the binary.
//
//go:embed static/art/*.webp static/art/*.png static/art/favicon.ico
var artFiles embed.FS

// The web fonts, cut down by fonts/subset.sh, and their licences.
//
//go:embed static/fonts/*.woff2 static/fonts/*.txt
var fontFiles embed.FS

// handleArt serves one drawing. The files change only with a new binary, so a
// browser may keep one for a day.
func (s *Server) handleArt() http.Handler {
	drawings, err := fs.Sub(artFiles, "static/art")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/art/", http.FileServer(http.FS(drawings)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

// handleFonts serves the web fonts, which also change only with a new binary.
func (s *Server) handleFonts() http.Handler {
	fonts, err := fs.Sub(fontFiles, "static/fonts")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/fonts/", http.FileServer(http.FS(fonts)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

// handleFavicon serves /favicon.ico, which browsers ask for at the root
// whatever the page's own icon links say.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFileFS(w, r, artFiles, "static/art/favicon.ico")
}
