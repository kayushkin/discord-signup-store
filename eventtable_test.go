package discordsignup

import (
	"encoding/json"
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

// TestEveryRowCarriesAllFourButtons is why this is one message per event.
//
// A Section's accessory can be a Button or a Thumbnail and nothing else — a
// select is rejected outright, measured against the live API — so a
// single-message layout gets exactly one button per row. An Action Row inside a
// container holds five, which is what makes all four fit.
func TestEveryRowCarriesAllFourButtons(t *testing.T) {
	ev := &Event{ID: 1, Name: "Board games", Status: StatusOpen, Capacity: 8,
		AttendingCount: 3, StartsAt: time.Now().Unix()}
	container := RenderEventRow(ev)["components"].([]any)[0].(map[string]any)
	if container["type"] != componentTypeContainer {
		t.Fatalf("row is type %v, want a container", container["type"])
	}
	inner := container["components"].([]any)
	if len(inner) != 2 {
		t.Fatalf("container holds %d components, want text and an action row", len(inner))
	}
	buttons := inner[1].(map[string]any)["components"].([]any)
	var ids []string
	for _, b := range buttons {
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
	if len(buttons) > 5 {
		t.Errorf("%d buttons, over Discord's five per action row", len(buttons))
	}
}

// TestClosedRowsLoseJoinAndLeave keeps a button that cannot act off the row.
func TestClosedRowsLoseJoinAndLeave(t *testing.T) {
	ev := &Event{ID: 4, Name: "Shut", Status: StatusClosed, Capacity: 8}
	inner := RenderEventRow(ev)["components"].([]any)[0].(map[string]any)["components"].([]any)
	buttons := inner[1].(map[string]any)["components"].([]any)
	var ids []string
	for _, b := range buttons {
		ids = append(ids, b.(map[string]any)["custom_id"].(string))
	}
	if len(ids) != 2 || ids[0] != DetailsCustomID(4) || ids[1] != EditCustomID(4) {
		t.Errorf("buttons = %v, want just Details and Edit", ids)
	}
}

// TestRowsUseComponentsV2AndNoContent covers a rule that rejects the whole
// message: with the V2 flag set, a message must carry no content field.
func TestRowsUseComponentsV2AndNoContent(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"row":    RenderEventRow(&Event{ID: 1, Name: "One", Status: StatusOpen}),
		"header": RenderTableHeader(3),
	} {
		if payload["flags"] != messageFlagComponentsV2 {
			t.Errorf("%s flags = %v, want the Components V2 flag", name, payload["flags"])
		}
		if _, present := payload["content"]; present {
			t.Errorf("%s carries a content field, which V2 forbids", name)
		}
	}
}

// TestRowsAreEditedNotReposted keeps each row where it is. A signup rewrites one
// message; anything else walks the whole table down the channel.
func TestRowsAreEditedNotReposted(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "One",
		StartsAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := srv.RefreshTableRow(ev); err != nil {
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
	// The row and the header, posted once each; three edits of the row.
	if posts != 2 || edits != 3 {
		t.Errorf("%d posts and %d edits, want 2 and 3", posts, edits)
	}
}

// TestFinishedEventsLeaveTheTable stops the board filling with things that
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
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Over",
		StartsAt: past, EndsAt: past + 3600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.RefreshTableRow(ev); err != nil {
		t.Fatalf("draw row: %v", err)
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if left, _ := store.TableRowMessageID(ev.ID); left != "" {
		t.Errorf("the row is still recorded as %q after the event finished", left)
	}
}

// TestRebuildRepostsInDateOrder covers the one thing Discord will not do:
// messages sit in posting order and cannot be moved, so sorting means deleting
// and reposting.
func TestRebuildRepostsInDateOrder(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	base := time.Now().Add(24 * time.Hour).Unix()
	for _, offset := range []int64{3, 1, 2} {
		if _, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
			Name: fmt.Sprintf("Event +%d", offset), StartsAt: base + offset*3600}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := srv.RebuildEventTable("g1"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var order []string
	for _, c := range fake.recorded() {
		if c.Method != "POST" || !strings.HasSuffix(c.Path, "/messages") {
			continue
		}
		body, _ := json.Marshal(c.Body)
		for _, offset := range []string{"+1", "+2", "+3"} {
			if strings.Contains(string(body), "Event "+offset) {
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
