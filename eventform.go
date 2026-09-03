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
	// fieldRepeats and fieldEndsAt are the Repeat form's two inputs.
	fieldRepeats = "repeats"
	fieldEndsAt  = "ends"
	// fieldRoster carries the roster into a modal as read-only-looking text.
	// Never read back: whatever somebody types into it is thrown away.
	fieldRoster = "roster"
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
	// Every input's custom_id is scoped to THIS modal. Five edit modals in a
	// row used to send five inputs called "name", and later ones opened with
	// their values rotated down a field — the start date in the name box, the
	// limit in the start box. The server's JSON was right each time; whatever
	// the client keys a rendered modal on, it evidently outlived the modal. An
	// id no other modal has ever used cannot collide with anything cached.
	scoped := func(field string) string { return field + "@" + customID }
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
			row(modalTextInput(scoped(fieldName), "Name", name, "Friday playtest",
				textInputStyleShort, true, 100)),
			row(modalTextInput(scoped(fieldStartsAt), "Starts — "+zone, starts, "9/29 3   or   9/29 3:00   or   9/29 3:00pm",
				textInputStyleShort, true, 40)),
			row(modalTextInput(scoped(fieldCapacity), "Max attendees — 0 for no limit", capacity, "20",
				textInputStyleShort, true, 6)),
			row(modalTextInput(scoped(fieldLocation), "Location", location, "Where it happens",
				textInputStyleShort, false, 100)),
			row(modalTextInput(scoped(fieldDescription), "Description", description,
				"What it is, what to bring, anything else",
				textInputStyleParagraph, false, 1000)),
		},
	}
}

// detailsField is the whole of Details in one read-only-looking box: what the
// event is, when and where, who is going, who is waiting.
//
// A Text Input, and one, not several. Discord refused every modal this service
// sent carrying a Text Display — the only read-only text a modal offers — so
// the box is the one vehicle a modal has for words, and one box is less of a
// form than four. It is not required, is labelled read only, and whatever is
// typed into it is thrown away: EventForm has no field for it.
func detailsField(ev *Event, roster []Signup, zone string) map[string]any {
	attending, waiting := splitRoster(roster)
	var b strings.Builder
	if ev.Description != "" {
		b.WriteString(ev.Description + "\n\n")
	}
	if ev.StartsAt > 0 {
		b.WriteString(FormatEventTime(ev.StartsAt, zone))
		if ev.EndsAt > 0 {
			b.WriteString(" – " + FormatEventTime(ev.EndsAt, zone))
		}
		b.WriteString(" (" + zone + ")\n")
	}
	if ev.Location != "" {
		b.WriteString("📍 " + ev.Location + "\n")
	}
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "\nGoing — %d of %d\n", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "\nGoing — %d\n", ev.AttendingCount)
	}
	if len(attending) == 0 {
		b.WriteString("Nobody yet.")
	} else {
		b.WriteString(rosterNames(attending))
	}
	if len(waiting) > 0 {
		fmt.Fprintf(&b, "\n\nWaitlist — %d\n%s", len(waiting), rosterNames(waiting))
	}
	return modalTextInput(fieldRoster, truncate(ev.Name, 40)+" (read only)", trimTo(b.String(), 4000), "",
		textInputStyleParagraph, false, 4000)
}

// buildRosterOnlyModal is Details: the roster, and nothing to change. Editing
// lives on the management table.
func buildRosterOnlyModal(ev *Event, roster []Signup, zone string) map[string]any {
	return map[string]any{
		"custom_id": DetailsModalCustomID(ev.ID),
		"title":     truncate(ev.Name, 45),
		"components": []any{
			map[string]any{"type": componentTypeActionRow,
				"components": []any{detailsField(ev, roster, zone)}},
		},
	}
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
