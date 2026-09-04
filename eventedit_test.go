package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An event is edited from three surfaces — the Discord modal, this web page
// and the machine API — and every one of them changes something the native
// scheduled event's title shows. These tests drive the surfaces and assert on
// what reached Discord, because that title is the one copy of the count built
// from an *Event struct rather than re-read from the database, so it is the
// copy a stale snapshot corrupts and a missing push leaves behind.

// webTestServer wires a server with a fake Discord, a logged-in manager of g1,
// and the real mux, so these drive the same routes a browser does.
func webTestServer(t *testing.T) (*Server, *Store, *fakeDiscord, http.Handler, string) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})

	session, err := store.CreateWebSession("manager", "Manager", "",
		map[string]uint64{"g1": permissionManageEvents})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	return srv, store, fake, mux, session.Token
}

// postForm drives one web route as the logged-in manager.
func postForm(t *testing.T, mux http.Handler, token, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// nativeNamesPushed collects the names sent to one native scheduled event.
//
// Polled rather than read once: every surface is written after the reply, in a
// goroutine, so that nobody waits on Discord's API to be told their click
// worked. An empty result after the deadline means nothing was pushed at all,
// which is its own failure and reads as one.
func nativeNamesPushed(t *testing.T, fake *fakeDiscord, nativeEventID string) []string {
	t.Helper()
	path := "/guilds/g1/scheduled-events/" + nativeEventID
	deadline := time.Now().Add(2 * time.Second)
	for {
		var names []string
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == path {
				name, _ := c.Body["name"].(string)
				names = append(names, name)
			}
		}
		if len(names) > 0 || time.Now().After(deadline) {
			return names
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nativeDescriptionsPushed collects the descriptions sent to one native
// scheduled event, in order. The description carries the live count and the
// names; the title deliberately does not (a rename is rate-limited, an edit is
// not), so this is where a stale-snapshot bug shows.
func nativeDescriptionsPushed(t *testing.T, fake *fakeDiscord, nativeEventID string) []string {
	t.Helper()
	path := "/guilds/g1/scheduled-events/" + nativeEventID
	deadline := time.Now().Add(2 * time.Second)
	for {
		var out []string
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPatch && c.Path == path {
				d, _ := c.Body["description"].(string)
				out = append(out, d)
			}
		}
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// publishedEvent is an event that already exists on Discord, which is the only
// kind with a title to go stale.
func publishedEvent(t *testing.T, store *Store, capacity int, joiners ...string) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: capacity,
		Status: StatusOpen, Timezone: "UTC",
		StartsAt:                time.Now().Add(48 * time.Hour).Unix(),
		DiscordScheduledEventID: "native-9",
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	for _, who := range joiners {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
	}
	return ev
}

func editForm(ev *Event, capacity string) url.Values {
	return url.Values{
		"name":        {ev.Name},
		"description": {ev.Description},
		"capacity":    {capacity},
		"status":      {StatusOpen},
		"starts_at":   {time.Unix(ev.StartsAt, 0).UTC().Format("2006-01-02T15:04")},
		"ends_at":     {""},
		"location":    {ev.Location},
		"timezone":    {"UTC"},
	}
}

// TestSavingTheEditFormPushesTheNativeTitle is the bug this file was written
// for. The form is where a capacity is raised, and raising it changes the
// number in the native event's title — but the handler saved the row, redrew
// the card, and never told Discord, so its title kept saying 3/8 while every
// other surface said 3/10.
func TestSavingTheEditFormPushesTheNativeTitle(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 8, "alice", "bob", "carol")

	postForm(t, mux, token, eventPath(ev), editForm(ev, "10"))

	names := nativeNamesPushed(t, fake, "native-9")
	if len(names) == 0 {
		t.Fatal("the edit pushed nothing to the native event; its title still shows the old limit")
	}
	if got := names[len(names)-1]; got != "Games [3/10]" {
		t.Errorf("pushed name = %q, want %q", got, "Games [3/10]")
	}
}

// TestRenamingOnTheWebPageReachesDiscord covers the same gap for a field that
// has nothing to do with capacity: the badge is only half the title.
func TestRenamingOnTheWebPageReachesDiscord(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 8, "alice")

	form := editForm(ev, "8")
	form.Set("name", "Board game night")
	postForm(t, mux, token, eventPath(ev), form)

	names := nativeNamesPushed(t, fake, "native-9")
	if len(names) == 0 {
		t.Fatal("the rename pushed nothing to the native event")
	}
	if got := names[len(names)-1]; got != "Board game night [1/8]" {
		t.Errorf("pushed name = %q, want %q", got, "Board game night [1/8]")
	}
}

// TestAddingSomeoneFromTheWebPagePushesTheNewCount covers the stale snapshot.
// The handler loads the event to check permissions, THEN adds the person, so
// the struct it held was one signup out of date — and the badge built from it
// published the count from before the add.
func TestAddingSomeoneFromTheWebPagePushesTheNewCount(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 3, "alice")

	postForm(t, mux, token, eventPath(ev)+"/roster/add", url.Values{"discord_user_id": {"bob"}})

	descs := nativeDescriptionsPushed(t, fake, "native-9")
	if len(descs) == 0 {
		t.Fatal("adding someone pushed nothing to the native event")
	}
	got := descs[len(descs)-1]
	if !strings.Contains(got, "2 of 3 places taken") || !strings.Contains(got, "Going: alice, bob") {
		t.Errorf("pushed description = %q, want 2 of 3 with both names — the count is one behind", got)
	}
}

// TestRemovingSomeoneFromTheWebPagePushesTheNewCount is the same fault in the
// other direction, and the worse one: a badge one too high says a place is
// taken that is free.
func TestRemovingSomeoneFromTheWebPagePushesTheNewCount(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 3, "alice", "bob")

	postForm(t, mux, token, eventPath(ev)+"/roster/remove", url.Values{"discord_user_id": {"alice"}})

	descs := nativeDescriptionsPushed(t, fake, "native-9")
	if len(descs) == 0 {
		t.Fatal("removing someone pushed nothing to the native event")
	}
	got := descs[len(descs)-1]
	if !strings.Contains(got, "1 of 3 places taken") || strings.Contains(got, "alice") {
		t.Errorf("pushed description = %q, want 1 of 3 without alice — the count is one behind", got)
	}
}

// TestAnEditPushesTheNativeEventExactlyOnce guards the other half of routing
// every edit through one function: SetCapacity used to push the title itself
// AND call the sync that pushes it, so every capacity change sent Discord the
// same PATCH twice.
func TestAnEditPushesTheNativeEventExactlyOnce(t *testing.T) {
	srv, store, fake, _, _ := webTestServer(t)
	ev := publishedEvent(t, store, 3, "alice")

	if _, _, err := srv.SetCapacity(ev.ID, 6, "test"); err != nil {
		t.Fatalf("set capacity: %v", err)
	}
	if names := nativeNamesPushed(t, fake, "native-9"); len(names) == 0 {
		t.Fatal("the capacity change pushed nothing to the native event")
	}
	// Let a second push land if one is coming; everything here is in-process.
	time.Sleep(250 * time.Millisecond)
	if names := nativeNamesPushed(t, fake, "native-9"); len(names) != 1 {
		t.Errorf("pushed the title %d times (%v), want once", len(names), names)
	}
}

func eventPath(ev *Event) string {
	return "/events/" + strconv.FormatInt(ev.ID, 10)
}

// TestRaisingTheLimitThroughTheAPIPromotesTheWaitlist covers the third copy of
// the edit rule. The API handler saved the row and pushed the title, and never
// promoted anybody — so raising a limit here freed places that the people
// waiting for them were never moved into.
func TestRaisingTheLimitThroughTheAPIPromotesTheWaitlist(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	ev := publishedEvent(t, store, 1, "alice", "bob", "carol")

	req := httptest.NewRequest(http.MethodPatch, "/api/events/"+strconv.FormatInt(ev.ID, 10),
		strings.NewReader(`{"capacity":3}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}

	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.AttendingCount != 3 {
		t.Errorf("%d attending after the limit went to 3, want 3 — the waitlist was not promoted",
			after.AttendingCount)
	}
	// Three places, three attending: the cap was raised to exactly the
	// attendance, so the title flips to Full — the one change a title is for.
	if names := nativeNamesPushed(t, fake, "native-9"); len(names) == 0 ||
		names[len(names)-1] != "[Full] Games" {
		t.Errorf("pushed names = %v, want the last to be %q", names, "[Full] Games")
	}
}

// TestTheWebFormAcceptsATypedTime covers the surface that used to be a
// datetime-local picker, which could only ever emit "2026-09-29T17:30". The
// year is not asserted because it depends on when the suite runs — the roll
// forward is pinned in eventtime_test.go — but the day and the hour are the
// whole point.
func TestTheWebFormAcceptsATypedTime(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 8, "alice")

	form := editForm(ev, "8")
	form.Set("starts_at", "9/29 5pm")
	form.Set("timezone", "America/Los_Angeles")
	postForm(t, mux, token, eventPath(ev), form)

	after, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	got := time.Unix(after.StartsAt, 0).In(loc)
	if got.Month() != time.September || got.Day() != 29 || got.Hour() != 17 || got.Minute() != 0 {
		t.Errorf("%q was stored as %s, want 29 September at 17:00",
			"9/29 5pm", got.Format("2006-01-02 15:04 MST"))
	}
}

// TestARunningEventIsPushedWithoutItsTimes: Discord refuses scheduled_start_time
// on an event that has begun, whether or not it changed, and used to refuse
// the whole PATCH with it — so a roster change on a running event never
// reached the native description and the sweep retried it every minute.
func TestARunningEventIsPushedWithoutItsTimes(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil)
	store.SetGuildChannels("g1", GuildChannels{Board: "board"})
	started := time.Now().Add(-10 * time.Minute).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games", Capacity: 4,
		StartsAt: started, EndsAt: started + 2*3600, DiscordScheduledEventID: "native-9"})
	if err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "alice", "Alice", JoinedViaButton)
	srv.syncAfterChange(ev.ID, nil)
	var patched bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/guilds/g1/scheduled-events/native-9" {
			patched = true
			if _, has := c.Body["scheduled_start_time"]; has {
				t.Error("a running event's PATCH carried scheduled_start_time, which Discord refuses")
			}
			if _, has := c.Body["scheduled_end_time"]; has {
				t.Error("a running event's PATCH carried scheduled_end_time")
			}
			if desc, _ := c.Body["description"].(string); !strings.Contains(desc, "1 of 4 places taken") {
				t.Errorf("the count did not reach the description: %q", desc)
			}
		}
	}
	if !patched {
		t.Fatal("the native event was not written at all")
	}
}
