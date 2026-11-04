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
	// TakesPlace is whether the role is part of the event: only people going
	// may take it, and they count toward the event's limit as anyone going
	// does. A role that does not take a place — an audience, say — is apart
	// from the main list: anyone in the server takes it without joining, and
	// only its own cap holds.
	TakesPlace bool `json:"takes_place"`
	// Members are the Discord user ids of the people in it, in the order they
	// took it: for a role that takes a place, only those going.
	Members []string `json:"members"`
	// People are the same people with the names to show them by.
	People []SheetRoleMember `json:"people"`
}

// SheetRoleMember is someone in a role, with their names: their name on the
// roster when they are on it, else the one Discord gave when they took the
// role, and their short name if they have one.
type SheetRoleMember struct {
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	ReadableName  string `json:"readable_name,omitempty"`
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

// SeparateRoles are the event's roles apart from its main list, which carry
// counts of their own beside its places; none while roles are off.
func (ev *Event) SeparateRoles() []SheetRole {
	if !ev.RolesEnabled {
		return nil
	}
	var out []SheetRole
	for _, r := range ev.Roles {
		if !r.TakesPlace {
			out = append(out, r)
		}
	}
	return out
}

// separateRoleMembers are the people in the event's separate roles who are
// not going, in the order they took them, each once. With those going they
// are everyone on the event: the reminders ping them and the forum post
// keeps them.
func (ev *Event) separateRoleMembers(roster []Signup) []string {
	going := map[string]bool{}
	for _, sg := range roster {
		if sg.State == StateAttending {
			going[sg.DiscordUserID] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range ev.SeparateRoles() {
		for _, id := range r.Members {
			if !going[id] && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// inSeparateRole says whether someone is in one of the event's separate
// roles.
func (ev *Event) inSeparateRole(userID string) bool {
	for _, r := range ev.SeparateRoles() {
		if r.Has(userID) {
			return true
		}
	}
	return false
}

// separateRoleCounts is each separate role's count after the event's own:
// " · 🎤 Audience 3/6 · 🎉 Hosts 2", or "" with none. plain writes a custom
// emoji as :name:, for text Discord does not render as markdown.
func separateRoleCounts(ev *Event, plain bool) string {
	out := ""
	for _, r := range ev.SeparateRoles() {
		emoji := r.Emoji
		if plain {
			emoji = roleEmojiPlainText(emoji)
		}
		out += " · " + emoji + " " + r.Name + " " + r.Count()
	}
	return out
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
	rows, err := s.db.Query(`SELECT id, event_id, name, emoji, capacity, takes_place FROM sheet_roles
		WHERE event_id = ? AND removed_at = 0 ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read sheet roles: %w", err)
	}
	var roles []SheetRole
	index := map[int64]int{}
	for rows.Next() {
		var r SheetRole
		if err := rows.Scan(&r.ID, &r.EventID, &r.Name, &r.Emoji, &r.Capacity, &r.TakesPlace); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan sheet role: %w", err)
		}
		r.Members = []string{}
		r.People = []SheetRoleMember{}
		index[r.ID] = len(roles)
		roles = append(roles, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sheet roles: %w", err)
	}
	members, err := s.db.Query(`
		SELECT m.role_id, m.discord_user_id, COALESCE(NULLIF(s.display_name, ''), m.display_name), COALESCE(n.readable_name, '')
		FROM sheet_role_signups m
		JOIN sheet_roles r ON r.id = m.role_id
		LEFT JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
		LEFT JOIN readable_names n ON n.discord_user_id = m.discord_user_id
		WHERE m.event_id = ? AND m.left_at = 0 AND (r.takes_place = 0 OR s.state = ?)
		ORDER BY m.joined_at, m.id`, eventID, StateAttending)
	if err != nil {
		return nil, fmt.Errorf("read sheet role members: %w", err)
	}
	defer members.Close()
	for members.Next() {
		var roleID int64
		var person SheetRoleMember
		if err := members.Scan(&roleID, &person.DiscordUserID, &person.DisplayName, &person.ReadableName); err != nil {
			return nil, fmt.Errorf("scan sheet role member: %w", err)
		}
		if i, ok := index[roleID]; ok {
			roles[i].Members = append(roles[i].Members, person.DiscordUserID)
			roles[i].People = append(roles[i].People, person)
		}
	}
	return roles, members.Err()
}

// AddSheetRole adds a role to an event's sheet. takesPlace says whether it
// is part of the event (see SheetRole.TakesPlace).
func (s *Store) AddSheetRole(eventID int64, name string, capacity int, takesPlace bool, actor string) (*SheetRole, error) {
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
	res, err := s.db.Exec(`INSERT INTO sheet_roles (event_id, name, emoji, capacity, takes_place, created_at, created_by) VALUES (?,?,?,?,?,?,?)`,
		eventID, name, emoji, capacity, takesPlace, now(), actor)
	if err != nil {
		return nil, fmt.Errorf("add sheet role: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read sheet role id: %w", err)
	}
	return &SheetRole{ID: id, EventID: eventID, Name: name, Emoji: emoji, Capacity: capacity, TakesPlace: takesPlace,
		Members: []string{}, People: []SheetRoleMember{}}, nil
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

// SheetRoleEdit is an organizer's change to a role: each field that is set
// changes, and a field left nil stays as it is.
type SheetRoleEdit struct {
	Emoji      *string
	Capacity   *int
	TakesPlace *bool
}

// EditSheetRole changes what an edit sets, recording each change in
// sheet_role_updates. Lowering the cap takes nobody out: the role is over
// its cap until people leave, as an event over its limit is. Switching a
// role between taking a place and not moves nobody either: people in it who
// are not going stop being shown in it while it takes a place, and show
// again if it stops. Returns which fields changed.
func (s *Store) EditSheetRole(eventID, roleID int64, edit SheetRoleEdit, actor string) ([]string, error) {
	if edit.Emoji != nil && !isRoleEmojiForm(*edit.Emoji) {
		return nil, fmt.Errorf("%w: pick one of the emojis offered", ErrInvalidSheetRole)
	}
	if edit.Capacity != nil && (*edit.Capacity < 0 || *edit.Capacity > sheetRoleCapLimit) {
		return nil, fmt.Errorf("%w: a role's cap is 0, for none, to %d", ErrInvalidSheetRole, sheetRoleCapLimit)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var oldEmoji string
	var oldCapacity int
	var oldTakesPlace bool
	err = tx.QueryRow(`SELECT emoji, capacity, takes_place FROM sheet_roles WHERE id = ? AND event_id = ? AND removed_at = 0`,
		roleID, eventID).Scan(&oldEmoji, &oldCapacity, &oldTakesPlace)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read sheet role: %w", err)
	}
	ts := now()
	var changed []string
	change := func(field, column, from, to string, value any) error {
		if from == to {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO sheet_role_updates (role_id, event_id, field, from_value, to_value, actor, at)
			VALUES (?,?,?,?,?,?,?)`, roleID, eventID, field, from, to, actor, ts); err != nil {
			return fmt.Errorf("record sheet role %s change: %w", field, err)
		}
		if _, err := tx.Exec(`UPDATE sheet_roles SET `+column+` = ? WHERE id = ?`, value, roleID); err != nil {
			return fmt.Errorf("edit sheet role %s: %w", field, err)
		}
		changed = append(changed, field)
		return nil
	}
	if edit.Emoji != nil {
		if err := change("emoji", "emoji", oldEmoji, *edit.Emoji, *edit.Emoji); err != nil {
			return nil, err
		}
	}
	if edit.Capacity != nil {
		if err := change("capacity", "capacity", strconv.Itoa(oldCapacity), strconv.Itoa(*edit.Capacity), *edit.Capacity); err != nil {
			return nil, err
		}
	}
	if edit.TakesPlace != nil {
		if err := change("takes_place", "takes_place", strconv.FormatBool(oldTakesPlace), strconv.FormatBool(*edit.TakesPlace), *edit.TakesPlace); err != nil {
			return nil, err
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return changed, tx.Commit()
}

// ToggleSheetRole puts someone into a role, or takes them out of it if they
// are in it. A role that takes a place is for people going; any other role
// is open to anyone, who stays off the main list. displayName is the name
// Discord gives them now, kept with the row for someone not on the roster.
// The cap is checked in the same transaction that writes the row, as a
// Join's is. Returns whether they are in it now.
func (s *Store) ToggleSheetRole(eventID, roleID int64, userID, displayName string) (bool, error) {
	tx, role, err := s.beginSheetRoleChange(eventID, roleID, userID)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ts := now()
	res, err := tx.Exec(`UPDATE sheet_role_signups SET left_at = ?, left_by = ?
		WHERE role_id = ? AND discord_user_id = ? AND left_at = 0`, ts, ActorUser, roleID, userID)
	if err != nil {
		return false, fmt.Errorf("leave sheet role: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, tx.Commit()
	}
	if err := role.mayJoin(tx, userID); err != nil {
		return false, err
	}
	if err := joinSheetRoleTx(tx, eventID, role.id, role.capacity, role.takesPlace, userID, displayName, "", "", ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ErrAlreadyInSheetRole is putting someone in a role they are already in.
var ErrAlreadyInSheetRole = errors.New("they are already in that role")

// ErrNotInSheetRole is taking someone out of a role they are not in.
var ErrNotInSheetRole = errors.New("they are not in that role")

// PutInSheetRole puts someone into a role on an organizer's say-so: someone
// going, for a role that takes a place, or anyone, for one that does not.
// The cap holds, as it does for people taking a role themselves: to swap
// someone into a full role, take someone out first. actor is "web:<id>".
func (s *Store) PutInSheetRole(eventID, roleID int64, userID, displayName, actor string) error {
	tx, role, err := s.beginSheetRoleChange(eventID, roleID, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var in int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sheet_role_signups
		WHERE role_id = ? AND discord_user_id = ? AND left_at = 0`, roleID, userID).Scan(&in); err != nil {
		return fmt.Errorf("read sheet role signup: %w", err)
	}
	if in > 0 {
		return ErrAlreadyInSheetRole
	}
	if err := role.mayJoin(tx, userID); err != nil {
		return err
	}
	if err := joinSheetRoleTx(tx, eventID, roleID, role.capacity, role.takesPlace, userID, displayName, "", actor, now()); err != nil {
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

// sheetRoleInTx is the role a change is about, read inside its transaction.
type sheetRoleInTx struct {
	id         int64
	capacity   int
	takesPlace bool
}

// mayJoin refuses someone not going a role that takes a place.
func (r sheetRoleInTx) mayJoin(tx *sql.Tx, userID string) error {
	if !r.takesPlace {
		return nil
	}
	var state string
	err := tx.QueryRow(`SELECT state FROM signups s JOIN sheet_roles r ON r.event_id = s.event_id
		WHERE r.id = ? AND s.discord_user_id = ?`, r.id, userID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != StateAttending) {
		return ErrNotGoing
	}
	if err != nil {
		return fmt.Errorf("read signup: %w", err)
	}
	return nil
}

// beginSheetRoleChange opens a transaction for a change to someone's place
// in a role, refusing an event whose roles are off or closed and a role no
// longer on it, and reads the role.
func (s *Store) beginSheetRoleChange(eventID, roleID int64, userID string) (*sql.Tx, sheetRoleInTx, error) {
	role := sheetRoleInTx{id: roleID}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, role, fmt.Errorf("begin: %w", err)
	}
	var enabled bool
	var status string
	err = tx.QueryRow(`SELECT roles_enabled, status FROM events WHERE id = ? AND deleted_at = 0`, eventID).Scan(&enabled, &status)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return nil, role, ErrNotFound
	}
	if err != nil {
		tx.Rollback()
		return nil, role, fmt.Errorf("read event: %w", err)
	}
	if !enabled {
		tx.Rollback()
		return nil, role, ErrSheetRolesOff
	}
	if !sheetRolesTakeAnswers(&Event{RolesEnabled: enabled, Status: status}) {
		tx.Rollback()
		return nil, role, ErrSheetRolesClosed
	}
	err = tx.QueryRow(`SELECT capacity, takes_place FROM sheet_roles WHERE id = ? AND event_id = ? AND removed_at = 0`,
		roleID, eventID).Scan(&role.capacity, &role.takesPlace)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return nil, role, ErrNotFound
	}
	if err != nil {
		tx.Rollback()
		return nil, role, fmt.Errorf("read sheet role: %w", err)
	}
	return tx, role, nil
}

// joinSheetRoleTx adds someone to a role if it has room. For a role that
// takes a place only people going count toward its cap; for any other,
// everyone in it does. joinedBy is "" when they take it themselves or the
// spreadsheet copies it, and "web:<id>" when an organizer puts them in.
func joinSheetRoleTx(tx *sql.Tx, eventID, roleID int64, capacity int, takesPlace bool, userID, displayName, source, joinedBy string, ts int64) error {
	if capacity > 0 {
		var taken int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM sheet_role_signups m
			LEFT JOIN signups s ON s.event_id = m.event_id AND s.discord_user_id = m.discord_user_id
			WHERE m.role_id = ? AND m.left_at = 0 AND (? = 0 OR s.state = ?)`, roleID, takesPlace, StateAttending).Scan(&taken); err != nil {
			return fmt.Errorf("count sheet role: %w", err)
		}
		if taken >= capacity {
			return ErrSheetRoleFull
		}
	}
	if _, err := tx.Exec(`INSERT INTO sheet_role_signups (role_id, event_id, discord_user_id, display_name, source, joined_at, joined_by)
		VALUES (?,?,?,?,?,?,?)`, roleID, eventID, userID, displayName, source, ts, joinedBy); err != nil {
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
func (s *Server) handleSheetRoleButton(w http.ResponseWriter, in *Interaction, action string, eventID int64, userID string) {
	inView := strings.HasPrefix(action, sheetRolePanelPrefix)
	roleText := strings.TrimPrefix(strings.TrimPrefix(action, sheetRolePanelPrefix), sheetRoleCardPrefix)
	roleID, err := strconv.ParseInt(roleText, 10, 64)
	if err != nil {
		s.replyEphemeral(w, "Unknown role.")
		return
	}
	_, displayName := in.actor()
	nowIn, err := s.store.ToggleSheetRole(eventID, roleID, userID, displayName)
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
		apart := false
		if readErr == nil {
			if r, ok := sheetRoleOf(ev.Roles, roleID); ok {
				name = fmt.Sprintf("%s (%s)", r.Name, r.Count())
				apart = !r.TakesPlace
			}
		}
		if nowIn {
			note = "You're down for " + name + "."
			if apart {
				note += " It's separate from the event's places, so it doesn't put you on the Going list."
			}
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
	// Others are the people an organizer may put in it, in signup order:
	// those going who are not in it, and for a role apart from the main list
	// those waiting and maybe too.
	Others []Signup
	// Mine is whether the viewer is in it, and ViewerMayTake whether they may
	// take it: anyone for a role apart from the main list, only someone going
	// for a role that takes a place.
	Mine          bool
	ViewerMayTake bool
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
func sheetRolesPageOf(ev *Event, going, waitingOrMaybe []Signup, viewerID string) *sheetRolesPage {
	if !ev.RolesEnabled {
		return nil
	}
	page := &sheetRolesPage{TakesAnswers: sheetRolesTakeAnswers(ev), ViewerID: viewerID}
	for _, sg := range going {
		if sg.DiscordUserID == viewerID {
			page.ViewerGoing = true
		}
	}
	page.Roles = sheetRolePages(ev.Roles, going, waitingOrMaybe, viewerID, page.ViewerGoing)
	return page
}

func sheetRolePages(roles []SheetRole, going, waitingOrMaybe []Signup, viewerID string, viewerGoing bool) []sheetRolePage {
	out := make([]sheetRolePage, 0, len(roles))
	for _, r := range roles {
		page := sheetRolePage{SheetRole: r, Mine: r.Has(viewerID), ViewerMayTake: viewerGoing || !r.TakesPlace}
		candidates := going
		if !r.TakesPlace {
			candidates = append(append([]Signup{}, going...), waitingOrMaybe...)
		}
		for _, sg := range candidates {
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
	// A form that does not say takes a place, as every role did before roles
	// could stand apart.
	takesPlace := r.FormValue("takes_place") != "false"
	role, err := s.store.AddSheetRole(ev.ID, r.FormValue("name"), capacity, takesPlace, "web:"+session.DiscordUserID)
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
	// Only the fields the request carries change, as on the event's own
	// form: the emoji picker sends the emoji, the count sends the cap, and
	// the switch sends whether it takes a place.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unreadable form: "+err.Error(), http.StatusBadRequest)
		return
	}
	var edit SheetRoleEdit
	if _, sent := r.PostForm["capacity"]; sent {
		capacity := 0
		if text := strings.TrimSpace(r.PostFormValue("capacity")); text != "" {
			if capacity, err = strconv.Atoi(text); err != nil {
				s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: the cap must be a whole number, or empty for none.")
				return
			}
		}
		edit.Capacity = &capacity
	}
	if _, sent := r.PostForm["emoji"]; sent {
		emoji, err := s.checkRoleEmoji(ev.GuildID, r.PostFormValue("emoji"))
		if err != nil {
			if !errors.Is(err, ErrInvalidSheetRole) {
				log.Printf("[discord-signup] check role emoji for %d: %v", ev.ID, err)
				s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: could not check that emoji against the server's: "+err.Error())
				return
			}
			s.redirectWithNotice(w, r, ev.ID, sheetRoleRefusal(err))
			return
		}
		edit.Emoji = &emoji
	}
	if _, sent := r.PostForm["takes_place"]; sent {
		takesPlace, err := strconv.ParseBool(r.PostFormValue("takes_place"))
		if err != nil {
			http.Error(w, "takes_place must be true or false", http.StatusBadRequest)
			return
		}
		edit.TakesPlace = &takesPlace
	}
	changed, err := s.store.EditSheetRole(ev.ID, roleID, edit, "web:"+session.DiscordUserID)
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

// nameForRole is the name to keep with someone's row in a role that does
// not take a place, whose people may not be on the roster: their name on the
// roster if they are on it, else the name Discord gives them in the server
// now. A role that takes a place needs none — its people are on the roster.
func (s *Server) nameForRole(ev *Event, roleID int64, userID string) (string, error) {
	role, ok := sheetRoleOf(ev.Roles, roleID)
	if !ok || role.TakesPlace {
		return "", nil
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		return "", fmt.Errorf("could not read the roster: %w", err)
	}
	for _, sg := range roster {
		if sg.DiscordUserID == userID && sg.DisplayName != "" {
			return sg.DisplayName, nil
		}
	}
	if s.discord == nil {
		return "", errors.New("no Discord client, so their name in the server cannot be read")
	}
	member, err := s.discord.GuildMember(ev.GuildID, userID)
	if errors.Is(err, ErrUnknownMember) {
		return "", errors.New("Discord says they are not in this event's server")
	}
	if err != nil {
		return "", fmt.Errorf("could not read their name in the server from Discord: %w", err)
	}
	return member.DisplayName, nil
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
		name, nameErr := s.nameForRole(ev, roleID, userID)
		if nameErr != nil {
			log.Printf("[discord-signup] put in role event=%d user=%s: %v", ev.ID, userID, nameErr)
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+nameErr.Error())
			return
		}
		err = s.store.PutInSheetRole(ev.ID, roleID, userID, name, actor)
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
	name, nameErr := s.nameForRole(ev, roleID, session.DiscordUserID)
	if nameErr != nil {
		log.Printf("[discord-signup] web role change event=%d user=%s: %v", ev.ID, session.DiscordUserID, nameErr)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+nameErr.Error())
		return
	}
	in, err := s.store.ToggleSheetRole(ev.ID, roleID, session.DiscordUserID, name)
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
			switch h.Field {
			case "capacity":
				what = "changed " + role(h.RoleName) + "'s cap from " + sheetRoleCapWords(h.FromValue) + " to " + sheetRoleCapWords(h.ToValue)
			case "takes_place":
				what = "made " + role(h.RoleName) + " separate from Going"
				if h.ToValue == "true" {
					what = "made " + role(h.RoleName) + " count toward Going"
				}
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
