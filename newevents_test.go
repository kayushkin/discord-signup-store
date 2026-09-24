package discordsignup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// newEventsFixture is a guild with a #new-events channel and one event ahead.
func newEventsFixture(t *testing.T) (*fakeDiscord, *Store, *Server, *Event) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	if err := store.SetGuildChannels("g1", GuildChannels{Board: "board", Past: "past", NewEvents: "new"}); err != nil {
		t.Fatalf("channels: %v", err)
	}
	start := time.Now().Add(24 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Roller skate", StartsAt: start})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return fake, store, srv, ev
}

func callsTo(fake *fakeDiscord, method, path string) []recordedCall {
	var out []recordedCall
	for _, c := range fake.recorded() {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// TestAnEventGetsOneMessageInNewEventsEditedInPlace: the first publish posts the
// line; a later change edits that same message rather than posting another.
func TestAnEventGetsOneMessageInNewEventsEditedInPlace(t *testing.T) {
	fake, store, srv, ev := newEventsFixture(t)
	fake.on(http.MethodPost, "/channels/new/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"line-1"}`))
	})

	srv.publishEventToDiscord(ev.ID, nil)
	posted := callsTo(fake, http.MethodPost, "/channels/new/messages")
	if len(posted) != 1 {
		t.Fatalf("posts = %+v, want one message", posted)
	}
	// The #events text, as plain content: no container and no buttons, so a
	// forwarded copy carries all of it.
	if content, _ := posted[0].Body["content"].(string); !strings.HasPrefix(content, "### Roller skate\n🗓️ ") ||
		!strings.Contains(content, "✅ **Going**") {
		t.Errorf("content = %q, want the event's #events text", content)
	}
	if _, has := posted[0].Body["components"]; has {
		t.Errorf("message carries components; want plain content only")
	}
	got, _ := store.GetEvent(ev.ID)
	if got.NewEventsMessageID != "line-1" {
		t.Fatalf("line id = %q, want line-1", got.NewEventsMessageID)
	}

	renamed := "Ice skate"
	if _, _, err := srv.applyEventEdit(got, EventPatch{Name: &renamed}, "api"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	srv.publishEventToDiscord(ev.ID, nil)
	if n := len(callsTo(fake, http.MethodPost, "/channels/new/messages")); n != 1 {
		t.Errorf("%d posts, want the one line edited rather than a second", n)
	}
	edits := callsTo(fake, http.MethodPatch, "/channels/new/messages/line-1")
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1].Body["content"].(string), "Ice skate") {
		t.Errorf("edits = %+v, want the line edited to the new name", edits)
	}
}

// TestFinishingMovesTheMessageOutOfNewEvents: when the event's line goes to past
// events, its #new-events line is deleted.
func TestFinishingMovesTheMessageOutOfNewEvents(t *testing.T) {
	fake, store, srv, ev := newEventsFixture(t)
	if err := store.SetNewEventsMessageID(ev.ID, "line-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	completed := StatusCompleted
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &completed}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	srv.finishEventEverywhere(ev.ID)

	if len(callsTo(fake, http.MethodPost, "/channels/past/messages")) != 1 {
		t.Errorf("no line posted to past events")
	}
	if len(callsTo(fake, http.MethodDelete, "/channels/new/messages/line-1")) != 1 {
		t.Errorf("the new-events line was not deleted")
	}
	got, _ := store.GetEvent(ev.ID)
	if got.NewEventsMessageID != "" {
		t.Errorf("line id = %q, want it cleared", got.NewEventsMessageID)
	}
}

// TestAMessageDeletedByHandIsPostedAgain: an edit that finds the message gone
// posts a fresh line rather than failing every publish.
func TestAMessageDeletedByHandIsPostedAgain(t *testing.T) {
	fake, store, srv, ev := newEventsFixture(t)
	if err := store.SetNewEventsMessageID(ev.ID, "line-gone"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake.on(http.MethodPatch, "/channels/new/messages/line-gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Message","code":10008}`))
	})
	fake.on(http.MethodPost, "/channels/new/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"line-2"}`))
	})
	got, _ := store.GetEvent(ev.ID)
	if err := srv.refreshNewEventsMessage(got, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ = store.GetEvent(ev.ID)
	if got.NewEventsMessageID != "line-2" {
		t.Errorf("line id = %q, want the reposted line-2", got.NewEventsMessageID)
	}
}

// TestTheSweepBackfillsAMissingMessage: an event already published before the
// guild had #new-events gets its line from the every-minute sweep.
func TestTheSweepBackfillsAMissingMessage(t *testing.T) {
	fake, store, srv, ev := newEventsFixture(t)
	fake.on(http.MethodPost, "/channels/new/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"line-1"}`))
	})
	// Published and titled, so neither a changed signature nor a due rename
	// can be what sends the sweep to it — only the missing line.
	if err := store.SetTitlesWritten(ev.ID, nativeEventName(ev), forumPostTitle(ev)); err != nil {
		t.Fatalf("titles: %v", err)
	}
	markPublished(t, store, ev.ID)

	srv.RepublishStaleEvents("g1")
	got, _ := store.GetEvent(ev.ID)
	if got.NewEventsMessageID != "line-1" {
		t.Errorf("line id = %q, want the sweep to have posted it", got.NewEventsMessageID)
	}
}
