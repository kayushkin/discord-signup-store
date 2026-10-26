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

// Requests on the potluck sheet: specific things an organiser asks for —
// "Ice", wanted twice, in supplies — that someone going takes by bringing
// it. Taking one adds an item in its category carrying the request's id, so
// it counts on the sheet like any other item and comes off the open list
// once as many as want it have taken it. Wherever someone says what they
// are bringing, the open requests are put in front of them: at the top of
// the What I'm bringing view with a menu to take one, as the example in each
// category's form, and on the members' page with a button each.

const (
	// potluckRequestsPerEvent keeps the list inside one Discord menu.
	potluckRequestsPerEvent = 25
	potluckRequestWantedMax = 99
	potluckRequestEmoji     = "📌"
)

// ErrRequestTaken is taking a request that is no longer open, or one this
// person has already taken.
var ErrRequestTaken = errors.New("that request is already taken")

// PotluckRequest is one thing an organiser asked for, with the items of
// people going that answer it.
type PotluckRequest struct {
	ID       int64         `json:"id"`
	EventID  int64         `json:"event_id"`
	Category string        `json:"category"`
	Item     string        `json:"item"`
	Wanted   int           `json:"wanted"`
	Claims   []PotluckItem `json:"claims"`
}

// Open says whether more people are wanted for it.
func (r PotluckRequest) Open() bool { return len(r.Claims) < r.Wanted }

// TakenBy says whether someone has taken it.
func (r PotluckRequest) TakenBy(userID string) bool {
	for _, c := range r.Claims {
		if c.DiscordUserID == userID {
			return true
		}
	}
	return false
}

// Label is the request as a line says it: "🍴 Ice", with "(1 of 2)" when
// more than one is wanted.
func (r PotluckRequest) Label() string {
	c, _ := potluckCategoryOf(r.Category)
	label := c.Emoji + " " + r.Item
	if r.Wanted > 1 {
		label += fmt.Sprintf(" (%d of %d)", len(r.Claims), r.Wanted)
	}
	return label
}

// OpenRequests are the requests still wanting someone, in the order asked.
func (p *PotluckSheet) OpenRequests() []PotluckRequest {
	var out []PotluckRequest
	for _, r := range p.Requests {
		if r.Open() {
			out = append(out, r)
		}
	}
	return out
}

// RequestOf finds one of the sheet's requests by id.
func (p *PotluckSheet) RequestOf(requestID int64) (PotluckRequest, bool) {
	for _, r := range p.Requests {
		if r.ID == requestID {
			return r, true
		}
	}
	return PotluckRequest{}, false
}

// AskedForLine is the open requests in one line — "📌 Asked for: 🍴 Ice ·
// ✨ Folding table" — or "" when none are open.
func (p *PotluckSheet) AskedForLine() string {
	open := p.OpenRequests()
	if len(open) == 0 {
		return ""
	}
	parts := make([]string, len(open))
	for i, r := range open {
		parts[i] = r.Label()
	}
	return potluckRequestEmoji + " Asked for: " + strings.Join(parts, " · ")
}

// askedForIn is the open requests in one category, for the example text of
// that category's form: "Asked for: Ice, Cups". "" when none.
func (p *PotluckSheet) askedForIn(category string) string {
	var names []string
	for _, r := range p.OpenRequests() {
		if r.Category == category {
			names = append(names, r.Item)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "Asked for: " + strings.Join(names, ", ")
}

// readPotluckRequests reads an event's live requests and fills in who is
// bringing each from the sheet's items.
func (s *Store) readPotluckRequests(eventID int64, items []PotluckItem) ([]PotluckRequest, error) {
	rows, err := s.db.Query(`SELECT id, event_id, category, item, wanted FROM potluck_requests
		WHERE event_id = ? AND removed_at = 0 ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read potluck requests: %w", err)
	}
	defer rows.Close()
	out := []PotluckRequest{}
	for rows.Next() {
		var r PotluckRequest
		if err := rows.Scan(&r.ID, &r.EventID, &r.Category, &r.Item, &r.Wanted); err != nil {
			return nil, fmt.Errorf("scan potluck request: %w", err)
		}
		r.Claims = []PotluckItem{}
		for _, it := range items {
			if it.RequestID == r.ID {
				r.Claims = append(r.Claims, it)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddPotluckRequest adds something an organiser asks for.
func (s *Store) AddPotluckRequest(eventID int64, category, item string, wanted int, actor string) (*PotluckRequest, error) {
	if _, ok := potluckCategoryOf(category); !ok {
		return nil, fmt.Errorf("%w: category %q is not one of food, drinks, supplies, other", ErrInvalidPotluck, category)
	}
	item = strings.Join(strings.Fields(item), " ")
	if item == "" || len([]rune(item)) > potluckItemLimit {
		return nil, fmt.Errorf("%w: say what is wanted, in at most %d characters", ErrInvalidPotluck, potluckItemLimit)
	}
	if wanted < 1 || wanted > potluckRequestWantedMax {
		return nil, fmt.Errorf("%w: how many is 1 to %d", ErrInvalidPotluck, potluckRequestWantedMax)
	}
	var live int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM potluck_requests WHERE event_id = ? AND removed_at = 0`,
		eventID).Scan(&live); err != nil {
		return nil, fmt.Errorf("count potluck requests: %w", err)
	}
	if live >= potluckRequestsPerEvent {
		return nil, fmt.Errorf("%w: an event has at most %d requests, what one Discord menu holds", ErrInvalidPotluck, potluckRequestsPerEvent)
	}
	res, err := s.db.Exec(`INSERT INTO potluck_requests (event_id, category, item, wanted, created_at, created_by)
		VALUES (?,?,?,?,?,?)`, eventID, category, item, wanted, now(), actor)
	if err != nil {
		return nil, fmt.Errorf("add potluck request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read potluck request id: %w", err)
	}
	return &PotluckRequest{ID: id, EventID: eventID, Category: category, Item: item, Wanted: wanted, Claims: []PotluckItem{}}, nil
}

// RemovePotluckRequest takes a request off the sheet. What people said they
// would bring for it stays on the sheet as their own item.
func (s *Store) RemovePotluckRequest(eventID, requestID int64, actor string) error {
	res, err := s.db.Exec(`UPDATE potluck_requests SET removed_at = ?, removed_by = ?
		WHERE id = ? AND event_id = ? AND removed_at = 0`, now(), actor, requestID, eventID)
	if err != nil {
		return fmt.Errorf("remove potluck request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TakePotluckRequest has someone going bring a request: an item in its
// category, named as it is.
func (s *Store) TakePotluckRequest(eventID, requestID int64, userID string) (*PotluckItem, error) {
	var category, item string
	err := s.db.QueryRow(`SELECT category, item FROM potluck_requests WHERE id = ? AND event_id = ? AND removed_at = 0`,
		requestID, eventID).Scan(&category, &item)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read potluck request: %w", err)
	}
	return s.addPotluckItem(eventID, userID, category, item, requestID)
}

// checkRequestOpenTx refuses a request that is gone, has as many as want it,
// or that this person has already taken. Only items of people going count.
func checkRequestOpenTx(tx *sql.Tx, eventID, requestID int64, userID string) error {
	var wanted int
	err := tx.QueryRow(`SELECT wanted FROM potluck_requests WHERE id = ? AND event_id = ? AND removed_at = 0`,
		requestID, eventID).Scan(&wanted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read potluck request: %w", err)
	}
	var taken, mine int
	if err := tx.QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE p.discord_user_id = ?) FROM potluck_items p
		JOIN signups s ON s.event_id = p.event_id AND s.discord_user_id = p.discord_user_id
		WHERE p.event_id = ? AND p.request_id = ? AND p.removed_at = 0 AND s.state = ?`,
		userID, eventID, requestID, StateAttending).Scan(&taken, &mine); err != nil {
		return fmt.Errorf("count potluck request: %w", err)
	}
	if mine > 0 || taken >= wanted {
		return ErrRequestTaken
	}
	return nil
}

// In Discord.

const potluckTakeRequestAction = "potluck-take"

// potluckRequestMenu is the What I'm bringing view's menu of open requests,
// or nil when none are open or the viewer has taken every one.
func potluckRequestMenu(ev *Event, userID string) map[string]any {
	var options []any
	for _, r := range ev.Potluck.OpenRequests() {
		if r.TakenBy(userID) {
			continue
		}
		c, _ := potluckCategoryOf(r.Category)
		options = append(options, map[string]any{
			"label": truncate(r.Item, 100), "value": strconv.FormatInt(r.ID, 10),
			"description": truncate(fmt.Sprintf("%s · %d of %d taken", c.Label, len(r.Claims), r.Wanted), 100),
			"emoji":       map[string]any{"name": c.Emoji},
		})
	}
	if len(options) == 0 {
		return nil
	}
	return map[string]any{"type": componentTypeActionRow, "components": []any{map[string]any{
		"type": componentTypeStringSelect, "custom_id": fmt.Sprintf("%s:%s:%d", customIDPrefix, potluckTakeRequestAction, ev.ID),
		"placeholder": "📌 Bring something that was asked for…", "options": options,
	}}}
}

// handleTakeRequestMenu takes the request picked in the view's menu.
func (s *Server) handleTakeRequestMenu(w http.ResponseWriter, in *Interaction, eventID int64, userID string) {
	note := "Pick one of the requests in the menu."
	if len(in.Data.Values) == 1 {
		requestID, err := strconv.ParseInt(in.Data.Values[0], 10, 64)
		if err != nil {
			s.replyEphemeral(w, "Unknown request.")
			return
		}
		item, err := s.store.TakePotluckRequest(eventID, requestID, userID)
		switch {
		case errors.Is(err, ErrRequestTaken):
			note = "Nothing was saved: someone has already taken that one, or you have."
		case errors.Is(err, ErrNotFound):
			note = "Nothing was saved: that request is no longer on the sheet."
		case err != nil:
			note = s.potluckRefusal(err, eventID, userID)
		default:
			c, _ := potluckCategoryOf(item.Category)
			note = fmt.Sprintf("Added: %s %s, as asked.", c.Emoji, item.Item)
			s.inBackground(func() { s.syncAfterChange(eventID, nil) })
		}
	}
	s.replyPotluckPanel(w, callbackTypeUpdateMessage, eventID, userID, note)
}

// On the web.

// potluckRequestPage is a request as the event pages show it.
type potluckRequestPage struct {
	PotluckRequest
	Emoji  string
	People []Signup
	// Mine is whether the viewer has taken it.
	Mine bool
}

func potluckRequestPages(sheet *PotluckSheet, going []Signup, viewerID string) []potluckRequestPage {
	people := map[string]Signup{}
	for _, sg := range going {
		people[sg.DiscordUserID] = sg
	}
	out := make([]potluckRequestPage, 0, len(sheet.Requests))
	for _, r := range sheet.Requests {
		c, _ := potluckCategoryOf(r.Category)
		page := potluckRequestPage{PotluckRequest: r, Emoji: c.Emoji, Mine: r.TakenBy(viewerID)}
		for _, claim := range r.Claims {
			page.People = append(page.People, people[claim.DiscordUserID])
		}
		out = append(out, page)
	}
	return out
}

// handleWebPotluckRequestAdd adds a request, for an organiser.
func (s *Server) handleWebPotluckRequestAdd(w http.ResponseWriter, r *http.Request) {
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
	wanted := 1
	if text := strings.TrimSpace(r.FormValue("wanted")); text != "" {
		var err error
		if wanted, err = strconv.Atoi(text); err != nil {
			s.redirectWithNotice(w, r, ev.ID, "Not added: how many must be a whole number.")
			return
		}
	}
	request, err := s.store.AddPotluckRequest(ev.ID, r.FormValue("category"), r.FormValue("item"), wanted,
		"web:"+session.DiscordUserID)
	if err != nil {
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Asked for "+request.Item+".")
}

// handleWebPotluckRequestRemove takes a request off, for an organiser.
func (s *Server) handleWebPotluckRequestRemove(w http.ResponseWriter, r *http.Request) {
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
	requestID, err := strconv.ParseInt(r.FormValue("request_id"), 10, 64)
	if err != nil {
		http.Error(w, "request_id must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.store.RemovePotluckRequest(ev.ID, requestID, "web:"+session.DiscordUserID); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.redirectWithNotice(w, r, ev.ID, "That request was already off the sheet.")
			return
		}
		log.Printf("[discord-signup] remove potluck request %d of %d: %v", requestID, ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was removed: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Took the request off the sheet. What people said they'd bring for it stays.")
}

// handleWebMyPotluckRequest takes a request for the viewer, from either
// event page.
func (s *Server) handleWebMyPotluckRequest(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberPotluckEvent(w, r)
	if ev == nil {
		return
	}
	requestID, err := strconv.ParseInt(r.FormValue("request_id"), 10, 64)
	if err != nil {
		http.Error(w, "request_id must be an integer", http.StatusBadRequest)
		return
	}
	item, err := s.store.TakePotluckRequest(ev.ID, requestID, session.DiscordUserID)
	switch {
	case errors.Is(err, ErrRequestTaken):
		s.redirectWithNotice(w, r, ev.ID, "Nothing was saved: someone has already taken that one, or you have.")
		return
	case errors.Is(err, ErrNotFound):
		s.redirectWithNotice(w, r, ev.ID, "Nothing was saved: that request is no longer on the sheet.")
		return
	case err != nil:
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Added: "+item.Item+", as asked.")
}

// In the log.

// PotluckRequestHistoryRow is a request as it was ever written.
type PotluckRequestHistoryRow struct {
	Category  string
	Item      string
	Wanted    int
	CreatedAt int64
	CreatedBy string
	RemovedAt int64
	RemovedBy string
}

// PotluckRequestHistory is every request an event's sheet has had.
func (s *Store) PotluckRequestHistory(eventID int64) ([]PotluckRequestHistoryRow, error) {
	rows, err := s.db.Query(`SELECT category, item, wanted, created_at, created_by, removed_at, removed_by
		FROM potluck_requests WHERE event_id = ? ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read potluck request history: %w", err)
	}
	defer rows.Close()
	var out []PotluckRequestHistoryRow
	for rows.Next() {
		var h PotluckRequestHistoryRow
		if err := rows.Scan(&h.Category, &h.Item, &h.Wanted, &h.CreatedAt, &h.CreatedBy, &h.RemovedAt, &h.RemovedBy); err != nil {
			return nil, fmt.Errorf("scan potluck request history: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func potluckRequestLogEntries(history []PotluckRequestHistoryRow, names eventLogNames) []eventLogEntry {
	subject := template.HTML(`<span class="muted">the potluck sheet</span>`)
	var out []eventLogEntry
	for i, h := range history {
		c, _ := potluckCategoryOf(h.Category)
		thing := template.HTMLEscapeString(c.Emoji + " " + h.Item)
		if h.Wanted > 1 {
			thing += fmt.Sprintf(" ×%d", h.Wanted)
		}
		out = append(out, eventLogEntry{At: h.CreatedAt, order: 7_000_000 + i, Subject: subject,
			What: template.HTML("asked for " + thing), By: names.actor(h.CreatedBy)})
		if h.RemovedAt > 0 {
			out = append(out, eventLogEntry{At: h.RemovedAt, order: 8_000_000 + i, Subject: subject,
				What: template.HTML("no longer asking for " + thing), By: names.actor(h.RemovedBy)})
		}
	}
	return out
}
