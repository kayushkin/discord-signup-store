package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Invites: an organiser asking someone to come. The invite is a DM with the
// event's Join and Maybe buttons, so the person decides and the usual rules
// apply to their answer — the limit, the waitlist, closed signups. Nothing
// is held for them. Adding someone to a list is the other tool, for when the
// organiser decides; see PlaceOnList.

// EventInvite is one invite sent.
type EventInvite struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	// ReadableName is the short name set for them, joined on the user id.
	ReadableName  string `json:"readable_name,omitempty"`
	InvitedBy     string `json:"invited_by"`
	Delivery      string `json:"delivery"`
	DeliveryError string `json:"delivery_error"`
	At            int64  `json:"at"`
	// HoldsPlace is whether the invite keeps a place for them until they
	// answer; HoldEndedAt, HoldOutcome and HoldEndedBy say when and how it
	// stopped. A hold with HoldEndedAt 0 is live and counts against the limit.
	HoldsPlace bool `json:"holds_place"`
	// PastLimit is an invite that lets them in even when the event is full,
	// without keeping a place.
	PastLimit   bool   `json:"past_limit"`
	HoldEndedAt int64  `json:"hold_ended_at"`
	HoldOutcome string `json:"hold_outcome"`
	HoldEndedBy string `json:"hold_ended_by"`
	// CurrentState is their row on the roster now — attending, waitlisted,
	// maybe or withdrawn — or "" when they have never had one. Read when the
	// page is drawn, never stored, so it cannot disagree with the roster.
	CurrentState string `json:"current_state"`
}

// Invites lists an event's invites, oldest first, each with where the person
// stands on the roster now.
func (s *Store) Invites(eventID int64) ([]EventInvite, error) {
	rows, err := s.db.Query(`
		SELECT i.id, i.event_id, i.discord_user_id, i.display_name, COALESCE(r.readable_name, ''),
		       i.invited_by, i.delivery, i.delivery_error, i.at, COALESCE(sg.state, ''),
		       i.holds_place, i.past_limit, i.hold_ended_at, i.hold_outcome, i.hold_ended_by
		FROM event_invites i
		LEFT JOIN readable_names r ON r.discord_user_id = i.discord_user_id
		LEFT JOIN signups sg ON sg.event_id = i.event_id AND sg.discord_user_id = i.discord_user_id
		WHERE i.event_id = ? ORDER BY i.at ASC, i.id ASC`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()
	out := []EventInvite{}
	for rows.Next() {
		var inv EventInvite
		if err := rows.Scan(&inv.ID, &inv.EventID, &inv.DiscordUserID, &inv.DisplayName, &inv.ReadableName,
			&inv.InvitedBy, &inv.Delivery, &inv.DeliveryError, &inv.At, &inv.CurrentState,
			&inv.HoldsPlace, &inv.PastLimit, &inv.HoldEndedAt, &inv.HoldOutcome, &inv.HoldEndedBy); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// inviteMessage is the DM: who asked, what and when, how full it is, and the
// event's own Join and Maybe buttons.
func inviteMessage(ev *Event, organiserName, reserve string) map[string]any {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** invited you to **%s**.", organiserName, ev.Name)
	switch reserve {
	case reserveHold:
		b.WriteString(" **A place is held for you.**")
	case reservePastLimit:
		b.WriteString(" **You can join even though it is full.**")
	}
	holdsPlace := reserve != ""
	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, "\n🗓️ <t:%d:F>", ev.StartsAt)
	}
	if ev.Location != "" {
		fmt.Fprintf(&b, "\n📍 %s", ev.Location)
	}
	switch {
	case reserve == reserveHold:
		b.WriteString("\n\nPress Join to take it. Can't go gives it back so someone else can have it.")
	case reserve == reservePastLimit:
		b.WriteString("\n\nPress Join and you are in. Can't go lets the organiser know.")
	case ev.Capacity == 0:
		fmt.Fprintf(&b, "\n%d going.", ev.AttendingCount)
	case !eventIsFull(ev):
		fmt.Fprintf(&b, "\n%d/%d places taken.", ev.AttendingCount, ev.Capacity)
	case !ev.WaitlistDisabled:
		fmt.Fprintf(&b, "\nIt is full (%d/%d), so Join puts you on the waitlist.", ev.AttendingCount, ev.Capacity)
	default:
		fmt.Fprintf(&b, "\nIt is full (%d/%d) and has no waitlist; Join works once a place opens.", ev.AttendingCount, ev.Capacity)
	}
	buttons := []any{
		map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
			"label": "Join", "custom_id": JoinCustomID(ev.ID)},
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Maybe", "custom_id": MaybeCustomID(ev.ID)},
	}
	if holdsPlace {
		// Leave, pressed by someone not on the list who holds a place, gives
		// the place back; see handleLeave.
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Can't go", "custom_id": LeaveCustomID(ev.ID)})
	} else {
		b.WriteString("\n\nNothing is held for you until you press Join.")
	}
	return map[string]any{
		"content":          b.String(),
		"components":       []any{map[string]any{"type": componentTypeActionRow, "components": buttons}},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// handleWebInvite sends one invite and records it, whether or not Discord
// delivered it: an invite that bounced is still something the organiser did
// and needs to see. There is no channel mention when DMs are closed, unlike
// a promotion — an invite is not news they would miss out by not hearing, and
// pinging someone in public to ask is the organiser's call, not the bot's.
func (s *Server) handleWebInvite(w http.ResponseWriter, r *http.Request) {
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
	if s.discord == nil {
		s.redirectWithNotice(w, r, ev.ID, "No invite was sent: there is no Discord client configured.")
		return
	}
	if ev.Status != StatusOpen {
		s.redirectWithNotice(w, r, ev.ID, "No invite was sent: signups are "+ev.Status+
			", so their Join button would be refused. Open signups first, or add them to a list directly.")
		return
	}
	userIDs := pickedUserIDs(r)
	if len(userIDs) == 0 {
		s.redirectWithNotice(w, r, ev.ID, "No invite was sent: pick someone from the list under the box.")
		return
	}
	reserve := r.FormValue("reserve")
	if reserve != "" && reserve != reserveHold && reserve != reservePastLimit {
		http.Error(w, "reserve is hold, past_limit or nothing", http.StatusBadRequest)
		return
	}
	lines := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		lines = append(lines, s.invitePerson(ev, session, userID, reserve))
	}
	s.redirectWithNotice(w, r, ev.ID, strings.Join(lines, " "))
}

// invitePerson sends one invite and records it, whether or not Discord
// delivered it, and says what happened in a sentence.
func (s *Server) invitePerson(ev *Event, session *WebSession, userID, reserve string) string {
	holdPlace := reserve == reserveHold
	displayName, err := s.discord.GuildMemberDisplayName(ev.GuildID, userID)
	if err != nil {
		return "No invite for " + userID + ": not a member of this server (" + err.Error() + ")."
	}
	switch state, err := s.store.SignupState(ev.ID, userID); {
	case err != nil && !errors.Is(err, ErrNotFound):
		log.Printf("[discord-signup] invite: read state of %s on %d: %v", userID, ev.ID, err)
		return "No invite for " + displayName + ": could not read the roster (" + err.Error() + ")."
	case state == StateAttending || state == StateWaitlisted:
		return fmt.Sprintf("No invite for %s: already %s.", displayName, state)
	}

	// Recorded before the DM goes, so a held place is taken before anyone
	// is told about it; the delivery is written once Discord answers.
	recorded, err := s.store.RecordInvite(EventInvite{EventID: ev.ID, DiscordUserID: userID,
		DisplayName: displayName, InvitedBy: "web:" + session.DiscordUserID,
		Delivery: InviteDeliverySent, HoldsPlace: holdPlace, PastLimit: reserve == reservePastLimit})
	if errors.Is(err, ErrNoPlaceToHold) {
		return "No invite for " + displayName + ": there is no free place left to hold. Send it without holding one, or raise the limit."
	}
	if err != nil {
		log.Printf("[discord-signup] record invite user=%s event=%d: %v", userID, ev.ID, err)
		return "No invite for " + displayName + ": could not record it (" + err.Error() + ")."
	}
	if holdPlace {
		// A held place can make the event read as full on Discord.
		s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	}
	sendErr := s.discord.SendDirectMessagePayload(userID, inviteMessage(ev, session.DisplayName, reserve))
	said := "Invited " + displayName + "."
	switch reserve {
	case reserveHold:
		said = "Invited " + displayName + " and held a place for them."
	case reservePastLimit:
		said = "Invited " + displayName + ", who can join even though it is full."
	}
	delivery, deliveryError := InviteDeliverySent, ""
	switch {
	case sendErr == nil:
		return said
	case errors.Is(sendErr, ErrCannotMessageUser):
		delivery, deliveryError = InviteDeliveryDMsClosed, sendErr.Error()
		said = displayName + " has DMs from server members turned off, so their invite was not delivered."
	default:
		delivery, deliveryError = InviteDeliveryFailed, sendErr.Error()
		log.Printf("[discord-signup] invite user=%s event=%d: %v", userID, ev.ID, sendErr)
		said = "Discord did not deliver the invite to " + displayName + ": " + sendErr.Error() + "."
	}
	promoted, err := s.store.SetInviteDelivery(recorded.ID, delivery, deliveryError)
	if err != nil {
		log.Printf("[discord-signup] record invite delivery user=%s event=%d: %v", userID, ev.ID, err)
		return said + " (Could not record that: " + err.Error() + ".)"
	}
	if holdPlace {
		said += " The place held for them is free again."
	}
	s.afterHeldPlaceFreed(ev, promoted)
	return said
}

// afterHeldPlaceFreed redraws the Discord copies once a held place is given
// back, and tells whoever moved up into it.
func (s *Server) afterHeldPlaceFreed(ev *Event, promoted *Signup) {
	var changes []stateChange
	if promoted != nil {
		changes = append(changes, stateChange{UserID: promoted.DiscordUserID, State: StateAttending})
		s.inBackground(func() { s.notifyPromoted(ev, promoted) })
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, changes) })
}

// handleWebReleaseHold gives back a place an invite held, on the organiser's
// say-so; the next person waiting takes it. The invite itself stands: they
// can still press Join, as anyone can.
func (s *Server) handleWebReleaseHold(w http.ResponseWriter, r *http.Request) {
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
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	promoted, err := s.store.GiveBackHeldPlace(ev.ID, userID, HoldOutcomeReleased, "web:"+session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		s.redirectWithNotice(w, r, ev.ID, "No place is held for them any more.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] release held place of %s on %d: %v", userID, ev.ID, err)
		http.Error(w, "could not release it: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.afterHeldPlaceFreed(ev, promoted)
	notice := "The held place is free again."
	if promoted != nil {
		notice += " " + promoted.NameOnDiscord() + " moved up from the waitlist into it and was messaged."
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// What an invite keeps for the person asked, as the form sends it: a held
// place, a pass past the limit, or "" for nothing.
const (
	reserveHold      = "hold"
	reservePastLimit = "past_limit"
)

// pickedUserIDs is everyone the Add someone box sent, in the order picked,
// each once. The box sends Discord user ids, never names.
func pickedUserIDs(r *http.Request) []string {
	if err := r.ParseForm(); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range r.Form["discord_user_id"] {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
