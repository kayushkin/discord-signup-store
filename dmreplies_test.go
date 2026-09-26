package discordsignup

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestARepliedDMIsPutWithItsEvent: a reply to one of our DMs goes with that
// DM's event, a plain message with the latest DM's, a stranger's with none;
// the reply is acknowledged and shown on the event page.
func TestARepliedDMIsPutWithItsEvent(t *testing.T) {
	srv, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm-chan"}`))
	})
	sent := 0
	fake.on(http.MethodPost, "/channels/dm-chan/messages", func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Write([]byte(`{"id":"dm-` + string(rune('0'+sent)) + `"}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/alice", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Alice","user":{"id":"alice","username":"alice"}}`))
	})
	first := publishedEvent(t, store, 8, "alice")
	second, _ := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "Quiz", Status: StatusOpen, StartsAt: 4102444800})
	store.Join(second.ID, "alice", "Alice", JoinedViaButton)
	postForm(t, mux, token, eventPath(first)+"/message", url.Values{"body": {"Bring snacks"}, "via": {"dm"}}) // dm-1
	postForm(t, mux, token, eventPath(second)+"/message", url.Values{"body": {"Quiz starts late"}, "via": {"dm"}}) // dm-2

	srv.receiveDMReply("dm-chan", "r1", "alice", "I'll bring crisps", "dm-1", 0) // Reply on the first
	srv.receiveDMReply("dm-chan", "r2", "alice", "ok!", "", 0)                   // plain: the latest, the second
	srv.receiveDMReply("dm-chan", "r3", "stranger", "hello?", "", 0)

	one, _ := store.DMReplies(first.ID)
	two, _ := store.DMReplies(second.ID)
	if len(one) != 1 || one[0].Content != "I'll bring crisps" || one[0].Matched != ReplyMatchedReply || one[0].AnsweringSummary != "Bring snacks" {
		t.Errorf("first event's replies = %+v", one)
	}
	if len(two) != 1 || two[0].Matched != ReplyMatchedLatest {
		t.Errorf("second event's replies = %+v", two)
	}
	reacted := 0
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPut && strings.Contains(c.Path, "/reactions/") {
			reacted++
		}
	}
	if reacted != 2 {
		t.Errorf("%d replies acknowledged, want 2", reacted)
	}
	page := getPage(t, mux, token, eventPath(first)).Body.String()
	if !strings.Contains(page, "I&#39;ll bring crisps") || !strings.Contains(page, "your message “Bring snacks”") {
		t.Error("the event page does not show the reply and what it answered")
	}
}
