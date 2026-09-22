package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestASiteAdminMayEditAndCreateAnywhere, even in a server whose editor role
// they do not hold, and a plain member still may not.
func TestASiteAdminMayEditAndCreateAnywhere(t *testing.T) {
	srv, store, _, ev := editorRoleServer(t, false)
	if ok, _ := srv.mayEdit(pressBy(t, "u-admin-person", 0), ev); ok {
		t.Fatal("may edit before being made an admin")
	}
	if _, err := store.AddSiteAdmin("u-admin-person"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if ok, why := srv.mayEdit(pressBy(t, "u-admin-person", 0), ev); !ok {
		t.Errorf("site admin may not edit: %s", why)
	}
	if ok, why := srv.mayCreate(pressBy(t, "u-admin-person", 0)); !ok {
		t.Errorf("site admin may not create: %s", why)
	}
	if ok, _ := srv.mayEdit(pressBy(t, "u-someone", 0), ev); ok {
		t.Error("a plain member may edit")
	}
	if err := store.RemoveSiteAdmin("u-admin-person"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ok, _ := srv.mayEdit(pressBy(t, "u-admin-person", 0), ev); ok {
		t.Error("may still edit after removal")
	}
}

// TestASiteAdminSeesServersTheyAreNotIn, on the front page and an event's
// page.
func TestASiteAdminSeesServersTheyAreNotIn(t *testing.T) {
	_, store, fake, mux, _ := webTestServer(t)
	fake.on(http.MethodGet, "/users/@me/guilds", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"g1","name":"One"},{"id":"g-other","name":"Other"}]`))
	})
	elsewhere, err := store.CreateEvent(Event{GuildID: "g-other", ChannelID: "c", Name: "Elsewhere night",
		Status: StatusOpen, StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	visitor, _ := store.CreateWebSession("u-visitor", "Visitor", "", map[string]uint64{"g1": 0})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: visitor.Token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	eventPage := "/events/" + strconv.FormatInt(elsewhere.ID, 10)
	if rec := get(eventPage); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-member non-admin got %d on another server's event", rec.Code)
	}
	if strings.Contains(get("/").Body.String(), "Elsewhere night") {
		t.Fatal("a non-admin sees another server's event on the front page")
	}
	store.AddSiteAdmin("u-visitor")
	if rec := get(eventPage); rec.Code != http.StatusOK {
		t.Errorf("a site admin got %d on another server's event", rec.Code)
	}
	if !strings.Contains(get("/").Body.String(), "Elsewhere night") {
		t.Error("a site admin does not see another server's event on the front page")
	}
}
