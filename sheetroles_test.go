package discordsignup

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestARoleTakesPeopleGoingUpToItsCap: going people take and leave a role
// with one toggle, a full role refuses, someone not going is refused, and
// the card carries a button for it.
func TestARoleTakesPeopleGoingUpToItsCap(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo", "cy")
	switchRolesOn(t, store, ev.ID)
	setup, err := store.AddSheetRole(ev.ID, "  Setup   (6:00) ", 2, "test")
	if err != nil || setup.Name != "Setup (6:00)" {
		t.Fatalf("add = %+v, %v", setup, err)
	}
	if _, err := store.AddSheetRole(ev.ID, "setup (6:00)", 0, "test"); !errors.Is(err, ErrInvalidSheetRole) {
		t.Fatalf("a second role of the same name: err = %v", err)
	}
	for _, who := range []string{"ann", "bo"} {
		if in, err := store.ToggleSheetRole(ev.ID, setup.ID, who); err != nil || !in {
			t.Fatalf("%s take = %v, %v", who, in, err)
		}
	}
	if _, err := store.ToggleSheetRole(ev.ID, setup.ID, "cy"); !errors.Is(err, ErrSheetRoleFull) {
		t.Fatalf("a third on a cap of 2: err = %v", err)
	}
	if _, err := store.ToggleSheetRole(ev.ID, setup.ID, "stranger"); !errors.Is(err, ErrNotGoing) {
		t.Fatalf("someone not going: err = %v", err)
	}
	if in, err := store.ToggleSheetRole(ev.ID, setup.ID, "ann"); err != nil || in {
		t.Fatalf("ann leaving = %v, %v", in, err)
	}
	if in, _ := store.ToggleSheetRole(ev.ID, setup.ID, "cy"); !in {
		t.Fatal("cy could not take the place ann left")
	}

	ev = mustEvent(t, store, ev.ID)
	if got := sheetRolesLine(ev.Roles); got != "🙋 Setup (6:00) 2/2" {
		t.Errorf("roles line = %q", got)
	}
	card := renderSignupMessage(ev, nil)
	if !strings.Contains(fmt.Sprint(card["components"]), sheetRoleCustomID(sheetRoleCardPrefix, ev.ID, setup.ID)) {
		t.Error("the card has no button for the role")
	}
	if !strings.Contains(card["content"].(string), "🙋 **Setup (6:00)** 2/2: <@bo>, <@cy>") {
		t.Errorf("card does not list the role:\n%s", card["content"])
	}

	// Leaving the event takes them off the role's count, as off the sheet.
	if _, err := store.Leave(ev.ID, "bo", ActorUser); err != nil {
		t.Fatal(err)
	}
	if got := mustEvent(t, store, ev.ID).Roles[0].Members; len(got) != 1 {
		t.Fatalf("after bo left the event the role has %v", got)
	}
	// A new date clears the roles' people and keeps the roles.
	if _, _, err := store.RollOverOccurrence(ev.ID, farFutureStart+7*86400, 0); err != nil {
		t.Fatal(err)
	}
	store.Join(ev.ID, "cy", "cy", JoinedViaButton)
	if roles := mustEvent(t, store, ev.ID).Roles; len(roles) != 1 || len(roles[0].Members) != 0 {
		t.Fatalf("next date roles = %+v", roles)
	}
}

// TestASpreadsheetColumnFillsTheRoleOfTheSameName: SETUP (6:00) fills
// Setup, a role ticked later does not copy the item again, and someone who
// leaves a copied role stays out until their row changes.
func TestASpreadsheetColumnFillsTheRoleOfTheSameName(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo")
	switchRolesOn(t, store, ev.ID)
	store.db.Exec(`UPDATE signups SET display_name = 'Ann' WHERE discord_user_id = 'ann'`)
	store.db.Exec(`UPDATE signups SET display_name = 'Bo' WHERE discord_user_id = 'bo'`)
	const sheet = "NAME,SETUP (6:00),TEARDOWN,DRINKS,FOOD,Explain\nAnn,TRUE,FALSE,TRUE,FALSE,Lemonade\nBo,FALSE,TRUE,FALSE,FALSE,\n"
	rows, err := parsePotluckSheet(strings.NewReader(sheet))
	if err != nil {
		t.Fatal(err)
	}
	roster, _ := store.Roster(ev.ID, false)
	copyAll := func() {
		roles := mustEvent(t, store, ev.ID).Roles
		for _, row := range rows {
			userID, _ := matchGoing(row.Name, roster)
			if _, err := store.copySheetRow(ev.ID, userID, row, roles); err != nil {
				t.Fatal(err)
			}
		}
	}
	copyAll()
	lemonade := mustSheet(t, store, ev.ID).ItemsOf("ann")[0]

	setup, _ := store.AddSheetRole(ev.ID, "Setup", 12, "test")
	teardown, _ := store.AddSheetRole(ev.ID, "Teardown (next day)", 12, "test")
	copyAll()
	got := mustSheet(t, store, ev.ID)
	roles := mustEvent(t, store, ev.ID).Roles
	if r, _ := sheetRoleOf(roles, setup.ID); strings.Join(r.Members, ",") != "ann" {
		t.Errorf("setup = %v, want ann", r.Members)
	}
	if r, _ := sheetRoleOf(roles, teardown.ID); strings.Join(r.Members, ",") != "bo" {
		t.Errorf("teardown = %v, want bo", r.Members)
	}
	if items := got.ItemsOf("ann"); len(items) != 1 || items[0].ID != lemonade.ID {
		t.Errorf("adding roles copied Ann's item again: %+v", items)
	}

	if in, _ := store.ToggleSheetRole(ev.ID, setup.ID, "ann"); in {
		t.Fatal("ann's toggle put her in rather than taking her out")
	}
	copyAll()
	if r, _ := sheetRoleOf(mustEvent(t, store, ev.ID).Roles, setup.ID); len(r.Members) != 0 {
		t.Fatal("an unchanged row put ann back in a role she left")
	}
}

// TestRolesOnTheEventPagesAndInTheLog: the organiser adds a role, a member
// going takes it from the members' page, and the log has both.
func TestRolesOnTheEventPagesAndInTheLog(t *testing.T) {
	_, store, _, mux, token := webTestServer(t)
	ev := publishedEvent(t, store, 0, "alice", "member")
	on := true
	store.UpdateEvent(ev.ID, EventPatch{RolesEnabled: &on})
	member, _ := store.CreateWebSession("member", "Member", "", map[string]uint64{"g1": 0})

	postForm(t, mux, token, eventPath(ev)+"/roles", url.Values{"name": {"Setup (6:00)"}, "capacity": {"12"}})
	role := mustEvent(t, store, ev.ID).Roles[0]
	page := getPage(t, mux, member.Token, eventPath(ev)).Body.String()
	if !strings.Contains(page, "Setup (6:00)") || !strings.Contains(page, "Count me in") {
		t.Fatal("the members' page does not offer the role")
	}
	postForm(t, mux, member.Token, eventPath(ev)+"/roles/toggle", url.Values{"role_id": {fmt.Sprint(role.ID)}})
	if r := mustEvent(t, store, ev.ID).Roles[0]; !r.Has("member") {
		t.Fatal("the toggle did not put the member in the role")
	}
	organiserPage := getPage(t, mux, token, eventPath(ev)).Body.String()
	for _, want := range []string{"added the role 🙋 Setup (6:00)", "took the role 🙋 Setup (6:00)"} {
		if !strings.Contains(organiserPage, want) {
			t.Errorf("the log lacks %q", want)
		}
	}
	if strings.Contains(page, "/roles/remove") {
		t.Error("the members' page offers to remove a role")
	}
}

// TestARoleButtonOnTheCardTogglesThroughTheSignedHandler.
func TestARoleButtonOnTheCardTogglesThroughTheSignedHandler(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := potluckEvent(t, store, "ann")
	switchRolesOn(t, store, ev.ID)
	role, _ := store.AddSheetRole(ev.ID, "Setup", 0, "test")
	press := func(customID string) string {
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000",
			fmt.Sprintf(`{"type":3,"guild_id":"g1","member":{"user":{"id":"ann"}},"message":{"id":"x"},"data":{"custom_id":%q}}`, customID)))
		return rec.Body.String()
	}
	if got := press(sheetRoleCustomID(sheetRoleCardPrefix, ev.ID, role.ID)); !strings.Contains(got, "You're down for Setup (1)") {
		t.Fatalf("first press = %s", got)
	}
	if got := press(sheetRoleCustomID(sheetRolePanelPrefix, ev.ID, role.ID)); !strings.Contains(got, "Took you off Setup (0)") ||
		!strings.Contains(got, fmt.Sprintf(`"type":%d`, callbackTypeUpdateMessage)) {
		t.Fatalf("press in the view = %s", got)
	}
}

// switchRolesOn turns an event's roles on, as the settings' switch does.
func switchRolesOn(t *testing.T, store *Store, eventID int64) {
	t.Helper()
	on := true
	if _, err := store.UpdateEvent(eventID, EventPatch{RolesEnabled: &on}); err != nil {
		t.Fatalf("switch roles on: %v", err)
	}
}

// TestRolesWorkWithoutAPotluckSheet: an event with roles on and the sheet
// off has the roles on its card and pages and no What I'm bringing; one
// with the sheet on and roles off shows no roles; and switching roles off
// refuses a press and keeps who was in them.
func TestRolesWorkWithoutAPotluckSheet(t *testing.T) {
	store := testStore(t)
	ev := testEvent(t, store, 0)
	switchRolesOn(t, store, ev.ID)
	store.Join(ev.ID, "ann", "ann", JoinedViaButton)
	setup, err := store.AddSheetRole(ev.ID, "Setup", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if in, err := store.ToggleSheetRole(ev.ID, setup.ID, "ann"); err != nil || !in {
		t.Fatalf("take a role with no sheet = %v, %v", in, err)
	}
	ev = mustEvent(t, store, ev.ID)
	if ev.Potluck != nil || len(ev.Roles) != 1 || !ev.Roles[0].Has("ann") {
		t.Fatalf("event = potluck %v, roles %+v; want no sheet and ann in Setup", ev.Potluck, ev.Roles)
	}
	card := renderSignupMessage(ev, nil)
	components := fmt.Sprint(card["components"])
	if !strings.Contains(components, sheetRoleCustomID(sheetRoleCardPrefix, ev.ID, setup.ID)) ||
		strings.Contains(components, PotluckCustomID(ev.ID)) {
		t.Errorf("card components = %s; want the role and no What I'm bringing", components)
	}
	content := card["content"].(string)
	if !strings.Contains(content, "**🙋 Roles**") || strings.Contains(content, "Potluck") {
		t.Errorf("card = %s; want Roles and no Potluck", content)
	}
	if note := potluckAndRolesJoinNote(ev); !strings.Contains(note, "Sign up to help if you can:** Setup") || strings.Contains(note, "potluck") {
		t.Errorf("join note = %q", note)
	}

	off := false
	store.UpdateEvent(ev.ID, EventPatch{RolesEnabled: &off})
	if _, err := store.ToggleSheetRole(ev.ID, setup.ID, "ann"); !errors.Is(err, ErrSheetRolesOff) {
		t.Errorf("a press with roles off: err = %v", err)
	}
	if card := renderSignupMessage(mustEvent(t, store, ev.ID), nil); strings.Contains(fmt.Sprint(card), "Setup") {
		t.Errorf("roles off, the card still shows them: %v", card)
	}
	potluckOnly := potluckEvent(t, store, "ann")
	store.AddSheetRole(potluckOnly.ID, "Teardown", 0, "test")
	if roles := mustEvent(t, store, potluckOnly.ID).Roles; roles != nil {
		t.Errorf("a potluck with roles off reads roles %+v", roles)
	}
	switchRolesOn(t, store, ev.ID)
	if roles := mustEvent(t, store, ev.ID).Roles; len(roles) != 1 || !roles[0].Has("ann") {
		t.Errorf("roles back on = %+v; want ann still in Setup", roles)
	}
}

// TestAPotluckThatHadRolesKeepsThemOn: the one-off fill for the new switch
// turns roles on where the sheet was on and had a role, and nowhere else.
func TestAPotluckThatHadRolesKeepsThemOn(t *testing.T) {
	store := testStore(t)
	withRoles := potluckEvent(t, store)
	store.AddSheetRole(withRoles.ID, "Setup", 0, "test")
	withoutRoles := potluckEvent(t, store)
	sheetOff := testEvent(t, store, 0)
	store.AddSheetRole(sheetOff.ID, "Setup", 0, "test")
	if _, err := store.db.Exec(columnBackfills["events.roles_enabled"]); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ev   *Event
		want bool
	}{{withRoles, true}, {withoutRoles, false}, {sheetOff, false}} {
		if got := mustEvent(t, store, c.ev.ID).RolesEnabled; got != c.want {
			t.Errorf("event %d roles on = %v, want %v", c.ev.ID, got, c.want)
		}
	}
}
