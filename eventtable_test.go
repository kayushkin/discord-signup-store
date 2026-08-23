package discordsignup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// tableEvents builds n open events an hour apart, for testing the panel's
// component budget at various sizes.
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

// TestAPageStaysInsideDiscordsComponentBudget is the ceiling that sets
// eventsPerPage, and it is measured rather than inferred: each event costs a
// text block, an action row and four buttons, and Discord allows 40 components
// in a message. Six fit; seven is COMPONENT_MAX_TOTAL_COMPONENTS_EXCEEDED.
func TestAPageStaysInsideDiscordsComponentBudget(t *testing.T) {
	for _, n := range []int{1, 3, eventsPerPage} {
		payload := RenderTablePage(tableEvents(n), 0, 1)
		if got := countComponents(payload["components"].([]any)); got > 40 {
			t.Errorf("%d events render %d components, over Discord's 40", n, got)
		}
	}
}

func countComponents(components []any) int {
	total := 0
	for _, c := range components {
		m := c.(map[string]any)
		total++
		if nested, ok := m["components"].([]any); ok {
			total += countComponents(nested)
		}
		if options, ok := m["options"].([]any); ok {
			total += len(options)
		}
	}
	return total
}

// TestOneTextBlockPerEvent is why six fit rather than two. Splitting the line
// into title, description and time would cost three components each.
func TestOneTextBlockPerEvent(t *testing.T) {
	events := tableEvents(3)
	events[0].Description = "Bring dice."
	body := RenderTablePage(events, 0, 1)["components"].([]any)[0].(map[string]any)["components"].([]any)

	var textBlocks, actionRows int
	for _, c := range body {
		switch c.(map[string]any)["type"] {
		case componentTypeTextDisplay:
			textBlocks++
		case componentTypeActionRow:
			actionRows++
		}
	}
	// One block per event and nothing else — no heading, no per-field split.
	if textBlocks != 3 {
		t.Errorf("%d text blocks, want one per event and no heading", textBlocks)
	}
	if actionRows != 3 {
		t.Errorf("%d action rows, want one per event", actionRows)
	}
}

// TestEveryEventCarriesAllFourButtons is what one message per event used to be
// needed for, and no longer is.
func TestEveryEventCarriesAllFourButtons(t *testing.T) {
	body := RenderTablePage(tableEvents(1), 0, 1)["components"].([]any)[0].(map[string]any)["components"].([]any)
	for _, c := range body {
		m := c.(map[string]any)
		if m["type"] != componentTypeActionRow {
			continue
		}
		var ids []string
		for _, b := range m["components"].([]any) {
			ids = append(ids, b.(map[string]any)["custom_id"].(string))
		}
		want := []string{JoinCustomID(1), LeaveCustomID(1), DetailsCustomID(1), EditCustomID(1)}
		if len(ids) != len(want) {
			t.Fatalf("buttons = %v, want %v", ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Errorf("button %d = %q, want %q", i, ids[i], want[i])
			}
		}
		return
	}
	t.Fatal("no action row found")
}

// TestClosedEventsLoseJoinAndLeave keeps a button that cannot act off the row.
func TestClosedEventsLoseJoinAndLeave(t *testing.T) {
	events := tableEvents(1)
	events[0].Status = StatusClosed
	body := RenderTablePage(events, 0, 1)["components"].([]any)[0].(map[string]any)["components"].([]any)
	for _, c := range body {
		m := c.(map[string]any)
		if m["type"] != componentTypeActionRow {
			continue
		}
		if len(m["components"].([]any)) != 2 {
			t.Errorf("a closed event has %d buttons, want just Details and Edit",
				len(m["components"].([]any)))
		}
	}
}

// TestPaginationSpillsPastSixAndNumbersContinuously covers the spill-over.
func TestPaginationSpillsPastSixAndNumbersContinuously(t *testing.T) {
	pages := paginate(tableEvents(14))
	if len(pages) != 3 {
		t.Fatalf("%d pages for 14 events, want 3", len(pages))
	}
	if len(pages[0]) != 5 || len(pages[1]) != 5 || len(pages[2]) != 4 {
		t.Errorf("page sizes %d/%d/%d, want 5/5/4",
			len(pages[0]), len(pages[1]), len(pages[2]))
	}
	// No page carries a heading, including the first.
	for _, p := range []map[string]any{
		RenderTablePage(pages[0], 0, 3), RenderTablePage(pages[1], 1, 3),
	} {
		body := p["components"].([]any)[0].(map[string]any)["components"].([]any)
		if strings.Contains(body[0].(map[string]any)["content"].(string), "## Events") {
			t.Error("a page carries a heading")
		}
	}
}

// TestTableIsEditedInPlaceAndShrinks covers the reason pages beat one message
// per event: rewriting them keeps the table sorted without reposting, because
// events move between pages while the messages stay put.
func TestTableIsEditedInPlaceAndShrinks(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	base := time.Now().Add(24 * time.Hour).Unix()
	var ids []int64
	for i := 0; i < 8; i++ {
		ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
			Name: fmt.Sprintf("Event %d", i), StartsAt: base + int64(i)*3600})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, ev.ID)
	}
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	if pages, _ := store.TablePages("g1"); len(pages) != 2 {
		t.Fatalf("%d pages for 8 events, want 2", len(pages))
	}

	// Redrawing again must edit, never post.
	before := len(fake.recorded())
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("second draw: %v", err)
	}
	for _, c := range fake.recorded()[before:] {
		if c.Method == "POST" {
			t.Errorf("redrawing posted a new message: %s", c.Path)
		}
	}

	// Dropping below six events must delete the surplus page.
	for _, id := range ids[5:] {
		if err := store.DeleteEvent(id); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("third draw: %v", err)
	}
	if pages, _ := store.TablePages("g1"); len(pages) != 1 {
		t.Errorf("%d pages for 5 events, want 1", len(pages))
	}
}

// TestDetailsModalIsAllReadOnlyText is the point of this view.
//
// Discord has no read-only text input, so an earlier version prefilled ordinary
// inputs — boxes that looked editable and were not. Text Display is allowed in
// a modal and is genuinely read-only, so there is no input here at all and
// nothing to explain away on submit.
func TestDetailsModalIsAllReadOnlyText(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Games", Description: "Bring dice.",
		Capacity: 3, AttendingCount: 2, WaitlistCount: 1, StartsAt: 1788067881,
		Location: "The pub", Timezone: "America/Los_Angeles"}
	roster := []Signup{
		{DiscordUserID: "1", DisplayName: "Alice", State: StateAttending},
		{DiscordUserID: "2", DisplayName: "Bob", State: StateAttending},
		{DiscordUserID: "3", DisplayName: "Carol", State: StateWaitlisted, WaitlistPlace: 1},
	}
	modal := buildDetailsModal(ev, roster)

	components := modal["components"].([]any)
	if len(components) == 0 || len(components) > 5 {
		t.Fatalf("%d components, want between 1 and 5", len(components))
	}
	for i, c := range components {
		block := c.(map[string]any)
		if block["type"] != componentTypeTextDisplay {
			t.Errorf("component %d is type %v, want %d (Text Display) — anything else is "+
				"an input, which cannot be made read-only", i, block["type"], componentTypeTextDisplay)
		}
		if len([]rune(block["content"].(string))) > textDisplayLimit {
			t.Errorf("component %d is over Discord's %d-character limit", i, textDisplayLimit)
		}
	}
	if len([]rune(modal["title"].(string))) > 45 {
		t.Error("title is over Discord's 45 runes")
	}
}

// TestDetailsModalReadsDescriptionThenGoingThenWaitlist pins the order the
// questions actually get asked in.
func TestDetailsModalReadsDescriptionThenGoingThenWaitlist(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Description: "Bring dice.", Capacity: 2,
		AttendingCount: 2, WaitlistCount: 1, StartsAt: 1788067881}
	roster := []Signup{
		{DiscordUserID: "1", DisplayName: "Alice", State: StateAttending},
		{DiscordUserID: "2", DisplayName: "Bob", State: StateAttending},
		{DiscordUserID: "3", DisplayName: "Carol", State: StateWaitlisted, WaitlistPlace: 1},
	}
	blocks := buildDetailsModal(ev, roster)["components"].([]any)
	var contents []string
	for _, c := range blocks {
		contents = append(contents, c.(map[string]any)["content"].(string))
	}
	if len(contents) < 3 {
		t.Fatalf("got %d blocks, want at least description, going and waitlist", len(contents))
	}
	if !strings.HasPrefix(contents[0], "Bring dice.") {
		t.Errorf("first block is %q, want the description first", contents[0])
	}
	if !strings.Contains(contents[1], "Going — 2 of 2") || !strings.Contains(contents[1], "Alice") {
		t.Errorf("second block is %q, want the going list", contents[1])
	}
	if !strings.Contains(contents[2], "Waitlist — 1") || !strings.Contains(contents[2], "Carol") {
		t.Errorf("third block is %q, want the waitlist", contents[2])
	}
}

// TestDetailsModalListsNamesNotMentions covers the one thing a modal will not
// render: a <@id> mention shows as a raw snowflake there.
func TestDetailsModalListsNamesNotMentions(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	roster := []Signup{{DiscordUserID: "110122051179687936", DisplayName: "Slava",
		State: StateAttending}}
	for _, c := range buildDetailsModal(ev, roster)["components"].([]any) {
		content := c.(map[string]any)["content"].(string)
		if strings.Contains(content, "<@") {
			t.Errorf("a block contains a mention, which a modal shows as a raw id: %q", content)
		}
	}
}

// TestDetailsModalOmitsAnEmptyWaitlist keeps a permanently blank heading out.
func TestDetailsModalOmitsAnEmptyWaitlist(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	for _, c := range buildDetailsModal(ev, nil)["components"].([]any) {
		if strings.Contains(c.(map[string]any)["content"].(string), "Waitlist") {
			t.Error("an empty waitlist still got a heading")
		}
	}
}

// TestWaitlistIsNumberedByPlaceNotPosition means the next person up reads as
// "1.", not their internal arrival number.
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

// TestEventLineIsSlotsTitleLocationTime pins the format, in that order and in
// one component, with the date compact rather than Discord-localised.
func TestEventLineIsSlotsTitleLocationTime(t *testing.T) {
	// 2026-08-29 16:00 in Los Angeles.
	when := time.Date(2026, 8, 29, 16, 0, 0, 0, mustZone(t, "America/Los_Angeles")).Unix()
	ev := &Event{ID: 1, Name: "Board games", Capacity: 8, AttendingCount: 3,
		Location: "The pub", Timezone: "America/Los_Angeles",
		StartsAt: when, Status: StatusOpen}

	want := "`3/8`  ·  **Board games**  ·  The pub  ·  8/29 4pm"
	if got := eventLine(ev); got != want {
		t.Errorf("eventLine =\n  %q\nwant\n  %q", got, want)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	return loc
}

// TestUncappedEventsShowNoCount pins the fix for the bare "1". A count with no
// limit next to it read as nothing — is it a count, a limit, a rank? — so an
// uncapped event shows no number at all.
func TestUncappedEventsShowNoCount(t *testing.T) {
	bare := eventLine(&Event{ID: 1, Name: "Just a name", Capacity: 0, AttendingCount: 7})
	if bare != "**Just a name**" {
		t.Errorf("eventLine = %q, want just the name", bare)
	}
}

// TestCompactWhenKeepsMinutesOnlyWhenTheyMatter: "4pm" not "4:00pm", but a
// half-hour start keeps its minutes.
func TestCompactWhenKeepsMinutesOnlyWhenTheyMatter(t *testing.T) {
	zone := mustZone(t, "America/Los_Angeles")
	onHour := &Event{StartsAt: time.Date(2026, 8, 29, 16, 0, 0, 0, zone).Unix(),
		Timezone: "America/Los_Angeles"}
	if got := compactWhen(onHour); got != "8/29 4pm" {
		t.Errorf("compactWhen = %q, want %q", got, "8/29 4pm")
	}
	halfPast := &Event{StartsAt: time.Date(2026, 8, 29, 16, 30, 0, 0, zone).Unix(),
		Timezone: "America/Los_Angeles"}
	if got := compactWhen(halfPast); got != "8/29 4:30pm" {
		t.Errorf("compactWhen = %q, want %q", got, "8/29 4:30pm")
	}
}

// TestSeparatorsSitBetweenEventsNotAround pins the divider placement: four
// separators for five events, none leading or trailing.
func TestSeparatorsSitBetweenEventsNotAround(t *testing.T) {
	body := RenderTablePage(tableEvents(5), 0, 1)["components"].([]any)[0].(map[string]any)["components"].([]any)
	var kinds []int
	for _, c := range body {
		kinds = append(kinds, int(c.(map[string]any)["type"].(int)))
	}
	separators := 0
	for _, k := range kinds {
		if k == componentTypeSeparator {
			separators++
		}
	}
	if separators != 4 {
		t.Errorf("%d separators for 5 events, want 4 (between, not around)", separators)
	}
	if kinds[0] == componentTypeSeparator || kinds[len(kinds)-1] == componentTypeSeparator {
		t.Error("a separator leads or trails the page")
	}
}
