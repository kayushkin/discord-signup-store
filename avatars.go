package discordsignup

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Avatars: drawings of a person in the site's print style, made from a photo
// they upload. Every drawing is kept in their gallery, and they choose which
// one shows beside their name on the event pages, or none. They get a new
// drawing by asking for one — from a new photo, again from the photo they
// uploaded before, or by changing a drawing they have — each with a comment on
// what they want it to look like. cmd/discord-avatar-drawer, which the
// scheduler runs, takes each request, has a model draw it in art/kit.js's
// form, prints it and hands back the code and the print. Only the person can
// ask, choose or delete. Their photo is kept in file-store until they delete
// it, remove everything, or upload another.

// The kinds of drawing a person can ask for.
const (
	// AvatarRequestNewPhoto draws from a photo uploaded with the request.
	AvatarRequestNewPhoto = "new_photo"
	// AvatarRequestRedrawPhoto draws again from the photo already kept.
	AvatarRequestRedrawPhoto = "redraw_photo"
	// AvatarRequestEditDrawing changes one of their drawings as the comment
	// asks.
	AvatarRequestEditDrawing = "edit_drawing"
)

var avatarRequestKinds = map[string]bool{AvatarRequestNewPhoto: true, AvatarRequestRedrawPhoto: true, AvatarRequestEditDrawing: true}

// The states a request moves through.
const (
	AvatarRequestWaiting = "waiting"
	AvatarRequestDrawing = "drawing"
	AvatarRequestDone    = "done"
	AvatarRequestFailed  = "failed"
)

// ErrAvatarState is an avatar asked to do something its state does not
// allow: a second request while one is under way, saving a drawing nobody
// started, drawing again from a photo that was deleted.
var ErrAvatarState = errors.New("the avatar is not in a state that allows this")

// avatarDrawingsPerDay is how many drawings one person may ask for in a day.
// Each costs a model's time on the host.
const avatarDrawingsPerDay = 6

// avatarRequestStaleAfter is how long a drawing may be under way before the
// drawer takes it again. The scheduler kills a drawer run after 30 minutes,
// so a drawing older than this belongs to a run that is gone.
const avatarRequestStaleAfter = time.Hour

// avatarPhotoMaximumBytes bounds an upload. A phone's photo is 2–6 MB.
const avatarPhotoMaximumBytes = 15 << 20

// avatarImageMaximumBytes bounds a printed drawing handed back by the drawer.
const avatarImageMaximumBytes = 2 << 20

// avatarCommentMaximumCharacters bounds what a person writes with a request.
const avatarCommentMaximumCharacters = 600

// avatarPhotoTypes are the photo types a model can read, by what the bytes
// are rather than what the browser said.
var avatarPhotoTypes = map[string]string{"image/jpeg": "photo.jpg", "image/png": "photo.png", "image/webp": "photo.webp"}

// AvatarPerson is one person's photo and choice.
type AvatarPerson struct {
	DiscordUserID    string `json:"discord_user_id"`
	PhotoFileID      string `json:"photo_file_id"`
	PhotoConsentedAt int64  `json:"photo_consented_at"`
	ChosenDrawingID  int64  `json:"chosen_drawing_id"`
	UpdatedAt        int64  `json:"updated_at"`
}

// AvatarDrawing is one drawing, without its print.
type AvatarDrawing struct {
	ID            int64  `json:"id"`
	DiscordUserID string `json:"discord_user_id"`
	DrawingCode   string `json:"drawing_code,omitempty"`
	RequestID     int64  `json:"request_id"`
	CreatedAt     int64  `json:"created_at"`
	// Kind and Comment are the request that made it, for the gallery.
	Kind    string `json:"kind,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Number is its place in the person's gallery, 1 the first drawn, for
	// the page to name it by.
	Number int `json:"-"`
}

// AvatarRequest is one drawing someone asked for.
type AvatarRequest struct {
	ID            int64  `json:"id"`
	DiscordUserID string `json:"discord_user_id"`
	Kind          string `json:"kind"`
	BaseDrawingID int64  `json:"base_drawing_id"`
	Comment       string `json:"comment"`
	State         string `json:"state"`
	Failure       string `json:"failure,omitempty"`
	RequestedAt   int64  `json:"requested_at"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at"`
	DrawingID     int64  `json:"drawing_id"`
}

// Open reports whether the request is still to be drawn or being drawn.
func (r *AvatarRequest) Open() bool {
	return r.State == AvatarRequestWaiting || r.State == AvatarRequestDrawing
}

// ---------------------------------------------------------------- store

// AvatarPersonOf is one person's photo and choice, or ErrNotFound.
func (s *Store) AvatarPersonOf(discordUserID string) (*AvatarPerson, error) {
	var p AvatarPerson
	err := s.db.QueryRow(`SELECT discord_user_id, photo_file_id, photo_consented_at, chosen_drawing_id, updated_at
		FROM avatar_people WHERE discord_user_id = ?`, discordUserID).
		Scan(&p.DiscordUserID, &p.PhotoFileID, &p.PhotoConsentedAt, &p.ChosenDrawingID, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read avatar person: %w", err)
	}
	return &p, nil
}

// AvatarPeople lists everyone with an avatar row.
func (s *Store) AvatarPeople() ([]AvatarPerson, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, photo_file_id, photo_consented_at, chosen_drawing_id, updated_at
		FROM avatar_people ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("list avatar people: %w", err)
	}
	defer rows.Close()
	out := []AvatarPerson{}
	for rows.Next() {
		var p AvatarPerson
		if err := rows.Scan(&p.DiscordUserID, &p.PhotoFileID, &p.PhotoConsentedAt, &p.ChosenDrawingID, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan avatar person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AvatarDrawingsOf is a person's drawings, newest first. withCode includes
// each drawing's code.
func (s *Store) AvatarDrawingsOf(discordUserID string, withCode bool) ([]AvatarDrawing, error) {
	code := `''`
	if withCode {
		code = `d.drawing_code`
	}
	rows, err := s.db.Query(`SELECT d.id, d.discord_user_id, `+code+`, d.request_id, d.created_at,
			COALESCE(r.kind, ''), COALESCE(r.comment, '')
		FROM avatar_drawings d LEFT JOIN avatar_requests r ON r.id = d.request_id
		WHERE d.discord_user_id = ? ORDER BY d.id DESC`, discordUserID)
	if err != nil {
		return nil, fmt.Errorf("list avatar drawings: %w", err)
	}
	defer rows.Close()
	out := []AvatarDrawing{}
	for rows.Next() {
		var d AvatarDrawing
		if err := rows.Scan(&d.ID, &d.DiscordUserID, &d.DrawingCode, &d.RequestID, &d.CreatedAt, &d.Kind, &d.Comment); err != nil {
			return nil, fmt.Errorf("scan avatar drawing: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AvatarDrawingByID is one drawing with its code, or ErrNotFound.
func (s *Store) AvatarDrawingByID(drawingID int64) (*AvatarDrawing, error) {
	var d AvatarDrawing
	err := s.db.QueryRow(`SELECT id, discord_user_id, drawing_code, request_id, created_at FROM avatar_drawings WHERE id = ?`, drawingID).
		Scan(&d.ID, &d.DiscordUserID, &d.DrawingCode, &d.RequestID, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read avatar drawing: %w", err)
	}
	return &d, nil
}

// AvatarDrawingImage is one drawing's print and whose drawing it is.
func (s *Store) AvatarDrawingImage(drawingID int64) (string, []byte, error) {
	var owner string
	var image []byte
	err := s.db.QueryRow(`SELECT discord_user_id, image_webp FROM avatar_drawings WHERE id = ?`, drawingID).Scan(&owner, &image)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("read avatar drawing image: %w", err)
	}
	return owner, image, nil
}

// ChosenAvatarImage is the print of the drawing a person chose to show.
func (s *Store) ChosenAvatarImage(discordUserID string) ([]byte, error) {
	var image []byte
	err := s.db.QueryRow(`SELECT d.image_webp FROM avatar_people p JOIN avatar_drawings d ON d.id = p.chosen_drawing_id
		WHERE p.discord_user_id = ?`, discordUserID).Scan(&image)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read chosen avatar: %w", err)
	}
	return image, nil
}

// ChosenAvatarUserIDs is everyone who chose a drawing to show beside their
// name.
func (s *Store) ChosenAvatarUserIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT p.discord_user_id FROM avatar_people p
		JOIN avatar_drawings d ON d.id = p.chosen_drawing_id`)
	if err != nil {
		return nil, fmt.Errorf("list chosen avatars: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan chosen avatar: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

const avatarRequestColumns = `id, discord_user_id, kind, base_drawing_id, comment, state, failure,
	requested_at, started_at, finished_at, drawing_id`

func scanAvatarRequest(row interface{ Scan(...any) error }) (*AvatarRequest, error) {
	var r AvatarRequest
	err := row.Scan(&r.ID, &r.DiscordUserID, &r.Kind, &r.BaseDrawingID, &r.Comment, &r.State, &r.Failure,
		&r.RequestedAt, &r.StartedAt, &r.FinishedAt, &r.DrawingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan avatar request: %w", err)
	}
	return &r, nil
}

// AvatarRequestByID is one request, or ErrNotFound.
func (s *Store) AvatarRequestByID(requestID int64) (*AvatarRequest, error) {
	return scanAvatarRequest(s.db.QueryRow(`SELECT `+avatarRequestColumns+` FROM avatar_requests WHERE id = ?`, requestID))
}

// LatestAvatarRequestOf is a person's newest request, or ErrNotFound.
func (s *Store) LatestAvatarRequestOf(discordUserID string) (*AvatarRequest, error) {
	return scanAvatarRequest(s.db.QueryRow(`SELECT `+avatarRequestColumns+` FROM avatar_requests
		WHERE discord_user_id = ? ORDER BY id DESC LIMIT 1`, discordUserID))
}

// AvatarRequestsToDraw is every request waiting, and every drawing started
// longer ago than avatarRequestStaleAfter, oldest first.
func (s *Store) AvatarRequestsToDraw() ([]AvatarRequest, error) {
	rows, err := s.db.Query(`SELECT `+avatarRequestColumns+` FROM avatar_requests
		WHERE state = ? OR (state = ? AND started_at < ?) ORDER BY id`,
		AvatarRequestWaiting, AvatarRequestDrawing, now()-int64(avatarRequestStaleAfter/time.Second))
	if err != nil {
		return nil, fmt.Errorf("list avatar requests to draw: %w", err)
	}
	defer rows.Close()
	out := []AvatarRequest{}
	for rows.Next() {
		r, err := scanAvatarRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// AvatarDrawingsAskedForSince counts the drawings a person asked for since a
// moment.
func (s *Store) AvatarDrawingsAskedForSince(discordUserID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM avatar_requests WHERE discord_user_id = ? AND requested_at >= ?`,
		discordUserID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count avatar drawings: %w", err)
	}
	return n, nil
}

// changeAvatar runs change inside one transaction and records action in the
// history in the same one.
func (s *Store) changeAvatar(discordUserID, action, detail string, change func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO avatar_updates (discord_user_id, action, detail, at) VALUES (?, ?, ?, ?)`,
		discordUserID, action, detail, now()); err != nil {
		return fmt.Errorf("record avatar update: %w", err)
	}
	return tx.Commit()
}

// ensureAvatarPerson makes the person's row if there is none.
func ensureAvatarPerson(tx *sql.Tx, discordUserID string) error {
	_, err := tx.Exec(`INSERT INTO avatar_people (discord_user_id, updated_at) VALUES (?, ?)
		ON CONFLICT(discord_user_id) DO NOTHING`, discordUserID, now())
	if err != nil {
		return fmt.Errorf("store avatar person: %w", err)
	}
	return nil
}

// avatarRequestAsked is a request as a person makes it.
type avatarRequestAsked struct {
	Kind          string
	BaseDrawingID int64
	Comment       string
	// NewPhotoFileID is the photo uploaded with a new_photo request.
	NewPhotoFileID string
}

// RequestAvatarDrawing records a drawing someone asked for. It refuses a
// second while one is under way, a redraw with no photo kept, and an edit of
// a drawing that is not theirs. A new photo replaces the kept one, whose id
// it returns for the caller to purge.
func (s *Store) RequestAvatarDrawing(discordUserID string, asked avatarRequestAsked) (requestID int64, replacedPhotoFileID string, err error) {
	if !avatarRequestKinds[asked.Kind] {
		return 0, "", fmt.Errorf("%w: kind %q is not one of new_photo, redraw_photo, edit_drawing", ErrInvalidEvent, asked.Kind)
	}
	err = s.changeAvatar(discordUserID, "requested_"+asked.Kind, asked.NewPhotoFileID, func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM avatar_requests WHERE discord_user_id = ? AND state IN (?, ?)`,
			discordUserID, AvatarRequestWaiting, AvatarRequestDrawing).Scan(&open); err != nil {
			return fmt.Errorf("read open avatar requests: %w", err)
		}
		if open > 0 {
			return fmt.Errorf("%w (a drawing is already under way)", ErrAvatarState)
		}
		if err := ensureAvatarPerson(tx, discordUserID); err != nil {
			return err
		}
		var photoFileID string
		if err := tx.QueryRow(`SELECT photo_file_id FROM avatar_people WHERE discord_user_id = ?`, discordUserID).Scan(&photoFileID); err != nil {
			return fmt.Errorf("read avatar photo: %w", err)
		}
		baseDrawingID := int64(0)
		switch asked.Kind {
		case AvatarRequestNewPhoto:
			if asked.NewPhotoFileID == "" {
				return fmt.Errorf("%w: a new photo is required", ErrInvalidEvent)
			}
			replacedPhotoFileID = photoFileID
			if _, err := tx.Exec(`UPDATE avatar_people SET photo_file_id = ?, photo_consented_at = ?, updated_at = ?
				WHERE discord_user_id = ?`, asked.NewPhotoFileID, now(), now(), discordUserID); err != nil {
				return fmt.Errorf("store avatar photo: %w", err)
			}
		case AvatarRequestRedrawPhoto:
			if photoFileID == "" {
				return fmt.Errorf("%w (no photo is kept to draw from; upload one)", ErrAvatarState)
			}
		case AvatarRequestEditDrawing:
			var owner string
			err := tx.QueryRow(`SELECT discord_user_id FROM avatar_drawings WHERE id = ?`, asked.BaseDrawingID).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) || owner != discordUserID {
				return fmt.Errorf("%w: that drawing is not one of yours", ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("read drawing to change: %w", err)
			}
			baseDrawingID = asked.BaseDrawingID
		}
		result, err := tx.Exec(`INSERT INTO avatar_requests (discord_user_id, kind, base_drawing_id, comment, state, requested_at)
			VALUES (?, ?, ?, ?, ?, ?)`, discordUserID, asked.Kind, baseDrawingID, asked.Comment, AvatarRequestWaiting, now())
		if err != nil {
			return fmt.Errorf("store avatar request: %w", err)
		}
		requestID, err = result.LastInsertId()
		return err
	})
	return requestID, replacedPhotoFileID, err
}

// moveAvatarRequest changes a request only from the given state, so two
// drawers cannot both take it and a drawing nobody started is not saved.
func moveAvatarRequest(tx *sql.Tx, requestID int64, fromState, set string, args ...any) error {
	result, err := tx.Exec(`UPDATE avatar_requests SET `+set+` WHERE id = ? AND state = ?`, append(args, requestID, fromState)...)
	if err != nil {
		return fmt.Errorf("update avatar request: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w (the request must be %s)", ErrAvatarState, fromState)
	}
	return nil
}

func (s *Store) avatarRequestOwner(requestID int64) (string, error) {
	request, err := s.AvatarRequestByID(requestID)
	if err != nil {
		return "", err
	}
	return request.DiscordUserID, nil
}

// StartAvatarRequest marks a drawing under way. It refuses one that is not
// waiting, unless a drawer run that died left it under way for too long.
func (s *Store) StartAvatarRequest(requestID int64) error {
	owner, err := s.avatarRequestOwner(requestID)
	if err != nil {
		return err
	}
	return s.changeAvatar(owner, "drawing_started", strconv.FormatInt(requestID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`UPDATE avatar_requests SET state = ?, started_at = ?, failure = ''
			WHERE id = ? AND (state = ? OR (state = ? AND started_at < ?))`,
			AvatarRequestDrawing, now(), requestID, AvatarRequestWaiting, AvatarRequestDrawing,
			now()-int64(avatarRequestStaleAfter/time.Second))
		if err != nil {
			return fmt.Errorf("start avatar request: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("%w (the request must be waiting)", ErrAvatarState)
		}
		return nil
	})
}

// FinishAvatarRequest adds the finished drawing to the person's gallery. It
// does not show it: the person chooses that.
func (s *Store) FinishAvatarRequest(requestID int64, drawingCode string, imageWebP []byte) (drawingID int64, err error) {
	owner, err := s.avatarRequestOwner(requestID)
	if err != nil {
		return 0, err
	}
	err = s.changeAvatar(owner, "drawing_saved", strconv.FormatInt(requestID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`INSERT INTO avatar_drawings (discord_user_id, drawing_code, image_webp, request_id, created_at)
			VALUES (?, ?, ?, ?, ?)`, owner, drawingCode, imageWebP, requestID, now())
		if err != nil {
			return fmt.Errorf("store avatar drawing: %w", err)
		}
		if drawingID, err = result.LastInsertId(); err != nil {
			return err
		}
		return moveAvatarRequest(tx, requestID, AvatarRequestDrawing, `state = ?, finished_at = ?, drawing_id = ?`,
			AvatarRequestDone, now(), drawingID)
	})
	return drawingID, err
}

// FailAvatarRequest records a drawing that did not come out, and why.
func (s *Store) FailAvatarRequest(requestID int64, reason string) error {
	owner, err := s.avatarRequestOwner(requestID)
	if err != nil {
		return err
	}
	return s.changeAvatar(owner, "drawing_failed", reason, func(tx *sql.Tx) error {
		return moveAvatarRequest(tx, requestID, AvatarRequestDrawing, `state = ?, failure = ?, finished_at = ?`,
			AvatarRequestFailed, reason, now())
	})
}

// ChooseAvatarDrawing shows one of the person's drawings beside their name;
// 0 shows none.
func (s *Store) ChooseAvatarDrawing(discordUserID string, drawingID int64) error {
	return s.changeAvatar(discordUserID, "chose", strconv.FormatInt(drawingID, 10), func(tx *sql.Tx) error {
		if drawingID != 0 {
			var owner string
			err := tx.QueryRow(`SELECT discord_user_id FROM avatar_drawings WHERE id = ?`, drawingID).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) || owner != discordUserID {
				return fmt.Errorf("%w: that drawing is not one of yours", ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("read chosen drawing: %w", err)
			}
		}
		if err := ensureAvatarPerson(tx, discordUserID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE avatar_people SET chosen_drawing_id = ?, updated_at = ? WHERE discord_user_id = ?`,
			drawingID, now(), discordUserID)
		if err != nil {
			return fmt.Errorf("choose avatar drawing: %w", err)
		}
		return nil
	})
}

// DeleteAvatarDrawing deletes one of the person's drawings. The one shown is
// no longer shown.
func (s *Store) DeleteAvatarDrawing(discordUserID string, drawingID int64) error {
	return s.changeAvatar(discordUserID, "deleted_drawing", strconv.FormatInt(drawingID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`DELETE FROM avatar_drawings WHERE id = ? AND discord_user_id = ?`, drawingID, discordUserID)
		if err != nil {
			return fmt.Errorf("delete avatar drawing: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: that drawing is not one of yours", ErrNotFound)
		}
		_, err = tx.Exec(`UPDATE avatar_people SET chosen_drawing_id = 0, updated_at = ?
			WHERE discord_user_id = ? AND chosen_drawing_id = ?`, now(), discordUserID, drawingID)
		if err != nil {
			return fmt.Errorf("unchoose deleted drawing: %w", err)
		}
		return nil
	})
}

// ForgetAvatarPhoto clears the kept photo's id. The caller purges the photo
// from file-store first, so a photo that could not be purged stays named.
func (s *Store) ForgetAvatarPhoto(discordUserID string) error {
	return s.changeAvatar(discordUserID, "deleted_photo", "", func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM avatar_requests WHERE discord_user_id = ? AND state IN (?, ?) AND kind != ?`,
			discordUserID, AvatarRequestWaiting, AvatarRequestDrawing, AvatarRequestEditDrawing).Scan(&open); err != nil {
			return fmt.Errorf("read open avatar requests: %w", err)
		}
		if open > 0 {
			return fmt.Errorf("%w (a drawing from the photo is under way)", ErrAvatarState)
		}
		_, err := tx.Exec(`UPDATE avatar_people SET photo_file_id = '', updated_at = ? WHERE discord_user_id = ?`, now(), discordUserID)
		if err != nil {
			return fmt.Errorf("forget avatar photo: %w", err)
		}
		return nil
	})
}

// RemoveAvatarEverything deletes the person's drawings, requests and row.
// The caller purges the photo first. The history stays.
func (s *Store) RemoveAvatarEverything(discordUserID string) error {
	return s.changeAvatar(discordUserID, "removed", "", func(tx *sql.Tx) error {
		for _, table := range []string{"avatar_drawings", "avatar_requests", "avatar_people"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE discord_user_id = ?`, discordUserID); err != nil {
				return fmt.Errorf("remove from %s: %w", table, err)
			}
		}
		return nil
	})
}

// SetAvatarByOperator adds a drawing made outside the requests — such as the
// site's mascot, drawn from photos she is in — to someone's gallery and shows
// it. reason is recorded in the history.
func (s *Store) SetAvatarByOperator(discordUserID, drawingCode string, imageWebP []byte, reason string) (drawingID int64, err error) {
	err = s.changeAvatar(discordUserID, "set_by_operator", reason, func(tx *sql.Tx) error {
		if err := ensureAvatarPerson(tx, discordUserID); err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO avatar_drawings (discord_user_id, drawing_code, image_webp, request_id, created_at)
			VALUES (?, ?, ?, 0, ?)`, discordUserID, drawingCode, imageWebP, now())
		if err != nil {
			return fmt.Errorf("store avatar drawing: %w", err)
		}
		if drawingID, err = result.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE avatar_people SET chosen_drawing_id = ?, updated_at = ? WHERE discord_user_id = ?`,
			drawingID, now(), discordUserID)
		return err
	})
	return drawingID, err
}

// migrateSingleAvatarsTable moves the first shape of avatars — one row per
// person in a table named avatars, 2026-09-28 — into avatar_people,
// avatar_drawings and avatar_requests, and drops it. A drawing that was
// approved becomes the chosen one; one waiting to be drawn becomes a request.
func migrateSingleAvatarsTable(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'avatars'`).Scan(&exists); err != nil {
		return fmt.Errorf("look for the old avatars table: %w", err)
	}
	if exists == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT discord_user_id, state, photo_file_id, drawing_code, image_webp, failure,
		consented_at, drawn_at, updated_at FROM avatars`)
	if err != nil {
		return fmt.Errorf("read the old avatars table: %w", err)
	}
	type oldAvatar struct {
		userID, state, photoFileID, code, failure string
		image                                     []byte
		consentedAt, drawnAt, updatedAt           int64
	}
	var old []oldAvatar
	for rows.Next() {
		var a oldAvatar
		if err := rows.Scan(&a.userID, &a.state, &a.photoFileID, &a.code, &a.image, &a.failure, &a.consentedAt, &a.drawnAt, &a.updatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan the old avatars table: %w", err)
		}
		old = append(old, a)
	}
	rows.Close()
	for _, a := range old {
		if _, err := tx.Exec(`INSERT INTO avatar_people (discord_user_id, photo_file_id, photo_consented_at, updated_at) VALUES (?, ?, ?, ?)`,
			a.userID, a.photoFileID, a.consentedAt, a.updatedAt); err != nil {
			return fmt.Errorf("move avatar of %s: %w", a.userID, err)
		}
		state := map[string]string{"waiting_for_drawing": AvatarRequestWaiting, "drawing": AvatarRequestWaiting,
			"drawing_failed": AvatarRequestFailed}[a.state]
		if a.image != nil {
			state = AvatarRequestDone
		}
		result, err := tx.Exec(`INSERT INTO avatar_requests (discord_user_id, kind, state, failure, requested_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?)`, a.userID, AvatarRequestNewPhoto, state, a.failure, a.consentedAt, a.drawnAt)
		if err != nil {
			return fmt.Errorf("move avatar request of %s: %w", a.userID, err)
		}
		requestID, _ := result.LastInsertId()
		if a.image == nil {
			continue
		}
		result, err = tx.Exec(`INSERT INTO avatar_drawings (discord_user_id, drawing_code, image_webp, request_id, created_at)
			VALUES (?, ?, ?, ?, ?)`, a.userID, a.code, a.image, requestID, a.drawnAt)
		if err != nil {
			return fmt.Errorf("move avatar drawing of %s: %w", a.userID, err)
		}
		drawingID, _ := result.LastInsertId()
		if _, err := tx.Exec(`UPDATE avatar_requests SET drawing_id = ? WHERE id = ?`, drawingID, requestID); err != nil {
			return err
		}
		if a.state == "approved" {
			if _, err := tx.Exec(`UPDATE avatar_people SET chosen_drawing_id = ? WHERE discord_user_id = ?`, drawingID, a.userID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DROP TABLE avatars`); err != nil {
		return fmt.Errorf("drop the old avatars table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("[discord-signup] moved %d avatars from the old avatars table", len(old))
	return nil
}

// ---------------------------------------------------------------- web pages

// EnableAvatars turns on the avatar pages, with file-store to keep photos
// in. Without it the avatar page says it is not set up.
func (s *Server) EnableAvatars(files *FileStoreClient) {
	s.files = files
}

// mayHaveAvatar is whether someone may ask for drawings: a member of a
// server the bot is in, so that a stranger with a Discord account cannot
// spend the host's drawing time.
func (s *Server) mayHaveAvatar(session *WebSession) (bool, error) {
	guilds, err := s.store.BotGuilds()
	if err != nil {
		return false, err
	}
	for _, g := range guilds {
		if session.IsMemberOf(g.ID) {
			return true, nil
		}
	}
	return false, nil
}

// avatarDrawingsLeftToday is how many more drawings the person may ask for.
func (s *Server) avatarDrawingsLeftToday(discordUserID string) (int, error) {
	asked, err := s.store.AvatarDrawingsAskedForSince(discordUserID, now()-24*3600)
	if err != nil {
		return 0, err
	}
	return max(0, avatarDrawingsPerDay-asked), nil
}

// avatarTile is one drawing in the avatar page's gallery.
type avatarTile struct {
	Drawing AvatarDrawing
	Chosen  bool
}

// avatarPage is what the avatar page shows.
type avatarPage struct {
	Person       *AvatarPerson
	Drawings     []AvatarDrawing
	Request      *AvatarRequest
	MayAsk       bool
	DrawingsLeft int
	PerDay       int
	CommentLimit int
}

func (s *Server) handleWebAvatar(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	data := pageData{Title: "Your avatar", Session: session, Notice: r.URL.Query().Get("notice")}
	page := &avatarPage{PerDay: avatarDrawingsPerDay, CommentLimit: avatarCommentMaximumCharacters}
	var problems []string
	if s.files == nil {
		problems = append(problems, "Avatars are not set up on this server: "+ErrFileStoreNotConfigured.Error())
	}
	mayHave, err := s.mayHaveAvatar(session)
	if err != nil {
		problems = append(problems, "Could not check your servers: "+err.Error())
	}
	page.MayAsk = mayHave && s.files != nil
	if page.Person, err = s.store.AvatarPersonOf(session.DiscordUserID); err != nil && !errors.Is(err, ErrNotFound) {
		problems = append(problems, "Could not load your avatar: "+err.Error())
	}
	if page.Drawings, err = s.store.AvatarDrawingsOf(session.DiscordUserID, false); err != nil {
		problems = append(problems, "Could not load your drawings: "+err.Error())
	}
	for i := range page.Drawings {
		page.Drawings[i].Number = len(page.Drawings) - i
	}
	if page.Request, err = s.store.LatestAvatarRequestOf(session.DiscordUserID); err != nil && !errors.Is(err, ErrNotFound) {
		problems = append(problems, "Could not load your last request: "+err.Error())
	}
	if page.DrawingsLeft, err = s.avatarDrawingsLeftToday(session.DiscordUserID); err != nil {
		problems = append(problems, "Could not count your drawings today: "+err.Error())
	}
	data.AvatarPage = page
	data.Error = strings.Join(problems, " ")
	s.render(w, "avatar.html", data)
}

// handleWebAvatarStatus answers the avatar page's script, which asks every
// few seconds while a drawing is under way and adds it to the gallery when
// it is done.
func (s *Server) handleWebAvatarStatus(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFrom(r)
	if session == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again"})
		return
	}
	request, err := s.store.LatestAvatarRequestOf(session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"request": nil})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": request})
}

func avatarRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/avatar?"+noticeQuery(notice), http.StatusSeeOther)
}

// handleWebAvatarRequest takes a request for a drawing: a new photo, the kept
// photo again, or a change to one of their drawings, each with a comment.
func (s *Server) handleWebAvatarRequest(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if s.files == nil {
		http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
		return
	}
	mayHave, err := s.mayHaveAvatar(session)
	if err != nil {
		http.Error(w, "could not check your servers: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !mayHave {
		http.Error(w, "avatars are for members of a server this bot is in", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, avatarPhotoMaximumBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		avatarRedirect(w, r, fmt.Sprintf("That upload could not be read. A photo may be at most %d MB. (%v)", avatarPhotoMaximumBytes>>20, err))
		return
	}
	asked := avatarRequestAsked{Kind: r.FormValue("kind"), Comment: strings.TrimSpace(r.FormValue("comment"))}
	if utf8.RuneCountInString(asked.Comment) > avatarCommentMaximumCharacters {
		avatarRedirect(w, r, fmt.Sprintf("Keep the comment to %d characters.", avatarCommentMaximumCharacters))
		return
	}
	if asked.Kind == AvatarRequestEditDrawing {
		if asked.BaseDrawingID, err = strconv.ParseInt(r.FormValue("base_drawing_id"), 10, 64); err != nil {
			avatarRedirect(w, r, "Choose the drawing to change.")
			return
		}
		if asked.Comment == "" {
			avatarRedirect(w, r, "Say how you want the drawing changed.")
			return
		}
	}
	left, err := s.avatarDrawingsLeftToday(session.DiscordUserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if left == 0 {
		avatarRedirect(w, r, fmt.Sprintf("You have asked for %d drawings in the last day, which is the most. Try again tomorrow.", avatarDrawingsPerDay))
		return
	}
	if asked.Kind == AvatarRequestNewPhoto {
		if r.FormValue("consent") != "yes" {
			avatarRedirect(w, r, "Tick the box to say avatars may be drawn from your photo.")
			return
		}
		fileID, notice := s.uploadAvatarPhoto(r, session.DiscordUserID)
		if fileID == "" {
			avatarRedirect(w, r, notice)
			return
		}
		asked.NewPhotoFileID = fileID
	}
	_, replaced, err := s.store.RequestAvatarDrawing(session.DiscordUserID, asked)
	if err != nil {
		if asked.NewPhotoFileID != "" {
			// The upload is kept nowhere now; do not leave its bytes behind.
			if purgeErr := s.files.PurgePhoto(asked.NewPhotoFileID); purgeErr != nil {
				log.Printf("[discord-signup] purge unrecorded avatar photo %s: %v", asked.NewPhotoFileID, purgeErr)
			}
		}
		avatarRedirect(w, r, "Could not ask for the drawing: "+err.Error())
		return
	}
	if replaced != "" {
		if err := s.files.PurgePhoto(replaced); err != nil {
			log.Printf("[discord-signup] purge replaced avatar photo %s of %s: %v", replaced, session.DiscordUserID, err)
		}
	}
	log.Printf("[discord-signup] avatar %s asked for by web:%s", asked.Kind, session.DiscordUserID)
	avatarRedirect(w, r, "Your drawing is under way. It appears here when it is done, usually within a few minutes.")
}

// uploadAvatarPhoto reads the uploaded photo and keeps it in file-store. It
// returns the file id, or "" and what to tell the person.
func (s *Server) uploadAvatarPhoto(r *http.Request, discordUserID string) (string, string) {
	file, _, err := r.FormFile("photo")
	if err != nil {
		return "", "Choose a photo to upload."
	}
	defer file.Close()
	photo, err := io.ReadAll(io.LimitReader(file, avatarPhotoMaximumBytes+1))
	if err != nil {
		return "", "Could not read the upload: " + err.Error()
	}
	if len(photo) > avatarPhotoMaximumBytes {
		return "", fmt.Sprintf("That photo is over %d MB.", avatarPhotoMaximumBytes>>20)
	}
	contentType := http.DetectContentType(photo)
	filename, readable := avatarPhotoTypes[contentType]
	if !readable {
		return "", "That file is not a JPEG, PNG or WebP photo."
	}
	fileID, err := s.files.UploadAvatarPhoto(discordUserID, filename, contentType, photo)
	if err != nil {
		log.Printf("[discord-signup] avatar photo for %s: %v", discordUserID, err)
		return "", "Could not keep your photo: " + err.Error()
	}
	return fileID, ""
}

func formDrawingID(r *http.Request) (int64, error) {
	if err := r.ParseForm(); err != nil {
		return 0, err
	}
	return strconv.ParseInt(r.FormValue("drawing_id"), 10, 64)
}

// handleWebAvatarChoose shows one drawing beside their name, or none (0).
func (s *Server) handleWebAvatarChoose(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	drawingID, err := formDrawingID(r)
	if err != nil {
		http.Error(w, "drawing_id is required", http.StatusBadRequest)
		return
	}
	if err := s.store.ChooseAvatarDrawing(session.DiscordUserID, drawingID); err != nil {
		avatarRedirect(w, r, "Could not choose it: "+err.Error())
		return
	}
	if drawingID == 0 {
		avatarRedirect(w, r, "No avatar shows beside your name now. Your drawings are kept.")
		return
	}
	avatarRedirect(w, r, "That drawing now shows beside your name.")
}

func (s *Server) handleWebAvatarDeleteDrawing(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	drawingID, err := formDrawingID(r)
	if err != nil {
		http.Error(w, "drawing_id is required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteAvatarDrawing(session.DiscordUserID, drawingID); err != nil {
		avatarRedirect(w, r, "Could not delete it: "+err.Error())
		return
	}
	avatarRedirect(w, r, "That drawing is deleted.")
}

// handleWebAvatarDeletePhoto purges the kept photo. The drawings stay.
func (s *Server) handleWebAvatarDeletePhoto(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	person, err := s.store.AvatarPersonOf(session.DiscordUserID)
	if err != nil || person.PhotoFileID == "" {
		avatarRedirect(w, r, "No photo of yours is kept.")
		return
	}
	if s.files == nil {
		http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := s.files.PurgePhoto(person.PhotoFileID); err != nil {
		log.Printf("[discord-signup] purge avatar photo %s of %s: %v", person.PhotoFileID, session.DiscordUserID, err)
		avatarRedirect(w, r, "Could not delete your photo: "+err.Error())
		return
	}
	if err := s.store.ForgetAvatarPhoto(session.DiscordUserID); err != nil {
		avatarRedirect(w, r, "Could not delete your photo: "+err.Error())
		return
	}
	avatarRedirect(w, r, "Your photo is deleted. Your drawings are kept.")
}

// handleWebAvatarRemove deletes the photo, every drawing and every request.
// The photo goes first: if it cannot be deleted nothing is, so the person can
// see it is still there and try again.
func (s *Server) handleWebAvatarRemove(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	person, err := s.store.AvatarPersonOf(session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		avatarRedirect(w, r, "You have no avatar.")
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if person.PhotoFileID != "" {
		if s.files == nil {
			http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := s.files.PurgePhoto(person.PhotoFileID); err != nil {
			log.Printf("[discord-signup] purge avatar photo %s of %s: %v", person.PhotoFileID, session.DiscordUserID, err)
			avatarRedirect(w, r, "Could not delete your photo, so nothing was removed: "+err.Error())
			return
		}
	}
	if err := s.store.RemoveAvatarEverything(session.DiscordUserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[discord-signup] avatar removed by web:%s", session.DiscordUserID)
	avatarRedirect(w, r, "Your photo and every drawing are deleted.")
}

// avatarHTML is the small round avatar shown before a person's name when
// shownUserIDs holds them. The name beside it says who it is, so the image
// itself is decoration to a screen reader.
func avatarHTML(shownUserIDs map[string]bool, discordUserID string) template.HTML {
	if !shownUserIDs[discordUserID] {
		return ""
	}
	return template.HTML(`<img class="avatar" src="/avatars/` + template.HTMLEscapeString(discordUserID) +
		`.webp" alt="" width="28" height="28" loading="lazy">`)
}

func writeWebPImage(w http.ResponseWriter, image []byte, cacheControl string) {
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", cacheControl)
	w.Write(image)
}

// handleWebOwnAvatarDrawing shows a person one of their own drawings, at
// /avatar/drawings/{id}.webp. Anyone else's is a 404.
func (s *Server) handleWebOwnAvatarDrawing(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	idText, isWebP := strings.CutSuffix(r.PathValue("file"), ".webp")
	drawingID, err := strconv.ParseInt(idText, 10, 64)
	if !isWebP || err != nil {
		http.NotFound(w, r)
		return
	}
	owner, image, err := s.store.AvatarDrawingImage(drawingID)
	if errors.Is(err, ErrNotFound) || (err == nil && owner != session.DiscordUserID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A drawing never changes; its id names it.
	writeWebPImage(w, image, "private, max-age=86400")
}

// handleAvatarImage serves the drawing a person chose to show, at
// /avatars/{userID}.webp, to anyone: they chose to show it with their name.
func (s *Server) handleAvatarImage(w http.ResponseWriter, r *http.Request) {
	userID, isWebP := strings.CutSuffix(r.PathValue("file"), ".webp")
	if !isWebP {
		http.NotFound(w, r)
		return
	}
	image, err := s.store.ChosenAvatarImage(userID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Short, because a person can choose another or remove it at any time.
	writeWebPImage(w, image, "public, max-age=300")
}

// ---------------------------------------------------------------- machine API

func writeAvatarError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrAvatarState) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeStoreError(w, err)
}

func (s *Server) handleListAvatars(w http.ResponseWriter, r *http.Request) {
	people, err := s.store.AvatarPeople()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"avatars": people})
}

// handleGetAvatar is one person's photo, choice and every drawing with its
// code.
func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	person, err := s.store.AvatarPersonOf(userID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	drawings, err := s.store.AvatarDrawingsOf(userID, true)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"person": person, "drawings": drawings})
}

// handleAvatarImageForMachines serves the chosen drawing's print by id.
func (s *Server) handleAvatarImageForMachines(w http.ResponseWriter, r *http.Request) {
	image, err := s.store.ChosenAvatarImage(r.PathValue("userID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeWebPImage(w, image, "no-store")
}

// avatarRequestToDraw is a request with what the drawer needs to draw it.
type avatarRequestToDraw struct {
	AvatarRequest
	// BaseDrawingCode is the drawing an edit_drawing request changes.
	BaseDrawingCode string `json:"base_drawing_code,omitempty"`
	// HasPhoto is whether the person's photo is kept to draw from.
	HasPhoto bool `json:"has_photo"`
}

func (s *Server) handleAvatarRequestsToDraw(w http.ResponseWriter, r *http.Request) {
	requests, err := s.store.AvatarRequestsToDraw()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := []avatarRequestToDraw{}
	for _, request := range requests {
		item := avatarRequestToDraw{AvatarRequest: request}
		if person, err := s.store.AvatarPersonOf(request.DiscordUserID); err == nil {
			item.HasPhoto = person.PhotoFileID != ""
		}
		if request.BaseDrawingID != 0 {
			base, err := s.store.AvatarDrawingByID(request.BaseDrawingID)
			if err != nil {
				// Deleted since it was asked for: there is nothing to change.
				log.Printf("[discord-signup] avatar request %d: base drawing %d: %v", request.ID, request.BaseDrawingID, err)
				continue
			}
			item.BaseDrawingCode = base.DrawingCode
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func requestIDFrom(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("requestID"), 10, 64)
}

// handleAvatarRequestPhoto gives the drawer the person's kept photo, only
// while one of their drawings is under way: nothing else here reads it.
func (s *Server) handleAvatarRequestPhoto(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	request, err := s.store.AvatarRequestByID(requestID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if request.State != AvatarRequestDrawing {
		writeAvatarError(w, fmt.Errorf("%w (the photo is read only while a drawing is under way)", ErrAvatarState))
		return
	}
	person, err := s.store.AvatarPersonOf(request.DiscordUserID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if person.PhotoFileID == "" {
		writeStoreError(w, fmt.Errorf("%w: no photo is kept", ErrNotFound))
		return
	}
	if s.files == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": ErrFileStoreNotConfigured.Error()})
		return
	}
	photo, contentType, err := s.files.PhotoContent(person.PhotoFileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(photo)
}

func (s *Server) handleAvatarRequestStarted(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.StartAvatarRequest(requestID); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isWebP is whether the bytes are a WebP file: RIFF, a size, then WEBP.
func isWebP(image []byte) bool {
	return len(image) > 12 && bytes.Equal(image[:4], []byte("RIFF")) && bytes.Equal(image[8:12], []byte("WEBP"))
}

// decodeDrawing reads {"drawing_code", "image_webp"} and checks both,
// answering the request itself when they do not hold.
func decodeDrawing(w http.ResponseWriter, r *http.Request, extra map[string]*string) (string, []byte, bool) {
	var body struct {
		DrawingCode string `json:"drawing_code"`
		// ImageWebP is the print, base64 in JSON as encoding/json does []byte.
		ImageWebP []byte `json:"image_webp"`
		Reason    string `json:"reason"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*avatarImageMaximumBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return "", nil, false
	}
	switch {
	case strings.TrimSpace(body.DrawingCode) == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drawing_code is required"})
		return "", nil, false
	case !isWebP(body.ImageWebP) || len(body.ImageWebP) > avatarImageMaximumBytes:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("image_webp must be a WebP image of at most %d bytes", avatarImageMaximumBytes)})
		return "", nil, false
	}
	if reason, wanted := extra["reason"]; wanted {
		if strings.TrimSpace(body.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason is required"})
			return "", nil, false
		}
		*reason = body.Reason
	} else if body.Reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason is not a field here"})
		return "", nil, false
	}
	return body.DrawingCode, body.ImageWebP, true
}

func (s *Server) handleAvatarRequestDrawing(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	code, image, ok := decodeDrawing(w, r, nil)
	if !ok {
		return
	}
	drawingID, err := s.store.FinishAvatarRequest(requestID, code, image)
	if err != nil {
		writeAvatarError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"drawing_id": drawingID})
}

func (s *Server) handleAvatarRequestFailed(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"reason\": \"…\"} is required"})
		return
	}
	if err := s.store.FailAvatarRequest(requestID, body.Reason); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetAvatarByOperator is PUT /api/avatars/{userID}: a drawing made
// outside the requests, added to the person's gallery and shown, with the
// reason recorded.
func (s *Server) handleSetAvatarByOperator(w http.ResponseWriter, r *http.Request) {
	var reason string
	code, image, ok := decodeDrawing(w, r, map[string]*string{"reason": &reason})
	if !ok {
		return
	}
	drawingID, err := s.store.SetAvatarByOperator(r.PathValue("userID"), code, image, reason)
	if err != nil {
		writeAvatarError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"drawing_id": drawingID})
}
