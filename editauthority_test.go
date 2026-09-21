package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pressBy is a button press in guild g1 by userID holding permission bits and
// roles.
func pressBy(t *testing.T, userID string, bits uint64, roles ...string) *Interaction {
	t.Helper()
	rolesJSON, _ := json.Marshal(roles)
	raw := fmt.Sprintf(`{"guild_id":"g1","channel_id":"c1",
		"member":{"permissions":%q,"roles":%s,"user":{"id":%q}}}`,
		strconv.FormatUint(bits, 10), rolesJSON, userID)
	var in Interaction
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("build interaction: %v", err)
	}
	return &in
}

// editorRoleServer is g1 with the editor role "role-mod", owned by u-owner.
func editorRoleServer(t *testing.T, anyoneMayCreate bool) (*Server, *Store, *fakeDiscord, *Event) {
	t.Helper()
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"g1","owner_id":"u-owner"}`))
	})
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	if _, err := store.SetGuildEditingRule(GuildEditingRule{GuildID: "g1", EditorRoleID: "role-mod",
		AnyoneMayCreate: anyoneMayCreate}); err != nil {
		t.Fatalf("set rule: %v", err)
	}
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games", Status: StatusOpen,
		StartsAt: time.Now().Add(time.Hour).Unix(), CreatedBy: "u-creator"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return srv, store, fake, ev
}

// TestTheDefaultRuleIsManageEventsOrCreator: servers without a rule behave
// as before.
func TestTheDefaultRuleIsManageEventsOrCreator(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games", Status: StatusOpen,
		StartsAt: time.Now().Add(time.Hour).Unix(), CreatedBy: "u-creator"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, c := range []struct {
		who  string
		in   *Interaction
		want bool
	}{
		{"Manage Events", pressBy(t, "u-a", permissionManageEvents), true},
		{"Administrator", pressBy(t, "u-b", permissionAdministrator), true},
		{"the creator", pressBy(t, "u-creator", 0), true},
		{"a member", pressBy(t, "u-c", 0), false},
		{"a member holding some role", pressBy(t, "u-d", 0, "role-mod"), false},
	} {
		if ok, why := srv.mayEdit(c.in, ev); ok != c.want {
			t.Errorf("%s: may edit = %v (%s), want %v", c.who, ok, why, c.want)
		}
	}
}

// TestAnEditorRoleReplacesManageEvents: creator, role or owner; Manage Events
// and Administrator no longer count.
func TestAnEditorRoleReplacesManageEvents(t *testing.T) {
	srv, _, _, ev := editorRoleServer(t, false)
	for _, c := range []struct {
		who  string
		in   *Interaction
		want bool
	}{
		{"the editor role", pressBy(t, "u-mod", 0, "role-other", "role-mod"), true},
		{"the owner", pressBy(t, "u-owner", 0), true},
		{"the creator", pressBy(t, "u-creator", 0), true},
		{"Manage Events without the role", pressBy(t, "u-a", permissionManageEvents), false},
		{"Administrator without the role", pressBy(t, "u-b", permissionAdministrator), false},
		{"a member", pressBy(t, "u-c", 0, "role-other"), false},
	} {
		if ok, why := srv.mayEdit(c.in, ev); ok != c.want {
			t.Errorf("%s: may edit = %v (%s), want %v", c.who, ok, why, c.want)
		}
	}
	_, why := srv.mayEdit(pressBy(t, "u-c", 0), ev)
	if !strings.Contains(why, "<@&role-mod>") || !strings.Contains(why, "owner") {
		t.Errorf("refusal = %q, want it to name the role and the owner", why)
	}
}

// TestARefusalNamingTheRolePingsNobody.
func TestARefusalNamingTheRolePingsNobody(t *testing.T) {
	srv, _, _, ev := editorRoleServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleEditButton(rec, pressBy(t, "u-c", 0), ev.ID)
	if !strings.Contains(rec.Body.String(), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("refusal carries no empty allowed_mentions: %s", rec.Body.String())
	}
}

// TestTheWebPageReadsRolesFreshEachTime: a login copies permission bits, not
// roles, and taking the role away must take the right away at once.
func TestTheWebPageReadsRolesFreshEachTime(t *testing.T) {
	srv, store, fake, ev := editorRoleServer(t, false)
	roles := `["role-mod"]`
	fake.on(http.MethodGet, "/guilds/g1/members/u-mod", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"roles":` + roles + `}`))
	})
	session, err := store.CreateWebSession("u-mod", "Mod", "", map[string]uint64{"g1": 0})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if ok, err := srv.mayEditEvent(session.editActor("g1"), ev); err != nil || !ok {
		t.Fatalf("with the role: %v, %v", ok, err)
	}
	roles = `[]`
	if ok, err := srv.mayEditEvent(session.editActor("g1"), ev); err != nil || ok {
		t.Errorf("after the role was taken away: may edit = %v, %v", ok, err)
	}
}

// TestAServerCanLetAnyoneCreate.
func TestAServerCanLetAnyoneCreate(t *testing.T) {
	closed, _, _, _ := editorRoleServer(t, false)
	if ok, _ := closed.mayCreate(pressBy(t, "u-c", 0)); ok {
		t.Error("a plain member may create where the server did not allow it")
	}
	if ok, _ := closed.mayCreate(pressBy(t, "u-a", permissionCreateEvents)); !ok {
		t.Error("Create Events may not create; the editor role should not change who creates")
	}
	open, _, _, _ := editorRoleServer(t, true)
	if ok, why := open.mayCreate(pressBy(t, "u-c", 0)); !ok {
		t.Errorf("a plain member may not create where anyone may: %s", why)
	}
}

// TestSettingTheRuleChecksTheRole: a mistyped role id would lock every
// organiser out, so it is refused.
func TestSettingTheRuleChecksTheRole(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1/roles", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"role-mod","name":"modewator","position":2}]`))
	})
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/guilds/g1/editing", strings.NewReader(body)))
		return rec
	}
	for _, bad := range []string{
		`{"editor_role_id":"role-typo","anyone_may_create":true}`,
		`{"editor_role_id":"role-mod"}`,
		`{"editor_role":"role-mod","anyone_may_create":true}`,
	} {
		if rec := put(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", bad, rec.Code)
		}
	}
	if rule, _ := store.GuildEditingRule("g1"); rule.EditorRoleID != "" {
		t.Fatalf("a refused PUT stored %+v", rule)
	}
	if rec := put(`{"editor_role_id":"role-mod","anyone_may_create":true}`); rec.Code != http.StatusOK {
		t.Fatalf("valid PUT answered %d: %s", rec.Code, rec.Body.String())
	}
	rule, err := store.GuildEditingRule("g1")
	if err != nil || rule.EditorRoleID != "role-mod" || !rule.AnyoneMayCreate {
		t.Errorf("stored %+v, %v", rule, err)
	}
	if other, _ := store.GuildEditingRule("g2"); other.EditorRoleID != "" || other.AnyoneMayCreate {
		t.Errorf("another server picked up the rule: %+v", other)
	}
}
