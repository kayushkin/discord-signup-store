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

// TestTheMergedModalIsLabelsAllTheWayDown is the test that would have caught
// the interaction failure.
//
// A modal holding a Text Display cannot also hold Action Rows: Discord refused
// the whole payload, and refused it silently, because a modal is validated
// after the interaction has been answered. Nothing reached any log here; it
// showed only as "This interaction failed" to the person pressing.
func TestTheMergedModalIsLabelsAllTheWayDown(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1,
		StartsAt: 1788067881, Timezone: "America/Los_Angeles"}
	modal := buildEditModalWithRoster(ev, []Signup{
		{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending},
	}, "America/Los_Angeles")

	components := modal["components"].([]any)
	if len(components) != 6 {
		t.Fatalf("%d components, want one summary and five fields", len(components))
	}
	for i, c := range components {
		m := c.(map[string]any)
		switch m["type"] {
		case componentTypeTextDisplay:
			if i != 0 {
				t.Errorf("component %d is a text display; the summary goes first", i)
			}
		case componentTypeLabel:
			field, ok := m["component"].(map[string]any)
			if !ok {
				t.Errorf("component %d is a Label wrapping nothing", i)
				continue
			}
			if field["type"] != componentTypeTextInput {
				t.Errorf("component %d wraps type %v, want a text input", i, field["type"])
			}
			// The Label carries the wording; a wrapped input keeps no label of
			// its own, and declaring it twice is how one of them gets ignored.
			if _, dup := field["label"]; dup {
				t.Errorf("component %d: the wrapped input still carries its own label", i)
			}
			if m["label"] == "" || m["label"] == nil {
				t.Errorf("component %d: the Label has no wording", i)
			}
		case componentTypeActionRow:
			t.Errorf("component %d is an Action Row — this is what Discord rejected", i)
		default:
			t.Errorf("component %d is type %v, which is not modal-legal here", i, m["type"])
		}
	}
}

// TestEveryFieldSurvivesTheLabelWrap. The wrap moves the wording and must move
// nothing else: an input that lost its custom_id would submit as blank and the
// save would quietly wipe the field.
func TestEveryFieldSurvivesTheLabelWrap(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, StartsAt: 1788067881,
		Location: "The shed", Description: "Bring dice.", Timezone: "America/Los_Angeles"}
	modal := buildEditModalWithRoster(ev, nil, "America/Los_Angeles")

	byID := map[string]map[string]any{}
	for _, c := range modal["components"].([]any) {
		m := c.(map[string]any)
		if m["type"] != componentTypeLabel {
			continue
		}
		field := m["component"].(map[string]any)
		byID[field["custom_id"].(string)] = field
	}
	for _, want := range []string{fieldName, fieldStartsAt, fieldCapacity, fieldLocation, fieldDescription} {
		field, ok := byID[want]
		if !ok {
			t.Errorf("field %q did not survive the wrap", want)
			continue
		}
		if _, has := field["style"]; !has {
			t.Errorf("field %q lost its style", want)
		}
	}
	if got := byID[fieldName]["value"]; got != "Games" {
		t.Errorf("the name field opens with %q, want the current name", got)
	}
	if got := byID[fieldLocation]["value"]; got != "The shed" {
		t.Errorf("the location field opens with %q", got)
	}
}
