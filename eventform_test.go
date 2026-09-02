package discordsignup

import (
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
		out[field["custom_id"].(string)] = field
	}
	return out
}

// TestNoModalCarriesATextDisplay is the test that was missing for ten days.
//
// On 23 August the Details modal was rebuilt out of Text Displays, with a
// comment asserting Discord allows them in a modal. It does not — every such
// modal was refused, and refused SILENTLY, because a modal is validated after
// the interaction has already been answered 200. Nothing reached any log here;
// it showed only as "didn't respond in time" to whoever pressed Details.
func TestNoModalCarriesATextDisplay(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2,
		StartsAt: 1788067881, Timezone: "America/Los_Angeles", Location: "The shed"}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateWaitlisted, WaitlistPlace: 1},
	}
	for name, modal := range map[string]map[string]any{
		"viewer": buildDetailsModal(ev, roster, false, "America/Los_Angeles"),
		"editor": buildDetailsModal(ev, roster, true, "America/Los_Angeles"),
		"create": buildEventModal(CreateModalCustomID(), "New event", nil, "America/Los_Angeles"),
	} {
		rendered := fmt.Sprint(modal)
		if strings.Contains(rendered, fmt.Sprintf("%d", componentTypeTextDisplay)) {
			for _, c := range modal["components"].([]any) {
				if c.(map[string]any)["type"] == componentTypeTextDisplay {
					t.Errorf("%s modal carries a Text Display, which Discord refuses", name)
				}
			}
		}
	}
}

// TestAModalNeverExceedsFiveRows. Discord takes five Action Rows in a modal and
// no more, which is why the merged form has no Description field.
func TestAModalNeverExceedsFiveRows(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, StartsAt: 1788067881}
	for name, modal := range map[string]map[string]any{
		"viewer": buildDetailsModal(ev, nil, false, "America/Los_Angeles"),
		"editor": buildDetailsModal(ev, nil, true, "America/Los_Angeles"),
		"edit":   buildEventModal(EditModalCustomID(1), "Edit", ev, "America/Los_Angeles"),
		"create": buildEventModal(CreateModalCustomID(), "New", nil, "America/Los_Angeles"),
	} {
		if n := len(modal["components"].([]any)); n > 5 {
			t.Errorf("%s modal has %d rows, over Discord's five", name, n)
		}
	}
}

// TestTheMergedModalShowsWhoIsGoingAndEdits is the merge itself.
func TestTheMergedModalShowsWhoIsGoingAndEdits(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2,
		StartsAt: 1788067881, Location: "The shed", Timezone: "America/Los_Angeles"}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateAttending},
		{DiscordUserID: "u3", DisplayName: "Cy", State: StateWaitlisted, WaitlistPlace: 1},
	}
	modal := buildDetailsModal(ev, roster, true, "America/Los_Angeles")
	fields := modalFields(t, modal)

	shown := fields[fieldRoster]
	if shown == nil {
		t.Fatal("the merged modal does not show the roster")
	}
	for _, want := range []string{"Al", "Bo", "Waitlist", "Cy"} {
		if !strings.Contains(shown["value"].(string), want) {
			t.Errorf("roster field = %q, want %q in it", shown["value"], want)
		}
	}
	if shown["required"] != false {
		t.Error("the roster field is required, so a viewer cannot submit without editing it")
	}
	if !strings.Contains(shown["label"].(string), "read only") {
		t.Errorf("roster label = %q; it looks editable, so it has to say it is not", shown["label"])
	}
	for _, want := range []string{fieldName, fieldStartsAt, fieldCapacity, fieldLocation} {
		if fields[want] == nil {
			t.Errorf("the merged modal cannot edit %q", want)
		}
	}
	if fields[fieldName]["value"] != "Games" {
		t.Errorf("name opens with %q, want the current name", fields[fieldName]["value"])
	}
	if id := modal["custom_id"].(string); !strings.Contains(id, "edit-modal") {
		t.Errorf("custom_id = %q, want it to submit as an edit", id)
	}
}

// TestAViewerGetsTheRosterAndNothingToChange.
func TestAViewerGetsTheRosterAndNothingToChange(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	modal := buildDetailsModal(ev, []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending}}, false, "")
	fields := modalFields(t, modal)

	if len(fields) != 1 || fields[fieldRoster] == nil {
		t.Fatalf("a viewer's modal holds %v, want only the roster", fields)
	}
	if id := modal["custom_id"].(string); !strings.Contains(id, "details-modal") {
		t.Errorf("custom_id = %q, want the view-only form's", id)
	}
}

// TestSubmittingTheRosterFieldChangesNothing. It is not required and looks
// editable, so somebody will type in it, and what they type must be ignored
// rather than saved over the event.
func TestSubmittingTheRosterFieldChangesNothing(t *testing.T) {
	form := EventForm{
		Name: "Games", StartsAt: "9/29 5pm", Capacity: "4", Location: "The shed",
	}
	result, err := form.Validate("America/Los_Angeles")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if result.Name != "Games" {
		t.Errorf("name = %q", result.Name)
	}
	// The roster field is not one of EventForm's, so it cannot reach a patch.
	if strings.Contains(fmt.Sprint(form), "roster") {
		t.Error("the form carries the roster field, which would let it be saved")
	}
}
