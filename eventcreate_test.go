package discordsignup

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTheOrganiserIsOnTheirOwnRoster covers the web create form. Somebody who
// fills in the form is going; making them press Join afterwards is a step that
// only exists because the software forgot.
func TestTheOrganiserIsOnTheirOwnRoster(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)

	postForm(t, mux, token, "/events/new", url.Values{
		"guild_id":  {"g1"},
		"name":      {"Board game night"},
		"capacity":  {"4"},
		"starts_at": {"9/29 7pm"},
		"timezone":  {"America/Los_Angeles"},
	})

	events, err := store.ListEvents("g1", "", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("listed %d events, %v", len(events), err)
	}
	ev := events[0]
	if ev.AttendingCount != 1 {
		t.Fatalf("%d attending on a brand new event, want the organiser", ev.AttendingCount)
	}
	roster, err := store.Roster(ev.ID, false)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(roster) != 1 || roster[0].DiscordUserID != "manager" {
		t.Fatalf("roster is %+v, want the person who filled in the form", roster)
	}
	if roster[0].JoinedVia != JoinedViaOrganiser {
		t.Errorf("joined_via = %q, want %q — they never pressed anything",
			roster[0].JoinedVia, JoinedViaOrganiser)
	}
	if roster[0].State != StateAttending {
		t.Errorf("state = %q, want attending", roster[0].State)
	}
}

// TestTheCardForANewEventAlreadyShowsItsOrganiser is why the helper re-reads
// the event before returning it. AttendingCount is filled by the read path, so
// the struct CreateEvent hands back says nobody is going — and the very next
// thing that happens is a card being rendered from it.
func TestTheCardForANewEventAlreadyShowsItsOrganiser(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")

	ev, err := srv.createEventAndJoinOrganiser(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 4,
		StartsAt: time.Now().Add(48 * time.Hour).Unix(), CreatedBy: "u-organiser",
	}, "Organiser")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ev.AttendingCount != 1 {
		t.Fatalf("the returned event says %d attending, want 1", ev.AttendingCount)
	}

	if _, err := srv.PostSignupMessage(ev.ID); err != nil {
		t.Fatalf("post card: %v", err)
	}
	var posted string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/board/messages" {
			posted, _ = c.Body["content"].(string)
		}
	}
	if !strings.Contains(posted, "1/4 places taken") {
		t.Errorf("the first card reads %q, want it to already say 1/4", posted)
	}
	if !strings.Contains(posted, "<@u-organiser>") {
		t.Error("the organiser is not named on their own event's card")
	}
}

// TestAnImportedEventDoesNotSignUpItsDiscordCreator holds the boundary. Making
// a native Discord event is not filling in a signup form, and this service
// putting somebody on a roster they never asked for would be it inventing a
// signup.
func TestAnImportedEventDoesNotSignUpItsDiscordCreator(t *testing.T) {
	store := testStore(t)
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Imported", Capacity: 4,
		StartsAt: time.Now().Add(48 * time.Hour).Unix(),
		Origin:   OriginDiscord, CreatedBy: "u-made-it-in-discord",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AttendingCount != 0 {
		t.Errorf("%d attending on an imported event, want 0 — nobody signed up yet",
			got.AttendingCount)
	}
}
