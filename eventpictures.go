package discordsignup

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Event pictures: the people going to an event, drawn together from their
// chosen avatars, for the home page. The painting needs a browser, so
// cmd/discord-event-picture-painter does it on the scheduler: it asks which
// events' pictures are out of date, paints each from the avatars' drawing
// code with art/render-event-picture.mjs, and hands the print back. No model
// is involved, so a roster change costs a few seconds of Chrome and nothing
// else.

// eventPictureMaximumPeople is the most people one picture shows: the first
// to sign up among those going with an avatar.
const eventPictureMaximumPeople = 12

// eventPictureMaximumBytes bounds a painted picture handed back.
const eventPictureMaximumBytes = 3 << 20

// eventPictureSubject is one person in a picture.
type eventPictureSubject struct {
	DiscordUserID string `json:"discord_user_id"`
	DrawingCode   string `json:"drawing_code"`
	// drawingID is which of their drawings; it changes when they choose
	// another.
	drawingID int64
}

// eventPictureSignature names who a picture shows and which drawing of each.
// "" for nobody, which is no picture.
func eventPictureSignature(subjects []eventPictureSubject) string {
	if len(subjects) == 0 {
		return ""
	}
	hash := sha256.New()
	for _, s := range subjects {
		fmt.Fprintf(hash, "%s:%d\n", s.DiscordUserID, s.drawingID)
	}
	return hex.EncodeToString(hash.Sum(nil))[:24]
}

func int64Placeholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// EventPictureSubjects is, for each event, the people going who have an
// chosen avatar, in sign-up order, at most eventPictureMaximumPeople.
func (s *Store) EventPictureSubjects(eventIDs []int64) (map[int64][]eventPictureSubject, error) {
	out := map[int64][]eventPictureSubject{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`
		SELECT s.event_id, s.discord_user_id, d.drawing_code, d.id
		FROM signups s
		JOIN avatar_people p ON p.discord_user_id = s.discord_user_id
		JOIN avatar_drawings d ON d.id = p.chosen_drawing_id
		WHERE s.state = ? AND s.event_id IN (`+placeholders+`)
		ORDER BY s.event_id, s.signed_up_at, s.id`, append([]any{StateAttending}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read who an event picture shows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var subject eventPictureSubject
		if err := rows.Scan(&eventID, &subject.DiscordUserID, &subject.DrawingCode, &subject.drawingID); err != nil {
			return nil, fmt.Errorf("scan event picture subject: %w", err)
		}
		if len(out[eventID]) < eventPictureMaximumPeople {
			out[eventID] = append(out[eventID], subject)
		}
	}
	return out, rows.Err()
}

// EventPictureSignatures is the signature of each stored picture.
func (s *Store) EventPictureSignatures(eventIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`SELECT event_id, signature FROM event_pictures WHERE event_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read event picture signatures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var signature string
		if err := rows.Scan(&eventID, &signature); err != nil {
			return nil, fmt.Errorf("scan event picture signature: %w", err)
		}
		out[eventID] = signature
	}
	return out, rows.Err()
}

// SaveEventPicture stores a painted picture, replacing the last.
func (s *Store) SaveEventPicture(eventID int64, signature string, imageWebP []byte) error {
	_, err := s.db.Exec(`INSERT INTO event_pictures (event_id, signature, image_webp, painted_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET signature = excluded.signature, image_webp = excluded.image_webp,
			painted_at = excluded.painted_at`, eventID, signature, imageWebP, now())
	if err != nil {
		return fmt.Errorf("store event picture: %w", err)
	}
	return nil
}

// EventPicture is the stored picture and its signature.
func (s *Store) EventPicture(eventID int64) ([]byte, string, error) {
	var image []byte
	var signature string
	err := s.db.QueryRow(`SELECT image_webp, signature FROM event_pictures WHERE event_id = ?`, eventID).Scan(&image, &signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read event picture: %w", err)
	}
	return image, signature, nil
}

// currentEventPictures is, for each event whose stored picture shows who is
// going now, that picture's signature — for the page's image address, so a
// repainted picture is fetched again. An event missing here shows none.
func (s *Store) currentEventPictures(events []Event) (map[int64]string, error) {
	ids := make([]int64, len(events))
	for i, ev := range events {
		ids[i] = ev.ID
	}
	subjects, err := s.EventPictureSubjects(ids)
	if err != nil {
		return nil, err
	}
	stored, err := s.EventPictureSignatures(ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, id := range ids {
		if want := eventPictureSignature(subjects[id]); want != "" && stored[id] == want {
			out[id] = want
		}
	}
	return out, nil
}

// eventPictureDue is one picture the painter should paint.
type eventPictureDue struct {
	EventID   int64                 `json:"event_id"`
	Signature string                `json:"signature"`
	People    []eventPictureSubject `json:"people"`
}

// handleEventPicturesDue lists every event not yet over whose picture does
// not show who is going now, with the drawings to paint it from.
func (s *Server) handleEventPicturesDue(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents("", "", 500)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var live []int64
	for _, ev := range events {
		if !IsArchived(ev.Status) {
			live = append(live, ev.ID)
		}
	}
	subjects, err := s.store.EventPictureSubjects(live)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	stored, err := s.store.EventPictureSignatures(live)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	due := []eventPictureDue{}
	for _, id := range live {
		if want := eventPictureSignature(subjects[id]); want != "" && stored[id] != want {
			due = append(due, eventPictureDue{EventID: id, Signature: want, People: subjects[id]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pictures": due})
}

// handleSaveEventPicture takes a painted picture. One painted for a roster
// that has changed since is refused, and the next run paints it again.
func (s *Server) handleSaveEventPicture(w http.ResponseWriter, r *http.Request) {
	eventID, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad event id"})
		return
	}
	var body struct {
		Signature string `json:"signature"`
		ImageWebP []byte `json:"image_webp"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*eventPictureMaximumBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	if !isWebP(body.ImageWebP) || len(body.ImageWebP) > eventPictureMaximumBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("image_webp must be a WebP image of at most %d bytes", eventPictureMaximumBytes)})
		return
	}
	if _, err := s.store.GetEvent(eventID); err != nil {
		writeStoreError(w, err)
		return
	}
	subjects, err := s.store.EventPictureSubjects([]int64{eventID})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if current := eventPictureSignature(subjects[eventID]); body.Signature != current {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "who is going has changed since this was painted", "signature": current})
		return
	}
	if err := s.store.SaveEventPicture(eventID, body.Signature, body.ImageWebP); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWebEventPicture serves an event's picture at /events/{id}/picture.webp
// to anyone signed in who is in the event's server, as the home page lists it.
func (s *Server) handleWebEventPicture(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mayView, err := s.mayViewGuild(session, ev.GuildID)
	if err != nil {
		log.Printf("[discord-signup] may %s see server %s: %v", session.DiscordUserID, ev.GuildID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !mayView {
		http.NotFound(w, r)
		return
	}
	image, _, err := s.store.EventPicture(eventID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The address carries the signature, so a new picture is a new address.
	writeWebPImage(w, image, "private, max-age=86400")
}
