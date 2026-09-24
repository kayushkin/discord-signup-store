package discordsignup

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// markPublished records the event as fully written to Discord, as a clean
// publish does.
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

// recordWrittenAsOurs records the native event as holding exactly the row's
// current values, as a successful publish leaves it.
func recordWrittenAsOurs(t *testing.T, store *Store, eventID int64) {
	t.Helper()
	ev, err := store.GetEvent(eventID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	endsAt := ev.EndsAt
	if endsAt == 0 {
		endsAt = ev.StartsAt + assumedRunTimeWithoutEndTime
	}
	if err := store.RecordNativeWrite(eventID, NativeWrite{
		Name: &ev.Name, Description: &ev.Description,
		StartsAt: &ev.StartsAt, EndsAt: &endsAt, Location: &ev.Location,
	}); err != nil {
		t.Fatalf("record native write: %v", err)
	}
}

func nativeEventAt(id, name string, startsAt, endsAt int64) DiscordScheduledEvent {
	return DiscordScheduledEvent{
		ID: id, GuildID: "g1", Name: name, Status: discordEventScheduled,
		ScheduledStartTime: time.Unix(startsAt, 0).UTC().Format(time.RFC3339),
		ScheduledEndTime:   time.Unix(endsAt, 0).UTC().Format(time.RFC3339),
	}
}

func patchesTo(fake *fakeDiscord, nativeID string) []recordedCall {
	var out []recordedCall
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && strings.HasSuffix(c.Path, "/scheduled-events/"+nativeID) {
			out = append(out, c)
		}
	}
	return out
}

// TestSyncPushesAnEditDiscordHasNotTaken: on 2026-09-24 an organiser moved an
// event a day later, Discord refused the push, and the sync copied Discord's
// old start back over the edit. Discord still holding what we last wrote
// means ours has not reached it — so ours goes out, and ours stays.
func TestSyncPushesAnEditDiscordHasNotTaken(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, EndsAt: start + 6*3600, DiscordScheduledEventID: "native-45",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recordWrittenAsOurs(t, store, ev.ID)

	moved := start + 86400
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &moved, EndsAt: ptrInt64(moved + 6*3600)}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, _, err := srv.syncOneScheduledEvent(nativeEventAt("native-45", "Roller skate", start, start+6*3600), "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.StartsAt != moved {
		t.Errorf("starts_at = %d, want ours %d — Discord's stale copy was taken as an edit", after.StartsAt, moved)
	}
	pushed := patchesTo(fake, "native-45")
	if len(pushed) == 0 || pushed[0].Body["scheduled_start_time"] != time.Unix(moved, 0).UTC().Format(time.RFC3339) {
		t.Errorf("patches = %+v, want our start pushed", pushed)
	}
	if after.NativeWritten.StartsAt != moved {
		t.Errorf("recorded start = %d, want %d after the push", after.NativeWritten.StartsAt, moved)
	}
}

// TestSyncTakesARenameMadeInDiscord: a rename in Discord's own event screen is
// an edit, recorded in the history as made there.
func TestSyncTakesARenameMadeInDiscord(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-47", Origin: OriginDiscord,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recordWrittenAsOurs(t, store, ev.ID)

	if _, _, err := srv.syncOneScheduledEvent(
		nativeEventAt("native-47", "Ice skate", start, start+assumedRunTimeWithoutEndTime), "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.Name != "Ice skate" {
		t.Errorf("name = %q, want Discord's edit", after.Name)
	}
	updates, _ := store.EventUpdates(ev.ID)
	for _, u := range updates {
		if u.Field == "name" && u.Actor == discordEventScreenActor {
			return
		}
	}
	t.Errorf("history = %+v, want a name row by %q", updates, discordEventScreenActor)
}

// TestADiscordEditAndAnUnsentOneBothSurvive: Discord's rename is taken and our
// own unsent start is kept — each field on its own evidence.
func TestADiscordEditAndAnUnsentOneBothSurvive(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-48",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recordWrittenAsOurs(t, store, ev.ID)
	moved := start + 86400
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &moved}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if _, _, err := srv.syncOneScheduledEvent(
		nativeEventAt("native-48", "Ice skate", start, start+assumedRunTimeWithoutEndTime), "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.Name != "Ice skate" || after.StartsAt != moved {
		t.Errorf("event = %q at %d, want Discord's name and our start %d", after.Name, after.StartsAt, moved)
	}
}

// TestSyncRecordsWhatItAgreesWith: an event from before the record existed,
// where both sides agree, gets its record on the first sync — so the next
// edit made in Discord is recognised as one.
func TestSyncRecordsWhatItAgreesWith(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-49",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	native := nativeEventAt("native-49", "Roller skate", start, start+assumedRunTimeWithoutEndTime)
	if _, _, err := srv.syncOneScheduledEvent(native, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(patchesTo(fake, "native-49")) != 0 {
		t.Errorf("pushed to Discord though both sides agreed")
	}
	after, _ := store.GetEvent(ev.ID)
	if after.NativeWritten.At == 0 || after.NativeWritten.StartsAt != start {
		t.Errorf("record = %+v, want it seeded from the agreeing copy", after.NativeWritten)
	}
}

// TestSyncRecordsWhatItChanges: a cancellation from Discord shows in the
// event's history under the sync's own actor.
func TestSyncRecordsWhatItChanges(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	start := time.Now().Add(6 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Roller skate",
		StartsAt: start, DiscordScheduledEventID: "native-46",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recordWrittenAsOurs(t, store, ev.ID)
	native := nativeEventAt("native-46", "Roller skate", start, start+assumedRunTimeWithoutEndTime)
	native.Status = discordEventCanceled
	if _, _, err := srv.syncOneScheduledEvent(native, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updates, _ := store.EventUpdates(ev.ID)
	for _, u := range updates {
		if u.Field == "status" && u.ToValue == StatusCancelled && u.Actor == syncEventUpdateActor {
			return
		}
	}
	t.Errorf("history = %+v, want a status row by %q", updates, syncEventUpdateActor)
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
	t.Cleanup(srv.WaitForBackgroundWork)
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
