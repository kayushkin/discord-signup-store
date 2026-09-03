package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// adminInteraction is a press or a submit by somebody with Administrator,
// built from JSON because the Interaction type nests anonymous structs.
func adminInteraction(t *testing.T, customID string, fields map[string]string) *Interaction {
	t.Helper()
	parts := []string{}
	for id, v := range fields {
		parts = append(parts, fmt.Sprintf(`{"custom_id":%q,"value":%q}`, id, v))
	}
	raw := fmt.Sprintf(`{"guild_id":"g1","channel_id":"c1",
		"member":{"permissions":"8","user":{"id":"u-admin","global_name":"Admin"}},
		"data":{"custom_id":%q,"components":[{"components":[%s]}]}}`, customID, strings.Join(parts, ","))
	var in Interaction
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("build interaction: %v", err)
	}
	return &in
}

func replyText(rec *httptest.ResponseRecorder) string {
	var out struct {
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Data.Content
}

// TestTheManagementRowIsEditCloseCancel, and the toggle's label says which
// way it goes.
func TestTheManagementRowIsEditCloseCancel(t *testing.T) {
	labels := func(ev *Event) []string {
		out := []string{}
		for _, b := range managementButtons(ev) {
			out = append(out, b.(map[string]any)["label"].(string))
		}
		return out
	}
	if got := labels(&Event{ID: 1, Status: StatusOpen}); strings.Join(got, ",") != "Edit,Close signups,Cancel" {
		t.Errorf("open row = %v", got)
	}
	if got := labels(&Event{ID: 1, Status: StatusClosed}); strings.Join(got, ",") != "Edit,Reopen signups,Cancel" {
		t.Errorf("closed row = %v", got)
	}
	for _, b := range managementButtons(&Event{ID: 1, Status: StatusOpen}) {
		m := b.(map[string]any)
		if m["label"] == "Cancel" && m["style"] != buttonStyleDanger {
			t.Error("Cancel is not the red button")
		}
	}
}

// TestClosingSignupsIsReversibleAndLogged.
func TestClosingSignupsIsReversibleAndLogged(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games",
		Status: StatusOpen, StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleCloseToggle(rec, adminInteraction(t, CloseCustomID(ev.ID), nil), ev.ID)
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusClosed {
		t.Fatalf("after one press status = %q, want closed", got.Status)
	}
	if !strings.Contains(replyText(rec), "closed") {
		t.Errorf("reply = %q, want it to say closed", replyText(rec))
	}

	rec = httptest.NewRecorder()
	srv.handleCloseToggle(rec, adminInteraction(t, CloseCustomID(ev.ID), nil), ev.ID)
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Fatalf("after a second press status = %q, want open again", got.Status)
	}

	updates, _ := store.EventUpdates(ev.ID)
	if len(updates) != 2 || updates[0].Field != "status" || updates[0].Actor != "u-admin" {
		t.Errorf("history = %+v, want two status rows by u-admin", updates)
	}
}

// TestCancelNeedsTheNameTypedBack: the wrong name does nothing at all, the
// right one cancels and is recorded as that person's doing.
func TestCancelNeedsTheNameTypedBack(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Board game night",
		Status: StatusOpen, StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	modalID := CancelModalCustomID(ev.ID)

	rec := httptest.NewRecorder()
	srv.applyCancelConfirm(rec, adminInteraction(t, modalID, map[string]string{fieldName + "@" + modalID: "Board game"}),
		ev.ID, EventForm{Name: "Board game"})
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Fatalf("a wrong name cancelled the event: status = %q", got.Status)
	}
	if !strings.Contains(replyText(rec), "did not match") {
		t.Errorf("reply = %q, want a refusal", replyText(rec))
	}

	rec = httptest.NewRecorder()
	srv.applyCancelConfirm(rec, adminInteraction(t, modalID, map[string]string{fieldName + "@" + modalID: " board GAME night "}),
		ev.ID, EventForm{Name: " board GAME night "})
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusCancelled {
		t.Fatalf("the right name (case and spaces aside) did not cancel: status = %q", got.Status)
	}
	updates, _ := store.EventUpdates(ev.ID)
	if len(updates) != 1 || updates[0].ToValue != StatusCancelled || updates[0].Actor != "u-admin" {
		t.Errorf("history = %+v, want one cancelled row by u-admin", updates)
	}
}

// TestOnlyAnEditorCanCloseOrCancel.
func TestOnlyAnEditorCanCloseOrCancel(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev, _ := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games",
		Status: StatusOpen, StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	member := adminInteraction(t, CloseCustomID(ev.ID), nil)
	member.Member.Permissions = "0"

	rec := httptest.NewRecorder()
	srv.handleCloseToggle(rec, member, ev.ID)
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Error("a member without Manage Events closed signups")
	}
	rec = httptest.NewRecorder()
	srv.applyCancelConfirm(rec, member, ev.ID, EventForm{Name: "Games"})
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Error("a member without Manage Events cancelled the event")
	}
}
