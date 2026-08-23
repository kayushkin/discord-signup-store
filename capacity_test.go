package discordsignup

import (
	"fmt"
	"sync"
	"testing"
)

// TestRaisingTheLimitPromotesInArrivalOrder is the behaviour that makes the
// limit control usable at all. Without it, raising a cap leaves people queued
// in front of empty places until an attendee happens to drop out.
func TestRaisingTheLimitPromotesInArrivalOrder(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	// Descending ids on purpose: if anything ever sorted by user id instead of
	// arrival, "zoe" would come off the waitlist before "alice".
	for _, user := range []string{"zoe", "mike", "alice", "brian"} {
		if _, err := store.Join(ev.ID, user, user, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", user, err)
		}
	}
	three := 3
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Capacity: &three}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	promoted, err := store.PromoteToFillCapacity(ev.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 2 {
		t.Fatalf("promoted %d, want 2 (one place was already taken)", len(promoted))
	}
	if promoted[0].DiscordUserID != "mike" || promoted[1].DiscordUserID != "alice" {
		t.Errorf("promoted %s then %s, want mike then alice — arrival order",
			promoted[0].DiscordUserID, promoted[1].DiscordUserID)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.AttendingCount != 3 || got.WaitlistCount != 1 {
		t.Errorf("%d attending / %d waiting, want 3 / 1", got.AttendingCount, got.WaitlistCount)
	}
}

// TestRemovingTheLimitAdmitsEveryoneWaiting covers capacity 0.
func TestRemovingTheLimitAdmitsEveryoneWaiting(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)
	for i := 0; i < 12; i++ {
		if _, err := store.Join(ev.ID, fmt.Sprintf("user%02d", i), "", JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	unlimited := 0
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Capacity: &unlimited}); err != nil {
		t.Fatalf("clear limit: %v", err)
	}
	promoted, err := store.PromoteToFillCapacity(ev.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 11 {
		t.Errorf("promoted %d, want 11", len(promoted))
	}
	got, _ := store.GetEvent(ev.ID)
	if got.WaitlistCount != 0 {
		t.Errorf("%d still waiting under no limit", got.WaitlistCount)
	}
}

// TestLoweringTheLimitPromotesNobody guards the other direction. Lowering frees
// nothing, and a promotion would be admitting someone to an event that just got
// smaller.
func TestLoweringTheLimitPromotesNobody(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)
	for i := 0; i < 8; i++ {
		if _, err := store.Join(ev.ID, fmt.Sprintf("user%d", i), "", JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	two := 2
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Capacity: &two}); err != nil {
		t.Fatalf("lower: %v", err)
	}
	promoted, err := store.PromoteToFillCapacity(ev.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 0 {
		t.Errorf("promoted %d after LOWERING the limit", len(promoted))
	}
	got, _ := store.GetEvent(ev.ID)
	if got.AttendingCount != 5 {
		t.Errorf("attending = %d, want 5 — nobody already in may be removed", got.AttendingCount)
	}
}

// TestClosedEventsDoNotPromote stops someone being told they got a place at an
// event that is not taking any.
func TestClosedEventsDoNotPromote(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)
	for _, u := range []string{"alice", "bob"} {
		if _, err := store.Join(ev.ID, u, u, JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	closed, ten := StatusClosed, 10
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &closed, Capacity: &ten}); err != nil {
		t.Fatalf("close: %v", err)
	}
	promoted, err := store.PromoteToFillCapacity(ev.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 0 {
		t.Errorf("promoted %d into a closed event", len(promoted))
	}
}

// TestConcurrentRaisesDoNotOverfill covers two people pressing the limit button
// at the same moment, which is the same race Join has and needs the same fix.
func TestConcurrentRaisesDoNotOverfill(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)
	for i := 0; i < 20; i++ {
		if _, err := store.Join(ev.ID, fmt.Sprintf("user%02d", i), "", JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	five := 5
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Capacity: &five}); err != nil {
		t.Fatalf("raise: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := store.PromoteToFillCapacity(ev.ID); err != nil {
				t.Errorf("promote: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	got, _ := store.GetEvent(ev.ID)
	if got.AttendingCount != 5 {
		t.Errorf("attending = %d, want exactly 5 — concurrent raises overfilled the event",
			got.AttendingCount)
	}
}

func TestParseCapacity(t *testing.T) {
	valid := map[string]int{
		"20": 20, " 20 ": 20, "0": 0, "": 0,
		"none": 0, "no limit": 0, "unlimited": 0, "Any": 0, "-": 0,
		"20 places": 20, "12 people": 12, "1": 1,
	}
	for input, want := range valid {
		got, err := ParseCapacity(input)
		if err != nil {
			t.Errorf("ParseCapacity(%q) errored: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCapacity(%q) = %d, want %d", input, got, want)
		}
	}
	for _, input := range []string{"twenty", "-5", "1e6", "20.5", "999999999", "🙂"} {
		if _, err := ParseCapacity(input); err == nil {
			t.Errorf("ParseCapacity(%q) was accepted", input)
		}
	}
}

// TestCapacityButtonIsOnTheCard stops the control quietly disappearing.
func TestCapacityButtonIsOnTheCard(t *testing.T) {
	ev := &Event{ID: 7, Name: "Open one", Status: StatusOpen, Capacity: 5}
	rows, _ := RenderSignupMessage(ev, nil)["components"].([]any)
	if len(rows) == 0 {
		t.Fatal("no components on an open event")
	}
	row, _ := rows[0].(map[string]any)
	buttons, _ := row["components"].([]any)
	var ids []string
	for _, b := range buttons {
		m, _ := b.(map[string]any)
		id, _ := m["custom_id"].(string)
		ids = append(ids, id)
	}
	want := []string{JoinCustomID(7), LeaveCustomID(7), CapacityCustomID(7)}
	if len(ids) != len(want) {
		t.Fatalf("buttons = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("button %d = %q, want %q", i, ids[i], want[i])
		}
	}
}
