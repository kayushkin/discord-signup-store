package discordsignup

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestASeparateRoleIsApartFromTheMainList: a game show with 2 places and an
// audience of 2 apart from them. Someone not going takes the audience role
// and is not put on Going; the audience's own cap holds; a role that takes
// a place still refuses someone not going; and every count shows the main
// places and then the audience's.
func TestASeparateRoleIsApartFromTheMainList(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 2)
	switchRolesOn(t, store, ev.ID)
	store.Join(ev.ID, "kat", "Kat", JoinedViaButton)
	players, _ := store.AddSheetRole(ev.ID, "Players", 12, true, "test")
	audience, err := store.AddSheetRole(ev.ID, "Audience", 2, false, "test")
	if err != nil || audience.TakesPlace {
		t.Fatalf("add = %+v, %v", audience, err)
	}

	for _, who := range []string{"ann", "bo"} {
		if in, err := store.ToggleSheetRole(ev.ID, audience.ID, who, strings.ToUpper(who)); err != nil || !in {
			t.Fatalf("%s taking the audience = %v, %v", who, in, err)
		}
	}
	if _, err := store.ToggleSheetRole(ev.ID, audience.ID, "cy", "CY"); !errors.Is(err, ErrSheetRoleFull) {
		t.Fatalf("a third in an audience of 2: err = %v", err)
	}
	if _, err := store.ToggleSheetRole(ev.ID, players.ID, "ann", "ANN"); !errors.Is(err, ErrNotGoing) {
		t.Fatalf("someone not going taking a role that takes a place: err = %v", err)
	}

	ev = mustEvent(t, store, ev.ID)
	if ev.AttendingCount != 1 {
		t.Fatalf("attending = %d; the audience must not take places", ev.AttendingCount)
	}
	got := ev.Roles[1]
	if len(got.People) != 2 || got.People[0].DisplayName != "ANN" {
		t.Fatalf("audience people = %+v, want ANN and BO by the names Discord gave", got.People)
	}

	card := renderSignupMessage(ev, nil)["content"].(string)
	if !strings.Contains(card, "**1/2 places taken** · ⭐ Audience 2/2") {
		t.Errorf("card count line:\n%s", card)
	}
	block := signupBlock(ev, nil, "board", 4000)
	if !strings.Contains(block, "1 of 2 places taken. Plus ⭐ Audience 2/2.") {
		t.Errorf("native event block = %q", block)
	}
	// Discord redraws when a role changes kind.
	before := eventPublishSignature(ev, nil)
	no := true
	store.EditSheetRole(ev.ID, audience.ID, SheetRoleEdit{TakesPlace: &no}, "web:manager")
	if eventPublishSignature(mustEvent(t, store, ev.ID), nil) == before {
		t.Error("switching a role's kind leaves the publish signature as it was")
	}
	// Counting toward Going, the audience shows only people going: nobody.
	if people := mustEvent(t, store, ev.ID).Roles[1].People; len(people) != 0 {
		t.Errorf("after the switch the audience shows %+v", people)
	}
}

// TestSeparateRolesOnThePages: the organizer adds a separate role and
// switches another, the log says so, a member who is not going may take the
// separate role, and the organizer may put someone from the waitlist or
// Maybe in it.
func TestSeparateRolesOnThePages(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/member", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Member","user":{"id":"member","username":"member"}}`))
	})
	ev := publishedEvent(t, store, 1, "alice")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{RolesEnabled: &on})
	store.MarkMaybe(ev.ID, "nj", "NJ", JoinedViaButton)
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})

	postForm(t, mux, token, eventPath(ev)+"/roles", url.Values{"name": {"Audience"}, "capacity": {"6"}, "takes_place": {"false"}})
	postForm(t, mux, token, eventPath(ev)+"/roles", url.Values{"name": {"Hosts"}, "capacity": {"2"}})
	roles := mustEvent(t, store, ev.ID).Roles
	if roles[0].TakesPlace || !roles[1].TakesPlace {
		t.Fatalf("roles = %+v; want Audience separate and Hosts counting", roles)
	}
	postForm(t, mux, token, eventPath(ev)+"/roles/edit", url.Values{"role_id": {fmt.Sprint(roles[1].ID)}, "takes_place": {"false"}})
	hosts := mustEvent(t, store, ev.ID).Roles[1]
	if hosts.TakesPlace || hosts.Capacity != 2 || hosts.Emoji != roles[1].Emoji {
		t.Fatalf("after switching, hosts = %+v; only takes_place should change", hosts)
	}

	page := getPage(t, mux, member.Token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "separate from Going") || !strings.Contains(page, "Count me in") {
		t.Fatal("the members' page does not offer a separate role to someone not going")
	}
	postForm(t, mux, member.Token, eventPath(ev)+"/roles/toggle", url.Values{"role_id": {fmt.Sprint(roles[0].ID)}})
	if r := mustEvent(t, store, ev.ID).Roles[0]; !r.Has("member") {
		t.Fatal("someone not going could not take a separate role")
	}

	organizerPage := getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(organizerPage, `name="discord_user_id" value="nj">NJ</button>`) {
		t.Error("the organizer cannot put someone on Maybe into a separate role")
	}
	postForm(t, mux, token, eventPath(ev)+"/roles/put-in", url.Values{"role_id": {fmt.Sprint(roles[0].ID)}, "discord_user_id": {"nj"}})
	audience := mustEvent(t, store, ev.ID).Roles[0]
	if !audience.Has("nj") || audience.People[0].DisplayName != "Member" || audience.People[1].DisplayName != "NJ" {
		t.Fatalf("audience = %+v; want Member and NJ by name", audience.People)
	}
	organizerPage = getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{"made ⭐ Hosts separate from Going", "⭐ Hosts 0/2"} {
		if !strings.Contains(organizerPage, want) {
			t.Errorf("the organizer's page lacks %q", want)
		}
	}
}
