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

// eventPictureStages are the crowd sizes a scene lays out places for, from
// the event's limit (0 for none). A scene gives each stage a fixed list of
// places; a picture uses the smallest stage that holds everyone going and
// fills its first places, so people move only when the crowd passes into the
// next stage. The last stage is the most people a picture shows.
//
//   - A limit of 6 or fewer: one stage, the limit.
//   - A limit up to 12: half of it, then all of it (12: 6, 12).
//   - More, or none: 5, 10, 20, 40, stopping at the limit (20: 5, 10, 20;
//     30: 5, 10, 20, 30; none: 5, 10, 20, 40).
func eventPictureStages(capacity int) []int {
	switch {
	case capacity > 0 && capacity <= 6:
		return []int{capacity}
	case capacity > 0 && capacity <= 12:
		return []int{(capacity + 1) / 2, capacity}
	}
	var stages []int
	for _, size := range []int{5, 10, 20, 40} {
		if capacity > 0 && size >= capacity {
			return append(stages, capacity)
		}
		stages = append(stages, size)
	}
	return stages
}

// eventPictureMostPeople is the largest stage: the most people a picture
// of the event shows, the first to sign up among those with avatars first.
func eventPictureMostPeople(capacity int) int {
	stages := eventPictureStages(capacity)
	return stages[len(stages)-1]
}

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
	// Request is an organiser asking for the scene again, nil when nobody is.
	Request   *eventSceneRequest `json:"request,omitempty"`
	UpdatedAt int64              `json:"updated_at"`
}

// The ways an organiser can ask for a scene again.
const (
	// EventSceneChange changes the scene there is as the comment says.
	EventSceneChange = "change"
	// EventSceneNew draws a new scene, with the comment to go by.
	EventSceneNew = "new"
)

// eventSceneCommentMaximumCharacters bounds what an organiser writes.
const eventSceneCommentMaximumCharacters = 600

// eventSceneRequest is an organiser asking for the scene again.
type eventSceneRequest struct {
	Kind        string `json:"kind"`
	Comment     string `json:"comment"`
	RequestedAt int64  `json:"requested_at"`
	RequestedBy string `json:"requested_by"`
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
// posed characters, then to moving in a loop, then to crowds with faceless
// stand-ins for the people going without avatars, then to fixed places for
// each stage of the crowd (eventPictureStages).
const eventSceneFormat = "stages"

// eventDetailsSignature names the details a scene is made from, and its
// stages, since a new limit can ask for other places. The date counts only as
// far as its day and time of day, so a repeating event's scene lasts from one
// date to the next.
func eventDetailsSignature(ev Event) string {
	d := sceneDetailsOf(ev)
	when := ""
	if zone, err := time.LoadLocation(d.Timezone); err == nil && d.StartsAt != 0 {
		when = time.Unix(d.StartsAt, 0).In(zone).Format("Monday 15:04")
	}
	return shortHash(eventSceneFormat, d.Name, d.Description, d.Location, when, d.RecurrenceRule,
		fmt.Sprint(eventPictureStages(ev.Capacity)))
}

// eventPictureSignature names what a picture should show: the scene and who
// is in it — each person with an avatar and which drawing of theirs, and how
// many stand-ins, who are all alike. "" for nobody or no scene. So one person
// without an avatar leaving and another joining asks for the same picture,
// and the print saved for it is shown again.
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

// EventPictureSubjects is, for each event, who its picture shows: the people
// going who chose an avatar, in sign-up order, then a faceless stand-in for
// each of the rest going, at most the event's largest stage in all. An event
// with nobody going who chose an avatar has none: a picture of stand-ins alone
// would show nobody.
func (s *Store) EventPictureSubjects(eventIDs []int64) (map[int64][]eventPictureSubject, error) {
	out := map[int64][]eventPictureSubject{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	most := map[int64]int{}
	capacities, err := s.db.Query(`SELECT id, capacity FROM events WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read event limits: %w", err)
	}
	for capacities.Next() {
		var eventID int64
		var capacity int
		if err := capacities.Scan(&eventID, &capacity); err != nil {
			capacities.Close()
			return nil, fmt.Errorf("scan event limit: %w", err)
		}
		most[eventID] = eventPictureMostPeople(capacity)
	}
	capacities.Close()
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
		if len(out[eventID]) < most[eventID] {
			out[eventID] = append(out[eventID], subject)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := s.db.Query(`SELECT event_id, COUNT(*) FROM signups WHERE state = ? AND event_id IN (`+placeholders+`) GROUP BY event_id`,
		append([]any{StateAttending}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("count who is going: %w", err)
	}
	defer counts.Close()
	for counts.Next() {
		var eventID int64
		var going int
		if err := counts.Scan(&eventID, &going); err != nil {
			return nil, fmt.Errorf("scan who is going: %w", err)
		}
		withAvatars := len(out[eventID])
		if withAvatars == 0 {
			continue
		}
		for range min(going, most[eventID]) - withAvatars {
			out[eventID] = append(out[eventID], eventPictureSubject{Format: eventPictureStandIn})
		}
	}
	return out, counts.Err()
}

// eventPictureStandIn is the format of a faceless stand-in for someone going
// without an avatar: the kit draws it (backgroundCharacter), with no code of
// its own.
const eventPictureStandIn = "background"

// EventScenes is each event's scene, where it has one.
func (s *Store) EventScenes(eventIDs []int64) (map[int64]*eventScene, error) {
	out := map[int64]*eventScene{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`SELECT event_id, details_signature, scene_code, failure, failed_details_signature, failed_at,
			request_kind, request_comment, requested_at, requested_by, updated_at
		FROM event_scenes WHERE event_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read event scenes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var scene eventScene
		var request eventSceneRequest
		if err := rows.Scan(&eventID, &scene.DetailsSignature, &scene.SceneCode, &scene.Failure, &scene.FailedDetailsSignature, &scene.FailedAt,
			&request.Kind, &request.Comment, &request.RequestedAt, &request.RequestedBy, &scene.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan event scene: %w", err)
		}
		if request.Kind != "" {
			scene.Request = &request
		}
		out[eventID] = &scene
	}
	return out, rows.Err()
}

// SaveEventScene stores a scene written from the given details. answered
// is the request it was written for, 0 for none: that request is done, and
// one made while it was being written still waits.
func (s *Store) SaveEventScene(eventID int64, detailsSignature, sceneCode string, answered int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The scene this one replaces goes into the history first: scenes, like
	// their pictures, are kept.
	if _, err := tx.Exec(`INSERT INTO event_scene_history (event_id, details_signature, scene_code, written_at, replaced_at)
		SELECT event_id, details_signature, scene_code, updated_at, ? FROM event_scenes WHERE event_id = ? AND scene_code != ''`,
		now(), eventID); err != nil {
		return fmt.Errorf("keep the scene this replaces: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO event_scenes (event_id, details_signature, scene_code, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET details_signature = excluded.details_signature, scene_code = excluded.scene_code,
			failure = '', failed_details_signature = '', failed_at = 0, updated_at = excluded.updated_at`, eventID, detailsSignature, sceneCode, now()); err != nil {
		return fmt.Errorf("store event scene: %w", err)
	}
	if err := clearSceneRequest(tx, eventID, answered); err != nil {
		return err
	}
	return tx.Commit()
}

func clearSceneRequest(tx *sql.Tx, eventID, answered int64) error {
	if answered == 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE event_scenes SET request_kind = '', request_comment = '', requested_at = 0, requested_by = ''
		WHERE event_id = ? AND requested_at = ?`, eventID, answered); err != nil {
		return fmt.Errorf("clear scene request: %w", err)
	}
	return nil
}

// RequestEventScene asks for an event's scene again, replacing any request
// not yet answered.
func (s *Store) RequestEventScene(eventID int64, kind, comment, by string) error {
	_, err := s.db.Exec(`INSERT INTO event_scenes (event_id, details_signature, request_kind, request_comment, requested_at, requested_by, updated_at)
		VALUES (?, '', ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET request_kind = excluded.request_kind, request_comment = excluded.request_comment,
			requested_at = excluded.requested_at, requested_by = excluded.requested_by, failure = ''`,
		eventID, kind, comment, now(), by, now())
	if err != nil {
		return fmt.Errorf("store scene request: %w", err)
	}
	return nil
}

// FailEventScene records a scene that did not come out. A scene already
// there stays, and is printed until a new one comes out. answered is the
// request it was written for, 0 for none: that request is cleared, so the
// page says it failed rather than it being tried again and again.
func (s *Store) FailEventScene(eventID int64, detailsSignature, reason string, answered int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO event_scenes (event_id, details_signature, failure, failed_details_signature, failed_at, updated_at)
		VALUES (?, '', ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET failure = excluded.failure,
			failed_details_signature = excluded.failed_details_signature, failed_at = excluded.failed_at`,
		eventID, reason, detailsSignature, now(), now()); err != nil {
		return fmt.Errorf("record event scene failure: %w", err)
	}
	if err := clearSceneRequest(tx, eventID, answered); err != nil {
		return err
	}
	return tx.Commit()
}

// eventPrints are the pictures saved for one event: every signature
// printed for its current scene, and the one printed last.
type eventPrints struct {
	signatures map[string]bool
	latest     string
}

func (p *eventPrints) has(signature string) bool { return p != nil && p.signatures[signature] }

// EventPicturePrints is the prints saved for each event.
func (s *Store) EventPicturePrints(eventIDs []int64) (map[int64]*eventPrints, error) {
	out := map[int64]*eventPrints{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	placeholders, args := int64Placeholders(eventIDs)
	rows, err := s.db.Query(`SELECT event_id, signature FROM event_picture_prints WHERE event_id IN (`+placeholders+`)
		ORDER BY event_id, painted_at, rowid`, args...)
	if err != nil {
		return nil, fmt.Errorf("read event picture prints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var signature string
		if err := rows.Scan(&eventID, &signature); err != nil {
			return nil, fmt.Errorf("scan event picture print: %w", err)
		}
		if out[eventID] == nil {
			out[eventID] = &eventPrints{signatures: map[string]bool{}}
		}
		out[eventID].signatures[signature] = true
		out[eventID].latest = signature
	}
	return out, rows.Err()
}

// SaveEventPicture keeps a painted picture beside every other painted of
// the event. Nothing is deleted: a picture of an older scene, or of an event
// whose pictures are off, stays, inactive.
func (s *Store) SaveEventPicture(eventID int64, signature string, sceneVersion int64, imageWebP []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO event_picture_prints (event_id, signature, scene_version, image_webp, painted_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(event_id, signature) DO UPDATE SET scene_version = excluded.scene_version, image_webp = excluded.image_webp,
			painted_at = excluded.painted_at`, eventID, signature, sceneVersion, imageWebP, now()); err != nil {
		return fmt.Errorf("store event picture: %w", err)
	}
	return tx.Commit()
}

// EventPicture is the print with the signature, or with "" the one printed
// last.
func (s *Store) EventPicture(eventID int64, signature string) ([]byte, error) {
	var image []byte
	var err error
	if signature == "" {
		err = s.db.QueryRow(`SELECT image_webp FROM event_picture_prints WHERE event_id = ? ORDER BY painted_at DESC, rowid DESC LIMIT 1`,
			eventID).Scan(&image)
	} else {
		err = s.db.QueryRow(`SELECT image_webp FROM event_picture_prints WHERE event_id = ? AND signature = ?`, eventID, signature).Scan(&image)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read event picture: %w", err)
	}
	return image, nil
}

// migrateSinglePicturePerEvent moves the pictures kept one per event, in a
// table named event_pictures until 2026-09-28, into event_picture_prints, and
// drops it. Their scene is not known, so the next print of each forgets them.
func migrateSinglePicturePerEvent(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'event_pictures'`).Scan(&exists); err != nil {
		return fmt.Errorf("look for the old event_pictures table: %w", err)
	}
	if exists == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO event_picture_prints (event_id, signature, scene_version, image_webp, painted_at)
		SELECT event_id, signature, -1, image_webp, painted_at FROM event_pictures`)
	if err != nil {
		return fmt.Errorf("move event pictures: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE event_pictures`); err != nil {
		return fmt.Errorf("drop the old event_pictures table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	moved, _ := result.RowsAffected()
	log.Printf("[discord-signup] moved %d event pictures to event_picture_prints", moved)
	return nil
}

// eventPictureState is, for a set of events, who each picture should show,
// the scene it is set in, and the picture stored.
type eventPictureState struct {
	subjects map[int64][]eventPictureSubject
	scenes   map[int64]*eventScene
	prints   map[int64]*eventPrints
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
	prints, err := s.EventPicturePrints(eventIDs)
	if err != nil {
		return nil, err
	}
	return &eventPictureState{subjects: subjects, scenes: scenes, prints: prints}, nil
}

// currentEventPictures is, for each event with a picture to show, its
// signature — for the page's image address. The print of who is going now
// when one is saved; otherwise the last one painted, which stays up while
// the new one is painted, rather than an empty card. An event nobody going
// has an avatar for shows none, since nothing would ever replace it.
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
	for _, ev := range events {
		prints := state.prints[ev.ID]
		if prints == nil || len(state.subjects[ev.ID]) == 0 || ev.PicturesDisabled {
			continue
		}
		if want := eventPictureSignature(state.scenes[ev.ID], state.subjects[ev.ID]); prints.has(want) {
			out[ev.ID] = want
		} else {
			out[ev.ID] = prints.latest
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
	NeedsScene bool `json:"needs_scene"`
	// UpdateExisting says, with NeedsScene, that the event has a scene
	// already, in SceneCode: change only what no longer fits the details
	// or the scene format, rather than write a new one.
	UpdateExisting bool `json:"update_existing"`
	// Stages are the crowd sizes the scene lays out places for.
	Stages           []int             `json:"stages"`
	Details          eventSceneDetails `json:"details"`
	DetailsSignature string            `json:"details_signature"`
	// Request is an organiser asking for the scene again: with NeedsScene,
	// write it as the request says, and send its requested_at back.
	Request *eventSceneRequest `json:"request,omitempty"`
	// SceneCode is the scene to print the people into, when it needs no new
	// one, or the scene a "change" request changes; Signature is the
	// picture's, to send back with it.
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
		if !IsArchived(ev.Status) && !ev.PicturesDisabled {
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
		item := eventPictureDue{EventID: id, People: people, Details: sceneDetailsOf(ev), DetailsSignature: details,
			Stages: eventPictureStages(ev.Capacity)}
		if scene != nil && scene.Request != nil {
			// An organiser asked: write it now, whatever else holds.
			item.NeedsScene, item.Request = true, scene.Request
			if scene.Request.Kind == EventSceneChange {
				item.SceneCode = scene.SceneCode
			}
			due = append(due, item)
			continue
		}
		if scene == nil || scene.DetailsSignature != details || scene.SceneCode == "" {
			recentlyFailed := scene != nil && scene.FailedDetailsSignature == details &&
				scene.FailedAt > now()-int64(eventSceneRetryAfter/time.Second)
			switch {
			case !recentlyFailed:
				item.NeedsScene = true
				if scene != nil && scene.SceneCode != "" {
					item.UpdateExisting, item.SceneCode = true, scene.SceneCode
				}
				due = append(due, item)
			case scene.SceneCode != "":
				// The new scene failed of late: print the one there is, so
				// who is going stays right meanwhile.
				if want := eventPictureSignature(scene, people); !state.prints[id].has(want) {
					item.SceneCode, item.Signature = scene.SceneCode, want
					due = append(due, item)
				}
			}
			continue
		}
		if want := eventPictureSignature(scene, people); !state.prints[id].has(want) {
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
		// Answered is the requested_at of the request it was written for.
		Answered int64 `json:"answered"`
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
	if err := s.store.SaveEventScene(eventID, body.DetailsSignature, body.SceneCode, body.Answered); err != nil {
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
		Answered         int64  `json:"answered"`
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
	if err := s.store.FailEventScene(eventID, body.DetailsSignature, body.Reason, body.Answered); err != nil {
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
	if err := s.store.SaveEventPicture(eventID, body.Signature, state.scenes[eventID].UpdatedAt, body.ImageWebP); err != nil {
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
	if !mayView || ev.PicturesDisabled {
		http.NotFound(w, r)
		return
	}
	// The page asks for a print by its signature; one no longer saved, or
	// none asked for, is the one printed last.
	image, err := s.store.EventPicture(eventID, r.URL.Query().Get("v"))
	if errors.Is(err, ErrNotFound) {
		image, err = s.store.EventPicture(eventID, "")
	}
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

// handleWebEventSceneRequest is an organiser asking for the event's scene
// again: "change" it as the comment says, or a "new" one with the comment to
// go by. The picture there stays up until the new one is painted.
func (s *Server) handleWebEventSceneRequest(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	kind, comment := r.FormValue("kind"), strings.TrimSpace(r.FormValue("comment"))
	scenes, err := s.store.EventScenes([]int64{ev.ID})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch {
	case ev.PicturesDisabled:
		s.redirectWithNotice(w, r, ev.ID, "The picture is off for this event. Turn it on first.")
		return
	case kind != EventSceneChange && kind != EventSceneNew:
		http.Error(w, "kind is change or new", http.StatusBadRequest)
		return
	case len([]rune(comment)) > eventSceneCommentMaximumCharacters:
		s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("Keep the comment to %d characters.", eventSceneCommentMaximumCharacters))
		return
	case kind == EventSceneChange && comment == "":
		s.redirectWithNotice(w, r, ev.ID, "Say how the scene should change.")
		return
	case kind == EventSceneChange && (scenes[ev.ID] == nil || scenes[ev.ID].SceneCode == ""):
		s.redirectWithNotice(w, r, ev.ID, "There is no scene to change yet. Ask for a new one instead.")
		return
	}
	if err := s.store.RequestEventScene(ev.ID, kind, comment, "web:"+session.DiscordUserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[discord-signup] %s scene for event %d asked for by web:%s", kind, ev.ID, session.DiscordUserID)
	s.redirectWithNotice(w, r, ev.ID, "The scene is being drawn again. The picture here stays up until the new one is ready, in about ten minutes.")
}

// eventPicturePanel is what the event page shows of its picture.
type eventPicturePanel struct {
	// Signature addresses the picture to show, "" for none yet.
	Signature string
	// People is how many going have an avatar: none, and there is no picture.
	People int
	// Scene is the event's scene, with any request waiting and the last
	// failure; nil before one is written.
	Scene *eventScene
}

func (s *Server) eventPicturePanelOf(ev *Event) (*eventPicturePanel, error) {
	state, err := s.store.eventPictureStateOf([]int64{ev.ID})
	if err != nil {
		return nil, err
	}
	shown, err := s.store.currentEventPictures([]Event{*ev})
	if err != nil {
		return nil, err
	}
	return &eventPicturePanel{Signature: shown[ev.ID], People: len(state.subjects[ev.ID]), Scene: state.scenes[ev.ID]}, nil
}
