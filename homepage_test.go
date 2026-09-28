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

// TestAnEventPictureShowsOnlyWhileItShowsWhoIsGoing: the painter is told
// what to paint, a picture painted for an old roster is refused, and the home
// page shows the picture until the roster changes.
func TestAnEventPictureShowsOnlyWhileItShowsWhoIsGoing(t *testing.T) {
	_, store, _, mux, _ := webTestServer(t)
	ev := publishedEvent(t, store, 5, "ann", "bob")
	member, _ := store.CreateWebSession("ann", "Ann", "", map[string]uint64{"g1": 0})
	if _, err := store.SetAvatarByOperator("ann", "const DRAWING = {}", testWebP, "test"); err != nil {
		t.Fatal(err)
	}

	var due struct {
		Pictures []eventPictureDue `json:"pictures"`
	}
	json.Unmarshal(callAPI(mux, http.MethodGet, "/api/event-pictures/due", "").Body.Bytes(), &due)
	if len(due.Pictures) != 1 || due.Pictures[0].EventID != ev.ID || len(due.Pictures[0].People) != 1 ||
		due.Pictures[0].People[0].DrawingCode != "const DRAWING = {}" {
		t.Fatalf("due = %+v", due.Pictures)
	}
	stale, _ := json.Marshal(map[string]any{"signature": "old", "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, fmt.Sprintf("/api/events/%d/picture", ev.ID), string(stale)); rec.Code != http.StatusConflict {
		t.Errorf("a picture of an old roster = %d, want 409", rec.Code)
	}
	fresh, _ := json.Marshal(map[string]any{"signature": due.Pictures[0].Signature, "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, fmt.Sprintf("/api/events/%d/picture", ev.ID), string(fresh)); rec.Code != http.StatusNoContent {
		t.Fatalf("save picture = %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAPI(mux, http.MethodGet, "/api/event-pictures/due", ""); strings.Contains(rec.Body.String(), `"event_id"`) {
		t.Errorf("a current picture is still due: %s", rec.Body.String())
	}
	picturePath := fmt.Sprintf("/events/%d/picture.webp", ev.ID)
	if home := getPage(t, mux, member.Token, "/").Body.String(); !strings.Contains(home, picturePath) {
		t.Error("the home page does not show the picture")
	}
	if rec := getPage(t, mux, member.Token, picturePath); rec.Code != http.StatusOK {
		t.Errorf("picture = %d", rec.Code)
	}
	stranger, _ := store.CreateWebSession("x", "X", "", map[string]uint64{"elsewhere": 0})
	if rec := getPage(t, mux, stranger.Token, picturePath); rec.Code != http.StatusNotFound {
		t.Errorf("someone outside the server got the picture: %d", rec.Code)
	}

	store.Leave(ev.ID, "ann", ActorUser)
	if home := getPage(t, mux, member.Token, "/").Body.String(); strings.Contains(home, picturePath) {
		t.Error("the picture still shows after the only person in it left")
	}
}

// TestAnOperatorSetsAnAvatarIntoTheGallery and shows it, alongside the
// person's own drawings.
func TestAnOperatorSetsAnAvatarIntoTheGallery(t *testing.T) {
	_, _, mux := avatarTestServer(t)
	body, _ := json.Marshal(map[string]any{"drawing_code": "const DRAWING = {}", "image_webp": testWebP})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-bob", string(body)); rec.Code != http.StatusBadRequest {
		t.Errorf("no reason = %d, want 400", rec.Code)
	}
	body, _ = json.Marshal(map[string]any{"drawing_code": "const DRAWING = {}", "image_webp": testWebP, "reason": "the mascot"})
	if rec := callAPI(mux, http.MethodPut, "/api/avatars/u-bob", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAPI(mux, http.MethodGet, "/avatars/u-bob.webp", ""); rec.Code != http.StatusOK {
		t.Errorf("the set avatar is not shown: %d", rec.Code)
	}
}
