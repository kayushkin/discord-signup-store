package discordsignup

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func forumEvent(t *testing.T, store *Store, joiners ...string) *Event {
	t.Helper()
	ev := publishedEvent(t, store, 8, joiners...)
	post := "thread-1"
	if _, err := store.UpdateEvent(ev.ID, EventPatch{ForumPostID: &post}); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestAMessageInTheForumPingsTheLists picked, and nobody else.
func TestAMessageInTheForumPingsTheLists(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := forumEvent(t, store, "alice", "bob")
	store.MarkMaybe(ev.ID, "cy", "Cy", JoinedViaButton)

	rec := postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"Bring snacks"}, "via": {"forum"}})
	if !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("pinging 2 people")) {
		t.Errorf("notice = %s", rec.Header().Get("Location"))
	}
	var post *recordedCall
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/thread-1/messages" {
			post = &c
		}
	}
	if post == nil {
		t.Fatal("nothing was posted in the thread")
	}
	content := post.Body["content"].(string)
	if !strings.Contains(content, "Bring snacks") || !strings.Contains(content, "<@alice>") || strings.Contains(content, "<@cy>") {
		t.Errorf("content = %q", content)
	}
	users := post.Body["allowed_mentions"].(map[string]any)["users"].([]any)
	if len(users) != 2 {
		t.Errorf("allowed to ping %v, want alice and bob", users)
	}
}

// TestMessagesAreLimitedPerEvent: two in ten minutes, and a send that
// reached nobody does not count.
func TestMessagesAreLimitedPerEvent(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := forumEvent(t, store, "alice")
	fake.on(http.MethodPost, "/channels/thread-1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Missing Access","code":50001}`))
	})
	postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"one"}, "via": {"forum"}})
	fake.on(http.MethodPost, "/channels/thread-1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"m"}`))
	})
	for _, body := range []string{"two", "three"} {
		postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {body}, "via": {"forum"}})
	}
	rec := postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"four"}, "via": {"forum"}})
	if !strings.Contains(rec.Header().Get("Location"), "Nothing+was+sent") {
		t.Errorf("a third counted message went out: %s", rec.Header().Get("Location"))
	}
	messages, _ := store.Messages(ev.ID)
	if len(messages) != 3 || messages[0].Status != messageFailed || messages[2].Status != messageSent {
		t.Errorf("messages = %+v", messages)
	}
}

// TestAMessageByDMSaysWhoItReached.
func TestAMessageByDMSaysWhoItReached(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 8, "alice", "bob")
	calls := 0
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Cannot send messages to this user","code":50007}`))
			return
		}
		w.Write([]byte(`{"id":"dm"}`))
	})
	rec := postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"hi"}, "via": {"dm"}})
	notice, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(notice, "Sent by DM to 1 of 2") || !strings.Contains(notice, "DMs closed: bob") {
		t.Errorf("notice = %q", notice)
	}
}
