package discordsignup

import (
	"strings"
	"testing"
)

// TestTheHostIsUnderlinedInTheTable, on whichever list they are on, and
// nobody else is.
func TestTheHostIsUnderlinedInTheTable(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Status: StatusOpen, CreatedBy: "u-kat", AttendingCount: 2}
	roster := []Signup{
		{DiscordUserID: "u-al", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u-kat", DisplayName: "Crab 🦀", ReadableName: "Kat", State: StateAttending},
		{DiscordUserID: "u-cy", DisplayName: "cy_the_great", State: StateMaybe},
	}
	text := buildEventTableBlock(ev, roster, true, eventTableButtons).text
	if !strings.Contains(text, "**Going** ✅ Al, __Kat__") {
		t.Errorf("row = %q, want Kat underlined after Al", text)
	}
	ev.CreatedBy = "u-cy"
	text = buildEventTableBlock(ev, roster, true, eventTableButtons).text
	if !strings.Contains(text, `**Maybe** 🤷 __cy\_the\_great__`) {
		t.Errorf("row = %q, want the host underlined on Maybe with their underscores escaped", text)
	}
	if strings.Contains(text, "__Kat__") {
		t.Errorf("row = %q, underlines someone who is not the host", text)
	}
}

// TestTheForumPostNamesTheHost without pinging them.
func TestTheForumPostNamesTheHost(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Status: StatusOpen, CreatedBy: "u-kat", StartsAt: 1790000000}
	card := RenderForumCard(ev, nil)
	if content, _ := card["content"].(string); !strings.Contains(content, "**Host:** <@u-kat>") {
		t.Errorf("card = %q, want a host line", content)
	}
	if mentions, _ := card["allowed_mentions"].(map[string]any); mentions == nil {
		t.Error("the card can ping the host")
	}
	ev.CreatedBy = ""
	if content, _ := RenderForumCard(ev, nil)["content"].(string); strings.Contains(content, "Host:") {
		t.Errorf("an event with no creator names a host: %q", content)
	}
}

// TestHandingOverAnEventRedrawsIt.
func TestHandingOverAnEventRedrawsIt(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", CreatedBy: "u-a"}
	before := eventPublishSignature(ev, nil)
	ev.CreatedBy = "u-b"
	if eventPublishSignature(ev, nil) == before {
		t.Error("a new host signs the same as the old one, so nothing would redraw")
	}
}
