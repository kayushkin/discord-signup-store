package discordsignup

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTwoRedrawsAtOnceLeaveNoUnrecordedPage is the 2026-09-04 duplicate. Two
// redraws of one table ran together, both read "page 0 only", both posted a
// page 2, and the second record overwrote the first. The first message was then
// recorded nowhere, so no later redraw edited or deleted it, and it showed test7
// as open for seventeen days after it had closed.
func TestTwoRedrawsAtOnceLeaveNoUnrecordedPage(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildTable("g1", "table-channel"); err != nil {
		t.Fatalf("set table: %v", err)
	}
	base := time.Now().Add(24 * time.Hour).Unix()
	for i := 0; i < 8; i++ {
		if _, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
			Name: fmt.Sprintf("Event %d", i), StartsAt: base + int64(i)*3600}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// A slow post holds each redraw between reading the pages and recording
	// the new one, which is the window the two redraws raced through.
	var posted atomic.Int64
	fake.on(http.MethodPost, "/channels/table-channel/messages", func(w http.ResponseWriter, r *http.Request) {
		id := posted.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg-%d"}`, id)
	})

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.RefreshEventTable("g1"); err != nil {
				t.Errorf("redraw: %v", err)
			}
		}()
	}
	wg.Wait()

	pages, err := store.TablePages("g1")
	if err != nil {
		t.Fatalf("pages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("%d pages recorded for 8 events, want 2", len(pages))
	}
	if got := posted.Load(); got != int64(len(pages)) {
		t.Errorf("%d messages posted for %d recorded pages; the rest are in the channel and recorded nowhere", got, len(pages))
	}
}
