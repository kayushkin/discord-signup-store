package discordsignup

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// markPublished records the event as fully written to Discord, as a clean
// publish does, so a sync treats Discord's copy as current.
func markPublished(t *testing.T, store *Store, eventID int64) {
	t.Helper()
	ev, err := store.GetEvent(eventID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	roster, err := store.Roster(eventID, false)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if err := store.SetPublishedSignature(eventID, eventPublishSignature(ev, roster)); err != nil {
		t.Fatalf("set signature: %v", err)
	}
}

// TestSyncKeepsAnEditDiscordHasNotAccepted: on 2026-09-24 an organiser moved
// an event a day later, Discord refused the push, and the ten-minute sync
// copied Discord's old start back over the edit. While an edit has not
// reached Discord, Discord's copy is the old one and must not win.
func TestSyncKeepsAnEditDiscordHasNotAccepted(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, EndsAt: start + 6*3600, Location: "Victorian square",
		DiscordScheduledEventID: "native-45",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markPublished(t, store, ev.ID)

	moved := start + 86400
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &moved, EndsAt: ptrInt64(moved + 6*3600)}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if _, _, err := srv.syncOneScheduledEvent(DiscordScheduledEvent{
		ID: "native-45", GuildID: "g1", Name: "Roller skate", Status: discordEventScheduled,
		ScheduledStartTime: time.Unix(start, 0).UTC().Format(time.RFC3339),
		ScheduledEndTime:   time.Unix(start+6*3600, 0).UTC().Format(time.RFC3339),
	}, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.StartsAt != moved {
		t.Errorf("starts_at = %d, want the edited %d — the sync copied Discord's stale start back", after.StartsAt, moved)
	}
}

// TestSyncRecordsWhatItChanges: the one thing the sync still takes from
// Discord that a person would ask about — a cancellation — shows in the
// event's history under the sync's own actor.
func TestSyncRecordsWhatItChanges(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-46",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markPublished(t, store, ev.ID)

	if _, _, err := srv.syncOneScheduledEvent(DiscordScheduledEvent{
		ID: "native-46", GuildID: "g1", Name: "Roller skate", Status: discordEventCanceled,
		ScheduledStartTime: time.Unix(start, 0).UTC().Format(time.RFC3339),
	}, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updates, err := store.EventUpdates(ev.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, u := range updates {
		if u.Field == "status" && u.ToValue == StatusCancelled && u.Actor == syncEventUpdateActor {
			return
		}
	}
	t.Errorf("history = %+v, want a status row by %q", updates, syncEventUpdateActor)
}

// TestSyncPushesOurNameBackOverADiscordRename: a rename in Discord's own event
// screen is not copied in; ours goes back out with the name in it.
func TestSyncPushesOurNameBackOverADiscordRename(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-47", Origin: OriginDiscord,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markPublished(t, store, ev.ID)

	if _, _, err := srv.syncOneScheduledEvent(DiscordScheduledEvent{
		ID: "native-47", GuildID: "g1", Name: "Ice skate", Status: discordEventScheduled,
		ScheduledStartTime: time.Unix(start, 0).UTC().Format(time.RFC3339),
		ScheduledEndTime:   time.Unix(start+assumedRunTimeWithoutEndTime, 0).UTC().Format(time.RFC3339),
	}, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.Name != "Roller skate" {
		t.Errorf("name = %q, want ours kept", after.Name)
	}
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && strings.HasSuffix(c.Path, "/scheduled-events/native-47") && c.Body["name"] != nil {
			return
		}
	}
	t.Errorf("no PATCH carried our name back to Discord: %+v", fake.recorded())
}

// TestUpdateRefusesAnEndBeforeTheStart: Discord refuses such an event, so the
// store must refuse it too rather than hold a row that can never publish.
func TestUpdateRefusesAnEndBeforeTheStart(t *testing.T) {
	store := testStore(t)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, EndsAt: start + 6*3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	moved := start + 86400
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &moved}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("err = %v, want ErrInvalidEvent for a start moved past the end", err)
	}
}

// TestApiEditMovingTheStartMovesTheEnd: every edit surface shares
// applyEventEdit, so a PATCH naming only a start keeps the event's length.
func TestApiEditMovingTheStartMovesTheEnd(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, EndsAt: start + 6*3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	moved := start + 86400
	after, _, err := srv.applyEventEdit(ev, EventPatch{StartsAt: &moved}, "api")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if after.EndsAt != moved+6*3600 {
		t.Errorf("ends_at = %d, want %d — six hours after the moved start", after.EndsAt, moved+6*3600)
	}
}

func ptrInt64(v int64) *int64 { return &v }
