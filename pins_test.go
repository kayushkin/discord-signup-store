package discordsignup

import (
	"testing"
	"time"
)

// TestPinnedPeopleAreOnEveryDate: the roster clears when a date rolls over,
// and the host and anyone pinned are put back on as going. An unpinned host
// is not pinned again.
func TestPinnedPeopleAreOnEveryDate(t *testing.T) {
	store := testStore(t)
	start := time.Now().Add(-3 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "Weekly games", Status: StatusOpen,
		StartsAt: start, EndsAt: start + 3600, Capacity: 2, RecurrenceRule: "FREQ=WEEKLY", Timezone: "UTC", CreatedBy: "host"})
	if err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "host", "Host", JoinedViaOrganiser)
	store.PinHost(ev.ID)
	store.Join(ev.ID, "regular", "Regular", JoinedViaButton)
	store.Pin(ev.ID, "regular", "Regular", "web:host")
	store.Join(ev.ID, "once", "Once", JoinedViaButton)

	withdrawn, seated, err := store.RollOverOccurrence(ev.ID, start+7*86400, start+7*86400+3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 3 || len(seated) != 2 {
		t.Fatalf("withdrew %d and seated %d, want 3 and 2", len(withdrawn), len(seated))
	}
	roster, _ := store.Roster(ev.ID, false)
	going := map[string]bool{}
	for _, sg := range roster {
		if sg.State == StateAttending {
			going[sg.DiscordUserID] = true
		}
	}
	if !going["host"] || !going["regular"] || going["once"] {
		t.Errorf("going after rollover = %v, want host and regular", going)
	}

	if err := store.Unpin(ev.ID, "host", "web:host"); err != nil {
		t.Fatal(err)
	}
	store.PinHost(ev.ID)
	pins, _ := store.Pins(ev.ID)
	live := 0
	for _, p := range pins {
		if p.UnpinnedAt == 0 {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d live pins after unpinning the host, want 1: an unpin must stand", live)
	}
}

// TestAPassLetsThemInPastTheLimit without keeping a place.
func TestAPassLetsThemInPastTheLimit(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 1, "alice")
	if _, err := store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: "bob", InvitedBy: "web:org",
		Delivery: InviteDeliverySent, PastLimit: true}); err != nil {
		t.Fatal(err)
	}
	if after, _ := store.GetEvent(ev.ID); after.HeldCount != 0 {
		t.Errorf("a pass holds %d places, want 0", after.HeldCount)
	}
	cy, _ := store.Join(ev.ID, "cy", "Cy", JoinedViaButton)
	bob, _ := store.Join(ev.ID, "bob", "Bob", JoinedViaButton)
	if cy.Signup.State != StateWaitlisted || bob.Signup.State != StateAttending {
		t.Errorf("cy %s, bob %s; want waitlisted, and bob in past the limit", cy.Signup.State, bob.Signup.State)
	}
}
