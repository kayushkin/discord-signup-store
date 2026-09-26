package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTheHistoryNamesThePeopleWhoDidThings, and leaves the words alone.
func TestTheHistoryNamesThePeopleWhoDidThings(t *testing.T) {
	for actor, want := range map[string][2]string{
		"web:493904201101869067":     {"493904201101869067", "web"},
		"discord:110122051179687936": {"110122051179687936", "discord"},
		"110122051179687936":         {"110122051179687936", ""},
		"interested":                 {"", ""},
		"web:manager":                {"", ""},
	} {
		if id, via := actorUserID(actor); id != want[0] || via != want[1] {
			t.Errorf("%s = %q %q, want %q %q", actor, id, via, want[0], want[1])
		}
	}
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1/members/493904201101869067", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Waleeha","user":{"id":"493904201101869067","username":"midnachew"}}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/110122051179687936", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := NewServer(testStore(t), nil, fake.client())
	names := srv.historyActorNames("g1", []string{"web:493904201101869067", "110122051179687936", "user"})
	if got := names["web:493904201101869067"]; got.DisplayName != "Waleeha" || got.Via != "web" {
		t.Errorf("names = %v", names)
	}
	if _, ok := names["110122051179687936"]; ok {
		t.Error("someone Discord cannot name was given a name; the page should show the raw actor")
	}
	if _, ok := names["user"]; ok {
		t.Error("a word was looked up as a person")
	}
}

func getPage(t *testing.T, mux http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestTheHomePageRemembersWhichServer, per user, and every server again when
// it is cleared.
func TestTheHomePageRemembersWhichServer(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	fake.on(http.MethodGet, "/users/@me/guilds", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"g1","name":"One"},{"id":"g2","name":"Two"}]`))
	})
	store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "First server night", Status: StatusOpen, StartsAt: 4102444800})
	store.CreateEvent(Event{GuildID: "g2", ChannelID: "c", Name: "Second server night", Status: StatusOpen, StartsAt: 4102444800})
	both, _ := store.CreateWebSession("u-both", "Both", "", map[string]uint64{"g1": permissionManageEvents, "g2": permissionManageEvents})

	page := getPage(t, mux, both.Token, "/").Body.String()
	if !strings.Contains(page, "First server night") || !strings.Contains(page, "Second server night") {
		t.Fatal("unfiltered home page does not show both servers")
	}
	if !strings.Contains(page, `name="guild_id"`) {
		t.Error("no server filter on the page")
	}
	postForm(t, mux, both.Token, "/preferences/home-server", url.Values{"guild_id": {"g2"}})
	page = getPage(t, mux, both.Token, "/").Body.String()
	if strings.Contains(page, "First server night") || !strings.Contains(page, "Second server night") {
		t.Error("filtered home page shows the wrong server")
	}
	// It is the person's, not the login's.
	again, _ := store.CreateWebSession("u-both", "Both", "", map[string]uint64{"g1": permissionManageEvents, "g2": permissionManageEvents})
	if strings.Contains(getPage(t, mux, again.Token, "/").Body.String(), "First server night") {
		t.Error("a new login lost the filter")
	}
	postForm(t, mux, both.Token, "/preferences/home-server", url.Values{"guild_id": {""}})
	if !strings.Contains(getPage(t, mux, both.Token, "/").Body.String(), "First server night") {
		t.Error("clearing the filter did not bring every server back")
	}
}

// TestTheNamesPageCanNameSomeoneOnNoList, found by search, and only in a
// server the viewer may name people in.
func TestTheNamesPageCanNameSomeoneOnNoList(t *testing.T) {
	store, fake, mux, token := namesPageServer(t)
	onMemberSearch(fake, "g1")
	fake.on(http.MethodGet, "/guilds/g1/members/222", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":null,"user":{"id":"222","username":"alfie","global_name":"Alfie"}}`))
	})
	publishedEvent(t, store, 4, "u-al")
	// u-al is on a roster, so the search looks them up too; they have left.
	fake.on(http.MethodGet, "/guilds/g1/members/u-al", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Member","code":10007}`))
	})

	rec := getPage(t, mux, token, "/names/members?guild_id=g1&q=al")
	var out struct {
		Members []struct {
			UserID string `json:"user_id"`
		} `json:"members"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Members) != 3 {
		t.Fatalf("search = %d %s", rec.Code, rec.Body.String())
	}
	if rec := getPage(t, mux, token, "/names/members?guild_id=g-elsewhere&q=al"); rec.Code != http.StatusForbidden {
		t.Errorf("searching another server = %d, want 403", rec.Code)
	}
	// 222 is on no list; naming them works through the server they are in.
	postForm(t, mux, token, "/names", url.Values{"guild_id": {"g1"}, "discord_user_id": {"222"}, "readable_name": {"Alf"}})
	names, _ := store.ReadableNames()
	if len(names) != 1 || names[0].DiscordUserID != "222" || names[0].ReadableName != "Alf" {
		t.Errorf("names = %+v, want 222 as Alf", names)
	}
}

// TestThePullInButtonIsGone: the sync runs every ten minutes and new Discord
// events arrive over the gateway at once.
func TestThePullInButtonIsGone(t *testing.T) {
	_, _, _, mux, token := webTestServer(t)
	if strings.Contains(getPage(t, mux, token, "/").Body.String(), "Pull in Discord events") {
		t.Error("the button is still on the home page")
	}
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
		t.Errorf("POST /sync still answers %d", rec.Code)
	}
}

// TestAddingSomeoneAlreadyGoingSaysSo rather than "Added".
func TestAddingSomeoneAlreadyGoingSaysSo(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/222", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":null,"user":{"id":"222","username":"alfie","global_name":"Alfie"}}`))
	})
	ev := publishedEvent(t, store, 4, "222")
	rec := postForm(t, mux, token, eventPath(ev)+"/roster/add", url.Values{"discord_user_id": {"222"}, "list": {StateAttending}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape("Alfie is already going")) {
		t.Errorf("notice = %s", loc)
	}
}
