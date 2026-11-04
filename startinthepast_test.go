package discordsignup

import (
	"errors"
	"testing"
	"time"
)

// TestAStartInThePastIsRefusedUnlessUnchanged: a start moved to a moment gone
// by would be saved, finished by the next sweep and refused by Discord, so
// it is refused at the form. The start an event already had still saves,
// so an event under way can have its other fields edited.
func TestAStartInThePastIsRefusedUnlessUnchanged(t *testing.T) {
	zone := "America/Los_Angeles"
	past := time.Now().Add(-27 * 24 * time.Hour).Unix()
	future := time.Now().Add(4 * 24 * time.Hour).Unix()

	if err := refuseStartInThePast(past, future, zone); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("moving a start into the past gave %v, want ErrInvalidEvent", err)
	}
	if err := refuseStartInThePast(past, 0, zone); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("creating an event in the past gave %v, want ErrInvalidEvent", err)
	}
	if err := refuseStartInThePast(past, past, zone); err != nil {
		t.Errorf("an unchanged past start was refused: %v", err)
	}
	if err := refuseStartInThePast(future, past, zone); err != nil {
		t.Errorf("a future start was refused: %v", err)
	}
	if err := refuseStartInThePast(0, future, zone); err != nil {
		t.Errorf("no start at all is requireStartTime's to refuse, got %v", err)
	}
}
