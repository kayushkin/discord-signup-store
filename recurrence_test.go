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
		{"daily", "FREQ=DAILY"},
		{"weekdays", "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR"},
		{"yearly", "FREQ=YEARLY;BYMONTH=9;BYMONTHDAY=8"},
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
		fmt.Sprint(rule["by_weekday"]) != "[2]" {
		t.Errorf("every 2 weeks -> %v ok=%v, want Wednesday (2), the UTC day of a Tuesday-evening LA start", rule, ok)
	}
	rule, ok = discordRecurrenceRule(mk("FREQ=MONTHLY;BYDAY=2TU"), "UTC")
	if !ok || rule["frequency"] != discordFrequencyMonthly || fmt.Sprint(rule["by_n_weekday"]) != "[map[day:2 n:2]]" {
		t.Errorf("monthly -> %v ok=%v", rule, ok)
	}
	// A rule with no BYDAY takes the day from the start in its own zone —
	// Tuesday in Los Angeles — and Discord is sent that day in UTC, where it
	// reads its rules: Wednesday.
	rule, ok = discordRecurrenceRule(mk("FREQ=WEEKLY"), "UTC")
	if !ok || fmt.Sprint(rule["by_weekday"]) != "[2]" {
		t.Errorf("weekly with no day -> %v, want Wednesday (2), the UTC day of the LA Tuesday", rule)
	}
	rule, ok = discordRecurrenceRule(mk("FREQ=DAILY"), "UTC")
	if !ok || rule["frequency"] != discordFrequencyDaily || rule["by_weekday"] != nil {
		t.Errorf("daily -> %v ok=%v, want frequency 3 and no weekday set", rule, ok)
	}
	rule, ok = discordRecurrenceRule(mk("FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR"), "UTC")
	if !ok || fmt.Sprint(rule["by_weekday"]) != "[1 2 3 4 5]" {
		t.Errorf("weekdays -> %v ok=%v, want Tuesday to Saturday, LA's weekday evenings in UTC", rule, ok)
	}
	rule, ok = discordRecurrenceRule(mk("FREQ=YEARLY"), "UTC")
	if !ok || rule["frequency"] != discordFrequencyYearly || fmt.Sprint(rule["by_month"]) != "[9]" || fmt.Sprint(rule["by_month_day"]) != "[9]" {
		t.Errorf("yearly -> %v ok=%v, want September 9, the UTC date of the LA September 8 start", rule, ok)
	}
	// Outside Discord's business rules: refused, and validateRecurrence
	// refuses the same set so none of them is ever stored.
	for _, unsupported := range []string{"FREQ=WEEKLY;INTERVAL=3", "FREQ=WEEKLY;BYDAY=MO,TU",
		"FREQ=MONTHLY;BYMONTHDAY=8", "FREQ=DAILY;BYDAY=MO,WE", "FREQ=DAILY;INTERVAL=2",
		"FREQ=YEARLY;BYMONTH=6", "FREQ=WEEKLY;COUNT=4", "FREQ=HOURLY"} {
		if _, ok := discordRecurrenceRule(mk(unsupported), "UTC"); ok {
			t.Errorf("%q was offered to Discord, which would refuse it", unsupported)
		}
		if err := validateRecurrence(unsupported, "UTC"); err == nil {
			t.Errorf("%q would be stored, and nothing could act on it", unsupported)
		}
	}
}

// TestAnEveningRuleKeepsItsLocalDay is "Late Night Homebody Hangout": made in
// Discord's form for Wednesdays at 11:45pm in Los Angeles, which is Thursday
// in UTC, so Discord sent by_weekday [3]. Stored as Thursday, it rolled on
// 2026-10-15 to the very next evening, a Thursday.
func TestAnEveningRuleKeepsItsLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 14, 23, 45, 0, 0, loc) // Wed 11:45pm PDT = Thu 06:45 UTC
	raw := json.RawMessage(`{"start":"2026-10-15T06:45:00+00:00","frequency":2,"interval":1,"by_weekday":[3]}`)
	rule := rruleInZoneFromDiscord(raw, start)
	if rule != "FREQ=WEEKLY;BYDAY=WE" {
		t.Fatalf("imported %q, want Wednesday in Los Angeles", rule)
	}
	next, ok := nextOccurrence(rule, start, start.Add(6*time.Hour))
	if want := time.Date(2026, time.October, 21, 23, 45, 0, 0, loc); !ok || !next.Equal(want) {
		t.Errorf("rolled to %v, want %v", next, want)
	}
	// And back to Discord as it came: Thursday, the UTC day.
	sent, ok := discordRecurrenceRule(&Event{StartsAt: next.Unix(), Timezone: "America/Los_Angeles", RecurrenceRule: rule}, "UTC")
	if !ok || fmt.Sprint(sent["by_weekday"]) != "[3]" {
		t.Errorf("sent %v, want by_weekday [3]", sent)
	}

	// A monthly rule across a month end: Saturday 31 October at 8pm in Los
	// Angeles is the 1st Sunday of November in UTC.
	halloween := time.Date(2026, time.October, 31, 20, 0, 0, 0, loc)
	monthly := rruleInZoneFromDiscord(json.RawMessage(`{"frequency":1,"interval":1,"by_n_weekday":[{"n":1,"day":6}]}`), halloween)
	if monthly != "FREQ=MONTHLY;BYDAY=5SA" {
		t.Errorf("imported monthly %q, want the 5th Saturday", monthly)
	}
	sent, _ = discordRecurrenceRule(&Event{StartsAt: halloween.Unix(), Timezone: "America/Los_Angeles", RecurrenceRule: monthly}, "UTC")
	if fmt.Sprint(sent["by_n_weekday"]) != "[map[day:6 n:1]]" {
		t.Errorf("sent monthly %v, want the 1st Sunday", sent)
	}
	// Yearly across the same date line.
	yearly := rruleInZoneFromDiscord(json.RawMessage(`{"frequency":0,"interval":1,"by_month":[11],"by_month_day":[1]}`), halloween)
	if yearly != "FREQ=YEARLY;BYMONTH=10;BYMONTHDAY=31" {
		t.Errorf("imported yearly %q, want 31 October", yearly)
	}
}

func TestAMonthlyRuleRoundTripsThroughTheImport(t *testing.T) {
	raw := json.RawMessage(`{"frequency":1,"interval":1,"by_n_weekday":[{"n":2,"day":1}]}`)
	if got := rruleInZoneFromDiscord(raw, time.Unix(0, 0).UTC()); got != "FREQ=MONTHLY;BYDAY=2TU" {
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
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
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
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
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

// TestTheNextOccurrenceIsTheOneAfterNow is the arithmetic a rollover runs on:
// the next time the rule fires, in the event's own zone, after the occurrence
// that just finished.
func TestTheNextOccurrenceIsTheOneAfterNow(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	// Tuesday 2026-03-03 19:00 Los Angeles — the week DST starts on the 8th.
	start := time.Date(2026, 3, 3, 19, 0, 0, 0, la)
	for _, c := range []struct {
		name  string
		rule  string
		after time.Time
		want  time.Time
	}{
		{"weekly, across the clock change, same wall clock", "FREQ=WEEKLY;BYDAY=TU",
			start.Add(time.Hour), time.Date(2026, 3, 10, 19, 0, 0, 0, la)},
		{"weekly, two occurrences already missed", "FREQ=WEEKLY;BYDAY=TU",
			time.Date(2026, 3, 18, 0, 0, 0, 0, la), time.Date(2026, 3, 24, 19, 0, 0, 0, la)},
		{"every two weeks counts from the anchor", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU",
			start.Add(time.Hour), time.Date(2026, 3, 17, 19, 0, 0, 0, la)},
		{"monthly, first Tuesday", "FREQ=MONTHLY;BYDAY=1TU",
			start.Add(time.Hour), time.Date(2026, 4, 7, 19, 0, 0, 0, la)},
		{"monthly, fifth Tuesday skips months without one", "FREQ=MONTHLY;BYDAY=5TU",
			time.Date(2026, 3, 31, 20, 0, 0, 0, la), time.Date(2026, 6, 30, 19, 0, 0, 0, la)},
		{"daily is tomorrow at the same wall clock", "FREQ=DAILY",
			start.Add(time.Hour), time.Date(2026, 3, 4, 19, 0, 0, 0, la)},
		{"daily, a week missed, is the day after now", "FREQ=DAILY",
			time.Date(2026, 3, 10, 12, 0, 0, 0, la), time.Date(2026, 3, 10, 19, 0, 0, 0, la)},
		{"weekdays skip the weekend", "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR",
			time.Date(2026, 3, 6, 20, 0, 0, 0, la), time.Date(2026, 3, 9, 19, 0, 0, 0, la)},
		{"yearly is the same date next year", "FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=3",
			start.Add(time.Hour), time.Date(2027, 3, 3, 19, 0, 0, 0, la)},
	} {
		got, ok := nextOccurrence(c.rule, start, c.after)
		if !ok {
			t.Errorf("%s: rule refused", c.name)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%s: next = %s, want %s", c.name, got, c.want)
		}
	}
	if _, ok := nextOccurrence("FREQ=WEEKLY;INTERVAL=3", start, start); ok {
		t.Error("a rule Discord cannot express must not roll either")
	}
}

// TestEveryRuleIsDescribedInWords is the table showing "FREQ=DAILY" beside
// "weekly": no surface may ever show an RRULE.
func TestEveryRuleIsDescribedInWords(t *testing.T) {
	for rule, want := range map[string]string{
		"":                                    "never",
		"FREQ=DAILY":                          "daily",
		"FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR":     "every weekday",
		"FREQ=DAILY;BYDAY=SA,SU":              "weekends",
		"FREQ=WEEKLY;BYDAY=FR":                "weekly",
		"FREQ=WEEKLY;INTERVAL=2;BYDAY=FR":     "every 2 weeks",
		"FREQ=MONTHLY;BYDAY=2TU":              "monthly",
		"FREQ=YEARLY;BYMONTH=7;BYMONTHDAY=24": "yearly",
	} {
		if got := describeRepeat(rule); got != want {
			t.Errorf("%q described as %q, want %q", rule, got, want)
		}
		if strings.Contains(describeRepeat(rule), "FREQ") {
			t.Errorf("%q leaked the RRULE", rule)
		}
	}
	// What Discord's own "every day" form sends, through the import.
	if got := rruleInZoneFromDiscord(json.RawMessage(`{"frequency":3,"interval":1,"by_weekday":null}`), time.Unix(0, 0).UTC()); got != "FREQ=DAILY" {
		t.Errorf("Discord's daily came in as %q", got)
	}
	if got := rruleInZoneFromDiscord(json.RawMessage(`{"frequency":0,"interval":1,"by_month":[7],"by_month_day":[24]}`), time.Unix(0, 0).UTC()); got != "FREQ=YEARLY;BYMONTH=7;BYMONTHDAY=24" {
		t.Errorf("Discord's yearly came in as %q", got)
	}
}
