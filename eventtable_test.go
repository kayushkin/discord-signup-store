package discordsignup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// tableEvents builds n open events an hour apart, for testing the panel's
// component budget at various sizes.
func tableEvents(n int) []Event {
	out := make([]Event, n)
	base := time.Now().Add(24 * time.Hour).Unix()
	for i := range out {
		out[i] = Event{
			ID: int64(i + 1), GuildID: "g1", Name: fmt.Sprintf("Event %d", i+1),
			Status: StatusOpen, StartsAt: base + int64(i)*3600,
			Capacity: 4, AttendingCount: 1, Timezone: "UTC",
		}
	}
	return out
}

func countComponents(components []any) int {
	total := 0
	for _, c := range components {
		m := c.(map[string]any)
		total++
		if nested, ok := m["components"].([]any); ok {
			total += countComponents(nested)
		}
		if options, ok := m["options"].([]any); ok {
			total += len(options)
		}
	}
	return total
}

// TestTableIsEditedInPlaceAndShrinks covers the reason pages beat one message
// per event: rewriting them keeps the table sorted without reposting, because
// events move between pages while the messages stay put.
func TestTableIsEditedInPlaceAndShrinks(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	base := time.Now().Add(24 * time.Hour).Unix()
	var ids []int64
	for i := 0; i < 8; i++ {
		ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
			Name: fmt.Sprintf("Event %d", i), StartsAt: base + int64(i)*3600})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, ev.ID)
	}
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	if pages, _ := store.TablePages("g1"); len(pages) != 2 {
		t.Fatalf("%d pages for 8 events, want 2", len(pages))
	}

	// Redrawing again must edit, never post.
	before := len(fake.recorded())
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("second draw: %v", err)
	}
	for _, c := range fake.recorded()[before:] {
		if c.Method == "POST" {
			t.Errorf("redrawing posted a new message: %s", c.Path)
		}
	}

	// Dropping below six events must delete the surplus page.
	for _, id := range ids[5:] {
		if err := store.DeleteEvent(id); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("third draw: %v", err)
	}
	if pages, _ := store.TablePages("g1"); len(pages) != 1 {
		t.Errorf("%d pages for 5 events, want 1", len(pages))
	}
}

// TestTheDetailsModalIsBuiltFromTheOnlyShapeThatWorks.
//
// This test used to assert every component was a Text Display, "because
// anything else is an input, which cannot be made read-only". The reasoning
// was sound and the premise was false: Discord refuses a modal carrying a Text
// Display, so that assertion held a button broken for ten days while the suite
// stayed green.
func TestTheDetailsModalIsBuiltFromTheOnlyShapeThatWorks(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	roster := []Signup{{DiscordUserID: "u1", DisplayName: "Al", State: StateAttending}}
	for i, c := range buildRosterOnlyModal(ev, roster, "America/Los_Angeles")["components"].([]any) {
		if m := c.(map[string]any); m["type"] != componentTypeActionRow {
			t.Errorf("component %d is type %v, want an Action Row", i, m["type"])
		}
	}
}

// TestWaitlistIsNumberedByItsPlaceInLine means the next person up reads as
// "1.", not their arrival number.
//
// Arrival order is now the order the roster comes back in — signed_up_at, then
// the row id to break a tie — and WaitlistPlace is counted from it. The number
// shown on a waitlisted row is their place in the queue, which is what somebody
// waiting wants to know.
func TestWaitlistIsNumberedByItsPlaceInLine(t *testing.T) {
	roster := []Signup{
		{DisplayName: "Carol", State: StateWaitlisted, WaitlistPlace: 1},
		{DisplayName: "Dan", State: StateWaitlisted, WaitlistPlace: 2},
	}
	got := rosterNames(roster)
	if !strings.Contains(got, "1. Carol") || !strings.Contains(got, "2. Dan") {
		t.Errorf("waitlist rendered as %q, want places 1 and 2", got)
	}
}

// TestEventLineIsSlotsTitleLocationTime pins the format, in that order and in
// one component, with the date compact rather than Discord-localised.
func TestEventLineIsSlotsTitleLocationTime(t *testing.T) {
	// 2026-08-29 16:00 in Los Angeles.
	when := time.Date(2026, 8, 29, 16, 0, 0, 0, mustZone(t, "America/Los_Angeles")).Unix()
	ev := &Event{ID: 1, Name: "Board games", Capacity: 8, AttendingCount: 3,
		Location: "The pub", Timezone: "America/Los_Angeles",
		StartsAt: when, Status: StatusOpen}

	want := "`3/8`  ·  **Board games**  ·  The pub  ·  8/29 4pm"
	if got := eventLine(ev); got != want {
		t.Errorf("eventLine =\n  %q\nwant\n  %q", got, want)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	return loc
}

// TestUncappedEventsShowNoCount pins the fix for the bare "1". A count with no
// limit next to it read as nothing — is it a count, a limit, a rank? — so an
// uncapped event shows no number at all.
func TestUncappedEventsShowNoCount(t *testing.T) {
	bare := eventLine(&Event{ID: 1, Name: "Just a name", Capacity: 0, AttendingCount: 7})
	if bare != "**Just a name**" {
		t.Errorf("eventLine = %q, want just the name", bare)
	}
}

// TestCompactWhenKeepsMinutesOnlyWhenTheyMatter: "4pm" not "4:00pm", but a
// half-hour start keeps its minutes.
func TestCompactWhenKeepsMinutesOnlyWhenTheyMatter(t *testing.T) {
	zone := mustZone(t, "America/Los_Angeles")
	onHour := &Event{StartsAt: time.Date(2026, 8, 29, 16, 0, 0, 0, zone).Unix(),
		Timezone: "America/Los_Angeles"}
	if got := compactWhen(onHour); got != "8/29 4pm" {
		t.Errorf("compactWhen = %q, want %q", got, "8/29 4pm")
	}
	halfPast := &Event{StartsAt: time.Date(2026, 8, 29, 16, 30, 0, 0, zone).Unix(),
		Timezone: "America/Los_Angeles"}
	if got := compactWhen(halfPast); got != "8/29 4:30pm" {
		t.Errorf("compactWhen = %q, want %q", got, "8/29 4:30pm")
	}
}

func TestUserSignupsInGuildAnswersThePerViewerQuestion(t *testing.T) {
	store := testStore(t)
	base := time.Now().Add(24 * time.Hour).Unix()

	going := timedEvent(t, store, Event{Name: "Going one", Capacity: 5, StartsAt: base})
	full := timedEvent(t, store, Event{Name: "Full one", Capacity: 1, StartsAt: base + 3600})
	left := timedEvent(t, store, Event{Name: "Left one", Capacity: 5, StartsAt: base + 7200})
	over := timedEvent(t, store, Event{Name: "Over one", Capacity: 5,
		StartsAt: base - 90*3600, EndsAt: base - 89*3600})

	for _, ev := range []*Event{going, full, left, over} {
		if ev.ID == full.ID {
			if _, err := store.Join(ev.ID, "someone-else", "", JoinedViaButton); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
		if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	if _, err := store.Leave(left.ID, "alice", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := store.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	signups, err := store.UserSignupsInGuild("g1", "alice")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := map[string]string{}
	for _, u := range signups {
		got[u.Event.Name] = u.Signup.State
	}
	if got["Going one"] != StateAttending {
		t.Errorf("Going one = %q, want attending", got["Going one"])
	}
	if got["Full one"] != StateWaitlisted {
		t.Errorf("Full one = %q, want waitlisted", got["Full one"])
	}
	if _, there := got["Left one"]; there {
		t.Error("a withdrawn signup is listed")
	}
	if _, there := got["Over one"]; there {
		t.Error("an archived event is listed")
	}
	for _, u := range signups {
		if u.Event.Name == "Full one" && u.Signup.WaitlistPlace != 1 {
			t.Errorf("waitlist place = %d, want 1", u.Signup.WaitlistPlace)
		}
	}
}

// TestTheRosterFieldNamesPeopleWithoutMentioning. A modal renders <@id> as a
// raw snowflake, so the roster has always had to carry names — and now it also
// carries them into a text input, where a mention would be worse still.
func TestTheRosterFieldNamesPeopleWithoutMentioning(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, AttendingCount: 1, StartsAt: 1788067881}
	modal := buildRosterOnlyModal(ev, []Signup{
		{DiscordUserID: "110122051179687936", DisplayName: "Slava", State: StateAttending},
	}, "America/Los_Angeles")

	rendered := fmt.Sprint(modal)
	if strings.Contains(rendered, "<@") {
		t.Errorf("the modal contains a mention, which shows there as a raw id: %q", rendered)
	}
	if !strings.Contains(rendered, "Slava") {
		t.Error("the modal does not name who is going")
	}
}

// TestAnEmptyRosterSaysSoRatherThanShowingNothing.
func TestAnEmptyRosterSaysSoRatherThanShowingNothing(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 4, StartsAt: 1788067881}
	modal := buildRosterOnlyModal(ev, nil, "America/Los_Angeles")
	if !strings.Contains(fmt.Sprint(modal), "Nobody yet") {
		t.Errorf("an empty roster renders as %q", fmt.Sprint(modal))
	}
}
