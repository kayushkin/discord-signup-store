package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Roles: things people going can sign themselves up for — a job such as
// setup or teardown, or coming for part of the event — each with an optional
// cap. An organizer switches them on for an event (events.roles_enabled) and
// adds them on the event page; anyone going takes or leaves one with its own
// button on the event's card, in the What I'm bringing view when the event
// also has a potluck sheet, or on the event page. They are not Discord
// roles: nothing is granted on Discord, and nobody but this service keeps
// them.
//
// Roles have their own switch, apart from the potluck sheet's: either can be
// on alone. Taking a role is open while roles are on and the event is
// neither over nor canceled, and is not an answer to what someone is
// bringing. The tables keep their sheet_ names from when roles were part of
// the sheet.

const (
	// sheetRolesPerEvent keeps the roles to one row of buttons, which is
	// what a Discord message has room for beside the others.
	sheetRolesPerEvent = 5
	sheetRoleNameLimit = 40
	sheetRoleCapLimit  = 999
	// sheetRoleEmoji heads the roles on every surface; each role has its
	// own emoji besides.
	sheetRoleEmoji = "🙋"
)

// sheetRoleEmojiDefaults are the emojis new roles take, in this order: the
// first no live role on the event has. An organizer can pick any of the
// server's emojis or the standard ones instead (roleemoji.go).
func sheetRoleEmojiDefaults() []string { return []string{"🎯", "⭐", "🎉", "🎁", "🎨"} }

// nextSheetRoleEmoji is the first default no live role on the event has, or
// the first default when every one is taken.
func nextSheetRoleEmoji(roles []SheetRole) string {
	used := map[string]bool{}
	for _, r := range roles {
		used[r.Emoji] = true
	}
	for _, e := range sheetRoleEmojiDefaults() {
		if !used[e] {
			return e
		}
	}
	return sheetRoleEmojiDefaults()[0]
}

// sheetRoleEmojiBackfill gives roles from before emojis the defaults, by
// the order each event's roles were added.
func sheetRoleEmojiBackfill() string {
	cases := ""
	for i, e := range sheetRoleEmojiDefaults() {
		cases += fmt.Sprintf(" WHEN %d THEN '%s'", i, e)
	}
	return `UPDATE sheet_roles SET emoji = (SELECT CASE ((x.n - 1) % ` + strconv.Itoa(sheetRolesPerEvent) + `)` + cases + ` END
		FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY id) AS n FROM sheet_roles) x
		WHERE x.id = sheet_roles.id) WHERE emoji = ''`
}


// ErrSheetRoleFull is taking a role that has no room.
var ErrSheetRoleFull = errors.New("that role is full")

// ErrSheetRolesOff is taking a role on an event whose roles are off.
var ErrSheetRolesOff = errors.New("this event has no roles to sign up to")

// ErrSheetRolesClosed is taking a role on an event that is over or
// canceled.
var ErrSheetRolesClosed = errors.New("this event is over, so its roles are closed")

// ErrInvalidSheetRole is a role the event cannot take.
var ErrInvalidSheetRole = errors.New("invalid role")

// sheetRolesTakeAnswers says whether people may take or leave roles: roles
// are on, and the event is neither over nor canceled. Closing signups does
// not close them.
func sheetRolesTakeAnswers(ev *Event) bool {
	return ev.RolesEnabled && (ev.Status == StatusOpen || ev.Status == StatusClosed)
}

// SheetRole is one role on an event's sheet, with the people going in it.
type SheetRole struct {
	ID       int64  `json:"id"`
	EventID  int64  `json:"event_id"`
	Name     string `json:"name"`
	Emoji    string `json:"emoji"`
	Capacity int    `json:"capacity"`
	// Members are the Discord user ids of the people going in it, in the
	// order they took it.
	Members []string `json:"members"`
}

// Has says whether someone is in the role.
func (r SheetRole) Has(userID string) bool {
	for _, id := range r.Members {
		if id == userID {
			return true
		}
	}
	return false
}

// Full says whether a capped role has no room.
func (r SheetRole) Full() bool { return r.Capacity > 0 && len(r.Members) >= r.Capacity }

// Count is "7/12", or "7" with no cap.
func (r SheetRole) Count() string {
	if r.Capacity > 0 {
		return fmt.Sprintf("%d/%d", len(r.Members), r.Capacity)
	}
	return strconv.Itoa(len(r.Members))
}

// sheetRolesLine is the roles in one line: "🙋 Setup 7/12 · Teardown 3/12",
// or "" with no roles.
func sheetRolesLine(roles []SheetRole) string {
	if len(roles) == 0 {
		return ""
	}
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = roleEmojiPlainText(r.Emoji) + " " + r.Name + " " + r.Count()
	}
	return strings.Join(parts, " · ")
}

// sheetRoleOf finds one role by id.
func sheetRoleOf(roles []SheetRole, roleID int64) (SheetRole, bool) {
	for _, r := range roles {
		if r.ID == roleID {
			return r, true
		}
	}
	return SheetRole{}, false
}

// writeSheetRolesSection adds the roles to an event's card: each role, its
// count, and who is in it.
func writeSheetRolesSection(b *strings.Builder, roles []SheetRole, open bool) {
	if len(roles) == 0 {
		return
	}
	b.WriteString("\n**🙋 Roles**")
	if open {
		b.WriteString(" — press one below to sign up")
	}
	b.WriteString("\n")
	for _, r := range roles {
		fmt.Fprintf(b, "%s **%s** %s", r.Emoji, r.Name, r.Count())
		if len(r.Members) > 0 {
			mentions := make([]string, len(r.Members))
			for i, id := range r.Members {
				mentions[i] = "<@" + id + ">"
			}
			b.WriteString(": " + strings.Join(mentions, ", "))
		}
		b.WriteString("\n")
	}
}

// sheetRoleRefusal says plainly why a role was not taken or left.
func sheetRoleRefusal(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "Nothing was changed: that role is no longer on the event."
	case errors.Is(err, ErrSheetRoleFull), errors.Is(err, ErrSheetRolesOff), errors.Is(err, ErrSheetRolesClosed),
		errors.Is(err, ErrAlreadyInSheetRole), errors.Is(err, ErrNotInSheetRole):
		return "Nothing was changed: " + err.Error() + "."
	case errors.Is(err, ErrNotGoing):
		return "Nothing was changed: roles are for people going. Join first, then sign up for one."
	case errors.Is(err, ErrInvalidSheetRole):
		return "Nothing was changed: " + strings.TrimPrefix(err.Error(), ErrInvalidSheetRole.Error()+": ") + "."
	}
	return "Something went wrong. Nothing was changed — try again."
}

// readSheetRoles reads an event's live roles and the people going in each.
func (s *Store) readSheetRoles(eventID int64) ([]SheetRole, error) {
	rows, err := s.db.Query(`SELECT id, event_id, name, emoji, capacity FROM sheet_roles
		WHERE event_id = ? AND removed_at = 0 ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read sheet roles: %w", err)
	}
	var roles []SheetRole
	index := map[int64]int{}
	for rows.Next() {
		var r SheetRole
		if err := rows.Scan(&r.ID, &r.EventID, &r.Name, &r.Emoji, &r.Capacity); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan sheet role: %w", err)
		}
		r.Members = []string{}
		index[r.ID] = len(roles)
		roles = append(roles, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sheet roles: %w", err)
	}
	members, err := s.db.Query(`
		SELECT m.role_id, m.discord_user_id FROM sheet_role_signups m
		JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
		WHERE m.event_id = ? AND m.left_at = 0 AND s.state = ?
		ORDER BY m.joined_at, m.id`, eventID, StateAttending)
	if err != nil {
		return nil, fmt.Errorf("read sheet role members: %w", err)
	}
	defer members.Close()
	for members.Next() {
		var roleID int64
		var userID string
		if err := members.Scan(&roleID, &userID); err != nil {
			return nil, fmt.Errorf("scan sheet role member: %w", err)
		}
		if i, ok := index[roleID]; ok {
			roles[i].Members = append(roles[i].Members, userID)
		}
	}
	return roles, members.Err()
}

// AddSheetRole adds a role to an event's sheet.
func (s *Store) AddSheetRole(eventID int64, name string, capacity int, actor string) (*SheetRole, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len([]rune(name)) > sheetRoleNameLimit {
		return nil, fmt.Errorf("%w: a role's name is 1 to %d characters", ErrInvalidSheetRole, sheetRoleNameLimit)
	}
	if capacity < 0 || capacity > sheetRoleCapLimit {
		return nil, fmt.Errorf("%w: a role's cap is 0, for none, to %d", ErrInvalidSheetRole, sheetRoleCapLimit)
	}
	roles, err := s.readSheetRoles(eventID)
	if err != nil {
		return nil, err
	}
	if len(roles) >= sheetRolesPerEvent {
		return nil, fmt.Errorf("%w: an event has at most %d roles, one row of buttons on Discord", ErrInvalidSheetRole, sheetRolesPerEvent)
	}
	for _, r := range roles {
		if strings.EqualFold(r.Name, name) {
			return nil, fmt.Errorf("%w: there is already a role called %s", ErrInvalidSheetRole, r.Name)
		}
	}
	emoji := nextSheetRoleEmoji(roles)
	res, err := s.db.Exec(`INSERT INTO sheet_roles (event_id, name, emoji, capacity, created_at, created_by) VALUES (?,?,?,?,?,?)`,
		eventID, name, emoji, capacity, now(), actor)
	if err != nil {
		return nil, fmt.Errorf("add sheet role: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read sheet role id: %w", err)
	}
	return &SheetRole{ID: id, EventID: eventID, Name: name, Emoji: emoji, Capacity: capacity, Members: []string{}}, nil
}

// RemoveSheetRole takes a role off an event's sheet. Who was in it stays in
// the history.
func (s *Store) RemoveSheetRole(eventID, roleID int64, actor string) error {
	res, err := s.db.Exec(`UPDATE sheet_roles SET removed_at = ?, removed_by = ?
		WHERE id = ? AND event_id = ? AND removed_at = 0`, now(), actor, roleID, eventID)
	if err != nil {
		return fmt.Errorf("remove sheet role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EditSheetRole changes a role's emoji and cap, recording each change in
// sheet_role_updates. Lowering the cap takes nobody out: the role is over
// its cap until people leave, as an event over its limit is. Returns which
// fields changed.
func (s *Store) EditSheetRole(eventID, roleID int64, emoji string, capacity int, actor string) ([]string, error) {
	if !isRoleEmojiForm(emoji) {
		return nil, fmt.Errorf("%w: pick one of the emojis offered", ErrInvalidSheetRole)
	}
	if capacity < 0 || capacity > sheetRoleCapLimit {
		return nil, fmt.Errorf("%w: a role's cap is 0, for none, to %d", ErrInvalidSheetRole, sheetRoleCapLimit)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var oldEmoji string
	var oldCapacity int
	err = tx.QueryRow(`SELECT emoji, capacity FROM sheet_roles WHERE id = ? AND event_id = ? AND removed_at = 0`,
		roleID, eventID).Scan(&oldEmoji, &oldCapacity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read sheet role: %w", err)
	}
	ts := now()
	var changed []string
	record := func(field, from, to string) error {
		_, err := tx.Exec(`INSERT INTO sheet_role_updates (role_id, event_id, field, from_value, to_value, actor, at)
			VALUES (?,?,?,?,?,?,?)`, roleID, eventID, field, from, to, actor, ts)
		if err != nil {
			return fmt.Errorf("record sheet role %s change: %w", field, err)
		}
		changed = append(changed, field)
		return nil
	}
	if emoji != oldEmoji {
		if err := record("emoji", oldEmoji, emoji); err != nil {
			return nil, err
		}
	}
	if capacity != oldCapacity {
		if err := record("capacity", strconv.Itoa(oldCapacity), strconv.Itoa(capacity)); err != nil {
			return nil, err
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`UPDATE sheet_roles SET emoji = ?, capacity = ? WHERE id = ?`, emoji, capacity, roleID); err != nil {
		return nil, fmt.Errorf("edit sheet role: %w", err)
	}
	return changed, tx.Commit()
}

// ToggleSheetRole puts someone going into a role, or takes them out of it if
// they are in it. The cap is checked in the same transaction that writes the
// row, as a Join's is. Returns whether they are in it now.
func (s *Store) ToggleSheetRole(eventID, roleID int64, userID string) (bool, error) {
	tx, err := s.beginSheetRoleChange(eventID, userID)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var capacity int
	err = tx.QueryRow(`SELECT capacity FROM sheet_roles WHERE id = ? AND event_id = ? AND removed_at = 0`,
		roleID, eventID).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read sheet role: %w", err)
	}
	ts := now()
	res, err := tx.Exec(`UPDATE sheet_role_signups SET left_at = ?, left_by = ?
		WHERE role_id = ? AND discord_user_id = ? AND left_at = 0`, ts, ActorUser, roleID, userID)
	if err != nil {
		return false, fmt.Errorf("leave sheet role: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, tx.Commit()
	}
	if err := joinSheetRoleTx(tx, eventID, roleID, capacity, userID, "", "", ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ErrAlreadyInSheetRole is putting someone in a role they are already in.
var ErrAlreadyInSheetRole = errors.New("they are already in that role")

// ErrNotInSheetRole is taking someone out of a role they are not in.
var ErrNotInSheetRole = errors.New("they are not in that role")

// PutInSheetRole puts someone going into a role on an organizer's say-so.
// The cap holds, as it does for people taking a role themselves: to swap
// someone into a full role, take someone out first. actor is "web:<id>".
func (s *Store) PutInSheetRole(eventID, roleID int64, userID, actor string) error {
	tx, err := s.beginSheetRoleChange(eventID, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var capacity int
	err = tx.QueryRow(`SELECT capacity FROM sheet_roles WHERE id = ? AND event_id = ? AND removed_at = 0`,
		roleID, eventID).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read sheet role: %w", err)
	}
	var in int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sheet_role_signups
		WHERE role_id = ? AND discord_user_id = ? AND left_at = 0`, roleID, userID).Scan(&in); err != nil {
		return fmt.Errorf("read sheet role signup: %w", err)
	}
	if in > 0 {
		return ErrAlreadyInSheetRole
	}
	if err := joinSheetRoleTx(tx, eventID, roleID, capacity, userID, "", actor, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// TakeOutOfSheetRole takes someone out of a role on an organizer's say-so.
// Unlike taking a role, it works whether or not the person is still going
// and whether or not the event is over. actor is "web:<id>".
func (s *Store) TakeOutOfSheetRole(eventID, roleID int64, userID, actor string) error {
	res, err := s.db.Exec(`UPDATE sheet_role_signups SET left_at = ?, left_by = ?
		WHERE role_id = ? AND event_id = ? AND discord_user_id = ? AND left_at = 0`, now(), actor, roleID, eventID, userID)
	if err != nil {
		return fmt.Errorf("take out of sheet role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotInSheetRole
	}
	return nil
}

// beginSheetRoleChange opens a transaction for someone taking or leaving a
// role, refusing an event whose roles are off or closed and a person not
// going.
func (s *Store) beginSheetRoleChange(eventID int64, userID string) (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	var enabled bool
	var status string
	err = tx.QueryRow(`SELECT roles_enabled, status FROM events WHERE id = ? AND deleted_at = 0`, eventID).Scan(&enabled, &status)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return nil, ErrNotFound
	}
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("read event: %w", err)
	}
	if !enabled {
		tx.Rollback()
		return nil, ErrSheetRolesOff
	}
	if !sheetRolesTakeAnswers(&Event{RolesEnabled: enabled, Status: status}) {
		tx.Rollback()
		return nil, ErrSheetRolesClosed
	}
	var state string
	err = tx.QueryRow(`SELECT state FROM signups WHERE event_id = ? AND discord_user_id = ?`, eventID, userID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != StateAttending) {
		tx.Rollback()
		return nil, ErrNotGoing
	}
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("read signup: %w", err)
	}
	return tx, nil
}

// joinSheetRoleTx adds someone to a role if it has room. joinedBy is ""
// when they take it themselves or the spreadsheet copies it, and
// "web:<id>" when an organizer puts them in.
func joinSheetRoleTx(tx *sql.Tx, eventID, roleID int64, capacity int, userID, source, joinedBy string, ts int64) error {
	if capacity > 0 {
		var taken int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM sheet_role_signups m
			JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
			WHERE m.role_id = ? AND m.left_at = 0 AND s.state = ?`, roleID, StateAttending).Scan(&taken); err != nil {
			return fmt.Errorf("count sheet role: %w", err)
		}
		if taken >= capacity {
			return ErrSheetRoleFull
		}
	}
	if _, err := tx.Exec(`INSERT INTO sheet_role_signups (role_id, event_id, discord_user_id, source, joined_at, joined_by)
		VALUES (?,?,?,?,?,?)`, roleID, eventID, userID, source, ts, joinedBy); err != nil {
		return fmt.Errorf("join sheet role: %w", err)
	}
	return nil
}

// roleNameKey is how a spreadsheet column is matched to a role: the name
// before any bracket, ignoring case, so "SETUP (6:00)" finds "Setup (6:00)"
// and "Setup".
func roleNameKey(name string) string {
	if i := strings.Index(name, "("); i >= 0 {
		name = name[:i]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// Roles in Discord components.

const (
	// sheetRoleCardPrefix is a role's button on the event's card; the press
	// is answered privately. sheetRolePanelPrefix is the same in the What
	// I'm bringing view, whose press edits the view.
	sheetRoleCardPrefix  = "potluck-role-"
	sheetRolePanelPrefix = "potluck-rolev-"
)

func sheetRoleCustomID(prefix string, eventID, roleID int64) string {
	return fmt.Sprintf("%s:%s%d:%d", customIDPrefix, prefix, roleID, eventID)
}

// sheetRoleButtons is one button per role, for the card or the view. In the
// view they say whether the viewer is in each.
func sheetRoleButtons(ev *Event, prefix, viewerID string) []any {
	var buttons []any
	for _, r := range ev.Roles {
		label := r.Name + " · " + r.Count()
		style := buttonStyleSecondary
		if viewerID != "" && r.Has(viewerID) {
			label, style = "✓ "+label, buttonStyleSuccess
		}
		buttons = append(buttons, map[string]any{
			"type": componentTypeButton, "style": style, "label": truncate(label, 80),
			"emoji": roleEmojiButton(r.Emoji), "custom_id": sheetRoleCustomID(prefix, ev.ID, r.ID),
		})
	}
	return buttons
}

// sheetRoleRow is the card's row of role buttons, or none.
func sheetRoleRow(ev *Event) []any {
	if !sheetRolesTakeAnswers(ev) || len(ev.Roles) == 0 {
		return nil
	}
	return []any{map[string]any{"type": componentTypeActionRow,
		"components": sheetRoleButtons(ev, sheetRoleCardPrefix, "")}}
}

// handleSheetRoleButton takes or leaves a role from a press on the card or
// in the view.
func (s *Server) handleSheetRoleButton(w http.ResponseWriter, action string, eventID int64, userID string) {
	inView := strings.HasPrefix(action, sheetRolePanelPrefix)
	roleText := strings.TrimPrefix(strings.TrimPrefix(action, sheetRolePanelPrefix), sheetRoleCardPrefix)
	roleID, err := strconv.ParseInt(roleText, 10, 64)
	if err != nil {
		s.replyEphemeral(w, "Unknown role.")
		return
	}
	in, err := s.store.ToggleSheetRole(eventID, roleID, userID)
	note := ""
	if err != nil {
		if !errors.Is(err, ErrSheetRoleFull) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrSheetRolesOff) &&
			!errors.Is(err, ErrSheetRolesClosed) && !errors.Is(err, ErrNotGoing) {
			log.Printf("[discord-signup] role change event=%d user=%s: %v", eventID, userID, err)
		}
		note = sheetRoleRefusal(err)
	} else {
		s.inBackground(func() { s.syncAfterChange(eventID, nil) })
	}
	ev, readErr := s.store.GetEvent(eventID)
	if note == "" {
		name := "the role"
		if readErr == nil {
			if r, ok := sheetRoleOf(ev.Roles, roleID); ok {
				name = fmt.Sprintf("%s (%s)", r.Name, r.Count())
			}
		}
		if in {
			note = "You're down for " + name + "."
		} else {
			note = "Took you off " + name + "."
		}
	}
	// The view is the potluck's; with the sheet since switched off there is
	// none to redraw, so the answer stands alone.
	if inView && readErr == nil && ev.Potluck != nil {
		s.replyPotluckPanel(w, callbackTypeUpdateMessage, eventID, userID, note)
		return
	}
	s.replyEphemeral(w, note)
}

// Roles on the web.

// sheetRolePage is a role as the event pages show it.
type sheetRolePage struct {
	SheetRole
	People []Signup
	// Others are the people going who are not in it, whom an organizer
	// may put in it, in signup order.
	Others []Signup
	// Mine is whether the viewer is in it.
	Mine bool
}

// sheetRolesPage is the event pages' roles section.
type sheetRolesPage struct {
	Roles []sheetRolePage
	// TakesAnswers is whether people may take and leave roles now, and
	// ViewerGoing whether the viewer is going and so may.
	TakesAnswers bool
	ViewerGoing  bool
	// ViewerID marks the viewer's own name tag, which carries their way
	// out of the role.
	ViewerID string
}

// sheetRolesPageOf builds the roles section; nil when roles are off.
func sheetRolesPageOf(ev *Event, going []Signup, viewerID string) *sheetRolesPage {
	if !ev.RolesEnabled {
		return nil
	}
	page := &sheetRolesPage{Roles: sheetRolePages(ev.Roles, going, viewerID), TakesAnswers: sheetRolesTakeAnswers(ev), ViewerID: viewerID}
	for _, sg := range going {
		if sg.DiscordUserID == viewerID {
			page.ViewerGoing = true
		}
	}
	return page
}

func sheetRolePages(roles []SheetRole, going []Signup, viewerID string) []sheetRolePage {
	people := map[string]Signup{}
	for _, sg := range going {
		people[sg.DiscordUserID] = sg
	}
	out := make([]sheetRolePage, 0, len(roles))
	for _, r := range roles {
		page := sheetRolePage{SheetRole: r, Mine: r.Has(viewerID)}
		for _, id := range r.Members {
			page.People = append(page.People, people[id])
		}
		for _, sg := range going {
			if !r.Has(sg.DiscordUserID) {
				page.Others = append(page.Others, sg)
			}
		}
		out = append(out, page)
	}
	return out
}

// handleWebSheetRoleAdd adds a role, for an organizer.
func (s *Server) handleWebSheetRoleAdd(w http.ResponseWriter, r *http.Request) {
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
	capacity := 0
	if text := strings.TrimSpace(r.FormValue("capacity")); text != "" {
		var err error
		if capacity, err = strconv.Atoi(text); err != nil {
			s.redirectWithNotice(w, r, ev.ID, "Not added: the cap must be a whole number, or empty for none.")
			return
		}
	}
	role, err := s.store.AddSheetRole(ev.ID, r.FormValue("name"), capacity, "web:"+session.DiscordUserID)
	if err != nil {
		if !errors.Is(err, ErrInvalidSheetRole) {
			log.Printf("[discord-signup] add role to %d: %v", ev.ID, err)
		}
		s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Added the role "+role.Name+".")
}

// handleWebSheetRoleRemove takes a role off, for an organizer.
func (s *Server) handleWebSheetRoleRemove(w http.ResponseWriter, r *http.Request) {
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
	roleID, err := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err != nil {
		http.Error(w, "role_id must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.store.RemoveSheetRole(ev.ID, roleID, "web:"+session.DiscordUserID); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.redirectWithNotice(w, r, ev.ID, "That role was already off the event.")
			return
		}
		log.Printf("[discord-signup] remove sheet role %d of %d: %v", roleID, ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was removed: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Took the role off the event.")
}

// handleWebEditSheetRole changes a role's emoji and cap, for an organizer.
func (s *Server) handleWebEditSheetRole(w http.ResponseWriter, r *http.Request) {
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
	roleID, err := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err != nil {
		http.Error(w, "role_id must be an integer", http.StatusBadRequest)
		return
	}
	capacity := 0
	if text := strings.TrimSpace(r.FormValue("capacity")); text != "" {
		if capacity, err = strconv.Atoi(text); err != nil {
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: the cap must be a whole number, or empty for none.")
			return
		}
	}
	emoji, err := s.checkRoleEmoji(ev.GuildID, r.FormValue("emoji"))
	if err != nil {
		if !errors.Is(err, ErrInvalidSheetRole) {
			log.Printf("[discord-signup] check role emoji for %d: %v", ev.ID, err)
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: could not check that emoji against the server's: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		return
	}
	changed, err := s.store.EditSheetRole(ev.ID, roleID, emoji, capacity, "web:"+session.DiscordUserID)
	if err != nil {
		if !errors.Is(err, ErrInvalidSheetRole) && !errors.Is(err, ErrNotFound) {
			log.Printf("[discord-signup] edit role %d of %d: %v", roleID, ev.ID, err)
		}
		s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		return
	}
	if len(changed) == 0 {
		s.redirectWithNotice(w, r, ev.ID, "Nothing to change.")
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Saved the role.")
}

// handleWebPutInSheetRole puts someone going into a role, for an organizer.
func (s *Server) handleWebPutInSheetRole(w http.ResponseWriter, r *http.Request) {
	s.organizerSheetRoleChange(w, r, true)
}

// handleWebTakeOutOfSheetRole takes someone out of a role, for an organizer.
func (s *Server) handleWebTakeOutOfSheetRole(w http.ResponseWriter, r *http.Request) {
	s.organizerSheetRoleChange(w, r, false)
}

// organizerSheetRoleChange puts discord_user_id into role_id, or takes them
// out of it, on the say-so of whoever may edit the event.
func (s *Server) organizerSheetRoleChange(w http.ResponseWriter, r *http.Request, putIn bool) {
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
	roleID, err := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err != nil {
		http.Error(w, "role_id must be an integer", http.StatusBadRequest)
		return
	}
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	if userID == "" {
		http.Error(w, "discord_user_id is required", http.StatusBadRequest)
		return
	}
	actor := "web:" + session.DiscordUserID
	if putIn {
		err = s.store.PutInSheetRole(ev.ID, roleID, userID, actor)
	} else {
		err = s.store.TakeOutOfSheetRole(ev.ID, roleID, userID, actor)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrNotGoing):
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: roles are for people going, and they are not.")
		case errors.Is(err, ErrSheetRoleFull):
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: that role is full. Take someone out of it first.")
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrSheetRolesOff), errors.Is(err, ErrSheetRolesClosed),
			errors.Is(err, ErrAlreadyInSheetRole), errors.Is(err, ErrNotInSheetRole):
			s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		default:
			log.Printf("[discord-signup] organizer role change event=%d role=%d user=%s: %v", ev.ID, roleID, userID, err)
			s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		}
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	role, _ := sheetRoleOf(ev.Roles, roleID)
	if putIn {
		s.redirectWithNotice(w, r, ev.ID, "Put them in "+role.Name+".")
		return
	}
	s.redirectWithNotice(w, r, ev.ID, "Took them out of "+role.Name+".")
}

// handleWebMySheetRole takes or leaves a role for the viewer, from either
// event page.
func (s *Server) handleWebMySheetRole(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberFormEvent(w, r)
	if ev == nil {
		return
	}
	roleID, err := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err != nil {
		http.Error(w, "role_id must be an integer", http.StatusBadRequest)
		return
	}
	in, err := s.store.ToggleSheetRole(ev.ID, roleID, session.DiscordUserID)
	if err != nil {
		if !errors.Is(err, ErrSheetRoleFull) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrSheetRolesOff) &&
			!errors.Is(err, ErrSheetRolesClosed) && !errors.Is(err, ErrNotGoing) {
			log.Printf("[discord-signup] web role change event=%d user=%s: %v", ev.ID, session.DiscordUserID, err)
		}
		s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	if in {
		s.redirectWithNotice(w, r, ev.ID, "You're down for it.")
		return
	}
	s.redirectWithNotice(w, r, ev.ID, "Took you off it.")
}

// Roles in the log.

// SheetRoleHistoryRow is one join or leave, or a role added or removed.
type SheetRoleHistoryRow struct {
	RoleName      string
	DiscordUserID string // "" for a role being added or removed
	DisplayName   string
	ReadableName  string
	Source        string
	At            int64
	LeftAt        int64
	By            string // who added the role, or who took them out of it
	JoinedBy      string // who put them in it: "" for themselves or the spreadsheet
	RemovedAt     int64
	RemovedBy     string
	RoleEmoji     string // the role's emoji now
	// Field, FromValue and ToValue are set on an organizer's edit of the
	// role ("emoji" or "capacity"), whose By is the organizer.
	Field, FromValue, ToValue string
}

// SheetRoleHistory is every role an event has had and everyone who
// took one, oldest first.
func (s *Store) SheetRoleHistory(eventID int64) ([]SheetRoleHistoryRow, error) {
	rows, err := s.db.Query(`
		SELECT r.name, '', '', '', '', r.created_at, 0, r.created_by, r.removed_at, r.removed_by, '', r.emoji, '', '', ''
		FROM sheet_roles r WHERE r.event_id = ?
		UNION ALL
		SELECT r.name, m.discord_user_id, COALESCE(s.display_name, ''), COALESCE(n.readable_name, ''), m.source,
		       m.joined_at, m.left_at, m.left_by, 0, '', m.joined_by, r.emoji, '', '', ''
		FROM sheet_role_signups m
		JOIN sheet_roles r ON r.id = m.role_id
		LEFT JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
		LEFT JOIN readable_names n ON n.discord_user_id = m.discord_user_id
		WHERE m.event_id = ?
		UNION ALL
		SELECT r.name, '', '', '', '', u.at, 0, u.actor, 0, '', '', r.emoji, u.field, u.from_value, u.to_value
		FROM sheet_role_updates u
		JOIN sheet_roles r ON r.id = u.role_id
		WHERE u.event_id = ?
		ORDER BY 6`, eventID, eventID, eventID)
	if err != nil {
		return nil, fmt.Errorf("read sheet role history: %w", err)
	}
	defer rows.Close()
	var out []SheetRoleHistoryRow
	for rows.Next() {
		var h SheetRoleHistoryRow
		if err := rows.Scan(&h.RoleName, &h.DiscordUserID, &h.DisplayName, &h.ReadableName, &h.Source,
			&h.At, &h.LeftAt, &h.By, &h.RemovedAt, &h.RemovedBy, &h.JoinedBy, &h.RoleEmoji,
			&h.Field, &h.FromValue, &h.ToValue); err != nil {
			return nil, fmt.Errorf("scan sheet role history: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// sheetRoleCapWords is a stored cap as the log says it: "none" for 0.
func sheetRoleCapWords(value string) template.HTML {
	if value == "0" {
		return template.HTML(`<span class="muted">none</span>`)
	}
	return template.HTML(template.HTMLEscapeString(value))
}

// sheetRoleLogEntries are the roles' lines in the event's log.
func sheetRoleLogEntries(history []SheetRoleHistoryRow, names eventLogNames) []eventLogEntry {
	muted := func(text string) template.HTML {
		return template.HTML(`<span class="muted">` + template.HTMLEscapeString(text) + `</span>`)
	}
	var out []eventLogEntry
	for i, h := range history {
		role := func(name string) template.HTML {
			return roleEmojiHTML(h.RoleEmoji) + " " + template.HTML(template.HTMLEscapeString(name))
		}
		if h.Field != "" {
			what := "changed " + role(h.RoleName) + "'s emoji from " + roleEmojiHTML(h.FromValue) + " to " + roleEmojiHTML(h.ToValue)
			if h.Field == "capacity" {
				what = "changed " + role(h.RoleName) + "'s cap from " + sheetRoleCapWords(h.FromValue) + " to " + sheetRoleCapWords(h.ToValue)
			}
			out = append(out, eventLogEntry{At: h.At, order: 5_000_000 + i, Subject: muted("the roles"),
				What: what, By: names.actor(h.By)})
			continue
		}
		if h.DiscordUserID == "" {
			out = append(out, eventLogEntry{At: h.At, order: 5_000_000 + i, Subject: muted("the roles"),
				What: "added the role " + role(h.RoleName), By: names.actor(h.By)})
			if h.RemovedAt > 0 {
				out = append(out, eventLogEntry{At: h.RemovedAt, order: 6_000_000 + i, Subject: muted("the roles"),
					What: "removed the role " + role(h.RoleName), By: names.actor(h.RemovedBy)})
			}
			continue
		}
		subject := names.person(h.ReadableName, h.DisplayName, h.DiscordUserID)
		by := subject
		what := "took the role " + role(h.RoleName)
		switch {
		case h.Source == potluckSourceSheet:
			by = muted("copied from the spreadsheet")
		case h.JoinedBy != "":
			by = names.actor(h.JoinedBy)
			what = "was put in the role " + role(h.RoleName)
		}
		out = append(out, eventLogEntry{At: h.At, order: 5_000_000 + i, Subject: subject,
			What: what, By: by})
		if h.LeftAt == 0 {
			continue
		}
		switch h.By {
		case ActorUser:
			by = subject
		case actorSheetImport:
			by = muted("their row in the spreadsheet changed")
		case ActorRecurrence:
			by = muted("the next date")
		default:
			by = names.actor(h.By)
		}
		what = "left the role " + role(h.RoleName)
		if strings.HasPrefix(h.By, "web:") {
			what = "was taken out of the role " + role(h.RoleName)
		}
		out = append(out, eventLogEntry{At: h.LeftAt, order: 6_000_000 + i, Subject: subject,
			What: what, By: by})
	}
	return out
}
