package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEventRefRoundTrips(t *testing.T) {
	if got := EventRef(126); got != "discord_event_000126" {
		t.Fatalf("EventRef(126) = %q", got)
	}
	if got := EventRef(1234567); got != "discord_event_1234567" {
		t.Fatalf("EventRef(1234567) = %q", got)
	}
	for _, bad := range []string{"126", "discord_event_126", "discord_event_00012x", "event_000126"} {
		if _, err := ParseEventRef(bad); err == nil {
			t.Errorf("ParseEventRef(%q) took it", bad)
		}
	}
	if id, err := ParseEventRef("discord_event_000126"); err != nil || id != 126 {
		t.Errorf("ParseEventRef = %d, %v", id, err)
	}
}

func TestEventIsReadByItsRef(t *testing.T) {
	srv, _, store := testServer(t)
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	start := time.Now().Add(6 * time.Hour).Unix()
	created, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Bowling", StartsAt: start})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := EventRef(created.ID)
	if created.Ref != want {
		t.Errorf("created ref = %q, want %q", created.Ref, want)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/discord-events/"+want, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got Event
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != created.ID || got.Ref != want || got.Name != "Bowling" {
		t.Errorf("got id %d ref %q name %q", got.ID, got.Ref, got.Name)
	}

	for path, code := range map[string]int{
		"/api/discord-events/discord_event_999999": http.StatusNotFound,
		"/api/discord-events/12":                   http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != code {
			t.Errorf("%s = %d, want %d", path, rec.Code, code)
		}
	}
}
