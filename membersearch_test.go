package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// searchReply is what Discord's member search returns for "al": a member with
// a server nickname, one with only a global name, one with neither, and a bot.
const searchReply = `[
	{"nick":"Captain Al","user":{"id":"111","username":"alpha","global_name":"Alpha Person"}},
	{"nick":null,"user":{"id":"222","username":"alfie","global_name":"Alfie"}},
	{"user":{"id":"333","username":"alan"}},
	{"user":{"id":"999","username":"alertbot","bot":true}}
]`

// onMemberSearch answers the search with searchReply and hands back the last
// query string it was asked with.
func onMemberSearch(fake *fakeDiscord, guildID string) func() url.Values {
	var mu sync.Mutex
	var last url.Values
	fake.on(http.MethodGet, "/guilds/"+guildID+"/members/search", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.URL.Query()
		mu.Unlock()
		w.Write([]byte(searchReply))
	})
	return func() url.Values { mu.Lock(); defer mu.Unlock(); return last }
}

// TestMemberSearchNamesPeopleTheWayTheServerDoes, and leaves bots out.
func TestMemberSearchNamesPeopleTheWayTheServerDoes(t *testing.T) {
	fake := newFakeDiscord(t)
	lastQuery := onMemberSearch(fake, "g1")
	matches, err := fake.client().SearchGuildMembers("g1", "al", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	want := []MemberMatch{
		{UserID: "111", DisplayName: "Captain Al", Username: "alpha"},
		{UserID: "222", DisplayName: "Alfie", Username: "alfie"},
		{UserID: "333", DisplayName: "alan", Username: "alan"},
	}
	if len(matches) != len(want) {
		t.Fatalf("matches = %+v, want %+v", matches, want)
	}
	for i := range want {
		if matches[i] != want[i] {
			t.Errorf("match %d = %+v, want %+v", i, matches[i], want[i])
		}
	}
	if q := lastQuery(); q.Get("query") != "al" || q.Get("limit") != "10" {
		t.Errorf("searched with %v, want query=al limit=10", q)
	}
}

func getJSON(t *testing.T, mux http.Handler, token, path string, into any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if into != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

// TestTheAddBoxSuggestsMembersAndSaysWhoIsAlreadyOn.
func TestTheAddBoxSuggestsMembersAndSaysWhoIsAlreadyOn(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	onMemberSearch(fake, "g1")
	ev := publishedEvent(t, store, 1, "111", "222")

	var out struct {
		Members []memberSuggestion `json:"members"`
	}
	if code := getJSON(t, mux, token, eventPath(ev)+"/members?q=al", &out); code != http.StatusOK {
		t.Fatalf("search answered %d", code)
	}
	got := map[string]string{}
	for _, m := range out.Members {
		got[m.DisplayName] = m.OnRoster
	}
	want := map[string]string{"Captain Al": StateAttending, "Alfie": StateWaitlisted, "alan": ""}
	for name, state := range want {
		if s, ok := got[name]; !ok || s != state {
			t.Errorf("%s: on_roster %q (present %v), want %q", name, s, ok, state)
		}
	}

	out.Members = nil
	if code := getJSON(t, mux, token, eventPath(ev)+"/members?q=", &out); code != http.StatusOK || len(out.Members) != 0 {
		t.Errorf("an empty query answered %d with %v, want 200 and nobody", code, out.Members)
	}
}

// TestOnlyAManagerCanSearchMembers: the list names people in the server.
func TestOnlyAManagerCanSearchMembers(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	onMemberSearch(fake, "g1")
	ev := publishedEvent(t, store, 4)
	member, err := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if code := getJSON(t, mux, member.Token, eventPath(ev)+"/members?q=al", nil); code != http.StatusForbidden {
		t.Errorf("a member without Manage Events got %d, want 403", code)
	}
}

// TestAddingByIDTakesTheNameFromDiscord.
func TestAddingByIDTakesTheNameFromDiscord(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/222", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":null,"user":{"id":"222","username":"alfie","global_name":"Alfie"}}`))
	})
	ev := publishedEvent(t, store, 4)

	rec := postForm(t, mux, token, eventPath(ev)+"/roster/add", url.Values{"discord_user_id": {"222"}})
	if !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("Added Alfie.")) {
		t.Errorf("redirected to %s", rec.Header().Get("Location"))
	}
	roster, _ := store.Roster(ev.ID, false)
	if len(roster) != 1 || roster[0].DiscordUserID != "222" || roster[0].DisplayName != "Alfie" {
		t.Errorf("roster = %+v, want Alfie by id 222", roster)
	}
}

// TestAnIDThatIsNotAMemberAddsNobody: a mistyped id used to join a stranger's
// snowflake to the roster.
func TestAnIDThatIsNotAMemberAddsNobody(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/404404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Member","code":10007}`))
	})
	ev := publishedEvent(t, store, 4)

	for _, id := range []string{"404404", ""} {
		rec := postForm(t, mux, token, eventPath(ev)+"/roster/add", url.Values{"discord_user_id": {id}})
		if !strings.Contains(rec.Header().Get("Location"), "Nobody+was+added") {
			t.Errorf("id %q: redirected to %s", id, rec.Header().Get("Location"))
		}
	}
	if roster, _ := store.Roster(ev.ID, false); len(roster) != 0 {
		t.Errorf("roster = %+v, want nobody", roster)
	}
}

// TestTheEventPageHasTheNameBox renders the real template.
func TestTheEventPageHasTheNameBox(t *testing.T) {
	srv := NewServer(testStore(t), nil, nil)
	rec := httptest.NewRecorder()
	srv.render(rec, "detail.html", pageData{Session: &WebSession{}, CanManage: true,
		Event: &Event{ID: 7, GuildID: "g1", Name: "Games", Status: StatusOpen}})
	page := rec.Body.String()
	for _, want := range []string{`data-search="/events/7/members"`, `name="discord_user_id" id="member-id"`,
		`id="member-list"`, `/^\d{15,21}$/`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}
