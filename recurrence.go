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
// event never repeated. Until 2026-09-04 only weekly, every-other-week and
// monthly were understood, and an event made "every day" in Discord's own
// form came in as FREQ=DAILY, which nothing could describe, send or roll —
// the table showed the raw RRULE beside "weekly" on the next row.
//
// What is understood now is exactly what Discord's form can make, read from
// its documented business rules (docs.discord.com, guild-scheduled-event):
//
//	DAILY    every day, or a "known set" of weekdays: Mon–Fri, Tue–Sat,
//	         Sun–Thu, Fri–Sat, Sat–Sun, Sun–Mon; interval 1
//	WEEKLY   by_weekday of exactly one day; interval 1 or 2 — the only
//	         frequency that may have interval 2
//	MONTHLY  by_n_weekday of exactly one {n 1..5, day}; interval 1
//	YEARLY   by_month and by_month_day of exactly one each; interval 1
//	count, end, by_year_day: not settable by clients, so no series end
//
// Anything else is refused at write time (validateRecurrence), so a rule that
// cannot be described, sent and rolled is never stored. Discord numbers
// frequency downward (YEARLY 0, MONTHLY 1, WEEKLY 2, DAILY 3) and weekdays
// from Monday = 0.

const (
	discordFrequencyYearly  = 0
	discordFrequencyMonthly = 1
	discordFrequencyWeekly  = 2
	discordFrequencyDaily   = 3
)

var rruleDayNames = []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"} // Monday first, as Discord counts

// dailyKnownSets are the only weekday sets Discord accepts on a DAILY rule,
// keyed by the RRULE BYDAY list in Discord's own order, with the word each
// form uses for it.
var dailyKnownSets = map[string]string{
	"MO,TU,WE,TH,FR": "every weekday",
	"TU,WE,TH,FR,SA": "Tuesday to Saturday",
	"SU,MO,TU,WE,TH": "Sunday to Thursday",
	"FR,SA":          "Fridays and Saturdays",
	"SA,SU":          "weekends",
	"SU,MO":          "Sundays and Mondays",
}

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

// repeatWords is what the Repeat form accepts, for its label and its error.
const repeatWords = "daily, weekdays, weekly, every 2 weeks, monthly, yearly, or never"

// repeatWordToRRule turns what somebody typed into the rule stored.
//
// The weekday, the week of the month and the date of the year come from the
// event's start: "weekly" means every week on the day it starts. Returns ""
// for never.
func repeatWordToRRule(word string, start time.Time) (string, error) {
	w := strings.ToLower(strings.Join(strings.Fields(word), " "))
	day := rruleDayNames[discordWeekday(start)]
	switch w {
	case "", "never", "none", "no", "once", "one off", "one-off", "does not repeat":
		return "", nil
	case "daily", "every day", "each day":
		return "FREQ=DAILY", nil
	case "weekdays", "every weekday", "each weekday", "mon-fri", "monday to friday":
		return "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR", nil
	case "weekly", "every week", "each week":
		return "FREQ=WEEKLY;BYDAY=" + day, nil
	case "every 2 weeks", "every two weeks", "every other week", "biweekly", "fortnightly", "2 weeks":
		return "FREQ=WEEKLY;INTERVAL=2;BYDAY=" + day, nil
	case "monthly", "every month", "each month":
		n := (start.Day()-1)/7 + 1
		return fmt.Sprintf("FREQ=MONTHLY;BYDAY=%d%s", n, day), nil
	case "yearly", "annually", "every year", "each year":
		return fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYMONTHDAY=%d", int(start.Month()), start.Day()), nil
	}
	return "", fmt.Errorf("%w: could not read %q — try %s", ErrInvalidEvent, word, repeatWords)
}

// describeRepeat is the rule in words, for every surface that says an event
// repeats. Never the RRULE: a rule this cannot describe cannot be stored,
// because validateRecurrence runs the same parser first.
func describeRepeat(rrule string) string {
	if rrule == "" {
		return "never"
	}
	parsed, ok := parseRRule(rrule)
	if !ok {
		return "on a schedule this service cannot read" // unreachable for stored rules
	}
	switch parsed.freq {
	case "DAILY":
		if parsed.byDay == "" {
			return "daily"
		}
		return dailyKnownSets[parsed.byDay]
	case "WEEKLY":
		if parsed.interval == 2 {
			return "every 2 weeks"
		}
		return "weekly"
	case "MONTHLY":
		return "monthly"
	case "YEARLY":
		return "yearly"
	}
	return "on a schedule this service cannot read"
}

// rrule is a parsed rule, holding only the shapes Discord can take.
type rrule struct {
	freq     string
	interval int
	byDay    string // "TU", "2TU", or for DAILY a known set "MO,TU,WE,TH,FR"
	byMonth  int    // YEARLY only
	monthDay int    // YEARLY only
}

// parseRRule reads a rule and reports whether it is one this service can
// describe, send to Discord and roll forward. ok is false for everything
// outside Discord's business rules — COUNT, UNTIL, an interval over two, a
// weekly rule with several days, a daily set Discord does not know.
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
			out.byDay = v
		case "BYMONTH":
			n, err := strconv.Atoi(v)
			if err != nil {
				return out, false
			}
			out.byMonth = n
		case "BYMONTHDAY":
			n, err := strconv.Atoi(v)
			if err != nil {
				return out, false
			}
			out.monthDay = n
		default:
			return out, false // COUNT, UNTIL, BYSETPOS…: not something Discord accepts from us
		}
	}
	switch out.freq {
	case "DAILY":
		if out.interval != 1 || out.byMonth != 0 || out.monthDay != 0 {
			return out, false
		}
		if out.byDay != "" {
			if _, known := dailyKnownSets[out.byDay]; !known {
				return out, false
			}
		}
	case "WEEKLY":
		if out.interval < 1 || out.interval > 2 || out.byMonth != 0 || out.monthDay != 0 {
			return out, false
		}
		if out.byDay != "" {
			if _, found := dayIndex(out.byDay); !found {
				return out, false
			}
		}
	case "MONTHLY":
		if out.interval != 1 || out.byMonth != 0 || out.monthDay != 0 {
			return out, false
		}
		if out.byDay != "" {
			if _, _, ok := nthWeekday(out.byDay); !ok {
				return out, false
			}
		}
	case "YEARLY":
		if out.interval != 1 || out.byDay != "" {
			return out, false
		}
		if (out.byMonth == 0) != (out.monthDay == 0) {
			return out, false // both or neither: neither means "the start's date"
		}
		if out.byMonth != 0 && (out.byMonth < 1 || out.byMonth > 12 || out.monthDay < 1 || out.monthDay > 31) {
			return out, false
		}
	default:
		return out, false
	}
	return out, true
}

// nthWeekday reads "2TU" as (2, Tuesday).
func nthWeekday(byDay string) (n, day int, ok bool) {
	digits := strings.TrimRight(byDay, "MOTUWEHFRSA")
	name := strings.TrimLeft(byDay, "-0123456789")
	n = 0
	if digits != "" {
		v, err := strconv.Atoi(digits)
		if err != nil || v < 1 || v > 5 {
			return 0, 0, false
		}
		n = v
	}
	day, found := dayIndex(name)
	if !found {
		return 0, 0, false
	}
	return n, day, true
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
	case "DAILY":
		base["frequency"] = discordFrequencyDaily
		if parsed.byDay != "" {
			days := []int{}
			for _, name := range strings.Split(parsed.byDay, ",") {
				d, _ := dayIndex(name)
				days = append(days, d)
			}
			base["by_weekday"] = days
		}
		return base, true
	case "WEEKLY":
		day := discordWeekday(start)
		if parsed.byDay != "" {
			day, _ = dayIndex(parsed.byDay)
		}
		base["frequency"] = discordFrequencyWeekly
		base["by_weekday"] = []int{day}
		return base, true
	case "MONTHLY":
		n, day := (start.Day()-1)/7+1, discordWeekday(start)
		if parsed.byDay != "" {
			pn, pd, _ := nthWeekday(parsed.byDay)
			if pn != 0 {
				n = pn
			}
			day = pd
		}
		base["frequency"] = discordFrequencyMonthly
		base["by_n_weekday"] = []map[string]any{{"n": n, "day": day}}
		return base, true
	case "YEARLY":
		month, day := int(start.Month()), start.Day()
		if parsed.byMonth != 0 {
			month, day = parsed.byMonth, parsed.monthDay
		}
		base["frequency"] = discordFrequencyYearly
		base["by_month"] = []int{month}
		base["by_month_day"] = []int{day}
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

// nextOccurrence is the first time the rule fires strictly after `after`,
// counting from `start`, both wall-clock times in the event's zone. Discord
// carries a series as one event whose start slides forward when an occurrence
// ends — measured 2026-09-03: a weekly probe kept its id and its start moved
// exactly a week — and this is the same arithmetic done locally, so the row
// can move on before Discord's copy is read back.
//
// ok is false for a rule this service cannot expand, which is the same set
// parseRRule refuses; such a rule never sends and never rolls.
func nextOccurrence(rule string, start, after time.Time) (next time.Time, ok bool) {
	parsed, valid := parseRRule(rule)
	if !valid {
		return time.Time{}, false
	}
	loc := start.Location()
	hour, minute, second := start.Clock()
	// Date arithmetic is done on calendar days so a clock change moves the
	// instant, not the hour.
	at := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, hour, minute, second, 0, loc)
	}
	switch parsed.freq {
	case "DAILY":
		allowed := map[int]bool{}
		if parsed.byDay != "" {
			for _, name := range strings.Split(parsed.byDay, ",") {
				d, _ := dayIndex(name)
				allowed[d] = true
			}
		}
		candidate := at(start.Year(), start.Month(), start.Day())
		for tries := 0; tries < 14; tries++ {
			candidate = at(candidate.Year(), candidate.Month(), candidate.Day()+1)
			if candidate.After(after) && (len(allowed) == 0 || allowed[discordWeekday(candidate)]) {
				return candidate, true
			}
			if !candidate.After(after) {
				tries-- // catching up to `after` does not spend a try
				if candidate.Sub(after) < -400*24*time.Hour {
					candidate = at(after.Year(), after.Month(), after.Day()-1)
				}
			}
		}
		return time.Time{}, false
	case "WEEKLY":
		day := discordWeekday(start)
		if parsed.byDay != "" {
			day, _ = dayIndex(parsed.byDay)
		}
		candidate := at(start.Year(), start.Month(), start.Day()-(discordWeekday(start)-day))
		step := 7 * parsed.interval
		for !candidate.After(after) {
			candidate = at(candidate.Year(), candidate.Month(), candidate.Day()+step)
		}
		return candidate, true
	case "MONTHLY":
		n, day := (start.Day()-1)/7+1, discordWeekday(start)
		if parsed.byDay != "" {
			pn, pd, _ := nthWeekday(parsed.byDay)
			if pn != 0 {
				n = pn
			}
			day = pd
		}
		// Month by month from the start's own month. A fifth weekday exists
		// in some months and not others; a month without one is skipped, as
		// Discord skips it.
		year, month := start.Year(), start.Month()
		for tries := 0; tries < 60; tries++ {
			if candidate, exists := nthWeekdayOfMonth(year, month, n, day, hour, minute, second, loc); exists && candidate.After(after) {
				return candidate, true
			}
			month++
			if month > time.December {
				month, year = time.January, year+1
			}
		}
		return time.Time{}, false
	case "YEARLY":
		month, day := start.Month(), start.Day()
		if parsed.byMonth != 0 {
			month, day = time.Month(parsed.byMonth), parsed.monthDay
		}
		for year := start.Year(); year < start.Year()+8; year++ {
			candidate := at(year, month, day)
			if candidate.Month() == month && candidate.After(after) {
				return candidate, true // a 29 February rolls to the next leap year
			}
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// nthWeekdayOfMonth is the nth `day` (Monday = 0) of a month at a wall-clock
// time, and whether the month has one.
func nthWeekdayOfMonth(year int, month time.Month, n, day, hour, minute, second int, loc *time.Location) (time.Time, bool) {
	first := time.Date(year, month, 1, hour, minute, second, 0, loc)
	offset := (day - discordWeekday(first) + 7) % 7
	candidate := time.Date(year, month, 1+offset+7*(n-1), hour, minute, second, 0, loc)
	return candidate, candidate.Month() == month
}
