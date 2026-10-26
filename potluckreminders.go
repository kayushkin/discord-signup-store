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
	// PotluckReminderForum is a ping in the forum thread. It does not stand
	// in for the DM a day before: a ping in a busy thread is easy to miss.
	PotluckReminderForum = "forum"

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
			if r.Kind == PotluckReminderAutomatic || (r.Kind == PotluckReminderNudge && r.SentAt >= windowOpens) {
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
	if line := ev.Potluck.AskedForLine(); line != "" {
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

// handleWebPotluckNudge asks everyone going who has not answered the sheet,
// on an organiser's press: by DM to each (via=dm), which goes through the
// same limit as any other message by DM, or by one post in the event's
// forum thread (via=forum) that shows the sheet and pings them.
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
	switch r.FormValue("via") {
	case MessageViaDM:
	case MessageViaForum:
		s.redirectWithNotice(w, r, ev.ID, s.nudgeInForum(ev, session, roster, people))
		return
	default:
		http.Error(w, "via must be dm or forum", http.StatusBadRequest)
		return
	}
	body := "Asked what they are bringing to the potluck."
	messageID, err := s.store.ClaimMessage(EventMessage{EventID: ev.ID, SentBy: "web:" + session.DiscordUserID,
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

// potluckForumNudge is the post asking in the forum thread: the sheet as it
// stands, who is bringing what, and a ping for each person who has not said.
// The people bringing something are written by name, never as a mention:
// they have answered, and a mention reads as a ping even where Discord is
// told not to send one.
// Over Discord's limit it is two posts — the sheet, then the pings — and the
// buttons go under the last.
func potluckForumNudge(ev *Event, organiser string, roster, unanswered []Signup) []string {
	sheet := ev.Potluck
	var head strings.Builder
	fmt.Fprintf(&head, "🍲 **Potluck check-in from %s** — <t:%d:R>\n", organiser, ev.StartsAt)
	head.WriteString(sheet.SummaryLine() + "\n")
	if line := sheet.StillNeededLine(); line != "" {
		head.WriteString("**" + line + "**\n")
	}
	if line := sheet.AskedForLine(); line != "" {
		head.WriteString("**" + line + "**\n")
	}
	names := map[string]string{}
	for _, sg := range roster {
		names[sg.DiscordUserID] = sg.NameOnDiscord()
	}
	// One line per thing brought, under its category, so a long list reads
	// down the screen rather than across it.
	bringing := func(itemLimit int) string {
		var b strings.Builder
		for _, c := range sheet.Counts() {
			if len(c.Items) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n**%s %s**\n", c.Category.Emoji, c.Category.Label)
			for _, it := range c.Items {
				fmt.Fprintf(&b, "- **%s** %s\n", names[it.DiscordUserID], truncate(it.Item, itemLimit))
			}
		}
		if b.Len() == 0 {
			return ""
		}
		return "\n**Who's bringing what**" + b.String()
	}
	mentions := make([]string, len(unanswered))
	for i, sg := range unanswered {
		mentions[i] = "<@" + sg.DiscordUserID + ">"
	}
	roles := ""
	for _, r := range sheet.Roles {
		people := make([]string, len(r.Members))
		for i, id := range r.Members {
			people[i] = names[id]
		}
		roles += fmt.Sprintf("\n**%s %s** %s", sheetRoleEmoji, r.Name, r.Count())
		if len(people) > 0 {
			roles += ": " + strings.Join(people, ", ")
		}
	}
	if roles != "" {
		head.WriteString(roles[1:] + "\n")
	}
	pings := fmt.Sprintf("\n**Haven't said yet (%d):** %s\nPress **What I'm bringing** below to add yours, or to say you're not bringing anything.",
		len(unanswered), strings.Join(mentions, " "))

	sheetPart := head.String() + bringing(potluckItemLimit)
	if len([]rune(sheetPart)) > discordMessageContentLimit {
		sheetPart = head.String() + bringing(40)
	}
	if len([]rune(sheetPart))+len([]rune(pings)) <= discordMessageContentLimit {
		return []string{sheetPart + pings}
	}
	return []string{truncate(sheetPart, discordMessageContentLimit), truncate(strings.TrimPrefix(pings, "\n"), discordMessageContentLimit)}
}

// nudgeInForum posts the check-in in the event's forum thread and says what
// happened. A forum post is not limited, as an organiser's other forum
// messages are not; it is recorded with them.
func (s *Server) nudgeInForum(ev *Event, session *WebSession, roster, people []Signup) string {
	if ev.ForumPostID == "" {
		return "Nothing was posted: this event has no forum post. Nudge them by DM instead."
	}
	if len(people) > mentionsPerMessage {
		return fmt.Sprintf("Nothing was posted: %d people have not answered, and Discord pings at most %d in a post. Nudge them by DM instead.",
			len(people), mentionsPerMessage)
	}
	posts := potluckForumNudge(ev, session.DisplayName, roster, people)
	ids := make([]string, len(people))
	for i, sg := range people {
		ids[i] = sg.DiscordUserID
	}
	messageID, err := s.store.ClaimMessage(EventMessage{EventID: ev.ID, SentBy: "web:" + session.DiscordUserID,
		Via: MessageViaForum, Audience: []string{StateAttending}, Body: "Potluck check-in, pinging everyone going who has not said what they are bringing.",
		Recipients: len(people)})
	if err != nil {
		log.Printf("[discord-signup] potluck forum nudge claim %d: %v", ev.ID, err)
		return "Nothing was posted: " + err.Error()
	}
	// A quiet thread is archived by Discord and takes no messages until opened.
	if err := s.discord.ModifyThread(ev.ForumPostID, map[string]any{"archived": false}); err != nil {
		log.Printf("[discord-signup] unarchive forum post of %d to nudge: %v", ev.ID, err)
	}
	buttons := []any{potluckButton(ev.ID)}
	if origin := s.webOrigin(); origin != "" {
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleLink,
			"label": "Event page", "url": fmt.Sprintf("%s/events/%d", origin, ev.ID)})
	}
	for i, content := range posts {
		// Only the people being asked are mentioned, and only they are
		// allowed to be pinged.
		payload := map[string]any{"content": content, "allowed_mentions": map[string]any{"users": ids}}
		if i < len(posts)-1 {
			payload["allowed_mentions"] = map[string]any{"parse": []string{}}
		} else {
			payload["components"] = []any{map[string]any{"type": componentTypeActionRow, "components": buttons}}
		}
		if _, err := s.discord.CreateMessage(ev.ForumPostID, payload); err != nil {
			log.Printf("[discord-signup] potluck forum nudge of %d: %v", ev.ID, err)
			if ferr := s.store.FinishMessage(messageID, 0, 1, err.Error()); ferr != nil {
				log.Printf("[discord-signup] potluck forum nudge finish %d: %v", ev.ID, ferr)
			}
			return "Discord refused the post: " + err.Error()
		}
	}
	if err := s.store.FinishMessage(messageID, 1, 0, ""); err != nil {
		log.Printf("[discord-signup] potluck forum nudge finish %d: %v", ev.ID, err)
	}
	for _, sg := range people {
		if err := s.store.RecordPotluckReminder(ev.ID, sg.DiscordUserID, PotluckReminderForum, potluckReminderSent); err != nil {
			log.Printf("[discord-signup] %v", err)
		}
	}
	return fmt.Sprintf("Posted in the forum thread, with who is bringing what, pinging the %d who have not said.", len(people))
}

// PotluckReminderHistory is every reminder an event's sheet has sent, every
// date included, oldest first. For the event's log.
func (s *Store) PotluckReminderHistory(eventID int64) ([]PotluckReminder, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, kind, outcome, sent_at FROM potluck_reminders
		WHERE event_id = ? ORDER BY sent_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read potluck reminder history: %w", err)
	}
	defer rows.Close()
	var out []PotluckReminder
	for rows.Next() {
		var r PotluckReminder
		if err := rows.Scan(&r.DiscordUserID, &r.Kind, &r.Outcome, &r.SentAt); err != nil {
			return nil, fmt.Errorf("scan potluck reminder: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
