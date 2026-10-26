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
	"time"
)

func potluckEvent(t *testing.T, store *Store, joiners ...string) *Event {
	t.Helper()
	ev := testEvent(t, store, 0)
	on := true
	if _, err := store.UpdateEvent(ev.ID, EventPatch{PotluckEnabled: &on}); err != nil {
		t.Fatalf("switch potluck on: %v", err)
	}
	for _, who := range joiners {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", who, err)
		}
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func mustSheet(t *testing.T, store *Store, eventID int64) *PotluckSheet {
	t.Helper()
	ev, err := store.GetEvent(eventID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if ev.Potluck == nil {
		t.Fatalf("event %d has no sheet", eventID)
	}
	return ev.Potluck
}

// TestPotluckSheetTakesOnlyPeopleGoing is the rule the sheet stands on: the
// people going say what they bring, and nobody else is asked or counted.
func TestPotluckSheetTakesOnlyPeopleGoing(t *testing.T) {
	store := testStore(t)
	plain := testEvent(t, store, 0)
	if _, err := store.Join(plain.ID, "ann", "Ann", JoinedViaButton); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPotluckItem(plain.ID, "ann", PotluckFood, "Chips"); !errors.Is(err, ErrPotluckOff) {
		t.Fatalf("event without a sheet: err = %v, want ErrPotluckOff", err)
	}

	ev := potluckEvent(t, store, "ann")
	if _, err := store.AddPotluckItem(ev.ID, "stranger", PotluckFood, "Chips"); !errors.Is(err, ErrNotGoing) {
		t.Fatalf("someone not going: err = %v, want ErrNotGoing", err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", "dessert", "Pie"); !errors.Is(err, ErrInvalidPotluck) {
		t.Fatalf("unknown category: err = %v, want ErrInvalidPotluck", err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "   "); !errors.Is(err, ErrInvalidPotluck) {
		t.Fatalf("empty item: err = %v, want ErrInvalidPotluck", err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "  Lasagna,   serves 8 "); err != nil {
		t.Fatalf("add: %v", err)
	}
	sheet := mustSheet(t, store, ev.ID)
	if len(sheet.Items) != 1 || sheet.Items[0].Item != "Lasagna, serves 8" {
		t.Fatalf("items = %+v, want the lasagna with its spaces tidied", sheet.Items)
	}

	// Leaving takes them off the sheet; rejoining brings what they said back.
	if _, err := store.Leave(ev.ID, "ann", ActorUser); err != nil {
		t.Fatal(err)
	}
	if got := mustSheet(t, store, ev.ID); len(got.Items) != 0 {
		t.Fatalf("after leaving the sheet still shows %+v", got.Items)
	}
	if _, err := store.Join(ev.ID, "ann", "Ann", JoinedViaButton); err != nil {
		t.Fatal(err)
	}
	if got := mustSheet(t, store, ev.ID); len(got.Items) != 1 {
		t.Fatalf("after rejoining the sheet shows %+v, want the lasagna back", got.Items)
	}
}

// TestPotluckNothingAndItemsReplaceEachOther: saying nothing takes your
// items off, and adding an item takes "nothing" back.
func TestPotluckNothingAndItemsReplaceEachOther(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo")
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckDrinks, "Soda"); err != nil {
		t.Fatal(err)
	}
	if err := store.SayBringingNothing(ev.ID, "ann"); err != nil {
		t.Fatal(err)
	}
	sheet := mustSheet(t, store, ev.ID)
	if len(sheet.ItemsOf("ann")) != 0 || !sheet.SaidNothing("ann") || !sheet.Answered("ann") {
		t.Fatalf("after nothing: items %+v, nothing %v", sheet.ItemsOf("ann"), sheet.BringingNothing)
	}
	if sheet.Answered("bo") {
		t.Fatal("bo said nothing at all, yet counts as answered")
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckSupplies, "Ice"); err != nil {
		t.Fatal(err)
	}
	sheet = mustSheet(t, store, ev.ID)
	if sheet.SaidNothing("ann") || len(sheet.ItemsOf("ann")) != 1 {
		t.Fatalf("after adding: nothing %v, items %+v", sheet.BringingNothing, sheet.ItemsOf("ann"))
	}
	if n, err := store.ClearPotluckAnswer(ev.ID, "ann"); err != nil || n != 1 {
		t.Fatalf("clear: %d, %v", n, err)
	}
	if mustSheet(t, store, ev.ID).Answered("ann") {
		t.Fatal("after clearing, ann still counts as answered")
	}
}

// TestPotluckSaysWhatIsStillNeeded is the quick summary: counts against what
// the organiser asked for, and what is missing.
func TestPotluckSaysWhatIsStillNeeded(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo")
	if err := store.SetPotluckNeeds(ev.ID, map[string]int{PotluckFood: 3, PotluckDrinks: 2, PotluckSupplies: 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPotluckNeeds(ev.ID, map[string]int{"dessert": 1}); !errors.Is(err, ErrInvalidPotluck) {
		t.Fatalf("unknown category need: err = %v", err)
	}
	for _, add := range []struct{ who, category, item string }{
		{"ann", PotluckFood, "Lasagna"}, {"bo", PotluckFood, "Salad"}, {"bo", PotluckDrinks, "Lemonade"}, {"ann", PotluckOther, "Speaker"},
	} {
		if _, err := store.AddPotluckItem(ev.ID, add.who, add.category, add.item); err != nil {
			t.Fatal(err)
		}
	}
	sheet := mustSheet(t, store, ev.ID)
	if got, want := sheet.SummaryLine(), "🍲 Food 2/3 · 🥤 Drinks 1/2 · ✨ Other 1"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if got, want := sheet.StillNeededLine(), "Still needed: 1 food, 1 drinks"; got != want {
		t.Errorf("still needed = %q, want %q", got, want)
	}

	card := renderSignupMessage(mustEvent(t, store, ev.ID), nil)
	content := card["content"].(string)
	for _, want := range []string{"**🍲 Potluck**", "Still needed: 1 food, 1 drinks", "🍲 <@ann> Lasagna · <@bo> Salad", "🥤 <@bo> Lemonade"} {
		if !strings.Contains(content, want) {
			t.Errorf("card is missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(fmt.Sprint(card["components"]), PotluckCustomID(ev.ID)) {
		t.Error("card has no What I'm bringing button")
	}
}

func mustEvent(t *testing.T, store *Store, id int64) *Event {
	t.Helper()
	ev, err := store.GetEvent(id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return ev
}

// TestPotluckChangeRedrawsTheCard: the publish signature must move when the
// sheet does, or the card shows a stale sheet until somebody joins.
func TestPotluckChangeRedrawsTheCard(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann")
	before := eventPublishSignature(mustEvent(t, store, ev.ID), nil)
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "Chips"); err != nil {
		t.Fatal(err)
	}
	if eventPublishSignature(mustEvent(t, store, ev.ID), nil) == before {
		t.Fatal("adding an item left the publish signature unchanged")
	}
	plain := testEvent(t, store, 0)
	if strings.Contains(fmt.Sprint(renderSignupMessage(plain, nil)["components"]), "potluck") {
		t.Fatal("an event without a sheet shows the potluck button")
	}
}

// TestPotluckSheetStaysOpenWhenSignupsClose: closing signups is about who
// may join, not about what the people going bring.
func TestPotluckSheetStaysOpenWhenSignupsClose(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann")
	closed := StatusClosed
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "Chips"); err != nil {
		t.Fatalf("add with signups closed: %v", err)
	}
	if !strings.Contains(fmt.Sprint(renderSignupMessage(mustEvent(t, store, ev.ID), nil)["components"]), PotluckCustomID(ev.ID)) {
		t.Fatal("closed event's card lost the What I'm bringing button")
	}
	cancelled := StatusCancelled
	if _, err := store.UpdateEvent(ev.ID, EventPatch{Status: &cancelled}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "Dip"); !errors.Is(err, ErrPotluckClosed) {
		t.Fatalf("cancelled event: err = %v, want ErrPotluckClosed", err)
	}
}

// TestPotluckClearsWhenTheDateRollsOver: a repeating event's next date
// starts with an empty sheet and nobody reminded; the needs stay.
func TestPotluckClearsWhenTheDateRollsOver(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann")
	if err := store.SetPotluckNeeds(ev.ID, map[string]int{PotluckFood: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "Chips"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPotluckReminder(ev.ID, "ann", PotluckReminderAutomatic, potluckReminderSent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RollOverOccurrence(ev.ID, farFutureStart+7*86400, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ev.ID, "ann", "Ann", JoinedViaButton); err != nil {
		t.Fatal(err)
	}
	sheet := mustSheet(t, store, ev.ID)
	if len(sheet.Items) != 0 || sheet.Needs[PotluckFood] != 4 {
		t.Fatalf("next date: items %+v, needs %v; want no items and food 4", sheet.Items, sheet.Needs)
	}
	if reminders, _ := store.PotluckRemindersFor(ev.ID); len(reminders) != 0 {
		t.Fatalf("next date still has reminders %+v", reminders)
	}
}

// TestPotluckButtonsThroughTheSignedHandler presses What I'm bringing and
// fills the food form the way Discord sends them.
func TestPotluckButtonsThroughTheSignedHandler(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := potluckEvent(t, store, "ann")
	press := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	member := `"guild_id":"g1","member":{"user":{"id":"ann","username":"ann"}}`

	panel := press(fmt.Sprintf(`{"type":3,%s,"message":{"id":"x"},"data":{"custom_id":%q}}`, member, PotluckCustomID(ev.ID)))
	if panel["type"].(float64) != callbackTypeChannelMessageWithSrc {
		t.Fatalf("panel answer = %v", panel)
	}
	if !strings.Contains(fmt.Sprint(panel["data"]), "You haven't said what you're bringing yet") {
		t.Fatalf("panel = %v", panel["data"])
	}

	modal := press(fmt.Sprintf(`{"type":3,%s,"message":{"id":"x"},"data":{"custom_id":%q}}`, member, potluckAddCustomID(ev.ID, PotluckFood)))
	if modal["type"].(float64) != callbackTypeModal {
		t.Fatalf("food button answer = %v, want a form", modal)
	}

	submitted := press(fmt.Sprintf(`{"type":5,%s,"message":{"id":"x"},"data":{"custom_id":%q,
		"components":[{"components":[{"custom_id":%q,"value":"Guacamole"}]}]}}`, member,
		potluckModalCustomID(ev.ID, PotluckFood), fieldPotluckItem+"@"+potluckModalCustomID(ev.ID, PotluckFood)))
	if submitted["type"].(float64) != callbackTypeUpdateMessage {
		t.Fatalf("form answer = %v, want the panel edited in place", submitted)
	}
	if !strings.Contains(fmt.Sprint(submitted["data"]), "You're bringing:** 🍲 Guacamole") {
		t.Fatalf("panel after the form = %v", submitted["data"])
	}
	if items := mustSheet(t, store, ev.ID).ItemsOf("ann"); len(items) != 1 || items[0].Item != "Guacamole" {
		t.Fatalf("sheet = %+v", items)
	}

	// Someone not going sees the sheet but gets no buttons to change it.
	stranger := press(fmt.Sprintf(`{"type":3,"guild_id":"g1","member":{"user":{"id":"zed"}},"message":{"id":"x"},"data":{"custom_id":%q}}`,
		PotluckCustomID(ev.ID)))
	data := stranger["data"].(map[string]any)
	if len(data["components"].([]any)) != 0 || !strings.Contains(data["content"].(string), "Join first") {
		t.Fatalf("not going: %v", data)
	}
}

// TestPotluckReminderGoesOnceADayBefore: one DM each to the people going
// who have not answered, from a day out, once, and not to someone who has
// only just got their place.
func TestPotluckReminderGoesOnceADayBefore(t *testing.T) {
	srv, store, fake := reminderServer(t)
	ev := reminderEvent(t, store, 20*time.Hour, "ann", "bo", "cy", "dee")
	on := true
	if _, err := store.UpdateEvent(ev.ID, EventPatch{PotluckEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	far := reminderEvent(t, store, 48*time.Hour, "ann")
	if _, err := store.UpdateEvent(far.ID, EventPatch{PotluckEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckFood, "Chips"); err != nil {
		t.Fatal(err)
	}
	if err := store.SayBringingNothing(ev.ID, "bo"); err != nil {
		t.Fatal(err)
	}
	// cy has had a place for an hour; dee got hers a moment ago.
	if _, err := store.db.Exec(`UPDATE signups SET state_changed_at = ? WHERE discord_user_id IN ('ann','bo','cy')`,
		time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	dms := func() []recordedCall {
		var out []recordedCall
		for _, c := range fake.recorded() {
			if c.Method == http.MethodPost && c.Path == "/users/@me/channels" {
				out = append(out, c)
			}
		}
		return out
	}

	if sent, err := srv.SendDuePotluckReminders(); err != nil || sent != 1 {
		t.Fatalf("sent %d, %v; want one, to cy", sent, err)
	}
	if got := dms(); len(got) != 1 || got[0].Body["recipient_id"] != "cy" {
		t.Fatalf("DMs opened = %+v, want one to cy", got)
	}
	if sent, _ := srv.SendDuePotluckReminders(); sent != 0 {
		t.Fatalf("second run sent %d, want 0", sent)
	}
	if _, err := store.db.Exec(`UPDATE signups SET state_changed_at = ? WHERE discord_user_id = 'dee'`,
		time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if sent, _ := srv.SendDuePotluckReminders(); sent != 1 {
		t.Fatalf("after dee settled sent %d, want 1", sent)
	}
}

// TestTheOrganiserTurnsThePotluckOnAndNudges: the switch, the needs form,
// the list of who has not answered, and the Nudge button's DMs, which count
// against the event's DM limit.
func TestTheOrganiserTurnsThePotluckOnAndNudges(t *testing.T) {
	_, store, fake, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 0, "alice", "bob", "cy")

	postForm(t, mux, token, eventPath(ev), url.Values{"potluck": {"on"}})
	if after, _ := store.GetEvent(ev.ID); !after.PotluckEnabled {
		t.Fatal("the switch did not turn the sheet on")
	}
	postForm(t, mux, token, eventPath(ev)+"/potluck/needs", url.Values{"wanted_food": {"4"}, "wanted_drinks": {"2"}})
	if _, err := store.AddPotluckItem(ev.ID, "alice", PotluckFood, "Tamales"); err != nil {
		t.Fatal(err)
	}

	page := getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{`id="potluck"`, "Still needed: 3 food, 2 drinks", "Tamales", "answered</h3>",
		`action="` + eventPath(ev) + `/potluck/nudge"`, "changed the potluck sheet: off → on"} {
		if !strings.Contains(page, want) {
			t.Errorf("event page lacks %s", want)
		}
	}

	postForm(t, mux, token, eventPath(ev)+"/potluck/nudge", url.Values{})
	var opened []string
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/users/@me/channels" {
			opened = append(opened, fmt.Sprint(c.Body["recipient_id"]))
		}
	}
	if strings.Join(opened, ",") != "bob,cy" {
		t.Fatalf("nudged %v, want bob and cy, who have not answered", opened)
	}
	messages, _ := store.Messages(ev.ID)
	if len(messages) != 1 || messages[0].Via != MessageViaDM || messages[0].Delivered != 2 {
		t.Fatalf("messages = %+v, want the nudge recorded as one DM message to two", messages)
	}
}

// TestABigRosterKeepsThePotluckSummaryWhole: with forty people going the
// card has no room for every item, so it shows the counts and points at the
// button rather than cutting the list off mid-item.
func TestABigRosterKeepsThePotluckSummaryWhole(t *testing.T) {
	store := testStore(t)
	var people []string
	for i := 0; i < 40; i++ {
		people = append(people, fmt.Sprintf("1%017d", i))
	}
	ev := potluckEvent(t, store, people...)
	for _, who := range people[:12] {
		if _, err := store.AddPotluckItem(ev.ID, who, PotluckDrinks, strings.Repeat("Spooky punch ", 6)); err != nil {
			t.Fatal(err)
		}
	}
	ev = mustEvent(t, store, ev.ID)
	roster, _ := store.Roster(ev.ID, false)
	content := renderSignupMessage(ev, roster)["content"].(string)
	if strings.Contains(content, "trimmed") {
		t.Fatalf("card was trimmed:\n%s", content)
	}
	for _, want := range []string{"🥤 Drinks 12", "to see who is bringing what"} {
		if !strings.Contains(content, want) {
			t.Errorf("card lacks %q:\n%s", want, content)
		}
	}
}

// TestAMemberFillsInThePotluckFromTheEventPage: someone going adds, says
// nothing and removes their own; they cannot remove anyone else's, and the
// page never names who has not answered.
func TestAMemberFillsInThePotluckFromTheEventPage(t *testing.T) {
	_, store, _, mux, _ := webTestServer(t)
	ev, _ := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c", Name: "Party", Status: StatusOpen,
		StartsAt: 4102444800, CreatedBy: "host"})
	on := true
	store.UpdateEvent(ev.ID, EventPatch{PotluckEnabled: &on})
	for _, who := range []string{"kat", "bo", "quiet"} {
		if _, err := store.Join(ev.ID, who, who, JoinedViaButton); err != nil {
			t.Fatal(err)
		}
	}
	kat, _ := store.CreateWebSession("kat", "Kat", "", map[string]uint64{"g1": 0})
	bo, _ := store.CreateWebSession("bo", "Bo", "", map[string]uint64{"g1": 0})
	stranger, _ := store.CreateWebSession("zed", "Zed", "", map[string]uint64{"g1": 0})

	postForm(t, mux, kat.Token, eventPath(ev)+"/potluck/mine", url.Values{"category": {PotluckFood}, "item": {"Pumpkin pie"}})
	postForm(t, mux, bo.Token, eventPath(ev)+"/potluck/mine", url.Values{"category": {PotluckDrinks}, "item": {"Cider"}})
	rec := postForm(t, mux, stranger.Token, eventPath(ev)+"/potluck/mine", url.Values{"category": {PotluckFood}, "item": {"Chips"}})
	if notice, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(notice, "Join first") {
		t.Errorf("someone not going adding: %q", notice)
	}
	sheet := mustSheet(t, store, ev.ID)
	if len(sheet.Items) != 2 {
		t.Fatalf("sheet = %+v, want the pie and the cider", sheet.Items)
	}

	page := getPage(t, mux, kat.Token, eventPath(ev)).Body.String()
	for _, want := range []string{"Pumpkin pie", "Cider", "What you", `/potluck/mine/remove`} {
		if !strings.Contains(page, want) {
			t.Errorf("Kat's page lacks %q", want)
		}
	}
	if strings.Contains(page, "answered</h3>") || strings.Contains(page, "only organisers see") {
		t.Error("the members' page says who has not answered")
	}

	cider := sheet.ItemsOf("bo")[0]
	postForm(t, mux, kat.Token, eventPath(ev)+"/potluck/mine/remove", url.Values{"item_id": {fmt.Sprint(cider.ID)}})
	if len(mustSheet(t, store, ev.ID).ItemsOf("bo")) != 1 {
		t.Fatal("Kat removed Bo's cider")
	}
	postForm(t, mux, kat.Token, eventPath(ev)+"/potluck/mine/nothing", url.Values{})
	if got := mustSheet(t, store, ev.ID); !got.SaidNothing("kat") || len(got.ItemsOf("kat")) != 0 {
		t.Fatalf("after nothing: %+v", got)
	}

	// Leave from the event page comes back to it.
	rec = postForm(t, mux, kat.Token, eventPath(ev)+"/leave", url.Values{"back": {"event"}})
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, eventPath(ev)+"?") {
		t.Errorf("leave from the event page went to %q", loc)
	}
}

// TestAPotluckCardLinksTheEventPage: the forum card of an event with a
// sheet carries a link button to its page, beside the other buttons; one
// without a sheet does not.
func TestAPotluckCardLinksTheEventPage(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	srv.EnableWeb(&OAuthConfig{RedirectURL: "https://events.example/auth/callback"})
	ev := potluckEvent(t, store, "ann")
	card := RenderForumCard(ev, nil)
	srv.addEventPageButton(card, ev)
	if !strings.Contains(fmt.Sprint(card["components"]), fmt.Sprintf("url:https://events.example/events/%d", ev.ID)) {
		t.Fatalf("potluck card has no link to its page: %v", card["components"])
	}
	if rows := card["components"].([]any); len(rows) != 1 {
		t.Fatalf("the link went on a row of its own: %d rows", len(rows))
	}
	plain := testEvent(t, store, 0)
	card = RenderForumCard(plain, nil)
	srv.addEventPageButton(card, plain)
	if strings.Contains(fmt.Sprint(card["components"]), "url:") {
		t.Fatal("a card without a sheet got the link")
	}
}
