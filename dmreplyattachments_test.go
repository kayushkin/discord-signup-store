package discordsignup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fakeDiscordCDN serves attachments as Discord's CDN does, and lets them be
// downloaded from it for the length of the test.
func fakeDiscordCDN(t *testing.T, files map[string][]byte) string {
	t.Helper()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(cdn.Close)
	host := strings.TrimPrefix(cdn.URL, "http://")
	discordAttachmentHosts[host] = true
	t.Cleanup(func() { delete(discordAttachmentHosts, host) })
	return cdn.URL
}

// TestAnImageInAReplyIsKeptAndShownToOrganizers: the image is downloaded and
// kept in file-store, a file that is not an image is only noted, the page
// shows the image, and only someone who may edit the event can open it.
func TestAnImageInAReplyIsKeptAndShownToOrganizers(t *testing.T) {
	srv, store, fake, mux, token := webTestServer(t)
	files, client := newFakeFileStore(t)
	srv.EnableAvatars(client)
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm-chan"}`))
	})
	fake.on(http.MethodPost, "/channels/dm-chan/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"dm-1"}`))
	})
	cdn := fakeDiscordCDN(t, map[string][]byte{"/a/photo.png": testPNG, "/a/notes.pdf": []byte("%PDF-1.4 not a picture")})
	ev := publishedEvent(t, store, 8, "alice")
	postForm(t, mux, token, eventPath(ev)+"/message", url.Values{"body": {"Send a photo"}, "via": {"dm"}})

	srv.receiveDMReply("dm-chan", "r1", "alice", "here it is", "dm-1", []DMAttachment{
		{ID: "at1", Filename: "photo.png", ContentType: "image/png", Size: len(testPNG), URL: cdn + "/a/photo.png"},
		{ID: "at2", Filename: "notes.pdf", ContentType: "application/pdf", Size: 22, URL: cdn + "/a/notes.pdf"},
	})

	replies, _ := store.DMReplies(ev.ID)
	if len(replies) != 1 || len(replies[0].Files) != 2 {
		t.Fatalf("replies = %+v", replies)
	}
	image, other := replies[0].Files[0], replies[0].Files[1]
	if !image.Kept() || image.ContentType != "image/png" || other.Kept() || other.NotKept != "not an image" {
		t.Errorf("files = %+v", replies[0].Files)
	}
	if files.count() != 1 {
		t.Errorf("file-store holds %d files, want the image alone", files.count())
	}

	link := eventPath(ev) + "/replies/" + strconv.FormatInt(replies[0].ID, 10) + "/files/0"
	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, `src="`+link+`"`) || !strings.Contains(page, "notes.pdf, see the DM") {
		t.Errorf("the page does not show the reply's files")
	}
	rec := getPage(t, mux, token, link)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Errorf("image = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := getPage(t, mux, token, eventPath(ev)+"/replies/"+strconv.FormatInt(replies[0].ID, 10)+"/files/1"); rec.Code != http.StatusNotFound {
		t.Errorf("a file not kept answered %d", rec.Code)
	}
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	if rec := getPage(t, mux, member.Token, link); rec.Code == http.StatusOK {
		t.Errorf("someone who cannot edit the event opened the image")
	}
}

// TestKeepAttachmentsFetchesOlderReplies: a reply from before images were
// kept is read from Discord again for a fresh link, kept once, and left alone
// after.
func TestKeepAttachmentsFetchesOlderReplies(t *testing.T) {
	srv, store, fake, mux, _ := webTestServer(t)
	_, client := newFakeFileStore(t)
	srv.EnableAvatars(client)
	cdn := fakeDiscordCDN(t, map[string][]byte{"/a/old.png": testPNG})
	ev := publishedEvent(t, store, 8, "alice")
	store.RecordEventDM("dm-1", "dm-chan", ev.ID, "alice", DMKindMessage, "hi")
	store.RecordDMReply(DMReply{EventID: ev.ID, DiscordUserID: "alice", MessageID: "r9", Attachments: 1,
		Matched: ReplyMatchedLatest, At: now()})
	fetched := 0
	fake.on(http.MethodGet, "/channels/dm-chan/messages/r9", func(w http.ResponseWriter, r *http.Request) {
		fetched++
		json.NewEncoder(w).Encode(map[string]any{"id": "r9", "attachments": []map[string]any{
			{"id": "at9", "filename": "old.png", "content_type": "image/png", "size": len(testPNG), "url": cdn + "/a/old.png"}}})
	})
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/dm-replies/keep-attachments", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("keep-attachments = %d: %s", rec.Code, rec.Body.String())
		}
	}
	replies, _ := store.DMReplies(ev.ID)
	if fetched != 1 || len(replies) != 1 || len(replies[0].Files) != 1 || !replies[0].Files[0].Kept() {
		t.Errorf("fetched %d times; replies = %+v", fetched, replies)
	}
}
