package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A Tuesday evening in Los Angeles that is already Wednesday in UTC. Every
// weekday derivation below has to come out Tuesday, or the rule is off by a
// day for anyone west of Greenwich.
func laTuesdayEvening(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, time.September, 8, 19, 0, 0, 0, loc) // Tue 8 Sep, 7pm PDT = 02:00 Wed UTC
}

func TestTheFourWordsBecomeTheFourRules(t *testing.T) {
	start := laTuesdayEvening(t)
	for _, c := range []struct{ word, want string }{
		{"weekly", "FREQ=WEEKLY;BYDAY=TU"},
		{"Every 2 Weeks", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU"},
		{"every other week", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU"},
		{"monthly", "FREQ=MONTHLY;BYDAY=2TU"}, // the 8th is in the second week
		{"never", ""},
		{"", ""},
	} {
		got, err := repeatWordToRRule(c.word, start)
		if err != nil || got != c.want {
			t.Errorf("%q -> %q, %v; want %q", c.word, got, err, c.want)
		}
	}
	if _, err := repeatWordToRRule("sometimes", start); err == nil {
		t.Error("a word the form does not offer was accepted")
	}
}

func TestTheRulesBecomeDiscordsObject(t *testing.T) {
	start := laTuesdayEvening(t).Unix()
	mk := func(rule string) *Event {
		return &Event{ID: 1, StartsAt: start, Timezone: "America/Los_Angeles", RecurrenceRule: rule}
	}
	if rule, ok := discordRecurrenceRule(mk(""), "UTC"); !ok || rule != nil {
		t.Errorf("never -> %v ok=%v, want nil and ok (send null)", rule, ok)
	}
	rule, ok := discordRecurrenceRule(mk("FREQ=WEEKLY;INTERVAL=2;BYDAY=TU"), "UTC")
	if !ok || rule["frequency"] != discordFrequencyWeekly || rule["interval"] != 2 ||
		fmt.Sprint(rule["by_weekday"]) != "[1]" {
		t.Errorf("every 2 weeks -> %v ok=%v", rule, ok)
	}
	rule, ok = discordRecurrenceRule(mk("FREQ=MONTHLY;BYDAY=2TU"), "UTC")
	if !ok || rule["frequency"] != discordFrequencyMonthly || fmt.Sprint(rule["by_n_weekday"]) != "[map[day:1 n:2]]" {
		t.Errorf("monthly -> %v ok=%v", rule, ok)
	}
	// A rule with no BYDAY takes the day from the START in its own zone —
	// Tuesday in Los Angeles, not the Wednesday it already is in UTC.
	rule, ok = discordRecurrenceRule(mk("FREQ=WEEKLY"), "UTC")
	if !ok || fmt.Sprint(rule["by_weekday"]) != "[1]" {
		t.Errorf("weekly with no day -> %v, want Tuesday (1) from the LA start", rule)
	}
	for _, unsupported := range []string{"FREQ=DAILY", "FREQ=WEEKLY;INTERVAL=3", "FREQ=WEEKLY;BYDAY=MO,TU", "FREQ=MONTHLY;BYMONTHDAY=8"} {
		if _, ok := discordRecurrenceRule(mk(unsupported), "UTC"); ok {
			t.Errorf("%q was offered to Discord, which would refuse it", unsupported)
		}
	}
}

func TestAMonthlyRuleRoundTripsThroughTheImport(t *testing.T) {
	raw := json.RawMessage(`{"frequency":1,"interval":1,"by_n_weekday":[{"n":2,"day":1}]}`)
	if got := recurrenceRuleText(raw); got != "FREQ=MONTHLY;BYDAY=2TU" {
		t.Errorf("imported monthly rule = %q", got)
	}
	if got := describeRepeat("FREQ=MONTHLY;BYDAY=2TU"); got != "monthly" {
		t.Errorf("describeRepeat = %q", got)
	}
}

// TestTheRepeatFormReachesDiscord is the bug: set from a form, stored, and
// until now never sent.
func TestTheRepeatFormReachesDiscord(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	srv.SetDefaultTimezone("America/Los_Angeles")
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Status: StatusOpen, StartsAt: laTuesdayEvening(t).Unix(), DiscordScheduledEventID: "native-9"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := httptestRecorder()
	srv.applyRepeatForm(rec, adminInteraction(t, RepeatModalCustomID(ev.ID), nil), ev.ID, "every 2 weeks", "")
	if got, _ := store.GetEvent(ev.ID); got.RecurrenceRule != "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU" || got.Timezone == "" {
		t.Fatalf("stored rule %q zone %q", got.RecurrenceRule, got.Timezone)
	}
	if !strings.Contains(replyText(rec), "every 2 weeks") {
		t.Errorf("reply = %q", replyText(rec))
	}
	deadline := time.Now().Add(2 * time.Second)
	var sent any
	for time.Now().Before(deadline) {
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-9" {
				sent = c.Body["recurrence_rule"]
			}
		}
		if sent != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rule, _ := sent.(map[string]any)
	if rule == nil || fmt.Sprint(rule["frequency"]) != "2" || fmt.Sprint(rule["interval"]) != "2" {
		t.Fatalf("Discord received recurrence_rule %v, want weekly interval 2", sent)
	}
	// The form also stamps the zone the event lacked, so the history holds a
	// timezone row as well; the rule's row is what this is about.
	updates, _ := store.EventUpdates(ev.ID)
	var ruleRow *EventUpdate
	for i := range updates {
		if updates[i].Field == "recurrence_rule" {
			ruleRow = &updates[i]
		}
	}
	if ruleRow == nil || ruleRow.ToValue != "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU" || ruleRow.Actor != "u-admin" {
		t.Errorf("history = %+v, want the rule change recorded as u-admin's", updates)
	}
}

func TestNeverSendsNullSoDiscordStopsRepeating(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev, _ := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games", Status: StatusOpen,
		StartsAt: time.Now().Add(48 * time.Hour).Unix(), Timezone: "UTC",
		RecurrenceRule: "FREQ=WEEKLY;BYDAY=TU", DiscordScheduledEventID: "native-9"})
	srv.applyRepeatForm(httptestRecorder(), adminInteraction(t, RepeatModalCustomID(ev.ID), nil), ev.ID, "never", "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-9" {
				if v, present := c.Body["recurrence_rule"]; present && v == nil {
					return // null went out
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never did not send recurrence_rule: null to Discord")
}

func TestTheManagementRowHasRepeat(t *testing.T) {
	labels := []string{}
	for _, b := range managementButtons(&Event{ID: 1, Status: StatusOpen}) {
		labels = append(labels, b.(map[string]any)["label"].(string))
	}
	if strings.Join(labels, ",") != "Edit,Repeat,Close signups,Cancel" {
		t.Errorf("row = %v", labels)
	}
}
