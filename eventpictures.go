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
	"time"
)

// Event pictures: the people going to an event, drawn into a scene made for
// that event, for the home page. Two steps, both done by
// cmd/discord-event-picture-painter on the scheduler:
//
//   - The scene. A model reads the event's name, description, place and time,
//     decides what the picture should be, and writes it as drawing code in
//     art/kit.js's form, with a place for each person. It is written again only
//     when those details change, so its cost is paid once per event.
//   - The picture. The scene is printed with each person's chosen avatar in
//     their place, by art/render-event-picture.mjs. No model: someone joining
//     or leaving costs a few seconds of Chrome.

// eventPictureMaximumPeople is the most people one picture shows: the first
// to sign up among those going with an avatar.
const eventPictureMaximumPeople = 12

// eventPictureMaximumBytes bounds a painted picture handed back: a loop of
// frames, 700 KB for two seconds of the haunted house on 2026-09-28.
const eventPictureMaximumBytes = 6 << 20

// eventSceneMaximumBytes bounds a scene's code handed back.
const eventSceneMaximumBytes = 256 << 10

// eventSceneRetryAfter is how long a scene that did not come out waits before
// it is tried again for the same details.
const eventSceneRetryAfter = time.Hour

// eventPictureSubject is one person in a picture.
type eventPictureSubject struct {
	DiscordUserID string `json:"discord_user_id"`
	// Format says whether DrawingCode is a poseable character or a
	// portrait, which a scene can only show as it is.
	Format      string `json:"format"`
	DrawingCode string `json:"drawing_code"`
	// drawingID is which of their drawings; it changes when they choose
	// another.
	drawingID int64
}

// eventScene is the scene an event's picture is set in.
type eventScene struct {
	DetailsSignature string `json:"details_signature"`
	SceneCode        string `json:"scene_code"`
	Failure          string `json:"failure,omitempty"`
	// FailedDetailsSignature names the details the last failed scene was
	// written from.
	FailedDetailsSignature string `json:"failed_details_signature,omitempty"`
	FailedAt               int64  `json:"failed_at"`
	UpdatedAt              int64  `json:"updated_at"`
}

// eventSceneDetails are what a scene is made from.
type eventSceneDetails struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Location       string `json:"location"`
	StartsAt       int64  `json:"starts_at"`
	EndsAt         int64  `json:"ends_at"`
	Timezone       string `json:"timezone"`
	RecurrenceRule string `json:"recurrence_rule"`
}

func sceneDetailsOf(ev Event) eventSceneDetails {
	return eventSceneDetails{Name: ev.Name, Description: ev.Description, Location: ev.Location,
		StartsAt: ev.StartsAt, EndsAt: ev.EndsAt, Timezone: ev.Timezone, RecurrenceRule: ev.RecurrenceRule}
}

func shortHash(parts ...string) string {
	hash := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(hash, "%d:%s\n", len(p), p)
	}
	return hex.EncodeToString(hash.Sum(nil))[:24]
}

// eventSceneFormat is the version of art/SCENE.md scenes are written to. It
// is part of every details signature, so a new version asks every event for a
// new scene: on 2026-09-28 scenes went from placing round portraits to casting
// posed characters, and then to moving in a loop.
const eventSceneFormat = "cast-motion"

// eventDetailsSignature names the details a scene is made from. The date
// counts only as far as its day and time of day, so a repeating event's
// scene lasts from one date to the next.
func eventDetailsSignature(ev Event) string {
	d := sceneDetailsOf(ev)
	when := ""
	if zone, err := time.LoadLocation(d.Timezone); err == nil && d.StartsAt != 0 {
		when = time.Unix(d.StartsAt, 0).In(zone).Format("Monday 15:04")
	}
	return shortHash(eventSceneFormat, d.Name, d.Description, d.Location, when, d.RecurrenceRule)
}

// eventPictureSignature names what a picture should show: the scene and who
// is in it. "" for nobody or no scene. A stored picture whose signature
// differs is repainted, and shown until then.
func eventPictureSignature(scene *eventScene, subjects []eventPictureSubject) string {
	if len(subjects) == 0 || scene == nil || scene.SceneCode == "" {
		return ""
	}
	parts := []string{scene.DetailsSignature, strconv.FormatInt(scene.UpdatedAt, 10)}
	for _, s := range subjects {
		parts = append(parts, s.DiscordUserID+":"+strconv.FormatInt(s.drawingID, 10))
	}
	return shortHash(parts...)
}

func int64Placeholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// EventPictureSubjects is, for each event, the people going who chose an
// avatar, in sign-up order, at most eventPictureMaximumPeople.
func (s *Store) EventPictureSubjects(eventIDs []int64) (map[int64][]eventPictureSubject, error) {
	out := map[int64][]eventPictureSubject{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`
		SELECT s.event_id, s.discord_user_id, d.format, d.drawing_code, d.id
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
		if err := rows.Scan(&eventID, &subject.DiscordUserID, &subject.Format, &subject.DrawingCode, &subject.drawingID); err != nil {
			return nil, fmt.Errorf("scan event picture subject: %w", err)
		}
		if len(out[eventID]) < eventPictureMaximumPeople {
			out[eventID] = append(out[eventID], subject)
		}
	}
	return out, rows.Err()
}

// EventScenes is each event's scene, where it has one.
func (s *Store) EventScenes(eventIDs []int64) (map[int64]*eventScene, error) {
	out := map[int64]*eventScene{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`SELECT event_id, details_signature, scene_code, failure, failed_details_signature, failed_at, updated_at
		FROM event_scenes WHERE event_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read event scenes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var scene eventScene
		if err := rows.Scan(&eventID, &scene.DetailsSignature, &scene.SceneCode, &scene.Failure, &scene.FailedDetailsSignature, &scene.FailedAt, &scene.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan event scene: %w", err)
		}
		out[eventID] = &scene
	}
	return out, rows.Err()
}

// SaveEventScene stores a scene written from the given details.
func (s *Store) SaveEventScene(eventID int64, detailsSignature, sceneCode string) error {
	_, err := s.db.Exec(`INSERT INTO event_scenes (event_id, details_signature, scene_code, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET details_signature = excluded.details_signature, scene_code = excluded.scene_code,
			failure = '', failed_details_signature = '', failed_at = 0, updated_at = excluded.updated_at`, eventID, detailsSignature, sceneCode, now())
	if err != nil {
		return fmt.Errorf("store event scene: %w", err)
	}
	return nil
}

// FailEventScene records a scene that did not come out. A scene already
// there stays, and is printed until a new one comes out.
func (s *Store) FailEventScene(eventID int64, detailsSignature, reason string) error {
	_, err := s.db.Exec(`INSERT INTO event_scenes (event_id, details_signature, failure, failed_details_signature, failed_at, updated_at)
		VALUES (?, '', ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET failure = excluded.failure,
			failed_details_signature = excluded.failed_details_signature, failed_at = excluded.failed_at`,
		eventID, reason, detailsSignature, now(), now())
	if err != nil {
		return fmt.Errorf("record event scene failure: %w", err)
	}
	return nil
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

// eventPictureState is, for a set of events, who each picture should show,
// the scene it is set in, and the picture stored.
type eventPictureState struct {
	subjects map[int64][]eventPictureSubject
	scenes   map[int64]*eventScene
	stored   map[int64]string
}

func (s *Store) eventPictureStateOf(eventIDs []int64) (*eventPictureState, error) {
	subjects, err := s.EventPictureSubjects(eventIDs)
	if err != nil {
		return nil, err
	}
	scenes, err := s.EventScenes(eventIDs)
	if err != nil {
		return nil, err
	}
	stored, err := s.EventPictureSignatures(eventIDs)
	if err != nil {
		return nil, err
	}
	return &eventPictureState{subjects: subjects, scenes: scenes, stored: stored}, nil
}

// currentEventPictures is, for each event with a picture to show, its
// signature — for the page's image address, so a repainted picture is
// fetched again. The last picture painted stays up while a new one is
// painted, which takes minutes: someone who joins sees the old one until
// theirs replaces it, rather than an empty card. An event nobody going has
// an avatar for shows none, since nothing would ever replace it.
func (s *Store) currentEventPictures(events []Event) (map[int64]string, error) {
	ids := make([]int64, len(events))
	for i, ev := range events {
		ids[i] = ev.ID
	}
	state, err := s.eventPictureStateOf(ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, id := range ids {
		if stored := state.stored[id]; stored != "" && len(state.subjects[id]) > 0 {
			out[id] = stored
		}
	}
	return out, nil
}

// eventPictureDue is one event whose picture the painter should paint, and
// whose scene it should write first when Scene says so.
type eventPictureDue struct {
	EventID int64                 `json:"event_id"`
	People  []eventPictureSubject `json:"people"`
	// NeedsScene says the scene must be written, from Details, before the
	// picture can be printed; DetailsSignature goes back with it.
	NeedsScene       bool              `json:"needs_scene"`
	Details          eventSceneDetails `json:"details"`
	DetailsSignature string            `json:"details_signature"`
	// SceneCode is the scene to print the people into, when it needs no new
	// one; Signature is the picture's, to send back with it.
	SceneCode string `json:"scene_code,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// handleEventPicturesDue lists every event not yet over, with someone going
// who chose an avatar, whose picture does not show its scene and who is going
// now — and whether its scene must be written first.
func (s *Server) handleEventPicturesDue(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents("", "", 500)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	live := map[int64]Event{}
	var ids []int64
	for _, ev := range events {
		if !IsArchived(ev.Status) {
			live[ev.ID] = ev
			ids = append(ids, ev.ID)
		}
	}
	state, err := s.store.eventPictureStateOf(ids)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	due := []eventPictureDue{}
	for _, id := range ids {
		people := state.subjects[id]
		if len(people) == 0 {
			continue
		}
		ev, scene := live[id], state.scenes[id]
		details := eventDetailsSignature(ev)
		item := eventPictureDue{EventID: id, People: people, Details: sceneDetailsOf(ev), DetailsSignature: details}
		if scene == nil || scene.DetailsSignature != details || scene.SceneCode == "" {
			recentlyFailed := scene != nil && scene.FailedDetailsSignature == details &&
				scene.FailedAt > now()-int64(eventSceneRetryAfter/time.Second)
			switch {
			case !recentlyFailed:
				item.NeedsScene = true
				due = append(due, item)
			case scene.SceneCode != "":
				// The new scene failed of late: print the one there is, so
				// who is going stays right meanwhile.
				if want := eventPictureSignature(scene, people); state.stored[id] != want {
					item.SceneCode, item.Signature = scene.SceneCode, want
					due = append(due, item)
				}
			}
			continue
		}
		if want := eventPictureSignature(scene, people); state.stored[id] != want {
			item.SceneCode, item.Signature = scene.SceneCode, want
			due = append(due, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pictures": due})
}

// handleSaveEventScene takes a scene written for an event. One written from
// details that have changed since is refused, and the next run writes it
// again. It answers the picture signature to print it with.
func (s *Server) handleSaveEventScene(w http.ResponseWriter, r *http.Request) {
	eventID, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad event id"})
		return
	}
	var body struct {
		DetailsSignature string `json:"details_signature"`
		SceneCode        string `json:"scene_code"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, eventSceneMaximumBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.SceneCode) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"details_signature\", \"scene_code\"} is required"})
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if current := eventDetailsSignature(*ev); body.DetailsSignature != current {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the event's details have changed since this scene was written"})
		return
	}
	if err := s.store.SaveEventScene(eventID, body.DetailsSignature, body.SceneCode); err != nil {
		writeStoreError(w, err)
		return
	}
	state, err := s.store.eventPictureStateOf([]int64{eventID})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"signature": eventPictureSignature(state.scenes[eventID], state.subjects[eventID])})
}

// handleEventSceneFailed records a scene that did not come out.
func (s *Server) handleEventSceneFailed(w http.ResponseWriter, r *http.Request) {
	eventID, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad event id"})
		return
	}
	var body struct {
		DetailsSignature string `json:"details_signature"`
		Reason           string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" || body.DetailsSignature == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"details_signature\", \"reason\"} is required"})
		return
	}
	if _, err := s.store.GetEvent(eventID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.FailEventScene(eventID, body.DetailsSignature, body.Reason); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSaveEventPicture takes a painted picture. One painted for a scene or
// roster that has changed since is refused, and the next run paints it again.
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
	state, err := s.store.eventPictureStateOf([]int64{eventID})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	current := eventPictureSignature(state.scenes[eventID], state.subjects[eventID])
	if current == "" || body.Signature != current {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the scene or who is going has changed since this was painted", "signature": current})
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
