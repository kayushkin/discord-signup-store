package discordsignup

import (
	"errors"
	"testing"
	"time"
)

// testZone is the deployment's own zone, so these read the way the person
// typing them experiences them.
func testZone(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	return loc
}

// TestTypedTimesAreReadTheWayPeopleTypeThem is the whole grammar in one table.
//
// The clock is fixed at Wednesday 2 September 2026, 10:15 in the morning, so
// every "next one of those" answer below is checkable by hand rather than
// dependent on when the suite runs.
func TestTypedTimesAreReadTheWayPeopleTypeThem(t *testing.T) {
	loc := testZone(t)
	now := time.Date(2026, time.September, 2, 10, 15, 0, 0, loc) // a Wednesday

	for _, c := range []struct {
		typed             string
		year              int
		month             time.Month
		day, hour, minute int
		why               string
	}{
		// The five from the request.
		{"09/29 5pm", 2026, time.September, 29, 17, 0, "month/day and a bare pm hour"},
		{"9/29 3:30am", 2026, time.September, 29, 3, 30, "no leading zero, explicit am"},
		{"5:30", 2026, time.September, 2, 17, 30, "no am/pm means pm, and 17:30 is still ahead"},
		{"14:30", 2026, time.September, 2, 14, 30, "already past noon, so it is left alone"},
		{"9/27 1330pm", 2026, time.September, 27, 13, 30, "the pm is redundant, not a contradiction"},

		// Times with no date at all.
		{"5pm", 2026, time.September, 2, 17, 0, "today, because 5pm has not happened yet"},
		{"9am", 2026, time.September, 3, 9, 0, "9am went by at 10:15, so it means tomorrow"},
		{"1730", 2026, time.September, 2, 17, 30, "compact 24-hour"},
		{"noon", 2026, time.September, 2, 12, 0, ""},
		{"midnight", 2026, time.September, 3, 0, 0, "tonight's midnight is tomorrow's date"},
		{"12:30", 2026, time.September, 2, 12, 30, "noon-thirty: 12 is not an hour the pm rule moves"},
		{"12:30am", 2026, time.September, 3, 0, 30, "12am is midnight, and it has passed"},
		{"7:05 pm", 2026, time.September, 2, 19, 5, "meridiem typed as its own word"},

		// Dates with no time.
		{"9/29", 2026, time.September, 29, 0, 0, "a date on its own starts at midnight"},
		{"29 sep", 2026, time.September, 29, 0, 0, "day before month"},
		{"sept 29", 2026, time.September, 29, 0, 0, "the abbreviation people actually type"},

		// Rolling forward.
		{"1/5", 2027, time.January, 5, 0, 0, "January is behind us, so it means next year"},
		{"9/1", 2027, time.September, 1, 0, 0, "yesterday's date means a year from now"},
		{"9/2", 2026, time.September, 2, 0, 0, "today's date is not rolled off"},

		// Named days.
		{"today 8pm", 2026, time.September, 2, 20, 0, ""},
		{"tomorrow 6pm", 2026, time.September, 3, 18, 0, ""},
		{"friday noon", 2026, time.September, 4, 12, 0, "the coming Friday"},
		{"wed 8pm", 2026, time.September, 2, 20, 0, "said on a Wednesday, before 8pm"},
		{"wed 8am", 2026, time.September, 9, 8, 0, "said on a Wednesday, after 8am: next week"},

		// Written in full.
		{"sept 29 7pm", 2026, time.September, 29, 19, 0, ""},
		{"sep 29th 5:30pm", 2026, time.September, 29, 17, 30, "ordinal suffix"},
		{"september 29 2027 7pm", 2027, time.September, 29, 19, 0, "year written out"},
		{"9/29/26 8pm", 2026, time.September, 29, 20, 0, "two-digit year"},
		{"9/29/2026 8pm", 2026, time.September, 29, 20, 0, "four-digit year"},
		{"2026-09-29 19:00", 2026, time.September, 29, 19, 0, "the shape the old parser wanted"},
		{"2026-09-29T17:30", 2026, time.September, 29, 17, 30, "what a datetime-local field sends"},
		{"  SEPT   29   7PM  ", 2026, time.September, 29, 19, 0, "case and spacing are noise"},
	} {
		got, err := parseEventTimeIn(c.typed, loc, now)
		if err != nil {
			t.Errorf("%q: %v", c.typed, err)
			continue
		}
		want := time.Date(c.year, c.month, c.day, c.hour, c.minute, 0, 0, loc)
		if got != want.Unix() {
			t.Errorf("%q read as %s, want %s (%s)", c.typed,
				time.Unix(got, 0).In(loc).Format("Mon 2006-01-02 15:04"),
				want.Format("Mon 2006-01-02 15:04"), c.why)
		}
	}
}

// TestNonsenseIsRefusedRatherThanGuessedAt. A parser this permissive has to say
// no somewhere, and the place is a string that has no reading rather than an
// unusual one — an event silently scheduled for a date nobody typed is worse
// than being asked to type it again.
func TestNonsenseIsRefusedRatherThanGuessedAt(t *testing.T) {
	loc := testZone(t)
	now := time.Date(2026, time.September, 2, 10, 15, 0, 0, loc)

	for _, typed := range []string{
		"sometime next week", "2/30", "13/1", "25:00", "13am", "5:99",
		"the 32nd", "yesterday 5pm", "9/29 banana", "0/5",
	} {
		if got, err := parseEventTimeIn(typed, loc, now); err == nil {
			t.Errorf("%q was read as %s, want a refusal", typed,
				time.Unix(got, 0).In(loc).Format("2006-01-02 15:04"))
		} else if !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%q gave %v, want an ErrInvalidEvent", typed, err)
		}
	}
}

// TestAnEmptyTimeIsNotAnError keeps the optional end time optional: the field
// is allowed to be blank, and blank means zero, not a parse failure.
func TestAnEmptyTimeIsNotAnError(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		got, err := ParseEventTime(blank, "America/Los_Angeles")
		if err != nil || got != 0 {
			t.Errorf("ParseEventTime(%q) = %d, %v; want 0, nil", blank, got, err)
		}
	}
}

// TestEveryRenderedTimeSurvivesBeingReadBack is the test the PM rule makes
// necessary, and the one that would have caught the trap it sets.
//
// Forms open prefilled with FormatEventTime. If that renderer dropped the
// am/pm, every morning event would read back twelve hours later — opening an
// event set for 9am and pressing Save with no other change would move it to
// 9pm, silently, on the surface people use to fix mistakes.
func TestEveryRenderedTimeSurvivesBeingReadBack(t *testing.T) {
	const zone = "America/Los_Angeles"
	loc := testZone(t)
	now := time.Date(2026, time.September, 2, 10, 15, 0, 0, loc)

	for hour := 0; hour < 24; hour++ {
		for _, minute := range []int{0, 1, 30, 59} {
			original := time.Date(2026, time.November, 14, hour, minute, 0, 0, loc)
			rendered := FormatEventTime(original.Unix(), zone)
			reread, err := parseEventTimeIn(rendered, loc, now)
			if err != nil {
				t.Fatalf("%s rendered as %q, which does not parse: %v",
					original.Format("15:04"), rendered, err)
			}
			if reread != original.Unix() {
				t.Errorf("%s rendered as %q and read back as %s",
					original.Format("15:04"), rendered,
					time.Unix(reread, 0).In(loc).Format("15:04"))
			}
		}
	}
}

// TestATimeWithoutAZoneIsRefused. An offset is not a zone and a wall-clock
// reading is not an instant, so there is nothing to fall back to here.
func TestATimeWithoutAZoneIsRefused(t *testing.T) {
	if _, err := ParseEventTime("9/29 5pm", ""); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("err = %v, want an ErrInvalidEvent naming the missing zone", err)
	}
	if _, err := ParseEventTime("9/29 5pm", "Pacific/Nowhere"); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("err = %v, want an ErrInvalidEvent naming the bad zone", err)
	}
}
