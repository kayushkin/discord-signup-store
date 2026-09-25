package discordsignup

import (
	"errors"
	"testing"
)

// TestAHeldPlaceIsTakenOnlyByTheOneItIsHeldFor: it counts as taken for
// everyone else, and its holder gets in even when the event is full.
func TestAHeldPlaceIsTakenOnlyByTheOneItIsHeldFor(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 3, "alice")
	if _, err := store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: "bob", InvitedBy: "web:org",
		Delivery: InviteDeliverySent, HoldsPlace: true}); err != nil {
		t.Fatal(err)
	}
	cy, _ := store.Join(ev.ID, "cy", "Cy", JoinedViaButton)
	dee, _ := store.Join(ev.ID, "dee", "Dee", JoinedViaButton)
	if cy.Signup.State != StateAttending || dee.Signup.State != StateWaitlisted {
		t.Fatalf("cy %s, dee %s; want attending and waitlisted — bob's place is held", cy.Signup.State, dee.Signup.State)
	}
	bob, err := store.Join(ev.ID, "bob", "Bob", JoinedViaButton)
	if err != nil || bob.Signup.State != StateAttending {
		t.Fatalf("bob = %+v %v, want his held place", bob, err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.AttendingCount != 3 || after.HeldCount != 0 || after.WaitlistCount != 1 {
		t.Errorf("after = %d going, %d held, %d waiting; want 3, 0, 1", after.AttendingCount, after.HeldCount, after.WaitlistCount)
	}
	if _, err := store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: "eve", InvitedBy: "web:org",
		Delivery: InviteDeliverySent, HoldsPlace: true}); !errors.Is(err, ErrNoPlaceToHold) {
		t.Errorf("holding a place on a full event = %v, want ErrNoPlaceToHold", err)
	}
}

// TestAHeldPlaceGivenBackGoesToTheNextPersonWaiting, whether the holder
// says Maybe, says Can't go, or the organiser releases it.
func TestAHeldPlaceGivenBackGoesToTheNextPersonWaiting(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 2, "alice")
	for _, who := range []string{"bob", "cy"} {
		if _, err := store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: who, InvitedBy: "web:org",
			Delivery: InviteDeliverySent, HoldsPlace: true}); err != nil && who == "bob" {
			t.Fatal(err)
		}
	}
	store.Join(ev.ID, "w1", "W1", JoinedViaButton)
	store.Join(ev.ID, "w2", "W2", JoinedViaButton)

	maybe, err := store.MarkMaybe(ev.ID, "bob", "Bob", JoinedViaButton)
	if err != nil || maybe.Promoted == nil || maybe.Promoted.DiscordUserID != "w1" {
		t.Fatalf("bob's Maybe promoted %+v (%v), want w1 into his held place", maybe, err)
	}
	if _, err := store.GiveBackHeldPlace(ev.ID, "bob", HoldOutcomeReleased, "web:org"); !errors.Is(err, ErrNotFound) {
		t.Errorf("giving back a place already given back = %v, want ErrNotFound", err)
	}
	invites, _ := store.Invites(ev.ID)
	if invites[0].HoldOutcome != HoldOutcomeDeclined || invites[0].HoldEndedAt == 0 {
		t.Errorf("bob's invite = %+v, want the hold ended as declined", invites[0])
	}
}

// TestAnUndeliveredInviteGivesItsPlaceBack.
func TestAnUndeliveredInviteGivesItsPlaceBack(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 1)
	inv, err := store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: "bob", InvitedBy: "web:org",
		Delivery: InviteDeliverySent, HoldsPlace: true})
	if err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "w1", "W1", JoinedViaButton)
	promoted, err := store.SetInviteDelivery(inv.ID, InviteDeliveryDMsClosed, "50007")
	if err != nil || promoted == nil || promoted.DiscordUserID != "w1" {
		t.Errorf("promoted %+v (%v), want w1 into the place bob's bounced invite held", promoted, err)
	}
}
