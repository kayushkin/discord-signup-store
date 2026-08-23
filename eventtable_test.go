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

// TestPanelStaysInsideDiscordsComponentBudget is the ceiling that shapes this
// layout, and it is not a guess: measured against the live API, a container of
// sections with separators and one menu fits nine events, without separators
// eleven, with neither eighteen. Everything nested counts — the container, each
// section, the text inside it, every separator, and every option in the menu.
func TestPanelStaysInsideDiscordsComponentBudget(t *testing.T) {
	for _, n := range []int{0, 1, 5, 9, 12, 20, 50, 200} {
		payload := RenderEventPanel(tableEvents(n))
		if got := countComponents(payload["components"].([]any)); got > messageComponentBudget {
			t.Errorf("%d events render %d components, over the budget of %d",
				n, got, messageComponentBudget)
		}
	}
}

// countComponents mirrors Discord's own accounting, including select options.
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

// TestPanelGivesUpDecorationBeforeInformation pins the degradation order:
// separators are decoration and go first, then the menu, and only then are
// events dropped — and dropping them is said out loud.
func TestPanelGivesUpDecorationBeforeInformation(t *testing.T) {
	small := planPanel(tableEvents(5))
	if !small.Separators || !small.Menu || small.Dropped != 0 {
		t.Errorf("five events should get the full layout, got %+v", small)
	}
	medium := planPanel(tableEvents(11))
	if medium.Separators {
		t.Error("eleven events kept separators, which do not fit alongside the menu")
	}
	if medium.Dropped != 0 {
		t.Errorf("eleven events dropped %d — decoration should go first", medium.Dropped)
	}
	large := planPanel(tableEvents(60))
	if large.Dropped == 0 {
		t.Error("sixty events fit, which contradicts the measured budget")
	}
	content := RenderEventPanel(tableEvents(60))["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "Showing") {
		t.Errorf("heading = %q, want it to say how many were left out", content)
	}
}

// TestPanelIsSortedByDate is free here and was not in the row-per-message
// version: a single message is redrawn whole, so it is always in order, whereas
// Discord will not reorder separate messages.
func TestPanelIsSortedByDate(t *testing.T) {
	base := time.Now().Add(24 * time.Hour).Unix()
	events := []Event{
		{ID: 1, Name: "Third", Status: StatusOpen, StartsAt: base + 3000},
		{ID: 2, Name: "First", Status: StatusOpen, StartsAt: base + 1000},
		{ID: 3, Name: "Second", Status: StatusOpen, StartsAt: base + 2000},
	}
	panel := RenderEventPanel(events)["components"].([]any)[0].(map[string]any)["components"].([]any)
	var order []string
	for _, c := range panel {
		m := c.(map[string]any)
		if m["type"] != componentTypeSection {
			continue
		}
		content := m["components"].([]any)[0].(map[string]any)["content"].(string)
		order = append(order, strings.Split(strings.TrimPrefix(content, "**"), "**")[0])
	}
	want := []string{"First", "Second", "Third"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestClosedEventsGetDetailsNotJoin keeps a trap off the panel. A Section must
// have an accessory, so the button becomes Details rather than disappearing.
func TestClosedEventsGetDetailsNotJoin(t *testing.T) {
	events := tableEvents(2)
	events[0].Status = StatusClosed
	panel := RenderEventPanel(events)["components"].([]any)[0].(map[string]any)["components"].([]any)
	var accessories []string
	for _, c := range panel {
		m := c.(map[string]any)
		if m["type"] != componentTypeSection {
			continue
		}
		accessories = append(accessories, m["accessory"].(map[string]any)["custom_id"].(string))
	}
	if len(accessories) != 2 {
		t.Fatalf("got %d sections, want 2", len(accessories))
	}
	if accessories[0] != DetailsCustomID(events[0].ID) {
		t.Errorf("a closed event offers %q, want Details", accessories[0])
	}
	if accessories[1] != JoinCustomID(events[1].ID) {
		t.Errorf("an open event offers %q, want Join", accessories[1])
	}
}

// TestPanelUsesComponentsV2AndNoContent covers a rule that rejects the whole
// message: with the V2 flag set, a message must carry no content field.
func TestPanelUsesComponentsV2AndNoContent(t *testing.T) {
	payload := RenderEventPanel(tableEvents(3))
	if payload["flags"] != messageFlagComponentsV2 {
		t.Errorf("flags = %v, want the Components V2 flag", payload["flags"])
	}
	if _, present := payload["content"]; present {
		t.Error("a Components V2 message must not carry a content field")
	}
}

// TestPanelIsEditedNotReposted keeps it where people scrolled to.
func TestPanelIsEditedNotReposted(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "panel-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	if _, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "One",
		StartsAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := srv.RefreshEventPanel("g1"); err != nil {
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
	if posts != 1 || edits != 3 {
		t.Errorf("%d posts and %d edits, want 1 and 3", posts, edits)
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
