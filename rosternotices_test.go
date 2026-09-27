package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSettledRosterChanges: a join or leave is told once the person has left
// it alone for a minute, compared from where it started to where it ended.
func TestSettledRosterChanges(t *testing.T) {
	update := func(user, from, to, actor string, at int64) SignupUpdate {
		return SignupUpdate{DiscordUserID: user, FromState: from, ToState: to, Actor: actor, At: at}
	}
	for _, c := range []struct {
		name    string
		updates []SignupUpdate
		after   int64
		through int64
		want    []string
	}{
		{"a join", []SignupUpdate{update("u1", "", StateAttending, ActorUser, 100)}, 0, 200,
			[]string{"u1 -> attending"}},
		{"a join not yet a minute old waits", []SignupUpdate{update("u1", "", StateAttending, ActorUser, 100)}, 0, 99, nil},
		{"a join already told is not told again", []SignupUpdate{update("u1", "", StateAttending, ActorUser, 100)}, 100, 200, nil},
		{"join then leave within the minute is nothing", []SignupUpdate{
			update("u1", "", StateAttending, ActorUser, 100),
			update("u1", StateAttending, StateWithdrawn, ActorUser, 130)}, 0, 200, nil},
		{"join then leave further apart is both", []SignupUpdate{
			update("u1", "", StateWaitlisted, ActorReaction, 100),
			update("u1", StateWaitlisted, StateWithdrawn, ActorUser, 170)}, 0, 200,
			[]string{"u1 -> waitlisted", "u1 -> withdrawn"}},
		{"a leave to Maybe", []SignupUpdate{update("u1", StateAttending, StateMaybe, ActorUser, 100)}, 0, 200,
			[]string{"u1 -> maybe"}},
		{"an organiser's add is not told", []SignupUpdate{update("u1", "", StateAttending, "web:u-org", 100)}, 0, 200, nil},
		{"a date rolling over is not told", []SignupUpdate{update("u1", StateAttending, StateWithdrawn, ActorRecurrence, 100)}, 0, 200, nil},
		{"moving up off the waitlist is not a join", []SignupUpdate{update("u1", StateWaitlisted, StateAttending, ActorPromotion, 100)}, 0, 200, nil},
		{"a change still growing is not told", []SignupUpdate{
			update("u1", "", StateAttending, ActorUser, 100),
			update("u1", StateAttending, StateWithdrawn, ActorUser, 150)}, 0, 120, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, ch := range settledRosterChanges(c.updates, c.after, c.through) {
				got = append(got, ch.UserID+" -> "+ch.To)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("changes = %v, want %v", got, c.want)
			}
		})
	}
}

// rosterNoticeFixture is a one-place event created by u-org, who watches it,
// with u1 going, u2 waiting and u3 having joined and left within the minute
// — all two minutes ago.
func rosterNoticeFixture(t *testing.T) (*fakeDiscord, *Store, *Server, *Event) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	t.Cleanup(srv.WaitForBackgroundWork)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Board games", Capacity: 1,
		StartsAt: farFutureStart, CreatedBy: "u-org"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.WatchRoster(ev.ID, "u-org"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	for _, j := range []struct{ id, name string }{{"u1", "Ann"}, {"u2", "Bea"}, {"u3", "Cal"}} {
		if _, err := store.Join(ev.ID, j.id, j.name, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", j.id, err)
		}
	}
	if _, err := store.Leave(ev.ID, "u3", ""); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE signup_updates SET at = at - 120`); err != nil {
		t.Fatalf("backdate updates: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE roster_watchers SET watching_since = watching_since - 300,
		reported_through = reported_through - 300`); err != nil {
		t.Fatalf("backdate watch: %v", err)
	}
	return fake, store, srv, ev
}

func TestARosterNoticeTellsSettledJoinsOnce(t *testing.T) {
	fake, _, srv, ev := rosterNoticeFixture(t)

	sent, err := srv.SendRosterNotices()
	if err != nil || sent != 1 {
		t.Fatalf("sent = %d, %v; want 1", sent, err)
	}
	dms := callsTo(fake, http.MethodPost, "/channels/msg-1/messages")
	if len(dms) != 1 {
		t.Fatalf("DMs posted = %d, want 1", len(dms))
	}
	content, _ := dms[0].Body["content"].(string)
	for _, want := range []string{"**Ann** joined, going", "**Bea** joined the waitlist", "Now 1/1 going, 1 waiting"} {
		if !strings.Contains(content, want) {
			t.Errorf("notice lacks %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "Cal") {
		t.Errorf("notice names Cal, who joined and left within the minute:\n%s", content)
	}
	if flags, _ := dms[0].Body["flags"].(float64); int(flags)&messageFlagSuppressEmbeds == 0 {
		t.Errorf("flags = %v: the event page link would be previewed as Discord's login page", dms[0].Body["flags"])
	}
	raw, _ := json.Marshal(dms[0].Body["components"])
	if !strings.Contains(string(raw), GiveAPlaceCustomID(ev.ID, "u2")) || strings.Contains(string(raw), "u1") {
		t.Errorf("buttons = %s, want one to give Bea (u2) a place and none for Ann", raw)
	}

	if sent, err := srv.SendRosterNotices(); err != nil || sent != 0 {
		t.Errorf("second sweep sent %d, %v; want 0", sent, err)
	}
}

// pressInDM is a button press in the bot's DMs, where Discord sends a user
// and no member, guild or permissions.
func pressInDM(t *testing.T, srv *Server, userID, customID string) string {
	t.Helper()
	raw := fmt.Sprintf(`{"type":3,"channel_id":"dm","user":{"id":%q},"data":{"custom_id":%q},"message":{"id":"notice"}}`,
		userID, customID)
	var in Interaction
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("build interaction: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.handleComponent(rec, &in)
	return rec.Body.String()
}

func TestTheNoticeButtonGivesAPlaceOnlyToWhoMayEdit(t *testing.T) {
	fake, store, srv, ev := rosterNoticeFixture(t)
	fake.on(http.MethodGet, "/guilds/g1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"g1","owner_id":"u-owner"}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/roles", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"g1","permissions":"0"},{"id":"role-events","permissions":"%d"}]`, permissionManageEvents)
	})
	fake.on(http.MethodGet, "/guilds/g1/members/u-stranger", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"roles":[]}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/u-mod", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"roles":["role-events"]}`))
	})
	button := GiveAPlaceCustomID(ev.ID, "u2")
	state := func() string {
		roster, err := store.Roster(ev.ID, true)
		if err != nil {
			t.Fatalf("roster: %v", err)
		}
		for _, sg := range roster {
			if sg.DiscordUserID == "u2" {
				return sg.State
			}
		}
		return ""
	}

	if reply := pressInDM(t, srv, "u-stranger", button); !strings.Contains(reply, "cannot give places") || state() != StateWaitlisted {
		t.Fatalf("a member without Manage Events pressed it: reply %s, u2 %s", reply, state())
	}
	if reply := pressInDM(t, srv, "u-mod", button); !strings.Contains(reply, "is going now") || state() != StateAttending {
		t.Fatalf("a member with Manage Events pressed it: reply %s, u2 %s", reply, state())
	}
	if reply := pressInDM(t, srv, "u-org", button); !strings.Contains(reply, "not on the waitlist") {
		t.Errorf("a second press: reply %s, want that they are not waiting", reply)
	}
}
