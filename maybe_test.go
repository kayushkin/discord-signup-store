package discordsignup

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func maybeEvent(t *testing.T, store *Store, capacity int) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games", Status: StatusOpen,
		Capacity: capacity, StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return ev
}

func stateOf(t *testing.T, store *Store, eventID int64, userID string) string {
	t.Helper()
	state, err := store.SignupState(eventID, userID)
	if err != nil {
		t.Fatalf("state of %s: %v", userID, err)
	}
	return state
}

// TestMaybeHoldsNoPlace: a full event still takes Maybes, and they do not
// count against the limit or the line.
func TestMaybeHoldsNoPlace(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 1)
	store.Join(ev.ID, "u-going", "Going", JoinedViaButton)
	if _, err := store.MarkMaybe(ev.ID, "u-maybe", "Maybe", JoinedViaButton); err != nil {
		t.Fatalf("maybe on a full event: %v", err)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.AttendingCount != 1 || got.WaitlistCount != 0 {
		t.Errorf("going %d waiting %d, want 1 and 0", got.AttendingCount, got.WaitlistCount)
	}
	roster, _ := store.Roster(ev.ID, false)
	if len(roster) != 2 || roster[1].State != StateMaybe {
		t.Errorf("roster = %+v, want the Maybe after the going", roster)
	}
	again, err := store.MarkMaybe(ev.ID, "u-maybe", "Maybe", JoinedViaButton)
	if err != nil || !again.AlreadyMaybe {
		t.Errorf("second Maybe = %+v, %v; want already", again, err)
	}
}

// TestJoinFromMaybeIsAnOrdinaryJoin: a place if there is one, the waitlist
// if not.
func TestJoinFromMaybeIsAnOrdinaryJoin(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 1)
	store.MarkMaybe(ev.ID, "u-a", "A", JoinedViaButton)
	store.MarkMaybe(ev.ID, "u-b", "B", JoinedViaButton)
	first, err := store.Join(ev.ID, "u-a", "A", JoinedViaButton)
	if err != nil || first.AlreadySignedUp || first.Signup.State != StateAttending {
		t.Fatalf("maybe → join with room = %+v, %v", first, err)
	}
	second, err := store.Join(ev.ID, "u-b", "B", JoinedViaButton)
	if err != nil || second.Signup.State != StateWaitlisted || second.Signup.WaitlistPlace != 1 {
		t.Fatalf("maybe → join when full = %+v, %v", second, err)
	}
}

// TestGoingToMaybeGivesUpThePlace, and the person waiting longest gets it.
func TestGoingToMaybeGivesUpThePlace(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 1)
	store.Join(ev.ID, "u-going", "Going", JoinedViaButton)
	store.Join(ev.ID, "u-waiting", "Waiting", JoinedViaButton)
	result, err := store.MarkMaybe(ev.ID, "u-going", "Going", JoinedViaButton)
	if err != nil {
		t.Fatalf("maybe: %v", err)
	}
	if result.FromState != StateAttending || result.Promoted == nil || result.Promoted.DiscordUserID != "u-waiting" {
		t.Fatalf("result = %+v, want the waiting person promoted", result)
	}
	if s := stateOf(t, store, ev.ID, "u-waiting"); s != StateAttending {
		t.Errorf("waiting person is %s, want attending", s)
	}
	// Waitlisted to Maybe leaves the line and promotes nobody.
	store.Join(ev.ID, "u-late", "Late", JoinedViaButton)
	result, err = store.MarkMaybe(ev.ID, "u-late", "Late", JoinedViaButton)
	if err != nil || result.FromState != StateWaitlisted || result.Promoted != nil {
		t.Errorf("waitlisted → maybe = %+v, %v", result, err)
	}
	// Leave takes them off the Maybe list.
	left, err := store.Leave(ev.ID, "u-late", ActorUser)
	if err != nil || left.FromState != StateMaybe || left.Promoted != nil {
		t.Errorf("leave from maybe = %+v, %v", left, err)
	}
}

// TestAClosedEventTakesNoMaybe, as it takes no Join.
func TestAClosedEventTakesNoMaybe(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 0)
	closed := StatusClosed
	store.UpdateEvent(ev.ID, EventPatch{Status: &closed})
	if _, err := store.MarkMaybe(ev.ID, "u-a", "A", JoinedViaButton); !errors.Is(err, ErrEventNotOpen) {
		t.Errorf("err = %v, want ErrEventNotOpen", err)
	}
}

// TestInterestedFromMaybeIsGoing: Discord's Interested means going.
func TestInterestedFromMaybeIsGoing(t *testing.T) {
	store := testStore(t)
	ev := maybeEvent(t, store, 0)
	store.MarkMaybe(ev.ID, "u-a", "A", JoinedViaButton)
	if _, err := store.MarkInterested(ev.ID, "u-a", "A"); err != nil {
		t.Fatalf("interested: %v", err)
	}
	if s := stateOf(t, store, ev.ID, "u-a"); s != StateAttending {
		t.Errorf("state = %s, want attending", s)
	}
}

// TestTheTableNamesEachList.
func TestTheTableNamesEachList(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Status: StatusOpen, Capacity: 2, AttendingCount: 2}
	roster := []Signup{
		{DiscordUserID: "1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "2", DisplayName: "Bo", State: StateAttending},
		{DiscordUserID: "3", DisplayName: "Cy", State: StateWaitlisted, WaitlistPlace: 1},
		{DiscordUserID: "4", DisplayName: "Di", State: StateMaybe},
	}
	text := buildEventTableBlock(ev, roster, true, eventTableButtons).text
	for _, want := range []string{"\n(2/2) **Going** ✅ Al, Bo", "\n**Maybe** 🤷 Di", "\n**Waitlist** ❌ Cy"} {
		if !strings.Contains(text, want) {
			t.Errorf("row = %q, want %q", text, want)
		}
	}
	if strings.Contains(buildEventTableBlock(ev, roster[:2], true, eventTableButtons).text, "Maybe:") {
		t.Error("an empty Maybe list still has a line")
	}
}

// TestTheMaybeButtonAnswersAndPromotes, through the press.
func TestTheMaybeButtonAnswersAndPromotes(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev := maybeEvent(t, store, 1)
	store.Join(ev.ID, "u-a", "A", JoinedViaButton)
	store.Join(ev.ID, "u-b", "B", JoinedViaButton)
	action, id, ok := parseCustomID(MaybeCustomID(ev.ID))
	if !ok || action != "maybe" || id != ev.ID {
		t.Fatalf("custom id parses as %q %d %v", action, id, ok)
	}
	rec := httptest.NewRecorder()
	srv.handleMaybe(rec, pressBy(t, "u-a", 0), ev.ID, "u-a", "A")
	if !strings.Contains(replyText(rec), "place has been given up") {
		t.Errorf("reply = %q", replyText(rec))
	}
	if s := stateOf(t, store, ev.ID, "u-b"); s != StateAttending {
		t.Errorf("the waiting person is %s, want attending", s)
	}
}
