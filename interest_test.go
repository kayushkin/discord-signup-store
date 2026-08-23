package discordsignup

import "testing"

// TestPressingLeaveBeatsALingeringInterested is the behaviour this whole file
// exists for.
//
// Discord has no endpoint to remove a subscriber, so someone who marks
// Interested and then presses Leave on the signup message stays Interested on
// Discord permanently. Every gateway reconnect and every reconciliation re-sees
// that RSVP. If it counted as fresh intent, the person would be silently put
// back on a roster they chose to leave, over and over.
func TestPressingLeaveBeatsALingeringInterested(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	if _, err := store.MarkInterested(ev.ID, "alice", "Alice"); err != nil {
		t.Fatalf("mark interested: %v", err)
	}
	if _, err := store.Leave(ev.ID, "alice", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}

	// Discord still lists her, so the signal arrives again.
	result, err := store.MarkInterested(ev.ID, "alice", "Alice")
	if err != nil {
		t.Fatalf("second mark interested: %v", err)
	}
	if result.Outcome != OutcomeRespectedWithdrawal {
		t.Errorf("outcome = %q, want %q — she left on purpose",
			result.Outcome, OutcomeRespectedWithdrawal)
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AttendingCount != 0 {
		t.Errorf("attending = %d, want 0 — she was put back on a roster she left",
			got.AttendingCount)
	}
}

// TestUnmarkingThenRemarkingInterestedIsFreshIntent is the other half. The
// stickiness must not be permanent, or someone who genuinely changes their mind
// on Discord can never get back on.
func TestUnmarkingThenRemarkingInterestedIsFreshIntent(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	if _, err := store.MarkInterested(ev.ID, "alice", "Alice"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := store.Leave(ev.ID, "alice", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}
	// She un-marks Interested on Discord. That is a real action, and it clears
	// the leftover signal.
	if _, err := store.MarkNotInterested(ev.ID, "alice"); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	// Now marking again means something.
	result, err := store.MarkInterested(ev.ID, "alice", "Alice")
	if err != nil {
		t.Fatalf("remark: %v", err)
	}
	if result.Outcome != OutcomeJoined {
		t.Errorf("outcome = %q, want %q — re-marking after un-marking is a new decision",
			result.Outcome, OutcomeJoined)
	}
}

// TestUnmarkingInterestedDoesNotEvictSomeoneWhoPressedJoin keeps the two
// signals independent. Turning off a notification is not forfeiting a seat.
func TestUnmarkingInterestedDoesNotEvictSomeoneWhoPressedJoin(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	// She also marked Interested at some point, then took it off.
	if _, err := store.MarkInterested(ev.ID, "alice", "Alice"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	result, err := store.MarkNotInterested(ev.ID, "alice")
	if err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if result.Outcome != OutcomeKeptPlace {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeKeptPlace)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.AttendingCount != 1 {
		t.Errorf("attending = %d, want 1 — she pressed Join, which stands on its own",
			got.AttendingCount)
	}
}

// TestUnmarkingInterestedRemovesSomeoneWhoOnlyEverMarkedInterested is the
// mirror: if Interested is the only way they got on, it is the way they get off.
func TestUnmarkingInterestedRemovesSomeoneWhoOnlyEverMarkedInterested(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	if _, err := store.MarkInterested(ev.ID, "alice", "Alice"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := store.MarkInterested(ev.ID, "bob", "Bob"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	result, err := store.MarkNotInterested(ev.ID, "alice")
	if err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if result.Outcome != OutcomeLeft {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeLeft)
	}
	if result.Promoted == nil || result.Promoted.DiscordUserID != "bob" {
		t.Error("bob should have been promoted into the place alice gave up")
	}
}

// TestInterestedRespectsTheCap proves Interested is not a way around a full
// event — it lands on the waitlist exactly as the Join button does.
func TestInterestedRespectsTheCap(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 1)

	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	result, err := store.MarkInterested(ev.ID, "bob", "Bob")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if result.Outcome != OutcomeWaitlisted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeWaitlisted)
	}
	if result.Signup.WaitlistPlace != 1 {
		t.Errorf("waitlist place = %d, want 1", result.Signup.WaitlistPlace)
	}
}

// TestRepeatedInterestedSignalsAreIdempotent covers gateway reconnects, which
// replay events. A duplicate must not cost a position or write history.
func TestRepeatedInterestedSignalsAreIdempotent(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	for i := 0; i < 4; i++ {
		result, err := store.MarkInterested(ev.ID, "alice", "Alice")
		if err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
		if i > 0 && result.Outcome != OutcomeAlreadyOn {
			t.Errorf("repeat %d outcome = %q, want %q", i, result.Outcome, OutcomeAlreadyOn)
		}
	}
	history, err := store.History(ev.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("history has %d rows, want 1 — a replayed gateway event is not a new signup",
			len(history))
	}
}

// TestInterestedIsDistinguishableInTheHistory keeps the two surfaces apart in
// the record, because they behave differently and a question will turn on it.
func TestInterestedIsDistinguishableInTheHistory(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 5)

	if _, err := store.MarkInterested(ev.ID, "alice", "Alice"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := store.Join(ev.ID, "bob", "Bob", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	history, err := store.History(ev.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	actors := map[string]string{}
	for _, tr := range history {
		actors[tr.DiscordUserID] = tr.Actor
	}
	if actors["alice"] != ActorInterested {
		t.Errorf("alice's actor = %q, want %q", actors["alice"], ActorInterested)
	}
	if actors["bob"] != ActorUser {
		t.Errorf("bob's actor = %q, want %q", actors["bob"], ActorUser)
	}
}
