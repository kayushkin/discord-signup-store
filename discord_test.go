package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCall is one request the fake Discord received.
type recordedCall struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// fakeDiscord stands in for the Discord REST API. Every test below asserts on
// the exact method and path, because a role grant that silently goes to the
// wrong URL looks identical to one that worked.
type fakeDiscord struct {
	mu       sync.Mutex
	calls    []recordedCall
	handlers map[string]func(w http.ResponseWriter, r *http.Request)
	server   *httptest.Server
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{handlers: map[string]func(http.ResponseWriter, *http.Request){}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := r.Method + " " + r.URL.Path

		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{
			Method: r.Method, Path: r.URL.Path,
			Auth: r.Header.Get("Authorization"), Body: body,
		})
		handler := f.handlers[key]
		f.mu.Unlock()

		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"msg-1"}`)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeDiscord) on(method, path string, h func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = h
}

func (f *fakeDiscord) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeDiscord) client() *DiscordClient {
	return NewDiscordClient(f.server.URL, func() (string, error) { return "test-bot-token", nil })
}

func TestRoleCallsUseTheRightMethodAndPath(t *testing.T) {
	fake := newFakeDiscord(t)
	client := fake.client()

	if err := client.AddMemberRole("g1", "u1", "r1"); err != nil {
		t.Fatalf("add role: %v", err)
	}
	if err := client.RemoveMemberRole("g1", "u1", "r1"); err != nil {
		t.Fatalf("remove role: %v", err)
	}

	calls := fake.recorded()
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want 2", len(calls))
	}
	want := "/guilds/g1/members/u1/roles/r1"
	if calls[0].Method != http.MethodPut || calls[0].Path != want {
		t.Errorf("add = %s %s, want PUT %s", calls[0].Method, calls[0].Path, want)
	}
	if calls[1].Method != http.MethodDelete || calls[1].Path != want {
		t.Errorf("remove = %s %s, want DELETE %s", calls[1].Method, calls[1].Path, want)
	}
	// "Bot <token>", not "Bearer". A bot token sent as Bearer is a 401 that
	// reads exactly like a wrong token.
	if calls[0].Auth != "Bot test-bot-token" {
		t.Errorf("Authorization = %q, want %q", calls[0].Auth, "Bot test-bot-token")
	}
}

func TestDirectMessageRefusalIsRecognised(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"code":%d,"message":"Cannot send messages to this user"}`, errorCodeCannotMessageUser)
	})

	err := fake.client().SendDirectMessage("u1", "hello")
	if !errors.Is(err, ErrCannotMessageUser) {
		t.Fatalf("err = %v, want ErrCannotMessageUser — a closed DM must be "+
			"distinguishable so the caller can fall back to a channel mention", err)
	}
}

func TestRateLimitIsRetriedOnce(t *testing.T) {
	fake := newFakeDiscord(t)
	var attempts int
	fake.on(http.MethodPut, "/guilds/g1/members/u1/roles/r1", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"retry_after":0.01}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := fake.client().AddMemberRole("g1", "u1", "r1"); err != nil {
		t.Fatalf("add role: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one 429, one success)", attempts)
	}
}

// TestRoleSyncGrantsAndRevokesForEachState pins the projection: attending gets
// the attending role and loses the waitlist one, and withdrawing loses both.
func TestRoleSyncGrantsAndRevokesForEachState(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", StartsAt: farFutureStart, ChannelID: "c1", Name: "Roles", Capacity: 1,
		AttendingRoleID: "going", WaitlistRoleID: "waiting",
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	srv := NewServer(store, nil, fake.client())

	if err := srv.applyRoles(ev, stateChange{UserID: "u1", State: StateAttending}); err != nil {
		t.Fatalf("apply attending: %v", err)
	}
	if err := srv.applyRoles(ev, stateChange{UserID: "u2", State: StateWaitlisted}); err != nil {
		t.Fatalf("apply waitlisted: %v", err)
	}
	if err := srv.applyRoles(ev, stateChange{UserID: "u3", State: StateWithdrawn}); err != nil {
		t.Fatalf("apply withdrawn: %v", err)
	}

	var got []string
	for _, c := range fake.recorded() {
		got = append(got, c.Method+" "+c.Path)
	}
	want := []string{
		"PUT /guilds/g1/members/u1/roles/going",
		"DELETE /guilds/g1/members/u1/roles/waiting",
		"DELETE /guilds/g1/members/u2/roles/going",
		"PUT /guilds/g1/members/u2/roles/waiting",
		"DELETE /guilds/g1/members/u3/roles/going",
		"DELETE /guilds/g1/members/u3/roles/waiting",
	}
	if len(got) != len(want) {
		t.Fatalf("made %d role calls, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestClosedEventEditRemovesTheButtons stops a stale Join button sitting under
// a message that says signups are closed.
func TestClosedEventEditRemovesTheButtons(t *testing.T) {
	open := &Event{ID: 1, Name: "Open one", Status: StatusOpen, Capacity: 5, AttendingCount: 2}
	closed := &Event{ID: 1, Name: "Closed one", Status: StatusClosed, Capacity: 5, AttendingCount: 5}

	openPayload := RenderForumCard(open, nil)
	components, ok := openPayload["components"].([]any)
	if !ok || len(components) == 0 {
		t.Fatal("an open event has no buttons")
	}

	closedPayload := RenderForumCard(closed, nil)
	closedComponents, ok := closedPayload["components"].([]any)
	if !ok {
		t.Fatal("components key missing on a closed event — omitting it leaves the old buttons live")
	}
	if len(closedComponents) != 0 {
		t.Errorf("closed event still has %d component rows", len(closedComponents))
	}
}

func TestRenderedMessageStatesBothNumbers(t *testing.T) {
	ev := &Event{ID: 1, Name: "Workshop", Status: StatusOpen, Capacity: 20,
		AttendingCount: 20, WaitlistCount: 7}
	content, _ := RenderForumCard(ev, nil)["content"].(string)
	if !strings.Contains(content, "20/20") {
		t.Errorf("content = %q, want the places taken", content)
	}
	if !strings.Contains(content, "7 waiting") {
		t.Errorf("content = %q, want the waitlist size", content)
	}
}

// TestLongRosterIsTrimmedToDiscordsLimit covers the one place truncation is
// allowed: the edge that renders a Discord message. Roster() itself never trims.
func TestLongRosterIsTrimmedToDiscordsLimit(t *testing.T) {
	ev := &Event{ID: 1, Name: "Big", Status: StatusOpen, Capacity: 0, AttendingCount: 300}
	roster := make([]Signup, 300)
	for i := range roster {
		roster[i] = Signup{DiscordUserID: fmt.Sprintf("1000000000000%05d", i), State: StateAttending}
	}
	content, _ := RenderForumCard(ev, roster)["content"].(string)
	if len(content) > discordMessageContentLimit {
		t.Errorf("content is %d bytes, over Discord's %d limit — this would be a 400",
			len(content), discordMessageContentLimit)
	}
	if !strings.Contains(content, "trimmed") {
		t.Error("the message was trimmed without saying so")
	}
}

// TestCreatingFromDiscordAlsoMakesANativeEvent covers the point of publishing:
// the event shows up in the server's own event list, not only on the board.
func TestCreatingFromDiscordAlsoMakesANativeEvent(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	srv.SetDefaultTimezone("UTC")

	fake.on(http.MethodPost, "/guilds/g1/scheduled-events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"native-99","name":"Board game night"}`)
	})

	start := time.Now().Add(72 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Board game night",
		StartsAt: start, Capacity: 8, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.PublishToDiscord(ev.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var payload map[string]any
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/guilds/g1/scheduled-events" {
			payload = c.Body
		}
	}
	if payload == nil {
		t.Fatal("no native event was created")
	}
	// EXTERNAL (3) is the only entity type that needs no voice or stage
	// channel, and GUILD_ONLY (2) is the only privacy level Discord accepts.
	if payload["entity_type"].(float64) != 3 {
		t.Errorf("entity_type = %v, want 3 (EXTERNAL)", payload["entity_type"])
	}
	if payload["privacy_level"].(float64) != 2 {
		t.Errorf("privacy_level = %v, want 2", payload["privacy_level"])
	}
	// Discord refuses an EXTERNAL event with an empty location, so one is
	// always sent even when nobody typed one.
	meta := payload["entity_metadata"].(map[string]any)
	if meta["location"].(string) == "" {
		t.Error("no location sent; Discord refuses an EXTERNAL event without one")
	}
	// Pressing Interested puts you on the roster — MarkInterested joins you,
	// waitlisting you if the event is full — so the description has to say so.
	// It said the opposite for months while the pinned how-to said this, which
	// is two shipped messages contradicting each other about the one thing
	// somebody reading a Discord event needs to know.
	if desc, _ := payload["description"].(string); !strings.Contains(desc, "**Interested** here") {
		t.Errorf("description = %q, want it to offer Interested as a way to sign up", desc)
	}
	if payload["scheduled_end_time"] == nil || payload["scheduled_end_time"] == "" {
		t.Error("no end time sent; Discord requires one on an EXTERNAL event")
	}

	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.DiscordScheduledEventID != "native-99" {
		t.Errorf("link = %q, want the id Discord handed back", got.DiscordScheduledEventID)
	}
}

// TestSyncSkipsEventsThisBotJustPublished closes the race that automatic
// publishing opens.
//
// The gateway sees GUILD_SCHEDULED_EVENT_CREATE before the id we were handed
// has been written back. Without the creator check, the sync would import our
// own output as a second local event, and the unique index would then refuse
// the link — leaving a duplicate and a broken pointer.
func TestSyncSkipsEventsThisBotJustPublished(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	fake.on(http.MethodGet, "/users/@me", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"the-bot"}`)
	})
	fake.on(http.MethodGet, "/guilds/g1/scheduled-events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"id":"ours","guild_id":"g1","creator_id":"the-bot","name":"Just published",
			 "scheduled_start_time":"2026-09-05T19:00:00+00:00","status":1,"entity_type":3},
			{"id":"theirs","guild_id":"g1","creator_id":"a-person","name":"Made by a human",
			 "scheduled_start_time":"2026-09-06T19:00:00+00:00","status":1,"entity_type":3}
		]`)
	})

	result, err := srv.SyncScheduledEvents("g1")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Imported != 1 {
		t.Errorf("imported %d, want 1 — only the human's event", result.Imported)
	}
	events, err := store.ListEvents("g1", "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, ev := range events {
		if ev.Name == "Just published" {
			t.Error("the bot imported an event it had created itself")
		}
	}
}

// TestEditingPushesThroughToTheNativeEvent keeps the two from drifting.
func TestEditingPushesThroughToTheNativeEvent(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	start := time.Now().Add(48 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Before", StartsAt: start,
		Capacity: 4, Timezone: "UTC", DiscordScheduledEventID: "native-7",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ev.Name = "After"
	ev.Location = "The new place"
	if err := srv.PushEditToDiscord(ev, nil, true); err != nil {
		t.Fatalf("push: %v", err)
	}

	var patched map[string]any
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-7" {
			patched = c.Body
		}
	}
	if patched == nil {
		t.Fatal("the native event was not updated")
	}
	// The decoration is part of what gets pushed, and the stored name stays
	// clean — that separation is what stops the title compounding. An empty
	// capped event carries its limit, not a live count.
	if patched["name"].(string) != "After [0/4]" {
		t.Errorf("name = %v, want the name with its limit", patched["name"])
	}
	meta := patched["entity_metadata"].(map[string]any)
	if meta["location"].(string) != "The new place" {
		t.Errorf("location = %v", meta["location"])
	}
}

// TestUnpublishedEventsPushNothing stops an edit on an ordinary event making a
// pointless Discord call.
func TestUnpublishedEventsPushNothing(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())

	if err := srv.PushEditToDiscord(&Event{ID: 1, GuildID: "g1", Name: "Local only"}, nil, true); err != nil {
		t.Fatalf("push: %v", err)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Errorf("made %d Discord calls for an event with no native counterpart", len(calls))
	}
}

func TestPluralise(t *testing.T) {
	for count, want := range map[int]string{0: "0 places", 1: "1 place", 2: "2 places", 20: "20 places"} {
		if got := pluralise(count, "place"); got != want {
			t.Errorf("pluralise(%d) = %q, want %q", count, got, want)
		}
	}
}

// TestTheSignupBlockDoesNotRoundTrip covers an accumulating corruption.
//
// This service appends a block to a native event's description — how to sign
// up, how full it is, who is going. The sync then reads that description back
// as the event's own. Without stripping it, the next publish appends a block to
// a description that already ends in one, and it grows by a paragraph on every
// signup until Discord refuses the event for length.
func TestTheSignupBlockDoesNotRoundTrip(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Games", Capacity: 8, AttendingCount: 2}
	roster := []Signup{
		{DiscordUserID: "u1", DisplayName: "Alice", State: StateAttending},
		{DiscordUserID: "u2", DisplayName: "Bob", State: StateAttending},
	}
	written := "Bring dice."
	ev.Description = written

	published := nativeEventDescription(ev, roster, "board-channel")
	if !strings.Contains(published, "Going: Alice, Bob") {
		t.Errorf("description = %q, want it to list who is going", published)
	}
	if got := stripSignupPointer(published); got != written {
		t.Fatalf("after one round trip the description is %q, want %q", got, written)
	}

	// Ten more. The failure mode is growth, so the test has to iterate.
	current := written
	for i := 0; i < 10; i++ {
		ev.Description = current
		current = stripSignupPointer(nativeEventDescription(ev, roster, "board-channel"))
	}
	if current != written {
		t.Errorf("after eleven round trips the description is %q (%d chars), want %q",
			current, len(current), written)
	}
}

// TestAnOldSignupBlockIsStillStripped. Descriptions written by earlier versions
// are sitting on Discord right now, and the marker they start with is not the
// one this version writes. Forgetting the old form does not edit Discord — it
// stops the strip finding our own text, which then round-trips and grows.
func TestAnOldSignupBlockIsStillStripped(t *testing.T) {
	old := "Bring dice.\n\n— Signups are in the forum: https://discord.com/channels/g1/p1 " +
		"(8 places, 6 left). Pressing Interested here does not hold you a place."
	if got := stripSignupPointer(old); got != "Bring dice." {
		t.Errorf("stripSignupPointer of an old-style description = %q, want %q", got, "Bring dice.")
	}
}

// TestALongRosterIsTrimmedAndTheDescriptionIsNot. The words are the organiser's
// and the block is ours, so ours is what gives way.
func TestALongRosterIsTrimmedAndTheDescriptionIsNot(t *testing.T) {
	var roster []Signup
	for i := 0; i < 200; i++ {
		roster = append(roster, Signup{
			DiscordUserID: fmt.Sprintf("u%d", i),
			DisplayName:   fmt.Sprintf("Person Number %d", i),
			State:         StateAttending,
		})
	}
	written := strings.Repeat("word ", 100)
	ev := &Event{ID: 1, GuildID: "g1", Name: "Big", Capacity: 300, AttendingCount: 200,
		Description: written}

	got := nativeEventDescription(ev, roster, "board-channel")
	if n := len([]rune(got)); n > nativeDescriptionLimit {
		t.Errorf("description is %d runes, over Discord's %d", n, nativeDescriptionLimit)
	}
	if !strings.HasPrefix(got, written) {
		t.Error("the organiser's own words were trimmed to make room for ours")
	}
	if !strings.Contains(got, "more") {
		t.Errorf("description = %q, want the roster shortened rather than dropped", got)
	}
}

// TestABlockIsSkippedRatherThanCrowdingOutALongDescription: at 990 characters
// of somebody else's writing there is no room, and the answer is no block.
func TestABlockIsSkippedRatherThanCrowdingOutALongDescription(t *testing.T) {
	written := strings.Repeat("x", 990)
	ev := &Event{ID: 1, GuildID: "g1", Name: "Wordy", Capacity: 8, AttendingCount: 1,
		Description: written}
	got := nativeEventDescription(ev, []Signup{{DiscordUserID: "u1", DisplayName: "Alice",
		State: StateAttending}}, "board-channel")
	if got != written {
		t.Errorf("description = %q (%d runes), want the original untouched",
			got, len([]rune(got)))
	}
}

func TestStripSignupPointerLeavesOtherTextAlone(t *testing.T) {
	for _, description := range []string{
		"", "Bring dice.", "Signups are in the channel",
		"Multi\nline\ndescription", "— an em dash that is not ours",
	} {
		if got := stripSignupPointer(description); got != description {
			t.Errorf("stripSignupPointer(%q) = %q, want it untouched", description, got)
		}
	}
}

// TestTheTitleIsPushedOnEveryRosterChange covers the reason this needs pushing
// at all: the count in the title is stale the moment somebody joins.
func TestTheTitleIsPushedOnEveryRosterChange(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 3,
		StartsAt:                time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-5",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	srv.syncAfterChange(after.ID, nil)

	var pushed string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-5" {
			pushed, _ = c.Body["name"].(string)
		}
	}
	// First publish ever, one of three places taken: the count goes out.
	if pushed != "Games [1/3]" {
		t.Errorf("pushed name = %q, want the count on a first publish", pushed)
	}
}

// TestReconcilePublishesEventsThatMissedIt heals the first desync source: only
// modal-created events auto-published, so web- and API-created ones never
// reached Discord's list.
func TestReconcilePublishesEventsThatMissedIt(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	fake.on(http.MethodPost, "/guilds/g1/scheduled-events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"native-new","name":"x"}`)
	})

	future := time.Now().Add(72 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
		Name: "Never published", StartsAt: future, Timezone: "UTC"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// And one in the past, which Discord would refuse: skipped, not errored.
	if _, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board",
		Name: "Already started", StartsAt: time.Now().Add(-time.Hour).Unix()}); err != nil {
		t.Fatalf("create past: %v", err)
	}

	published, cancelled, _, problems := srv.reconcileWithNative("g1", nil)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if published != 1 || cancelled != 0 {
		t.Errorf("published=%d cancelled=%d, want 1 and 0", published, cancelled)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.DiscordScheduledEventID != "native-new" {
		t.Errorf("link = %q, want the new native id", got.DiscordScheduledEventID)
	}
}

// TestReconcileCancelsWhenTheNativeEventIsGone closes the structural hole:
// deleting a native event is how a person cancels in Discord's UI, and nothing
// used to notice.
func TestReconcileCancelsWhenTheNativeEventIsGone(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	// deleted: direct GET answers 404. completedNative: direct GET still finds
	// it, status COMPLETED — must NOT be cancelled, the time sweep owns it.
	fake.on(http.MethodGet, "/guilds/g1/scheduled-events/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":10070,"message":"Unknown Guild Scheduled Event"}`)
	})
	fake.on(http.MethodGet, "/guilds/g1/scheduled-events/done", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"done","status":3,"name":"x"}`)
	})

	future := time.Now().Add(72 * time.Hour).Unix()
	mk := func(name, nativeID string) *Event {
		ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: name,
			StartsAt: future, DiscordScheduledEventID: nativeID})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return ev
	}
	deleted := mk("Deleted natively", "gone")
	completed := mk("Completed natively", "done")

	published, cancelled, finished, problems := srv.reconcileWithNative("g1", nil)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cancelled != 1 || published != 0 {
		t.Errorf("cancelled=%d published=%d, want 1 and 0", cancelled, published)
	}
	if got, _ := store.GetEvent(deleted.ID); got.Status != StatusCancelled {
		t.Errorf("deleted-native event = %q, want cancelled", got.Status)
	}

	// This assertion used to read "want left for the time sweep", and that was
	// the bug rather than the behaviour. Absence from the list is ambiguous —
	// Discord drops COMPLETED events from it as well as deleted ones — and the
	// old code, having asked directly and been told COMPLETED, decided to do
	// nothing. Pressing End on a native event is an explicit act meaning NOW.
	// Waiting for the scheduled end left the event on the board with a live
	// Join button, out of past events, for however long remained: an event
	// ended half an hour early stayed up for half an hour.
	if finished != 1 {
		t.Errorf("finished=%d, want 1", finished)
	}
	if got, _ := store.GetEvent(completed.ID); got.Status != StatusCompleted {
		t.Errorf("completed-native event = %q, want completed the moment Discord says it is",
			got.Status)
	}
}

// TestReconcileDeletesTheNativeEventWhenCancelledLocally: cancelling on any
// surface cancels everywhere, including Discord's own list.
func TestReconcileDeletesTheNativeEventWhenCancelledLocally(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	future := time.Now().Add(72 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Called off",
		StartsAt: future, DiscordScheduledEventID: "native-5", Status: StatusCancelled})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	remote := []DiscordScheduledEvent{{ID: "native-5", GuildID: "g1", Status: discordEventScheduled}}
	if _, _, _, problems := srv.reconcileWithNative("g1", remote); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	var deleted bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodDelete && c.Path == "/guilds/g1/scheduled-events/native-5" {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the native event for cancelled %q was not deleted", ev.Name)
	}
}

// TestTheLocationPlaceholderDoesNotRoundTrip: the third instance of the same
// corruption, after the description pointer and the title badge. The filler
// sent because Discord refuses an EXTERNAL event with no location must not
// come home as a location somebody typed. Found live, on two events.
func TestTheLocationPlaceholderDoesNotRoundTrip(t *testing.T) {
	if got := stripLocationPlaceholder(locationPlaceholder); got != "" {
		t.Errorf("the placeholder came home as %q", got)
	}
	for _, real := range []string{"The pub", "See the signup card please", ""} {
		if got := stripLocationPlaceholder(real); got != real {
			t.Errorf("stripLocationPlaceholder(%q) = %q, want untouched", real, got)
		}
	}
}

// TestTitlesCarryFullOrTheLimitNeverTheLiveCount. A thread rename is
// rate-limited to about two per ten minutes, so a live count in a title was
// the count from two renames ago. What is in a title now changes only when the
// event fills, empties back below its cap, or has its cap changed.
func TestTitlesCarryFullOrTheLimitNeverTheLiveCount(t *testing.T) {
	for _, c := range []struct {
		capacity, attending int
		want                string
	}{
		{8, 3, "Board game night [3/8]"},
		{8, 8, "[Full] Board game night"},
		{8, 9, "[Full] Board game night"}, // over, after a lowered cap
		{0, 12, "Board game night"},
		{1, 0, "Board game night [0/1]"},
	} {
		ev := &Event{Name: "Board game night", Capacity: c.capacity, AttendingCount: c.attending}
		if got := nativeEventName(ev); got != c.want {
			t.Errorf("capacity %d, %d attending: %q, want %q", c.capacity, c.attending, got, c.want)
		}
		if titleIsFull(c.want) && strings.Contains(c.want, "/") {
			t.Errorf("%q carries both Full and a count; Full replaces it", c.want)
		}
	}
}

// TestEveryTitleFormEverWrittenIsStripped is the round-trip guard. Names read
// back from Discord are stored as the event's own, and titles decorated by
// earlier versions are sitting there now — miss one and the next publish
// decorates a name that already carries last week's decoration.
func TestEveryTitleFormEverWrittenIsStripped(t *testing.T) {
	for _, decorated := range []string{
		"[3/8] Games",       // the retired live badge
		"[Full] Games",      // full now
		"Games [3/8]",       // room now
		"Games · 8 places",  // the form written for one evening
		"Games · 1 place",   // singular of that form
		"[3/8] Games [3/8]", // never written, but strip it anyway
	} {
		if got := stripTitleDecorations(decorated); got != "Games" {
			t.Errorf("stripTitleDecorations(%q) = %q, want %q", decorated, got, "Games")
		}
	}
	// Twenty round trips through every state must never grow the name.
	ev := &Event{Name: "Board game night", Capacity: 8}
	current := ev.Name
	for i := 0; i < 20; i++ {
		ev.AttendingCount = i % 9
		ev.Name = current
		current = stripTitleDecorations(nativeEventName(ev))
	}
	if current != "Board game night" {
		t.Errorf("after twenty round trips the name is %q", current)
	}
}

// TestDecorationsSurviveALongName: the name is trimmed, never the decorations.
func TestDecorationsSurviveALongName(t *testing.T) {
	ev := &Event{Name: strings.Repeat("long name ", 20), Capacity: 8, AttendingCount: 8}
	got := nativeEventName(ev)
	if n := len([]rune(got)); n > discordEventNameLimit {
		t.Errorf("name is %d runes, over Discord's %d", n, discordEventNameLimit)
	}
	if !strings.HasPrefix(got, "[Full] ") {
		t.Errorf("name = %q, want [Full] kept at the front", got)
	}
	ev.AttendingCount = 3
	got = nativeEventName(ev)
	if n := len([]rune(got)); n > discordEventNameLimit {
		t.Errorf("name is %d runes, over Discord's %d", n, discordEventNameLimit)
	}
	if !strings.HasSuffix(got, " [3/8]") {
		t.Errorf("name = %q, want the limit kept at the end", got)
	}
}

// TestStripTitleDecorationsLeavesRealNamesAlone keeps the patterns from eating
// something a person typed.
func TestStripTitleDecorationsLeavesRealNamesAlone(t *testing.T) {
	for _, name := range []string{
		"Games", "[Board] games", "3/8 of the way there", "Full house", "Games · 8 people",
		"[Full]Games", "Games · places", "Games [3 of 8]", "Games[3/8]",
	} {
		if got := stripTitleDecorations(name); got != name {
			t.Errorf("stripTitleDecorations(%q) = %q, want it untouched", name, got)
		}
	}
}

// TestACountRenameWaitsTenMinutesButFillingDoesNot pins the rename budget.
// Discord allows a thread about two renames per ten minutes; a count change
// alone spends one every ten, and becoming full spends the other at once,
// because that is the change a reader most needs and it is rare. A place
// opening up waits like a count change: it is usually taken again within
// minutes, and a title that says open when the place has gone is the worse
// lie. A moved date waits too — only the forum title carries it, and the
// forum pair is what says it changed.
func TestACountRenameWaitsTenMinutesButFillingDoesNot(t *testing.T) {
	const at = int64(1_000_000)
	base := func(attending int) *Event {
		ev := &Event{Name: "Games", Capacity: 4, AttendingCount: attending,
			StartsAt: 1_700_000_000, Timezone: "UTC", TitleWrittenAt: at}
		ev.NativeTitleWritten = nativeEventName(&Event{Name: "Games", Capacity: 4, AttendingCount: 1})
		ev.ForumTitleWritten = forumPostTitle(&Event{Name: "Games", Capacity: 4, AttendingCount: 1,
			StartsAt: 1_700_000_000, Timezone: "UTC"})
		return ev
	}
	renamed := base(1)
	renamed.Name = "Board games"
	moved := base(1)
	moved.StartsAt += 86400
	wasFull := base(3)
	wasFull.NativeTitleWritten = "[Full] Games"
	wasFull.ForumTitleWritten = "[Full] Games — 11/14 10:13pm"
	for _, c := range []struct {
		name string
		ev   *Event
		now  int64
		want bool
	}{
		{"never written", &Event{Name: "Games", Capacity: 4, AttendingCount: 1}, at, true},
		{"same title", base(1), at + 10, false},
		{"count moved, 10s later", base(2), at + 10, false},
		{"count moved, 9m59s later", base(2), at + 599, false},
		{"count moved, 10m later", base(2), at + 600, true},
		{"became full, at once", base(4), at + 1, true},
		{"renamed by the organiser, at once", renamed, at + 1, true},
		{"date moved, waits its turn", moved, at + 1, false},
		{"date moved, 10m later", moved, at + 600, true},
		{"was full, someone left, waits its turn", wasFull, at + 1, false},
		{"was full, someone left, 10m later", wasFull, at + 600, true},
		{"a limit set on an uncapped event, at once", func() *Event {
			ev := base(1)
			ev.NativeTitleWritten, ev.ForumTitleWritten = "Games", "Games — 11/14 10:13pm"
			return ev
		}(), at + 1, true},
		{"the limit raised, at once", func() *Event {
			ev := base(1)
			ev.Capacity = 6
			return ev
		}(), at + 1, true},
		{"uncapped, nothing to rename", &Event{Name: "Open", AttendingCount: 9,
			NativeTitleWritten: "Open", ForumTitleWritten: "Open", TitleWrittenAt: at}, at + 9999, false},
	} {
		if got := titleRenameDue(c.ev, nativeEventName(c.ev), forumPostTitle(c.ev), c.now); got != c.want {
			t.Errorf("%s: due=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestFillingRenamesAtOnceAndAPlaceOpeningDoesNot is the budget end to end:
// three renames inside ten minutes is a 429, so the fill takes the one slot a
// count rename has left and the un-fill waits.
func TestFillingRenamesAtOnceAndAPlaceOpeningDoesNot(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 2,
		StartsAt:                time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-5",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	names := func() (out []any) {
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-5" {
				out = append(out, c.Body["name"])
			}
		}
		return
	}
	store.Join(ev.ID, "alice", "Alice", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil)
	store.Join(ev.ID, "bob", "Bob", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil) // full: at once
	if got := names(); len(got) != 2 || got[0] != "Games [1/2]" || got[1] != "[Full] Games" {
		t.Fatalf("names = %v; want Games [1/2] then [Full] Games", got)
	}
	store.Leave(ev.ID, "bob", "")
	srv.syncAfterChange(ev.ID, nil) // a place opened: the description goes, the name waits
	got := names()
	if len(got) != 3 {
		t.Fatalf("%d native writes after the leave, want 3 — the description must still go", len(got))
	}
	if got[2] != nil {
		t.Errorf("a place opening renamed the title to %v inside the window", got[2])
	}
}

// TestASecondSignupWithinTheWindowSendsNoName is the throttle end to end: the
// description goes every time, the name does not.
func TestASecondSignupWithinTheWindowSendsNoName(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 4,
		StartsAt:                time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-5",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	patches := func() (names []any, count int) {
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-5" {
				count++
				names = append(names, c.Body["name"])
			}
		}
		return
	}

	store.Join(ev.ID, "alice", "Alice", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil) // first ever: the name goes
	names, n := patches()
	if n != 1 || names[0] != "Games [1/4]" {
		t.Fatalf("first publish: %d patches, names %v; want one carrying Games [1/4]", n, names)
	}

	store.Join(ev.ID, "bob", "Bob", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil) // seconds later: description yes, name no
	names, n = patches()
	if n != 2 {
		t.Fatalf("second publish: %d patches, want 2 — the description must still go", n)
	}
	if names[1] != nil {
		t.Errorf("second publish sent name %v inside the five-minute window", names[1])
	}

	store.Join(ev.ID, "carol", "Carol", JoinedViaButton)
	store.Join(ev.ID, "dan", "Dan", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil) // full: the name goes at once
	names, n = patches()
	if n != 3 || names[2] != "[Full] Games" {
		t.Errorf("full publish: %d patches, names %v; want the third carrying [Full] Games", n, names)
	}
}

// TestASyncDoesNotReopenClosedSignups. Closed is ours; Discord's event stays
// "scheduled", and every native update — including the one our own publish
// triggers — used to map that back to open and silently undo the close.
func TestASyncDoesNotReopenClosedSignups(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	closed := StatusClosed
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Status: StatusOpen, StartsAt: time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-5", Origin: OriginLocal})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &closed}); err != nil {
		t.Fatalf("close: %v", err)
	}
	remote := DiscordScheduledEvent{ID: "native-5", GuildID: "g1", Name: "Games",
		ScheduledStartTime: time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339), Status: discordEventScheduled}
	if _, _, err := srv.syncOneScheduledEvent(remote, "board"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusClosed {
		t.Errorf("after a sync from a scheduled native event, status = %q; the close was undone", got.Status)
	}
	// Discord saying it ran still wins.
	remote.Status = discordEventCompleted
	if _, _, err := srv.syncOneScheduledEvent(remote, "board"); err != nil {
		t.Fatalf("sync completed: %v", err)
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusCompleted {
		t.Errorf("after Discord says completed, status = %q, want completed", got.Status)
	}
}

// TestABlockAppendedToAnEmptyDescriptionIsStrippedOnTheWayBack: Discord trims
// leading whitespace, so the block comes home without its "\n\n" and used to be
// stored as the organiser's text, then appended to again on every publish.
func TestABlockAppendedToAnEmptyDescriptionIsStrippedOnTheWayBack(t *testing.T) {
	ev := &Event{ID: 3, GuildID: "g1", Name: "Games", Capacity: 2, AttendingCount: 1}
	written := nativeEventDescription(ev, []Signup{{DiscordUserID: "u1", DisplayName: "Slava", State: StateAttending}}, "board")
	asDiscordReturnsIt := strings.TrimLeft(written, "\n")
	if got := stripSignupPointer(asDiscordReturnsIt); got != "" {
		t.Errorf("an empty description came back as %q", got)
	}
	if got := stripSignupPointer("Bring dice." + written); got != "Bring dice." {
		t.Errorf("a real description came back as %q", got)
	}
}
