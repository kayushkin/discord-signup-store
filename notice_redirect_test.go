package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// noticeFromLocation reads a notice back exactly the way a browser does: parse
// the Location header, then ask for the query parameter. Anything the notice
// loses between the redirect and this call is a message the user never sees.
func noticeFromLocation(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("a browser could not parse the Location we sent: %q: %v", location, err)
	}
	return parsed.Query().Get("notice")
}

// TestANoticeSurvivesTheRedirectVerbatim is the guard for the whole family. Each
// case is a value the running service really can put in a notice, and every one
// of them is destroyed by a hand-rolled space-and-newline replacement.
func TestANoticeSurvivesTheRedirectVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		notice string
	}{
		{"a percent sign", "100% done"},
		{"an ampersand that would otherwise start a second parameter",
			"Problems: my guild&notice=Everything worked: boom"},
		{"a newline", "line one\nline two"},
		{"a plus sign", "1+1 problems"},
		{"a guild name in a non-Latin script", "Проблемы: сервер"},
		{"a bare question mark", "Sync failed. Retry?"},
		{"a hash", "Problems: #general is gone"},
		{"a semicolon and a space, which is how problems are joined",
			"Problems: one: boom; two: bang"},
		{"plain text with no reserved character at all",
			"Pulled from Discord: 3 new, 1 updated, 0 unchanged, 2 cards posted."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := noticeFromLocation(t, "/?"+noticeQuery(tc.notice))
			if got != tc.notice {
				t.Errorf("the notice did not survive the redirect\n sent: %q\n read: %q", tc.notice, got)
			}
		})
	}
}

// TestTheNoticeIsTheOnlyParameterAValueCanCreate pins the specific escalation an
// unescaped ampersand allows: a guild name is chosen by whoever owns the Discord
// server, so a value arriving here can try to inject a parameter of its own.
func TestTheNoticeIsTheOnlyParameterAValueCanCreate(t *testing.T) {
	hostile := "boom&admin=1&notice=all+fine"
	parsed, err := url.Parse("/?" + noticeQuery(hostile))
	if err != nil {
		t.Fatalf("Location did not parse: %v", err)
	}
	query := parsed.Query()
	if len(query) != 1 {
		t.Fatalf("a notice value created extra query parameters: %v", query)
	}
	if len(query["notice"]) != 1 {
		t.Fatalf("a notice value created a second notice: %v", query["notice"])
	}
	if query.Get("notice") != hostile {
		t.Errorf("notice = %q, want %q", query.Get("notice"), hostile)
	}
}

// TestTheRedirectItselfCarriesTheNoticeIntact drives the real redirect helper
// rather than the encoder underneath it, so a call site that goes back to
// concatenating "?notice=" by hand reddens here. Nine of the ten notices the
// detail page can emit go through this one function; without this test the
// guard above would stay green while every one of them was broken again.
func TestTheRedirectItselfCarriesTheNoticeIntact(t *testing.T) {
	server := &Server{}
	for _, notice := range []string{
		"100% done",
		"Problems: one: boom; two: bang",
		"Problems: #general is gone",
		"boom&admin=1",
	} {
		t.Run(notice, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/events/7/publish", nil)

			server.redirectWithNotice(recorder, request, 7, notice)

			if recorder.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusSeeOther)
			}
			location := recorder.Header().Get("Location")
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatalf("a browser could not parse Location %q: %v", location, err)
			}
			if parsed.Path != "/events/7" {
				t.Errorf("redirected to path %q, want %q", parsed.Path, "/events/7")
			}
			if got := parsed.Query().Get("notice"); got != notice {
				t.Errorf("notice lost in the redirect\n sent: %q\n read: %q\n  loc: %q", notice, got, location)
			}
		})
	}
}
