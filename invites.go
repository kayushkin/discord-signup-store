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
	// CurrentState is their row on the roster now — attending, waitlisted,
	// maybe or withdrawn — or "" when they have never had one. Read when the
	// page is drawn, never stored, so it cannot disagree with the roster.
	CurrentState string `json:"current_state"`
}

// RecordInvite stores one invite.
func (s *Store) RecordInvite(inv EventInvite) (*EventInvite, error) {
	inv.At = now()
	res, err := s.db.Exec(`
		INSERT INTO event_invites (event_id, discord_user_id, display_name, invited_by,
		                           delivery, delivery_error, at)
		VALUES (?,?,?,?,?,?,?)`,
		inv.EventID, inv.DiscordUserID, inv.DisplayName, inv.InvitedBy,
		inv.Delivery, inv.DeliveryError, inv.At)
	if err != nil {
		return nil, fmt.Errorf("record invite: %w", err)
	}
	if inv.ID, err = res.LastInsertId(); err != nil {
		return nil, fmt.Errorf("read invite id: %w", err)
	}
	return &inv, nil
}

// Invites lists an event's invites, oldest first, each with where the person
// stands on the roster now.
func (s *Store) Invites(eventID int64) ([]EventInvite, error) {
	rows, err := s.db.Query(`
		SELECT i.id, i.event_id, i.discord_user_id, i.display_name, COALESCE(r.readable_name, ''),
		       i.invited_by, i.delivery, i.delivery_error, i.at, COALESCE(sg.state, '')
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
			&inv.InvitedBy, &inv.Delivery, &inv.DeliveryError, &inv.At, &inv.CurrentState); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// inviteMessage is the DM: who asked, what and when, how full it is, and the
// event's own Join and Maybe buttons.
func inviteMessage(ev *Event, organiserName string) map[string]any {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** invited you to **%s**.", organiserName, ev.Name)
	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, "\n🗓️ <t:%d:F>", ev.StartsAt)
	}
	if ev.Location != "" {
		fmt.Fprintf(&b, "\n📍 %s", ev.Location)
	}
	switch {
	case ev.Capacity == 0:
		fmt.Fprintf(&b, "\n%d going.", ev.AttendingCount)
	case !eventIsFull(ev):
		fmt.Fprintf(&b, "\n%d/%d places taken.", ev.AttendingCount, ev.Capacity)
	case !ev.WaitlistDisabled:
		fmt.Fprintf(&b, "\nIt is full (%d/%d), so Join puts you on the waitlist.", ev.AttendingCount, ev.Capacity)
	default:
		fmt.Fprintf(&b, "\nIt is full (%d/%d) and has no waitlist; Join works once a place opens.", ev.AttendingCount, ev.Capacity)
	}
	b.WriteString("\n\nNothing is held for you until you press Join.")
	return map[string]any{
		"content": b.String(),
		"components": []any{map[string]any{"type": componentTypeActionRow, "components": []any{
			map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
				"label": "Join", "custom_id": JoinCustomID(ev.ID)},
			map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Maybe", "custom_id": MaybeCustomID(ev.ID)},
		}}},
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
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	if userID == "" {
		s.redirectWithNotice(w, r, ev.ID, "No invite was sent: pick a person from the list under the box.")
		return
	}
	displayName, err := s.discord.GuildMemberDisplayName(ev.GuildID, userID)
	if err != nil {
		s.redirectWithNotice(w, r, ev.ID, "No invite was sent: "+userID+" is not a member of this server ("+err.Error()+").")
		return
	}
	switch state, err := s.store.SignupState(ev.ID, userID); {
	case err != nil && !errors.Is(err, ErrNotFound):
		log.Printf("[discord-signup] invite: read state of %s on %d: %v", userID, ev.ID, err)
		http.Error(w, "could not read the roster", http.StatusInternalServerError)
		return
	case state == StateAttending || state == StateWaitlisted:
		s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("No invite was sent: %s is already %s.", displayName, state))
		return
	}

	inv := EventInvite{EventID: ev.ID, DiscordUserID: userID, DisplayName: displayName,
		InvitedBy: "web:" + session.DiscordUserID, Delivery: InviteDeliverySent}
	sendErr := s.discord.SendDirectMessagePayload(userID, inviteMessage(ev, session.DisplayName))
	var notice string
	switch {
	case sendErr == nil:
		notice = "Invited " + displayName + ". They have a DM with Join and Maybe buttons; the log below shows when they answer."
	case errors.Is(sendErr, ErrCannotMessageUser):
		inv.Delivery, inv.DeliveryError = InviteDeliveryDMsClosed, sendErr.Error()
		notice = displayName + " has DMs from server members turned off, so the invite could not be delivered. " +
			"Ask them in the server, or add them to a list directly."
	default:
		inv.Delivery, inv.DeliveryError = InviteDeliveryFailed, sendErr.Error()
		log.Printf("[discord-signup] invite user=%s event=%d: %v", userID, ev.ID, sendErr)
		notice = "Discord did not deliver the invite to " + displayName + ": " + sendErr.Error()
	}
	if _, err := s.store.RecordInvite(inv); err != nil {
		log.Printf("[discord-signup] record invite user=%s event=%d: %v", userID, ev.ID, err)
		notice += " (It could not be saved to the log: " + err.Error() + ".)"
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}
