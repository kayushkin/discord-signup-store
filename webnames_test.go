package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func namesPageServer(t *testing.T) (*Store, http.Handler, string) {
	t.Helper()
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/users/@me/guilds", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"g1","name":"Games club"}]`))
	})
	return store, mux, token
}

// TestTheNamesPageListsEveryoneOnAListAndSavesByID.
func TestTheNamesPageListsEveryoneOnAListAndSavesByID(t *testing.T) {
	store, mux, token := namesPageServer(t)
	ev := publishedEvent(t, store, 1, "u-matt", "u-al")
	store.MarkMaybe(ev.ID, "u-cy", "Cy", JoinedViaButton)
	store.Join(ev.ID, "u-gone", "Gone", JoinedViaButton)
	store.Leave(ev.ID, "u-gone", ActorUser)

	req := httptest.NewRequest(http.MethodGet, "/names", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	page := rec.Body.String()
	for _, want := range []string{`value="u-matt"`, `value="u-al"`, `value="u-cy"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(page, `value="u-gone"`) {
		t.Error("someone who left is still listed")
	}

	rec = postForm(t, mux, token, "/names", url.Values{"discord_user_id": {"u-matt"}, "readable_name": {"Matt"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	roster, _ := store.Roster(ev.ID, false)
	if roster[0].NameOnDiscord() != "Matt" {
		t.Errorf("shown as %q, want Matt", roster[0].NameOnDiscord())
	}
	// An empty box removes it.
	postForm(t, mux, token, "/names", url.Values{"discord_user_id": {"u-matt"}, "readable_name": {""}})
	roster, _ = store.Roster(ev.ID, false)
	if roster[0].ReadableName != "" {
		t.Errorf("readable name still %q after clearing it", roster[0].ReadableName)
	}
}

// TestTheNamesPageNamesOnlyPeopleOnYourServersLists: naming someone changes
// how every table shows them, so a stranger's id is refused.
func TestTheNamesPageNamesOnlyPeopleOnYourServersLists(t *testing.T) {
	store, mux, token := namesPageServer(t)
	publishedEvent(t, store, 4, "u-al")
	req := httptest.NewRequest(http.MethodPost, "/names",
		strings.NewReader(url.Values{"discord_user_id": {"u-stranger"}, "readable_name": {"X"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("naming a stranger answered %d, want 403", rec.Code)
	}
	if names, _ := store.ReadableNames(); len(names) != 0 {
		t.Errorf("stored %+v", names)
	}
}
