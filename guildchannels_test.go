package discordsignup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEachGuildPostsIntoItsOwnChannels is the reason the three channels are
// per guild: until 2026-09-04 they were one env var each, and a second
// server's finished events left their line in the first server's
// past-events channel.
func TestEachGuildPostsIntoItsOwnChannels(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildChannels("g1", GuildChannels{Board: "board-1", Past: "past-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGuildChannels("g2", GuildChannels{Board: "board-2", Past: "past-2"}); err != nil {
		t.Fatal(err)
	}
	ago := time.Now().Add(-2 * time.Hour).Unix()
	for _, g := range []string{"g1", "g2"} {
		if _, err := store.CreateEvent(Event{GuildID: g, ChannelID: "board-" + g[1:], Name: "Games " + g,
			StartsAt: ago - 3600, EndsAt: ago}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatal(err)
	}
	posted := map[string]string{}
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && (c.Path == "/channels/past-1/messages" || c.Path == "/channels/past-2/messages") {
			posted[c.Path] = c.Body["content"].(string)
		}
	}
	if len(posted) != 2 {
		t.Fatalf("past lines went to %v, want one in each guild's own channel", posted)
	}
	for path, want := range map[string]string{"/channels/past-1/messages": "Games g1", "/channels/past-2/messages": "Games g2"} {
		if got := posted[path]; got == "" || !contains(got, want) {
			t.Errorf("%s carries %q, want %s's event", path, got, want)
		}
	}
}

// TestAGuildWithNoChannelsRecordedIsLeftAlone: nothing posts anywhere, and
// nothing is stamped that would stop it posting once the channels are set.
func TestAGuildWithNoChannelsRecordedIsLeftAlone(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	soon := time.Now().Add(30 * time.Minute).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g9", ChannelID: "c", Name: "Quiet", StartsAt: soon})
	if err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "alice", "Alice", JoinedViaButton)
	if sent, err := srv.SendDueReminders(); err != nil || sent != 0 {
		t.Fatalf("sent=%d err=%v, want nothing sent", sent, err)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.RemindedBeforeAt != 0 {
		t.Error("the hour-before reminder was stamped for a guild that has no reminder channel yet")
	}
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost {
			t.Errorf("posted to %s with no channels recorded", c.Path)
		}
	}
	ch, err := store.GuildChannels("g9")
	if err != nil || ch != (GuildChannels{}) {
		t.Errorf("channels for an unknown guild = %+v, %v; want empty and no error", ch, err)
	}
}

// TestChannelsCanBeSetBeforeTheTable: recording the channels does not need a
// table first, and a row that only carries channels draws no table.
func TestChannelsCanBeSetBeforeTheTable(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.SetGuildChannels("g1", GuildChannels{Board: "board", Reminder: "rem"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.RefreshEventTable("g1"); err != nil {
		t.Fatalf("refresh with no table channel: %v", err)
	}
	if n := len(fake.recorded()); n != 0 {
		t.Errorf("%d Discord calls drawing a table nobody placed", n)
	}
	table, err := store.GuildTable("g1")
	if err != nil || table.BoardChannelID != "board" || table.ReminderChannelID != "rem" || table.ChannelID != "" {
		t.Errorf("row = %+v, %v", table, err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
