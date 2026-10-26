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

// Roles on the potluck sheet: things people going can sign themselves up
// for — a job such as setup or teardown, or coming for part of the event —
// each with an optional cap. An organiser adds them on the event page; anyone
// going takes or leaves one with its own button on the event's card, in the
// What I'm bringing view, or on the event page. They are not Discord roles:
// nothing is granted on Discord, and nobody but the sheet keeps them.
//
// A role is part of the sheet, so it shows only while the sheet is on, and
// taking one is open while the sheet takes answers. Taking a role is not an
// answer to what someone is bringing.

const (
	// sheetRolesPerEvent keeps the roles to one row of buttons, which is
	// what a Discord message has room for beside the others.
	sheetRolesPerEvent = 5
	sheetRoleNameLimit = 40
	sheetRoleCapLimit  = 999
	sheetRoleEmoji     = "🙋"
)

// ErrSheetRoleFull is taking a role that has no room.
var ErrSheetRoleFull = errors.New("that role is full")

// SheetRole is one role on an event's sheet, with the people going in it.
type SheetRole struct {
	ID       int64  `json:"id"`
	EventID  int64  `json:"event_id"`
	Name     string `json:"name"`
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

// RolesLine is the roles in one line: "🙋 Setup 7/12 · Teardown 3/12", or
// "" with no roles.
func (p *PotluckSheet) RolesLine() string {
	if len(p.Roles) == 0 {
		return ""
	}
	parts := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		parts[i] = r.Name + " " + r.Count()
	}
	return sheetRoleEmoji + " " + strings.Join(parts, " · ")
}

// RoleOf finds one of the sheet's roles by id.
func (p *PotluckSheet) RoleOf(roleID int64) (SheetRole, bool) {
	for _, r := range p.Roles {
		if r.ID == roleID {
			return r, true
		}
	}
	return SheetRole{}, false
}

// readSheetRoles reads an event's live roles and the people going in each.
func (s *Store) readSheetRoles(eventID int64) ([]SheetRole, error) {
	rows, err := s.db.Query(`SELECT id, event_id, name, capacity FROM sheet_roles
		WHERE event_id = ? AND removed_at = 0 ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read sheet roles: %w", err)
	}
	var roles []SheetRole
	index := map[int64]int{}
	for rows.Next() {
		var r SheetRole
		if err := rows.Scan(&r.ID, &r.EventID, &r.Name, &r.Capacity); err != nil {
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
		return nil, fmt.Errorf("%w: a role's name is 1 to %d characters", ErrInvalidPotluck, sheetRoleNameLimit)
	}
	if capacity < 0 || capacity > sheetRoleCapLimit {
		return nil, fmt.Errorf("%w: a role's cap is 0, for none, to %d", ErrInvalidPotluck, sheetRoleCapLimit)
	}
	roles, err := s.readSheetRoles(eventID)
	if err != nil {
		return nil, err
	}
	if len(roles) >= sheetRolesPerEvent {
		return nil, fmt.Errorf("%w: an event has at most %d roles, one row of buttons on Discord", ErrInvalidPotluck, sheetRolesPerEvent)
	}
	for _, r := range roles {
		if strings.EqualFold(r.Name, name) {
			return nil, fmt.Errorf("%w: there is already a role called %s", ErrInvalidPotluck, r.Name)
		}
	}
	res, err := s.db.Exec(`INSERT INTO sheet_roles (event_id, name, capacity, created_at, created_by) VALUES (?,?,?,?,?)`,
		eventID, name, capacity, now(), actor)
	if err != nil {
		return nil, fmt.Errorf("add sheet role: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read sheet role id: %w", err)
	}
	return &SheetRole{ID: id, EventID: eventID, Name: name, Capacity: capacity, Members: []string{}}, nil
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

// ToggleSheetRole puts someone going into a role, or takes them out of it if
// they are in it. The cap is checked in the same transaction that writes the
// row, as a Join's is. Returns whether they are in it now.
func (s *Store) ToggleSheetRole(eventID, roleID int64, userID string) (bool, error) {
	tx, err := s.beginPotluckChange(eventID, userID)
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
	if err := joinSheetRoleTx(tx, eventID, roleID, capacity, userID, "", ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// joinSheetRoleTx adds someone to a role if it has room.
func joinSheetRoleTx(tx *sql.Tx, eventID, roleID int64, capacity int, userID, source string, ts int64) error {
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
	if _, err := tx.Exec(`INSERT INTO sheet_role_signups (role_id, event_id, discord_user_id, source, joined_at)
		VALUES (?,?,?,?,?)`, roleID, eventID, userID, source, ts); err != nil {
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
	for _, r := range ev.Potluck.Roles {
		label := r.Name + " · " + r.Count()
		style := buttonStyleSecondary
		if viewerID != "" && r.Has(viewerID) {
			label, style = "✓ "+label, buttonStyleSuccess
		}
		buttons = append(buttons, map[string]any{
			"type": componentTypeButton, "style": style, "label": truncate(label, 80),
			"emoji": map[string]any{"name": sheetRoleEmoji}, "custom_id": sheetRoleCustomID(prefix, ev.ID, r.ID),
		})
	}
	return buttons
}

// sheetRoleRow is the card's row of role buttons, or none.
func sheetRoleRow(ev *Event) []any {
	if !potluckTakesAnswers(ev) || ev.Potluck == nil || len(ev.Potluck.Roles) == 0 {
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
	switch {
	case errors.Is(err, ErrSheetRoleFull):
		note = "Nothing was changed: that role is full."
	case errors.Is(err, ErrNotFound):
		note = "Nothing was changed: that role is no longer on the sheet."
	case err != nil:
		note = s.potluckRefusal(err, eventID, userID)
	default:
		s.inBackground(func() { s.syncAfterChange(eventID, nil) })
	}
	if note == "" {
		ev, readErr := s.store.GetEvent(eventID)
		name := "the role"
		if readErr == nil && ev.Potluck != nil {
			if r, ok := ev.Potluck.RoleOf(roleID); ok {
				name = fmt.Sprintf("%s (%s)", r.Name, r.Count())
			}
		}
		if in {
			note = "You're down for " + name + "."
		} else {
			note = "Took you off " + name + "."
		}
	}
	if inView {
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
	// Mine is whether the viewer is in it.
	Mine bool
}

func sheetRolePages(sheet *PotluckSheet, going []Signup, viewerID string) []sheetRolePage {
	people := map[string]Signup{}
	for _, sg := range going {
		people[sg.DiscordUserID] = sg
	}
	out := make([]sheetRolePage, 0, len(sheet.Roles))
	for _, r := range sheet.Roles {
		page := sheetRolePage{SheetRole: r, Mine: r.Has(viewerID)}
		for _, id := range r.Members {
			page.People = append(page.People, people[id])
		}
		out = append(out, page)
	}
	return out
}

// handleWebSheetRoleAdd adds a role, for an organiser.
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
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Added the role "+role.Name+".")
}

// handleWebSheetRoleRemove takes a role off, for an organiser.
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
			s.redirectWithNotice(w, r, ev.ID, "That role was already off the sheet.")
			return
		}
		log.Printf("[discord-signup] remove sheet role %d of %d: %v", roleID, ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was removed: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Took the role off the sheet.")
}

// handleWebMySheetRole takes or leaves a role for the viewer, from either
// event page.
func (s *Server) handleWebMySheetRole(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberPotluckEvent(w, r)
	if ev == nil {
		return
	}
	roleID, err := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err != nil {
		http.Error(w, "role_id must be an integer", http.StatusBadRequest)
		return
	}
	in, err := s.store.ToggleSheetRole(ev.ID, roleID, session.DiscordUserID)
	switch {
	case errors.Is(err, ErrSheetRoleFull):
		s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: that role is full.")
		return
	case errors.Is(err, ErrNotFound):
		s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: that role is no longer on the sheet.")
		return
	case err != nil:
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
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
	RemovedAt     int64
	RemovedBy     string
}

// SheetRoleHistory is every role an event's sheet has had and everyone who
// took one, oldest first.
func (s *Store) SheetRoleHistory(eventID int64) ([]SheetRoleHistoryRow, error) {
	rows, err := s.db.Query(`
		SELECT r.name, '', '', '', '', r.created_at, 0, r.created_by, r.removed_at, r.removed_by
		FROM sheet_roles r WHERE r.event_id = ?
		UNION ALL
		SELECT r.name, m.discord_user_id, COALESCE(s.display_name, ''), COALESCE(n.readable_name, ''), m.source,
		       m.joined_at, m.left_at, m.left_by, 0, ''
		FROM sheet_role_signups m
		JOIN sheet_roles r ON r.id = m.role_id
		LEFT JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
		LEFT JOIN readable_names n ON n.discord_user_id = m.discord_user_id
		WHERE m.event_id = ?
		ORDER BY 6`, eventID, eventID)
	if err != nil {
		return nil, fmt.Errorf("read sheet role history: %w", err)
	}
	defer rows.Close()
	var out []SheetRoleHistoryRow
	for rows.Next() {
		var h SheetRoleHistoryRow
		if err := rows.Scan(&h.RoleName, &h.DiscordUserID, &h.DisplayName, &h.ReadableName, &h.Source,
			&h.At, &h.LeftAt, &h.By, &h.RemovedAt, &h.RemovedBy); err != nil {
			return nil, fmt.Errorf("scan sheet role history: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// sheetRoleLogEntries are the roles' lines in the event's log.
func sheetRoleLogEntries(history []SheetRoleHistoryRow, names eventLogNames) []eventLogEntry {
	muted := func(text string) template.HTML {
		return template.HTML(`<span class="muted">` + template.HTMLEscapeString(text) + `</span>`)
	}
	role := func(name string) template.HTML {
		return template.HTML(template.HTMLEscapeString(sheetRoleEmoji + " " + name))
	}
	var out []eventLogEntry
	for i, h := range history {
		if h.DiscordUserID == "" {
			out = append(out, eventLogEntry{At: h.At, order: 5_000_000 + i, Subject: muted("the potluck sheet"),
				What: "added the role " + role(h.RoleName), By: names.actor(h.By)})
			if h.RemovedAt > 0 {
				out = append(out, eventLogEntry{At: h.RemovedAt, order: 6_000_000 + i, Subject: muted("the potluck sheet"),
					What: "removed the role " + role(h.RoleName), By: names.actor(h.RemovedBy)})
			}
			continue
		}
		subject := names.person(h.ReadableName, h.DisplayName, h.DiscordUserID)
		by := subject
		if h.Source == potluckSourceSheet {
			by = muted("copied from the spreadsheet")
		}
		out = append(out, eventLogEntry{At: h.At, order: 5_000_000 + i, Subject: subject,
			What: "potluck: took the role " + role(h.RoleName), By: by})
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
		out = append(out, eventLogEntry{At: h.LeftAt, order: 6_000_000 + i, Subject: subject,
			What: "potluck: left the role " + role(h.RoleName), By: by})
	}
	return out
}
