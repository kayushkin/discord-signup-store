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
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateWaitlisted, WaitlistPlace: 1},
	}
	for name, modal := range map[string]map[string]any{
		"details": buildRosterOnlyModal(ev, roster),
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
		"details": buildRosterOnlyModal(ev, nil),
		"edit":    buildEventModal(EditModalCustomID(1), "Edit", ev, "America/Los_Angeles"),
		"create":  buildEventModal(CreateModalCustomID(), "New", nil, "America/Los_Angeles"),
	} {
		if n := len(modal["components"].([]any)); n > 5 {
			t.Errorf("%s modal has %d rows, over Discord's five", name, n)
		}
	}
}

// TestDetailsIsTheRosterAndNothingToChange. Editing lives on the management
// table; Details shows who is going, to everybody, and nothing else.
func TestDetailsIsTheRosterAndNothingToChange(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2, StartsAt: 1788067881}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateAttending},
		{DiscordUserID: "u3", DisplayName: "Cy", State: StateWaitlisted, WaitlistPlace: 1},
	}
	modal := buildRosterOnlyModal(ev, roster)
	fields := modalFields(t, modal)
	if len(fields) != 1 || fields[fieldRoster] == nil {
		t.Fatalf("Details holds %v, want only the roster", fields)
	}
	shown := fields[fieldRoster]
	for _, want := range []string{"Al", "Bo", "Waitlist", "Cy"} {
		if !strings.Contains(shown["value"].(string), want) {
			t.Errorf("roster field = %q, want %q in it", shown["value"], want)
		}
	}
	if shown["required"] != false {
		t.Error("the roster field is required, so a viewer cannot dismiss without editing it")
	}
	if !strings.Contains(shown["label"].(string), "read only") {
		t.Errorf("roster label = %q; it looks editable, so it has to say it is not", shown["label"])
	}
	if id := modal["custom_id"].(string); !strings.Contains(id, "details-modal") {
		t.Errorf("custom_id = %q, want the view-only form's", id)
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
