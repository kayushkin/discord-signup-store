package discordsignup

import (
	"fmt"
	"strings"
)

// A Discord modal holds at most five action rows, each with exactly one text
// input. Five fields is the hard ceiling, so these are the five, chosen for
// what someone actually changes while looking at the channel.
//
// What deliberately did NOT fit, and why that is survivable:
//
//	end time    — set on the web page. Most events do not need one, and the
//	              archive sweep assumes a run time when it is absent
//	timezone    — one per deployment, set in the unit, printed on the label
//	recurrence  — an RRULE typed by hand is how you get BYDAY=3TU when you
//	              meant BYDAY=TU;BYSETPOS=3
//	roles       — a role picker is a list, not a text field
//
// All four live on the web page, which has real controls for them. A modal that
// silently dropped a sixth field would be worse than one that says where it is.
const (
	fieldName        = "name"
	fieldStartsAt    = "starts"
	fieldCapacity    = "capacity"
	fieldLocation    = "location"
	fieldDescription = "description"
)

// EventForm is the five typed values, before validation.
//
// There is deliberately no EndsAt. The Discord form does not carry one, and a
// zero-valued field here would be indistinguishable from "the person cleared
// it" — which is exactly how editing from Discord would silently wipe an end
// time somebody set on the web page.
type EventForm struct {
	Name        string
	StartsAt    string
	Capacity    string
	Location    string
	Description string
}

// EventFormResult is the form after validation, ready to store.
type EventFormResult struct {
	Name        string
	StartsAt    int64
	Capacity    int
	Location    string
	Description string
}

// Validate turns typed text into values, reporting the FIRST thing wrong in
// terms of what was typed.
//
// The modal is gone by the time this answer is read, so the message has to be
// specific enough to retype from: "could not read "sept 5th"" is actionable,
// "invalid input" is not.
func (f EventForm) Validate(zone string) (*EventFormResult, error) {
	name := strings.TrimSpace(f.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: an event needs a name", ErrInvalidEvent)
	}
	if len(name) > 100 {
		return nil, fmt.Errorf("%w: Discord caps a name at 100 characters; that one is %d",
			ErrInvalidEvent, len(name))
	}
	startsAt, err := ParseEventTime(f.StartsAt, zone)
	if err != nil {
		return nil, fmt.Errorf("start time: %w", err)
	}
	if startsAt == 0 {
		return nil, fmt.Errorf("%w: a start time is required — without one the event cannot "+
			"be published to Discord", ErrInvalidEvent)
	}
	capacity, err := ParseCapacity(f.Capacity)
	if err != nil {
		return nil, err
	}
	description := strings.TrimSpace(f.Description)
	// Discord caps its own scheduled event description at 1000, and a local
	// event can be published as one. Refusing here beats being refused by
	// Discord later, when the text is no longer on screen to shorten.
	if len(description) > 1000 {
		return nil, fmt.Errorf("%w: the description is %d characters; Discord allows 1000",
			ErrInvalidEvent, len(description))
	}
	return &EventFormResult{
		Name: name, StartsAt: startsAt, Capacity: capacity,
		Location: strings.TrimSpace(f.Location), Description: description,
	}, nil
}

// modalTextInput builds one field.
func modalTextInput(customID, label, value, placeholder string, style int, required bool, maxLength int) map[string]any {
	field := map[string]any{
		"type":       componentTypeTextInput,
		"custom_id":  customID,
		"label":      truncate(label, 45),
		"style":      style,
		"value":      value,
		"required":   required,
		"max_length": maxLength,
	}
	if placeholder != "" {
		field["placeholder"] = placeholder
	}
	return field
}

// labelWrapModalRows converts Action-Row-per-input into Label-per-input.
//
// The Label carries the wording and the input keeps everything else — a wrapped
// Text Input does NOT keep its own "label" field, so it is moved rather than
// copied, or the same words would be declared twice and one of them ignored.
func labelWrapModalRows(rows []any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok || m["type"] != componentTypeActionRow {
			out = append(out, row) // already a Label, or a Text Display
			continue
		}
		inner, ok := m["components"].([]any)
		if !ok || len(inner) != 1 {
			out = append(out, row)
			continue
		}
		field, ok := inner[0].(map[string]any)
		if !ok {
			out = append(out, row)
			continue
		}
		label, _ := field["label"].(string)
		delete(field, "label")
		out = append(out, map[string]any{
			"type":      componentTypeLabel,
			"label":     label,
			"component": field,
		})
	}
	return out
}

// buildEventModal assembles the form. ev is nil when creating, in which case
// every field opens empty except the ones with a sensible starting point.
func buildEventModal(customID, title string, ev *Event, zone string) map[string]any {
	var name, starts, capacity, location, description string
	if ev != nil {
		eventZone := ev.Timezone
		if eventZone == "" {
			eventZone = zone
		}
		name = ev.Name
		starts = FormatEventTime(ev.StartsAt, eventZone)
		capacity = fmt.Sprintf("%d", ev.Capacity)
		location = ev.Location
		description = ev.Description
	} else {
		capacity = "0"
	}

	row := func(field map[string]any) map[string]any {
		return map[string]any{"type": componentTypeActionRow, "components": []any{field}}
	}
	return map[string]any{
		"custom_id": customID,
		// Discord rejects the whole interaction if the title exceeds 45
		// characters, so it is trimmed rather than sent whole and refused.
		"title": truncate(title, 45),
		"components": []any{
			row(modalTextInput(fieldName, "Name", name, "Friday playtest",
				textInputStyleShort, true, 100)),
			row(modalTextInput(fieldStartsAt, "Starts — "+zone, starts, "9/29 3   or   9/29 3:00   or   9/29 3:00pm",
				textInputStyleShort, true, 40)),
			row(modalTextInput(fieldCapacity, "Max attendees — 0 for no limit", capacity, "20",
				textInputStyleShort, true, 6)),
			row(modalTextInput(fieldLocation, "Location", location, "Where it happens",
				textInputStyleShort, false, 100)),
			row(modalTextInput(fieldDescription, "Description", description,
				"What it is, what to bring, anything else",
				textInputStyleParagraph, false, 1000)),
		},
	}
}

// buildEditModalWithRoster is the edit form with the roster written above it.
//
// One modal instead of two. Somebody changing an event's capacity wants to see
// who is already on it while they choose the number, and before this they had
// to open Details, read it, dismiss it and press Edit.
//
// The roster is a single Text Display, not one per section. Discord documents
// no total component limit for modals, and this one already spends five on the
// inputs, so the summary stays at one rather than finding that limit live.
func buildEditModalWithRoster(ev *Event, roster []Signup, zone string) map[string]any {
	modal := buildEventModal(EditModalCustomID(ev.ID), "Edit "+ev.Name, ev, zone)
	// Every input is re-wrapped in a Label, which is what makes the summary
	// legal beside them. The first attempt at this modal put a Text Display in
	// an array of Action Rows and Discord refused the whole thing — silently,
	// because a modal is validated after the interaction is answered, so it
	// showed as "This interaction failed" and left no trace in any log here.
	//
	// Discord's own reference says Action Row with Text Inputs in modals is
	// deprecated in favour of Label. A modal is evidently one shape or the
	// other, so a modal holding a Text Display has to be Label all through.
	modal["components"] = labelWrapModalRows(modal["components"].([]any))

	attending, waiting := splitRoster(roster)
	var b strings.Builder
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "**Going — %d of %d**", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "**Going — %d**, no limit", ev.AttendingCount)
	}
	if len(attending) == 0 {
		b.WriteString("\n-# Nobody yet.")
	} else {
		b.WriteString("\n" + rosterNames(attending))
	}
	if len(waiting) > 0 {
		fmt.Fprintf(&b, "\n\n**Waitlist — %d**\n%s", len(waiting), rosterNames(waiting))
	}
	// Lowering the limit never removes anyone, so somebody typing a smaller
	// number needs to know it will not — said here, where the number is typed.
	if ev.AttendingCount > 0 {
		b.WriteString("\n\n-# Lowering the limit does not remove anyone. Raising it lets " +
			"the waitlist in, oldest first.")
	}

	summary := map[string]any{
		"type": componentTypeTextDisplay, "content": trimTo(b.String(), textDisplayLimit),
	}
	modal["components"] = append([]any{summary}, modal["components"].([]any)...)
	return modal
}
