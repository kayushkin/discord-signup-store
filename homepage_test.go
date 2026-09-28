package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAMemberJoinsAndLeavesFromTheHomePage under the name Discord gives them
// in the server, and a leave moves the next person waiting up.
func TestAMemberJoinsAndLeavesFromTheHomePage(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/ann", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Annie","user":{"id":"ann","username":"ann"}}`))
	})
	ev := publishedEvent(t, store, 1, "first")
	store.Join(ev.ID, "waiting", "Waiting", JoinedViaButton)
	member, _ := store.CreateWebSession("ann", "Ann", "", map[string]uint64{"g1": 0})
	stranger, _ := store.CreateWebSession("x", "X", "", map[string]uint64{"elsewhere": 0})

	strangerJoin := httptest.NewRequest(http.MethodPost, eventPath(ev)+"/join", nil)
	strangerJoin.AddCookie(&http.Cookie{Name: sessionCookieName, Value: stranger.Token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, strangerJoin)
	if rec.Code != http.StatusForbidden {
		t.Errorf("someone outside the server joining = %d, want 403", rec.Code)
	}
	if home := getPage(t, mux, member.Token, "/").Body.String(); !strings.Contains(home, "Join waitlist") {
		t.Error("a full event with a waitlist does not offer Join waitlist")
	}
	rec = postPage(t, mux, member.Token, eventPath(ev)+"/join")
	notice, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(notice, "waitlist at number 2") {
		t.Errorf("join notice = %q", notice)
	}
	roster, _ := store.Roster(ev.ID, false)
	var joined *Signup
	for i := range roster {
		if roster[i].DiscordUserID == "ann" {
			joined = &roster[i]
		}
	}
	if joined == nil || joined.DisplayName != "Annie" || joined.JoinedVia != JoinedViaWebPage {
		t.Fatalf("ann on the roster = %+v, want Annie joined via web", joined)
	}
	if home := getPage(t, mux, member.Token, "/").Body.String(); !strings.Contains(home, "you're waiting") || !strings.Contains(home, eventPath(ev)+"/leave") {
		t.Error("the home page does not show ann waiting with a Leave button")
	}

	store.Leave(ev.ID, "first", ActorUser)
	if after, _ := store.GetEvent(ev.ID); after.AttendingCount != 1 {
		t.Fatalf("going = %d after first left", after.AttendingCount)
	}
	rec = postPage(t, mux, member.Token, eventPath(ev)+"/leave")
	notice, _ = url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(notice, "off the list") {
		t.Errorf("leave notice = %q", notice)
	}
	if states, _ := store.SignupStatesOf("ann"); states[ev.ID] != "" {
		t.Errorf("ann is still %s", states[ev.ID])
	}
}

// TestAnEventPictureIsItsSceneWithWhoIsGoing: the painter is asked for a
// scene first, one written from old details is refused, a picture shows only
// while it matches the scene and who is going, and a roster change prints the
// same scene again without asking for a new one.
func TestAnEventPictureIsItsSceneWithWhoIsGoing(t *testing.T) {
	_, store, _, mux, _ := webTestServer(t)
	ev := publishedEvent(t, store, 5, "ann", "bob")
	member, _ := store.CreateWebSession("ann", "Ann", "", map[string]uint64{"g1": 0})
	if _, err := store.SetAvatarByOperator("ann", newAvatarDrawing{Format: AvatarFormatPortrait, Code: "const DRAWING = {}", ImageWebP: testWebP}, "test"); err != nil {
		t.Fatal(err)
	}
	due := func() []eventPictureDue {
		var body struct {
			Pictures []eventPictureDue `json:"pictures"`
		}
		json.Unmarshal(callAPI(mux, http.MethodGet, "/api/event-pictures/due", "").Body.Bytes(), &body)
		return body.Pictures
	}
	first := due()
	if len(first) != 1 || !first[0].NeedsScene || first[0].Details.Name != "Games" || len(first[0].People) != 1 {
		t.Fatalf("due = %+v", first)
	}
	stale, _ := json.Marshal(map[string]string{"details_signature": "old", "scene_code": "const SCENE = {}"})
	scenePath := fmt.Sprintf("/api/events/%d/scene", ev.ID)
	if rec := callAPI(mux, http.MethodPut, scenePath, string(stale)); rec.Code != http.StatusConflict {
		t.Errorf("a scene from old details = %d, want 409", rec.Code)
	}
	scene, _ := json.Marshal(map[string]string{"details_signature": first[0].DetailsSignature, "scene_code": "const SCENE = {}"})
	rec := callAPI(mux, http.MethodPut, scenePath, string(scene))
	var saved struct {
		Signature string `json:"signature"`
	}
	json.Unmarshal(rec.Body.Bytes(), &saved)
	if rec.Code != http.StatusOK || saved.Signature == "" {
		t.Fatalf("save scene = %d %s", rec.Code, rec.Body.String())
	}
	if next := due(); len(next) != 1 || next[0].NeedsScene || next[0].SceneCode != "const SCENE = {}" {
		t.Fatalf("after the scene, due = %+v", next)
	}
	fresh, _ := json.Marshal(map[string]any{"signature": saved.Signature, "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, fmt.Sprintf("/api/events/%d/picture", ev.ID), string(fresh)); rec.Code != http.StatusNoContent {
		t.Fatalf("save picture = %d %s", rec.Code, rec.Body.String())
	}
	if next := due(); len(next) != 0 {
		t.Errorf("a current picture is still due: %+v", next)
	}
	picturePath := fmt.Sprintf("/events/%d/picture.webp", ev.ID)
	if home := getPage(t, mux, member.Token, "/").Body.String(); !strings.Contains(home, picturePath) {
		t.Error("the home page does not show the picture")
	}
	stranger, _ := store.CreateWebSession("x", "X", "", map[string]uint64{"elsewhere": 0})
	if rec := getPage(t, mux, stranger.Token, picturePath); rec.Code != http.StatusNotFound {
		t.Errorf("someone outside the server got the picture: %d", rec.Code)
	}

	// Someone else with an avatar joins: the same scene, printed again.
	store.SetAvatarByOperator("bob", newAvatarDrawing{Format: AvatarFormatPortrait, Code: "const DRAWING = {}", ImageWebP: testWebP}, "test")
	if next := due(); len(next) != 1 || next[0].NeedsScene || len(next[0].People) != 2 {
		t.Errorf("after bob's avatar, due = %+v", next)
	}
	if home := getPage(t, mux, member.Token, "/").Body.String(); strings.Contains(home, picturePath) {
		t.Error("the picture still shows though it lacks bob")
	}
	// The details change: a new scene is asked for.
	store.db.Exec(`UPDATE events SET description = 'Now with pizza' WHERE id = ?`, ev.ID)
	if next := due(); len(next) != 1 || !next[0].NeedsScene {
		t.Errorf("after a new description, due = %+v", next)
	}
	failed, _ := json.Marshal(map[string]string{"details_signature": due()[0].DetailsSignature, "reason": "no"})
	callAPI(mux, http.MethodPost, fmt.Sprintf("/api/events/%d/scene-failed", ev.ID), string(failed))
	if next := due(); len(next) != 1 || next[0].NeedsScene || next[0].SceneCode != "const SCENE = {}" {
		t.Errorf("after a failed scene, due = %+v; want the old scene printed, not a new one asked for", next)
	}
}

// TestAnOperatorSetsAnAvatarIntoTheGallery and shows it, alongside the
// person's own drawings.
func TestAnOperatorSetsAnAvatarIntoTheGallery(t *testing.T) {
	_, _, mux := avatarTestServer(t)
	body, _ := json.Marshal(map[string]any{"format": AvatarFormatPortrait, "drawing_code": "const DRAWING = {}", "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-bob", string(body)); rec.Code != http.StatusBadRequest {
		t.Errorf("no reason = %d, want 400", rec.Code)
	}
	body, _ = json.Marshal(map[string]any{"format": AvatarFormatPortrait, "drawing_code": "const DRAWING = {}", "image_webp": testWebP, "reason": "the mascot"})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-bob", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-bob.webp", ""); rec.Code != http.StatusOK {
		t.Errorf("the set avatar is not shown: %d", rec.Code)
	}
}
