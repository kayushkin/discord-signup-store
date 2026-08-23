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
		GuildID: "g1", ChannelID: "c1", Name: "Roles", Capacity: 1,
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

	openPayload := RenderSignupMessage(open, nil)
	components, ok := openPayload["components"].([]any)
	if !ok || len(components) == 0 {
		t.Fatal("an open event has no buttons")
	}

	closedPayload := RenderSignupMessage(closed, nil)
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
	content, _ := RenderSignupMessage(ev, nil)["content"].(string)
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
	content, _ := RenderSignupMessage(ev, roster)["content"].(string)
	if len(content) > discordMessageContentLimit {
		t.Errorf("content is %d bytes, over Discord's %d limit — this would be a 400",
			len(content), discordMessageContentLimit)
	}
	if !strings.Contains(content, "trimmed") {
		t.Error("the message was trimmed without saying so")
	}
}

// TestOnlyImportedEventsGetACardPostedAutomatically pins the rule that decides
// what appears in the channel without anyone asking.
//
// An event created in Discord is already public the moment it exists, so
// mirroring it surprises nobody. One created on the web page has been announced
// nowhere, and posting it the instant it is saved would take the decision away
// from whoever wrote it.
func TestOnlyImportedEventsGetACardPostedAutomatically(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board-channel")

	mk := func(name, origin, status, messageID string) *Event {
		ev, err := store.CreateEvent(Event{
			GuildID: "g1", ChannelID: "board-channel", Name: name,
			Origin: origin, Status: status, MessageID: messageID,
			DiscordScheduledEventID: "d-" + name,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return ev
	}
	shouldPost := mk("imported-open", OriginDiscord, StatusOpen, "")
	mk("imported-already-posted", OriginDiscord, StatusOpen, "existing-msg")
	mk("imported-finished", OriginDiscord, StatusCompleted, "")
	mk("imported-cancelled", OriginDiscord, StatusCancelled, "")
	mk("made-here", OriginLocal, StatusOpen, "")

	posted, problems := srv.postMissingCards("g1")
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if posted != 1 {
		t.Fatalf("posted %d cards, want exactly 1", posted)
	}

	var boardPosts, threadCreates int
	for _, c := range fake.recorded() {
		switch {
		case c.Method == http.MethodPost && c.Path == "/channels/board-channel/messages":
			boardPosts++
		case c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/threads"):
			threadCreates++
		}
	}
	if boardPosts != 1 {
		t.Errorf("%d posts to the board, want 1", boardPosts)
	}
	// The card gets a discussion thread the moment it exists.
	if threadCreates != 1 {
		t.Errorf("%d threads created, want 1", threadCreates)
	}

	got, err := store.GetEvent(shouldPost.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.MessageID == "" {
		t.Error("the posted card's message id was not recorded, so the next sync would post it again")
	}
}

// TestPostingACardIsRetriedOnTheNextSync covers Discord being unavailable at
// the moment an event is imported. The empty message id is what makes the
// retry automatic — there is no failed-post flag to get stuck.
func TestPostingACardIsRetriedOnTheNextSync(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board-channel")

	if _, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board-channel", Name: "flaky",
		Origin: OriginDiscord, DiscordScheduledEventID: "d-flaky",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var attempts int
	fake.on(http.MethodPost, "/channels/board-channel/messages", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"code":0,"message":"upstream is having a moment"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"msg-later"}`)
	})

	posted, problems := srv.postMissingCards("g1")
	if posted != 0 || len(problems) != 1 {
		t.Fatalf("first pass: posted=%d problems=%v, want 0 and one problem", posted, problems)
	}

	posted, problems = srv.postMissingCards("g1")
	if posted != 1 || len(problems) != 0 {
		t.Fatalf("second pass: posted=%d problems=%v, want 1 and none", posted, problems)
	}
}

// TestCreatingFromDiscordAlsoMakesANativeEvent covers the point of publishing:
// the event shows up in the server's own event list, not only on the board.
func TestCreatingFromDiscordAlsoMakesANativeEvent(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
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
	if _, err := srv.PublishToDiscord(ev.ID, "board"); err != nil {
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
	// The description has to say that Interested does not hold a place, because
	// the native event's own button cannot be capped or cleared.
	if desc, _ := payload["description"].(string); !strings.Contains(desc, "does not hold you a place") {
		t.Errorf("description = %q, want the warning about Interested", desc)
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
	srv.EnableWeb(nil, "board")

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

	result, err := srv.SyncScheduledEvents("g1", "board")
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
	srv.EnableWeb(nil, "board")

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
	if err := srv.PushEditToDiscord(ev); err != nil {
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
	// The badge is part of what gets pushed now, and the stored name stays
	// clean — that separation is what stops the title compounding.
	if patched["name"].(string) != "[0/4] After" {
		t.Errorf("name = %v, want the badge plus the name", patched["name"])
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

	if err := srv.PushEditToDiscord(&Event{ID: 1, GuildID: "g1", Name: "Local only"}); err != nil {
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

// TestTheSignupPointerDoesNotRoundTrip covers an accumulating corruption.
//
// This service appends a line to a native event's description saying where the
// real roster is. The sync then reads that description back as the event's own.
// Without stripping it, the next publish appends the pointer to a description
// that already ends in one, and it grows by a paragraph on every edit until
// Discord refuses the event for length.
func TestTheSignupPointerDoesNotRoundTrip(t *testing.T) {
	ev := &Event{ID: 1, Name: "Games", Capacity: 8, AttendingCount: 2}
	written := "Bring dice."

	// One trip out and back.
	published := written + signupPointer(ev, "board-channel")
	got := stripSignupPointer(published)
	if got != written {
		t.Fatalf("after one round trip the description is %q, want %q", got, written)
	}

	// Ten more. The failure mode is growth, so the test has to iterate.
	current := written
	for i := 0; i < 10; i++ {
		current = stripSignupPointer(current + signupPointer(ev, "board-channel"))
	}
	if current != written {
		t.Errorf("after eleven round trips the description is %q (%d chars), want %q",
			current, len(current), written)
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

// TestTheCapacityBadgeDoesNotRoundTrip is the same failure the description
// pointer had, in a field capped at 100 characters — so it breaks sooner.
//
// We push "[3/8] Games" as the native event's name; the importer reads that
// name back as the event's own; the next push prefixes it again. Without
// stripping, the title grows a badge per signup until Discord refuses it.
func TestTheCapacityBadgeDoesNotRoundTrip(t *testing.T) {
	ev := &Event{Name: "Board game night", Capacity: 8, AttendingCount: 3}

	if got := nativeEventName(ev); got != "[3/8] Board game night" {
		t.Fatalf("nativeEventName = %q", got)
	}
	// Twenty signups, each pushing a new badge and each read back by a sync.
	current := ev.Name
	for i := 0; i < 20; i++ {
		ev.AttendingCount = i % 9
		pushed := capacityPrefix(ev) + current
		current = stripCapacityPrefix(pushed)
	}
	if current != "Board game night" {
		t.Errorf("after twenty round trips the name is %q (%d chars), want %q",
			current, len(current), "Board game night")
	}
}

func TestCapacityBadgeOnlyAppearsWhenThereIsALimit(t *testing.T) {
	uncapped := &Event{Name: "Open house", Capacity: 0, AttendingCount: 12}
	if got := nativeEventName(uncapped); got != "Open house" {
		t.Errorf("nativeEventName = %q, want no badge on an uncapped event", got)
	}
	full := &Event{Name: "Full one", Capacity: 4, AttendingCount: 4}
	if got := nativeEventName(full); got != "[4/4] Full one" {
		t.Errorf("nativeEventName = %q", got)
	}
}

// TestTheBadgeSurvivesALongName covers Discord's 100-character cap. The name is
// trimmed, never the badge — a title cut to "[3/8] Board game ni" still says
// what the badge is for, one cut the other way does not.
func TestTheBadgeSurvivesALongName(t *testing.T) {
	ev := &Event{Name: strings.Repeat("long name ", 20), Capacity: 8, AttendingCount: 3}
	got := nativeEventName(ev)
	if len([]rune(got)) > discordEventNameLimit {
		t.Errorf("name is %d runes, over Discord's %d", len([]rune(got)), discordEventNameLimit)
	}
	if !strings.HasPrefix(got, "[3/8] ") {
		t.Errorf("name = %q, want the badge kept at the front", got)
	}
}

// TestStripCapacityPrefixLeavesRealNamesAlone keeps the pattern from eating
// something a person actually typed.
func TestStripCapacityPrefixLeavesRealNamesAlone(t *testing.T) {
	for _, name := range []string{
		"Board game night", "3/8 people", "[draft] Planning", "[3 of 8] Games",
		"Games [3/8]", "[a/b] Games", "",
	} {
		if got := stripCapacityPrefix(name); got != name {
			t.Errorf("stripCapacityPrefix(%q) = %q, want it untouched", name, got)
		}
	}
	// And it does remove the real thing, including odd spacing.
	for _, badged := range []string{"[3/8] Games", "[12/100]  Games", "[0/1]\tGames"} {
		if got := stripCapacityPrefix(badged); got != "Games" {
			t.Errorf("stripCapacityPrefix(%q) = %q, want %q", badged, got, "Games")
		}
	}
}

// TestTheTitleIsPushedOnEveryRosterChange covers the reason this needs pushing
// at all: the count in the title is stale the moment somebody joins.
func TestTheTitleIsPushedOnEveryRosterChange(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")

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
	srv.syncAfterChange(after, nil)

	var pushed string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-5" {
			pushed, _ = c.Body["name"].(string)
		}
	}
	if pushed != "[1/3] Games" {
		t.Errorf("pushed name = %q, want the badge to reflect the new count", pushed)
	}
}

// TestThreadIsCreatedOnceAndArchivedWhenTheEventEnds pins the lifecycle: one
// thread per event however many times the card refreshes, closed by the sweep.
func TestThreadIsCreatedOnceAndArchivedWhenTheEventEnds(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")

	past := time.Now().Add(-10 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Talky",
		StartsAt: past + 20*3600, EndsAt: past + 21*3600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.PostSignupMessage(ev.ID); err != nil {
		t.Fatalf("post card: %v", err)
	}
	// Three more refreshes must not open three more threads.
	for i := 0; i < 3; i++ {
		if err := srv.RefreshSignupMessage(ev.ID); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	var creates int
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/threads") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("%d thread creates across four card writes, want 1", creates)
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.ThreadID == "" {
		t.Fatal("the thread id was not recorded")
	}

	// Push the event into the past and sweep: the thread archives.
	newEnd := past
	newStart := past - 3600
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &newStart, EndsAt: &newEnd}); err != nil {
		t.Fatalf("re-date: %v", err)
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var archived bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/channels/"+got.ThreadID {
			if v, ok := c.Body["archived"].(bool); ok && v {
				archived = true
			}
		}
	}
	if !archived {
		t.Error("the thread was not archived when the event finished")
	}
}

// TestReconcilePublishesEventsThatMissedIt heals the first desync source: only
// modal-created events auto-published, so web- and API-created ones never
// reached Discord's list.
func TestReconcilePublishesEventsThatMissedIt(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
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

	published, cancelled, problems := srv.reconcileWithNative("g1", nil)
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
	srv.EnableWeb(nil, "board")

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

	published, cancelled, problems := srv.reconcileWithNative("g1", nil)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cancelled != 1 || published != 0 {
		t.Errorf("cancelled=%d published=%d, want 1 and 0", cancelled, published)
	}
	if got, _ := store.GetEvent(deleted.ID); got.Status != StatusCancelled {
		t.Errorf("deleted-native event = %q, want cancelled", got.Status)
	}
	if got, _ := store.GetEvent(completed.ID); got.Status != StatusOpen {
		t.Errorf("completed-native event = %q, want left for the time sweep", got.Status)
	}
}

// TestReconcileDeletesTheNativeEventWhenCancelledLocally: cancelling on any
// surface cancels everywhere, including Discord's own list.
func TestReconcileDeletesTheNativeEventWhenCancelledLocally(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")

	future := time.Now().Add(72 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Called off",
		StartsAt: future, DiscordScheduledEventID: "native-5", Status: StatusCancelled})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	remote := []DiscordScheduledEvent{{ID: "native-5", GuildID: "g1", Status: discordEventScheduled}}
	if _, _, problems := srv.reconcileWithNative("g1", remote); len(problems) != 0 {
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
