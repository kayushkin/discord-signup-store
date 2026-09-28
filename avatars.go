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
	"strings"
	"time"
)

// Avatars: a drawing of a person in the site's print style, made from a photo
// they upload and shown beside their name on the event pages once they
// approve it. Only the person can start one, approve it or remove it. The
// drawing is made by cmd/discord-avatar-drawer, which the scheduler runs:
// it takes each photo waiting here, has a model draw it in art/kit.js's form,
// prints it and hands back the code and the print. The photo is purged from
// file-store when the person approves the drawing or removes their avatar.

// The states an avatar moves through, in order. A failed drawing and a
// drawing the person asks to redo both go back to waiting.
const (
	AvatarWaitingForDrawing = "waiting_for_drawing"
	AvatarDrawing           = "drawing"
	AvatarReadyForApproval  = "ready_for_approval"
	AvatarApproved          = "approved"
	AvatarDrawingFailed     = "drawing_failed"
)

// ErrAvatarState is an avatar asked to do something its state does not
// allow: approving one not drawn yet, saving a drawing nobody started.
var ErrAvatarState = errors.New("the avatar is not in a state that allows this")

// avatarDrawingsPerDay is how many drawings one person may ask for in a day,
// uploads and redraws together. Each costs a model's time on the host.
const avatarDrawingsPerDay = 4

// avatarDrawingStaleAfter is how long a drawing may be under way before the
// drawer takes it again. The scheduler kills a drawer run after 30 minutes,
// so a drawing older than this belongs to a run that is gone.
const avatarDrawingStaleAfter = time.Hour

// avatarPhotoMaximumBytes bounds an upload. A phone's photo is 2–6 MB.
const avatarPhotoMaximumBytes = 15 << 20

// avatarImageMaximumBytes bounds a printed drawing handed back by the drawer.
const avatarImageMaximumBytes = 2 << 20

// avatarPhotoTypes are the photo types a model can read, by what the bytes
// are rather than what the browser said.
var avatarPhotoTypes = map[string]string{"image/jpeg": "photo.jpg", "image/png": "photo.png", "image/webp": "photo.webp"}

// Avatar is one person's avatar, without its printed image.
type Avatar struct {
	DiscordUserID    string `json:"discord_user_id"`
	State            string `json:"state"`
	PhotoFileID      string `json:"photo_file_id"`
	DrawingCode      string `json:"drawing_code,omitempty"`
	HasImage         bool   `json:"has_image"`
	Failure          string `json:"failure,omitempty"`
	ConsentedAt      int64  `json:"consented_at"`
	DrawingStartedAt int64  `json:"drawing_started_at"`
	DrawnAt          int64  `json:"drawn_at"`
	ApprovedAt       int64  `json:"approved_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

const avatarColumns = `discord_user_id, state, photo_file_id, drawing_code, image_webp IS NOT NULL,
	failure, consented_at, drawing_started_at, drawn_at, approved_at, updated_at`

func scanAvatar(row interface{ Scan(...any) error }) (*Avatar, error) {
	var a Avatar
	err := row.Scan(&a.DiscordUserID, &a.State, &a.PhotoFileID, &a.DrawingCode, &a.HasImage,
		&a.Failure, &a.ConsentedAt, &a.DrawingStartedAt, &a.DrawnAt, &a.ApprovedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan avatar: %w", err)
	}
	return &a, nil
}

// AvatarOf is one person's avatar, or ErrNotFound when they have none.
func (s *Store) AvatarOf(discordUserID string) (*Avatar, error) {
	return scanAvatar(s.db.QueryRow(`SELECT `+avatarColumns+` FROM avatars WHERE discord_user_id = ?`, discordUserID))
}

// Avatars lists every avatar, or those in one state when state is set.
func (s *Store) Avatars(state string) ([]Avatar, error) {
	query, args := `SELECT `+avatarColumns+` FROM avatars`, []any{}
	if state != "" {
		query, args = query+` WHERE state = ?`, append(args, state)
	}
	return s.queryAvatars(query+` ORDER BY updated_at`, args...)
}

// AvatarsToDraw is every avatar waiting for a drawing, and every drawing
// started longer ago than avatarDrawingStaleAfter, oldest first.
func (s *Store) AvatarsToDraw() ([]Avatar, error) {
	return s.queryAvatars(`SELECT `+avatarColumns+` FROM avatars
		WHERE state = ? OR (state = ? AND drawing_started_at < ?) ORDER BY updated_at`,
		AvatarWaitingForDrawing, AvatarDrawing, now()-int64(avatarDrawingStaleAfter/time.Second))
}

func (s *Store) queryAvatars(query string, args ...any) ([]Avatar, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list avatars: %w", err)
	}
	defer rows.Close()
	out := []Avatar{}
	for rows.Next() {
		a, err := scanAvatar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ApprovedAvatarUserIDs is everyone whose avatar the pages may show.
func (s *Store) ApprovedAvatarUserIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT discord_user_id FROM avatars WHERE state = ?`, AvatarApproved)
	if err != nil {
		return nil, fmt.Errorf("list approved avatars: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan approved avatar: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// AvatarImage is the printed drawing. approvedOnly refuses one the person
// has not approved, which is every caller but the person themselves.
func (s *Store) AvatarImage(discordUserID string, approvedOnly bool) ([]byte, int64, error) {
	var image []byte
	var state string
	var updatedAt int64
	err := s.db.QueryRow(`SELECT state, image_webp, updated_at FROM avatars WHERE discord_user_id = ?`, discordUserID).
		Scan(&state, &image, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) || image == nil || (approvedOnly && state != AvatarApproved) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read avatar image: %w", err)
	}
	return image, updatedAt, nil
}

// AvatarDrawingsAskedForSince counts the drawings a person asked for since a
// moment: uploads and redraws.
func (s *Store) AvatarDrawingsAskedForSince(discordUserID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM avatar_updates
		WHERE discord_user_id = ? AND action IN ('photo_uploaded', 'redraw_asked') AND at >= ?`,
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

// moveAvatar is a change that applies only in the given states, so two
// clicks, or a click and the drawer, cannot both win.
func moveAvatar(tx *sql.Tx, discordUserID string, fromStates []string, set string, args ...any) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(fromStates)), ",")
	all := append(args, now(), discordUserID)
	for _, state := range fromStates {
		all = append(all, state)
	}
	result, err := tx.Exec(`UPDATE avatars SET `+set+`, updated_at = ? WHERE discord_user_id = ? AND state IN (`+placeholders+`)`, all...)
	if err != nil {
		return fmt.Errorf("update avatar: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w (it must be %s)", ErrAvatarState, strings.Join(fromStates, " or "))
	}
	return nil
}

// UploadAvatarPhoto records a new photo to draw from, replacing whatever
// avatar the person had. It returns the photo the new one replaces, for the
// caller to purge.
func (s *Store) UploadAvatarPhoto(discordUserID, photoFileID string) (replacedPhotoFileID string, err error) {
	err = s.changeAvatar(discordUserID, "photo_uploaded", photoFileID, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT photo_file_id FROM avatars WHERE discord_user_id = ?`, discordUserID).Scan(&replacedPhotoFileID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read avatar: %w", err)
		}
		t := now()
		_, err = tx.Exec(`INSERT INTO avatars (discord_user_id, state, photo_file_id, consented_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(discord_user_id) DO UPDATE SET state = excluded.state, photo_file_id = excluded.photo_file_id,
				drawing_code = '', image_webp = NULL, failure = '', consented_at = excluded.consented_at,
				drawing_started_at = 0, drawn_at = 0, approved_at = 0, updated_at = excluded.updated_at`,
			discordUserID, AvatarWaitingForDrawing, photoFileID, t, t)
		if err != nil {
			return fmt.Errorf("store avatar: %w", err)
		}
		return nil
	})
	return replacedPhotoFileID, err
}

// StartAvatarDrawing marks a drawing under way. It refuses one that is not
// waiting, unless a drawer run that died left it under way for too long.
func (s *Store) StartAvatarDrawing(discordUserID string) error {
	return s.changeAvatar(discordUserID, "drawing_started", "", func(tx *sql.Tx) error {
		stale := now() - int64(avatarDrawingStaleAfter/time.Second)
		result, err := tx.Exec(`UPDATE avatars SET state = ?, drawing_started_at = ?, failure = '', updated_at = ?
			WHERE discord_user_id = ? AND (state = ? OR (state = ? AND drawing_started_at < ?))`,
			AvatarDrawing, now(), now(), discordUserID, AvatarWaitingForDrawing, AvatarDrawing, stale)
		if err != nil {
			return fmt.Errorf("start avatar drawing: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("%w (it must be waiting for a drawing)", ErrAvatarState)
		}
		return nil
	})
}

// SaveAvatarDrawing stores a finished drawing for the person to approve.
func (s *Store) SaveAvatarDrawing(discordUserID, drawingCode string, imageWebP []byte) error {
	return s.changeAvatar(discordUserID, "drawing_saved", "", func(tx *sql.Tx) error {
		return moveAvatar(tx, discordUserID, []string{AvatarDrawing},
			`state = ?, drawing_code = ?, image_webp = ?, drawn_at = ?`, AvatarReadyForApproval, drawingCode, imageWebP, now())
	})
}

// FailAvatarDrawing records a drawing that did not come out, and why.
func (s *Store) FailAvatarDrawing(discordUserID, reason string) error {
	return s.changeAvatar(discordUserID, "drawing_failed", reason, func(tx *sql.Tx) error {
		return moveAvatar(tx, discordUserID, []string{AvatarDrawing}, `state = ?, failure = ?`, AvatarDrawingFailed, reason)
	})
}

// RedrawAvatar sends the same photo back to be drawn again.
func (s *Store) RedrawAvatar(discordUserID string) error {
	return s.changeAvatar(discordUserID, "redraw_asked", "", func(tx *sql.Tx) error {
		return moveAvatar(tx, discordUserID, []string{AvatarReadyForApproval, AvatarDrawingFailed},
			`state = ?, drawing_code = '', image_webp = NULL, failure = '', drawn_at = 0`, AvatarWaitingForDrawing)
	})
}

// ApproveAvatar lets the pages show the drawing, and forgets the photo. The
// caller purges the photo from file-store before calling this, so a photo
// that could not be purged leaves the avatar unapproved rather than the
// photo kept with nothing pointing at it.
func (s *Store) ApproveAvatar(discordUserID string) error {
	return s.changeAvatar(discordUserID, "approved", "", func(tx *sql.Tx) error {
		return moveAvatar(tx, discordUserID, []string{AvatarReadyForApproval},
			`state = ?, approved_at = ?, photo_file_id = ''`, AvatarApproved, now())
	})
}

// RemoveAvatar deletes the avatar. The history stays.
func (s *Store) RemoveAvatar(discordUserID string) error {
	return s.changeAvatar(discordUserID, "removed", "", func(tx *sql.Tx) error {
		result, err := tx.Exec(`DELETE FROM avatars WHERE discord_user_id = ?`, discordUserID)
		if err != nil {
			return fmt.Errorf("remove avatar: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- web pages

// EnableAvatars turns on the avatar pages, with file-store to keep photos
// in. Without it the avatar page says it is not set up.
func (s *Server) EnableAvatars(files *FileStoreClient) {
	s.files = files
}

// mayHaveAvatar is whether someone may upload a photo: a member of a server
// the bot is in, so that a stranger with a Discord account cannot spend the
// host's drawing time.
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

func (s *Server) handleWebAvatar(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	data := pageData{Title: "Your avatar", Session: session, Notice: r.URL.Query().Get("notice")}
	var problems []string
	if s.files == nil {
		problems = append(problems, "Avatars are not set up on this server: "+ErrFileStoreNotConfigured.Error())
	}
	mayHave, err := s.mayHaveAvatar(session)
	if err != nil {
		problems = append(problems, "Could not check your servers: "+err.Error())
	}
	data.AvatarMayUpload = mayHave && s.files != nil
	avatar, err := s.store.AvatarOf(session.DiscordUserID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		problems = append(problems, "Could not load your avatar: "+err.Error())
	}
	data.Avatar = avatar
	if data.AvatarDrawingsLeft, err = s.avatarDrawingsLeftToday(session.DiscordUserID); err != nil {
		problems = append(problems, "Could not count your drawings today: "+err.Error())
	}
	data.AvatarDrawingsPerDay = avatarDrawingsPerDay
	data.Error = strings.Join(problems, " ")
	s.render(w, "avatar.html", data)
}

func avatarRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/avatar?"+noticeQuery(notice), http.StatusSeeOther)
}

// handleWebAvatarPhoto takes an uploaded photo and the consent that comes
// with it, and queues a drawing.
func (s *Server) handleWebAvatarPhoto(w http.ResponseWriter, r *http.Request) {
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
	if r.FormValue("consent") != "yes" {
		avatarRedirect(w, r, "Tick the box to say an avatar may be drawn from your photo and shown with your name.")
		return
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
	file, _, err := r.FormFile("photo")
	if err != nil {
		avatarRedirect(w, r, "Choose a photo to upload.")
		return
	}
	defer file.Close()
	photo, err := io.ReadAll(io.LimitReader(file, avatarPhotoMaximumBytes+1))
	if err != nil {
		http.Error(w, "read the upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(photo) > avatarPhotoMaximumBytes {
		avatarRedirect(w, r, fmt.Sprintf("That photo is over %d MB.", avatarPhotoMaximumBytes>>20))
		return
	}
	contentType := http.DetectContentType(photo)
	filename, readable := avatarPhotoTypes[contentType]
	if !readable {
		avatarRedirect(w, r, "That file is not a JPEG, PNG or WebP photo.")
		return
	}
	fileID, err := s.files.UploadAvatarPhoto(session.DiscordUserID, filename, contentType, photo)
	if err != nil {
		log.Printf("[discord-signup] avatar photo for %s: %v", session.DiscordUserID, err)
		avatarRedirect(w, r, "Could not keep your photo: "+err.Error())
		return
	}
	replaced, err := s.store.UploadAvatarPhoto(session.DiscordUserID, fileID)
	if err != nil {
		// The upload is kept nowhere now; do not leave its bytes behind.
		if purgeErr := s.files.PurgePhoto(fileID); purgeErr != nil {
			log.Printf("[discord-signup] purge unrecorded avatar photo %s: %v", fileID, purgeErr)
		}
		http.Error(w, "record the photo: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if replaced != "" {
		if err := s.files.PurgePhoto(replaced); err != nil {
			log.Printf("[discord-signup] purge replaced avatar photo %s of %s: %v", replaced, session.DiscordUserID, err)
		}
	}
	log.Printf("[discord-signup] avatar photo %s uploaded by web:%s", fileID, session.DiscordUserID)
	avatarRedirect(w, r, "Got it. Your avatar is being drawn, which takes up to half an hour. This page shows it when it is ready.")
}

func (s *Server) handleWebAvatarApprove(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	avatar, err := s.store.AvatarOf(session.DiscordUserID)
	if err != nil {
		avatarRedirect(w, r, "You have no avatar to approve.")
		return
	}
	if avatar.State != AvatarReadyForApproval {
		avatarRedirect(w, r, "Your avatar is not ready to approve.")
		return
	}
	if avatar.PhotoFileID != "" {
		if s.files == nil {
			http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := s.files.PurgePhoto(avatar.PhotoFileID); err != nil {
			log.Printf("[discord-signup] purge avatar photo %s of %s: %v", avatar.PhotoFileID, session.DiscordUserID, err)
			avatarRedirect(w, r, "Could not delete your photo, so your avatar is not in use yet: "+err.Error())
			return
		}
	}
	if err := s.store.ApproveAvatar(session.DiscordUserID); err != nil {
		avatarRedirect(w, r, "Could not approve it: "+err.Error())
		return
	}
	log.Printf("[discord-signup] avatar approved by web:%s", session.DiscordUserID)
	avatarRedirect(w, r, "Your avatar now shows beside your name, and your photo is deleted.")
}

func (s *Server) handleWebAvatarRedraw(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
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
	if err := s.store.RedrawAvatar(session.DiscordUserID); err != nil {
		avatarRedirect(w, r, "Could not ask for another drawing: "+err.Error())
		return
	}
	avatarRedirect(w, r, "Your avatar is being drawn again.")
}

// handleWebAvatarRemove deletes the avatar and the photo. The photo goes
// first: an avatar whose photo could not be deleted stays, so the person can
// see it is still there and try again.
func (s *Server) handleWebAvatarRemove(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	avatar, err := s.store.AvatarOf(session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		avatarRedirect(w, r, "You have no avatar.")
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if avatar.PhotoFileID != "" {
		if s.files == nil {
			http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := s.files.PurgePhoto(avatar.PhotoFileID); err != nil {
			log.Printf("[discord-signup] purge avatar photo %s of %s: %v", avatar.PhotoFileID, session.DiscordUserID, err)
			avatarRedirect(w, r, "Could not delete your photo, so nothing was removed: "+err.Error())
			return
		}
	}
	if err := s.store.RemoveAvatar(session.DiscordUserID); err != nil && !errors.Is(err, ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[discord-signup] avatar removed by web:%s", session.DiscordUserID)
	avatarRedirect(w, r, "Your avatar and photo are deleted.")
}

// avatarHTML is the small round avatar shown before a person's name when
// approvedUserIDs holds them. The name beside it says who it is, so the image
// itself is decoration to a screen reader.
func avatarHTML(approvedUserIDs map[string]bool, discordUserID string) template.HTML {
	if !approvedUserIDs[discordUserID] {
		return ""
	}
	return template.HTML(`<img class="avatar" src="/avatars/` + template.HTMLEscapeString(discordUserID) +
		`.webp" alt="" width="28" height="28" loading="lazy">`)
}

func writeAvatarImage(w http.ResponseWriter, image []byte, cacheControl string) {
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", cacheControl)
	w.Write(image)
}

// handleWebOwnAvatarImage shows a person their own drawing, approved or not.
func (s *Server) handleWebOwnAvatarImage(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	image, _, err := s.store.AvatarImage(session.DiscordUserID, false)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeAvatarImage(w, image, "private, no-store")
}

// handleAvatarImage serves an approved avatar at /avatars/{userID}.webp to
// anyone: the person agreed to it being shown with their name.
func (s *Server) handleAvatarImage(w http.ResponseWriter, r *http.Request) {
	userID, isWebP := strings.CutSuffix(r.PathValue("file"), ".webp")
	if !isWebP {
		http.NotFound(w, r)
		return
	}
	image, _, err := s.store.AvatarImage(userID, true)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Short, because a person can remove or replace theirs at any time.
	writeAvatarImage(w, image, "public, max-age=300")
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
	avatars, err := s.store.Avatars(r.URL.Query().Get("state"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"avatars": avatars})
}

func (s *Server) handleAvatarsToDraw(w http.ResponseWriter, r *http.Request) {
	avatars, err := s.store.AvatarsToDraw()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"avatars": avatars})
}

func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	avatar, err := s.store.AvatarOf(r.PathValue("userID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, avatar)
}

// handleAvatarPhoto gives the drawer the photo, only while a drawing is
// under way: nothing else here has a reason to read it.
func (s *Server) handleAvatarPhoto(w http.ResponseWriter, r *http.Request) {
	avatar, err := s.store.AvatarOf(r.PathValue("userID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if avatar.State != AvatarDrawing || avatar.PhotoFileID == "" {
		writeAvatarError(w, fmt.Errorf("%w (the photo is read only while a drawing is under way)", ErrAvatarState))
		return
	}
	if s.files == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": ErrFileStoreNotConfigured.Error()})
		return
	}
	photo, contentType, err := s.files.PhotoContent(avatar.PhotoFileID)
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

func (s *Server) handleAvatarDrawingStarted(w http.ResponseWriter, r *http.Request) {
	if err := s.store.StartAvatarDrawing(r.PathValue("userID")); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isWebP is whether the bytes are a WebP file: RIFF, a size, then WEBP.
func isWebP(image []byte) bool {
	return len(image) > 12 && bytes.Equal(image[:4], []byte("RIFF")) && bytes.Equal(image[8:12], []byte("WEBP"))
}

func (s *Server) handleSaveAvatarDrawing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DrawingCode string `json:"drawing_code"`
		// ImageWebP is the print, base64 in JSON as encoding/json does []byte.
		ImageWebP []byte `json:"image_webp"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*avatarImageMaximumBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	switch {
	case strings.TrimSpace(body.DrawingCode) == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drawing_code is required"})
		return
	case !isWebP(body.ImageWebP):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "image_webp is not a WebP image"})
		return
	case len(body.ImageWebP) > avatarImageMaximumBytes:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("image_webp is over %d bytes", avatarImageMaximumBytes)})
		return
	}
	if err := s.store.SaveAvatarDrawing(r.PathValue("userID"), body.DrawingCode, body.ImageWebP); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAvatarDrawingFailed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"reason\": \"…\"} is required"})
		return
	}
	if err := s.store.FailAvatarDrawing(r.PathValue("userID"), body.Reason); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAvatarImageForMachines serves any drawing, approved or not, by id:
// what a picture of who is going reads.
func (s *Server) handleAvatarImageForMachines(w http.ResponseWriter, r *http.Request) {
	image, _, err := s.store.AvatarImage(r.PathValue("userID"), false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeAvatarImage(w, image, "no-store")
}
