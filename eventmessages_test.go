package discordsignup

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
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

// TestMessagesByDMAreLimitedPerEvent: two in ten minutes, a send that
// reached nobody does not count, and forum posts are not limited.
func TestMessagesByDMAreLimitedPerEvent(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := forumEvent(t, store, "alice")
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Cannot send messages to this user","code":50007}`))
	})
	postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"one"}, "via": {"dm"}})
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm"}`))
	})
	for _, body := range []string{"two", "three"} {
		postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {body}, "via": {"dm"}})
	}
	rec := postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"four"}, "via": {"dm"}})
	if !strings.Contains(rec.Header().Get("Location"), "Nothing+was+sent") {
		t.Errorf("a third counted DM went out: %s", rec.Header().Get("Location"))
	}
	for _, body := range []string{"five", "six", "seven"} {
		rec = postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {body}, "via": {"forum"}})
		if strings.Contains(rec.Header().Get("Location"), "Nothing+was+sent") {
			t.Errorf("a forum post was limited: %s", rec.Header().Get("Location"))
		}
	}
	messages, _ := store.Messages(ev.ID)
	if len(messages) != 6 || messages[0].Status != messageFailed {
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

// postMultipart posts a form with files, as the page does when images are
// attached to a message.
func postMultipart(t *testing.T, mux http.Handler, token, path string, form url.Values, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for key, values := range form {
		for _, v := range values {
			parts.WriteField(key, v)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := parts.CreateFormFile("image", name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(files[name])
	}
	parts.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", parts.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// testPNG is the smallest PNG header http.DetectContentType knows.
var testPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

// TestAMessageInTheForumCarriesItsImages: the post goes as multipart with the
// image named in its attachments, and the message records what it carried.
func TestAMessageInTheForumCarriesItsImages(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := forumEvent(t, store, "alice")

	rec := postMultipart(t, mux, token, eventPath(ev)+"/message",
		url.Values{"body": {"The map"}, "via": {"forum"}}, map[string][]byte{"park map (final).png": testPNG})
	if !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("pinging 1 person")) {
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
	if len(post.Files) != 1 || post.Files[0].Field != "files[0]" || post.Files[0].Filename != "park-map--final.png" ||
		post.Files[0].ContentType != "image/png" || post.Files[0].Size != len(testPNG) {
		t.Errorf("files = %+v", post.Files)
	}
	attachments, _ := post.Body["attachments"].([]any)
	if len(attachments) != 1 || attachments[0].(map[string]any)["filename"] != "park-map--final.png" {
		t.Errorf("attachments = %v", post.Body["attachments"])
	}
	if content := post.Body["content"].(string); !strings.Contains(content, "The map") || !strings.Contains(content, "<@alice>") {
		t.Errorf("content = %q", content)
	}
	messages, _ := store.Messages(ev.ID)
	if len(messages) != 1 || len(messages[0].Images) != 1 || messages[0].Images[0].Filename != "park-map--final.png" ||
		messages[0].Images[0].Bytes != len(testPNG) {
		t.Errorf("messages = %+v", messages)
	}
}

// TestAnImageAloneGoesByDMToEach: no text is needed when there is an image,
// each DM carries it, and a reply to it is shown as answering the image.
func TestAnImageAloneGoesByDMToEach(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 8, "alice", "bob")
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm"}`))
	})
	rec := postMultipart(t, mux, token, eventPath(ev)+"/message", url.Values{"via": {"dm"}},
		map[string][]byte{"flyer.png": testPNG})
	notice, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(notice, "Sent by DM to 2 of 2") {
		t.Errorf("notice = %q", notice)
	}
	sent := 0
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/dm/messages" {
			sent++
			if len(c.Files) != 1 || c.Files[0].Filename != "flyer.png" {
				t.Errorf("DM files = %+v", c.Files)
			}
		}
	}
	if sent != 2 {
		t.Errorf("%d DMs carried the image, want 2", sent)
	}
	var summary string
	store.db.QueryRow(`SELECT summary FROM event_dms WHERE event_id = ? LIMIT 1`, ev.ID).Scan(&summary)
	if summary != "1 image" {
		t.Errorf("summary = %q", summary)
	}
}

// TestAMessageRefusesWhatIsNotAnImage, and more images than it may carry,
// before anything is sent.
func TestAMessageRefusesWhatIsNotAnImage(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := forumEvent(t, store, "alice")
	rec := postMultipart(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"hi"}, "via": {"forum"}},
		map[string][]byte{"notes.png": []byte("just some text, not a picture")})
	if notice, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(notice, "notes.png is not a JPEG, PNG, GIF or WebP image") {
		t.Errorf("notice = %q", notice)
	}
	tooMany := map[string][]byte{}
	for i := 0; i <= messageImagesMaximumCount; i++ {
		tooMany[fmt.Sprintf("p%d.png", i)] = testPNG
	}
	rec = postMultipart(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"hi"}, "via": {"forum"}}, tooMany)
	if notice, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(notice, "may carry 4 images") {
		t.Errorf("notice = %q", notice)
	}
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/thread-1/messages" {
			t.Errorf("a refused message was posted: %+v", c)
		}
	}
	if messages, _ := store.Messages(ev.ID); len(messages) != 0 {
		t.Errorf("a refused message was recorded: %+v", messages)
	}
}
