package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A roster says who signed up; attendance says who went. One row is one
// person at one date of one event, and a row exists only for someone who
// went. The roster never writes attendance by itself: an organizer, the
// operator or an agent records it after the event, saying what it rests on.

// What an attendance row rests on.
const (
	// AttendanceBasisRoster: they were on the roster as going when the event
	// started, and nothing says they stayed away.
	AttendanceBasisRoster = "roster"
	// AttendanceBasisEvidence: something shows they were there — a photo, a
	// phone's location, a message — named in evidence.
	AttendanceBasisEvidence = "evidence"
	// AttendanceBasisConfirmed: the person, or an organizer who was there,
	// said so.
	AttendanceBasisConfirmed = "confirmed"
)

var attendanceBases = []string{AttendanceBasisRoster, AttendanceBasisEvidence, AttendanceBasisConfirmed}

const attendanceRefPrefix = "discord_attendance_"

var attendanceRefPattern = regexp.MustCompile(`^discord_attendance_(\d{6,})$`)

// AttendanceRef writes an attendance id as other stores cite it.
func AttendanceRef(id int64) string { return fmt.Sprintf("%s%06d", attendanceRefPrefix, id) }

// Attendance is one person who went to one date of an event.
type Attendance struct {
	ID                 int64  `json:"id"`
	Ref                string `json:"ref"`
	EventID            int64  `json:"event_id"`
	EventRef           string `json:"event_ref"`
	OccurrenceStartsAt int64  `json:"occurrence_starts_at"`
	DiscordUserID      string `json:"discord_user_id"`
	DisplayName        string `json:"display_name"`
	Basis              string `json:"basis"`
	Evidence           string `json:"evidence"`
	MarkedBy           string `json:"marked_by"`
	MarkedAt           int64  `json:"marked_at"`
	UpdatedAt          int64  `json:"updated_at"`
	RemovedAt          int64  `json:"removed_at"`
}

// AttendanceInput records that someone went.
type AttendanceInput struct {
	OccurrenceStartsAt int64  `json:"occurrence_starts_at"`
	DisplayName        string `json:"display_name"`
	Basis              string `json:"basis"`
	Evidence           string `json:"evidence"`
	MarkedBy           string `json:"marked_by"`
}

const attendanceColumns = `id, event_id, occurrence_starts_at, discord_user_id, display_name, basis, evidence, marked_by, marked_at, updated_at, removed_at`

func scanAttendance(scanner interface{ Scan(...any) error }) (Attendance, error) {
	var row Attendance
	err := scanner.Scan(&row.ID, &row.EventID, &row.OccurrenceStartsAt, &row.DiscordUserID, &row.DisplayName,
		&row.Basis, &row.Evidence, &row.MarkedBy, &row.MarkedAt, &row.UpdatedAt, &row.RemovedAt)
	row.Ref = AttendanceRef(row.ID)
	row.EventRef = EventRef(row.EventID)
	return row, err
}

func validBasis(basis string) bool {
	for _, known := range attendanceBases {
		if basis == known {
			return true
		}
	}
	return false
}

// RecordAttendance says a person went to one date of an event, or changes
// what an earlier record rests on. A removed record comes back. Every change
// keeps the row as it was in attendance_updates.
func (s *Store) RecordAttendance(eventID int64, discordUserID string, input AttendanceInput) (Attendance, error) {
	discordUserID = strings.TrimSpace(discordUserID)
	input.MarkedBy = strings.TrimSpace(input.MarkedBy)
	input.Evidence = strings.TrimSpace(input.Evidence)
	switch {
	case discordUserID == "":
		return Attendance{}, fmt.Errorf("%w: a Discord user id is required", ErrInvalidEvent)
	case !validBasis(input.Basis):
		return Attendance{}, fmt.Errorf("%w: basis must be one of %v", ErrInvalidEvent, attendanceBases)
	case input.MarkedBy == "":
		return Attendance{}, fmt.Errorf("%w: marked_by is required: who is recording this", ErrInvalidEvent)
	case input.Basis != AttendanceBasisRoster && input.Evidence == "":
		return Attendance{}, fmt.Errorf("%w: a %s record needs evidence saying what shows it", ErrInvalidEvent, input.Basis)
	}
	event, err := s.GetEvent(eventID)
	if err != nil {
		return Attendance{}, err
	}
	if input.OccurrenceStartsAt == 0 {
		if event.RecurrenceRule != "" {
			return Attendance{}, fmt.Errorf("%w: %s repeats, so occurrence_starts_at must name the date", ErrInvalidEvent, event.Ref)
		}
		input.OccurrenceStartsAt = event.StartsAt
	}
	if input.OccurrenceStartsAt > now() {
		return Attendance{}, fmt.Errorf("%w: that date has not happened yet", ErrInvalidEvent)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Attendance{}, err
	}
	defer tx.Rollback()
	ts := now()
	existing, err := scanAttendance(tx.QueryRow(`SELECT `+attendanceColumns+` FROM attendance
		WHERE event_id = ? AND occurrence_starts_at = ? AND discord_user_id = ?`, eventID, input.OccurrenceStartsAt, discordUserID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		result, err := tx.Exec(`INSERT INTO attendance (event_id, occurrence_starts_at, discord_user_id, display_name, basis, evidence, marked_by, marked_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, eventID, input.OccurrenceStartsAt, discordUserID, input.DisplayName,
			input.Basis, input.Evidence, input.MarkedBy, ts, ts)
		if err != nil {
			return Attendance{}, fmt.Errorf("insert attendance: %w", err)
		}
		id, _ := result.LastInsertId()
		if err := tx.Commit(); err != nil {
			return Attendance{}, err
		}
		return s.GetAttendance(id)
	case err != nil:
		return Attendance{}, err
	}
	if err := keepAttendanceUpdate(tx, existing, "changed", input.MarkedBy, ts); err != nil {
		return Attendance{}, err
	}
	displayName := input.DisplayName
	if displayName == "" {
		displayName = existing.DisplayName
	}
	if _, err := tx.Exec(`UPDATE attendance SET display_name = ?, basis = ?, evidence = ?, marked_by = ?, updated_at = ?, removed_at = 0 WHERE id = ?`,
		displayName, input.Basis, input.Evidence, input.MarkedBy, ts, existing.ID); err != nil {
		return Attendance{}, err
	}
	if err := tx.Commit(); err != nil {
		return Attendance{}, err
	}
	return s.GetAttendance(existing.ID)
}

func keepAttendanceUpdate(tx *sql.Tx, before Attendance, action, actor string, at int64) error {
	snapshot, err := json.Marshal(before)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO attendance_updates (attendance_id, action, row_json, actor, at) VALUES (?, ?, ?, ?, ?)`,
		before.ID, action, string(snapshot), actor, at)
	return err
}

// RemoveAttendance says a record was wrong: the person did not go. The row is
// kept with removed_at set, and lists leave it out.
func (s *Store) RemoveAttendance(id int64, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: say who is removing it (?actor=)", ErrInvalidEvent)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	existing, err := scanAttendance(tx.QueryRow(`SELECT `+attendanceColumns+` FROM attendance WHERE id = ? AND removed_at = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, AttendanceRef(id))
	}
	if err != nil {
		return err
	}
	ts := now()
	if err := keepAttendanceUpdate(tx, existing, "removed", actor, ts); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE attendance SET removed_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAttendance reads one record, removed ones included.
func (s *Store) GetAttendance(id int64) (Attendance, error) {
	row, err := scanAttendance(s.db.QueryRow(`SELECT `+attendanceColumns+` FROM attendance WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row, fmt.Errorf("%w: %s", ErrNotFound, AttendanceRef(id))
	}
	return row, err
}

// ListAttendance lists live records, by event and date then the order
// recorded. Either filter may be empty.
func (s *Store) ListAttendance(eventID int64, discordUserID string) ([]Attendance, error) {
	query := `SELECT ` + attendanceColumns + ` FROM attendance WHERE removed_at = 0`
	var args []any
	if eventID != 0 {
		query += ` AND event_id = ?`
		args = append(args, eventID)
	}
	if discordUserID != "" {
		query += ` AND discord_user_id = ?`
		args = append(args, discordUserID)
	}
	rows, err := s.db.Query(query+` ORDER BY occurrence_starts_at, event_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Attendance{}
	for rows.Next() {
		row, err := scanAttendance(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, row)
	}
	return list, rows.Err()
}

func (s *Server) handleListAttendance(w http.ResponseWriter, r *http.Request) {
	var eventID int64
	if value := r.URL.Query().Get("event_id"); value != "" {
		var err error
		if eventID, err = strconv.ParseInt(value, 10, 64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_id must be an integer"})
			return
		}
	}
	list, err := s.store.ListAttendance(eventID, r.URL.Query().Get("discord_user_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attendance": list, "bases": attendanceBases})
}

func (s *Server) handleRecordAttendance(w http.ResponseWriter, r *http.Request) {
	eventID, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	var input AttendanceInput
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON: " + err.Error()})
		return
	}
	row, err := s.store.RecordAttendance(eventID, r.PathValue("userID"), input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func attendanceIDFromRef(ref string) (int64, error) {
	match := attendanceRefPattern.FindStringSubmatch(ref)
	if match == nil {
		return 0, fmt.Errorf("%q is not an attendance ref (%s000001)", ref, attendanceRefPrefix)
	}
	return strconv.ParseInt(match[1], 10, 64)
}

func (s *Server) handleGetAttendanceByRef(w http.ResponseWriter, r *http.Request) {
	id, err := attendanceIDFromRef(r.PathValue("ref"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	row, err := s.store.GetAttendance(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleRemoveAttendance(w http.ResponseWriter, r *http.Request) {
	id, err := attendanceIDFromRef(r.PathValue("ref"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.RemoveAttendance(id, r.URL.Query().Get("actor")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
