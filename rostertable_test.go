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

	pages := packRosterTable(events, rosters)
	if len(pages) < 2 {
		t.Fatalf("%d pages for twelve events with big rosters, want the packer to overflow", len(pages))
	}
	seen := 0
	for i, page := range pages {
		seen += len(page)
		payload := RenderRosterTablePage(page, i, len(pages))
		components := countComponents(payload["components"].([]any))
		if components > rosterTableComponentBudget {
			t.Errorf("page %d renders %d components, over Discord's %d",
				i, components, rosterTableComponentBudget)
		}
		chars := 0
		for _, block := range page {
			chars += len([]rune(block.text))
		}
		if chars > rosterTableCharBudget {
			t.Errorf("page %d holds %d characters, over the %d budget",
				i, chars, rosterTableCharBudget)
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
	pages := packRosterTable(events, rosters)
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

	payload := RenderRosterTablePage(packRosterTable(events, rosters)[0], 0, 1)
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
	pages := packRosterTable(nil, nil)
	if len(pages) != 1 {
		t.Fatalf("%d pages for no events, want 1", len(pages))
	}
	rendered := fmt.Sprint(RenderRosterTablePage(pages[0], 0, 1))
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
	pages := packRosterTable(events, map[int64][]Signup{events[0].ID: roster})

	text := pages[0][0].text
	if !strings.Contains(text, "Going: Al, Bo") {
		t.Errorf("block = %q, want the going list", text)
	}
	if !strings.Contains(text, "Waiting: Cy") {
		t.Errorf("block = %q, want the waitlist named separately", text)
	}
}
