package discordsignup

import (
	"net/http"
	"testing"
	"time"
)

// TestAMissingCardIsForgottenNotRetriedForever. A card deleted by hand — or
// with its channel — used to make its event unpublishable: the edit 404'd,
// the signature was never stamped, and the sweep retried and logged it every
// minute. Gone means somebody deleted it; the event carries on without one.
func TestAMissingCardIsForgottenNotRetriedForever(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", MessageID: "gone-1",
		Name: "Games", Capacity: 3, Status: StatusOpen,
		StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.on(http.MethodPatch, "/channels/board/messages/gone-1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":10008,"message":"Unknown Message"}`, http.StatusNotFound)
	})

	srv.syncAfterChange(ev.ID, nil)

	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.MessageID != "" {
		t.Errorf("message_id = %q after a 404, want it forgotten", after.MessageID)
	}
	if after.PublishedSignature == "" {
		t.Error("the publish was not stamped, so the sweep will retry the missing card forever")
	}
}
