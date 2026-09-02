package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// syncTestEvent is an event with a card already posted, which is the only kind
// that has anything to go stale.
func syncTestEvent(t *testing.T, store *Store, capacity int, joiners ...string) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", MessageID: "msg-1", Name: "Games",
		Capacity: capacity, Status: StatusOpen,
		StartsAt:                time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-9",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, who := range joiners {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
	}
	return ev
}

// cardWrites returns the content of every edit made to the signup card, in the
// order the fake received them.
func cardWrites(fake *fakeDiscord) []string {
	var out []string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/channels/board/messages/msg-1" {
			content, _ := c.Body["content"].(string)
			out = append(out, content)
		}
	}
	return out
}

// TestTheLastWriteWinsWhenChangesOverlap is the bug this file exists for.
//
// Four Interested clicks in thirty-three seconds used to start four
// independent syncs. Each makes five or six sequential Discord calls, so they
// overlapped, and whichever finished last won regardless of what it had read.
// On 2 September the winner was a pass that had read three people five seconds
// before the pass that read two, and every Discord surface sat on 3/7 while the
// database and both web pages said 2/7.
//
// Here the first pass is held inside its card write while two more changes land
// and two more syncs are asked for. Both must fold into the run already going,
// and the write that lands last must be the one carrying the final roster.
func TestTheLastWriteWinsWhenChangesOverlap(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev := syncTestEvent(t, store, 3, "alice")

	var writes int32
	held := make(chan struct{})
	release := make(chan struct{})
	fake.on(http.MethodPatch, "/channels/board/messages/msg-1",
		func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&writes, 1) == 1 {
				close(held)
				<-release // the first pass is stuck mid-flight, as a real one can be
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg-1"}`)
		})

	done := make(chan struct{})
	go func() {
		srv.syncAfterChange(ev.ID, nil)
		close(done)
	}()
	<-held // the first pass has read a roster of one and is writing it

	// Two more people arrive while that write is in the air, each asking for a
	// sync of their own.
	for _, who := range []string{"bob", "carol"} {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
		srv.syncAfterChange(ev.ID, nil)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never finished")
	}

	got := cardWrites(fake)
	if len(got) != 2 {
		t.Errorf("%d card writes for three changes, want 2 — one in flight and one owed", len(got))
	}
	if len(got) == 0 {
		t.Fatal("nothing was written at all")
	}
	if last := got[len(got)-1]; !strings.Contains(last, "3/3 places taken") {
		t.Errorf("the last write says %q, want the final roster of 3/3", firstLine(last))
	}
}

// TestAPassWithNothingToSayWritesNothing is what makes the reconcile sweep
// affordable: it re-checks every live event every ten minutes, and an event
// nobody has touched must cost no Discord calls at all.
func TestAPassWithNothingToSayWritesNothing(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev := syncTestEvent(t, store, 3, "alice")

	srv.syncAfterChange(ev.ID, nil)
	after := len(cardWrites(fake))
	if after != 1 {
		t.Fatalf("%d card writes on the first publish, want 1", after)
	}

	for i := 0; i < 3; i++ {
		srv.syncAfterChange(ev.ID, nil)
	}
	if got := len(cardWrites(fake)); got != after {
		t.Errorf("%d card writes after three idle passes, want %d — an unchanged event "+
			"must not be rewritten", got, after)
	}
}

// TestARosterChangeAfterAnIdlePassStillPublishes keeps the skip from becoming a
// stuck surface: skipping is only correct while nothing has changed.
func TestARosterChangeAfterAnIdlePassStillPublishes(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev := syncTestEvent(t, store, 3, "alice")

	srv.syncAfterChange(ev.ID, nil)
	srv.syncAfterChange(ev.ID, nil)
	if _, err := store.Join(ev.ID, "bob", "Bob", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	srv.syncAfterChange(ev.ID, nil)

	got := cardWrites(fake)
	if len(got) != 2 {
		t.Fatalf("%d card writes, want 2 — one for alice, one for bob", len(got))
	}
	if !strings.Contains(got[1], "2/3 places taken") {
		t.Errorf("the second write says %q, want 2/3", firstLine(got[1]))
	}
}

// TestAFailedWriteIsRepairedByTheSweep covers the case that used to have no
// answer at all. A Discord 500 left that surface wrong until the next time
// somebody happened to join; nothing noticed and nothing retried.
func TestAFailedWriteIsRepairedByTheSweep(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	ev := syncTestEvent(t, store, 3, "alice")

	var attempts int32
	fake.on(http.MethodPatch, "/channels/board/messages/msg-1",
		func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				http.Error(w, `{"message":"internal"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg-1"}`)
		})

	srv.syncAfterChange(ev.ID, nil) // the card write fails

	stored, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.PublishedSignature != "" {
		t.Error("a half-written publish was recorded as done, so nothing will ever retry it")
	}

	srv.RepublishStaleEvents("g1")

	got := cardWrites(fake)
	if len(got) < 2 {
		t.Fatalf("%d card writes, want the sweep to have retried the failed one", len(got))
	}
	if !strings.Contains(got[len(got)-1], "1/3 places taken") {
		t.Errorf("the repair wrote %q, want 1/3", firstLine(got[len(got)-1]))
	}
	repaired, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if repaired.PublishedSignature == "" {
		t.Error("the successful repair was not recorded, so the sweep will keep rewriting it")
	}
}

// TestTheQueueHandsOffRatherThanStartingASecondWriter pins the queue's own
// contract, without any Discord in the way.
func TestTheQueueHandsOffRatherThanStartingASecondWriter(t *testing.T) {
	q := newEventSyncQueue()

	if !q.enqueue(7, []stateChange{{UserID: "a"}}) {
		t.Fatal("the first caller must become the writer")
	}
	if q.enqueue(7, []stateChange{{UserID: "b"}}) {
		t.Error("a second caller must hand off, not start a second writer")
	}
	if !q.enqueue(9, nil) {
		t.Error("a different event must get its own writer")
	}

	batch, owed := q.claim(7)
	if !owed || len(batch) != 2 {
		t.Fatalf("first pass claimed %d changes (owed=%v), want both", len(batch), owed)
	}
	if _, owed := q.claim(7); owed {
		t.Error("nothing more was asked for, so no further pass is owed")
	}
	if !q.enqueue(7, nil) {
		t.Error("once released, the next caller becomes the writer")
	}
}

// firstLine keeps a failure message to one line — a card is a whole paragraph.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
