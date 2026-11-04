package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAHandedOffPersonsDMsStayOffTheEventPage: once a person's DMs are
// handed off, what they write is not put with an event, what they wrote
// since the handoff began leaves the event page, earlier replies stay, and
// ending the handoff puts new DMs with events again.
func TestAHandedOffPersonsDMsStayOffTheEventPage(t *testing.T) {
	srv, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm-chan"}`))
	})
	fake.on(http.MethodPost, "/channels/dm-chan/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm-1"}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/alice", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Alice","user":{"id":"alice","username":"alice"}}`))
	})
	event := publishedEvent(t, store, 8, "alice")
	postForm(t, mux, token, eventPath(event)+"/message", url.Values{"body": {"Bring snacks"}, "via": {"dm"}})

	since := now() - 100
	store.RecordDMReply(DMReply{EventID: event.ID, DiscordUserID: "alice", MessageID: "r1", Content: "before the handoff", Matched: ReplyMatchedLatest, At: since - 50})
	// A reply filed after the handoff's start, as if it raced the handoff.
	store.RecordDMReply(DMReply{EventID: event.ID, DiscordUserID: "alice", MessageID: "r2", Content: "filed late", Matched: ReplyMatchedLatest, At: since + 5})

	request := httptest.NewRequest(http.MethodPost, "/api/dm-handoffs",
		strings.NewReader(`{"discord_user_id":"alice","handled_by":"chat-link-1","reason":"an agent answers her","since":`+jsonInt(since)+`}`))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create handoff: %d %s", recorder.Code, recorder.Body)
	}
	var handoff DMHandoff
	json.Unmarshal(recorder.Body.Bytes(), &handoff)

	again := httptest.NewRecorder()
	mux.ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/api/dm-handoffs", strings.NewReader(`{"discord_user_id":"alice","handled_by":"other"}`)))
	if again.Code != http.StatusConflict {
		t.Errorf("a second active handoff answered %d, want 409", again.Code)
	}

	srv.receiveDMReply("dm-chan", "r3", "alice", "while handed off", "", 0)
	stored, _ := store.DMReplies(event.ID)
	if len(stored) != 1 || stored[0].Content != "before the handoff" {
		t.Errorf("replies shown while handed off = %+v, want only the one from before", stored)
	}
	var filed int
	store.db.QueryRow(`SELECT COUNT(*) FROM event_dm_replies WHERE message_id = 'r3'`).Scan(&filed)
	if filed != 0 {
		t.Error("a DM sent while handed off was put with the event")
	}

	ended := httptest.NewRecorder()
	mux.ServeHTTP(ended, httptest.NewRequest(http.MethodPost, "/api/dm-handoffs/"+jsonInt(handoff.ID)+"/end", nil))
	if ended.Code != http.StatusOK {
		t.Fatalf("end handoff: %d %s", ended.Code, ended.Body)
	}
	srv.receiveDMReply("dm-chan", "r4", "alice", "after the handoff", "", 0)
	stored, _ = store.DMReplies(event.ID)
	var contents []string
	for _, reply := range stored {
		contents = append(contents, reply.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "after the handoff") || strings.Contains(joined, "filed late") || strings.Contains(joined, "while handed off") {
		t.Errorf("replies after the handoff ended = %q", joined)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
