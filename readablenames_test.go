package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAReadableNameReplacesTheDisplayNameOnDiscord, joined on the id, and the
// display name comes back when it is removed.
func TestAReadableNameReplacesTheDisplayNameOnDiscord(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 0)
	store.Join(ev.ID, "u-matt", "Lil' Fascist Matt 🌟", JoinedViaButton)
	store.Join(ev.ID, "u-al", "Al", JoinedViaButton)
	if _, err := store.SetReadableName("u-matt", "Matt"); err != nil {
		t.Fatalf("set: %v", err)
	}
	roster, _ := store.Roster(ev.ID, false)
	if got := strings.Join(rosterNamesOnDiscord(roster), ", "); got != "Matt, Al" {
		t.Errorf("names = %q, want Matt, Al", got)
	}
	if roster[0].DisplayName != "Lil' Fascist Matt 🌟" {
		t.Errorf("the display name was overwritten: %q", roster[0].DisplayName)
	}
	withName := eventPublishSignature(ev, roster)
	store.DeleteReadableName("u-matt")
	roster, _ = store.Roster(ev.ID, false)
	if got := rosterNamesOnDiscord(roster)[0]; got != "Lil' Fascist Matt 🌟" {
		t.Errorf("after delete = %q", got)
	}
	if eventPublishSignature(ev, roster) == withName {
		t.Error("changing a readable name does not change the signature, so nothing would redraw")
	}
}

// TestReadableNameRoutes.
func TestReadableNameRoutes(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	if rec := do(http.MethodPut, "/api/readable-names/u-1", `{"readable_name":"Heidi"}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{`{"readable_name":""}`, `{"name":"Heidi"}`} {
		if rec := do(http.MethodPut, "/api/readable-names/u-1", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", bad, rec.Code)
		}
	}
	if rec := do(http.MethodGet, "/api/readable-names", ""); !strings.Contains(rec.Body.String(), `"readable_name":"Heidi"`) {
		t.Errorf("list = %s", rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/api/readable-names/u-1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d", rec.Code)
	}
	if rec := do(http.MethodDelete, "/api/readable-names/u-1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}
