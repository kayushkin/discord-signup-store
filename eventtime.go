package discordsignup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reading a date and time somebody typed.
//
// One parser, used by the Discord modal and the web form alike, because two
// would drift and the same string would mean two things depending on where it
// was typed.
//
// The shapes accepted, all of them optional in either half:
//
//	date   9/29 · 09/29 · 9/29/26 · 9/29/2026 · 2026-09-29 · sep 29 · 29 sep
//	       sept 29th · today · tomorrow · friday · fri
//	time   5pm · 5 pm · 5:30pm · 5:30 · 14:30 · 1730 · 1330pm · noon · midnight
//
// A date with no time means midnight. A time with no date means the next day
// that time occurs. A date with no year means the next year it occurs, so on
// 2 September "9/29" is this year and "1/5" is next.
//
// ⚠️ A time with no am/pm and an hour of 1–11 is read as PM. That is a choice,
// not an accident: this field only ever schedules events, and 5:30 in the
// morning is not one. It applies to "09:00" exactly as it does to "9:00" — a
// rule that turned on a leading zero would put two nearly identical strings
// twelve hours apart, which is worse than one rule that is always stated. The
// field label says so, and every rendered time carries an explicit am/pm so
// that reopening a form and saving it cannot move the event.

// isoTimeSeparator matches the T in "2026-09-29T17:30" — only between digits,
// so it cannot eat the t in "sept".
var isoTimeSeparator = regexp.MustCompile(`(\d)t(\d)`)

var (
	slashDatePattern   = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})(?:/(\d{2}|\d{4}))?$`)
	isoDatePattern     = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	clockTimePattern   = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)
	compactTimePattern = regexp.MustCompile(`^(\d{3,4})(am|pm)?$`)
	ordinalSuffix      = regexp.MustCompile(`^(\d{1,2})(?:st|nd|rd|th)$`)
)

// monthNames covers the abbreviation, the full name, and "sept" — which is
// what people actually type and what a three-letter table misses.
var monthNames = map[string]time.Month{
	"jan": time.January, "january": time.January,
	"feb": time.February, "february": time.February,
	"mar": time.March, "march": time.March,
	"apr": time.April, "april": time.April,
	"may": time.May,
	"jun": time.June, "june": time.June,
	"jul": time.July, "july": time.July,
	"aug": time.August, "august": time.August,
	"sep": time.September, "sept": time.September, "september": time.September,
	"oct": time.October, "october": time.October,
	"nov": time.November, "november": time.November,
	"dec": time.December, "december": time.December,
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

type dateKind int

const (
	dateAbsent dateKind = iota
	dateCalendar
	dateToday
	dateTomorrow
	dateWeekday
)

// datePart is a date as typed, before it is resolved against a clock. The year
// is separately flagged because "9/29" and "9/29/2026" resolve differently.
type datePart struct {
	kind      dateKind
	month     time.Month
	day       int
	year      int
	yearKnown bool
	weekday   time.Weekday
}

// timePart is a time as typed. meridiem is empty when none was written, which
// is the case the PM rule exists for.
type timePart struct {
	present  bool
	hour     int
	minute   int
	meridiem string
}

// hourMinute applies the am/pm rules and reports whether the result is a real
// time of day.
func (t timePart) hourMinute() (hour, minute int, ok bool) {
	hour, minute = t.hour, t.minute
	if minute > 59 {
		return 0, 0, false
	}
	switch t.meridiem {
	case "am":
		if hour > 12 {
			return 0, 0, false // "13am" is not a time anybody means
		}
		if hour == 12 {
			hour = 0 // 12am is midnight
		}
	case "pm":
		if hour < 12 {
			hour += 12
		}
		// An hour already past noon with "pm" on it — "1330pm" — keeps the
		// hour and drops the redundant word rather than being refused. The
		// digits say one thing only, so there is nothing to resolve.
	default:
		// The rule: unmarked evening hours are evening hours.
		if hour >= 1 && hour <= 11 {
			hour += 12
		}
	}
	if hour > 23 {
		return 0, 0, false
	}
	return hour, minute, true
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
		return 0, fmt.Errorf("%w: a time needs a timezone — %q on its own is not an instant",
			ErrInvalidEvent, trimmed)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not an IANA zone name", ErrInvalidEvent, zone)
	}
	return parseEventTimeIn(trimmed, loc, time.Now().In(loc))
}

// parseEventTimeIn is the parser with its clock injected, so the defaulting
// rules can be tested without waiting for a particular Tuesday.
func parseEventTimeIn(input string, loc *time.Location, now time.Time) (int64, error) {
	normalised := strings.ToLower(strings.Join(strings.Fields(input), " "))
	normalised = strings.ReplaceAll(normalised, ",", " ")
	normalised = isoTimeSeparator.ReplaceAllString(normalised, "$1 $2")
	fields := strings.Fields(normalised)

	// Every split of the words into a date half and a time half, longest date
	// first: "sep 29" is a date, not the month of September at 29 o'clock.
	for i := len(fields); i >= 0; i-- {
		date, ok := parseDateFields(fields[:i])
		if !ok {
			continue
		}
		clock, ok := parseTimeFields(fields[i:])
		if !ok {
			continue
		}
		if unix, ok := resolve(date, clock, loc, now); ok {
			return unix, nil
		}
	}
	return 0, fmt.Errorf("%w: could not read %q as a date and time — try 9/29 5pm, "+
		"5:30, 14:30, tomorrow 7pm or 2026-09-29 19:00", ErrInvalidEvent, input)
}

// resolve turns the typed halves into an instant, filling in whatever was not
// typed from the clock.
func resolve(date datePart, clock timePart, loc *time.Location, now time.Time) (int64, bool) {
	if date.kind == dateAbsent && !clock.present {
		return 0, false
	}
	hour, minute := 0, 0 // a date with no time means the start of that day
	if clock.present {
		var ok bool
		if hour, minute, ok = clock.hourMinute(); !ok {
			return 0, false
		}
	}

	switch date.kind {
	case dateAbsent:
		// The next time that clock reading comes round.
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at.Unix(), true

	case dateToday:
		return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc).Unix(), true

	case dateTomorrow:
		return time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, loc).Unix(), true

	case dateWeekday:
		// "friday" means the coming Friday. Said on a Friday it means today if
		// the time is still ahead, and a week out if it is not — nobody says
		// "friday" at 9pm on Friday meaning the hour that just passed.
		ahead := (int(date.weekday) - int(now.Weekday()) + 7) % 7
		at := time.Date(now.Year(), now.Month(), now.Day()+ahead, hour, minute, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 7)
		}
		return at.Unix(), true

	case dateCalendar:
		year := date.year
		if !date.yearKnown {
			// Roll to next year only when the DAY is behind us, never the
			// hour. "9/2" typed on 2 September means today even though its
			// midnight has long gone — comparing instants here sent it a full
			// year out, which is the most confident kind of wrong.
			year = now.Year()
			typed := time.Date(year, date.month, date.day, 0, 0, 0, 0, loc)
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			if typed.Before(today) {
				year++
			}
		}
		at := time.Date(year, date.month, date.day, hour, minute, 0, 0, loc)
		// time.Date rolls 30 February into 2 March rather than refusing it, so
		// a date that came back as a different day was never a real date.
		if at.Month() != date.month || at.Day() != date.day {
			return 0, false
		}
		return at.Unix(), true
	}
	return 0, false
}

func parseDateFields(fields []string) (datePart, bool) {
	switch len(fields) {
	case 0:
		return datePart{kind: dateAbsent}, true

	case 1:
		word := fields[0]
		switch word {
		case "today", "tonight":
			return datePart{kind: dateToday}, true
		case "tomorrow", "tmrw":
			return datePart{kind: dateTomorrow}, true
		}
		if weekday, ok := weekdayNames[word]; ok {
			return datePart{kind: dateWeekday, weekday: weekday}, true
		}
		if m := slashDatePattern.FindStringSubmatch(word); m != nil {
			return calendarDate(atoi(m[1]), atoi(m[2]), m[3])
		}
		if m := isoDatePattern.FindStringSubmatch(word); m != nil {
			return calendarDate(atoi(m[2]), atoi(m[3]), m[1])
		}
		return datePart{}, false

	case 2:
		// "sep 29" and "29 sep", either way round.
		if month, ok := monthNames[fields[0]]; ok {
			if day, ok := dayNumber(fields[1]); ok {
				return calendarDate(int(month), day, "")
			}
		}
		if month, ok := monthNames[fields[1]]; ok {
			if day, ok := dayNumber(fields[0]); ok {
				return calendarDate(int(month), day, "")
			}
		}
		return datePart{}, false

	case 3:
		// The same two with a year on the end.
		if month, ok := monthNames[fields[0]]; ok {
			if day, ok := dayNumber(fields[1]); ok {
				return calendarDate(int(month), day, fields[2])
			}
		}
		if month, ok := monthNames[fields[1]]; ok {
			if day, ok := dayNumber(fields[0]); ok {
				return calendarDate(int(month), day, fields[2])
			}
		}
		return datePart{}, false
	}
	return datePart{}, false
}

// calendarDate assembles a month and day, and a year if one was written.
//
// Month first: "5/6" is 6 May. US order, because that is the order the person
// this runs for types in, and a rule that guessed per-value would read 5/6 and
// 6/5 as the same date.
func calendarDate(month, day int, year string) (datePart, bool) {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return datePart{}, false
	}
	out := datePart{kind: dateCalendar, month: time.Month(month), day: day}
	if year == "" {
		return out, true
	}
	n, err := strconv.Atoi(year)
	if err != nil {
		return datePart{}, false
	}
	if n < 100 {
		n += 2000
	}
	if n < 1970 || n > 9999 {
		return datePart{}, false
	}
	out.year, out.yearKnown = n, true
	return out, true
}

// dayNumber reads "29" and "29th" alike.
func dayNumber(word string) (int, bool) {
	if m := ordinalSuffix.FindStringSubmatch(word); m != nil {
		word = m[1]
	}
	n, err := strconv.Atoi(word)
	if err != nil || n < 1 || n > 31 {
		return 0, false
	}
	return n, true
}

func parseTimeFields(fields []string) (timePart, bool) {
	switch len(fields) {
	case 0:
		return timePart{}, true

	case 1:
		return parseTimeWord(fields[0], "")

	case 2:
		// "5:30 pm", where the meridiem was typed as its own word.
		switch fields[1] {
		case "am", "a.m.", "pm", "p.m.":
			return parseTimeWord(fields[0], strings.ReplaceAll(fields[1], ".", ""))
		}
		return timePart{}, false
	}
	return timePart{}, false
}

func parseTimeWord(word, meridiem string) (timePart, bool) {
	switch word {
	case "noon", "midday":
		return timePart{present: true, hour: 12, meridiem: "pm"}, true
	case "midnight":
		return timePart{present: true, hour: 12, meridiem: "am"}, true
	}
	if m := clockTimePattern.FindStringSubmatch(word); m != nil {
		hour := atoi(m[1])
		if hour > 23 {
			return timePart{}, false
		}
		written := m[3]
		if written == "" {
			written = meridiem
		} else if meridiem != "" && written != meridiem {
			return timePart{}, false // "5pm am"
		}
		return timePart{present: true, hour: hour, minute: atoi(m[2]), meridiem: written}, true
	}
	if m := compactTimePattern.FindStringSubmatch(word); m != nil {
		digits := m[1]
		if len(digits) == 3 {
			digits = "0" + digits
		}
		hour, minute := atoi(digits[:2]), atoi(digits[2:])
		if hour > 23 {
			return timePart{}, false
		}
		written := m[2]
		if written == "" {
			written = meridiem
		} else if meridiem != "" && written != meridiem {
			return timePart{}, false
		}
		return timePart{present: true, hour: hour, minute: minute, meridiem: written}, true
	}
	return timePart{}, false
}

// atoi is for digits a regexp already proved are digits. An empty group is 0,
// which is what an omitted ":mm" means.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// FormatEventTime renders an instant back into the shape the field accepts, so
// a form opens with something editable rather than something to retype.
//
// The am/pm is not decoration and must never be dropped. Without it, "09:00"
// read back through the PM rule above becomes 21:00, so opening a form and
// saving it unchanged would move a 9am event to the evening. This is the one
// thing that keeps the parser and the renderer honest about each other, and
// TestEveryRenderedTimeSurvivesBeingReadBack holds it there.
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
	return time.Unix(unix, 0).In(loc).Format("2006-01-02 3:04pm")
}
