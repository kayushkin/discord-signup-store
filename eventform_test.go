package discordsignup

import (
	"fmt"
	"strings"
	"testing"
)

// TestTheEditModalShowsWhoIsGoing is the merge: somebody choosing a new
// capacity wants the roster in front of them while they choose it, and before
// this they had to open Details, read it, dismiss it and press Edit.
func TestTheEditModalShowsWhoIsGoing(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 2,
		StartsAt: 1788067881, Timezone: "America/Los_Angeles"}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bo", State: StateAttending},
		{DiscordUserID: "u3", DisplayName: "Cy", State: StateWaitlisted, WaitlistPlace: 1},
	}
	modal := buildEditModalWithRoster(ev, roster, "America/Los_Angeles")

	components := modal["components"].([]any)
	if len(components) != 6 {
		t.Fatalf("%d components, want one summary block and five inputs", len(components))
	}
	summary := components[0].(map[string]any)
	if summary["type"] != componentTypeTextDisplay {
		t.Errorf("first component is type %v, want a text display", summary["type"])
	}
	content := summary["content"].(string)
	for _, want := range []string{"Going — 2 of 4", "Al", "Bo", "Waitlist", "Cy"} {
		if !strings.Contains(content, want) {
			t.Errorf("summary = %q, want it to mention %q", content, want)
		}
	}
	// It submits as an edit, so the existing save path handles it.
	if id := modal["custom_id"].(string); !strings.Contains(id, "edit-modal") {
		t.Errorf("custom_id = %q, want the edit form's", id)
	}
}

// TestAViewerGetsNoInputs. Discord shows a component to everybody or nobody, so
// the check happens on the press: somebody who may not edit gets the roster and
// nothing to type in.
func TestAViewerGetsNoInputs(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	modal := buildDetailsModal(ev, []Signup{{DiscordUserID: "u1", DisplayName: "Al",
		State: StateAttending}}, false, "America/Los_Angeles")

	rendered := fmt.Sprint(modal)
	if strings.Contains(rendered, fmt.Sprint(componentTypeTextInput)) &&
		strings.Contains(rendered, fieldCapacity) {
		t.Error("a viewer's modal carries edit inputs")
	}
	if id := modal["custom_id"].(string); !strings.Contains(id, "details-modal") {
		t.Errorf("custom_id = %q, want the view-only form's", id)
	}
}
