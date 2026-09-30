package discordsignup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// askForMascot posts the mascot page's request form.
func askForMascot(t *testing.T, mux http.Handler, token string, fields map[string]string, photo []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range fields {
		form.WriteField(k, v)
	}
	if photo != nil {
		part, _ := form.CreateFormFile("photo", "mascot.png")
		part.Write(photo)
	}
	form.Close()
	req := httptest.NewRequest(http.MethodPost, "/mascot/requests", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func characterBody(code string) string {
	body, _ := json.Marshal(map[string]any{"format": AvatarFormatCharacter, "drawing_code": code, "image_webp": testWebP, "full_body_webp": testWebP})
	return string(body)
}

// TestOnlyTheOwnerSetsTheServersMascot, or a site admin; a member who owns
// nothing is refused the page and every form.
func TestOnlyTheOwnerSetsTheServersMascot(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	owner, _ := store.CreateWebSession("owner", "Owner", "", map[string]uint64{"g1": 0})
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": permissionManageEvents})
	admin, _ := store.CreateWebSession("admin", "Admin", "", map[string]uint64{})
	if _, err := store.AddSiteAdmin("admin"); err != nil {
		t.Fatal(err)
	}

	if rec := getPage(t, mux, owner.Token, "/mascot"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Games club: mascot") {
		t.Errorf("the owner's mascot page = %d", rec.Code)
	}
	if rec := getPage(t, mux, admin.Token, "/mascot?guild_id=g1"); rec.Code != http.StatusOK {
		t.Errorf("a site admin's mascot page = %d", rec.Code)
	}
	if rec := getPage(t, mux, member.Token, "/mascot"); rec.Code != http.StatusForbidden {
		t.Errorf("a member's mascot page = %d, want 403", rec.Code)
	}
	if rec := askForMascot(t, mux, member.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestDescribe, "comment": "a frog"}, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a member asking for a mascot = %d, want 403", rec.Code)
	}
	home := getPage(t, mux, owner.Token, "/").Body.String()
	if !strings.Contains(home, `href="/mascot"`) {
		t.Error("the owner's home page has no link to the mascot page")
	}
	if strings.Contains(getPage(t, mux, member.Token, "/").Body.String(), `href="/mascot`) {
		t.Error("a member who owns no server is offered the mascot page")
	}
}

// TestAMascotIsDrawnFromADescription: the drawer draws it, it becomes the
// mascot as the server's first, and every page of the server then shows it
// in the header, and nowhere else new.
func TestAMascotIsDrawnFromADescription(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	owner, _ := store.CreateWebSession("owner", "Owner", "", map[string]uint64{"g1": 0})
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})
	if err := store.SetHomeGuild("member", "g1"); err != nil {
		t.Fatal(err)
	}
	if home := getPage(t, mux, member.Token, "/").Body.String(); !strings.Contains(home, "/art/maleeha-128.webp") {
		t.Fatal("a server with no mascot does not show the site's own")
	}

	if rec := askForMascot(t, mux, owner.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestDescribe}, nil); !strings.Contains(rec.Header().Get("Location"), "Could+not") {
		t.Errorf("a description with no words was taken: %s", rec.Header().Get("Location"))
	}
	askForMascot(t, mux, owner.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestDescribe, "comment": "a raccoon in a cowboy hat"}, nil)
	if rec := askForMascot(t, mux, owner.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestDescribe, "comment": "again"}, nil); !strings.Contains(rec.Header().Get("Location"), "already+under+way") {
		t.Errorf("a second request while one is under way: %s", rec.Header().Get("Location"))
	}

	var due struct {
		Requests []mascotRequestToDraw `json:"requests"`
	}
	json.Unmarshal(callAPI(mux, http.MethodGet, "/api/mascot-requests/to-draw", "").Body.Bytes(), &due)
	if len(due.Requests) != 1 || due.Requests[0].GuildName != "Games club" || due.Requests[0].Comment != "a raccoon in a cowboy hat" || due.Requests[0].HasPhoto {
		t.Fatalf("to draw = %+v", due.Requests)
	}
	base := fmt.Sprintf("/api/mascot-requests/%d", due.Requests[0].ID)
	if rec := callAPI(mux, http.MethodPost, base+"/started", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("started = %d: %s", rec.Code, rec.Body)
	}
	if rec := callAPI(mux, http.MethodPut, base+"/drawing", characterBody("const CHARACTER = {}")); rec.Code != http.StatusOK {
		t.Fatalf("drawing = %d: %s", rec.Code, rec.Body)
	}
	mascot, err := store.GuildMascotOf("g1")
	if err != nil || mascot.MascotDrawingID == 0 || mascot.SetBy != "owner" {
		t.Fatalf("the server's first drawing is not its mascot: %+v, %v", mascot, err)
	}
	home := getPage(t, mux, member.Token, "/").Body.String()
	if !strings.Contains(home, `class="mascot" src="/mascots/g1/portrait.webp`) {
		t.Error("the header does not show the server's mascot")
	}
	if strings.Contains(home, "mascot-pop") {
		t.Error("the page still carries the corner popup")
	}
	if rec := getPage(t, mux, "", "/mascots/g1/portrait.webp"); rec.Code != http.StatusOK || !isWebP(rec.Body.Bytes()) {
		t.Errorf("the mascot's portrait = %d", rec.Code)
	}
	if rec := getPage(t, mux, "", "/mascots/g1/joined.webp"); rec.Code != http.StatusNotFound {
		t.Errorf("a reaction loop = %d, want 404: there are none", rec.Code)
	}

	// Back to the site's own.
	postForm(t, mux, owner.Token, "/mascot/choose", url.Values{"guild_id": {"g1"}, "source": {"site"}})
	if _, err := store.GuildMascotOf("g1"); err != ErrNotFound {
		t.Errorf("after choosing the site's own, the mascot = %v", err)
	}
	if rec := getPage(t, mux, "", "/mascots/g1/portrait.webp"); rec.Code != http.StatusNotFound {
		t.Errorf("the portrait of a mascot no longer chosen = %d, want 404", rec.Code)
	}
}

// TestAMembersAvatarIsTheMascotOnlyWhileTheyShowIt: only a drawing a member
// of the server shows can be chosen, and deleting it puts the site's own
// mascot back.
func TestAMembersAvatarIsTheMascotOnlyWhileTheyShowIt(t *testing.T) {
	store, _, mux := avatarTestServer(t)
	owner, _ := store.CreateWebSession("owner", "Owner", "", map[string]uint64{"g1": 0})
	shown, err := store.SetAvatarByOperator("ann", newAvatarDrawing{Format: AvatarFormatCharacter, Code: "const CHARACTER = {}", ImageWebP: testWebP, FullBodyWebP: testWebP}, "test")
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := store.SetAvatarByOperator("stranger", newAvatarDrawing{Format: AvatarFormatPortrait, Code: "const DRAWING = {}", ImageWebP: testWebP}, "test")
	if err := store.RecordMemberName("g1", "ann", "Ann"); err != nil {
		t.Fatal(err)
	}

	if err := store.SetGuildMascotToAvatar("g1", "owner", "owner", stranger); err == nil {
		t.Error("someone outside the server's avatar was made its mascot")
	}
	page := getPage(t, mux, owner.Token, "/mascot?guild_id=g1").Body.String()
	if !strings.Contains(page, "Ann") || strings.Contains(page, "/avatars/stranger.webp") {
		t.Error("the mascot page does not offer exactly the members' shown avatars")
	}
	postForm(t, mux, owner.Token, "/mascot/choose", url.Values{"guild_id": {"g1"}, "source": {"avatar"}, "drawing_id": {fmt.Sprint(shown)}})
	if mascot, err := store.GuildMascotOf("g1"); err != nil || mascot.AvatarDrawingID != shown || mascot.AvatarOwnerID != "ann" {
		t.Fatalf("mascot = %+v, %v", mascot, err)
	}
	if err := store.DeleteAvatarDrawing("ann", shown); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GuildMascotOf("g1"); err != ErrNotFound {
		t.Errorf("after ann deleted her drawing, the mascot = %v, want none", err)
	}
}

// TestAPhotoForAMascotIsPurgedOnceItIsDrawn: it is kept only for the drawing.
func TestAPhotoForAMascotIsPurgedOnceItIsDrawn(t *testing.T) {
	store, files, mux := avatarTestServer(t)
	owner, _ := store.CreateWebSession("owner", "Owner", "", map[string]uint64{"g1": 0})
	if rec := askForMascot(t, mux, owner.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestFromPhoto}, testPhoto); !strings.Contains(rec.Header().Get("Location"), "Tick") {
		t.Errorf("a photo without consent: %s", rec.Header().Get("Location"))
	}
	askForMascot(t, mux, owner.Token, map[string]string{"guild_id": "g1", "kind": MascotRequestFromPhoto, "consent": "yes"}, testPhoto)
	request, err := store.LatestMascotRequestOf("g1")
	if err != nil || request.PhotoFileID == "" {
		t.Fatalf("request = %+v, %v", request, err)
	}
	files.mu.Lock()
	owned := files.owners[request.PhotoFileID]
	files.mu.Unlock()
	if owned != "discord-signup-store mascot:g1" {
		t.Errorf("the photo is owned by %q", owned)
	}
	base := fmt.Sprintf("/api/mascot-requests/%d", request.ID)
	if rec := callAPI(mux, http.MethodGet, base+"/photo", ""); rec.Code != http.StatusConflict {
		t.Errorf("the photo before the drawing started = %d, want 409", rec.Code)
	}
	callAPI(mux, http.MethodPost, base+"/started", "")
	if rec := callAPI(mux, http.MethodGet, base+"/photo", ""); rec.Code != http.StatusOK {
		t.Errorf("the photo while drawing = %d", rec.Code)
	}
	if rec := callAPI(mux, http.MethodPost, base+"/failed", `{"reason":"would not print"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("failed = %d: %s", rec.Code, rec.Body)
	}
	files.mu.Lock()
	_, kept := files.files[request.PhotoFileID]
	files.mu.Unlock()
	if kept {
		t.Error("the photo is still in file-store after the drawing ended")
	}
	if after, _ := store.LatestMascotRequestOf("g1"); after.PhotoFileID != "" || after.State != AvatarRequestFailed {
		t.Errorf("request after = %+v", after)
	}
}

// TestJoiningAndLeavingAskTheMascotToReact on the next page.
func TestJoiningAndLeavingAskTheMascotToReact(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/ann", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Annie","user":{"id":"ann","username":"ann"}}`))
	})
	ev := publishedEvent(t, store, 2)
	member, _ := store.CreateWebSession("ann", "Ann", "", map[string]uint64{"g1": 0})
	if location := postPage(t, mux, member.Token, eventPath(ev)+"/join").Header().Get("Location"); !strings.Contains(location, "mascot=joined") {
		t.Errorf("join went to %s", location)
	}
	if location := postPage(t, mux, member.Token, eventPath(ev)+"/leave").Header().Get("Location"); !strings.Contains(location, "mascot=left") {
		t.Errorf("leave went to %s", location)
	}
}

// TestTheMascotLoopTableAndColumnsAreDroppedFromAnOlderDatabase: the corner
// popup's loops lived in guild_mascot_reactions, with two columns on
// guild_mascots, for two days; opening a database that has them drops them
// and keeps the mascot.
func TestTheMascotLoopTableAndColumnsAreDroppedFromAnOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE guild_mascots ADD COLUMN reaction_failure TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE guild_mascots ADD COLUMN reaction_failed_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE guild_mascot_reactions (guild_id TEXT NOT NULL REFERENCES guild_mascots(guild_id) ON DELETE CASCADE,
			reaction TEXT NOT NULL, image_webp BLOB NOT NULL, printed_at INTEGER NOT NULL, PRIMARY KEY (guild_id, reaction))`,
		`INSERT INTO guild_mascots (guild_id, avatar_drawing_id, set_by, set_at, reaction_failure) VALUES ('g1', 4, 'owner', 1, 'x')`,
		`INSERT INTO guild_mascot_reactions VALUES ('g1', 'joined', x'00', 1)`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	store.Close()
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if have, err := tableExists(store.db, "guild_mascot_reactions"); err != nil || have {
		t.Errorf("guild_mascot_reactions still exists (%v)", err)
	}
	for _, column := range []string{"reaction_failure", "reaction_failed_at"} {
		if have, err := columnExists(store.db, "guild_mascots", column); err != nil || have {
			t.Errorf("guild_mascots.%s still exists (%v)", column, err)
		}
	}
	var sets int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM guild_mascots WHERE guild_id = 'g1'`).Scan(&sets); err != nil || sets != 1 {
		t.Errorf("the mascot row = %d (%v), want kept", sets, err)
	}
}
