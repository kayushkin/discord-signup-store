package discordsignup

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAttendanceIsRecordedChangedAndRemoved(t *testing.T) {
	srv, _, store := testServer(t)
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	past := time.Now().Add(-48 * time.Hour).Unix()
	event, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Bowling", StartsAt: past})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	future, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Later", StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for name, input := range map[string]AttendanceInput{
		"no basis":            {MarkedBy: "agent"},
		"unknown basis":       {Basis: "vibes", MarkedBy: "agent"},
		"no one marking":      {Basis: AttendanceBasisRoster},
		"evidence not named":  {Basis: AttendanceBasisEvidence, MarkedBy: "agent"},
		"confirmed not named": {Basis: AttendanceBasisConfirmed, MarkedBy: "operator"},
	} {
		if _, err := store.RecordAttendance(event.ID, "u1", input); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if _, err := store.RecordAttendance(future.ID, "u1", AttendanceInput{Basis: AttendanceBasisRoster, MarkedBy: "agent"}); err == nil {
		t.Error("attendance for a date still to come was taken")
	}

	first, err := store.RecordAttendance(event.ID, "u1", AttendanceInput{Basis: AttendanceBasisRoster, MarkedBy: "agent", DisplayName: "Ann"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref != AttendanceRef(first.ID) || first.EventRef != event.Ref || first.OccurrenceStartsAt != past {
		t.Errorf("first = %+v", first)
	}
	changed, err := store.RecordAttendance(event.ID, "u1", AttendanceInput{Basis: AttendanceBasisEvidence, Evidence: "photo_000001", MarkedBy: "agent"})
	if err != nil || changed.ID != first.ID || changed.Basis != AttendanceBasisEvidence || changed.DisplayName != "Ann" {
		t.Fatalf("changed = %+v %v", changed, err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/discord-attendance/"+first.Ref, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"photo_000001"`) {
		t.Errorf("by ref = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/events/"+strconv.FormatInt(event.ID, 10)+"/attendance/u2",
		strings.NewReader(`{"basis":"roster","marked_by":"agent","display_name":"Bo"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := store.ListAttendance(event.ID, ""); len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/discord-attendance/"+first.Ref, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("remove with no actor = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/discord-attendance/"+first.Ref+"?actor=operator", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := store.ListAttendance(0, "u1"); len(list) != 0 {
		t.Errorf("a removed record is still listed: %+v", list)
	}
	var updates int
	store.db.QueryRow(`SELECT COUNT(*) FROM attendance_updates WHERE attendance_id = ?`, first.ID).Scan(&updates)
	if updates != 2 {
		t.Errorf("updates kept = %d, want 2", updates)
	}
	back, err := store.RecordAttendance(event.ID, "u1", AttendanceInput{Basis: AttendanceBasisConfirmed, Evidence: "she said so", MarkedBy: "operator"})
	if err != nil || back.ID != first.ID || back.RemovedAt != 0 {
		t.Errorf("record again = %+v %v", back, err)
	}
}
