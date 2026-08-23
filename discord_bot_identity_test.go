package discordsignup

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// countUsersMeCalls reports how many times GET /users/@me reached the fake.
func countUsersMeCalls(f *fakeDiscord) int {
	n := 0
	for _, c := range f.recorded() {
		if c.Method == http.MethodGet && c.Path == "/users/@me" {
			n++
		}
	}
	return n
}

// usersMeFailingTimes installs a /users/@me handler that answers 500 for the
// first failures calls and hands back id thereafter. A transient failure is the
// realistic case: the token is resolved lazily, so the very first lookup after a
// boot races auth-store coming up.
func usersMeFailingTimes(f *fakeDiscord, failures int, id string) {
	var mu sync.Mutex
	seen := 0
	f.on(http.MethodGet, "/users/@me", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		fail := seen <= failures
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"internal error"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"id":%q}`, id)
	})
}

// TestBotUserIDIsLookedUpAgainAfterAFailure pins the property sync.Once removes.
//
// sync.Once runs its body once whether or not the body succeeded, so a single
// transient /users/@me failure — a 500, a timeout, a token not yet resolved at
// boot — left the bot's own id empty for the life of the process and nothing
// ever looked again. What the id is used for makes that expensive: three of its
// four call sites compare it against somebody else's id to recognise the bot's
// own work, and "" matches nobody, so each of those guards quietly fails open.
//
// The property is that a failure is not an answer. Only a successful lookup may
// be cached.
func TestBotUserIDIsLookedUpAgainAfterAFailure(t *testing.T) {
	fake := newFakeDiscord(t)
	usersMeFailingTimes(fake, 1, "bot-1")
	srv := NewServer(testStore(t), nil, fake.client())

	if got := srv.applicationUserID(); got != "" {
		t.Fatalf("first call = %q, want %q — the lookup was set up to fail", got, "")
	}
	if got := srv.applicationUserID(); got != "bot-1" {
		t.Fatalf("second call = %q, want %q — a failed lookup must not be cached as the answer", got, "bot-1")
	}
	if n := countUsersMeCalls(fake); n != 2 {
		t.Errorf("GET /users/@me happened %d time(s), want 2 — one that failed and one that retried", n)
	}
}

// TestBotUserIDIsLookedUpOnlyOnceOnceItIsKnown pins the half of the old
// behaviour that was right. Retrying after a failure must not turn every call
// into a round trip: the id is immutable for the life of the bot, and the
// gateway asks for it on every single reaction.
func TestBotUserIDIsLookedUpOnlyOnceOnceItIsKnown(t *testing.T) {
	fake := newFakeDiscord(t)
	usersMeFailingTimes(fake, 0, "bot-1")
	srv := NewServer(testStore(t), nil, fake.client())

	for i := 0; i < 5; i++ {
		if got := srv.applicationUserID(); got != "bot-1" {
			t.Fatalf("call %d = %q, want %q", i+1, got, "bot-1")
		}
	}
	if n := countUsersMeCalls(fake); n != 1 {
		t.Errorf("GET /users/@me happened %d time(s), want 1 — a known id must be cached", n)
	}
}

// TestConcurrentBotUserIDLookupsCollapseToOneRequest pins that the retry does
// not cost a request per caller. The gateway calls this from every reaction
// handler, and those arrive concurrently.
func TestConcurrentBotUserIDLookupsCollapseToOneRequest(t *testing.T) {
	fake := newFakeDiscord(t)
	usersMeFailingTimes(fake, 0, "bot-1")
	srv := NewServer(testStore(t), nil, fake.client())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := srv.applicationUserID(); got != "bot-1" {
				t.Errorf("concurrent call = %q, want %q", got, "bot-1")
			}
		}()
	}
	wg.Wait()

	if n := countUsersMeCalls(fake); n != 1 {
		t.Errorf("GET /users/@me happened %d time(s), want 1 — concurrent callers must share one lookup", n)
	}
}

// TestBotUserIDWithoutADiscordClientAsksNobody pins the nil-client case. The web
// routes are enabled separately from the Discord client, so a server with no
// client must answer without panicking and without inventing an id.
func TestBotUserIDWithoutADiscordClientAsksNobody(t *testing.T) {
	srv := NewServer(testStore(t), nil, nil)
	if got := srv.applicationUserID(); got != "" {
		t.Errorf("applicationUserID() = %q, want %q with no Discord client", got, "")
	}
}
