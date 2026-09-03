package discordsignup

import (
	"net/url"
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
