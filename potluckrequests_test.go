package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestARequestIsTakenUpToHowManyAreWanted: each person takes it once, it
// closes when enough have, it counts on the sheet in its category, and
// someone leaving opens it again.
func TestARequestIsTakenUpToHowManyAreWanted(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo", "cy")
	ice, err := store.AddPotluckRequest(ev.ID, PotluckSupplies, "  Ice ", 2, "test")
	if err != nil || ice.Item != "Ice" {
		t.Fatalf("add = %+v, %v", ice, err)
	}
	if _, err := store.TakePotluckRequest(ev.ID, ice.ID, "ann"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakePotluckRequest(ev.ID, ice.ID, "ann"); !errors.Is(err, ErrRequestTaken) {
		t.Fatalf("ann taking it twice: err = %v", err)
	}
	if _, err := store.TakePotluckRequest(ev.ID, ice.ID, "bo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakePotluckRequest(ev.ID, ice.ID, "cy"); !errors.Is(err, ErrRequestTaken) {
		t.Fatalf("a third on 2 wanted: err = %v", err)
	}
	sheet := mustSheet(t, store, ev.ID)
	if sheet.AskedForLine() != "" || len(sheet.ItemsIn(PotluckSupplies)) != 2 {
		t.Fatalf("asked-for %q, supplies %+v", sheet.AskedForLine(), sheet.ItemsIn(PotluckSupplies))
	}
	store.Leave(ev.ID, "bo", ActorUser)
	if got := mustSheet(t, store, ev.ID).AskedForLine(); got != "📌 Asked for: 🍴 Ice (1 of 2)" {
		t.Fatalf("after bo left, asked-for = %q", got)
	}
	if _, err := store.TakePotluckRequest(ev.ID, ice.ID, "cy"); err != nil {
		t.Fatalf("cy taking the place bo left: %v", err)
	}
	// Taking a request off the sheet leaves what people said they'd bring.
	if err := store.RemovePotluckRequest(ev.ID, ice.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if got := mustSheet(t, store, ev.ID); len(got.Requests) != 0 || len(got.ItemsIn(PotluckSupplies)) != 2 {
		t.Fatalf("after removing: requests %+v, supplies %d", got.Requests, len(got.ItemsIn(PotluckSupplies)))
	}
}

// TestRequestsArePutInFrontOfSomeoneAddingTheirs: in the view's text and
// menu, as the example in the category's form, and on the card; and taking
// one from the menu adds it.
func TestRequestsArePutInFrontOfSomeoneAddingTheirs(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := potluckEvent(t, store, "ann")
	ice, _ := store.AddPotluckRequest(ev.ID, PotluckSupplies, "Ice", 1, "test")
	store.AddPotluckRequest(ev.ID, PotluckSupplies, "Cups", 1, "test")
	press := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", body))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return out
	}
	member := `"guild_id":"g1","member":{"user":{"id":"ann"}},"message":{"id":"x"}`

	view := fmt.Sprint(press(fmt.Sprintf(`{"type":3,%s,"data":{"custom_id":%q}}`, member, PotluckCustomID(ev.ID)))["data"])
	for _, want := range []string{"📌 Asked for: 🍴 Ice · 🍴 Cups", "Bring something that was asked for"} {
		if !strings.Contains(view, want) {
			t.Errorf("the view lacks %q", want)
		}
	}
	form := fmt.Sprint(press(fmt.Sprintf(`{"type":3,%s,"data":{"custom_id":%q}}`, member, potluckAddCustomID(ev.ID, PotluckSupplies)))["data"])
	if !strings.Contains(form, "placeholder:Asked for: Ice, Cups") {
		t.Errorf("the supplies form does not show what was asked for: %s", form)
	}
	taken := fmt.Sprint(press(fmt.Sprintf(`{"type":3,%s,"data":{"custom_id":"signup:%s:%d","values":["%d"]}}`,
		member, potluckTakeRequestAction, ev.ID, ice.ID))["data"])
	if !strings.Contains(taken, "Added: 🍴 Ice, as asked.") {
		t.Fatalf("taking from the menu = %s", taken)
	}
	if got := mustEvent(t, store, ev.ID); got.Potluck.AskedForLine() != "📌 Asked for: 🍴 Cups" {
		t.Errorf("asked-for after taking ice = %q", got.Potluck.AskedForLine())
	}
	if card := renderSignupMessage(mustEvent(t, store, ev.ID), nil)["content"].(string); !strings.Contains(card, "📌 Asked for: 🍴 Cups") {
		t.Errorf("the card does not say what is asked for:\n%s", card)
	}
}

// TestAnOrganiserAsksAndAMemberTakesItOnTheWeb, with both in the log.
func TestAnOrganiserAsksAndAMemberTakesItOnTheWeb(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 0, "alice", "member")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{PotluckEnabled: &on})
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})

	postForm(t, mux, token, eventPath(ev)+"/potluck/requests", url.Values{"category": {PotluckOther}, "item": {"Folding table"}, "wanted": {"1"}})
	request := mustEvent(t, store, ev.ID).Potluck.Requests[0]
	page := getPage(t, mux, member.Token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "Folding table") || !strings.Contains(page, "I&#39;ll bring this") && !strings.Contains(page, "I'll bring this") {
		t.Fatal("the members' page does not offer the request")
	}
	postForm(t, mux, member.Token, eventPath(ev)+"/potluck/requests/take", url.Values{"request_id": {fmt.Sprint(request.ID)}})
	if items := mustSheet(t, store, ev.ID).ItemsOf("member"); len(items) != 1 || items[0].RequestID != request.ID {
		t.Fatalf("member's items = %+v", items)
	}
	organiserPage := getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{"asked for ✨ Folding table", "potluck: bringing ✨ Folding table"} {
		if !strings.Contains(organiserPage, want) {
			t.Errorf("the log lacks %q", want)
		}
	}
	rec := httptest.NewRequest(http.MethodPost, eventPath(ev)+"/potluck/requests", strings.NewReader("category=food&item=x"))
	rec.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec.AddCookie(&http.Cookie{Name: sessionCookieName, Value: member.Token})
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, rec)
	if got.Code != http.StatusNotFound {
		t.Errorf("a member asking for something = %d, want 404", got.Code)
	}
}
