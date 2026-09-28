package discordsignup

import (
	"bytes"
	"net/url"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeFileStore holds files in memory, as file-store's routes answer.
type fakeFileStore struct {
	mu     sync.Mutex
	files  map[string][]byte
	owners map[string]string
	next   int
}

func newFakeFileStore(t *testing.T) (*fakeFileStore, *FileStoreClient) {
	t.Helper()
	fake := &fakeFileStore{files: map[string][]byte{}, owners: map[string]string{}}
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-File-Store-Service-Token") != "file-token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		content, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.next++
		id := fmt.Sprintf("file_%06d", fake.next)
		fake.files[id] = content
		fake.owners[id] = r.URL.Query().Get("owner_service") + " " + r.URL.Query().Get("owner_ref")
		fake.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("GET /files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		fake.mu.Lock()
		content, ok := fake.files[r.PathValue("id")]
		fake.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(content)
	})
	mux.HandleFunc("DELETE /files/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		fake.mu.Lock()
		_, ok := fake.files[r.PathValue("id")]
		delete(fake.files, r.PathValue("id"))
		fake.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return fake, NewFileStoreClient(server.URL, "file-token")
}

func (f *fakeFileStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.files)
}

// A PNG's first bytes are enough for http.DetectContentType.
var testPhoto = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
var testWebP = append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{1}, 32)...)

func avatarTestServer(t *testing.T) (*Store, *fakeFileStore, http.Handler) {
	t.Helper()
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	srv.EnableWeb(nil)
	files, client := newFakeFileStore(t)
	srv.EnableAvatars(client)
	recordBotGuild(t, store, "g1", "Games club", "owner")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	return store, files, mux
}

// askForDrawing posts the avatar page's request form.
func askForDrawing(t *testing.T, mux http.Handler, token string, fields map[string]string, photo []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range fields {
		form.WriteField(k, v)
	}
	if photo != nil {
		part, _ := form.CreateFormFile("photo", "me.png")
		part.Write(photo)
	}
	form.Close()
	req := httptest.NewRequest(http.MethodPost, "/avatar/requests", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func newPhotoRequest(comment string) map[string]string {
	return map[string]string{"kind": AvatarRequestNewPhoto, "consent": "yes", "comment": comment}
}

func postPage(t *testing.T, mux http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, mux, token, path, url.Values{})
}

func callAPI(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func latestRequest(t *testing.T, store *Store, userID string) *AvatarRequest {
	t.Helper()
	r, err := store.LatestAvatarRequestOf(userID)
	if err != nil {
		t.Fatalf("latest request of %s: %v", userID, err)
	}
	return r
}

// drawRequest plays the drawer: start, then hand back a drawing.
func drawRequest(t *testing.T, mux http.Handler, requestID int64, code string) int64 {
	t.Helper()
	base := fmt.Sprintf("/api/avatar-requests/%d", requestID)
	if rec := callAPI(mux, http.MethodPost, base+"/started", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("started = %d %s", rec.Code, rec.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"drawing_code": code, "image_webp": testWebP})
	rec := callAPI(mux, http.MethodPut, base+"/drawing", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("drawing = %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		DrawingID int64 `json:"drawing_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &answer)
	return answer.DrawingID
}

// TestDrawingsAreKeptAndThePersonChoosesWhichShows walks a new photo with a
// comment, the drawer's calls, choosing, a change to that drawing, and the
// avatar on an event page.
func TestDrawingsAreKeptAndThePersonChoosesWhichShows(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})

	if rec := askForDrawing(t, mux, member.Token, newPhotoRequest("add my glasses"), testPhoto); rec.Code != http.StatusSeeOther {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body.String())
	}
	person, _ := store.AvatarPersonOf("u-ann")
	if files.count() != 1 || files.owners[person.PhotoFileID] != "discord-signup-store avatar:u-ann" || person.PhotoConsentedAt == 0 {
		t.Fatalf("photo not kept with consent: %+v, owners %v", person, files.owners)
	}
	first := latestRequest(t, store, "u-ann")
	if first.State != AvatarRequestWaiting || first.Comment != "add my glasses" {
		t.Fatalf("request = %+v", first)
	}
	if rec := askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestRedrawPhoto}, nil); latestRequest(t, store, "u-ann").ID != first.ID {
		t.Errorf("a second request was taken while one was waiting (%d)", rec.Code)
	}
	if rec := callAPI(mux, http.MethodGet, fmt.Sprintf("/api/avatar-requests/%d/photo", first.ID), ""); rec.Code != http.StatusConflict {
		t.Errorf("photo before the drawing started = %d, want 409", rec.Code)
	}
	toDraw := callAPI(mux, http.MethodGet, "/api/avatar-requests/to-draw", "").Body.String()
	if !strings.Contains(toDraw, `"has_photo":true`) || !strings.Contains(toDraw, `"comment":"add my glasses"`) {
		t.Fatalf("to-draw = %s", toDraw)
	}
	callAPI(mux, http.MethodPost, fmt.Sprintf("/api/avatar-requests/%d/started", first.ID), "")
	if rec := callAPI(mux, http.MethodPost, fmt.Sprintf("/api/avatar-requests/%d/started", first.ID), ""); rec.Code != http.StatusConflict {
		t.Errorf("a second start = %d, want 409, so two drawers cannot both draw", rec.Code)
	}
	if rec := callAPI(mux, http.MethodGet, fmt.Sprintf("/api/avatar-requests/%d/photo", first.ID), ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testPhoto) {
		t.Fatalf("photo while drawing = %d", rec.Code)
	}
	notWebP, _ := json.Marshal(map[string]any{"drawing_code": "x", "image_webp": testPhoto})
	if rec := callAPI(mux, http.MethodPut, fmt.Sprintf("/api/avatar-requests/%d/drawing", first.ID), string(notWebP)); rec.Code != http.StatusBadRequest {
		t.Errorf("a PNG as the print = %d, want 400", rec.Code)
	}
	body, _ := json.Marshal(map[string]any{"drawing_code": "const DRAWING = 1", "image_webp": testWebP})
	callAPI(mux, http.MethodPut, fmt.Sprintf("/api/avatar-requests/%d/drawing", first.ID), string(body))
	status := getPage(t, mux, member.Token, "/avatar/status").Body.String()
	if !strings.Contains(status, `"state":"done"`) || !strings.Contains(status, `"drawing_id":`) {
		t.Fatalf("status = %s", status)
	}
	drawings, _ := store.AvatarDrawingsOf("u-ann", false)
	if len(drawings) != 1 {
		t.Fatalf("drawings = %d", len(drawings))
	}
	firstDrawing := drawings[0].ID

	// Nothing shows until they choose.
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-ann.webp", ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unchosen drawing is public: %d", rec.Code)
	}
	ownPath := fmt.Sprintf("/avatar/drawings/%d.webp", firstDrawing)
	if rec := getPage(t, mux, member.Token, ownPath); rec.Code != http.StatusOK {
		t.Errorf("own drawing = %d", rec.Code)
	}
	other, _ := store.CreateWebSession("u-bob", "Bob", "", map[string]uint64{"g1": 0})
	if rec := getPage(t, mux, other.Token, ownPath); rec.Code != http.StatusNotFound {
		t.Errorf("someone else's drawing = %d, want 404", rec.Code)
	}
	postForm(t, mux, member.Token, "/avatar/choose", url.Values{"drawing_id": {fmt.Sprint(firstDrawing)}})
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-ann.webp", ""); rec.Code != http.StatusOK {
		t.Errorf("chosen avatar = %d", rec.Code)
	}
	if files.count() != 1 {
		t.Error("choosing a drawing deleted the photo, which later requests draw from")
	}

	// A change needs a comment and starts from their drawing.
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestEditDrawing, "base_drawing_id": fmt.Sprint(firstDrawing)}, nil)
	if latestRequest(t, store, "u-ann").ID != first.ID {
		t.Error("a change with no comment was taken")
	}
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestEditDrawing, "base_drawing_id": fmt.Sprint(firstDrawing), "comment": "green jacket"}, nil)
	edit := latestRequest(t, store, "u-ann")
	if edit.Kind != AvatarRequestEditDrawing || edit.BaseDrawingID != firstDrawing {
		t.Fatalf("edit request = %+v", edit)
	}
	if toDraw := callAPI(mux, http.MethodGet, "/api/avatar-requests/to-draw", "").Body.String(); !strings.Contains(toDraw, `"base_drawing_code":"const DRAWING = 1"`) {
		t.Errorf("to-draw lacks the drawing to change: %s", toDraw)
	}
	second := drawRequest(t, mux, edit.ID, "const DRAWING = 2")
	if drawings, _ := store.AvatarDrawingsOf("u-ann", false); len(drawings) != 2 {
		t.Errorf("drawings = %d, want both kept", len(drawings))
	}
	postForm(t, mux, member.Token, "/avatar/choose", url.Values{"drawing_id": {fmt.Sprint(second)}})
	postForm(t, mux, member.Token, "/avatar/drawings/delete", url.Values{"drawing_id": {fmt.Sprint(firstDrawing)}})
	if p, _ := store.AvatarPersonOf("u-ann"); p.ChosenDrawingID != second {
		t.Errorf("chosen = %d, want %d", p.ChosenDrawingID, second)
	}
	// Someone else cannot choose or delete it.
	postForm(t, mux, other.Token, "/avatar/drawings/delete", url.Values{"drawing_id": {fmt.Sprint(second)}})
	if _, err := store.AvatarDrawingByID(second); err != nil {
		t.Error("someone else deleted the drawing")
	}

	ev := publishedEvent(t, store, 5, "u-ann", "u-bob")
	organiser, _ := store.CreateWebSession("u-org", "Org", "", map[string]uint64{"g1": permissionManageEvents})
	page := getPage(t, mux, organiser.Token, fmt.Sprintf("/events/%d", ev.ID)).Body.String()
	if !strings.Contains(page, `src="/avatars/u-ann.webp"`) || strings.Contains(page, `src="/avatars/u-bob.webp"`) {
		t.Error("the event page shows the wrong avatars")
	}
}

// TestNoPhotoIsKeptWithoutConsentOrMembership.
func TestNoPhotoIsKeptWithoutConsentOrMembership(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	stranger, _ := store.CreateWebSession("u-x", "X", "", map[string]uint64{"elsewhere": 0})

	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestNewPhoto}, testPhoto)
	if rec := askForDrawing(t, mux, stranger.Token, newPhotoRequest(""), testPhoto); rec.Code != http.StatusForbidden {
		t.Errorf("a stranger's upload = %d, want 403", rec.Code)
	}
	askForDrawing(t, mux, member.Token, newPhotoRequest(""), []byte("<html>not a photo</html>"))
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestRedrawPhoto}, nil)
	if files.count() != 0 {
		t.Errorf("file-store holds %d files", files.count())
	}
	if _, err := store.LatestAvatarRequestOf("u-ann"); err == nil {
		t.Error("a request exists without a consented photo")
	}
}

// TestDeletingThePhotoKeepsTheDrawingsAndRemovingDeletesEverything.
func TestDeletingThePhotoKeepsTheDrawingsAndRemovingDeletesEverything(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	askForDrawing(t, mux, member.Token, newPhotoRequest(""), testPhoto)
	drawing := drawRequest(t, mux, latestRequest(t, store, "u-ann").ID, "const DRAWING = 1")
	askForDrawing(t, mux, member.Token, newPhotoRequest(""), testPhoto)
	if files.count() != 1 {
		t.Errorf("after a second photo file-store holds %d files, want 1", files.count())
	}
	drawRequest(t, mux, latestRequest(t, store, "u-ann").ID, "const DRAWING = 2")

	postPage(t, mux, member.Token, "/avatar/photo/delete")
	if files.count() != 0 {
		t.Error("the photo outlived Delete my photo")
	}
	before := latestRequest(t, store, "u-ann").ID
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestRedrawPhoto}, nil)
	if latestRequest(t, store, "u-ann").ID != before {
		t.Error("a redraw was taken with no photo kept")
	}
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestEditDrawing, "base_drawing_id": fmt.Sprint(drawing), "comment": "smile"}, nil)
	if r := latestRequest(t, store, "u-ann"); r.Kind != AvatarRequestEditDrawing {
		t.Error("a change was refused though it needs no photo")
	} else if toDraw := callAPI(mux, http.MethodGet, "/api/avatar-requests/to-draw", "").Body.String(); !strings.Contains(toDraw, `"has_photo":false`) {
		t.Errorf("to-draw = %s", toDraw)
	}

	postPage(t, mux, member.Token, "/avatar/remove")
	if _, err := store.AvatarPersonOf("u-ann"); err == nil {
		t.Error("the avatar outlived Delete everything")
	}
	if drawings, _ := store.AvatarDrawingsOf("u-ann", false); len(drawings) != 0 {
		t.Errorf("%d drawings outlived Delete everything", len(drawings))
	}
}

// TestAPersonMayAskForOnlySoManyDrawingsADay.
func TestAPersonMayAskForOnlySoManyDrawingsADay(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	for range avatarDrawingsPerDay {
		askForDrawing(t, mux, member.Token, newPhotoRequest(""), testPhoto)
		drawRequest(t, mux, latestRequest(t, store, "u-ann").ID, "const DRAWING = 1")
	}
	before := latestRequest(t, store, "u-ann").ID
	askForDrawing(t, mux, member.Token, newPhotoRequest(""), testPhoto)
	if latestRequest(t, store, "u-ann").ID != before {
		t.Error("a request past the day's limit was taken")
	}
}

// TestAFailedDrawingSaysWhy on the page and to its script, and they can ask
// again.
func TestAFailedDrawingSaysWhy(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	askForDrawing(t, mux, member.Token, newPhotoRequest(""), testPhoto)
	id := latestRequest(t, store, "u-ann").ID
	if rec := callAPI(mux, http.MethodPost, fmt.Sprintf("/api/avatar-requests/%d/failed", id), `{"reason":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("failing a request never started = %d, want 409", rec.Code)
	}
	callAPI(mux, http.MethodPost, fmt.Sprintf("/api/avatar-requests/%d/started", id), "")
	callAPI(mux, http.MethodPost, fmt.Sprintf("/api/avatar-requests/%d/failed", id), `{"reason":"the drawing would not print"}`)
	if page := getPage(t, mux, member.Token, "/avatar").Body.String(); !strings.Contains(page, "the drawing would not print") {
		t.Error("the page does not say why the drawing failed")
	}
	askForDrawing(t, mux, member.Token, map[string]string{"kind": AvatarRequestRedrawPhoto}, nil)
	if latestRequest(t, store, "u-ann").ID == id {
		t.Error("they could not ask again after a failure")
	}
}

// TestTheOldAvatarsTableMovesToTheGallery: the first shape, one row per
// person, keeps its photo, drawing and approval.
func TestTheOldAvatarsTableMovesToTheGallery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.db.Exec(`CREATE TABLE avatars (discord_user_id TEXT PRIMARY KEY, state TEXT NOT NULL, photo_file_id TEXT NOT NULL DEFAULT '',
		drawing_code TEXT NOT NULL DEFAULT '', image_webp BLOB, failure TEXT NOT NULL DEFAULT '', consented_at INTEGER NOT NULL,
		drawing_started_at INTEGER NOT NULL DEFAULT 0, drawn_at INTEGER NOT NULL DEFAULT 0, approved_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL)`)
	store.db.Exec(`INSERT INTO avatars VALUES ('ready', 'ready_for_approval', 'file_1', 'code-r', ?, '', 10, 11, 12, 0, 12),
		('shown', 'approved', '', 'code-s', ?, '', 20, 21, 22, 23, 23),
		('queued', 'waiting_for_drawing', 'file_3', '', NULL, '', 30, 0, 0, 0, 30)`, testWebP, testWebP)
	store.Close()
	store, err = Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	if ready, _ := store.AvatarPersonOf("ready"); ready == nil || ready.PhotoFileID != "file_1" || ready.ChosenDrawingID != 0 {
		t.Errorf("ready = %+v", ready)
	}
	if drawings, _ := store.AvatarDrawingsOf("ready", true); len(drawings) != 1 || drawings[0].DrawingCode != "code-r" {
		t.Errorf("ready's drawings = %+v", drawings)
	}
	if shown, _ := store.AvatarPersonOf("shown"); shown == nil || shown.ChosenDrawingID == 0 {
		t.Errorf("an approved avatar is no longer shown: %+v", shown)
	}
	if queued, _ := store.LatestAvatarRequestOf("queued"); queued == nil || queued.State != AvatarRequestWaiting {
		t.Errorf("a waiting avatar lost its request: %+v", queued)
	}
	var tables int
	store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'avatars'`).Scan(&tables)
	if tables != 0 {
		t.Error("the old table is still there")
	}
}
