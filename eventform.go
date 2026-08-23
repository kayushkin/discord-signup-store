package discordsignup

import (
	"fmt"
	"strings"
	"time"
)

// A Discord modal holds at most five action rows, each with exactly one text
// input. Five fields is the hard ceiling, so these are the five, chosen for
// what someone actually changes while looking at the channel.
//
// What deliberately did NOT fit, and why that is survivable:
//
//	description — long-form, and for an imported event Discord's own event
//	              editor already owns it
//	timezone    — one per deployment, set in the unit, printed in the how-to
//	recurrence  — an RRULE typed by hand is how you get BYDAY=3TU when you
//	              meant BYDAY=TU;BYSETPOS=3
//	roles       — a role picker is a list, not a text field
//
// All four live on the web page, which has real controls for them. A modal that
// silently dropped a sixth field would be worse than one that says where it is.
const (
	fieldName     = "name"
	fieldStartsAt = "starts"
	fieldEndsAt   = "ends"
	fieldCapacity = "capacity"
	fieldLocation = "location"
)

// eventTimeLayouts are the shapes a typed date is accepted in.
//
// More than one, because a person typing into a phone keyboard will produce
// several of these and refusing the near-misses teaches nothing. Seconds are
// accepted and ignored; a bare date means midnight.
var eventTimeLayouts = []string{
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 3:04pm",
	"2006-01-02 3:04 pm",
	"2006-01-02 3pm",
	"2006-01-02",
}

// ParseEventTime reads a date and time typed by a person, in a named zone.
//
// The zone is a parameter and never inferred from the text: an offset typed by
// hand cannot survive a daylight-saving change, and guessing the reader's zone
// from a server-side request is not possible at all.
func ParseEventTime(input, zone string) (int64, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return 0, nil
	}
	if zone == "" {
		return 0, fmt.Errorf("%w: no timezone is configured, so %q cannot be turned into "+
			"an actual moment", ErrInvalidEvent, trimmed)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not an IANA zone name", ErrInvalidEvent, zone)
	}
	normalised := strings.ToLower(strings.Join(strings.Fields(trimmed), " "))
	for _, layout := range eventTimeLayouts {
		if t, err := time.ParseInLocation(layout, normalised, loc); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("%w: could not read %q as a date and time — try 2026-09-05 19:00",
		ErrInvalidEvent, trimmed)
}

// FormatEventTime renders an instant back into the shape the form accepts, so
// the modal opens with something editable rather than something to retype.
func FormatEventTime(unix int64, zone string) string {
	if unix == 0 {
		return ""
	}
	loc := time.UTC
	if zone != "" {
		if parsed, err := time.LoadLocation(zone); err == nil {
			loc = parsed
		}
	}
	return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04")
}

// EventForm is the five typed values, before validation.
type EventForm struct {
	Name     string
	StartsAt string
	EndsAt   string
	Capacity string
	Location string
}

// EventFormResult is the form after validation, ready to store.
type EventFormResult struct {
	Name     string
	StartsAt int64
	EndsAt   int64
	Capacity int
	Location string
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
	endsAt, err := ParseEventTime(f.EndsAt, zone)
	if err != nil {
		return nil, fmt.Errorf("end time: %w", err)
	}
	if endsAt != 0 && endsAt < startsAt {
		return nil, fmt.Errorf("%w: the end time is before the start time", ErrInvalidEvent)
	}
	capacity, err := ParseCapacity(f.Capacity)
	if err != nil {
		return nil, err
	}
	return &EventFormResult{
		Name: name, StartsAt: startsAt, EndsAt: endsAt,
		Capacity: capacity, Location: strings.TrimSpace(f.Location),
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
	var name, starts, ends, capacity, location string
	if ev != nil {
		eventZone := ev.Timezone
		if eventZone == "" {
			eventZone = zone
		}
		name = ev.Name
		starts = FormatEventTime(ev.StartsAt, eventZone)
		ends = FormatEventTime(ev.EndsAt, eventZone)
		capacity = fmt.Sprintf("%d", ev.Capacity)
		location = ev.Location
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
			row(modalTextInput(fieldStartsAt, "Starts — "+zone, starts, "2026-09-05 19:00",
				textInputStyleShort, true, 40)),
			row(modalTextInput(fieldEndsAt, "Ends (leave blank if open-ended)", ends,
				"2026-09-05 21:00", textInputStyleShort, false, 40)),
			row(modalTextInput(fieldCapacity, "Places — 0 means no limit", capacity, "20",
				textInputStyleShort, true, 6)),
			row(modalTextInput(fieldLocation, "Location", location, "Where it happens",
				textInputStyleShort, false, 100)),
		},
	}
}
