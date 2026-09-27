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
