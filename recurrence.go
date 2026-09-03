package discordsignup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Recurrence, in both directions.
//
// Storage is an RFC 5545 RRULE string; Discord speaks a structured object with
// its own constraints. Until 2026-09-03 only the import direction existed: a
// rule set on the web page was stored, displayed, and never sent, so Discord's
// event never repeated. The four rules an organiser can set from a Discord
// form are exactly the four Discord can represent, and nothing else is offered.
//
// Discord's constraints (guild scheduled event recurrence_rule):
//
//	WEEKLY   by_weekday of exactly one day; interval 1 or 2 — the only
//	         frequency that may have interval 2
//	MONTHLY  by_n_weekday of exactly one {n 1..5, day}
//	DAILY, YEARLY: not offered here
//	count, end, by_year_day: not settable by clients
//
// Discord numbers frequency downward (YEARLY 0, MONTHLY 1, WEEKLY 2, DAILY 3)
// and weekdays from Monday = 0.

const (
	discordFrequencyMonthly = 1
	discordFrequencyWeekly  = 2
)

var rruleDayNames = []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"} // Monday first, as Discord counts

// discordWeekday is Discord's weekday number for a Go time, Monday = 0.
func discordWeekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// startInZone is the event's start as a wall-clock time in its own zone, which
// is what "which weekday" and "which week of the month" mean to a person. The
// same instant is a different weekday in UTC often enough to matter.
func startInZone(ev *Event, fallbackZone string) time.Time {
	zone := ev.Timezone
	if zone == "" {
		zone = fallbackZone
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return time.Unix(ev.StartsAt, 0).In(loc)
}

// repeatWordToRRule turns what somebody typed into the rule stored.
//
// The weekday, and for monthly the week of the month, come from the event's
// start: "weekly" means every week on the day it starts. Returns "" for never.
func repeatWordToRRule(word string, start time.Time) (string, error) {
	w := strings.ToLower(strings.Join(strings.Fields(word), " "))
	day := rruleDayNames[discordWeekday(start)]
	switch w {
	case "", "never", "none", "no", "once", "one off", "one-off", "does not repeat":
		return "", nil
	case "weekly", "every week", "each week":
		return "FREQ=WEEKLY;BYDAY=" + day, nil
	case "every 2 weeks", "every two weeks", "every other week", "biweekly", "fortnightly", "2 weeks":
		return "FREQ=WEEKLY;INTERVAL=2;BYDAY=" + day, nil
	case "monthly", "every month", "each month":
		n := (start.Day()-1)/7 + 1
		return fmt.Sprintf("FREQ=MONTHLY;BYDAY=%d%s", n, day), nil
	}
	return "", fmt.Errorf("%w: could not read %q — try weekly, every 2 weeks, monthly, or never",
		ErrInvalidEvent, word)
}

// describeRepeat is the rule in the words the form accepts, so the form opens
// with something editable rather than an RRULE to decode by eye.
func describeRepeat(rrule string) string {
	parsed, ok := parseRRule(rrule)
	switch {
	case rrule == "":
		return "never"
	case !ok:
		return rrule // something the web page set that this form cannot express; shown as is
	case parsed.freq == "WEEKLY" && parsed.interval == 2:
		return "every 2 weeks"
	case parsed.freq == "WEEKLY":
		return "weekly"
	case parsed.freq == "MONTHLY":
		return "monthly"
	}
	return rrule
}

type rrule struct {
	freq     string
	interval int
	byDay    string // "TU" or "2TU"
}

func parseRRule(s string) (rrule, bool) {
	out := rrule{interval: 1}
	if s == "" {
		return out, false
	}
	for _, part := range strings.Split(s, ";") {
		k, v, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(part)), "=")
		switch k {
		case "FREQ":
			out.freq = v
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil {
				return out, false
			}
			out.interval = n
		case "BYDAY":
			if strings.Contains(v, ",") {
				return out, false // Discord takes one day
			}
			out.byDay = v
		default:
			return out, false // COUNT, UNTIL, BYMONTHDAY…: not something Discord accepts from us
		}
	}
	return out, out.freq != ""
}

// discordRecurrenceRule is the object to send Discord for an event's rule.
//
// ok is false when the rule cannot be expressed within Discord's constraints,
// in which case the caller leaves recurrence_rule out of the request rather
// than sending something Discord will refuse and losing the whole edit. A
// nil rule with ok true means "send null": clear it.
func discordRecurrenceRule(ev *Event, fallbackZone string) (rule map[string]any, ok bool) {
	if ev.RecurrenceRule == "" {
		return nil, true
	}
	parsed, valid := parseRRule(ev.RecurrenceRule)
	if !valid {
		return nil, false
	}
	start := startInZone(ev, fallbackZone)
	base := map[string]any{
		"start":    time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339),
		"interval": parsed.interval,
	}
	switch parsed.freq {
	case "WEEKLY":
		if parsed.interval < 1 || parsed.interval > 2 {
			return nil, false
		}
		day := discordWeekday(start)
		if parsed.byDay != "" {
			d, found := dayIndex(parsed.byDay)
			if !found {
				return nil, false
			}
			day = d
		}
		base["frequency"] = discordFrequencyWeekly
		base["by_weekday"] = []int{day}
		return base, true
	case "MONTHLY":
		if parsed.interval != 1 {
			return nil, false
		}
		n, day := (start.Day()-1)/7+1, discordWeekday(start)
		if parsed.byDay != "" {
			digits := strings.TrimRight(parsed.byDay, "MOTUWEHFRSA")
			name := strings.TrimLeft(parsed.byDay, "-0123456789")
			if digits != "" {
				v, err := strconv.Atoi(digits)
				if err != nil || v < 1 || v > 5 {
					return nil, false
				}
				n = v
			}
			d, found := dayIndex(name)
			if !found {
				return nil, false
			}
			day = d
		}
		base["frequency"] = discordFrequencyMonthly
		base["by_n_weekday"] = []map[string]any{{"n": n, "day": day}}
		return base, true
	}
	return nil, false
}

func dayIndex(name string) (int, bool) {
	for i, d := range rruleDayNames {
		if d == name {
			return i, true
		}
	}
	return 0, false
}
