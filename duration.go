package discordsignup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// How long an event lasts, as a person types it on the Repeat form:
// "30 mins", "4 hours", "3:30" (three and a half hours), "60m", "4h",
// "4h30m", "4h 30m", "1 day", "1.5h". Blank means no set length.

// maxEventLength keeps a typo — "30" days meant as minutes — from making an
// event that runs for a month.
const maxEventLength = 14 * 24 * time.Hour

var (
	clockLength = regexp.MustCompile(`^(\d{1,3}):([0-5]\d)$`)
	lengthPart  = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m)`)
	lengthJoin  = regexp.MustCompile(`^(?:\s|,|and\b)+`)
	lengthUnits = map[string]time.Duration{
		"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
		"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
		"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	}
)

// ParseEventLength reads a length. Zero with no error means none was given.
func ParseEventLength(input string) (time.Duration, error) {
	text := strings.ToLower(strings.TrimSpace(input))
	if text == "" {
		return 0, nil
	}
	wrong := func() (time.Duration, error) {
		return 0, fmt.Errorf("%w: %q is not a length — say it like 2h, 90m, 3:30 or 1 day", ErrInvalidEvent, input)
	}
	var total time.Duration
	if m := clockLength.FindStringSubmatch(text); m != nil {
		hours, _ := strconv.Atoi(m[1])
		minutes, _ := strconv.Atoi(m[2])
		total = time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	} else {
		rest := text
		for rest != "" {
			m := lengthPart.FindStringSubmatch(rest)
			if m == nil {
				return wrong()
			}
			amount, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return wrong()
			}
			total += time.Duration(amount * float64(lengthUnits[m[2]]))
			rest = strings.TrimPrefix(rest, m[0])
			rest = lengthJoin.ReplaceAllString(rest, "")
		}
	}
	if total <= 0 {
		return wrong()
	}
	if total > maxEventLength {
		return 0, fmt.Errorf("%w: %s is longer than the %d days an event can run", ErrInvalidEvent,
			FormatEventLength(total), int(maxEventLength.Hours()/24))
	}
	return total.Round(time.Minute), nil
}

// FormatEventLength writes a length the way the form reads it back: "2h 30m",
// "45m", "1 day", "2 days 3h".
func FormatEventLength(d time.Duration) string {
	d = d.Round(time.Minute)
	if d <= 0 {
		return ""
	}
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	minutes := int(d % time.Hour / time.Minute)
	parts := []string{}
	switch days {
	case 0:
	case 1:
		parts = append(parts, "1 day")
	default:
		parts = append(parts, fmt.Sprintf("%d days", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	return strings.Join(parts, " ")
}

// eventLength is how long an event runs, or zero when it has no end.
func eventLength(ev *Event) time.Duration {
	if ev.EndsAt <= ev.StartsAt {
		return 0
	}
	return time.Duration(ev.EndsAt-ev.StartsAt) * time.Second
}
