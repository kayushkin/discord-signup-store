package discordsignup

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The bot's own user id, used as both the real id and the id the guards compare
// against, so the only variable in every table below is whether the lookup that
// produces it succeeded.
const guardBotID = "bot-1"

// usersMeAlwaysFails installs a /users/@me handler that never succeeds — the
// persistent case, as opposed to usersMeFailingTimes' transient one.
//
// A revoked token, a bot removed from the guild, or auth-store down for the
// whole window all look like this: applicationUserID retries every time and
// returns "" every time. The retry landed by TestBotUserIDIsLookedUpAgainAfterAFailure
// fixed the transient case; it cannot fix this one, and this is what the three
// guards below see when it happens.
func usersMeAlwaysFails(f *fakeDiscord) {
	f.on(http.MethodGet, "/users/@me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"401: Unauthorized"}`)
	})
}

// serverWithBotIDKnown builds a Server whose /users/@me answers, so
// applicationUserID() == guardBotID.
func serverWithBotIDKnown(t *testing.T) (*Server, *fakeDiscord) {
	t.Helper()
	fake := newFakeDiscord(t)
	usersMeFailingTimes(fake, 0, guardBotID)
	srv := NewServer(testStore(t), nil, fake.client())
	if got := srv.applicationUserID(); got != guardBotID {
		t.Fatalf("fixture is wrong: applicationUserID() = %q, want %q", got, guardBotID)
	}
	return srv, fake
}

// serverWithBotIDUnknown builds a Server whose /users/@me never answers, so
// applicationUserID() == "".
func serverWithBotIDUnknown(t *testing.T) (*Server, *fakeDiscord) {
	t.Helper()
	fake := newFakeDiscord(t)
	usersMeAlwaysFails(fake)
	srv := NewServer(testStore(t), nil, fake.client())
	if got := srv.applicationUserID(); got != "" {
		t.Fatalf("fixture is wrong: applicationUserID() = %q, want %q", got, "")
	}
	return srv, fake
}

// TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheImportGuard is site 3 of 3,
// discordevents.go's `r.CreatorID == s.applicationUserID()`.
//
// ⚠️ CHARACTERISATION. It asserts what the code does TODAY, so it passes against
// unmodified source and is expected to go red when the repair lands. When it
// reddens: the guard stopped failing open, which is the point — delete this test
// and close card dc0c4346-a5d9-4fbf-82a8-b0b4000da1ea.
//
// The guard exists so an event this bot published seconds ago is not re-imported
// as a second local event before its id has been written back. "" matches
// nobody, so an unknown id turns the skip into an import — and the comment above
// the guard says what that costs: "the unique index then refuses, leaving a
// duplicate and a broken link".
func TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheImportGuard(t *testing.T) {
	// The native event this bot created. CreatorID is the bot's real id in both
	// arms; only the lookup differs.
	native := DiscordScheduledEvent{
		ID:        "native-1",
		GuildID:   "g1",
		ChannelID: "c1",
		CreatorID: guardBotID,
		Name:      "An event this bot published",
		Status:    discordEventScheduled,
		// An event with no start is refused before either guard is reached.
		ScheduledStartTime: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
	}

	t.Run("bot id known — the guard fires and the event is skipped", func(t *testing.T) {
		srv, _ := serverWithBotIDKnown(t)
		_, imported, err := srv.syncOneScheduledEvent(native, "board-1")
		if err != nil {
			t.Fatalf("syncOneScheduledEvent: %v", err)
		}
		if imported {
			t.Error("imported = true, want false — the bot's own event must be skipped")
		}
		if _, err := srv.store.EventByDiscordScheduledEventID(native.ID); err == nil {
			t.Error("a local event was created for the bot's own native event")
		}
	})

	t.Run("bot id unknown — the guard fails OPEN and the event is imported", func(t *testing.T) {
		srv, _ := serverWithBotIDUnknown(t)
		_, imported, err := srv.syncOneScheduledEvent(native, "board-1")
		if err != nil {
			t.Fatalf("syncOneScheduledEvent: %v", err)
		}
		if !imported {
			t.Fatal("imported = false — the guard now distinguishes an unknown id " +
				"from 'not me'. That is the repair; delete this characterisation and close dc0c4346.")
		}
		if _, err := srv.store.EventByDiscordScheduledEventID(native.ID); err != nil {
			t.Errorf("expected the duplicate local event to exist: %v", err)
		}
	})
}

// TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheReactionRemoveGuard is
// site 2 of 3, gateway.go:221.
//
// ⚠️ CHARACTERISATION — see the note on the import-guard test above.
//
// The bot removes reactions itself to reconcile state after a button Leave. The
// guard is what stops those removals being read as a member leaving; the comment
// above onReactionRemove calls that "the loop", and this is the id that breaks it.
func TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheReactionRemoveGuard(t *testing.T) {
	// The bot's own ✅ removal, arriving over the gateway.
	removal := &discordgo.MessageReactionRemove{
		MessageReaction: &discordgo.MessageReaction{
			UserID:    guardBotID,
			MessageID: "post-1",
			GuildID:   "g1",
			Emoji:     discordgo.Emoji{Name: joinReactionEmoji},
		},
	}

	// seedRoster puts the bot on an event's roster so a Leave has something to
	// take off — without it both arms are a no-op for the same reason and the
	// test could not tell them apart.
	seedRoster := func(t *testing.T, srv *Server) *Event {
		t.Helper()
		ev := eventWithForumPost(t, srv.store)
		if _, err := srv.store.Join(ev.ID, guardBotID, "The bot", JoinedViaReaction); err != nil {
			t.Fatalf("seed join: %v", err)
		}
		return ev
	}

	attending := func(t *testing.T, srv *Server, eventID int64) bool {
		t.Helper()
		roster, err := srv.store.Roster(eventID, false)
		if err != nil {
			t.Fatalf("roster: %v", err)
		}
		for _, s := range roster {
			if s.DiscordUserID == guardBotID && s.State == StateAttending {
				return true
			}
		}
		return false
	}

	t.Run("bot id known — the guard fires and the roster is untouched", func(t *testing.T) {
		srv, _ := serverWithBotIDKnown(t)
		ev := seedRoster(t, srv)
		listener := &GatewayListener{server: srv}

		listener.onReactionRemove(nil, removal)

		if !attending(t, srv, ev.ID) {
			t.Error("the bot's own reaction removal was processed as a member Leave")
		}
	})

	t.Run("bot id unknown — the guard fails OPEN and the removal is a Leave", func(t *testing.T) {
		srv, _ := serverWithBotIDUnknown(t)
		ev := seedRoster(t, srv)
		listener := &GatewayListener{server: srv}

		listener.onReactionRemove(nil, removal)

		if attending(t, srv, ev.ID) {
			t.Fatal("the roster survived — the guard now distinguishes an unknown id " +
				"from 'not me'. That is the repair; delete this characterisation and close dc0c4346.")
		}
	})
}

// TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheReactionAddGuard is site
// 1 of 3, gateway.go:173, and the one that costs a capacity seat.
//
// ⚠️ CHARACTERISATION — see the note on the import-guard test above.
//
// The bot seeds its own ✅ on every post it makes, so this guard is asked on the
// bot's own reaction for every single event. With an unknown id the bot joins
// its own event and occupies a place against the capacity that is the whole
// point of this service.
func TestAnUnknownBotIDIsIndistinguishableFromNotMeAtTheReactionAddGuard(t *testing.T) {
	add := &discordgo.MessageReactionAdd{
		MessageReaction: &discordgo.MessageReaction{
			UserID:    guardBotID,
			MessageID: "post-1",
			GuildID:   "g1",
			Emoji:     discordgo.Emoji{Name: joinReactionEmoji},
		},
	}

	seedEvent := func(t *testing.T, srv *Server) *Event {
		t.Helper()
		return eventWithForumPost(t, srv.store)
	}

	onRoster := func(t *testing.T, srv *Server, eventID int64) bool {
		t.Helper()
		roster, err := srv.store.Roster(eventID, true)
		if err != nil {
			t.Fatalf("roster: %v", err)
		}
		for _, s := range roster {
			if s.DiscordUserID == guardBotID {
				return true
			}
		}
		return false
	}

	t.Run("bot id known — the guard fires and nobody is added", func(t *testing.T) {
		srv, _ := serverWithBotIDKnown(t)
		ev := seedEvent(t, srv)
		listener := &GatewayListener{server: srv, session: offlineGatewaySession(t)}

		listener.onReactionAdd(nil, add)

		if onRoster(t, srv, ev.ID) {
			t.Error("the bot joined its own event")
		}
	})

	t.Run("bot id unknown — the guard fails OPEN and the bot takes a seat", func(t *testing.T) {
		srv, _ := serverWithBotIDUnknown(t)
		ev := seedEvent(t, srv)
		listener := &GatewayListener{server: srv, session: offlineGatewaySession(t)}

		listener.onReactionAdd(nil, add)

		if !onRoster(t, srv, ev.ID) {
			t.Fatal("the bot stayed off its own roster — the guard now distinguishes an " +
				"unknown id from 'not me'. That is the repair; delete this characterisation " +
				"and close dc0c4346.")
		}
		fresh, err := srv.store.GetEvent(ev.ID)
		if err != nil {
			t.Fatalf("get event: %v", err)
		}
		if fresh.AttendingCount != 1 {
			t.Errorf("AttendingCount = %d, want 1 — the seat the bot took", fresh.AttendingCount)
		}
	})
}

// eventWithForumPost creates an event that EventByForumPostID can actually find.
//
// ⚠️ CreateEvent's INSERT does not carry forum_post_id — passing it on the Event
// struct is silently dropped, and both reaction handlers then return at their
// EventByForumPostID lookup for a reason that has nothing to do with the guard
// under test. That made the first draft of this file report the guard firing in
// BOTH arms. The write has to go through UpdateEvent, and the read-back below is
// what makes the fixture prove itself rather than be assumed.
func eventWithForumPost(t *testing.T, store *Store) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "c1", Name: "Test event", Capacity: 3,
		StartsAt: time.Now().Add(48 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	postID := "post-1"
	if _, err := store.UpdateEvent(ev.ID, EventPatch{ForumPostID: &postID}); err != nil {
		t.Fatalf("set forum post id: %v", err)
	}
	found, err := store.EventByForumPostID(postID)
	if err != nil {
		t.Fatalf("fixture is wrong: EventByForumPostID(%q): %v", postID, err)
	}
	return found
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// offlineGatewaySession builds a discordgo session whose every REST call is
// answered locally.
//
// onReactionAdd reaches g.session.GuildMember for the clicker's display name.
// That is discordgo's own client, not this package's, so the fakeDiscord server
// cannot intercept it — and a nil session panics. Answering every request with
// one member object keeps the handler on its real path without a network.
func offlineGatewaySession(t *testing.T) *discordgo.Session {
	t.Helper()
	session, err := discordgo.New("Bot test-bot-token")
	if err != nil {
		t.Fatalf("build gateway session: %v", err)
	}
	session.Client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"nick":"The bot","user":{"id":"` + guardBotID + `","username":"bot"}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		}),
	}
	return session
}
