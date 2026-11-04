package discordsignup

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestARoleTakesOneOfTheServersOwnEmojis: the picker lists the server's
// emojis before the standard ones; picking one stores it as Discord writes
// it, rebuilt from Discord's record by id; an emoji from somewhere else is
// refused; and each surface shows it as it can — the card as markdown, the
// button as an emoji object with its id, the native event as :name:, and
// the page as Discord's picture.
func TestARoleTakesOneOfTheServersOwnEmojis(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/emojis", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"111","name":"kermit_blush","animated":false,"available":true},
			{"id":"222","name":"party_parrot","animated":true,"available":true},
			{"id":"333","name":"lost_boost","animated":false,"available":false}]`))
	})
	ev := publishedEvent(t, store, 0, "alice")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{RolesEnabled: &on})
	role, _ := store.AddSheetRole(ev.ID, "Host", 1, true, "test")

	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	server := strings.Index(page, `value="&lt;:kermit_blush:111&gt;"`)
	standard := strings.Index(page, `value="😀"`)
	if server < 0 || standard < 0 || server > standard {
		t.Fatalf("the picker should list the server's emoji (at %d) before the standard ones (at %d)", server, standard)
	}
	if strings.Contains(page, "lost_boost") {
		t.Error("the picker offers an emoji Discord marks unavailable")
	}

	// The form says the name wrong; the id decides, and Discord's name is kept.
	postForm(t, mux, token, eventPath(ev)+"/roles/edit",
		url.Values{"role_id": {fmt.Sprint(role.ID)}, "emoji": {"<a:renamed:222>"}, "capacity": {"1"}})
	got := mustEvent(t, store, ev.ID).Roles[0]
	if got.Emoji != "<a:party_parrot:222>" {
		t.Fatalf("stored emoji = %q, want Discord's own <a:party_parrot:222>", got.Emoji)
	}

	srv := NewServer(store, nil, fake.client())
	if _, err := srv.checkRoleEmoji("g1", "<:someone_elses:999>"); !errors.Is(err, ErrInvalidSheetRole) {
		t.Errorf("an emoji not in the server: err = %v", err)
	}
	if _, err := srv.checkRoleEmoji("g1", "not an emoji"); !errors.Is(err, ErrInvalidSheetRole) {
		t.Errorf("text that is no emoji: err = %v", err)
	}

	ev = mustEvent(t, store, ev.ID)
	switchRolesOnAndOpen := ev
	card := renderSignupMessage(switchRolesOnAndOpen, nil)
	if !strings.Contains(card["content"].(string), "<a:party_parrot:222> **Host**") {
		t.Errorf("card = %s", card["content"])
	}
	button := fmt.Sprint(sheetRoleButtons(ev, sheetRoleCardPrefix, ""))
	if !strings.Contains(button, "id:222") || !strings.Contains(button, "animated:true") {
		t.Errorf("button = %s, want an emoji object with id 222", button)
	}
	if line := sheetRolesLine(ev.Roles); line != ":party_parrot: Host 0/1" {
		t.Errorf("native event line = %q", line)
	}
	page = getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, `src="https://cdn.discordapp.com/emojis/222.webp?size=48&amp;animated=true"`) {
		t.Error("the page does not show the custom emoji as Discord's picture")
	}
}

// TestThePickerSaysWhenTheServersEmojisCannotBeRead: a failed read shows
// in the picker, and the standard emojis are still offered.
func TestThePickerSaysWhenTheServersEmojisCannotBeRead(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	fake.on(http.MethodGet, "/guilds/g1/emojis", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Missing Access","code":50001}`, http.StatusForbidden)
	})
	ev := publishedEvent(t, store, 0, "alice")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{RolesEnabled: &on})
	store.AddSheetRole(ev.ID, "Host", 1, true, "test")
	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "Could not read the server's emojis: list the emojis of guild g1: discord api 403") || !strings.Contains(page, `value="😀"`) {
		t.Error("the picker should say the server's emojis failed and still offer the standard ones")
	}
}
