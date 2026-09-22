package discordsignup

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestEveryWayOfSayingHowLong, from the examples asked for on 2026-09-22.
func TestEveryWayOfSayingHowLong(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"30 mins": 30 * time.Minute, "4 hours": 4 * time.Hour, "3:30": 3*time.Hour + 30*time.Minute,
		"60m": time.Hour, "4h": 4 * time.Hour, "4h30m": 4*time.Hour + 30*time.Minute,
		"4h 30m": 4*time.Hour + 30*time.Minute, "1 day": 24 * time.Hour,
		"90 minutes": 90 * time.Minute, "1.5h": 90 * time.Minute, "2 hrs": 2 * time.Hour,
		"4 hours 30 mins": 4*time.Hour + 30*time.Minute, "1 day, 2h": 26 * time.Hour, "4H30M": 4*time.Hour + 30*time.Minute,
		"": 0,
	} {
		got, err := ParseEventLength(input)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, bad := range []string{"2", "soon", "4 months", "0h", "3:75", "30 days", "h"} {
		if _, err := ParseEventLength(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestALengthReadsBackAsTyped: what the form is prefilled with parses to
// the same length.
func TestALengthReadsBackAsTyped(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Minute, 4 * time.Hour, 4*time.Hour + 30*time.Minute,
		24 * time.Hour, 50 * time.Hour, 49*time.Hour + 15*time.Minute} {
		text := FormatEventLength(d)
		back, err := ParseEventLength(text)
		if err != nil || back != d {
			t.Errorf("%v wrote %q, which reads back as %v, %v", d, text, back, err)
		}
	}
	if got := FormatEventLength(4*time.Hour + 30*time.Minute); got != "4h 30m" {
		t.Errorf("4h30m wrote %q", got)
	}
	if got := FormatEventLength(24 * time.Hour); got != "1 day" {
		t.Errorf("a day wrote %q", got)
	}
}

// TestTheRepeatFormSetsTheLength: the end follows from the start, and blank
// clears it.
func TestTheRepeatFormSetsTheLength(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games", Status: StatusOpen,
		Timezone: "America/Los_Angeles", StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.applyRepeatForm(rec, adminInteraction(t, RepeatModalCustomID(ev.ID), nil), ev.ID, "weekly", "3:30")
	got, _ := store.GetEvent(ev.ID)
	if got.EndsAt != ev.StartsAt+int64(3*3600+30*60) || got.RecurrenceRule == "" {
		t.Fatalf("after 3:30 weekly: ends %d (start %d), rule %q; reply %q", got.EndsAt, ev.StartsAt, got.RecurrenceRule, replyText(rec))
	}
	rec = httptest.NewRecorder()
	srv.applyRepeatForm(rec, adminInteraction(t, RepeatModalCustomID(ev.ID), nil), ev.ID, "weekly", "")
	if got, _ := store.GetEvent(ev.ID); got.EndsAt != 0 {
		t.Errorf("blank length left ends_at = %d", got.EndsAt)
	}
	rec = httptest.NewRecorder()
	srv.applyRepeatForm(rec, adminInteraction(t, RepeatModalCustomID(ev.ID), nil), ev.ID, "weekly", "2")
	if replyText(rec) == "" {
		t.Error("a bare number was accepted without a word")
	}
}
