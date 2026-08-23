package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEveryRowFitsDiscordsButtonCeiling is the constraint that forced one
// message per event. A message holds five action rows and a row holds five
// buttons; putting the whole table in one message caps it around twelve events
// and leaves the buttons in a block that lines up with nothing.
func TestEveryRowFitsDiscordsButtonCeiling(t *testing.T) {
	ev := &Event{ID: 1, Name: "Board games", Status: StatusOpen, Capacity: 8,
		AttendingCount: 3, StartsAt: time.Now().Unix()}
	rows, _ := RenderTableRow(ev)["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("%d action rows on one event's row, want 1", len(rows))
	}
	buttons := rows[0].(map[string]any)["components"].([]any)
	if len(buttons) > 5 {
		t.Fatalf("%d buttons in a row, want at most 5", len(buttons))
	}
	var ids []string
	for _, b := range buttons {
		ids = append(ids, b.(map[string]any)["custom_id"].(string))
	}
	// The same ids the full card uses, so one handler serves both views.
	want := []string{JoinCustomID(1), LeaveCustomID(1), DetailsCustomID(1), EditCustomID(1)}
	if len(ids) != len(want) {
		t.Fatalf("buttons = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("button %d = %q, want %q", i, ids[i], want[i])
		}
	}
}

// TestClosedEventsLoseJoinAndLeave keeps a stale button off a row that cannot
// take a signup, while leaving Details and Edit, which still make sense.
func TestClosedEventsLoseJoinAndLeave(t *testing.T) {
	ev := &Event{ID: 4, Name: "Shut", Status: StatusClosed, Capacity: 8}
	buttons := RenderTableRow(ev)["components"].([]any)[0].(map[string]any)["components"].([]any)
	var ids []string
	for _, b := range buttons {
		ids = append(ids, b.(map[string]any)["custom_id"].(string))
	}
	for _, unwanted := range []string{JoinCustomID(4), LeaveCustomID(4)} {
		for _, got := range ids {
			if got == unwanted {
				t.Errorf("a closed event still offers %q", unwanted)
			}
		}
	}
	if len(ids) != 2 {
		t.Errorf("buttons = %v, want just Details and Edit", ids)
	}
}

// TestRowLineIsCompact is the whole point of the table: one line, not a card.
func TestRowLineIsCompact(t *testing.T) {
	ev := &Event{ID: 1, Name: "Board games", Status: StatusOpen, Capacity: 8,
		AttendingCount: 3, WaitlistCount: 2, StartsAt: 1788067881, Location: "The pub"}
	content := RenderTableRow(ev)["content"].(string)
	if strings.Count(content, "\n") != 0 {
		t.Errorf("the row spans %d lines, want 1:\n%s", strings.Count(content, "\n")+1, content)
	}
	for _, want := range []string{"3/8", "Board games", "<t:1788067881:f>", "The pub", "2 waiting"} {
		if !strings.Contains(content, want) {
			t.Errorf("row = %q, want it to contain %q", content, want)
		}
	}
	// The time is Discord's own markup so it renders in each reader's zone,
	// rather than a fixed string in the server's.
	if strings.Contains(content, "2026-") {
		t.Error("the row hard-codes a formatted date instead of letting Discord localise it")
	}
}

// TestUncappedRowsShowInfinity so a row without a limit still reads as a count.
func TestUncappedRowsShowInfinity(t *testing.T) {
	ev := &Event{ID: 1, Name: "Open house", Status: StatusOpen, AttendingCount: 12}
	content := RenderTableRow(ev)["content"].(string)
	if !strings.Contains(content, "12/∞") {
		t.Errorf("row = %q, want an uncapped count", content)
	}
}

// TestRowsAreEditedNotReposted keeps each row where it is. A signup rewrites
// one message; anything else would walk the whole table down the channel.
func TestRowsAreEditedNotReposted(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "One",
		StartsAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 4; i++ {
		if err := srv.RefreshTableRow(ev); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	var rowPosts, rowEdits int
	for _, c := range fake.recorded() {
		switch {
		case c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/messages"):
			rowPosts++
		case c.Method == http.MethodPatch:
			rowEdits++
		}
	}
	// One post for the row, one for the header the first time; three edits.
	if rowPosts != 2 {
		t.Errorf("posted %d messages, want 2 (the row and the header, once each)", rowPosts)
	}
	if rowEdits != 3 {
		t.Errorf("edited %d times, want 3", rowEdits)
	}
}

// TestFinishedEventsLeaveTheTable stops the board filling with things that have
// already happened.
func TestFinishedEventsLeaveTheTable(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	past := time.Now().Add(-10 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Over",
		StartsAt: past, EndsAt: past + 3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.RefreshTableRow(ev); err != nil {
		t.Fatalf("draw row: %v", err)
	}
	rowMessage, err := store.TableRowMessageID(ev.ID)
	if err != nil || rowMessage == "" {
		t.Fatalf("no row recorded: %v", err)
	}

	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if left, _ := store.TableRowMessageID(ev.ID); left != "" {
		t.Errorf("the row is still recorded as %q after the event finished", left)
	}
	var deletedRow bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodDelete && strings.Contains(c.Path, rowMessage) {
			deletedRow = true
		}
	}
	if !deletedRow {
		t.Error("the row message was not deleted from the table channel")
	}
}

// TestRebuildRepostsInDateOrder covers the one thing Discord will not do:
// messages sit in the order they were posted and cannot be moved, so sorting
// means deleting and reposting.
func TestRebuildRepostsInDateOrder(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	base := time.Now().Add(24 * time.Hour).Unix()
	// Created out of order on purpose.
	for _, offset := range []int64{3, 1, 2} {
		if _, err := store.CreateEvent(Event{
			GuildID: "g1", ChannelID: "board",
			Name: fmt.Sprintf("Event +%d", offset), StartsAt: base + offset*3600,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := srv.RebuildEventTable("g1"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var order []string
	for _, c := range fake.recorded() {
		if c.Method != http.MethodPost || !strings.HasSuffix(c.Path, "/messages") {
			continue
		}
		content, _ := c.Body["content"].(string)
		for _, offset := range []string{"+1", "+2", "+3"} {
			if strings.Contains(content, "Event "+offset) {
				order = append(order, offset)
			}
		}
	}
	want := []string{"+1", "+2", "+3"}
	if len(order) != len(want) {
		t.Fatalf("posted %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("posted %v, want soonest first %v", order, want)
			break
		}
	}
}

// TestDetailsModalFitsDiscordsLimits covers the ceilings that reject the whole
// interaction rather than degrading: five components, a 45-character title, and
// 4000 characters per value.
func TestDetailsModalFitsDiscordsLimits(t *testing.T) {
	ev := &Event{
		ID: 1, GuildID: "g1", Name: strings.Repeat("A very long event name ", 5),
		Description: strings.Repeat("word ", 2000), Capacity: 3, AttendingCount: 2,
		WaitlistCount: 2, StartsAt: 1788067881, Location: "The pub",
		Timezone: "America/Los_Angeles", DiscordScheduledEventID: "native-1",
		DiscordInterestedCount: 9,
	}
	roster := []Signup{
		{DiscordUserID: "1", DisplayName: "Alice", State: StateAttending, Position: 1},
		{DiscordUserID: "2", DisplayName: "Bob", State: StateAttending, Position: 2},
		{DiscordUserID: "3", DisplayName: "Carol", State: StateWaitlisted, Position: 3, WaitlistPlace: 1},
		{DiscordUserID: "4", DisplayName: "Dan", State: StateWaitlisted, Position: 4, WaitlistPlace: 2},
	}
	modal := buildDetailsModal(ev, roster)

	if len([]rune(modal["title"].(string))) > 45 {
		t.Errorf("title is %d runes, over Discord's 45", len([]rune(modal["title"].(string))))
	}
	rows := modal["components"].([]any)
	if len(rows) == 0 {
		t.Fatal("a modal with no components is rejected outright")
	}
	if len(rows) > 5 {
		t.Fatalf("%d components, want at most 5", len(rows))
	}
	for _, r := range rows {
		f := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		if len([]rune(f["label"].(string))) > 45 {
			t.Errorf("label %q is over 45 runes", f["label"])
		}
		if len([]rune(f["value"].(string))) > detailsFieldLimit {
			t.Errorf("a value is %d runes, over Discord's %d",
				len([]rune(f["value"].(string))), detailsFieldLimit)
		}
	}
}

// TestDetailsModalListsNamesNotMentions covers the one thing a modal will not
// render. A <@id> mention shows as a raw snowflake in angle brackets there, so
// the roster has to be display names — which is what the backfill is for.
func TestDetailsModalListsNamesNotMentions(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	roster := []Signup{{DiscordUserID: "110122051179687936", DisplayName: "Slava",
		State: StateAttending, Position: 1}}

	for _, r := range buildDetailsModal(ev, roster)["components"].([]any) {
		f := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		value := f["value"].(string)
		if strings.Contains(value, "<@") {
			t.Errorf("field %q contains a mention, which a modal shows as a raw id: %q",
				f["custom_id"], value)
		}
		if f["custom_id"] == "details-going" && !strings.Contains(value, "Slava") {
			t.Errorf("the Going list is %q, want the display name", value)
		}
	}
}

// TestDetailsModalOmitsAnEmptyWaitlist keeps a permanently blank box out, since
// one reads as a broken field rather than an empty list.
func TestDetailsModalOmitsAnEmptyWaitlist(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	for _, r := range buildDetailsModal(ev, nil)["components"].([]any) {
		f := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		if f["custom_id"] == "details-waitlist" {
			t.Error("an empty waitlist still got a box")
		}
	}
}

// TestWaitlistIsNumberedByPlaceNotPosition means the modal shows "1." for the
// next person up, not their arrival number, which is an internal id nobody
// outside this package has any use for.
func TestWaitlistIsNumberedByPlaceNotPosition(t *testing.T) {
	got := rosterNames([]Signup{
		{DisplayName: "Carol", State: StateWaitlisted, Position: 17, WaitlistPlace: 1},
		{DisplayName: "Dan", State: StateWaitlisted, Position: 22, WaitlistPlace: 2},
	})
	if !strings.HasPrefix(got, "1. Carol") {
		t.Errorf("waitlist = %q, want it numbered by place", got)
	}
	if strings.Contains(got, "17") {
		t.Errorf("waitlist = %q, leaking the internal arrival position", got)
	}
}
