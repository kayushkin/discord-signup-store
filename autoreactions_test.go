package discordsignup

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func autoReactionFake(t *testing.T) (*fakeDiscord, *Store, *Server, *GatewayListener) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.botID = "bot-user"
	return fake, store, srv, &GatewayListener{server: srv}
}

func guildMessage(guildID, channelID, messageID, authorID string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: messageID, ChannelID: channelID, GuildID: guildID, Author: &discordgo.User{ID: authorID},
	}}
}

// reactionsPut lists the paths of the reactions the bot added.
func reactionsPut(fake *fakeDiscord) []string {
	var out []string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPut && strings.Contains(c.Path, "/reactions/") {
			out = append(out, c.Path)
		}
	}
	return out
}

// TestAutoReactionReactsToEveryMessageByThatPersonOnly: Loukic's message gets
// the milk, someone else's in the same channel does not, and neither does
// Loukic's message in a server the rule does not name.
func TestAutoReactionReactsToEveryMessageByThatPersonOnly(t *testing.T) {
	fake, store, _, listener := autoReactionFake(t)
	rule, err := store.CreateAutoReaction("g1", "loukic", "🥛", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	listener.onGuildMessage(nil, guildMessage("g1", "general", "m1", "loukic"))
	listener.onGuildMessage(nil, guildMessage("g1", "general", "m2", "someone-else"))
	listener.onGuildMessage(nil, guildMessage("g2", "general", "m3", "loukic"))
	listener.onGuildMessage(nil, guildMessage("g1", "a-thread", "m4", "loukic"))

	got := reactionsPut(fake)
	want := []string{
		// The fake reads the decoded path; CreateOwnReaction sends it escaped.
		"/channels/general/messages/m1/reactions/🥛/@me",
		"/channels/a-thread/messages/m4/reactions/🥛/@me",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reactions:\n got %q\nwant %q", got, want)
	}
	after, err := store.GetAutoReaction(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReactionCount != 2 || after.LastReactedAt == 0 {
		t.Errorf("count=%d last=%d, want 2 and a time", after.ReactionCount, after.LastReactedAt)
	}
}

func TestAutoReactionIgnoresDirectMessagesTheBotAndSwitchedOffRules(t *testing.T) {
	fake, store, _, listener := autoReactionFake(t)
	rule, err := store.CreateAutoReaction("g1", "loukic", "🥛", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAutoReaction("g1", "bot-user", "🥛", true); err != nil {
		t.Fatal(err)
	}

	listener.onGuildMessage(nil, guildMessage("", "dm", "m1", "loukic"))
	listener.onGuildMessage(nil, guildMessage("g1", "general", "m2", "bot-user"))
	off := false
	if _, err := store.UpdateAutoReaction(rule.ID, AutoReactionChange{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	listener.onGuildMessage(nil, guildMessage("g1", "general", "m3", "loukic"))

	if got := reactionsPut(fake); len(got) != 0 {
		t.Fatalf("reacted %q, want nothing", got)
	}
}

// TestAutoReactionKeepsDiscordsRefusal: a missing permission is the likely
// failure, and the page is the only place anyone would see it.
func TestAutoReactionKeepsDiscordsRefusal(t *testing.T) {
	fake, store, _, listener := autoReactionFake(t)
	rule, err := store.CreateAutoReaction("g1", "loukic", "🥛", true)
	if err != nil {
		t.Fatal(err)
	}
	fake.on(http.MethodPut, "/channels/general/messages/m1/reactions/🥛/@me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Missing Permissions","code":50013}`))
	})
	listener.onGuildMessage(nil, guildMessage("g1", "general", "m1", "loukic"))

	after, err := store.GetAutoReaction(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.LastError, "Missing Permissions") || after.LastErrorAt == 0 || after.ReactionCount != 0 {
		t.Errorf("after a 403: error=%q at=%d count=%d", after.LastError, after.LastErrorAt, after.ReactionCount)
	}

	// A new emoji starts clean: the error belonged to the old one.
	emoji := "🧀"
	changed, err := store.UpdateAutoReaction(rule.ID, AutoReactionChange{Emoji: &emoji})
	if err != nil {
		t.Fatal(err)
	}
	if changed.LastError != "" || changed.Emoji != "🧀" {
		t.Errorf("after changing the emoji: %+v", changed)
	}
}

func TestAutoReactionWritesAreChecked(t *testing.T) {
	store := testStore(t)
	for _, c := range []struct{ guild, user, emoji string }{
		{"", "loukic", "🥛"},
		{"g1", "", "🥛"},
		{"g1", "loukic", ""},
		{"g1", "loukic", "milk/../x"},
		{"g1", "loukic", "two words"},
		{"g1", "loukic", strings.Repeat("🥛", 20)},
	} {
		if _, err := store.CreateAutoReaction(c.guild, c.user, c.emoji, true); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("create %+v: err=%v, want invalid", c, err)
		}
	}
	if _, err := store.CreateAutoReaction("g1", "loukic", "🥛", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAutoReaction("g1", "loukic", "🥛", false); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("a second identical rule: err=%v, want invalid", err)
	}
	if _, err := store.CreateAutoReaction("g1", "loukic", "milk:123456789012345678", true); err != nil {
		t.Errorf("a custom emoji as name:id: %v", err)
	}
	if err := store.DeleteAutoReaction(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete a missing rule: %v", err)
	}
}

// TestAutoReactionRoutes runs the API the dash page uses, and shows the
// person's name from member_names once the create recorded it.
func TestAutoReactionRoutes(t *testing.T) {
	_, store, srv, _ := autoReactionFake(t)
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	rec := call(http.MethodPost, "/api/auto-reactions", `{"guild_id":"g1","discord_user_id":"198680080069689344","member_display_name":"Loukic","emoji":"🥛"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"member_display_name":"Loukic"`) || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("create answered %s", rec.Body)
	}
	if rec := call(http.MethodPost, "/api/auto-reactions", `{"guild_id":"g1","discord_user_id":"x","emoji":"🥛","colour":"blue"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d, want 400", rec.Code)
	}
	if rec := call(http.MethodPatch, "/api/auto-reactions/1", `{"enabled":false}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("switch off: %d %s", rec.Code, rec.Body)
	}
	if rec := call(http.MethodGet, "/api/auto-reactions", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"emoji":"🥛"`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(http.MethodDelete, "/api/auto-reactions/1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
	rules, err := store.AutoReactions()
	if err != nil || len(rules) != 0 {
		t.Errorf("after delete: %v %v", rules, err)
	}
	if rec := call(http.MethodGet, "/api/guilds", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"guilds":[]`) {
		t.Errorf("guilds: %d %s", rec.Code, rec.Body)
	}
}
