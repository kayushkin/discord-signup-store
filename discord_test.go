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

	var createdIn []string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/messages") {
			createdIn = append(createdIn, c.Path)
		}
	}
	if len(createdIn) != 1 || createdIn[0] != "/channels/board-channel/messages" {
		t.Errorf("message posts = %v, want one to the board channel", createdIn)
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
