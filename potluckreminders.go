package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Asking people going what they will bring.
//
// Two ways, both by DM with the What I'm bringing button under it. A day
// before the event, everyone going who has not answered gets one by itself,
// once per date. And an organiser can nudge everyone not answered from the
// event page at any time; a nudge counts against the event's DM limit
// (eventmessages.go), since it is a message to many people on one press.

const (
	// potluckReminderLead is how long before the start the reminder goes.
	potluckReminderLead = 24 * time.Hour
	// potluckReminderSettle is how long someone has had their place before
	// the reminder asks them, so a person who joins inside the last day
	// has a moment to fill it in themselves.
	potluckReminderSettle = 30 * time.Minute
)

// Kinds and outcomes of a potluck reminder.
const (
	PotluckReminderAutomatic = "automatic"
	PotluckReminderNudge     = "nudge"

	potluckReminderSent      = "sent"
	potluckReminderDMsClosed = "dms-closed"
	potluckReminderFailed    = "failed"
)

// PotluckReminder is one DM asking someone what they will bring.
type PotluckReminder struct {
	DiscordUserID string `json:"discord_user_id"`
	Kind          string `json:"kind"`
	Outcome       string `json:"outcome"`
	SentAt        int64  `json:"sent_at"`
}

// PotluckRemindersFor are the reminders sent for an event's current date.
func (s *Store) PotluckRemindersFor(eventID int64) ([]PotluckReminder, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, kind, outcome, sent_at FROM potluck_reminders
		WHERE event_id = ? AND cleared_at = 0 ORDER BY sent_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read potluck reminders: %w", err)
	}
	defer rows.Close()
	out := []PotluckReminder{}
	for rows.Next() {
		var r PotluckReminder
		if err := rows.Scan(&r.DiscordUserID, &r.Kind, &r.Outcome, &r.SentAt); err != nil {
			return nil, fmt.Errorf("scan potluck reminder: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordPotluckReminder notes a reminder sent, or refused by Discord.
func (s *Store) RecordPotluckReminder(eventID int64, userID, kind, outcome string) error {
	if _, err := s.db.Exec(`INSERT INTO potluck_reminders (event_id, discord_user_id, kind, outcome, sent_at)
		VALUES (?,?,?,?,?)`, eventID, userID, kind, outcome, now()); err != nil {
		return fmt.Errorf("record potluck reminder: %w", err)
	}
	return nil
}

// PotluckEventsStartingBy are live events with a sheet that start after now
// and no later than by.
func (s *Store) PotluckEventsStartingBy(by int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT `+eventColumns+` FROM events
		WHERE deleted_at = 0 AND potluck_enabled = 1 AND status IN (?, ?) AND starts_at > ? AND starts_at <= ?
		ORDER BY starts_at, id`, StatusOpen, StatusClosed, now(), by)
	if err != nil {
		return nil, fmt.Errorf("list potluck events: %w", err)
	}
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, *e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	for i := range out {
		if err := s.fillCounts(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unansweredGoing are the people going who have said nothing on the sheet,
// in the order they got their place.
func unansweredGoing(ev *Event, roster []Signup) []Signup {
	var out []Signup
	for _, sg := range roster {
		if sg.State == StateAttending && !ev.Potluck.Answered(sg.DiscordUserID) {
			out = append(out, sg)
		}
	}
	return out
}

// SendDuePotluckReminders DMs everyone going who has not answered, once per
// date, from a day before the event. Run every minute with the other
// reminders; the rows in potluck_reminders keep it to once.
func (s *Server) SendDuePotluckReminders() (sent int, err error) {
	if s.discord == nil {
		return 0, nil
	}
	ts := time.Now()
	events, err := s.store.PotluckEventsStartingBy(ts.Add(potluckReminderLead).Unix())
	if err != nil {
		return 0, err
	}
	for i := range events {
		ev := &events[i]
		roster, err := s.store.Roster(ev.ID, false)
		if err != nil {
			log.Printf("[discord-signup] potluck reminders roster %d: %v", ev.ID, err)
			continue
		}
		reminders, err := s.store.PotluckRemindersFor(ev.ID)
		if err != nil {
			log.Printf("[discord-signup] potluck reminders %d: %v", ev.ID, err)
			continue
		}
		// A nudge inside the last day counts as the reminder; one sent before
		// it does not, since the plans a day out are the ones that matter.
		windowOpens := ev.StartsAt - int64(potluckReminderLead/time.Second)
		reminded := map[string]bool{}
		for _, r := range reminders {
			if r.Kind == PotluckReminderAutomatic || r.SentAt >= windowOpens {
				reminded[r.DiscordUserID] = true
			}
		}
		settledBy := ts.Add(-potluckReminderSettle).Unix()
		for _, sg := range unansweredGoing(ev, roster) {
			if reminded[sg.DiscordUserID] || sg.StateChangedAt > settledBy {
				continue
			}
			content := fmt.Sprintf("🍲 **%s** is a potluck, <t:%d:R>, and you haven't said what you're bringing yet.",
				ev.Name, ev.StartsAt)
			if s.sendPotluckReminder(ev, sg.DiscordUserID, PotluckReminderAutomatic, content) {
				sent++
			}
		}
	}
	return sent, nil
}

// sendPotluckReminder DMs one person and records how it went. Reports
// whether Discord took it.
func (s *Server) sendPotluckReminder(ev *Event, userID, kind, opening string) bool {
	var b strings.Builder
	b.WriteString(opening + "\n\n")
	b.WriteString(ev.Potluck.SummaryLine() + "\n")
	if line := ev.Potluck.StillNeededLine(); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\nPress **What I'm bringing** to add yours, or to say you're not bringing anything.")
	payload := map[string]any{
		"content":          b.String(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components": []any{map[string]any{"type": componentTypeActionRow,
			"components": []any{potluckButton(ev.ID)}}},
	}
	err := s.sendEventDM(ev, userID, payload, DMKindPotluck, "what they are bringing")
	outcome := potluckReminderSent
	switch {
	case errors.Is(err, ErrCannotMessageUser):
		outcome = potluckReminderDMsClosed
	case err != nil:
		log.Printf("[discord-signup] potluck reminder to %s for %d: %v", userID, ev.ID, err)
		outcome = potluckReminderFailed
	}
	if err := s.store.RecordPotluckReminder(ev.ID, userID, kind, outcome); err != nil {
		log.Printf("[discord-signup] %v", err)
	}
	return outcome == potluckReminderSent
}

// handleWebPotluckNudge DMs everyone going who has not answered the sheet,
// on an organiser's press. It is a message to many people, so it goes
// through the same limit as any other message by DM.
func (s *Server) handleWebPotluckNudge(w http.ResponseWriter, r *http.Request) {
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
	if !potluckTakesAnswers(ev) || ev.Potluck == nil {
		s.redirectWithNotice(w, r, ev.ID, "Nobody was nudged: the potluck sheet is off or the event is over.")
		return
	}
	if s.discord == nil {
		s.redirectWithNotice(w, r, ev.ID, "Nobody was nudged: there is no Discord client configured.")
		return
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] potluck nudge roster %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nobody was nudged: could not read the roster: "+err.Error())
		return
	}
	people := unansweredGoing(ev, roster)
	if len(people) == 0 {
		s.redirectWithNotice(w, r, ev.ID, "Nobody to nudge: everyone going has answered.")
		return
	}
	body := "Asked what they are bringing to the potluck."
	messageID, err := s.store.ClaimMessage(EventMessage{EventID: ev.ID, SentBy: session.DiscordUserID,
		Via: MessageViaDM, Audience: []string{StateAttending}, Body: body, Recipients: len(people)})
	if errors.Is(err, ErrMessageLimit) {
		s.redirectWithNotice(w, r, ev.ID, "Nobody was nudged: "+strings.TrimPrefix(err.Error(), ErrMessageLimit.Error()+": ")+
			". Messages by DM are limited per event.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] potluck nudge claim %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nobody was nudged: "+err.Error())
		return
	}
	opening := fmt.Sprintf("🍲 **%s** asked: what are you bringing to **%s**, <t:%d:R>?",
		session.DisplayName, ev.Name, ev.StartsAt)
	delivered := 0
	var missed []string
	for _, sg := range people {
		if s.sendPotluckReminder(ev, sg.DiscordUserID, PotluckReminderNudge, opening) {
			delivered++
		} else {
			missed = append(missed, sg.NameOnDiscord())
		}
	}
	detail := ""
	if len(missed) > 0 {
		detail = "Not delivered: " + strings.Join(missed, ", ") + "."
	}
	if err := s.store.FinishMessage(messageID, delivered, len(missed), detail); err != nil {
		log.Printf("[discord-signup] potluck nudge finish %d: %v", ev.ID, err)
	}
	notice := fmt.Sprintf("Nudged %d of %d by DM.", delivered, len(people))
	if detail != "" {
		notice += " " + detail
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}
