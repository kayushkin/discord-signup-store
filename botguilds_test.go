package discordsignup

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// recordBotGuild puts a server in bot_guilds, as the gateway would.
func recordBotGuild(t *testing.T, store *Store, guildID, name, ownerID string) {
	t.Helper()
	if err := store.RecordBotGuild(guildID, name, ownerID); err != nil {
		t.Fatal(err)
	}
}

// TestTheHomePageDoesNotAskDiscordForServers: Discord allows one call a second
// to the bot's server list, and the home page used to make three, so a site
// admin waited two seconds on 429s for every load. Servers and owners now come
// from bot_guilds.
func TestTheHomePageDoesNotAskDiscordForServers(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	recordBotGuild(t, store, "g1", "One", "manager")
	recordBotGuild(t, store, "g2", "Two", "someone-else")
	if _, err := store.AddSiteAdmin("manager"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvent(Event{GuildID: "g2", ChannelID: "c", Name: "Second server night",
		Status: StatusOpen, StartsAt: 4102444800}); err != nil {
		t.Fatal(err)
	}

	body := getPage(t, mux, token, "/").Body.String()
	if !strings.Contains(body, "Second server night") {
		t.Error("a site admin does not see an event in a server recorded in bot_guilds")
	}
	for _, call := range fake.recorded() {
		t.Errorf("the home page called Discord: %s %s", call.Method, call.Path)
	}
}

func TestRefreshBotGuildsFollowsDiscordAndReadsOnlyUnknownOwners(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/users/@me/guilds", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"g1","name":"One renamed"},{"id":"g2","name":"Two"}]`))
	})
	fake.on(http.MethodGet, "/guilds/g2", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"g2","owner_id":"owner-two"}`))
	})
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	recordBotGuild(t, store, "g1", "One", "owner-one")
	recordBotGuild(t, store, "g-left", "Left while offline", "owner-left")

	if err := srv.RefreshBotGuilds(); err != nil {
		t.Fatal(err)
	}

	guilds, err := store.BotGuilds()
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 2 || guilds[0] != (Guild{ID: "g1", Name: "One renamed"}) || guilds[1] != (Guild{ID: "g2", Name: "Two"}) {
		t.Errorf("bot guilds = %+v, want One renamed and Two, and not the server the bot left", guilds)
	}
	for guildID, want := range map[string]string{"g1": "owner-one", "g2": "owner-two"} {
		if got, err := store.BotGuildOwnerID(guildID); err != nil || got != want {
			t.Errorf("owner of %s = %q, %v; want %q", guildID, got, err, want)
		}
	}
	for _, call := range fake.recorded() {
		if call.Path == "/guilds/g1" {
			t.Error("refresh re-read the owner of a server whose owner was already known")
		}
	}
}

func TestTheGatewayKeepsBotGuildsCurrent(t *testing.T) {
	store := testStore(t)
	listener := &GatewayListener{server: NewServer(store, nil, nil)}
	recordBotGuild(t, store, "g-left", "Left while offline", "someone")
	recordBotGuild(t, store, "g1", "One", "owner-one")
	recordBotGuild(t, store, "g2", "Two", "owner-two")

	listener.onReady(nil, &discordgo.Ready{User: &discordgo.User{},
		Guilds: []*discordgo.Guild{{ID: "g1", Unavailable: true}, {ID: "g2", Unavailable: true}}})
	listener.onGuildUpdate(nil, &discordgo.GuildUpdate{Guild: &discordgo.Guild{ID: "g1", Name: "One", OwnerID: "new-owner"}})
	listener.onGuildDelete(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "g2", Unavailable: true}})

	guilds, err := store.BotGuilds()
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 2 || guilds[0].ID != "g1" || guilds[1].ID != "g2" {
		t.Errorf("after READY and an outage, bot guilds = %+v, want g1 and g2", guilds)
	}
	if owner, _ := store.BotGuildOwnerID("g1"); owner != "new-owner" {
		t.Errorf("owner after GUILD_UPDATE = %q, want new-owner", owner)
	}

	listener.onGuildDelete(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "g2"}})
	if _, err := store.BotGuildOwnerID("g2"); err == nil {
		t.Error("a server the bot was removed from is still recorded")
	}
}
