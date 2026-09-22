package discordsignup

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// modalFields pulls the text inputs out of a modal, by custom_id.
func modalFields(t *testing.T, modal map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for i, c := range modal["components"].([]any) {
		m := c.(map[string]any)
		if m["type"] != componentTypeActionRow {
			t.Fatalf("component %d is type %v; the only modal shape this service "+
				"has ever got past Discord is Action Rows holding Text Inputs", i, m["type"])
		}
		inner := m["components"].([]any)
		if len(inner) != 1 {
			t.Fatalf("component %d holds %d things, want one input", i, len(inner))
		}
		field := inner[0].(map[string]any)
		if field["type"] != componentTypeTextInput {
			t.Fatalf("component %d wraps type %v, want a text input", i, field["type"])
		}
		// Ids are scoped "name@<modal>"; tests look them up by field.
		out[strings.SplitN(field["custom_id"].(string), "@", 2)[0]] = field
	}
	return out
}

// TestNoModalCarriesATextDisplay is the test that was missing for ten days.
// Discord refuses a modal carrying one, silently: a modal is validated after
// the interaction has already been answered 200.
func TestNoModalCarriesATextDisplay(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2,
		StartsAt: 1788067881, Timezone: "America/Los_Angeles", Location: "The shed"}
	for name, modal := range map[string]map[string]any{
		"edit":    buildEventModal(EditModalCustomID(1), "Edit", ev, "America/Los_Angeles"),
		"create":  buildEventModal(CreateModalCustomID(), "New event", nil, "America/Los_Angeles"),
	} {
		for _, c := range modal["components"].([]any) {
			if c.(map[string]any)["type"] == componentTypeTextDisplay {
				t.Errorf("%s modal carries a Text Display, which Discord refuses", name)
			}
		}
	}
}

// TestAModalNeverExceedsFiveRows. Discord takes five Action Rows in a modal
// and no more.
func TestAModalNeverExceedsFiveRows(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, StartsAt: 1788067881}
	for name, modal := range map[string]map[string]any{
		"edit":    buildEventModal(EditModalCustomID(1), "Edit", ev, "America/Los_Angeles"),
		"create":  buildEventModal(CreateModalCustomID(), "New", nil, "America/Los_Angeles"),
	} {
		if n := len(modal["components"].([]any)); n > 5 {
			t.Errorf("%s modal has %d rows, over Discord's five", name, n)
		}
	}
}

// TestDetailsIsAPrivateMessageOfWhoIsOn: everything about the event, read
// only, with no box to type in. Editing lives on the management table.
func TestDetailsIsAPrivateMessageOfWhoIsOn(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2, StartsAt: 1788067881, Status: StatusOpen}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateAttending},
		{DiscordUserID: "u3", DisplayName: "Cy", State: StateWaitlisted, WaitlistPlace: 1},
		{DiscordUserID: "u4", DisplayName: "Di", State: StateMaybe},
	}
	msg := detailsMessage(ev, roster)
	if msg["flags"] != messageFlagEphemeral|messageFlagComponentsV2 {
		t.Errorf("flags = %v, want a private Components V2 message", msg["flags"])
	}
	text := detailsText(t, msg)
	for _, want := range []string{"## Games", "### Going — 2 of 4", "1. Al", "2. Bo", "### Waitlist — 1", "1. Cy", "### Maybe — 1", "Di"} {
		if !strings.Contains(text, want) {
			t.Errorf("Details = %q, want %q in it", text, want)
		}
	}
	if strings.Contains(fmt.Sprint(msg), `"type":4`) || strings.Contains(fmt.Sprint(msg), "type:4 ") {
		t.Error("Details carries a text box")
	}
}

// TestTheEditModalCarriesEveryField. Description is back, because Edit is
// no longer sharing a modal with the roster and has all five rows to itself.
func TestTheEditModalCarriesEveryField(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, StartsAt: 1788067881,
		Location: "The shed", Description: "Bring dice.", Timezone: "America/Los_Angeles"}
	fields := modalFields(t, buildEventModal(EditModalCustomID(1), "Edit", ev, "America/Los_Angeles"))
	for _, want := range []string{fieldName, fieldStartsAt, fieldCapacity, fieldLocation, fieldDescription} {
		if fields[want] == nil {
			t.Errorf("the edit modal cannot edit %q", want)
		}
	}
	if fields[fieldDescription]["value"] != "Bring dice." {
		t.Errorf("description opens with %q", fields[fieldDescription]["value"])
	}
}

// TestSubmittingTheRosterFieldChangesNothing. It is not required and looks
// editable, so somebody will type in it, and what they type must be ignored.
func TestSubmittingTheRosterFieldChangesNothing(t *testing.T) {
	form := EventForm{Name: "Games", StartsAt: "9/29 5pm", Capacity: "4", Location: "The shed"}
	if _, err := form.Validate("America/Los_Angeles"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(fmt.Sprint(form), "roster") {
		t.Error("the form carries the roster field, which would let it be saved")
	}
}

// TestEveryInputIdIsUniqueToItsModal is the fix for values rotating down a
// field on the second modal of a session. Two modals must never share an
// input id, and the reader must still find the field.
func TestEveryInputIdIsUniqueToItsModal(t *testing.T) {
	a := buildEventModal(EditModalCustomID(13), "Edit", &Event{Name: "deez"}, "UTC")
	b := buildEventModal(EditModalCustomID(19), "Edit", &Event{Name: "Test time"}, "UTC")
	idsOf := func(m map[string]any) map[string]bool {
		out := map[string]bool{}
		for _, c := range m["components"].([]any) {
			out[c.(map[string]any)["components"].([]any)[0].(map[string]any)["custom_id"].(string)] = true
		}
		return out
	}
	for id := range idsOf(a) {
		if idsOf(b)[id] {
			t.Errorf("input id %q is shared by two modals", id)
		}
		if !strings.Contains(id, "@") {
			t.Errorf("input id %q is not scoped to a modal", id)
		}
	}
	var in Interaction
	raw := fmt.Sprintf(`{"data":{"components":[{"components":[
		{"custom_id":%q,"value":"deez"},
		{"custom_id":%q,"value":"4"}]}]}}`, fieldName+"@"+EditModalCustomID(13), fieldCapacity)
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("build interaction: %v", err)
	}
	if got := in.fieldValue(fieldName); got != "deez" {
		t.Errorf("fieldValue(name) = %q through a scoped id", got)
	}
	if got := in.fieldValue(fieldCapacity); got != "4" {
		t.Errorf("fieldValue(capacity) = %q through a bare id", got)
	}
}

// TestDetailsShowsTheDescriptionFirst, after the title — it once did not
// show it at all.
func TestDetailsShowsTheDescriptionFirst(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Description: "Bring dice.", Capacity: 4, AttendingCount: 1,
		StartsAt: 1788067881, EndsAt: 1788067881 + 7200, Location: "The shed", CreatedBy: "u1"}
	text := detailsText(t, detailsMessage(ev, []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending}}))
	if !strings.HasPrefix(text, "## Games\nBring dice.") {
		t.Errorf("Details = %q, want the description right after the title", text)
	}
	for _, want := range []string{"📍 The shed", "### Going — 1 of 4", "Al", "<t:1788067881:F> – <t:1788075081:t>", "**Host:** <@u1>"} {
		if !strings.Contains(text, want) {
			t.Errorf("Details = %q, want %q in it", text, want)
		}
	}
}

// TestDetailsCarriesJoinMaybeAndLeaveOnlyWhileOpen.
func TestDetailsCarriesJoinMaybeAndLeaveOnlyWhileOpen(t *testing.T) {
	labels := func(msg map[string]any) string {
		out := []string{}
		for _, c := range msg["components"].([]any)[0].(map[string]any)["components"].([]any) {
			if m := c.(map[string]any); m["type"] == componentTypeActionRow {
				for _, b := range m["components"].([]any) {
					out = append(out, b.(map[string]any)["label"].(string))
				}
			}
		}
		return strings.Join(out, ",")
	}
	ev := &Event{ID: 1, Name: "Games", Status: StatusOpen, StartsAt: 1788067881}
	if got := labels(detailsMessage(ev, nil)); got != "Join,Maybe,Leave" {
		t.Errorf("open = %q", got)
	}
	ev.Status = StatusClosed
	if got := labels(detailsMessage(ev, nil)); got != "" {
		t.Errorf("closed = %q, want no buttons", got)
	}
}

// detailsText is the text of a Details message.
func detailsText(t *testing.T, msg map[string]any) string {
	t.Helper()
	for _, c := range msg["components"].([]any)[0].(map[string]any)["components"].([]any) {
		if m := c.(map[string]any); m["type"] == componentTypeTextDisplay {
			return m["content"].(string)
		}
	}
	t.Fatal("Details has no text")
	return ""
}
