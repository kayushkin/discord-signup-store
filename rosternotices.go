package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Roster notices: an organiser can ask, on an event's page, to be told by DM
// when people join or leave it. A change is told only after the person has
// left it alone for rosterNoticeGrace, so someone who presses Join by mistake
// and then Leave is never reported. A minute's sweep, driven by the scheduler
// like the reminders, sends them.

// rosterNoticeGrace is how long a person must leave their place alone before
// the change is told. Updates by one person closer together than this are one
// change, and only where it started and where it ended are compared.
const rosterNoticeGrace = time.Minute

// giveAPlaceActionPrefix starts the action of the button a roster notice puts
// under each person who joined the waitlist. The person's id follows it,
// because the custom_id's third part is the event.
const giveAPlaceActionPrefix = "give-place-"

// maxNoticeButtons is Discord's limit on a message: five rows of five.
const maxNoticeButtons = 25

// RosterWatcher is one organiser watching one event's roster.
type RosterWatcher struct {
	EventID         int64
	DiscordUserID   string
	WatchingSince   int64
	ReportedThrough int64
	LastSentAt      int64
	LastError       string
}

// WatchRoster turns roster notices on for one organiser on one event. Turning
// it on again keeps the row as it is.
func (s *Store) WatchRoster(eventID int64, userID string) error {
	ts := now()
	_, err := s.db.Exec(`INSERT INTO roster_watchers (event_id, discord_user_id, watching_since, reported_through)
		VALUES (?,?,?,?) ON CONFLICT(event_id, discord_user_id) DO NOTHING`, eventID, userID, ts, ts)
	if err != nil {
		return fmt.Errorf("watch roster of %d for %s: %w", eventID, userID, err)
	}
	return nil
}

// UnwatchRoster turns roster notices off.
func (s *Store) UnwatchRoster(eventID int64, userID string) error {
	if _, err := s.db.Exec(`DELETE FROM roster_watchers WHERE event_id = ? AND discord_user_id = ?`,
		eventID, userID); err != nil {
		return fmt.Errorf("unwatch roster of %d for %s: %w", eventID, userID, err)
	}
	return nil
}

// RosterWatcherFor returns one organiser's watch on an event, or nil when
// they are not watching it.
func (s *Store) RosterWatcherFor(eventID int64, userID string) (*RosterWatcher, error) {
	var w RosterWatcher
	err := s.db.QueryRow(`SELECT event_id, discord_user_id, watching_since, reported_through, last_sent_at, last_error
		FROM roster_watchers WHERE event_id = ? AND discord_user_id = ?`, eventID, userID).
		Scan(&w.EventID, &w.DiscordUserID, &w.WatchingSince, &w.ReportedThrough, &w.LastSentAt, &w.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read roster watcher: %w", err)
	}
	return &w, nil
}

// RosterWatchers returns every watch on an event that is still live.
func (s *Store) RosterWatchers() ([]RosterWatcher, error) {
	rows, err := s.db.Query(`SELECT w.event_id, w.discord_user_id, w.watching_since, w.reported_through,
		       w.last_sent_at, w.last_error
		FROM roster_watchers w JOIN events e ON e.id = w.event_id
		WHERE e.deleted_at = 0 AND e.status IN (?, ?)
		ORDER BY w.event_id, w.discord_user_id`, StatusOpen, StatusClosed)
	if err != nil {
		return nil, fmt.Errorf("read roster watchers: %w", err)
	}
	defer rows.Close()
	var out []RosterWatcher
	for rows.Next() {
		var w RosterWatcher
		if err := rows.Scan(&w.EventID, &w.DiscordUserID, &w.WatchingSince, &w.ReportedThrough,
			&w.LastSentAt, &w.LastError); err != nil {
			return nil, fmt.Errorf("scan roster watcher: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// markRosterReported records that changes up to through have been told, or
// that telling them failed, and why.
func (s *Store) markRosterReported(w RosterWatcher, through, sentAt int64, lastError string) error {
	if _, err := s.db.Exec(`UPDATE roster_watchers SET reported_through = ?,
		last_sent_at = CASE WHEN ? > 0 THEN ? ELSE last_sent_at END, last_error = ?
		WHERE event_id = ? AND discord_user_id = ?`,
		through, sentAt, sentAt, lastError, w.EventID, w.DiscordUserID); err != nil {
		return fmt.Errorf("mark roster reported for %s on %d: %w", w.DiscordUserID, w.EventID, err)
	}
	return nil
}

// signupUpdatesAfter returns an event's signup updates after a time, oldest
// first.
func (s *Store) signupUpdatesAfter(eventID, after int64) ([]SignupUpdate, error) {
	rows, err := s.db.Query(`SELECT id, event_id, discord_user_id, action, from_state, to_state, actor, at
		FROM signup_updates WHERE event_id = ? AND at > ? ORDER BY at ASC, id ASC`, eventID, after)
	if err != nil {
		return nil, fmt.Errorf("read signup updates: %w", err)
	}
	defer rows.Close()
	var out []SignupUpdate
	for rows.Next() {
		var u SignupUpdate
		if err := rows.Scan(&u.ID, &u.EventID, &u.DiscordUserID, &u.Action, &u.FromState,
			&u.ToState, &u.Actor, &u.At); err != nil {
			return nil, fmt.Errorf("scan signup update: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// rosterChange is one person's settled join or leave.
type rosterChange struct {
	UserID string
	From   string
	To     string
	At     int64
}

// onTheRoster reports whether a state holds a place or a place in line.
func onTheRoster(state string) bool {
	return state == StateAttending || state == StateWaitlisted
}

// ownChoiceActors are the actors that mean the person did it themselves. An
// organiser's add or removal, a promotion, a date rolling over and a regular
// put back on are not news to the organiser who is watching.
var ownChoiceActors = map[string]bool{
	ActorUser:       true,
	ActorReaction:   true,
	ActorInterested: true,
}

// settledRosterChanges finds the joins and leaves whose last update falls in
// (after, through]. One person's updates closer together than the grace are
// one change, compared from where it started to where it ended, so a Join
// followed within the minute by a Leave is no change at all. A change whose
// last update is at or before through cannot grow: any update that would
// extend it lands within the grace, which the sweep has already waited out.
func settledRosterChanges(updates []SignupUpdate, after, through int64) []rosterChange {
	grace := int64(rosterNoticeGrace / time.Second)
	byUser := map[string][]SignupUpdate{}
	var order []string
	for _, u := range updates {
		if _, seen := byUser[u.DiscordUserID]; !seen {
			order = append(order, u.DiscordUserID)
		}
		byUser[u.DiscordUserID] = append(byUser[u.DiscordUserID], u)
	}
	var out []rosterChange
	for _, userID := range order {
		list := byUser[userID]
		start := 0
		for i := range list {
			if i+1 < len(list) && list[i+1].At-list[i].At < grace {
				continue
			}
			burst := list[start : i+1]
			start = i + 1
			first, last := burst[0], burst[len(burst)-1]
			if last.At <= after || last.At > through {
				continue
			}
			byThem := false
			for _, u := range burst {
				byThem = byThem || ownChoiceActors[u.Actor]
			}
			if !byThem || onTheRoster(first.FromState) == onTheRoster(last.ToState) {
				continue
			}
			out = append(out, rosterChange{UserID: userID, From: first.FromState, To: last.ToState, At: last.At})
		}
	}
	return out
}

// SendRosterNotices tells each watching organiser the settled joins and
// leaves since their last notice, one DM per organiser per event. Called
// every minute; calling it more often sends nothing twice, because each
// watch records how far it has been told.
func (s *Server) SendRosterNotices() (sent int, err error) {
	watchers, err := s.store.RosterWatchers()
	if err != nil {
		return 0, err
	}
	through := now() - int64(rosterNoticeGrace/time.Second)
	for _, w := range watchers {
		if through <= w.ReportedThrough {
			continue
		}
		ok, err := s.sendRosterNotice(w, through)
		if err != nil {
			log.Printf("[discord-signup] roster notice to %s about %d: %v", w.DiscordUserID, w.EventID, err)
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// sendRosterNotice tells one organiser what changed on one event up to
// through. An error leaves the watch where it was, so the next sweep tries
// the same changes again; a DM Discord refuses is recorded on the watch and
// shown on the page, and the changes are not tried again.
func (s *Server) sendRosterNotice(w RosterWatcher, through int64) (bool, error) {
	ev, err := s.store.GetEvent(w.EventID)
	if err != nil {
		return false, err
	}
	updates, err := s.store.signupUpdatesAfter(ev.ID, w.WatchingSince)
	if err != nil {
		return false, err
	}
	changes := settledRosterChanges(updates, w.ReportedThrough, through)
	if len(changes) == 0 {
		return false, s.store.markRosterReported(w, through, 0, w.LastError)
	}
	// Someone who may no longer edit the event stops hearing who is on it.
	mayEdit, err := s.mayEditEventAsMember(ev, w.DiscordUserID)
	if err != nil {
		return false, fmt.Errorf("check they may still edit it: %w", err)
	}
	if !mayEdit {
		log.Printf("[discord-signup] %s may no longer edit event %d; roster notices turned off", w.DiscordUserID, ev.ID)
		return false, s.store.UnwatchRoster(ev.ID, w.DiscordUserID)
	}
	roster, err := s.store.Roster(ev.ID, true)
	if err != nil {
		return false, err
	}
	if err := s.discord.SendDirectMessagePayload(w.DiscordUserID, renderRosterNotice(ev, changes, roster, s.webOrigin())); err != nil {
		log.Printf("[discord-signup] roster notice to %s about %d not sent: %v", w.DiscordUserID, ev.ID, err)
		return false, s.store.markRosterReported(w, through, 0, rosterNoticeFailure(err))
	}
	return true, s.store.markRosterReported(w, through, now(), "")
}

// rosterNoticeFailure says, for the event page, why a notice was not sent.
func rosterNoticeFailure(err error) string {
	if errors.Is(err, ErrCannotMessageUser) {
		return "Discord refused the DM: your DMs from server members are turned off."
	}
	return "Discord refused the DM: " + err.Error()
}

// renderRosterNotice is the DM: who joined and who left, the count now, and
// a button to give a place to each person who joined the waitlist and is
// still on it.
func renderRosterNotice(ev *Event, changes []rosterChange, roster []Signup, origin string) map[string]any {
	now := map[string]Signup{}
	going, waiting := 0, 0
	for _, sg := range roster {
		now[sg.DiscordUserID] = sg
		switch sg.State {
		case StateAttending:
			going++
		case StateWaitlisted:
			waiting++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n", ev.Name)
	var buttons []map[string]any
	for _, c := range changes {
		sg, known := now[c.UserID]
		name := c.UserID
		if known {
			name = sg.NameOnDiscord()
		}
		switch {
		case onTheRoster(c.To) && c.To == StateAttending:
			fmt.Fprintf(&b, "✅ **%s** joined, going\n", name)
		case onTheRoster(c.To):
			fmt.Fprintf(&b, "⏳ **%s** joined the waitlist\n", name)
		case c.To == StateMaybe:
			fmt.Fprintf(&b, "➖ **%s** left and is now Maybe\n", name)
		default:
			fmt.Fprintf(&b, "➖ **%s** left\n", name)
		}
		if known && sg.State == StateWaitlisted && c.To == StateWaitlisted && len(buttons) < maxNoticeButtons {
			buttons = append(buttons, map[string]any{
				"type": componentTypeButton, "style": buttonStylePrimary,
				"label":     truncate("Give "+name+" a place", 80),
				"custom_id": GiveAPlaceCustomID(ev.ID, c.UserID),
			})
		}
	}
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "Now %d/%d going", going, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "Now %d going", going)
	}
	if waiting > 0 {
		fmt.Fprintf(&b, ", %d waiting", waiting)
	}
	b.WriteString(".")
	if origin != "" {
		fmt.Fprintf(&b, " [Event page](%s/events/%d)", origin, ev.ID)
	}
	b.WriteString("\n-# You turned these on on the event's page, where you can turn them off.")
	payload := map[string]any{
		"content":          b.String(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"flags":            messageFlagSuppressEmbeds,
	}
	var rows []map[string]any
	for i := 0; i < len(buttons); i += 5 {
		rows = append(rows, map[string]any{"type": componentTypeActionRow, "components": buttons[i:min(i+5, len(buttons))]})
	}
	if rows != nil {
		payload["components"] = rows
	}
	return payload
}

// GiveAPlaceCustomID is a roster notice's button that gives one waitlisted
// person a place.
func GiveAPlaceCustomID(eventID int64, userID string) string {
	return fmt.Sprintf("%s:%s%s:%d", customIDPrefix, giveAPlaceActionPrefix, userID, eventID)
}

// handleGiveAPlaceButton gives a place from a roster notice's button. The
// press comes from a DM, where Discord sends no permissions, so whether the
// presser may edit the event is asked of Discord.
func (s *Server) handleGiveAPlaceButton(w http.ResponseWriter, in *Interaction, action string, eventID int64) {
	target := strings.TrimPrefix(action, giveAPlaceActionPrefix)
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	presser, _ := in.actor()
	mayEdit, err := s.mayEditEventAsMember(ev, presser)
	if err != nil {
		log.Printf("[discord-signup] check %s may edit %d from a DM: %v", presser, ev.ID, err)
		s.replyEphemeral(w, "Could not check whether you may edit this event: "+err.Error())
		return
	}
	if !mayEdit {
		s.replyEphemeral(w, "You can no longer edit this event, so you cannot give places on it.")
		return
	}
	notice, err := s.giveAPlace(ev, target, "discord:"+presser)
	if err != nil {
		log.Printf("[discord-signup] give %s a place on %d from a DM: %v", target, ev.ID, err)
		s.replyEphemeral(w, "Nothing was changed: "+err.Error())
		return
	}
	s.replyEphemeral(w, notice)
}

// mayEditEventAsMember is mayEditEvent for someone acting outside the server
// — a press in a DM, or a notice about to be sent — where Discord hands over
// no permissions. They are read from Discord as the server grants them, as
// the web pages' login reads them: the server's owner has every permission,
// and everyone else has what @everyone and their roles give.
func (s *Server) mayEditEventAsMember(ev *Event, userID string) (bool, error) {
	if ev.CreatedBy != "" && ev.CreatedBy == userID {
		return true, nil
	}
	if s.discord == nil {
		return false, errors.New("checking who may edit needs a Discord client")
	}
	roleIDs, err := s.discord.GuildMemberRoleIDs(ev.GuildID, userID)
	if err != nil {
		return false, fmt.Errorf("read their roles: %w", err)
	}
	roles, err := s.discord.ListGuildRoles(ev.GuildID)
	if err != nil {
		return false, fmt.Errorf("read the server's roles: %w", err)
	}
	ownerID, err := s.discord.GuildOwnerID(ev.GuildID)
	if err != nil {
		return false, fmt.Errorf("read the server's owner: %w", err)
	}
	held := map[string]bool{ev.GuildID: true} // @everyone's id is the server's
	for _, id := range roleIDs {
		held[id] = true
	}
	var bits uint64
	for _, r := range roles {
		if !held[r.ID] {
			continue
		}
		roleBits, err := strconv.ParseUint(r.Permissions, 10, 64)
		if err != nil {
			return false, fmt.Errorf("read role %s's permissions %q: %w", r.ID, r.Permissions, err)
		}
		bits |= roleBits
	}
	if ownerID == userID {
		bits = ^uint64(0)
	}
	return s.mayEditEvent(editActor{GuildID: ev.GuildID, UserID: userID, PermissionBits: bits,
		RoleIDs: roleIDs, RolesKnown: true}, ev)
}
