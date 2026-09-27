package discordsignup

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAnEventPageAsksDiscordAboutEachPersonOnce: an event's history used to
// look up every person in it on every load, one Discord call after another.
// The first load records each name, or that they left; the second asks nothing.
func TestAnEventPageAsksDiscordAboutEachPersonOnce(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1/members/493904201101869067", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nick":"Waleeha","user":{"id":"493904201101869067","username":"midnachew"}}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/110122051179687936", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":10007,"message":"Unknown Member"}`))
	})
	srv := NewServer(testStore(t), nil, fake.client())
	actors := []string{"web:493904201101869067", "110122051179687936", "user"}

	first := srv.historyActorNames("g1", actors)
	calls := len(fake.recorded())
	second := srv.historyActorNames("g1", actors)

	if calls != 2 {
		t.Errorf("the first load made %d Discord calls, want one per person (2)", calls)
	}
	if extra := len(fake.recorded()) - calls; extra != 0 {
		t.Errorf("the second load made %d Discord calls, want 0", extra)
	}
	for _, names := range []map[string]actorName{first, second} {
		if got := names["web:493904201101869067"]; got.DisplayName != "Waleeha" || got.Via != "web" {
			t.Errorf("names = %v", names)
		}
		if _, ok := names["110122051179687936"]; ok {
			t.Error("someone who left before their name was read was given one")
		}
	}
}

// TestTheSyncKeepsRecordedNamesCurrent: a new nickname, a departure and a
// return each show after the sync, and someone who left keeps their last name.
func TestTheSyncKeepsRecordedNamesCurrent(t *testing.T) {
	var renamed, left atomic.Bool
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1/members/u-renamed", func(w http.ResponseWriter, r *http.Request) {
		name := "Old"
		if renamed.Load() {
			name = "New"
		}
		w.Write([]byte(`{"nick":"` + name + `","user":{"id":"u-renamed"}}`))
	})
	fake.on(http.MethodGet, "/guilds/g1/members/u-leaves", func(w http.ResponseWriter, r *http.Request) {
		if left.Load() {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":10007,"message":"Unknown Member"}`))
			return
		}
		w.Write([]byte(`{"nick":"Leaver","user":{"id":"u-leaves"}}`))
	})
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	known := map[string]memberName{}
	srv.memberNameFor("g1", "u-renamed", known)
	srv.memberNameFor("g1", "u-leaves", known)

	renamed.Store(true)
	left.Store(true)
	if _, err := srv.RefreshDisplayNames("g1"); err != nil {
		t.Fatal(err)
	}
	names, err := store.MemberNames("g1")
	if err != nil {
		t.Fatal(err)
	}
	if got := names["u-renamed"]; got != (memberName{DisplayName: "New"}) {
		t.Errorf("after a new nickname: %+v, want New", got)
	}
	if got := names["u-leaves"]; got != (memberName{DisplayName: "Leaver", LeftGuild: true}) {
		t.Errorf("after leaving: %+v, want left with the last name kept", got)
	}

	left.Store(false)
	if _, err := srv.RefreshDisplayNames("g1"); err != nil {
		t.Fatal(err)
	}
	names, _ = store.MemberNames("g1")
	if got := names["u-leaves"]; got.LeftGuild {
		t.Errorf("after rejoining: %+v, still recorded as left", got)
	}
	for _, call := range fake.recorded() {
		if !strings.HasPrefix(call.Path, "/guilds/g1/members/") {
			t.Errorf("unexpected call %s %s", call.Method, call.Path)
		}
	}
}
