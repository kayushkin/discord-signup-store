package discordsignup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// recurringTestEvent is a weekly event whose current occurrence ended an hour
// ago, with two people on it and a native event and forum post to republish.
func recurringTestEvent(t *testing.T, store *Store, rule string) *Event {
	t.Helper()
	start := time.Now().Add(-3 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 2,
		StartsAt: start, EndsAt: start + 2*3600,
		RecurrenceRule: rule, Timezone: "America/Los_Angeles",
		DiscordScheduledEventID: "native-9",
		AttendingRoleID:         "going",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// CreateEvent does not take a forum post — the publisher opens one — so
	// it is attached the way the publisher attaches it.
	post := "post-9"
	if ev, err = store.UpdateEvent(ev.ID, EventPatch{ForumPostID: &post}); err != nil {
		t.Fatalf("attach post: %v", err)
	}
	for _, who := range []string{"alice", "bob", "carol"} {
		if _, err := store.Join(ev.ID, who, strings.ToUpper(who[:1])+who[1:], JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
	}
	if err := store.StampReminder(ev.ID, reminderStageBefore); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := store.StampReminder(ev.ID, reminderStageStart); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	return ev
}

// TestAFinishedOccurrenceRollsTheEventForward is what recurrence means here:
// the date moves on by the rule, last time's people are off the roster, the
// reminders are owed again, and the occurrence that ran left its line in past
// events.
func TestAFinishedOccurrenceRollsTheEventForward(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	store.SetGuildChannels("g1", GuildChannels{Board: "board", Past: "past"})
	ev := recurringTestEvent(t, store, "FREQ=WEEKLY")

	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	after, _ := store.GetEvent(ev.ID)
	if after.Status != StatusOpen {
		t.Errorf("status = %q, want open", after.Status)
	}
	if want := ev.StartsAt + 7*86400; after.StartsAt != want {
		t.Errorf("starts_at = %d, want %d (a week on)", after.StartsAt, want)
	}
	if want := after.StartsAt + 2*3600; after.EndsAt != want {
		t.Errorf("ends_at = %d, want %d (the same run time)", after.EndsAt, want)
	}
	if after.AttendingCount != 0 || after.WaitlistCount != 0 {
		t.Errorf("roster after rollover = %d attending, %d waiting; want empty",
			after.AttendingCount, after.WaitlistCount)
	}
	if after.RemindedBeforeAt != 0 || after.RemindedStartAt != 0 {
		t.Errorf("reminder stamps survived the rollover: before=%d start=%d",
			after.RemindedBeforeAt, after.RemindedStartAt)
	}

	history, _ := store.History(ev.ID, 100)
	byActor := 0
	for _, h := range history {
		if h.Actor == ActorRecurrence && h.Action == ActionWithdrew {
			byActor++
		}
	}
	if byActor != 3 {
		t.Errorf("%d withdrawals recorded under %q, want 3", byActor, ActorRecurrence)
	}
	edits, _ := store.EventUpdates(ev.ID)
	var dateMoved bool
	for _, e := range edits {
		if e.Field == "starts_at" && e.Actor == ActorRecurrence {
			dateMoved = true
		}
	}
	if !dateMoved {
		t.Error("the date moving was not logged as an edit by the recurrence")
	}

	var pastLines, nativeStarts []string
	var reactionsCleared int
	for _, c := range fake.recorded() {
		switch {
		case c.Method == http.MethodPost && c.Path == "/channels/past/messages":
			pastLines = append(pastLines, c.Body["content"].(string))
		case c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-9":
			nativeStarts = append(nativeStarts, c.Body["scheduled_start_time"].(string))
		case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/channels/post-9/messages/post-9/reactions/"):
			reactionsCleared++
		}
	}
	if len(pastLines) != 1 || !strings.Contains(pastLines[0], "2/2 👥 Alice, Bob") {
		t.Errorf("past-events lines = %q, want one naming Alice and Bob", pastLines)
	}
	if len(nativeStarts) == 0 || nativeStarts[len(nativeStarts)-1] != time.Unix(after.StartsAt, 0).UTC().Format(time.RFC3339) {
		t.Errorf("native event start pushed = %v, want the next occurrence", nativeStarts)
	}
	if reactionsCleared != 3 {
		t.Errorf("%d ✅ reactions cleared, want 3 — last time's people must not look joined", reactionsCleared)
	}
	if after.ChannelID != "board" {
		t.Errorf("channel moved to %q; a rolled event is not over and stays where it is", after.ChannelID)
	}
}

// TestDiscordSlidingTheStartForwardIsARollover: Discord moves a recurring
// event's start itself once an occurrence ends. When the import sees that,
// it is the same rollover, with Discord's date.
func TestDiscordSlidingTheStartForwardIsARollover(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	ev := recurringTestEvent(t, store, "FREQ=WEEKLY")
	next := ev.StartsAt + 7*86400

	changed, imported, err := srv.syncOneScheduledEvent(DiscordScheduledEvent{
		ID: "native-9", GuildID: "g1", Name: "Games", Status: discordEventScheduled,
		ScheduledStartTime: time.Unix(next, 0).UTC().Format(time.RFC3339),
		ScheduledEndTime:   time.Unix(next+2*3600, 0).UTC().Format(time.RFC3339),
	}, "board")
	if err != nil || imported {
		t.Fatalf("sync: changed=%v imported=%v err=%v", changed, imported, err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.StartsAt != next {
		t.Errorf("starts_at = %d, want Discord's %d", after.StartsAt, next)
	}
	if after.AttendingCount != 0 {
		t.Errorf("%d still attending after Discord rolled the date", after.AttendingCount)
	}
}

// TestMovingAFutureDateIsAnEditNotARollover: an organiser pushing next week's
// game to Thursday has not ended anything, and nobody loses their place.
func TestMovingAFutureDateIsAnEditNotARollover(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	start := time.Now().Add(48 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 4,
		StartsAt: start, EndsAt: start + 3600,
		RecurrenceRule: "FREQ=WEEKLY", Timezone: "America/Los_Angeles",
		DiscordScheduledEventID: "native-9",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store.Join(ev.ID, "alice", "Alice", JoinedViaButton)

	moved := start + 2*86400
	if _, _, err := srv.syncOneScheduledEvent(DiscordScheduledEvent{
		ID: "native-9", GuildID: "g1", Name: "Games", Status: discordEventScheduled,
		ScheduledStartTime: time.Unix(moved, 0).UTC().Format(time.RFC3339),
	}, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.StartsAt != moved {
		t.Errorf("starts_at = %d, want the moved %d", after.StartsAt, moved)
	}
	if after.AttendingCount != 1 {
		t.Errorf("Alice lost her place to a date change: %d attending", after.AttendingCount)
	}
}

// TestARecurringEventSaysSoEverywhere: the same short label on the table row,
// the card and Details, and words rather than an RRULE on the web page.
func TestARecurringEventSaysSoEverywhere(t *testing.T) {
	ev := &Event{ID: 7, Name: "Games", Capacity: 4, StartsAt: farFutureStart,
		RecurrenceRule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU", Timezone: "UTC", ForumPostID: "post-7"}
	if got := eventTableHeadline(ev); !strings.Contains(got, "🔁 every 2 weeks") {
		t.Errorf("table headline %q does not say it repeats", got)
	}
	ev.ForumPostID = ""
	if got := eventLine(ev); !strings.Contains(got, "🔁 every 2 weeks") {
		t.Errorf("event line %q does not say it repeats", got)
	}
	card := renderSignupMessage(ev, nil)["content"].(string)
	if !strings.Contains(card, "🔁 every 2 weeks — signups are for this date") {
		t.Errorf("card does not say the roster is per date:\n%s", card)
	}
	details := detailsField(ev, nil, "UTC")["value"].(string)
	if !strings.Contains(details, "🔁 every 2 weeks") {
		t.Errorf("Details does not say it repeats:\n%s", details)
	}
	if got := repeatsLabel(&Event{}); got != "" {
		t.Errorf("a one-off got the label %q", got)
	}
}
