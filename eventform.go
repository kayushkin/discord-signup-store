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
			row(modalTextInput(fieldStartsAt, "Starts — "+zone, starts, "9/29 5pm · 14:30 · tomorrow 6pm",
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
