package discordsignup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func rosterTableEvents(n int) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = Event{
			ID: int64(i + 1), GuildID: "g1", Name: fmt.Sprintf("Event %d", i+1),
			Status: StatusOpen, Capacity: 8, AttendingCount: 2,
			StartsAt: time.Now().Add(time.Duration(i+1) * time.Hour).Unix(),
		}
	}
	return out
}

func rosterOf(names ...string) []Signup {
	out := make([]Signup, 0, len(names))
	for _, n := range names {
		out = append(out, Signup{DiscordUserID: "u-" + n, DisplayName: n, State: StateAttending})
	}
	return out
}

// TestEveryPageStaysInsideDiscordsBudgets is the whole reason this is packed
// rather than counted. The event table can say "five per message" because its
// rows are all about the same size; a row here carrying twenty names is many
// times one carrying none.
func TestEveryPageStaysInsideDiscordsBudgets(t *testing.T) {
	events := rosterTableEvents(12)
	rosters := map[int64][]Signup{}
	for i := range events {
		// Wildly uneven, which is the case a fixed count gets wrong.
		var names []string
		for n := 0; n < i*7; n++ {
			names = append(names, fmt.Sprintf("A Person With A Long Name %d", n))
		}
		rosters[events[i].ID] = rosterOf(names...)
	}

	pages := packEventTable(events, rosters, eventTableButtons, 1)
	if len(pages) < 2 {
		t.Fatalf("%d pages for twelve events with big rosters, want the packer to overflow", len(pages))
	}
	seen := 0
	for i, page := range pages {
		seen += len(page)
		payload := RenderEventTablePage(page, i, len(pages), eventTableButtons, nil)
		components := countComponents(payload["components"].([]any))
		if components > eventTableComponentBudget {
			t.Errorf("page %d renders %d components, over Discord's %d",
				i, components, eventTableComponentBudget)
		}
		chars := 0
		for _, block := range page {
			chars += len([]rune(block.text))
		}
		if chars > eventTableCharBudget {
			t.Errorf("page %d holds %d characters, over the %d budget",
				i, chars, eventTableCharBudget)
		}
	}
	if seen != len(events) {
		t.Errorf("%d events across all pages, want all %d — packing dropped some",
			seen, len(events))
	}
}

// TestSmallRostersPackMoreEventsPerMessage is the other half: the packer must
// actually use the space, not just avoid overflowing it.
func TestSmallRostersPackMoreEventsPerMessage(t *testing.T) {
	events := rosterTableEvents(6)
	rosters := map[int64][]Signup{}
	for i := range events {
		rosters[events[i].ID] = rosterOf("Al", "Bo")
	}
	pages := packEventTable(events, rosters, eventTableButtons, 1)
	if len(pages[0]) < 5 {
		t.Errorf("first page holds %d events with tiny rosters, want at least 5", len(pages[0]))
	}
}

// TestTheRosterTableNamesPeopleWithoutPingingThem. It is redrawn on every
// signup, so a mention here would ping the whole roster every time somebody
// joined.
func TestTheRosterTableNamesPeopleWithoutPingingThem(t *testing.T) {
	events := rosterTableEvents(1)
	rosters := map[int64][]Signup{events[0].ID: rosterOf("Domonation", "Twili Midna")}

	payload := RenderEventTablePage(packEventTable(events, rosters, eventTableButtons, 1)[0], 0, 1, eventTableButtons, nil)
	rendered := fmt.Sprint(payload)
	if !strings.Contains(rendered, "Domonation") || !strings.Contains(rendered, "Twili Midna") {
		t.Errorf("the roster table does not name who is going: %q", rendered)
	}
	if strings.Contains(rendered, "<@") {
		t.Error("the roster table uses mentions, which ping everyone on every redraw")
	}
	mentions, ok := payload["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatal("no allowed_mentions, so a name that looks like a mention would ping")
	}
	if parse, _ := mentions["parse"].([]string); len(parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want it empty", parse)
	}
}

// TestAnEmptyGuildStillGetsAPage. Otherwise the last thing posted stays up
// saying something that stopped being true.
func TestAnEmptyGuildStillGetsAPage(t *testing.T) {
	pages := packEventTable(nil, nil, eventTableButtons, 1)
	if len(pages) != 1 {
		t.Fatalf("%d pages for no events, want 1", len(pages))
	}
	rendered := fmt.Sprint(RenderEventTablePage(pages[0], 0, 1, eventTableButtons, nil))
	if !strings.Contains(rendered, "Nothing coming up") {
		t.Errorf("an empty roster table says %q", rendered)
	}
}

// TestTheWaitlistIsNamedSeparately: the whole point is being able to see who is
// in and who is behind them without pressing anything.
func TestTheWaitlistIsNamedSeparately(t *testing.T) {
	events := rosterTableEvents(1)
	roster := rosterOf("Al", "Bo")
	roster = append(roster, Signup{DiscordUserID: "u-cy", DisplayName: "Cy",
		State: StateWaitlisted, WaitlistPlace: 1})
	pages := packEventTable(events, map[int64][]Signup{events[0].ID: roster}, eventTableButtons, 1)

	text := pages[0][0].text
	if !strings.Contains(text, "2/8 👥 Al, Bo") {
		t.Errorf("block = %q, want the count and the going list", text)
	}
	if !strings.Contains(text, "⏳ Cy") {
		t.Errorf("block = %q, want the waitlist on its own line", text)
	}
}

// TestARowSaysNothingItsThreadTitleAlreadySays. The forum post title is
// "[3/8] Board game night — 8/29 4pm" and Discord renders <#post> as exactly
// those words, so printing them beside the link said all of it twice and spent
// the room this table needs for names.
func TestARowSaysNothingItsThreadTitleAlreadySays(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Board game night", Status: StatusOpen,
		Capacity: 8, AttendingCount: 3, Location: "The shed", ForumPostID: "post-9",
		StartsAt: time.Now().Add(30 * time.Hour).Unix()}

	block := buildEventTableBlock(ev, rosterOf("Al", "Bo", "Cy"), true, eventTableButtons)
	if !strings.Contains(block.text, "<#post-9>") {
		t.Errorf("row = %q, want it to link the thread", block.text)
	}
	if strings.Contains(block.text, "Board game night") {
		t.Errorf("row = %q, repeats the name the thread title already carries", block.text)
	}
	// The count is in the row on purpose. It used to be read off the thread
	// title, and Discord rate-limits thread renames to about two per ten
	// minutes, so under signups the number people read was two renames old. A
	// message edit has no such limit.
	if !strings.Contains(block.text, "3/8") {
		t.Errorf("row = %q, want the live count in the message", block.text)
	}
	// Location is the one thing the title does not hold.
	if !strings.Contains(block.text, "The shed") {
		t.Errorf("row = %q, want the location kept", block.text)
	}
	if !strings.Contains(block.text, "3/8 👥 Al, Bo, Cy") {
		t.Errorf("row = %q, want the names", block.text)
	}
}

// TestAnEventWithNoThreadStillSaysWhatItIs: with nothing to click, the link
// would be the only content and there would be none.
func TestAnEventWithNoThreadStillSaysWhatItIs(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Unlinked", Status: StatusOpen,
		Capacity: 4, AttendingCount: 1, StartsAt: time.Now().Add(30 * time.Hour).Unix()}
	block := buildEventTableBlock(ev, rosterOf("Al"), true, eventTableButtons)
	if !strings.Contains(block.text, "Unlinked") {
		t.Errorf("row = %q, want the full line when there is no thread to link", block.text)
	}
}

// TestTheRosterTableHasNoEditButton. Details is the edit form for anybody
// allowed to use it, so a second button would open the same modal.
func TestTheRosterTableHasNoEditButton(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Games", Status: StatusOpen}
	labels := []string{}
	for _, b := range eventTableButtons(ev) {
		labels = append(labels, b.(map[string]any)["label"].(string))
	}
	if strings.Contains(strings.Join(labels, ","), "Edit") {
		t.Errorf("buttons = %v, want Details to cover editing", labels)
	}
	if len(labels) != 3 {
		t.Errorf("buttons = %v, want Join, Leave and Details", labels)
	}
}

// TestTheRowReadsLikeTheExample pins the shape asked for:
//
//	<#thread>  📍  in my butt
//	2/10 👥 Twili Midna, Slava
func TestTheRowReadsLikeTheExample(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Party", Status: StatusOpen,
		Capacity: 10, AttendingCount: 2, Location: "in my butt", ForumPostID: "post-9"}
	block := buildEventTableBlock(ev, rosterOf("Twili Midna", "Slava"), true, eventTableButtons)
	want := "<#post-9>  📍  in my butt\n2/10 👥 Twili Midna, Slava"
	if block.text != want {
		t.Errorf("row =\n%q\nwant\n%q", block.text, want)
	}
}

// TestTheManagementTableHasEditAndCreateAndNothingAMemberDoes.
func TestTheManagementTableHasEditAndCreateAndNothingAMemberDoes(t *testing.T) {
	events := rosterTableEvents(2)
	rosters := map[int64][]Signup{events[0].ID: rosterOf("Al"), events[1].ID: nil}
	pages := packEventTable(events, rosters, managementButtons, len(managementTrailing())+1)
	payload := RenderEventTablePage(pages[0], 0, 1, managementButtons, managementTrailing())
	labels := []string{}
	var walk func([]any)
	walk = func(cs []any) {
		for _, c := range cs {
			m := c.(map[string]any)
			if m["type"] == componentTypeButton {
				labels = append(labels, m["label"].(string))
			}
			if nested, ok := m["components"].([]any); ok {
				walk(nested)
			}
		}
	}
	walk(payload["components"].([]any))
	joined := strings.Join(labels, ",")
	for _, banned := range []string{"Join", "Leave", "Details"} {
		if strings.Contains(joined, banned) {
			t.Errorf("management table carries %s: %v", banned, labels)
		}
	}
	if strings.Count(joined, "Edit") != 2 {
		t.Errorf("want Edit on each of two rows, got %v", labels)
	}
	if !strings.Contains(joined, "Create an event") {
		t.Errorf("want Create on the last page, got %v", labels)
	}
	if strings.Contains(joined, "My events") {
		t.Errorf("My events is back on the management table: %v", labels)
	}
	if n := countComponents(payload["components"].([]any)); n > eventTableComponentBudget {
		t.Errorf("management page renders %d components, over %d", n, eventTableComponentBudget)
	}
}

// TestThePublicTableCarriesNoTrailingControls: Create and My events live on
// the management table only.
func TestThePublicTableCarriesNoTrailingControls(t *testing.T) {
	events := rosterTableEvents(1)
	pages := packEventTable(events, nil, eventTableButtons, 1)
	rendered := fmt.Sprint(RenderEventTablePage(pages[0], 0, 1, eventTableButtons, nil))
	if strings.Contains(rendered, "Create an event") || strings.Contains(rendered, myEventsButtonID) {
		t.Error("the public table carries management controls")
	}
}
