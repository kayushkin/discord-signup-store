package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMyEventsButtonActuallyRoutes pins the fix for a button that shipped dead:
// "my-events" matched no case and no prefix, so it answered "Unknown signup
// action" to everyone.
func TestMyEventsButtonActuallyRoutes(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := testEvent(t, store, 3)
	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 3, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q},
		"member": {"permissions": "0", "user": {"id": "alice", "username": "alice"}}
	}`, myEventsButtonID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "Unknown signup action") {
		t.Fatal("the button still routes nowhere")
	}
	data := out["data"].(map[string]any)
	flags := int(data["flags"].(float64))
	if flags&messageFlagEphemeral == 0 || flags&messageFlagComponentsV2 == 0 {
		t.Errorf("flags = %d, want ephemeral and Components V2", flags)
	}
	// Alice is in the event, so her row says so and offers Leave, not Join.
	if !strings.Contains(raw, "you are **going**") {
		t.Error("the viewer's own state is not on the row")
	}
	if !strings.Contains(raw, dashLeaveCustomID(ev.ID)) {
		t.Error("no Leave button for an event the viewer is in")
	}
	if strings.Contains(raw, dashJoinCustomID(ev.ID)) {
		t.Error("a Join button for an event the viewer is already in")
	}
	// No Manage Events, not the creator: no Edit button.
	if strings.Contains(raw, EditCustomID(ev.ID)) {
		t.Error("an Edit button for someone with no permission")
	}
}

// TestDashboardJoinUpdatesInPlace: pressing Join answers with UPDATE_MESSAGE
// (type 7) and the re-rendered view flips the row to Leave.
func TestDashboardJoinUpdatesInPlace(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := testEvent(t, store, 3)

	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 3, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q},
		"member": {"permissions": "0", "user": {"id": "bob", "username": "bob"}}
	}`, dashJoinCustomID(ev.ID))))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if int(out["type"].(float64)) != callbackTypeUpdateMessage {
		t.Fatalf("type = %v, want %d (update in place)", out["type"], callbackTypeUpdateMessage)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, "you are **going**") {
		t.Error("the updated view does not show the new state")
	}
	if !strings.Contains(raw, dashLeaveCustomID(ev.ID)) {
		t.Error("the row did not flip to Leave")
	}
	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AttendingCount != 1 {
		t.Errorf("attending = %d, want 1 — the join must be real, not just rendered", got.AttendingCount)
	}
}

// TestDashboardShowsEditOnlyToWhoMayEdit: Manage Events sees Edit everywhere,
// a creator sees it on their own event, everyone else does not see it at all.
func TestDashboardShowsEditOnlyToWhoMayEdit(t *testing.T) {
	srv, priv, store := testServer(t)
	mine, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Mine",
		Capacity: 3, CreatedBy: "carol", StartsAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	other, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Other",
		Capacity: 3, CreatedBy: "boss", StartsAt: time.Now().Add(2 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	open := func(userID, permissions string) string {
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
			"type": 3, "guild_id": "g1", "channel_id": "c1",
			"data": {"custom_id": %q},
			"member": {"permissions": %q, "user": {"id": %q, "username": %q}}
		}`, myEventsButtonID, permissions, userID, userID)))
		return rec.Body.String()
	}

	carol := open("carol", "0")
	if !strings.Contains(carol, EditCustomID(mine.ID)) {
		t.Error("carol cannot edit her own event")
	}
	if strings.Contains(carol, EditCustomID(other.ID)) {
		t.Error("carol can edit an event that is not hers")
	}
	admin := open("admin", permissionsWithManageEvents)
	for _, id := range []int64{mine.ID, other.ID} {
		if !strings.Contains(admin, EditCustomID(id)) {
			t.Errorf("Manage Events cannot edit event %d from the dashboard", id)
		}
	}
}

// TestDashboardStaysInsideTheComponentBudget at its cap of seven events, each
// with the widest button row.
func TestDashboardStaysInsideTheComponentBudget(t *testing.T) {
	srv, priv, store := testServer(t)
	for i := 0; i < 10; i++ {
		if _, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1",
			Name: fmt.Sprintf("Event %d", i), Capacity: 5, CreatedBy: "boss",
			StartsAt: time.Now().Add(time.Duration(i+1) * time.Hour).Unix()}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 3, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q},
		"member": {"permissions": %q, "user": {"id": "boss", "username": "boss"}}
	}`, myEventsButtonID, permissionsWithManageEvents)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	components := out["data"].(map[string]any)["components"].([]any)
	if got := countComponents(components); got > 40 {
		t.Errorf("dashboard renders %d components, over Discord's 40", got)
	}
	if !strings.Contains(rec.Body.String(), "Showing 6 of 10") {
		t.Error("the dashboard does not say it truncated")
	}
}
