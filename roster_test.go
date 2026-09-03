package discordsignup

import (
	"fmt"
	"sync"
	"testing"
)

// farFutureStart is the start time for an event whose date does not matter to
// the test. Every event needs one — CreateEvent refuses a row without — and
// far enough out that no sweep in the suite can mistake it for finished.
const farFutureStart int64 = 4102444800 // 2100-01-01T00:00:00Z

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testEvent(t *testing.T, store *Store, capacity int) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", StartsAt: farFutureStart, ChannelID: "c1", Name: "Test event", Capacity: capacity,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return ev
}

// TestCapacityHoldsUnderConcurrentJoins is the test this whole service exists
// for. Twenty people press Join on a three-place event at the same moment.
//
// Without BEGIN IMMEDIATE on the DSN, several of them read the same "there is
// room" answer before any of them writes, and the event ends up oversubscribed
// — the exact bug that makes a hand-rolled cap worse than no cap, because the
// people who were told they were in are not.
func TestCapacityHoldsUnderConcurrentJoins(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 3)

	const racers = 20
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			if _, err := store.Join(ev.ID, fmt.Sprintf("user%02d", n), "", JoinedViaButton); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent join failed: %v", err)
	}

	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload event: %v", err)
	}
	if got.AttendingCount != 3 {
		t.Errorf("attending = %d, want exactly 3 (the cap leaked)", got.AttendingCount)
	}
	if got.WaitlistCount != racers-3 {
		t.Errorf("waitlisted = %d, want %d", got.WaitlistCount, racers-3)
	}
}

// TestWaitlistPromotesInArrivalOrder pins the ordering that Discord's own API
// cannot reproduce: the person who signed up first is promoted first, whatever
// their user id sorts like.
func TestWaitlistPromotesInArrivalOrder(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	// Deliberately descending ids. Discord's subscriber endpoint returns users
	// ascending by user_id, so if anything here ever sorted by id instead of by
	// arrival, "zoe" would be promoted before "alice" and this would catch it.
	for _, user := range []string{"zoe", "mike", "alice"} {
		if _, err := store.Join(ev.ID, user, user, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", user, err)
		}
	}

	result, err := store.Leave(ev.ID, "zoe", ActorUser)
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if result.Promoted == nil {
		t.Fatal("nobody was promoted into the freed place")
	}
	if result.Promoted.DiscordUserID != "mike" {
		t.Errorf("promoted %q, want \"mike\" — the one who waited longest",
			result.Promoted.DiscordUserID)
	}
}

// TestDoubleClickKeepsPlaceAndWritesNoHistory covers the button being pressed
// twice, which happens constantly on a message that sits in a channel for days.
func TestDoubleClickKeepsPlaceAndWritesNoHistory(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("first join: %v", err)
	}
	if _, err := store.Join(ev.ID, "bob", "Bob", JoinedViaButton); err != nil {
		t.Fatalf("bob join: %v", err)
	}

	second, err := store.Join(ev.ID, "bob", "Bob", JoinedViaButton)
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	if !second.AlreadySignedUp {
		t.Error("second press was not reported as a no-op")
	}
	if second.Signup.WaitlistPlace != 1 {
		t.Errorf("waitlist place = %d, want 1 — a double click must not cost a position",
			second.Signup.WaitlistPlace)
	}

	history, err := store.History(ev.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("history has %d rows, want 2 — a double click is not an event", len(history))
	}
}

// TestRejoiningGoesToTheBack stops the obvious way to jump a queue.
func TestRejoiningGoesToTheBack(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	for _, user := range []string{"alice", "bob", "carol"} {
		if _, err := store.Join(ev.ID, user, user, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", user, err)
		}
	}
	// bob is first in line. He leaves and immediately rejoins.
	if _, err := store.Leave(ev.ID, "bob", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}
	rejoined, err := store.Join(ev.ID, "bob", "bob", JoinedViaButton)
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if rejoined.Signup.State != StateWaitlisted {
		t.Fatalf("state = %q, want waitlisted", rejoined.Signup.State)
	}
	if rejoined.Signup.WaitlistPlace != 2 {
		t.Errorf("waitlist place = %d, want 2 — behind carol, who did not leave",
			rejoined.Signup.WaitlistPlace)
	}
}

// TestLoweringCapacityDoesNotDemoteAnyone pins the promise made to people who
// were already told they had a place.
func TestLoweringCapacityDoesNotDemoteAnyone(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	for i := 0; i < 5; i++ {
		if _, err := store.Join(ev.ID, fmt.Sprintf("user%d", i), "", JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	smaller := 2
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Capacity: &smaller}); err != nil {
		t.Fatalf("shrink capacity: %v", err)
	}

	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AttendingCount != 5 {
		t.Errorf("attending = %d, want 5 — nobody already admitted may be demoted", got.AttendingCount)
	}

	// The new cap governs the next join, which must now be waitlisted.
	next, err := store.Join(ev.ID, "latecomer", "", JoinedViaButton)
	if err != nil {
		t.Fatalf("join after shrink: %v", err)
	}
	if next.Signup.State != StateWaitlisted {
		t.Errorf("state = %q, want waitlisted — the lower cap must bind new joins", next.Signup.State)
	}
}

// TestUnlimitedCapacityNeverWaitlists checks the 0-means-unlimited convention,
// which matches Discord's own for channel user_limit and invite max_uses.
func TestUnlimitedCapacityNeverWaitlists(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 0)

	for i := 0; i < 50; i++ {
		result, err := store.Join(ev.ID, fmt.Sprintf("user%02d", i), "", JoinedViaButton)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if result.Signup.State != StateAttending {
			t.Fatalf("user %d got %q, want attending — capacity 0 means unlimited",
				i, result.Signup.State)
		}
	}
}

// TestClosedEventRefusesJoins keeps a stale button from admitting people.
func TestClosedEventRefusesJoins(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 10)

	closed := StatusClosed
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &closed}); err != nil {
		t.Fatalf("close event: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "", JoinedViaButton); err == nil {
		t.Fatal("join succeeded on a closed event")
	}
}

// TestHistoryRecordsWhoCausedEachChange covers the reason the signup updates table
// exists: telling an automatic promotion apart from a person's own decision.
func TestHistoryRecordsWhoCausedEachChange(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	if _, err := store.Join(ev.ID, "alice", "", JoinedViaButton); err != nil {
		t.Fatalf("join alice: %v", err)
	}
	if _, err := store.Join(ev.ID, "bob", "", JoinedViaButton); err != nil {
		t.Fatalf("join bob: %v", err)
	}
	if _, err := store.Leave(ev.ID, "alice", "operator"); err != nil {
		t.Fatalf("leave: %v", err)
	}

	history, err := store.History(ev.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var sawOperatorWithdrawal, sawAutomaticPromotion bool
	for _, tr := range history {
		if tr.Action == ActionWithdrew && tr.Actor == "operator" {
			sawOperatorWithdrawal = true
		}
		if tr.Action == ActionPromoted && tr.Actor == ActorPromotion {
			sawAutomaticPromotion = true
		}
	}
	if !sawOperatorWithdrawal {
		t.Error("the operator-caused withdrawal is not distinguishable in the log")
	}
	if !sawAutomaticPromotion {
		t.Error("the automatic promotion is not distinguishable in the log")
	}
}

// TestArrivalOrderHoldsWithinASingleSecond is why the row id is part of the
// ordering and not decoration.
//
// signed_up_at is accurate to the second, and three button presses land inside
// one. Ordering on the timestamp alone would leave those three in whatever
// order SQLite felt like returning them.
func TestArrivalOrderHoldsWithinASingleSecond(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 0)

	want := []string{"alice", "bob", "carol", "dan", "erin"}
	for _, user := range want {
		if _, err := store.Join(ev.ID, user, user, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", user, err)
		}
	}
	roster, err := store.Roster(ev.ID, false)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(roster) != len(want) {
		t.Fatalf("%d on the roster, want %d", len(roster), len(want))
	}
	stamps := map[int64]bool{}
	for i, sg := range roster {
		stamps[sg.SignedUpAt] = true
		if sg.DiscordUserID != want[i] {
			t.Errorf("position %d is %s, want %s", i+1, sg.DiscordUserID, want[i])
		}
	}
	if len(stamps) > 1 {
		t.Logf("note: the joins spanned %d seconds, so this run did not exercise the tie",
			len(stamps))
	}
}

// TestARejoinerGoesBehindEveryoneEvenInTheSameSecond is the case that made a
// delete-and-insert necessary. An updated row keeps its old, lower id, so a
// rejoiner would sort ahead of somebody who never left whenever both landed in
// the same second — which is the common case, since a leave and a rejoin are
// two clicks.
func TestARejoinerGoesBehindEveryoneEvenInTheSameSecond(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 0)

	for _, user := range []string{"alice", "bob", "carol"} {
		if _, err := store.Join(ev.ID, user, user, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", user, err)
		}
	}
	if _, err := store.Leave(ev.ID, "alice", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "alice", JoinedViaButton); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	roster, err := store.Roster(ev.ID, false)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if got := roster[len(roster)-1].DiscordUserID; got != "alice" {
		var order []string
		for _, sg := range roster {
			order = append(order, sg.DiscordUserID)
		}
		t.Errorf("roster order is %v, want alice last", order)
	}
}
