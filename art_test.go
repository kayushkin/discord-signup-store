package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestEveryDrawingAPageNamesIsServed: the pages point at /art/… files, and
// each one must be in the binary, served as an image without a login.
func TestEveryDrawingAPageNamesIsServed(t *testing.T) {
	_, _, _, mux, _ := webTestServer(t)
	page := getPage(t, mux, "", "/").Body.String()
	named := regexp.MustCompile(`(?:src|href)="(/art/[^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(named) < 4 {
		t.Fatalf("the signed-out home page names %d drawings, want two icons, the masthead one and the sign-in one", len(named))
	}
	for _, match := range named {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, match[1], nil))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/") {
			t.Errorf("%s = %d %q, want 200 and an image", match[1], rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/vnd.microsoft.icon" {
		t.Errorf("/favicon.ico = %d %q, want 200 image/vnd.microsoft.icon", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/art/nobody.webp", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a drawing that does not exist = %d, want 404", rec.Code)
	}
}

// TestEveryFontAPageNamesIsServed: the fonts come from this binary, not from
// Google, whose stylesheet held up the first paint; every /fonts/… file a page
// names must be served as a font without a login.
func TestEveryFontAPageNamesIsServed(t *testing.T) {
	_, _, _, mux, _ := webTestServer(t)
	page := getPage(t, mux, "", "/").Body.String()
	if strings.Contains(page, "fonts.googleapis.com") || strings.Contains(page, "fonts.gstatic.com") {
		t.Error("the page still loads fonts from Google")
	}
	named := map[string]bool{}
	for _, match := range regexp.MustCompile(`(/fonts/[^")]+\.woff2)`).FindAllStringSubmatch(page, -1) {
		named[match[1]] = true
	}
	if len(named) != 5 {
		t.Errorf("the page names %d font files, want five: four weights of Zen Kaku Gothic New and Dela Gothic One", len(named))
	}
	for path := range named {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "font/woff2" {
			t.Errorf("%s = %d %q, want 200 font/woff2", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
