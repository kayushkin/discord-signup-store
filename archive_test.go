package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func timedEvent(t *testing.T, store *Store, e Event) *Event {
	t.Helper()
	if e.GuildID == "" {
		e.GuildID = "g1"
	}
	if e.ChannelID == "" {
		e.ChannelID = "c1"
	}
	if e.Name == "" {
		e.Name = "Timed event"
	}
	created, err := store.CreateEvent(e)
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return created
}

func TestFinishedEventsMoveToTheArchive(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-3 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{StartsAt: past - 3600, EndsAt: past})

	finished, err := store.CompleteFinishedEvents()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(finished) != 1 || finished[0] != ev.ID {
		t.Fatalf("finished = %v, want [%d]", finished, ev.ID)
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", got.Status, StatusCompleted)
	}
	if !IsArchived(got.Status) {
		t.Error("a completed event is not being archived")
	}
}

func TestUpcomingEventsAreLeftAlone(t *testing.T) {
	store := testStore(t)
	future := time.Now().Add(48 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{StartsAt: future, EndsAt: future + 3600})

	finished, err := store.CompleteFinishedEvents()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(finished) != 0 {
		t.Fatalf("archived %d upcoming events", len(finished))
	}
	got, _ := store.GetEvent(ev.ID)
	if got.Status != StatusOpen {
		t.Errorf("status = %q, want %q", got.Status, StatusOpen)
	}
}

// TestEventWithNoEndTimeFinishesAfterTheAssumedRunTime pins the one guess in
// this logic, so a change to it is a deliberate change and not a surprise.
func TestEventWithNoEndTimeFinishesAfterTheAssumedRunTime(t *testing.T) {
	store := testStore(t)
	nowUnix := time.Now().Unix()

	justStarted := timedEvent(t, store, Event{Name: "still on", StartsAt: nowUnix - 60})
	longOver := timedEvent(t, store, Event{
		Name: "long over", StartsAt: nowUnix - assumedRunTimeWithoutEndTime - 60,
	})

	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got, _ := store.GetEvent(justStarted.ID); got.Status != StatusOpen {
		t.Errorf("an event that started a minute ago was archived (status %q)", got.Status)
	}
	if got, _ := store.GetEvent(longOver.ID); got.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", got.Status, StatusCompleted)
	}
}

// TestCancelledEventsAreNotRelabelledAsCompleted keeps the record honest. A
// cancelled event did not happen, and saying it completed would be a lie that
// outlives everyone who remembers otherwise.
func TestCancelledEventsAreNotRelabelledAsCompleted(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-10 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{StartsAt: past, EndsAt: past + 3600})

	cancelled := StatusCancelled
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &cancelled}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want %q — it did not happen", got.Status, StatusCancelled)
	}
	if !IsArchived(got.Status) {
		t.Error("a cancelled event should still be archived")
	}
}

// TestRecurringEventsSurviveTheSweep stops a whole series disappearing because
// one occurrence is over: the store's sweep completes only one-off events.
func TestRecurringEventsSurviveTheSweep(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-30 * 24 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{
		StartsAt: past, EndsAt: past + 3600,
		RecurrenceRule: "FREQ=WEEKLY", Timezone: "America/Los_Angeles",
	})

	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.Status != StatusOpen {
		t.Errorf("status = %q, want %q — the series comes round again", got.Status, StatusOpen)
	}
}

// TestFinishedEventsRefuseNewSignups is what the archive is actually for.
func TestFinishedEventsRefuseNewSignups(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-10 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{StartsAt: past, EndsAt: past + 3600, Capacity: 10})

	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := store.Join(ev.ID, "latecomer", "", JoinedViaButton); err == nil {
		t.Error("joined an event that already happened")
	}
	// And the same through Discord's Interested button, which is a separate
	// code path and would otherwise be a way around it.
	result, err := store.MarkInterested(ev.ID, "alice", "Alice")
	if err != nil {
		t.Fatalf("mark interested: %v", err)
	}
	if result.Outcome != OutcomeEventClosed {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeEventClosed)
	}
}

// TestFinishedForumCardLosesItsButtons stops a past event still taking signups from
// a message sitting in the channel.
func TestFinishedForumCardLosesItsButtons(t *testing.T) {
	ev := &Event{ID: 1, Name: "Over", Status: StatusCompleted, Capacity: 5, AttendingCount: 5}
	payload := RenderForumCard(ev, nil)
	components, ok := payload["components"].([]any)
	if !ok {
		t.Fatal("components key missing — omitting it leaves the old buttons live")
	}
	if len(components) != 0 {
		t.Errorf("a finished event still shows %d component rows", len(components))
	}
	if content, _ := payload["content"].(string); !strings.Contains(content, "finished") {
		t.Errorf("content = %q, want it to say the event has finished", content)
	}
}

func TestSplitByArchivedOrdersEachListUsefully(t *testing.T) {
	soon := time.Now().Add(time.Hour).Unix()
	later := time.Now().Add(72 * time.Hour).Unix()
	old := time.Now().Add(-72 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()

	live, archived := splitByArchived([]Event{
		{ID: 1, Status: StatusOpen, StartsAt: later},
		{ID: 2, Status: StatusCompleted, StartsAt: old},
		{ID: 3, Status: StatusOpen, StartsAt: soon},
		{ID: 4, Status: StatusCancelled, StartsAt: recent},
		{ID: 5, Status: StatusOpen, StartsAt: 0},
		{ID: 6, Status: StatusClosed, StartsAt: soon + 1},
	})

	var liveIDs []int64
	for _, e := range live {
		liveIDs = append(liveIDs, e.ID)
	}
	// Soonest first; the undated one last, because unknown is not imminent.
	// The closed-but-upcoming event stays in the live list on purpose.
	want := []int64{3, 6, 1, 5}
	if len(liveIDs) != len(want) {
		t.Fatalf("live = %v, want %v", liveIDs, want)
	}
	for i := range want {
		if liveIDs[i] != want[i] {
			t.Errorf("live = %v, want %v", liveIDs, want)
			break
		}
	}
	if len(archived) != 2 || archived[0].ID != 4 || archived[1].ID != 2 {
		t.Errorf("archived should be most-recent-first, got %v", archived)
	}
}

// TestClosedEventsStayInTheLiveList pins a decision that is easy to undo by
// accident, because "closed" sounds like it belongs with "completed".
//
// It does not. Closed means signups are shut on an event that has not happened
// yet. Archiving it would hide something that is still coming up, from the
// people most likely to be asking about it — which is the opposite of what the
// archive is for.
func TestClosedEventsStayInTheLiveList(t *testing.T) {
	if IsArchived(StatusClosed) {
		t.Fatal("closed is being archived; it means signups are shut, not that the event is over")
	}
	for _, status := range []string{StatusCompleted, StatusCancelled} {
		if !IsArchived(status) {
			t.Errorf("%q should be archived", status)
		}
	}
	if IsArchived(StatusOpen) {
		t.Error("open is being archived")
	}

	soon := time.Now().Add(time.Hour).Unix()
	live, archived := splitByArchived([]Event{
		{ID: 1, Status: StatusClosed, StartsAt: soon},
		{ID: 2, Status: StatusCompleted, StartsAt: soon},
	})
	if len(live) != 1 || live[0].ID != 1 {
		t.Errorf("the closed event should be live, got %v", live)
	}
	if len(archived) != 1 || archived[0].ID != 2 {
		t.Errorf("only the completed event should be archived, got %v", archived)
	}
}

// TestTheSweepNeverClosesAnUpcomingEventItself is the other half of the same
// decision: closed is a state a person chooses, never one the sweep produces.
// If the sweep ever started closing things on its own, the live list would fill
// with events nobody shut.
func TestTheSweepNeverClosesAnUpcomingEventItself(t *testing.T) {
	store := testStore(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	ev := timedEvent(t, store, Event{StartsAt: future, EndsAt: future + 3600})

	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != StatusOpen {
		t.Errorf("status = %q, want %q — the sweep only ever completes, never closes",
			got.Status, StatusOpen)
	}
}

// TestFinishedEventLeavesOneLineInPastEvents: the table row folded flat, with
// nothing to press, and nothing deleted anywhere — there is no board card to
// take down any more.
func TestFinishedEventLeavesOneLineInPastEvents(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	srv.SetPastChannelID("past")

	past := time.Now().Add(-10 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Done", Location: "The shed",
		StartsAt: past, EndsAt: past + 3600, Capacity: 4,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}

	finished, err := srv.CompleteFinishedEvents()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(finished) != 1 {
		t.Fatalf("finished %d events, want 1", len(finished))
	}

	var posted []recordedCall
	for _, c := range fake.recorded() {
		if c.Method == http.MethodDelete {
			t.Errorf("deleted %s; a finished event has nothing to take down", c.Path)
		}
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/messages") {
			posted = append(posted, c)
		}
	}
	if len(posted) != 1 || posted[0].Path != "/channels/past/messages" {
		t.Fatalf("posted to %v, want exactly one line in the past-events channel", posted)
	}
	line, _ := posted[0].Body["content"].(string)
	for _, want := range []string{"Done", "The shed", "1/4", "Alice"} {
		if !strings.Contains(line, want) {
			t.Errorf("past-events line = %q, want %q in it", line, want)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("past-events line = %q, want one line", line)
	}
	if _, has := posted[0].Body["components"]; has {
		t.Error("the past-events line carries components; there is nothing to press")
	}
	if got, _ := store.GetEvent(ev.ID); got.ChannelID != "past" {
		t.Errorf("event channel = %q, want it repointed at past-events", got.ChannelID)
	}
}

// TestTheOriginalCardSurvivesAFailedMove pins the ordering. Post first, delete
// second: the other way round deletes the only copy and then finds out the
// destination is unreachable.
func TestTheOriginalCardSurvivesAFailedMove(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	srv.SetPastChannelID("past")
	fake.on(http.MethodPost, "/channels/past/messages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":50013,"message":"Missing Permissions"}`)
	})

	past := time.Now().Add(-10 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", MessageID: "card-on-board",
		Name: "Done", StartsAt: past, EndsAt: past + 3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for _, c := range fake.recorded() {
		if c.Method == http.MethodDelete {
			t.Fatalf("the original card was deleted after the move failed: %s", c.Path)
		}
	}
	got, _ := store.GetEvent(ev.ID)
	if got.ChannelID != "board" || got.MessageID != "card-on-board" {
		t.Errorf("card moved to %s/%s despite the post failing", got.ChannelID, got.MessageID)
	}
	// The event is still archived in the store; only the tidying failed.
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want %q — a failed move must not block the archive",
			got.Status, StatusCompleted)
	}
}
