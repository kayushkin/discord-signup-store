package discordsignup

import (
	"testing"
	"time"
)

// TestAnEditIsRecordedWithWhoMadeIt is the question nobody could answer before:
// an event's name, time, place and limit could all change and the only trace
// was the new value.
func TestAnEditIsRecordedWithWhoMadeIt(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	starts := time.Now().Add(48 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 4,
		Status: StatusOpen, StartsAt: starts, Location: "The shed",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newName, newCapacity := "Board game night", 8
	if _, _, err := srv.applyEventEdit(ev,
		EventPatch{Name: &newName, Capacity: &newCapacity}, "web:u-organiser"); err != nil {
		t.Fatalf("edit: %v", err)
	}

	updates, err := store.EventUpdates(ev.ID)
	if err != nil {
		t.Fatalf("read updates: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("%d updates recorded, want one per changed field: %+v", len(updates), updates)
	}
	byField := map[string]EventUpdate{}
	for _, u := range updates {
		byField[u.Field] = u
	}
	if got := byField["name"]; got.FromValue != "Games" || got.ToValue != "Board game night" {
		t.Errorf("name update = %q -> %q", got.FromValue, got.ToValue)
	}
	if got := byField["capacity"]; got.FromValue != "4" || got.ToValue != "8" {
		t.Errorf("capacity update = %q -> %q", got.FromValue, got.ToValue)
	}
	for field, u := range byField {
		if u.Actor != "web:u-organiser" {
			t.Errorf("%s recorded actor %q, want the person who did it", field, u.Actor)
		}
	}
}

// TestBookkeepingIsNotAnEdit. A publish signature and a reminder stamp change
// without anybody editing anything, and a history that recorded them would bury
// the rows somebody cares about.
func TestBookkeepingIsNotAnEdit(t *testing.T) {
	store := testStore(t)
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Status: StatusOpen,
		StartsAt: time.Now().Add(48 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPublishedSignature(ev.ID, "deadbeef"); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if err := store.StampReminder(ev.ID, reminderStageStart); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := store.LogEventUpdates(ev, after, "sweep"); err != nil {
		t.Fatalf("log: %v", err)
	}
	updates, err := store.EventUpdates(ev.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("%d updates recorded for bookkeeping alone: %+v", len(updates), updates)
	}
}
