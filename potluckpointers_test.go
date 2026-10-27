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

// TestJoiningAPotluckSaysToBringSomething: the reply to Join tells them,
// names the roles and what is asked for, and carries What I'm bringing;
// an event without a sheet says none of it.
func TestJoiningAPotluckSaysToBringSomething(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := potluckEvent(t, store)
	store.AddSheetRole(ev.ID, "Setup", 0, "test")
	store.AddPotluckRequest(ev.ID, PotluckSupplies, "Ice", 1, "test")
	plain := testEvent(t, store, 0)
	join := func(eventID int64) map[string]any {
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(
			`{"type":3,"guild_id":"g1","member":{"user":{"id":"ann","username":"ann"}},"message":{"id":"x"},"data":{"custom_id":%q}}`,
			JoinCustomID(eventID))))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out["data"].(map[string]any)
	}
	data := join(ev.ID)
	content := data["content"].(string)
	for _, want := range []string{"You're in.", "please bring something", "sign up to help if you can: Setup", "📌 Asked for: 🍴 Ice"} {
		if !strings.Contains(content, want) {
			t.Errorf("join reply lacks %q:\n%s", want, content)
		}
	}
	if !strings.Contains(fmt.Sprint(data["components"]), PotluckCustomID(ev.ID)) {
		t.Error("join reply has no What I'm bringing button")
	}
	if data := join(plain.ID); strings.Contains(data["content"].(string), "potluck") || data["components"] != nil {
		t.Errorf("an event without a sheet mentions it: %v", data)
	}
}

// TestMovingUpToAPotluckSaysToBringSomething: the DM a place brings carries
// the note and the button.
func TestMovingUpToAPotluckSaysToBringSomething(t *testing.T) {
	srv, store, fake := reminderServer(t)
	ev := potluckEvent(t, store, "ann")
	srv.notifyPromoted(mustEvent(t, store, ev.ID), &Signup{DiscordUserID: "ann"})
	var sent []recordedCall
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && strings.HasPrefix(c.Path, "/channels/") {
			sent = append(sent, c)
		}
	}
	if len(sent) != 1 || !strings.Contains(fmt.Sprint(sent[0].Body["content"]), "please bring something") ||
		!strings.Contains(fmt.Sprint(sent[0].Body["components"]), PotluckCustomID(ev.ID)) {
		t.Fatalf("moved-up DM = %+v", sent)
	}
}

// TestAWebJoinToAPotluckLandsOnItsPage, and #events links the sheet.
func TestAWebJoinToAPotluckLandsOnItsPage(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/members/member", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Member","user":{"id":"member","username":"member"}}`))
	})
	ev := publishedEvent(t, store, 0, "alice")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{PotluckEnabled: &on})
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})
	rec := postForm(t, mux, member.Token, eventPath(ev)+"/join", url.Values{})
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, eventPath(ev)+"?") || !strings.Contains(loc, "potluck") {
		t.Errorf("web join went to %q, want the event page saying it's a potluck", loc)
	}
	linker := NewServer(store, nil, nil)
	linker.EnableWeb(&OAuthConfig{RedirectURL: "https://events.example/auth/callback"})
	store.AddSheetRole(ev.ID, "Setup (6:00)", 0, "test")
	store.AddSheetRole(ev.ID, "Teardown", 0, "test")
	want := fmt.Sprintf("\n[Potluck list →](https://events.example%[1]s#potluck)\n[Sign up to Setup, Teardown →](https://events.example%[1]s#roles)", eventPath(ev))
	if link := linker.potluckSheetLinks(mustEvent(t, store, ev.ID)); link != want {
		t.Errorf("#events link = %q", link)
	}
}
