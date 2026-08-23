package discordsignup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func tableEvents(n int) []Event {
	out := make([]Event, n)
	base := time.Now().Add(24 * time.Hour).Unix()
	for i := range out {
		out[i] = Event{
			ID: int64(i + 1), GuildID: "g1", Name: fmt.Sprintf("Event %d", i+1),
			Status: StatusOpen, StartsAt: base + int64(i)*3600,
			Capacity: 4, AttendingCount: 1, Timezone: "UTC",
		}
	}
	return out
}

// TestTableFitsDiscordsComponentCeilings is the constraint that decided the
// whole design. A message carries five action rows; a select takes a whole one
// and holds 25 options. Break either and Discord rejects the message outright.
func TestTableFitsDiscordsComponentCeilings(t *testing.T) {
	payload := RenderEventTable(tableEvents(40))
	rows, _ := payload["components"].([]any)
	if len(rows) > 5 {
		t.Fatalf("%d action rows, want at most 5", len(rows))
	}
	for _, r := range rows {
		row := r.(map[string]any)
		components := row["components"].([]any)
		if len(components) != 1 {
			t.Errorf("a row holds %d components; a select must be alone in its row",
				len(components))
		}
		c := components[0].(map[string]any)
		if opts, ok := c["options"].([]any); ok && len(opts) > maxSelectOptions {
			t.Errorf("a menu holds %d options, want at most %d", len(opts), maxSelectOptions)
		}
	}
	content := payload["content"].(string)
	if len(content) > discordMessageContentLimit {
		t.Errorf("content is %d bytes, over Discord's %d", len(content), discordMessageContentLimit)
	}
	// Forty events do not fit in a 25-option menu, and the ones left out are
	// said out loud rather than silently dropped.
	if !strings.Contains(content, "Showing the first 25 of 40") {
		t.Error("the table did not say that it was showing only part of the list")
	}
}

// TestEmptyTableHasNoMenus covers a real 400: Discord rejects a select menu
// with zero options, so a menu with nothing to offer must be left out.
func TestEmptyTableHasNoMenus(t *testing.T) {
	payload := RenderEventTable(nil)
	rows, _ := payload["components"].([]any)
	if len(rows) != 0 {
		t.Errorf("an empty table has %d component rows, want 0", len(rows))
	}
	if !strings.Contains(payload["content"].(string), "Nothing coming up") {
		t.Error("an empty table should say so")
	}
}

// TestClosedEventsCannotBeJoinedFromTheTable keeps the menu honest: an event
// that is not taking signups must not appear in the Join menu, even though it
// is still listed and still has details worth reading.
func TestClosedEventsCannotBeJoinedFromTheTable(t *testing.T) {
	events := tableEvents(3)
	events[1].Status = StatusClosed

	payload := RenderEventTable(events)
	menus := menuValues(payload)
	for _, value := range menus[tableJoinSelectID] {
		if value == "2" {
			t.Error("a closed event is offered in the Join menu")
		}
	}
	// It stays in Details and Edit, because a closed event is still one people
	// ask about and organisers still adjust.
	var inDetails bool
	for _, value := range menus[tableDetailsSelectID] {
		if value == "2" {
			inDetails = true
		}
	}
	if !inDetails {
		t.Error("a closed event vanished from the Details menu")
	}
}

// TestFullEventsSaySoInTheJoinMenu means nobody picks a full event expecting a
// place — the option's own description warns first.
func TestFullEventsSaySoInTheJoinMenu(t *testing.T) {
	events := tableEvents(2)
	events[0].AttendingCount = events[0].Capacity

	payload := RenderEventTable(events)
	for _, r := range payload["components"].([]any) {
		c := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		if c["custom_id"] != tableJoinSelectID {
			continue
		}
		first := c["options"].([]any)[0].(map[string]any)
		desc, _ := first["description"].(string)
		if !strings.Contains(desc, "waitlist") {
			t.Errorf("a full event's option says %q, want a waitlist warning", desc)
		}
		return
	}
	t.Fatal("no join menu found")
}

// TestLongNamesDoNotBreakTheColumns covers padding by runes rather than bytes.
// An emoji is one rune and several bytes, so padding by bytes leaves the column
// short by exactly the difference — which is how a fixed-width table stops
// being one.
func TestLongNamesDoNotBreakTheColumns(t *testing.T) {
	events := tableEvents(3)
	events[0].Name = "🎲 Board games 🎲"
	events[1].Name = "Short"

	content := RenderEventTable(events)["content"].(string)
	parts := strings.Split(content, "```")
	if len(parts) < 2 {
		t.Fatal("no code block; the columns would not line up")
	}
	lines := strings.Split(strings.TrimSpace(parts[1]), "\n")
	// Header, rule, then one line per event. Every one must be the same width
	// in RUNES, which is what a monospace font actually lays out.
	width := len([]rune(lines[0]))
	for i, line := range lines[2:] {
		got := len([]rune(strings.TrimRight(line, " ")))
		if got > width {
			t.Errorf("row %d is %d runes wide, header is %d — the columns have drifted",
				i, got, width)
		}
	}

	// And every option label stays inside Discord's per-option cap.
	for _, r := range RenderEventTable(events)["components"].([]any) {
		c := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		opts, ok := c["options"].([]any)
		if !ok {
			continue
		}
		for _, opt := range opts {
			label := opt.(map[string]any)["label"].(string)
			if len([]rune(label)) > selectOptionLabelLimit {
				t.Errorf("option label is %d runes, over Discord's %d",
					len([]rune(label)), selectOptionLabelLimit)
			}
		}
	}
}

// menuValues collects the selectable event ids per menu, skipping rows that
// hold a button rather than a select — those carry no options at all.
func menuValues(payload map[string]any) map[string][]string {
	out := map[string][]string{}
	rows, _ := payload["components"].([]any)
	for _, r := range rows {
		c := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		opts, ok := c["options"].([]any)
		if !ok {
			continue
		}
		id, _ := c["custom_id"].(string)
		for _, o := range opts {
			out[id] = append(out[id], o.(map[string]any)["value"].(string))
		}
	}
	return out
}

// TestTableIsEditedNotReposted keeps it where people scrolled to.
func TestTableIsEditedNotReposted(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())

	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	if _, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "One",
		StartsAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := srv.RefreshEventTable("g1"); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	var posts, edits int
	for _, c := range fake.recorded() {
		switch c.Method {
		case "POST":
			posts++
		case "PATCH":
			edits++
		}
	}
	if posts != 1 {
		t.Errorf("posted %d times, want 1 — the table must be edited in place", posts)
	}
	if edits != 2 {
		t.Errorf("edited %d times, want 2", edits)
	}
}
