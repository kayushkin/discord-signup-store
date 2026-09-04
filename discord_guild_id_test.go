package discordsignup

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestCreateScheduledEventRefusesAnEmptyGuildID pins the failure this call site
// used to hide.
//
// The guild id was read back out of the caller's payload map, and both arms of
// that read swallowed: a payload that was not a map[string]any, and a map whose
// guild_id was absent or not a string, each came back "". The client then posted
// to "/guilds//scheduled-events", which is a different route rather than a bad
// argument — so whatever Discord answered described the request and never the
// mistake that built it.
//
// The request must not be sent at all. A 404 from Discord is not the same
// finding: it says the route is wrong, which is true of a great many mistakes,
// and it costs a round trip to learn nothing.
func TestCreateScheduledEventRefusesAnEmptyGuildID(t *testing.T) {
	fake := newFakeDiscord(t)

	created, err := fake.client().CreateScheduledEvent("", map[string]any{"name": "no guild"})
	if err == nil {
		t.Fatal("an empty guild id was accepted; it addresses a different route, so it has to be refused")
	}
	if created != nil {
		t.Errorf("created = %v, want nil alongside the error", created)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Errorf("%d request(s) reached Discord: %v — an empty guild id must be caught before the wire", len(calls), calls)
	}
}

// TestTheGuildIsTheRouteNotTheBody pins where the published guild id comes from.
//
// Discord's create-scheduled-event body has no guild_id field: the guild is the
// route. The payload carried one anyway, for one reason — so the client could
// dig it back out to build that route. Now that the id is a parameter, a
// guild_id in the body would be a second copy of a value the route already
// states, and the two could disagree.
func TestTheGuildIsTheRouteNotTheBody(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	srv.SetDefaultTimezone("UTC")

	fake.on(http.MethodPost, "/guilds/g1/scheduled-events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"native-1","name":"Board game night"}`)
	})

	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Board game night",
		StartsAt: time.Now().Add(72 * time.Hour).Unix(), Capacity: 8, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.PublishToDiscord(ev.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var found bool
	for _, c := range fake.recorded() {
		if c.Method != http.MethodPost || c.Path != "/guilds/g1/scheduled-events" {
			continue
		}
		found = true
		if guild, present := c.Body["guild_id"]; present {
			t.Errorf("body carries guild_id = %v; the guild belongs in the route, and a second copy can disagree with it", guild)
		}
	}
	if !found {
		t.Fatalf("nothing was posted to /guilds/g1/scheduled-events; calls were %v", fake.recorded())
	}
}
