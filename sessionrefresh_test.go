package discordsignup

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// visit asks the server for the session behind token, as a page load would.
func visit(t *testing.T, srv *Server, token string) (*WebSession, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	return srv.sessionFrom(rec, req), rec
}

func ageSession(t *testing.T, store *Store, token string, checkedAgo, expiresIn time.Duration) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE web_sessions SET permissions_checked_at = ?, expires_at = ? WHERE token = ?`,
		now()-int64(checkedAgo.Seconds()), now()+int64(expiresIn.Seconds()), token); err != nil {
		t.Fatal(err)
	}
}

func TestOldPermissionsAreReadAgainByTheBot(t *testing.T) {
	store := testStore(t)
	fake := newFakeDiscord(t)
	srv := NewServer(store, nil, fake.client())
	for _, g := range []string{"g-member", "g-owner", "g-left"} {
		if err := store.RecordBotGuild(g, g, ""); err != nil {
			t.Fatal(err)
		}
	}
	member := func(roles string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"user":{"id":"u1"},"roles":%s}`, roles)
		}
	}
	fake.on("GET", "/guilds/g-member/members/u1", member(`["r-mod"]`))
	fake.on("GET", "/guilds/g-owner/members/u1", member(`[]`))
	fake.on("GET", "/guilds/g-left/members/u1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":10007,"message":"Unknown Member"}`)
	})
	fake.on("GET", "/guilds/g-member", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"owner_id":"someone-else","roles":[
			{"id":"g-member","permissions":"1024"},
			{"id":"r-mod","permissions":"%d"},
			{"id":"r-other","permissions":"8"}]}`, uint64(permissionManageEvents))
	})
	fake.on("GET", "/guilds/g-owner", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"owner_id":"u1","roles":[{"id":"g-owner","permissions":"0"}]}`)
	})

	// At login Discord said: admin in g-left, plain member of g-member, and a
	// server the bot is not in at all.
	session, err := store.CreateWebSession("u1", "Ann", "", map[string]uint64{
		"g-member": 0, "g-left": permissionAdministrator, "g-no-bot": permissionAdministrator})
	if err != nil {
		t.Fatal(err)
	}
	ageSession(t, store, session.Token, 13*time.Hour, 20*24*time.Hour)

	got, _ := visit(t, srv, session.Token)
	if got == nil {
		t.Fatal("a session with old permissions logged the person out")
	}
	want := map[string]uint64{"g-member": 1024 | permissionManageEvents, "g-owner": permissionsAllBits}
	if fmt.Sprint(got.GuildPermissions) != fmt.Sprint(want) {
		t.Fatalf("permissions = %v, want %v", got.GuildPermissions, want)
	}
	stored, err := store.WebSessionByToken(session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stored.GuildPermissions) != fmt.Sprint(want) {
		t.Fatalf("stored permissions = %v, want %v", stored.GuildPermissions, want)
	}
	if now()-stored.PermissionsCheckedAt > 5 {
		t.Fatalf("permissions_checked_at not moved: %d", stored.PermissionsCheckedAt)
	}

	// Fresh now, so the next visit asks Discord nothing.
	calls := len(fake.recorded())
	visit(t, srv, session.Token)
	if len(fake.recorded()) != calls {
		t.Fatalf("a visit with fresh permissions called Discord %d more times", len(fake.recorded())-calls)
	}
}

func TestDiscordFailingKeepsTheOldPermissionsAndTheLogin(t *testing.T) {
	store := testStore(t)
	fake := newFakeDiscord(t)
	srv := NewServer(store, nil, fake.client())
	if err := store.RecordBotGuild("g1", "g1", ""); err != nil {
		t.Fatal(err)
	}
	fake.on("GET", "/guilds/g1/members/u1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"code":0,"message":"down"}`)
	})
	session, err := store.CreateWebSession("u1", "Ann", "", map[string]uint64{"g1": permissionManageEvents})
	if err != nil {
		t.Fatal(err)
	}
	ageSession(t, store, session.Token, 13*time.Hour, 20*24*time.Hour)
	before, _ := store.WebSessionByToken(session.Token)

	got, _ := visit(t, srv, session.Token)
	if got == nil {
		t.Fatal("Discord failing logged the person out")
	}
	if got.GuildPermissions["g1"] != permissionManageEvents {
		t.Fatalf("permissions changed on a failed read: %v", got.GuildPermissions)
	}
	after, _ := store.WebSessionByToken(session.Token)
	if after.PermissionsCheckedAt != before.PermissionsCheckedAt {
		t.Fatal("a failed read marked the permissions as checked, so nothing would try again for 12 hours")
	}
}

func TestAVisitMovesTheEndOfTheLoginForward(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	session, err := store.CreateWebSession("u1", "Ann", "", map[string]uint64{"g1": 0})
	if err != nil {
		t.Fatal(err)
	}
	// Permissions fresh, so this test is only about the end of the login.
	ageSession(t, store, session.Token, 0, 2*time.Hour)

	got, rec := visit(t, srv, session.Token)
	if got == nil {
		t.Fatal("a live session was refused")
	}
	wantEnd := now() + int64(sessionIdleLifetime.Seconds())
	if wantEnd-got.ExpiresAt > 5 {
		t.Fatalf("expires_at = %d, want about %d", got.ExpiresAt, wantEnd)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != session.Token {
		t.Fatal("the browser was not sent the cookie with its new end, so it would drop it at the old one")
	}
	if wantEnd-cookie.Expires.Unix() > 5 {
		t.Fatalf("cookie expires %s, want about %s", cookie.Expires, time.Unix(wantEnd, 0))
	}

	// A second visit straight after has nothing worth writing.
	_, rec = visit(t, srv, session.Token)
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("every visit rewrote the cookie")
	}
}

func TestAnUnvisitedLoginStillEnds(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	session, err := store.CreateWebSession("u1", "Ann", "", map[string]uint64{"g1": 0})
	if err != nil {
		t.Fatal(err)
	}
	ageSession(t, store, session.Token, 0, -time.Second)
	if got, _ := visit(t, srv, session.Token); got != nil {
		t.Fatal("a login past its end still worked")
	}
}

