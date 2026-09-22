package discordsignup

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestAnOrganiserCanGiveAPlaceFromTheWaitlistOrMaybe, past the limit, and it
// is logged as theirs.
func TestAnOrganiserCanGiveAPlaceFromTheWaitlistOrMaybe(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 1, "u-going", "u-waiting")
	store.MarkMaybe(ev.ID, "u-maybe", "Maybe", JoinedViaButton)
	path := "/events/" + strconv.FormatInt(ev.ID, 10) + "/roster/promote"

	rec := postForm(t, mux, token, path, url.Values{"discord_user_id": {"u-waiting"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "over+the+limit") {
		t.Errorf("notice = %s, want it to say the event is over its limit", loc)
	}
	postForm(t, mux, token, path, url.Values{"discord_user_id": {"u-maybe"}})
	after, _ := store.GetEvent(ev.ID)
	if after.AttendingCount != 3 || after.WaitlistCount != 0 {
		t.Errorf("going %d waiting %d, want 3 and 0", after.AttendingCount, after.WaitlistCount)
	}
	history, _ := store.History(ev.ID, 100)
	byOrganiser := 0
	for _, h := range history {
		if h.Action == ActionPromoted && h.Actor == "web:manager" {
			byOrganiser++
		}
	}
	if byOrganiser != 2 {
		t.Errorf("want two promotions by web:manager in the history, got %d", byOrganiser)
	}
	// Someone already going is not promotable.
	rec = postForm(t, mux, token, path, url.Values{"discord_user_id": {"u-going"}})
	if !strings.Contains(rec.Header().Get("Location"), "not+on+the+waitlist") {
		t.Errorf("promoting someone going = %s", rec.Header().Get("Location"))
	}
}
