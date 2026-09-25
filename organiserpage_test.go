package discordsignup

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAFullEventWithNoWaitlistRefusesAJoin, and the people already waiting
// still move up when a place opens.
func TestAFullEventWithNoWaitlistRefusesAJoin(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 1, "alice", "bob")
	disabled := true
	if _, err := store.UpdateEvent(ev.ID, EventPatch{WaitlistDisabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ev.ID, "cy", "Cy", JoinedViaButton); !errors.Is(err, ErrEventFull) {
		t.Fatalf("join a full event with no waitlist = %v, want ErrEventFull", err)
	}
	result, err := store.Leave(ev.ID, "alice", ActorUser)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promoted == nil || result.Promoted.DiscordUserID != "bob" {
		t.Errorf("promoted = %+v, want bob, who was already waiting", result.Promoted)
	}
}

// TestAnOrganiserPutsSomeoneOnTheListTheyPick: going past the limit, the
// waitlist only when full, and Maybe from going gives the place away.
func TestAnOrganiserPutsSomeoneOnTheListTheyPick(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 1, "alice")

	if _, err := store.PlaceOnList(ev.ID, "bob", "Bob", StateAttending, "web:org"); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetEvent(ev.ID)
	if after.AttendingCount != 2 {
		t.Errorf("going = %d, want 2: an organiser can take it past the limit", after.AttendingCount)
	}
	if _, err := store.PlaceOnList(ev.ID, "cy", "Cy", StateWaitlisted, "web:org"); err != nil {
		t.Fatalf("waitlist on a full event: %v", err)
	}
	// Maybe from going frees nothing: two going on a limit of one.
	placed, err := store.PlaceOnList(ev.ID, "alice", "Alice", StateMaybe, "web:org")
	if err != nil {
		t.Fatal(err)
	}
	if placed.Promoted != nil {
		t.Errorf("promoted %s while the event was still at its limit", placed.Promoted.DiscordUserID)
	}
	placed, err = store.PlaceOnList(ev.ID, "bob", "Bob", StateMaybe, "web:org")
	if err != nil {
		t.Fatal(err)
	}
	if placed.Promoted == nil || placed.Promoted.DiscordUserID != "cy" {
		t.Errorf("promoted = %+v, want cy to take the freed place", placed.Promoted)
	}
	// Cy has the one place; moving them to Maybe leaves the event with room,
	// and nobody can be put in a line with a free place at its head.
	if _, err := store.PlaceOnList(ev.ID, "cy", "Cy", StateMaybe, "web:org"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PlaceOnList(ev.ID, "dee", "Dee", StateWaitlisted, "web:org"); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("waitlist on an event with room = %v, want ErrInvalidEvent", err)
	}
	history, _ := store.History(ev.ID, 0)
	last := history[len(history)-1]
	if last.DiscordUserID != "cy" || last.Action != ActionAdded || last.Actor != "web:org" || last.ToState != StateMaybe {
		t.Errorf("history row = %+v, want cy added → maybe by web:org", last)
	}
}

// TestTheEventPageIsTheEditForm: an organiser edits in place, the old edit
// link lands on the page, and the waitlist switch saves and is logged.
func TestTheEventPageIsTheEditForm(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 3, "alice")

	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{`name="name" required maxlength="100" value="Games"`, `name="waitlist"`,
		`action="` + eventPath(ev) + `/signups"`, `data-tone="ok" aria-checked="true"`, "Send invite", "Add them yourself", `data-edit`} {
		if !strings.Contains(page, want) {
			t.Errorf("event page lacks %s", want)
		}
	}
	if strings.Contains(page, `/edit"`) {
		t.Error("the event page still links a separate edit page")
	}
	if rec := getPage(t, mux, token, eventPath(ev)+"/edit"); rec.Code != http.StatusMovedPermanently ||
		rec.Header().Get("Location") != eventPath(ev) {
		t.Errorf("/edit = %d to %q, want a redirect to the event page", rec.Code, rec.Header().Get("Location"))
	}

	form := editForm(ev, "3")
	form.Del("status")
	form.Set("waitlist", "off")
	postForm(t, mux, token, eventPath(ev), form)
	after, _ := store.GetEvent(ev.ID)
	if !after.WaitlistDisabled || after.Status != StatusOpen {
		t.Errorf("after save: waitlist disabled %v, status %s", after.WaitlistDisabled, after.Status)
	}
	page = getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "changed the waitlist: on → off") {
		t.Error("the log does not show the waitlist being turned off")
	}

	postForm(t, mux, token, eventPath(ev)+"/signups", url.Values{})
	if after, _ := store.GetEvent(ev.ID); after.Status != StatusClosed {
		t.Errorf("status after Close signups = %s", after.Status)
	}
}

// TestAnInviteIsADMWithJoinAndIsLogged, and a bounced one is logged as not
// delivered rather than dropped.
func TestAnInviteIsADMWithJoinAndIsLogged(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	for _, id := range []string{"222", "333"} {
		fake.on(http.MethodGet, "/guilds/g1/members/"+id, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"nick":"Alfie","user":{"id":"` + id + `","username":"alfie"}}`))
		})
	}
	ev := publishedEvent(t, store, 4)

	rec := postForm(t, mux, token, eventPath(ev)+"/invite", url.Values{"discord_user_id": {"222"}})
	if !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("Invited Alfie")) {
		t.Errorf("notice = %s", rec.Header().Get("Location"))
	}
	var dm *recordedCall
	for _, call := range fake.recorded() {
		if call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/messages") &&
			strings.Contains(call.Body["content"].(string), "invited you to **Games**") {
			dm = &call
		}
	}
	if dm == nil {
		t.Fatal("no invite DM was sent")
	}
	row := dm.Body["components"].([]any)[0].(map[string]any)["components"].([]any)
	if row[0].(map[string]any)["custom_id"] != JoinCustomID(ev.ID) {
		t.Errorf("first button = %v, want the event's Join", row[0])
	}

	fake.on(http.MethodPost, "/users/@me/channels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Cannot send messages to this user","code":50007}`))
	})
	postForm(t, mux, token, eventPath(ev)+"/invite", url.Values{"discord_user_id": {"333"}})
	invites, _ := store.Invites(ev.ID)
	if len(invites) != 2 || invites[0].Delivery != InviteDeliverySent || invites[1].Delivery != InviteDeliveryDMsClosed {
		t.Fatalf("invites = %+v", invites)
	}
	store.Join(ev.ID, "222", "Alfie", JoinedViaButton)
	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{"not delivered · DMs closed", `<span class="pill open">going</span>`, "not delivered, their DMs are closed"} {
		if !strings.Contains(page, want) {
			t.Errorf("event page lacks %q", want)
		}
	}
}

// TestTheRosterShowsShortNamesWithTheDiscordNameBehindThem.
func TestTheRosterShowsShortNamesWithTheDiscordNameBehindThem(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 4)
	store.Join(ev.ID, "u-matt", "Lil' Fascist Matt 🌟", JoinedViaButton)
	store.SetReadableName("u-matt", "Matt")
	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	if !strings.Contains(page, `<summary title="On Discord: Lil&#39; Fascist Matt 🌟">Matt<svg class="caret"`) ||
		!strings.Contains(page, `<span class="aka-label">On Discord</span>Lil&#39; Fascist Matt 🌟</span>`) {
		t.Error("the roster does not show Matt with his Discord name behind it")
	}
}

// TestCancellingFromThePageNeedsTheName typed back.
func TestCancellingFromThePageNeedsTheName(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 4)
	postForm(t, mux, token, eventPath(ev)+"/cancel", url.Values{"confirm_name": {"Chess"}})
	if after, _ := store.GetEvent(ev.ID); after.Status != StatusOpen {
		t.Fatalf("a wrong name cancelled it: %s", after.Status)
	}
	postForm(t, mux, token, eventPath(ev)+"/cancel", url.Values{"confirm_name": {"games"}})
	if after, _ := store.GetEvent(ev.ID); after.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", after.Status)
	}
}

// TestAnOrganiserReordersTheWaitlist, the next promotion follows the new
// order, and someone joining later waits behind everyone moved.
func TestAnOrganiserReordersTheWaitlist(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 1, "alice", "w1", "w2", "w3")
	postForm(t, mux, token, eventPath(ev)+"/waitlist/move", url.Values{"discord_user_id": {"w3"}, "to": {"1"}})
	store.Join(ev.ID, "w4", "w4", JoinedViaButton)
	roster, _ := store.Roster(ev.ID, false)
	var line []string
	for _, sg := range roster {
		if sg.State == StateWaitlisted {
			line = append(line, fmt.Sprintf("%s#%d", sg.DiscordUserID, sg.WaitlistPlace))
		}
	}
	if got := strings.Join(line, " "); got != "w3#1 w1#2 w2#3 w4#4" {
		t.Errorf("waitlist = %s", got)
	}
	result, _ := store.Leave(ev.ID, "alice", ActorUser)
	if result.Promoted == nil || result.Promoted.DiscordUserID != "w3" {
		t.Errorf("promoted %+v, want w3, moved to the front", result.Promoted)
	}
}

// TestSavingOneFieldLeavesTheOthersAlone: the page sends one field.
func TestSavingOneFieldLeavesTheOthersAlone(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 3)
	store.UpdateEvent(ev.ID, EventPatch{Location: strPtr("Cafe"), Description: strPtr("Bring dice")})
	req := httptest.NewRequest(http.MethodPost, eventPath(ev), strings.NewReader(url.Values{"name": {"Chess"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"notice"`) {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	after, _ := store.GetEvent(ev.ID)
	if after.Name != "Chess" || after.Location != "Cafe" || after.Description != "Bring dice" ||
		after.Capacity != 3 || after.StartsAt != ev.StartsAt {
		t.Errorf("after = %+v", after)
	}
	// A bad value answers the script with the reason, not a page.
	req = httptest.NewRequest(http.MethodPost, eventPath(ev), strings.NewReader(url.Values{"capacity": {"lots"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "whole number") {
		t.Errorf("bad limit = %d %s", rec.Code, rec.Body.String())
	}
}

// TestLeavingAnEventOverItsLimitPromotesNobody: 3/2 with someone waiting,
// one leaves, 2/2 is still full.
func TestLeavingAnEventOverItsLimitPromotesNobody(t *testing.T) {
	store := testStore(t)
	ev := publishedEvent(t, store, 2, "alice", "bob", "waiting")
	if _, err := store.PlaceOnList(ev.ID, "cy", "Cy", StateAttending, "web:org"); err != nil {
		t.Fatal(err)
	}
	result, err := store.Leave(ev.ID, "alice", ActorUser)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promoted != nil {
		t.Errorf("promoted %s while the event was still at its limit", result.Promoted.DiscordUserID)
	}
	if after, _ := store.GetEvent(ev.ID); after.AttendingCount != 2 || after.WaitlistCount != 1 {
		t.Errorf("after = %d going, %d waiting; want 2 and 1", after.AttendingCount, after.WaitlistCount)
	}
}
