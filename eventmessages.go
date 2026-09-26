package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Messaging the people on an event, from the web page: a post in the
// event's forum thread with each of them mentioned — the one place this
// service pings on an organiser's say-so — or a DM to each. Going always;
// the waitlist and Maybe if the organiser ticks them.
//
// Limited to messageLimit per event in any messageWindow, whoever sends
// them, so a page cannot be used to ping a roster over and over. The limit is
// checked and the send recorded in one transaction, so two organisers
// pressing Send at once cannot both slip under it.

const (
	messageLimit  = 2
	messageWindow = 10 * time.Minute
	// messageBodyLimit leaves room in Discord's 2000 characters for the
	// heading and a line of mentions.
	messageBodyLimit = 1500
)

// Message routes and statuses.
const (
	MessageViaForum = "forum"
	MessageViaDM    = "dm"

	messageSending = "sending"
	messageSent    = "sent"
	messageFailed  = "failed"
)

// ErrMessageLimit is a message over the limit; the error says when the next
// one is allowed.
var ErrMessageLimit = errors.New("message limit reached")

// EventMessage is one message sent.
type EventMessage struct {
	ID         int64    `json:"id"`
	EventID    int64    `json:"event_id"`
	SentBy     string   `json:"sent_by"`
	Via        string   `json:"via"`
	Audience   []string `json:"audience"`
	Body       string   `json:"body"`
	Recipients int      `json:"recipients"`
	Delivered  int      `json:"delivered"`
	Failed     int      `json:"failed"`
	Status     string   `json:"status"`
	Detail     string   `json:"detail"`
	At         int64    `json:"at"`
}

// ClaimMessage records a message about to go out, if the event is under its
// limit, and returns its id. Over the limit it returns ErrMessageLimit with
// the time the next is allowed.
func (s *Store) ClaimMessage(m EventMessage) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	ts := now()
	since := ts - int64(messageWindow/time.Second)
	rows, err := tx.Query(`SELECT at FROM event_messages WHERE event_id = ? AND at > ? AND status != ?
		ORDER BY at ASC`, m.EventID, since, messageFailed)
	if err != nil {
		return 0, fmt.Errorf("read recent messages: %w", err)
	}
	var recent []int64
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan message time: %w", err)
		}
		recent = append(recent, at)
	}
	rows.Close()
	if len(recent) >= messageLimit {
		next := recent[len(recent)-messageLimit] + int64(messageWindow/time.Second)
		return 0, fmt.Errorf("%w: %d in %d minutes; the next can go at %d", ErrMessageLimit,
			messageLimit, int(messageWindow/time.Minute), next)
	}
	res, err := tx.Exec(`INSERT INTO event_messages (event_id, sent_by, via, audience, body, recipients, status, at)
		VALUES (?,?,?,?,?,?,?,?)`, m.EventID, m.SentBy, m.Via, strings.Join(m.Audience, ","), m.Body,
		m.Recipients, messageSending, ts)
	if err != nil {
		return 0, fmt.Errorf("record message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read message id: %w", err)
	}
	return id, tx.Commit()
}

// FinishMessage records how a message went. One that reached nobody is
// failed, and does not count against the limit.
func (s *Store) FinishMessage(id int64, delivered, failed int, detail string) error {
	status := messageSent
	if delivered == 0 {
		status = messageFailed
	}
	_, err := s.db.Exec(`UPDATE event_messages SET delivered = ?, failed = ?, status = ?, detail = ? WHERE id = ?`,
		delivered, failed, status, detail, id)
	if err != nil {
		return fmt.Errorf("record message result: %w", err)
	}
	return nil
}

// Messages lists an event's messages, oldest first.
func (s *Store) Messages(eventID int64) ([]EventMessage, error) {
	rows, err := s.db.Query(`SELECT id, event_id, sent_by, via, audience, body, recipients, delivered, failed,
		status, detail, at FROM event_messages WHERE event_id = ? ORDER BY at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	out := []EventMessage{}
	for rows.Next() {
		var m EventMessage
		var audience string
		if err := rows.Scan(&m.ID, &m.EventID, &m.SentBy, &m.Via, &audience, &m.Body, &m.Recipients,
			&m.Delivered, &m.Failed, &m.Status, &m.Detail, &m.At); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Audience = strings.Split(audience, ",")
		out = append(out, m)
	}
	return out, rows.Err()
}

// messageAllowance is how many messages an event may still send in the
// current window, and when the next is allowed once there are none left.
func messageAllowance(messages []EventMessage, at int64) (left int, nextAt int64) {
	since := at - int64(messageWindow/time.Second)
	var recent []int64
	for _, m := range messages {
		if m.At > since && m.Status != messageFailed {
			recent = append(recent, m.At)
		}
	}
	left = max(0, messageLimit-len(recent))
	if left == 0 {
		nextAt = recent[len(recent)-messageLimit] + int64(messageWindow/time.Second)
	}
	return left, nextAt
}

// audienceWords says who a message went to.
func audienceWords(lists []string) string {
	words := map[string]string{StateAttending: "going", StateWaitlisted: "the waitlist", StateMaybe: "maybe"}
	var out []string
	for _, l := range lists {
		if w := words[l]; w != "" {
			out = append(out, w)
		}
	}
	switch len(out) {
	case 0:
		return "nobody"
	case 1:
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

// handleWebMessage sends an organiser's message to the people on the lists
// they picked, by a post in the forum thread or by DM.
func (s *Server) handleWebMessage(w http.ResponseWriter, r *http.Request) {
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
		s.redirectWithNotice(w, r, ev.ID, "Nothing was sent: there is no Discord client configured.")
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		s.redirectWithNotice(w, r, ev.ID, "Nothing was sent: the message is empty.")
		return
	}
	if n := len([]rune(body)); n > messageBodyLimit {
		s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("Nothing was sent: the message is %d characters; the most is %d.", n, messageBodyLimit))
		return
	}
	via := r.FormValue("via")
	if via != MessageViaForum && via != MessageViaDM {
		http.Error(w, "via is forum or dm", http.StatusBadRequest)
		return
	}
	if via == MessageViaForum && ev.ForumPostID == "" {
		s.redirectWithNotice(w, r, ev.ID, "Nothing was sent: this event has no forum post to write in. Send it by DM instead.")
		return
	}
	lists := []string{StateAttending}
	for _, l := range []string{StateWaitlisted, StateMaybe} {
		if r.FormValue("include_"+l) == "on" {
			lists = append(lists, l)
		}
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] message: roster %d: %v", ev.ID, err)
		http.Error(w, "could not read the roster", http.StatusInternalServerError)
		return
	}
	var people []Signup
	for _, sg := range roster {
		for _, l := range lists {
			if sg.State == l {
				people = append(people, sg)
			}
		}
	}
	if len(people) == 0 {
		s.redirectWithNotice(w, r, ev.ID, "Nothing was sent: nobody is on "+audienceWords(lists)+".")
		return
	}

	id, err := s.store.ClaimMessage(EventMessage{EventID: ev.ID, SentBy: "web:" + session.DiscordUserID,
		Via: via, Audience: lists, Body: body, Recipients: len(people)})
	if errors.Is(err, ErrMessageLimit) {
		_, next := messageAllowance(mustMessages(s.store, ev.ID), now())
		s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("Nothing was sent: an event can send %d messages in any %d minutes. The next can go at %s.",
			messageLimit, int(messageWindow/time.Minute), time.Unix(next, 0).UTC().Format("15:04 UTC")))
		return
	}
	if err != nil {
		log.Printf("[discord-signup] claim message on %d: %v", ev.ID, err)
		http.Error(w, "could not record the message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var notice string
	var delivered, failed int
	var detail string
	switch via {
	case MessageViaForum:
		delivered, failed, detail, notice = s.postMessageInForum(ev, session, body, people, lists)
	case MessageViaDM:
		delivered, failed, detail, notice = s.sendMessageByDM(ev, session, body, people, lists)
	}
	if err := s.store.FinishMessage(id, delivered, failed, detail); err != nil {
		log.Printf("[discord-signup] finish message %d: %v", id, err)
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

func mustMessages(store *Store, eventID int64) []EventMessage {
	messages, err := store.Messages(eventID)
	if err != nil {
		log.Printf("[discord-signup] messages of %d: %v", eventID, err)
	}
	return messages
}

// postMessageInForum posts the message in the event's forum thread with
// everyone on the lists mentioned, and those mentions allowed to ping.
func (s *Server) postMessageInForum(ev *Event, session *WebSession, body string, people []Signup, lists []string) (delivered, failed int, detail, notice string) {
	ids := make([]string, 0, len(people))
	mentions := make([]string, 0, len(people))
	for _, sg := range people {
		ids = append(ids, sg.DiscordUserID)
		mentions = append(mentions, "<@"+sg.DiscordUserID+">")
	}
	content := fmt.Sprintf("📣 **From %s, to everyone %s:**\n%s\n\n%s",
		session.DisplayName, audienceWords(lists), body, strings.Join(mentions, " "))
	// Discord allows 2000 characters and pings at most 100 users a message.
	if n := len([]rune(content)); n > 2000 || len(ids) > 100 {
		return 0, 0, "too long", fmt.Sprintf("Nothing was posted: with %d people mentioned the post would be %d characters, over Discord's 2000. Shorten the message, or send it by DM.", len(ids), n)
	}
	// A quiet thread is archived by Discord, and an archived thread takes no
	// messages until it is opened again.
	if err := s.discord.ModifyThread(ev.ForumPostID, map[string]any{"archived": false}); err != nil {
		log.Printf("[discord-signup] unarchive forum post of %d to message: %v", ev.ID, err)
	}
	payload := map[string]any{"content": content, "allowed_mentions": map[string]any{"users": ids}}
	if _, err := s.discord.CreateMessage(ev.ForumPostID, payload); err != nil {
		log.Printf("[discord-signup] message in forum post of %d: %v", ev.ID, err)
		return 0, 1, err.Error(), "Discord refused the post: " + err.Error() + ". It does not count against the limit."
	}
	return 1, 0, "", fmt.Sprintf("Posted in the forum thread, pinging %d %s.", len(ids), plural(len(ids), "person", "people"))
}

// sendMessageByDM sends the message to each person, and says how many it
// reached.
func (s *Server) sendMessageByDM(ev *Event, session *WebSession, body string, people []Signup, lists []string) (delivered, failed int, detail, notice string) {
	content := fmt.Sprintf("📣 **%s** sent a message to everyone %s at **%s**:\n\n%s",
		session.DisplayName, audienceWords(lists), ev.Name, body)
	var closed, broken []string
	for _, sg := range people {
		err := s.discord.SendDirectMessage(sg.DiscordUserID, content)
		switch {
		case err == nil:
			delivered++
		case errors.Is(err, ErrCannotMessageUser):
			closed = append(closed, sg.NameOnDiscord())
		default:
			log.Printf("[discord-signup] message DM to %s for %d: %v", sg.DiscordUserID, ev.ID, err)
			broken = append(broken, sg.NameOnDiscord())
		}
	}
	failed = len(closed) + len(broken)
	notice = fmt.Sprintf("Sent by DM to %d of %d.", delivered, len(people))
	if len(closed) > 0 {
		notice += " DMs closed: " + strings.Join(closed, ", ") + "."
		detail += "DMs closed: " + strings.Join(closed, ", ") + ". "
	}
	if len(broken) > 0 {
		notice += " Discord refused: " + strings.Join(broken, ", ") + "."
		detail += "Refused: " + strings.Join(broken, ", ") + "."
	}
	if delivered == 0 {
		notice += " It does not count against the limit."
	}
	return delivered, failed, strings.TrimSpace(detail), notice
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
