package discordsignup

import (
	"net/url"
	"strings"
	"testing"
)

// TestAnOrganizerMakesAnotherWhoCanThenEditTheEvent: the host makes a
// member an organizer; that member then gets the edit page, can make a third
// person one, wears the badge, and is a member again once taken back.
func TestAnOrganizerMakesAnotherWhoCanThenEditTheEvent(t *testing.T) {
	_, store, _, mux, _ := webTestServer(t)
	ev, _ := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "Party", Status: StatusOpen,
		StartsAt: 4102444800, CreatedBy: "host"})
	for _, who := range []string{"host", "kim", "lee"} {
		store.Join(ev.ID, who, who, JoinedViaButton)
	}
	host, _ := store.CreateWebSession("host", "Host", "", map[string]uint64{"g1": 0})
	kim, _ := store.CreateWebSession("kim", "Kim", "", map[string]uint64{"g1": 0})
	lee, _ := store.CreateWebSession("lee", "Lee", "", map[string]uint64{"g1": 0})
	editPage := func(token string) bool {
		return strings.Contains(getPage(t, mux, token, eventPath(ev)).Body.String(), `name="name" required`)
	}
	if editPage(kim.Token) {
		t.Fatal("kim gets the edit page before anyone made her an organizer")
	}
	postForm(t, mux, host.Token, eventPath(ev)+"/organisers", url.Values{"discord_user_id": {"kim"}, "organiser": {"true"}})
	if !editPage(kim.Token) {
		t.Fatal("kim, made an organizer, does not get the edit page")
	}
	postForm(t, mux, kim.Token, eventPath(ev)+"/organisers", url.Values{"discord_user_id": {"lee"}, "organiser": {"true"}})
	if !editPage(lee.Token) {
		t.Fatal("lee, made an organizer by kim, does not get the edit page")
	}
	page := getPage(t, mux, host.Token, eventPath(ev)).Body.String()
	for _, want := range []string{`organiser-mark`, `host-mark`, "Stop being an organizer", "made an organizer"} {
		if !strings.Contains(page, want) {
			t.Errorf("the organizer page lacks %q", want)
		}
	}
	postForm(t, mux, host.Token, eventPath(ev)+"/organisers", url.Values{"discord_user_id": {"kim"}, "organiser": {"false"}})
	if editPage(kim.Token) {
		t.Fatal("kim keeps the edit page after it was taken back")
	}
	if member := getPage(t, mux, kim.Token, eventPath(ev)).Body.String(); !strings.Contains(member, `organiser-badge">organizer`) {
		t.Error("the members' page does not badge lee as an organizer")
	}
	// The creator cannot be made one again, and the route is organisers'.
	rec := postForm(t, mux, lee.Token, eventPath(ev)+"/organisers", url.Values{"discord_user_id": {"host"}, "organiser": {"true"}})
	if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(loc, "host it, so they already are one") {
		t.Errorf("making the host an organizer said %q", loc)
	}
}
