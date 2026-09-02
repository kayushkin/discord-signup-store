package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func reminderEvent(t *testing.T, store *Store, startsIn time.Duration, joiners ...string) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", MessageID: "msg-1", Name: "Board game night",
		Status: StatusOpen, Location: "The shed",
		StartsAt: time.Now().Add(startsIn).Unix(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, who := range joiners {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
	}
	return ev
}

// posted returns every message posted to the board channel.
func posted(fake *fakeDiscord) []recordedCall {
	var out []recordedCall
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/reminders/messages" {
			out = append(out, c)
		}
	}
	return out
}

func reminderServer(t *testing.T) (*Server, *Store, *fakeDiscord) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	srv.SetReminderChannelID("reminders")
	return srv, store, fake
}

// TestBothRemindersGoOutOnceEach is the whole feature: an hour before, and
// again when it starts, to the people who have a place.
func TestBothRemindersGoOutOnceEach(t *testing.T) {
	srv, store, fake := reminderServer(t)
	reminderEvent(t, store, 30*time.Minute, "alice", "bob")

	if sent, err := srv.SendDueReminders(); err != nil || sent != 1 {
		t.Fatalf("sent %d, %v; want the hour-before message", sent, err)
	}
	// Called again a minute later, as the cron does. Nothing more is owed.
	if sent, _ := srv.SendDueReminders(); sent != 0 {
		t.Errorf("sent %d on the second run, want 0 — the stamp did not hold", sent)
	}

	msgs := posted(fake)
	if len(msgs) != 1 {
		t.Fatalf("%d messages posted, want 1", len(msgs))
	}
	content, _ := msgs[0].Body["content"].(string)
	if !strings.Contains(content, "starts in an hour") {
		t.Errorf("content = %q, want the hour-before wording", content)
	}
	for _, who := range []string{"alice", "bob"} {
		if !strings.Contains(content, "<@"+who+">") {
			t.Errorf("content = %q, want %s mentioned", content, who)
		}
	}
	// The one deliberate ping in the whole service, and it names ids rather
	// than parsing the text, so it cannot ping anybody the roster does not hold.
	mentions, ok := msgs[0].Body["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatal("no allowed_mentions on a reminder, so nobody is actually pinged")
	}
	if _, parses := mentions["parse"]; parses {
		t.Error("allowed_mentions uses parse, which pings whatever looks like a mention")
	}
	if users, _ := mentions["users"].([]any); len(users) != 2 {
		t.Errorf("allowed_mentions.users = %v, want the two people with places", mentions["users"])
	}
}

// TestTheStartingReminderFiresWhenItStarts.
func TestTheStartingReminderFiresWhenItStarts(t *testing.T) {
	srv, store, fake := reminderServer(t)
	reminderEvent(t, store, -2*time.Minute, "alice")

	sent, err := srv.SendDueReminders()
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// The hour-before moment went by unsent, so only the starting one goes.
	if sent != 1 {
		t.Fatalf("sent %d, want 1", sent)
	}
	content, _ := posted(fake)[0].Body["content"].(string)
	if !strings.Contains(content, "is starting now") {
		t.Errorf("content = %q, want the starting wording", content)
	}
	if sent, _ := srv.SendDueReminders(); sent != 0 {
		t.Errorf("sent %d on a second run, want 0", sent)
	}
}

// TestAMissedReminderIsDroppedNotSentLate is the one that matters after an
// outage. Coming back up to find yesterday's events must not ping everybody
// about things that already happened.
func TestAMissedReminderIsDroppedNotSentLate(t *testing.T) {
	srv, store, fake := reminderServer(t)
	ev := reminderEvent(t, store, -26*time.Hour, "alice")

	sent, err := srv.SendDueReminders()
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != 0 {
		t.Errorf("sent %d reminders about an event that ran yesterday, want 0", sent)
	}
	if n := len(posted(fake)); n != 0 {
		t.Errorf("%d messages posted about yesterday's event, want 0", n)
	}
	// Stamped, so it is settled rather than reconsidered every minute forever.
	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.RemindedBeforeAt == 0 || after.RemindedStartAt == 0 {
		t.Error("a written-off reminder was left unstamped, so it will be checked forever")
	}
}

// TestAnEventNobodyJoinedRemindsNobody.
func TestAnEventNobodyJoinedRemindsNobody(t *testing.T) {
	srv, store, fake := reminderServer(t)
	reminderEvent(t, store, 30*time.Minute)

	if sent, _ := srv.SendDueReminders(); sent != 0 {
		t.Errorf("sent %d reminders for an empty event, want 0", sent)
	}
	if n := len(posted(fake)); n != 0 {
		t.Errorf("%d messages posted for an empty event, want 0", n)
	}
}

// TestACancelledEventIsNotAnnounced.
func TestACancelledEventIsNotAnnounced(t *testing.T) {
	srv, store, fake := reminderServer(t)
	ev := reminderEvent(t, store, 20*time.Minute, "alice")
	cancelled := StatusCancelled
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &cancelled}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if sent, _ := srv.SendDueReminders(); sent != 0 {
		t.Errorf("sent %d reminders about a cancelled event, want 0", sent)
	}
	if n := len(posted(fake)); n != 0 {
		t.Errorf("%d messages about a cancelled event, want 0", n)
	}
}

// TestAFailedReminderIsRetriedRatherThanLost. Not stamping on failure is what
// makes the next minute try again; the grace window is what stops it trying
// forever.
func TestAFailedReminderIsRetriedRatherThanLost(t *testing.T) {
	srv, store, fake := reminderServer(t)
	reminderEvent(t, store, 30*time.Minute, "alice")

	var attempts int
	fake.on(http.MethodPost, "/channels/reminders/messages",
		func(w http.ResponseWriter, r *http.Request) {
			attempts++
			if attempts == 1 {
				http.Error(w, `{"message":"internal"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg-2"}`)
		})

	if sent, _ := srv.SendDueReminders(); sent != 0 {
		t.Error("a failed post was counted as sent")
	}
	if sent, _ := srv.SendDueReminders(); sent != 1 {
		t.Error("the failed reminder was not retried on the next run")
	}
}

// TestRemindersAreSilentUntilAChannelIsNamed. Reminders are the only pinging
// messages here, so an unconfigured deployment says nothing rather than picking
// a channel on somebody's behalf.
//
// And it must not STAMP while unconfigured: naming the channel next week has to
// start reminding about the events that already exist, not find every one of
// them already written off.
func TestRemindersAreSilentUntilAChannelIsNamed(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")

	ev := reminderEvent(t, store, 30*time.Minute, "alice")
	if sent, err := srv.SendDueReminders(); err != nil || sent != 0 {
		t.Fatalf("sent %d, %v; want silence with no channel configured", sent, err)
	}
	if n := len(fake.recorded()); n != 0 {
		t.Errorf("%d Discord calls with no reminder channel, want 0", n)
	}
	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.RemindedBeforeAt != 0 {
		t.Error("stamped while unconfigured, so naming the channel later reminds nobody")
	}

	srv.SetReminderChannelID("reminders")
	if sent, _ := srv.SendDueReminders(); sent != 1 {
		t.Error("naming the channel did not start reminding about an event that already existed")
	}
}
