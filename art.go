package discordsignup

import (
	"embed"
	"io/fs"
	"net/http"
)

// The drawings the web pages show, painted by art/render.mjs from the code in
// art/drawings and built into the binary.
//
//go:embed static/art/*.webp
var artFiles embed.FS

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
