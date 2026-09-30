package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Mascots: each server's own mascot, the character in the masthead and on
// the home page while a viewer has that server chosen, and on that server's
// event pages. A server with none shows the site's own, Maleeha
// (art/drawings/maleeha.js). The server's owner, or a site admin, sets it:
// either a member's avatar that the member shows beside their name, or a
// drawing made for the server — from words alone, from a photo, or as a
// change to one made before — which cmd/discord-character-drawer draws as a
// character, the same way it draws avatars. The header's script moves the
// mascot when the viewer does something (mascotReactionQuery).

// The kinds of mascot drawing an owner can ask for.
const (
	// MascotRequestDescribe draws from the comment alone: the mascot need not
	// be a person.
	MascotRequestDescribe = "describe"
	// MascotRequestFromPhoto draws from a photo uploaded with the request.
	MascotRequestFromPhoto = "from_photo"
	// MascotRequestEditDrawing changes one of the server's mascot drawings as
	// the comment asks.
	MascotRequestEditDrawing = "edit_drawing"
)

var mascotRequestKinds = map[string]bool{MascotRequestDescribe: true, MascotRequestFromPhoto: true, MascotRequestEditDrawing: true}

// mascotDrawingsPerDay is how many mascot drawings one server may ask for in
// a day. Each costs as much as an avatar.
const mascotDrawingsPerDay = 6

// GuildMascot is the drawing a server shows as its mascot.
type GuildMascot struct {
	GuildID string `json:"guild_id"`
	// MascotDrawingID is a drawing made for the server; AvatarDrawingID a
	// member's avatar, whose is AvatarOwnerID. One of the two is 0.
	MascotDrawingID int64  `json:"mascot_drawing_id"`
	AvatarDrawingID int64  `json:"avatar_drawing_id"`
	AvatarOwnerID   string `json:"avatar_owner_id,omitempty"`
	Format          string `json:"format"`
	SetBy           string `json:"set_by"`
	SetAt           int64  `json:"set_at"`
}

// MascotDrawing is one drawing made for a server's mascot, without its print.
type MascotDrawing struct {
	ID          int64  `json:"id"`
	GuildID     string `json:"guild_id"`
	Format      string `json:"format"`
	DrawingCode string `json:"drawing_code,omitempty"`
	RequestID   int64  `json:"request_id"`
	CreatedAt   int64  `json:"created_at"`
	// Kind, Comment and RequestedBy are the request that made it.
	Kind        string `json:"kind"`
	Comment     string `json:"comment"`
	RequestedBy string `json:"requested_by"`
	// Number is its place among the server's drawings, 1 the first drawn.
	Number int `json:"-"`
}

// MascotRequest is one mascot drawing someone asked for.
type MascotRequest struct {
	ID            int64  `json:"id"`
	GuildID       string `json:"guild_id"`
	RequestedBy   string `json:"requested_by"`
	Kind          string `json:"kind"`
	BaseDrawingID int64  `json:"base_drawing_id"`
	Comment       string `json:"comment"`
	PhotoFileID   string `json:"-"`
	State         string `json:"state"`
	Failure       string `json:"failure,omitempty"`
	RequestedAt   int64  `json:"requested_at"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at"`
	DrawingID     int64  `json:"drawing_id"`
}

// Open reports whether the request is still to be drawn or being drawn.
func (r *MascotRequest) Open() bool {
	return r.State == AvatarRequestWaiting || r.State == AvatarRequestDrawing
}

// MascotAvatarChoice is a member's shown avatar that may be made a server's
// mascot. DisplayName is for the page only.
type MascotAvatarChoice struct {
	DiscordUserID string
	DrawingID     int64
	Format        string
	DisplayName   string
}

// ---------------------------------------------------------------- store

// changeGuildMascot runs change inside one transaction and records action in
// the server's mascot history in the same one.
func (s *Store) changeGuildMascot(guildID, actor, action, detail string, change func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return err
	}
	if err := recordGuildMascotUpdate(tx, guildID, actor, action, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func recordGuildMascotUpdate(tx *sql.Tx, guildID, actor, action, detail string) error {
	if _, err := tx.Exec(`INSERT INTO guild_mascot_updates (guild_id, discord_user_id, action, detail, at) VALUES (?, ?, ?, ?, ?)`,
		guildID, actor, action, detail, now()); err != nil {
		return fmt.Errorf("record mascot update: %w", err)
	}
	return nil
}

const guildMascotSelect = `SELECT g.guild_id, g.mascot_drawing_id, g.avatar_drawing_id, COALESCE(a.discord_user_id, ''),
		COALESCE(m.format, a.format), g.set_by, g.set_at
	FROM guild_mascots g
	LEFT JOIN mascot_drawings m ON g.mascot_drawing_id != 0 AND m.id = g.mascot_drawing_id
	LEFT JOIN avatar_drawings a ON g.avatar_drawing_id != 0 AND a.id = g.avatar_drawing_id
	WHERE (m.id IS NOT NULL OR a.id IS NOT NULL)`

func scanGuildMascot(row interface{ Scan(...any) error }) (*GuildMascot, error) {
	var m GuildMascot
	err := row.Scan(&m.GuildID, &m.MascotDrawingID, &m.AvatarDrawingID, &m.AvatarOwnerID, &m.Format, &m.SetBy, &m.SetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan guild mascot: %w", err)
	}
	return &m, nil
}

// GuildMascotOf is the server's mascot, or ErrNotFound when it shows the
// site's own.
func (s *Store) GuildMascotOf(guildID string) (*GuildMascot, error) {
	return scanGuildMascot(s.db.QueryRow(guildMascotSelect+` AND g.guild_id = ?`, guildID))
}

// GuildMascotImage is one print of the server's mascot: its round portrait,
// or the whole character when fullBody (none for a portrait drawing).
func (s *Store) GuildMascotImage(guildID string, fullBody bool) ([]byte, error) {
	column := "image_webp"
	if fullBody {
		column = "full_body_webp"
	}
	var image []byte
	err := s.db.QueryRow(`SELECT COALESCE(m.`+column+`, a.`+column+`) FROM guild_mascots g
		LEFT JOIN mascot_drawings m ON g.mascot_drawing_id != 0 AND m.id = g.mascot_drawing_id
		LEFT JOIN avatar_drawings a ON g.avatar_drawing_id != 0 AND a.id = g.avatar_drawing_id
		WHERE g.guild_id = ?`, guildID).Scan(&image)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && image == nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read mascot print: %w", err)
	}
	return image, nil
}

// setGuildMascot makes one drawing the server's mascot.
func setGuildMascot(tx *sql.Tx, guildID, actor string, mascotDrawingID, avatarDrawingID int64) error {
	_, err := tx.Exec(`INSERT INTO guild_mascots (guild_id, mascot_drawing_id, avatar_drawing_id, set_by, set_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET mascot_drawing_id = excluded.mascot_drawing_id,
			avatar_drawing_id = excluded.avatar_drawing_id, set_by = excluded.set_by, set_at = excluded.set_at`,
		guildID, mascotDrawingID, avatarDrawingID, actor, now())
	if err != nil {
		return fmt.Errorf("set guild mascot: %w", err)
	}
	return nil
}

// SetGuildMascotToMascotDrawing makes one of the server's mascot drawings its
// mascot.
func (s *Store) SetGuildMascotToMascotDrawing(guildID, actor string, drawingID int64) error {
	return s.changeGuildMascot(guildID, actor, "chose_mascot_drawing", strconv.FormatInt(drawingID, 10), func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRow(`SELECT guild_id FROM mascot_drawings WHERE id = ?`, drawingID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || owner != guildID {
			return fmt.Errorf("%w: that drawing is not one of this server's", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read mascot drawing: %w", err)
		}
		return setGuildMascot(tx, guildID, actor, drawingID, 0)
	})
}

// memberAvatarChoiceQuery lists the drawings members of a server show beside
// their names: the people member_names knows in it who have not left, and
// alsoMember, whom the caller knows to be in it.
const memberAvatarChoiceQuery = `SELECT p.discord_user_id, d.id, d.format, COALESCE(n.display_name, '')
	FROM avatar_people p
	JOIN avatar_drawings d ON d.id = p.chosen_drawing_id
	LEFT JOIN member_names n ON n.guild_id = ? AND n.discord_user_id = p.discord_user_id AND n.left_guild = 0
	WHERE (n.discord_user_id IS NOT NULL OR p.discord_user_id = ?)`

// MascotAvatarChoices is every member's shown avatar that may be made the
// server's mascot, by name.
func (s *Store) MascotAvatarChoices(guildID, alsoMember string) ([]MascotAvatarChoice, error) {
	rows, err := s.db.Query(memberAvatarChoiceQuery+` ORDER BY n.display_name, p.discord_user_id`, guildID, alsoMember)
	if err != nil {
		return nil, fmt.Errorf("list member avatars: %w", err)
	}
	defer rows.Close()
	out := []MascotAvatarChoice{}
	for rows.Next() {
		var c MascotAvatarChoice
		if err := rows.Scan(&c.DiscordUserID, &c.DrawingID, &c.Format, &c.DisplayName); err != nil {
			return nil, fmt.Errorf("scan member avatar: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetGuildMascotToAvatar makes a member's avatar the server's mascot. It must
// be the drawing they show beside their name, and they must be in the
// server: a drawing they keep to themselves is not the owner's to publish.
// alsoMember is someone the caller knows to be in the server.
func (s *Store) SetGuildMascotToAvatar(guildID, actor, alsoMember string, avatarDrawingID int64) error {
	return s.changeGuildMascot(guildID, actor, "chose_member_avatar", strconv.FormatInt(avatarDrawingID, 10), func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRow(`SELECT 1 FROM (`+memberAvatarChoiceQuery+`) WHERE id = ?`, guildID, alsoMember, avatarDrawingID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: that is not an avatar a member of this server shows", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("check member avatar: %w", err)
		}
		return setGuildMascot(tx, guildID, actor, 0, avatarDrawingID)
	})
}

// ClearGuildMascot puts the site's own mascot back on the server.
func (s *Store) ClearGuildMascot(guildID, actor string) error {
	return s.changeGuildMascot(guildID, actor, "cleared", "", func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM guild_mascots WHERE guild_id = ?`, guildID); err != nil {
			return fmt.Errorf("clear guild mascot: %w", err)
		}
		return nil
	})
}

// dropMascotsShowingAvatarDrawings deletes every server's mascot choice that
// shows one of a person's avatar drawings — one, or all of them when
// drawingID is 0 — because the person deleted it. Called inside the avatar
// change that deletes them.
func dropMascotsShowingAvatarDrawings(tx *sql.Tx, discordUserID string, drawingID int64) error {
	rows, err := tx.Query(`SELECT g.guild_id, g.avatar_drawing_id FROM guild_mascots g
		JOIN avatar_drawings d ON d.id = g.avatar_drawing_id
		WHERE g.avatar_drawing_id != 0 AND d.discord_user_id = ? AND (? = 0 OR d.id = ?)`, discordUserID, drawingID, drawingID)
	if err != nil {
		return fmt.Errorf("find mascots showing the drawing: %w", err)
	}
	type shown struct {
		guildID   string
		drawingID int64
	}
	var found []shown
	for rows.Next() {
		var s shown
		if err := rows.Scan(&s.guildID, &s.drawingID); err != nil {
			rows.Close()
			return fmt.Errorf("scan mascot showing the drawing: %w", err)
		}
		found = append(found, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range found {
		if _, err := tx.Exec(`DELETE FROM guild_mascots WHERE guild_id = ?`, f.guildID); err != nil {
			return fmt.Errorf("clear mascot showing a deleted drawing: %w", err)
		}
		if err := recordGuildMascotUpdate(tx, f.guildID, discordUserID, "avatar_deleted", strconv.FormatInt(f.drawingID, 10)); err != nil {
			return err
		}
	}
	return nil
}

// MascotDrawingsOf is a server's mascot drawings, newest first. withCode
// includes each drawing's code.
func (s *Store) MascotDrawingsOf(guildID string, withCode bool) ([]MascotDrawing, error) {
	code := `''`
	if withCode {
		code = `d.drawing_code`
	}
	rows, err := s.db.Query(`SELECT d.id, d.guild_id, d.format, `+code+`, d.request_id, d.created_at,
			COALESCE(r.kind, ''), COALESCE(r.comment, ''), COALESCE(r.requested_by, '')
		FROM mascot_drawings d LEFT JOIN mascot_requests r ON r.id = d.request_id
		WHERE d.guild_id = ? ORDER BY d.id DESC`, guildID)
	if err != nil {
		return nil, fmt.Errorf("list mascot drawings: %w", err)
	}
	defer rows.Close()
	out := []MascotDrawing{}
	for rows.Next() {
		var d MascotDrawing
		if err := rows.Scan(&d.ID, &d.GuildID, &d.Format, &d.DrawingCode, &d.RequestID, &d.CreatedAt, &d.Kind, &d.Comment, &d.RequestedBy); err != nil {
			return nil, fmt.Errorf("scan mascot drawing: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Number = len(out) - i
	}
	return out, nil
}

// MascotDrawingByID is one mascot drawing with its code, or ErrNotFound.
func (s *Store) MascotDrawingByID(drawingID int64) (*MascotDrawing, error) {
	var d MascotDrawing
	err := s.db.QueryRow(`SELECT id, guild_id, format, drawing_code, request_id, created_at FROM mascot_drawings WHERE id = ?`, drawingID).
		Scan(&d.ID, &d.GuildID, &d.Format, &d.DrawingCode, &d.RequestID, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read mascot drawing: %w", err)
	}
	return &d, nil
}

// MascotDrawingImage is one mascot drawing's print — the portrait, or the
// whole body when fullBody — and whose server's it is.
func (s *Store) MascotDrawingImage(drawingID int64, fullBody bool) (string, []byte, error) {
	var guildID string
	var image []byte
	column := "image_webp"
	if fullBody {
		column = "full_body_webp"
	}
	err := s.db.QueryRow(`SELECT guild_id, `+column+` FROM mascot_drawings WHERE id = ?`, drawingID).Scan(&guildID, &image)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("read mascot drawing image: %w", err)
	}
	return guildID, image, nil
}

// DeleteMascotDrawing deletes one of the server's mascot drawings. If it is
// the mascot, the server shows the site's own again.
func (s *Store) DeleteMascotDrawing(guildID, actor string, drawingID int64) error {
	return s.changeGuildMascot(guildID, actor, "deleted_drawing", strconv.FormatInt(drawingID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`DELETE FROM mascot_drawings WHERE id = ? AND guild_id = ?`, drawingID, guildID)
		if err != nil {
			return fmt.Errorf("delete mascot drawing: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: that drawing is not one of this server's", ErrNotFound)
		}
		if _, err := tx.Exec(`DELETE FROM guild_mascots WHERE guild_id = ? AND mascot_drawing_id = ?`, guildID, drawingID); err != nil {
			return fmt.Errorf("clear deleted mascot: %w", err)
		}
		return nil
	})
}

const mascotRequestColumns = `id, guild_id, requested_by, kind, base_drawing_id, comment, photo_file_id, state, failure,
	requested_at, started_at, finished_at, drawing_id`

func scanMascotRequest(row interface{ Scan(...any) error }) (*MascotRequest, error) {
	var r MascotRequest
	err := row.Scan(&r.ID, &r.GuildID, &r.RequestedBy, &r.Kind, &r.BaseDrawingID, &r.Comment, &r.PhotoFileID, &r.State, &r.Failure,
		&r.RequestedAt, &r.StartedAt, &r.FinishedAt, &r.DrawingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan mascot request: %w", err)
	}
	return &r, nil
}

// MascotRequestByID is one request, or ErrNotFound.
func (s *Store) MascotRequestByID(requestID int64) (*MascotRequest, error) {
	return scanMascotRequest(s.db.QueryRow(`SELECT `+mascotRequestColumns+` FROM mascot_requests WHERE id = ?`, requestID))
}

// LatestMascotRequestOf is a server's newest mascot request, or ErrNotFound.
func (s *Store) LatestMascotRequestOf(guildID string) (*MascotRequest, error) {
	return scanMascotRequest(s.db.QueryRow(`SELECT `+mascotRequestColumns+` FROM mascot_requests
		WHERE guild_id = ? ORDER BY id DESC LIMIT 1`, guildID))
}

// MascotRequestsToDraw is every request waiting, and every drawing started
// longer ago than avatarRequestStaleAfter, oldest first.
func (s *Store) MascotRequestsToDraw() ([]MascotRequest, error) {
	rows, err := s.db.Query(`SELECT `+mascotRequestColumns+` FROM mascot_requests
		WHERE state = ? OR (state = ? AND started_at < ?) ORDER BY id`,
		AvatarRequestWaiting, AvatarRequestDrawing, now()-int64(avatarRequestStaleAfter/time.Second))
	if err != nil {
		return nil, fmt.Errorf("list mascot requests to draw: %w", err)
	}
	defer rows.Close()
	out := []MascotRequest{}
	for rows.Next() {
		r, err := scanMascotRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// MascotDrawingsAskedForSince counts the mascot drawings a server asked for
// since a moment.
func (s *Store) MascotDrawingsAskedForSince(guildID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mascot_requests WHERE guild_id = ? AND requested_at >= ?`, guildID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count mascot drawings: %w", err)
	}
	return n, nil
}

// mascotRequestAsked is a mascot request as someone makes it.
type mascotRequestAsked struct {
	Kind          string
	BaseDrawingID int64
	Comment       string
	// PhotoFileID is the photo uploaded with a from_photo request.
	PhotoFileID string
}

// RequestMascotDrawing records a mascot drawing asked for. It refuses a
// second while one is under way, a description with no words, a photo
// request with no photo, and a change to a drawing not the server's.
func (s *Store) RequestMascotDrawing(guildID, actor string, asked mascotRequestAsked) (requestID int64, err error) {
	if !mascotRequestKinds[asked.Kind] {
		return 0, fmt.Errorf("%w: kind %q is not one of describe, from_photo, edit_drawing", ErrInvalidEvent, asked.Kind)
	}
	err = s.changeGuildMascot(guildID, actor, "requested_"+asked.Kind, asked.PhotoFileID, func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM mascot_requests WHERE guild_id = ? AND state IN (?, ?)`,
			guildID, AvatarRequestWaiting, AvatarRequestDrawing).Scan(&open); err != nil {
			return fmt.Errorf("read open mascot requests: %w", err)
		}
		if open > 0 {
			return fmt.Errorf("%w (a mascot drawing is already under way)", ErrAvatarState)
		}
		baseDrawingID := int64(0)
		switch asked.Kind {
		case MascotRequestDescribe:
			if strings.TrimSpace(asked.Comment) == "" {
				return fmt.Errorf("%w: say what the mascot should look like", ErrInvalidEvent)
			}
		case MascotRequestFromPhoto:
			if asked.PhotoFileID == "" {
				return fmt.Errorf("%w: a photo is required", ErrInvalidEvent)
			}
		case MascotRequestEditDrawing:
			if strings.TrimSpace(asked.Comment) == "" {
				return fmt.Errorf("%w: say how the drawing should change", ErrInvalidEvent)
			}
			var owner string
			err := tx.QueryRow(`SELECT guild_id FROM mascot_drawings WHERE id = ?`, asked.BaseDrawingID).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) || owner != guildID {
				return fmt.Errorf("%w: that drawing is not one of this server's", ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("read mascot drawing to change: %w", err)
			}
			baseDrawingID = asked.BaseDrawingID
		}
		result, err := tx.Exec(`INSERT INTO mascot_requests (guild_id, requested_by, kind, base_drawing_id, comment, photo_file_id, state, requested_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, guildID, actor, asked.Kind, baseDrawingID, asked.Comment, asked.PhotoFileID,
			AvatarRequestWaiting, now())
		if err != nil {
			return fmt.Errorf("store mascot request: %w", err)
		}
		requestID, err = result.LastInsertId()
		return err
	})
	return requestID, err
}

// moveMascotRequest changes a request only from the given state, so two
// drawers cannot both take it and a drawing nobody started is not saved.
func moveMascotRequest(tx *sql.Tx, requestID int64, fromState, set string, args ...any) error {
	result, err := tx.Exec(`UPDATE mascot_requests SET `+set+` WHERE id = ? AND state = ?`, append(args, requestID, fromState)...)
	if err != nil {
		return fmt.Errorf("update mascot request: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w (the request must be %s)", ErrAvatarState, fromState)
	}
	return nil
}

// StartMascotRequest marks a drawing under way. It refuses one that is not
// waiting, unless a drawer run that died left it under way for too long.
func (s *Store) StartMascotRequest(requestID int64) error {
	request, err := s.MascotRequestByID(requestID)
	if err != nil {
		return err
	}
	return s.changeGuildMascot(request.GuildID, "drawer", "drawing_started", strconv.FormatInt(requestID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`UPDATE mascot_requests SET state = ?, started_at = ?, failure = ''
			WHERE id = ? AND (state = ? OR (state = ? AND started_at < ?))`,
			AvatarRequestDrawing, now(), requestID, AvatarRequestWaiting, AvatarRequestDrawing,
			now()-int64(avatarRequestStaleAfter/time.Second))
		if err != nil {
			return fmt.Errorf("start mascot request: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("%w (the request must be waiting)", ErrAvatarState)
		}
		return nil
	})
}

// FinishMascotRequest adds the finished drawing to the server's mascot
// drawings. A server with no mascot of its own gets it as its mascot; one
// with a mascot keeps it until someone chooses. It returns the request's
// photo, for the caller to purge.
func (s *Store) FinishMascotRequest(requestID int64, drawing newAvatarDrawing) (drawingID int64, photoFileID string, err error) {
	if drawing.Format != AvatarFormatCharacter {
		return 0, "", fmt.Errorf("%w: a mascot is drawn as a character", ErrInvalidEvent)
	}
	request, err := s.MascotRequestByID(requestID)
	if err != nil {
		return 0, "", err
	}
	err = s.changeGuildMascot(request.GuildID, "drawer", "drawing_saved", strconv.FormatInt(requestID, 10), func(tx *sql.Tx) error {
		result, err := tx.Exec(`INSERT INTO mascot_drawings (guild_id, format, drawing_code, image_webp, full_body_webp, request_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, request.GuildID, drawing.Format, drawing.Code, drawing.ImageWebP, drawing.FullBodyWebP, requestID, now())
		if err != nil {
			return fmt.Errorf("store mascot drawing: %w", err)
		}
		if drawingID, err = result.LastInsertId(); err != nil {
			return err
		}
		if err := moveMascotRequest(tx, requestID, AvatarRequestDrawing, `state = ?, finished_at = ?, drawing_id = ?`,
			AvatarRequestDone, now(), drawingID); err != nil {
			return err
		}
		var hasMascot int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM guild_mascots WHERE guild_id = ?`, request.GuildID).Scan(&hasMascot); err != nil {
			return fmt.Errorf("read guild mascot: %w", err)
		}
		if hasMascot == 0 {
			return setGuildMascot(tx, request.GuildID, request.RequestedBy, drawingID, 0)
		}
		return nil
	})
	return drawingID, request.PhotoFileID, err
}

// FailMascotRequest records a drawing that did not come out, and why. It
// returns the request's photo, for the caller to purge.
func (s *Store) FailMascotRequest(requestID int64, reason string) (photoFileID string, err error) {
	request, err := s.MascotRequestByID(requestID)
	if err != nil {
		return "", err
	}
	err = s.changeGuildMascot(request.GuildID, "drawer", "drawing_failed", reason, func(tx *sql.Tx) error {
		return moveMascotRequest(tx, requestID, AvatarRequestDrawing, `state = ?, failure = ?, finished_at = ?`,
			AvatarRequestFailed, reason, now())
	})
	return request.PhotoFileID, err
}

// ForgetMascotRequestPhoto clears a request's photo id once file-store has
// purged it.
func (s *Store) ForgetMascotRequestPhoto(requestID int64) error {
	if _, err := s.db.Exec(`UPDATE mascot_requests SET photo_file_id = '' WHERE id = ?`, requestID); err != nil {
		return fmt.Errorf("forget mascot photo: %w", err)
	}
	return nil
}
