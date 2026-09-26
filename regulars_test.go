package discordsignup

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestRegularsAreOnEveryDate: the roster clears when a date rolls over,
// and the host and the other regulars are put back on as going. A host who
// stopped being a regular is not made one again.
func TestRegularsAreOnEveryDate(t *testing.T) {
	store := testStore(t)
	start := time.Now().Add(-3 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "Weekly games", Status: StatusOpen,
		StartsAt: start, EndsAt: start + 3600, Capacity: 2, RecurrenceRule: "FREQ=WEEKLY", Timezone: "UTC", CreatedBy: "host"})
	if err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "host", "Host", JoinedViaOrganiser)
	store.MakeHostRegular(ev.ID)
	store.Join(ev.ID, "regular", "Regular", JoinedViaButton)
	store.MakeRegular(ev.ID, "regular", "Regular", "web:host")
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

	if err := store.EndRegular(ev.ID, "host", "web:host"); err != nil {
		t.Fatal(err)
	}
	store.MakeHostRegular(ev.ID)
	regulars, _ := store.Regulars(ev.ID)
	current := 0
	for _, p := range regulars {
		if p.EndedAt == 0 {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d current regulars after the host stopped, want 1: stopping must stand", current)
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

// TestTheEventPageMakesSomeoneARegular through its own route and wording.
func TestTheEventPageMakesSomeoneARegular(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/bob", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Bob","user":{"id":"bob","username":"bob"}}`))
	})
	ev := publishedEvent(t, store, 8, "bob")
	rule := "FREQ=WEEKLY"
	store.UpdateEvent(ev.ID, EventPatch{RecurrenceRule: &rule})
	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "Make regular") || strings.Contains(page, "Pinned") {
		t.Error("the event page does not offer Make regular, or still says Pinned")
	}
	postForm(t, mux, token, eventPath(ev)+"/regulars", url.Values{"discord_user_id": {"bob"}, "regular": {"true"}})
	regulars, _ := store.Regulars(ev.ID)
	if len(regulars) != 1 || regulars[0].DiscordUserID != "bob" || regulars[0].EndedAt != 0 {
		t.Fatalf("regulars = %+v, want bob", regulars)
	}
	postForm(t, mux, token, eventPath(ev)+"/regulars", url.Values{"discord_user_id": {"bob"}, "regular": {"false"}})
	if regulars, _ = store.Regulars(ev.ID); regulars[0].EndedAt == 0 {
		t.Error("bob is still a regular after stopping")
	}
}
