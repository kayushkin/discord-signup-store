package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// pathProbeIDs is the id table every path-segment test in the fleet uses. Each
// one carries a character that changes which endpoint the request addresses if
// it reaches the path unescaped.
var pathProbeIDs = []struct {
	name string
	id   string
}{
	{"well formed", "1290000000000000001"},
	{"fragment", "id#frag"},
	{"query", "id?with_user_count=false"},
	{"extra segment", "a/b"},
	{"climbs out of the collection", "../channels"},
	{"space", "id one"},
	{"already percent encoded", "id%2Fb"},
}

// recordDiscordRequestURI stands up a fake Discord that records the raw request
// line and answers every route with the given body.
//
// It records r.RequestURI, NOT r.URL.Path. Go's server has already decoded
// %2F back to a slash by the time it fills URL.Path, so a URL.Path assertion
// reads identically whether or not the client escaped anything — it cannot hold
// this property at all. discord_test.go's own fakeDiscord records URL.Path,
// which is right for what it asserts and useless for what this file asserts.
func recordDiscordRequestURI(t *testing.T, body string) (*DiscordClient, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client := NewDiscordClient(srv.URL, func() (string, error) { return "tok", nil })
	return client, &got
}

// fixedID is the well-formed value every position holds while another position
// is driven by the probe table. A snowflake, because that is what really flows.
const fixedID = "1290000000000000002"

// pathSegmentSite is one place a caller-chosen id reaches a Discord path.
// call drives that one position with probe and holds every other position at
// fixedID; want states the request line the wire should carry.
type pathSegmentSite struct {
	name string
	body string
	call func(c *DiscordClient, probe string)
	want func(escaped string) string
}

// TestADiscordIDStaysOnePathSegment pins that every id this client puts in a
// Discord API path occupies exactly one segment on the wire.
//
// The ids really are snowflakes today, so no probe below is a value that has
// been observed. That is the point: the safety was an argument about the data
// and nothing in the package enforced it, so this file makes it a property of
// the code that a later caller cannot quietly break.
func TestADiscordIDStaysOnePathSegment(t *testing.T) {
	sites := []pathSegmentSite{
		{"AddMemberRole guild", `{}`,
			func(c *DiscordClient, p string) { _ = c.AddMemberRole(p, fixedID, fixedID) },
			func(e string) string {
				return "/guilds/" + e + "/members/" + fixedID + "/roles/" + fixedID
			}},
		{"AddMemberRole user", `{}`,
			func(c *DiscordClient, p string) { _ = c.AddMemberRole(fixedID, p, fixedID) },
			func(e string) string {
				return "/guilds/" + fixedID + "/members/" + e + "/roles/" + fixedID
			}},
		{"AddMemberRole role", `{}`,
			func(c *DiscordClient, p string) { _ = c.AddMemberRole(fixedID, fixedID, p) },
			func(e string) string {
				return "/guilds/" + fixedID + "/members/" + fixedID + "/roles/" + e
			}},
		{"RemoveMemberRole role", `{}`,
			func(c *DiscordClient, p string) { _ = c.RemoveMemberRole(fixedID, fixedID, p) },
			func(e string) string {
				return "/guilds/" + fixedID + "/members/" + fixedID + "/roles/" + e
			}},
		{"CreateMessage channel", `{"id":"msg-1"}`,
			func(c *DiscordClient, p string) { _, _ = c.CreateMessage(p, map[string]any{}) },
			func(e string) string { return "/channels/" + e + "/messages" }},
		{"EditMessage channel", `{}`,
			func(c *DiscordClient, p string) { _ = c.EditMessage(p, fixedID, map[string]any{}) },
			func(e string) string { return "/channels/" + e + "/messages/" + fixedID }},
		{"EditMessage message", `{}`,
			func(c *DiscordClient, p string) { _ = c.EditMessage(fixedID, p, map[string]any{}) },
			func(e string) string { return "/channels/" + fixedID + "/messages/" + e }},
		{"ListGuildRoles guild", `[]`,
			func(c *DiscordClient, p string) { _, _ = c.ListGuildRoles(p) },
			func(e string) string { return "/guilds/" + e + "/roles" }},
		{"GuildMemberRoleIDs guild", `{"roles":[]}`,
			func(c *DiscordClient, p string) { _, _ = c.GuildMemberRoleIDs(p, fixedID) },
			func(e string) string { return "/guilds/" + e + "/members/" + fixedID }},
		{"GuildMemberRoleIDs user", `{"roles":[]}`,
			func(c *DiscordClient, p string) { _, _ = c.GuildMemberRoleIDs(fixedID, p) },
			func(e string) string { return "/guilds/" + fixedID + "/members/" + e }},
		{"GuildMemberDisplayName guild", `{"nick":"n"}`,
			func(c *DiscordClient, p string) { _, _ = c.GuildMemberDisplayName(p, fixedID) },
			func(e string) string { return "/guilds/" + e + "/members/" + fixedID }},
		{"GuildMemberDisplayName user", `{"nick":"n"}`,
			func(c *DiscordClient, p string) { _, _ = c.GuildMemberDisplayName(fixedID, p) },
			func(e string) string { return "/guilds/" + fixedID + "/members/" + e }},
		{"PinMessage channel", `{}`,
			func(c *DiscordClient, p string) { _ = c.PinMessage(p, fixedID) },
			func(e string) string { return "/channels/" + e + "/messages/pins/" + fixedID }},
		{"PinMessage message", `{}`,
			func(c *DiscordClient, p string) { _ = c.PinMessage(fixedID, p) },
			func(e string) string { return "/channels/" + fixedID + "/messages/pins/" + e }},
		{"CreateGuildChannel guild", `{"id":"chan-1"}`,
			func(c *DiscordClient, p string) { _, _ = c.CreateGuildChannel(p, map[string]any{}) },
			func(e string) string { return "/guilds/" + e + "/channels" }},
		{"DeleteMessage channel", `{}`,
			func(c *DiscordClient, p string) { _ = c.DeleteMessage(p, fixedID) },
			func(e string) string { return "/channels/" + e + "/messages/" + fixedID }},
		{"DeleteMessage message", `{}`,
			func(c *DiscordClient, p string) { _ = c.DeleteMessage(fixedID, p) },
			func(e string) string { return "/channels/" + fixedID + "/messages/" + e }},
		{"CreateThreadFromMessage channel", `{"id":"thread-1"}`,
			func(c *DiscordClient, p string) { _, _ = c.CreateThreadFromMessage(p, fixedID, "n") },
			func(e string) string {
				return "/channels/" + e + "/messages/" + fixedID + "/threads"
			}},
		{"CreateThreadFromMessage message", `{"id":"thread-1"}`,
			func(c *DiscordClient, p string) { _, _ = c.CreateThreadFromMessage(fixedID, p, "n") },
			func(e string) string {
				return "/channels/" + fixedID + "/messages/" + e + "/threads"
			}},
		{"ArchiveThread thread", `{}`,
			func(c *DiscordClient, p string) { _ = c.ArchiveThread(p) },
			func(e string) string { return "/channels/" + e }},
		{"CreateForumPost forum channel", `{"id":"thread-1"}`,
			func(c *DiscordClient, p string) { _, _ = c.CreateForumPost(p, "n", nil, nil) },
			func(e string) string { return "/channels/" + e + "/threads" }},
		{"ModifyThread thread", `{}`,
			func(c *DiscordClient, p string) { _ = c.ModifyThread(p, map[string]any{}) },
			func(e string) string { return "/channels/" + e }},
		{"ForumChannelTags channel", `{"type":15,"available_tags":[]}`,
			func(c *DiscordClient, p string) { _, _ = c.ForumChannelTags(p) },
			func(e string) string { return "/channels/" + e }},
		{"CreateOwnReaction channel", `{}`,
			func(c *DiscordClient, p string) { _ = c.CreateOwnReaction(p, fixedID, "x") },
			func(e string) string {
				return "/channels/" + e + "/messages/" + fixedID + "/reactions/%78/@me"
			}},
		{"CreateOwnReaction message", `{}`,
			func(c *DiscordClient, p string) { _ = c.CreateOwnReaction(fixedID, p, "x") },
			func(e string) string {
				return "/channels/" + fixedID + "/messages/" + e + "/reactions/%78/@me"
			}},
		{"RemoveUserReaction user", `{}`,
			func(c *DiscordClient, p string) { _ = c.RemoveUserReaction(fixedID, fixedID, "x", p) },
			func(e string) string {
				return "/channels/" + fixedID + "/messages/" + fixedID + "/reactions/%78/" + e
			}},
		{"ListScheduledEvents guild", `[]`,
			func(c *DiscordClient, p string) { _, _ = c.ListScheduledEvents(p) },
			func(e string) string {
				return "/guilds/" + e + "/scheduled-events?with_user_count=true"
			}},
		{"ModifyScheduledEvent guild", `{}`,
			func(c *DiscordClient, p string) { _ = c.ModifyScheduledEvent(p, fixedID, map[string]any{}) },
			func(e string) string { return "/guilds/" + e + "/scheduled-events/" + fixedID }},
		{"ModifyScheduledEvent event", `{}`,
			func(c *DiscordClient, p string) { _ = c.ModifyScheduledEvent(fixedID, p, map[string]any{}) },
			func(e string) string { return "/guilds/" + fixedID + "/scheduled-events/" + e }},
		{"GetScheduledEvent guild", `{"id":"ev-1"}`,
			func(c *DiscordClient, p string) { _, _, _ = c.GetScheduledEvent(p, fixedID) },
			func(e string) string { return "/guilds/" + e + "/scheduled-events/" + fixedID }},
		{"GetScheduledEvent event", `{"id":"ev-1"}`,
			func(c *DiscordClient, p string) { _, _, _ = c.GetScheduledEvent(fixedID, p) },
			func(e string) string { return "/guilds/" + fixedID + "/scheduled-events/" + e }},
		{"DeleteScheduledEvent event", `{}`,
			func(c *DiscordClient, p string) { _ = c.DeleteScheduledEvent(fixedID, p) },
			func(e string) string { return "/guilds/" + fixedID + "/scheduled-events/" + e }},

		// CreateScheduledEvent is the one site where the value is NOT a
		// snowflake by construction: payloadGuildID digs it out of a caller's
		// payload map. Everything else on this list arrives from the store or
		// from Discord itself.
		{"CreateScheduledEvent payload guild", `{"id":"ev-1"}`,
			func(c *DiscordClient, p string) {
				_, _ = c.CreateScheduledEvent(map[string]any{"guild_id": p})
			},
			func(e string) string { return "/guilds/" + e + "/scheduled-events" }},
	}

	for _, site := range sites {
		for _, probe := range pathProbeIDs {
			t.Run(site.name+" "+probe.name, func(t *testing.T) {
				client, got := recordDiscordRequestURI(t, site.body)
				site.call(client, probe.id)

				want := site.want(url.PathEscape(probe.id))
				if *got != want {
					t.Errorf("id %q addressed %q, want %q", probe.id, *got, want)
				}
			})
		}
	}
}

// TestSendDirectMessageEscapesTheChannelDiscordHandedBack covers the one caller
// whose id comes back in a response body rather than from an argument: the DM
// channel Discord mints in answer to POST /users/@me/channels. It is Discord's
// own value, which is exactly why nothing here validates it.
func TestSendDirectMessageEscapesTheChannelDiscordHandedBack(t *testing.T) {
	for _, probe := range pathProbeIDs {
		t.Run(probe.name, func(t *testing.T) {
			var second string
			seen := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen++
				w.Header().Set("Content-Type", "application/json")
				if seen == 1 {
					// The DM-channel create. Hand back the probe as the id.
					_, _ = w.Write([]byte(`{"id":` + jsonString(probe.id) + `}`))
					return
				}
				second = r.RequestURI
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			client := NewDiscordClient(srv.URL, func() (string, error) { return "tok", nil })
			_ = client.SendDirectMessage(fixedID, "hello")

			want := "/channels/" + url.PathEscape(probe.id) + "/messages"
			if second != want {
				t.Errorf("dm channel %q addressed %q, want %q", probe.id, second, want)
			}
		})
	}
}

// jsonString quotes a value for a hand-built JSON body, so the probe ids reach
// the client through a real decode rather than through a struct literal.
func jsonString(s string) string {
	out := []rune{'"'}
	for _, r := range s {
		switch r {
		case '"', '\\':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(append(out, '"'))
}

// TestAdoptForumEscapesEveryChannelItAddresses covers the one path build that
// is not on a DiscordClient method at all: AdoptForum reaches past the client's
// own API and calls do() directly to re-read a forum's available_tags before
// PATCHing them. Nothing else drives it, so without this test that site sits
// outside the table above — the scorer's restore-the-bug-forum-3 arm survived
// until this was added, which is how the hole was found rather than assumed.
//
// It asserts the whole request sequence rather than "the escaped line appears
// somewhere". AdoptForum addresses the same channel five times over three call
// sites, so a membership check is satisfied by any one of them and would read
// green with the raw read still unescaped.
func TestAdoptForumEscapesEveryChannelItAddresses(t *testing.T) {
	for _, probe := range pathProbeIDs {
		t.Run(probe.name, func(t *testing.T) {
			var lines []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lines = append(lines, r.Method+" "+r.RequestURI)
				w.Header().Set("Content-Type", "application/json")
				// A forum with none of the managed tags, so AdoptForum takes
				// the branch that reads the channel back raw.
				_, _ = w.Write([]byte(`{"type":15,"available_tags":[]}`))
			}))
			t.Cleanup(srv.Close)

			server := NewServer(testStore(t), nil,
				NewDiscordClient(srv.URL, func() (string, error) { return "tok", nil }))
			_, _ = server.AdoptForum(fixedID, probe.id)

			at := "/channels/" + url.PathEscape(probe.id)
			want := []string{
				"GET " + at,   // ForumChannelTags
				"GET " + at,   // the raw read-back this test exists for
				"PATCH " + at, // ModifyThread, adding the managed tags
				"GET " + at,   // ForumChannelTags again, for the new tag ids
				"PATCH " + at, // ModifyThread, setting the default reaction
			}
			if len(lines) != len(want) {
				t.Fatalf("channel %q addressed %d times, want %d: %q",
					probe.id, len(lines), len(want), lines)
			}
			for i := range want {
				if lines[i] != want[i] {
					t.Errorf("channel %q: request %d was %q, want %q",
						probe.id, i, lines[i], want[i])
				}
			}
		})
	}
}
