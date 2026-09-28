package discordsignup

import (
	"bytes"
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

func uploadPhoto(t *testing.T, mux http.Handler, token string, photo []byte, consent bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if consent {
		form.WriteField("consent", "yes")
	}
	part, _ := form.CreateFormFile("photo", "me.png")
	part.Write(photo)
	form.Close()
	req := httptest.NewRequest(http.MethodPost, "/avatar/photo", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func postPage(t *testing.T, mux http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func callAPI(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func mustAvatarState(t *testing.T, store *Store, userID, want string) *Avatar {
	t.Helper()
	avatar, err := store.AvatarOf(userID)
	if err != nil {
		t.Fatalf("avatar of %s: %v", userID, err)
	}
	if avatar.State != want {
		t.Fatalf("avatar of %s is %s, want %s", userID, avatar.State, want)
	}
	return avatar
}

// TestAnAvatarGoesFromPhotoToApprovedAndThePhotoIsPurged walks the whole
// way: upload with consent, the drawer's calls, the person's approval, and
// the avatar on an event page.
func TestAnAvatarGoesFromPhotoToApprovedAndThePhotoIsPurged(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})

	if rec := uploadPhoto(t, mux, member.Token, testPhoto, true); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
	avatar := mustAvatarState(t, store, "u-ann", AvatarWaitingForDrawing)
	if files.count() != 1 || files.owners[avatar.PhotoFileID] != "discord-signup-store avatar:u-ann" {
		t.Fatalf("file-store holds %d files, owners %v", files.count(), files.owners)
	}
	if avatar.ConsentedAt == 0 {
		t.Error("the consent was not recorded")
	}

	// The photo is read only while a drawing is under way.
	if rec := callAPI(mux, http.MethodGet, "/api/avatars/u-ann/photo", ""); rec.Code != http.StatusConflict {
		t.Errorf("photo before the drawing started = %d, want 409", rec.Code)
	}
	if rec := callAPI(mux, http.MethodGet, "/api/avatars/to-draw", ""); !strings.Contains(rec.Body.String(), `"u-ann"`) {
		t.Fatalf("to-draw = %s", rec.Body.String())
	}
	if rec := callAPI(mux, http.MethodPost, "/api/avatars/u-ann/drawing-started", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("drawing-started = %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAPI(mux, http.MethodPost, "/api/avatars/u-ann/drawing-started", ""); rec.Code != http.StatusConflict {
		t.Errorf("a second drawing-started = %d, want 409, so two drawers cannot both draw", rec.Code)
	}
	if rec := callAPI(mux, http.MethodGet, "/api/avatars/to-draw", ""); strings.Contains(rec.Body.String(), `"u-ann"`) {
		t.Error("a drawing under way is still listed to draw")
	}
	rec := callAPI(mux, http.MethodGet, "/api/avatars/u-ann/photo", "")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testPhoto) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("photo = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	notWebP, _ := json.Marshal(map[string]any{"drawing_code": "const DRAWING = {}", "image_webp": testPhoto})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-ann/drawing", string(notWebP)); rec.Code != http.StatusBadRequest {
		t.Errorf("a PNG as the print = %d, want 400", rec.Code)
	}
	drawing, _ := json.Marshal(map[string]any{"drawing_code": "const DRAWING = {}", "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-ann/drawing", string(drawing)); rec.Code != http.StatusNoContent {
		t.Fatalf("save drawing = %d %s", rec.Code, rec.Body.String())
	}
	mustAvatarState(t, store, "u-ann", AvatarReadyForApproval)

	// Only the person sees it before approving.
	if rec := getPage(t, mux, member.Token, "/avatar/drawing.webp"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testWebP) {
		t.Errorf("own drawing = %d", rec.Code)
	}
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-ann.webp", ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unapproved avatar is public: %d", rec.Code)
	}

	if rec := postPage(t, mux, member.Token, "/avatar/approve"); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	approved := mustAvatarState(t, store, "u-ann", AvatarApproved)
	if approved.PhotoFileID != "" || files.count() != 0 {
		t.Errorf("the photo outlived the approval: id %q, %d files", approved.PhotoFileID, files.count())
	}
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-ann.webp", ""); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Errorf("approved avatar = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	ev := publishedEvent(t, store, 5, "u-ann", "u-bob")
	organiser, _ := store.CreateWebSession("u-org", "Org", "", map[string]uint64{"g1": permissionManageEvents})
	page := getPage(t, mux, organiser.Token, fmt.Sprintf("/events/%d", ev.ID)).Body.String()
	if !strings.Contains(page, `src="/avatars/u-ann.webp"`) {
		t.Error("the event page does not show the approved avatar")
	}
	if strings.Contains(page, `src="/avatars/u-bob.webp"`) {
		t.Error("the event page shows an avatar for someone with none")
	}
}

// TestNoPhotoIsKeptWithoutConsentOrMembership.
func TestNoPhotoIsKeptWithoutConsentOrMembership(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	stranger, _ := store.CreateWebSession("u-x", "X", "", map[string]uint64{"elsewhere": 0})

	uploadPhoto(t, mux, member.Token, testPhoto, false)
	if rec := uploadPhoto(t, mux, stranger.Token, testPhoto, true); rec.Code != http.StatusForbidden {
		t.Errorf("a stranger's upload = %d, want 403", rec.Code)
	}
	uploadPhoto(t, mux, member.Token, []byte("<html>not a photo</html>"), true)
	if files.count() != 0 {
		t.Errorf("file-store holds %d files", files.count())
	}
	if _, err := store.AvatarOf("u-ann"); err == nil {
		t.Error("an avatar exists without a consented photo")
	}
}

// TestRemovingAnAvatarDeletesThePhotoAndANewPhotoReplacesTheOld.
func TestRemovingAnAvatarDeletesThePhotoAndANewPhotoReplacesTheOld(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	uploadPhoto(t, mux, member.Token, testPhoto, true)
	uploadPhoto(t, mux, member.Token, testPhoto, true)
	if files.count() != 1 {
		t.Errorf("after a second photo file-store holds %d files, want 1", files.count())
	}
	if rec := postPage(t, mux, member.Token, "/avatar/remove"); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d", rec.Code)
	}
	if files.count() != 0 {
		t.Errorf("the photo outlived the removal")
	}
	if _, err := store.AvatarOf("u-ann"); err == nil {
		t.Error("the avatar outlived the removal")
	}
}

// TestAPersonMayAskForOnlySoManyDrawingsADay.
func TestAPersonMayAskForOnlySoManyDrawingsADay(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	for range avatarDrawingsPerDay {
		uploadPhoto(t, mux, member.Token, testPhoto, true)
	}
	before, _ := store.AvatarOf("u-ann")
	uploadPhoto(t, mux, member.Token, testPhoto, true)
	after, _ := store.AvatarOf("u-ann")
	if after.PhotoFileID != before.PhotoFileID {
		t.Error("an upload past the day's limit was taken")
	}
}

// TestAFailedDrawingIsShownAndCanBeRedrawn.
func TestAFailedDrawingIsShownAndCanBeRedrawn(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	member, _ := store.CreateWebSession("u-ann", "Ann", "", map[string]uint64{"g1": 0})
	uploadPhoto(t, mux, member.Token, testPhoto, true)
	if rec := callAPI(mux, http.MethodPost, "/api/avatars/u-ann/drawing-failed", `{"reason":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("failing a drawing never started = %d, want 409", rec.Code)
	}
	callAPI(mux, http.MethodPost, "/api/avatars/u-ann/drawing-started", "")
	callAPI(mux, http.MethodPost, "/api/avatars/u-ann/drawing-failed", `{"reason":"the drawing would not print"}`)
	mustAvatarState(t, store, "u-ann", AvatarDrawingFailed)
	if page := getPage(t, mux, member.Token, "/avatar").Body.String(); !strings.Contains(page, "the drawing would not print") {
		t.Error("the page does not say why the drawing failed")
	}
	postPage(t, mux, member.Token, "/avatar/redraw")
	mustAvatarState(t, store, "u-ann", AvatarWaitingForDrawing)
}
