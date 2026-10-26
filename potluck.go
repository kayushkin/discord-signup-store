package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The potluck sheet: who is bringing what.
//
// An organiser switches it on for an event (events.potluck_enabled) and may
// say how many of each category the event wants (potluck_needs). Everyone
// going says what they will bring, from the button on the event's card or the
// reminder DM, or says they are bringing nothing. The card shows the whole
// sheet to everyone and what is still wanted; the organiser's page also shows
// who going has not answered yet, and can nudge them. A DM goes by itself to
// anyone still silent a day before (potluckreminders.go).
//
// Only people going are on the sheet. Someone who leaves drops off it — what
// they said stays stored and comes back if they rejoin — and the waitlist
// and Maybe are never asked.

// Potluck categories. potluckNothing is not a category on the sheet: it is
// the answer "I am bringing nothing", stored as a row so the person stops
// counting as unanswered.
const (
	PotluckFood     = "food"
	PotluckDrinks   = "drinks"
	PotluckSupplies = "supplies"
	PotluckOther    = "other"
	potluckNothing  = "nothing"
)

// potluckCategory is one kind of thing to bring, in the order the sheet
// lists them.
type potluckCategory struct {
	Key   string
	Label string
	Emoji string
	// Prompt is the modal's question; Example its placeholder.
	Prompt  string
	Example string
}

var potluckCategories = []potluckCategory{
	{PotluckFood, "Food", "🍲", "What food are you bringing?", "Lasagna, serves 8"},
	{PotluckDrinks, "Drinks", "🥤", "What drinks are you bringing?", "Two bottles of lemonade"},
	{PotluckSupplies, "Supplies", "🍴", "What supplies are you bringing?", "Plates, cups and ice"},
	{PotluckOther, "Other", "✨", "What else are you bringing?", "A speaker for music"},
}

func potluckCategoryOf(key string) (potluckCategory, bool) {
	for _, c := range potluckCategories {
		if c.Key == key {
			return c, true
		}
	}
	return potluckCategory{}, false
}

const (
	// potluckItemLimit is the most characters one item may have.
	potluckItemLimit = 100
	// potluckItemsPerPerson stops one person filling the sheet.
	potluckItemsPerPerson = 6
	// potluckWantedLimit is the most an organiser may ask for of one category.
	potluckWantedLimit = 99
)

// ErrPotluckOff is a change to the sheet of an event that has none.
var ErrPotluckOff = errors.New("this event has no potluck sheet")

// ErrPotluckClosed is a change to the sheet of an event that is over or
// cancelled.
var ErrPotluckClosed = errors.New("this event is over, so its potluck sheet is closed")

// ErrNotGoing is someone who is not going trying to say what they will bring.
var ErrNotGoing = errors.New("only people going can fill in the potluck sheet")

// ErrInvalidPotluck is an item or a need the sheet cannot take.
var ErrInvalidPotluck = errors.New("invalid potluck entry")

// PotluckItem is one thing one person going said they will bring.
type PotluckItem struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	Category      string `json:"category"`
	Item          string `json:"item"`
	AddedAt       int64  `json:"added_at"`
}

// PotluckSheet is an event's sheet as it stands. Read from the rows each
// time, like the roster's counts, so it cannot disagree with them.
type PotluckSheet struct {
	// Needs is how many of each category the organiser asked for; a
	// category not in it has no number.
	Needs map[string]int `json:"needs"`
	// Items are the things people going will bring, in the order they said.
	Items []PotluckItem `json:"items"`
	// BringingNothing are the Discord user ids of people going who said they
	// are bringing nothing.
	BringingNothing []string `json:"bringing_nothing"`
}

// Answered says whether this person has said anything: an item, or nothing.
func (p *PotluckSheet) Answered(userID string) bool {
	return len(p.ItemsOf(userID)) > 0 || p.SaidNothing(userID)
}

// SaidNothing says whether this person said they are bringing nothing.
func (p *PotluckSheet) SaidNothing(userID string) bool {
	for _, id := range p.BringingNothing {
		if id == userID {
			return true
		}
	}
	return false
}

// ItemsOf is what this person said they will bring.
func (p *PotluckSheet) ItemsOf(userID string) []PotluckItem {
	var out []PotluckItem
	for _, it := range p.Items {
		if it.DiscordUserID == userID {
			out = append(out, it)
		}
	}
	return out
}

// ItemsIn is what everyone is bringing in one category.
func (p *PotluckSheet) ItemsIn(category string) []PotluckItem {
	var out []PotluckItem
	for _, it := range p.Items {
		if it.Category == category {
			out = append(out, it)
		}
	}
	return out
}

// PotluckCount is one category's line in the summary.
type PotluckCount struct {
	Category potluckCategory
	Have     int
	// Wanted is 0 when the organiser asked for no number.
	Wanted int
	Items  []PotluckItem
}

// Missing is how many more the category wants.
func (c PotluckCount) Missing() int { return max(0, c.Wanted-c.Have) }

// Counts is one line per category that has a need or an item, in sheet order.
func (p *PotluckSheet) Counts() []PotluckCount {
	var out []PotluckCount
	for _, c := range potluckCategories {
		items := p.ItemsIn(c.Key)
		if len(items) == 0 && p.Needs[c.Key] == 0 {
			continue
		}
		out = append(out, PotluckCount{Category: c, Have: len(items), Wanted: p.Needs[c.Key], Items: items})
	}
	return out
}

// SummaryLine is the counts in one line: "🍲 Food 3/6 · 🥤 Drinks 1/3".
func (p *PotluckSheet) SummaryLine() string {
	var parts []string
	for _, c := range p.Counts() {
		if c.Wanted > 0 {
			parts = append(parts, fmt.Sprintf("%s %s %d/%d", c.Category.Emoji, c.Category.Label, c.Have, c.Wanted))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s %d", c.Category.Emoji, c.Category.Label, c.Have))
		}
	}
	if len(parts) == 0 {
		return "Nobody has said what they're bringing yet."
	}
	return strings.Join(parts, " · ")
}

// StillNeededLine says what the organiser asked for that nobody has said
// they will bring yet, or "" when nothing is missing.
func (p *PotluckSheet) StillNeededLine() string {
	var parts []string
	for _, c := range p.Counts() {
		if m := c.Missing(); m > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m, strings.ToLower(c.Category.Label)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Still needed: " + strings.Join(parts, ", ")
}

// PotluckSheetOf reads an event's sheet.
func (s *Store) PotluckSheetOf(eventID int64) (*PotluckSheet, error) {
	sheet := &PotluckSheet{Needs: map[string]int{}, Items: []PotluckItem{}, BringingNothing: []string{}}
	rows, err := s.db.Query(`SELECT category, wanted FROM potluck_needs WHERE event_id = ?`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read potluck needs: %w", err)
	}
	for rows.Next() {
		var category string
		var wanted int
		if err := rows.Scan(&category, &wanted); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan potluck need: %w", err)
		}
		sheet.Needs[category] = wanted
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate potluck needs: %w", err)
	}

	rows, err = s.db.Query(`
		SELECT p.id, p.event_id, p.discord_user_id, p.category, p.item, p.added_at
		FROM potluck_items p
		JOIN signups s ON s.event_id = p.event_id AND s.discord_user_id = p.discord_user_id
		WHERE p.event_id = ? AND p.removed_at = 0 AND s.state = ?
		ORDER BY p.added_at, p.id`, eventID, StateAttending)
	if err != nil {
		return nil, fmt.Errorf("read potluck items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it PotluckItem
		if err := rows.Scan(&it.ID, &it.EventID, &it.DiscordUserID, &it.Category, &it.Item, &it.AddedAt); err != nil {
			return nil, fmt.Errorf("scan potluck item: %w", err)
		}
		if it.Category == potluckNothing {
			sheet.BringingNothing = append(sheet.BringingNothing, it.DiscordUserID)
			continue
		}
		sheet.Items = append(sheet.Items, it)
	}
	return sheet, rows.Err()
}

// beginPotluckChange opens a transaction for a change to one person's
// answer, refusing an event with no sheet and a person not going.
func (s *Store) beginPotluckChange(eventID int64, userID string) (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	var enabled bool
	var status string
	err = tx.QueryRow(`SELECT potluck_enabled, status FROM events WHERE id = ? AND deleted_at = 0`, eventID).Scan(&enabled, &status)
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
		return nil, ErrPotluckOff
	}
	if !potluckTakesAnswers(&Event{PotluckEnabled: enabled, Status: status}) {
		tx.Rollback()
		return nil, ErrPotluckClosed
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

// AddPotluckItem records one thing a person going will bring. It replaces
// a "bringing nothing" answer.
func (s *Store) AddPotluckItem(eventID int64, userID, category, item string) (*PotluckItem, error) {
	if _, ok := potluckCategoryOf(category); !ok {
		return nil, fmt.Errorf("%w: category %q is not one of food, drinks, supplies, other", ErrInvalidPotluck, category)
	}
	item = strings.Join(strings.Fields(item), " ")
	if item == "" {
		return nil, fmt.Errorf("%w: say what you are bringing", ErrInvalidPotluck)
	}
	if len([]rune(item)) > potluckItemLimit {
		return nil, fmt.Errorf("%w: keep it to %d characters", ErrInvalidPotluck, potluckItemLimit)
	}
	tx, err := s.beginPotluckChange(eventID, userID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM potluck_items WHERE event_id = ? AND discord_user_id = ?
		AND removed_at = 0 AND category != ?`, eventID, userID, potluckNothing).Scan(&count); err != nil {
		return nil, fmt.Errorf("count potluck items: %w", err)
	}
	if count >= potluckItemsPerPerson {
		return nil, fmt.Errorf("%w: you already have %d things on the sheet", ErrInvalidPotluck, count)
	}
	ts := now()
	if _, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND removed_at = 0 AND category = ?`,
		ts, ActorUser, eventID, userID, potluckNothing); err != nil {
		return nil, fmt.Errorf("clear bringing nothing: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO potluck_items (event_id, discord_user_id, category, item, added_at)
		VALUES (?,?,?,?,?)`, eventID, userID, category, item, ts)
	if err != nil {
		return nil, fmt.Errorf("add potluck item: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read potluck item id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &PotluckItem{ID: id, EventID: eventID, DiscordUserID: userID, Category: category, Item: item, AddedAt: ts}, nil
}

// SayBringingNothing records that a person going is bringing nothing,
// taking anything they said before off the sheet.
func (s *Store) SayBringingNothing(eventID int64, userID string) error {
	tx, err := s.beginPotluckChange(eventID, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	if _, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND removed_at = 0`, ts, ActorUser, eventID, userID); err != nil {
		return fmt.Errorf("clear potluck answer: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO potluck_items (event_id, discord_user_id, category, item, added_at)
		VALUES (?,?,?,'',?)`, eventID, userID, potluckNothing, ts); err != nil {
		return fmt.Errorf("record bringing nothing: %w", err)
	}
	return tx.Commit()
}

// ClearPotluckAnswer takes everything a person said off the sheet, so they
// count as unanswered again. Returns how many rows it took off.
func (s *Store) ClearPotluckAnswer(eventID int64, userID string) (int64, error) {
	tx, err := s.beginPotluckChange(eventID, userID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND removed_at = 0`, now(), ActorUser, eventID, userID)
	if err != nil {
		return 0, fmt.Errorf("clear potluck answer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared: %w", err)
	}
	return n, tx.Commit()
}

// RemovePotluckItem takes one row off an event's sheet, for an organiser.
func (s *Store) RemovePotluckItem(eventID, itemID int64, actor string) error {
	res, err := s.db.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE id = ? AND event_id = ? AND removed_at = 0`, now(), actor, itemID, eventID)
	if err != nil {
		return fmt.Errorf("remove potluck item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count removed: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPotluckNeeds replaces how many of each category an event wants. A
// category at 0 has no number.
func (s *Store) SetPotluckNeeds(eventID int64, needs map[string]int) error {
	for category, wanted := range needs {
		if _, ok := potluckCategoryOf(category); !ok {
			return fmt.Errorf("%w: category %q is not one of food, drinks, supplies, other", ErrInvalidPotluck, category)
		}
		if wanted < 0 || wanted > potluckWantedLimit {
			return fmt.Errorf("%w: %s wanted must be 0 to %d", ErrInvalidPotluck, category, potluckWantedLimit)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM potluck_needs WHERE event_id = ?`, eventID); err != nil {
		return fmt.Errorf("clear potluck needs: %w", err)
	}
	for category, wanted := range needs {
		if wanted == 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO potluck_needs (event_id, category, wanted) VALUES (?,?,?)`,
			eventID, category, wanted); err != nil {
			return fmt.Errorf("save potluck need: %w", err)
		}
	}
	return tx.Commit()
}

// clearPotluckForNextDateTx starts a repeating event's next date with an
// empty sheet and nobody reminded. The needs stay.
func clearPotluckForNextDateTx(tx *sql.Tx, eventID, ts int64) error {
	if _, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE event_id = ? AND removed_at = 0`, ts, ActorRecurrence, eventID); err != nil {
		return fmt.Errorf("clear potluck sheet: %w", err)
	}
	if _, err := tx.Exec(`UPDATE potluck_reminders SET cleared_at = ?
		WHERE event_id = ? AND cleared_at = 0`, ts, eventID); err != nil {
		return fmt.Errorf("clear potluck reminders: %w", err)
	}
	return nil
}

// writePotluckSection adds the sheet to an event's card: the counts, what is
// still needed, and who is bringing what, by category. Without withItems it
// leaves out who is bringing what and says the button shows it: a big
// event's roster can leave no room on the card for the list.
func writePotluckSection(b *strings.Builder, sheet *PotluckSheet, open, withItems bool) {
	b.WriteString("\n**🍲 Potluck**")
	if open {
		b.WriteString(" — press **What I'm bringing** to add yours")
	}
	b.WriteString("\n")
	b.WriteString(sheet.SummaryLine())
	b.WriteString("\n")
	if line := sheet.StillNeededLine(); line != "" {
		b.WriteString(line + "\n")
	}
	if !withItems {
		if len(sheet.Items) > 0 {
			b.WriteString("Press **What I'm bringing**, or open the event page, to see who is bringing what.\n")
		}
		return
	}
	for _, c := range sheet.Counts() {
		if len(c.Items) == 0 {
			continue
		}
		parts := make([]string, len(c.Items))
		for i, it := range c.Items {
			parts[i] = fmt.Sprintf("<@%s> %s", it.DiscordUserID, it.Item)
		}
		fmt.Fprintf(b, "%s %s\n", c.Category.Emoji, strings.Join(parts, " · "))
	}
}

// potluckTakesAnswers says whether people may change the sheet: it is on,
// and the event is neither over nor cancelled. Closing signups does not
// close the sheet.
func potluckTakesAnswers(ev *Event) bool {
	return ev.PotluckEnabled && (ev.Status == StatusOpen || ev.Status == StatusClosed)
}

// potluckButton opens the person's own view of the sheet.
func potluckButton(eventID int64) map[string]any {
	return map[string]any{
		"type":      componentTypeButton,
		"style":     buttonStyleSecondary,
		"label":     "What I'm bringing",
		"emoji":     map[string]any{"name": "🍲"},
		"custom_id": PotluckCustomID(eventID),
	}
}

// RemoveOwnPotluckItem takes one of a person's own rows off an event's
// sheet; a row that is not theirs is ErrNotFound.
func (s *Store) RemoveOwnPotluckItem(eventID, itemID int64, userID string) error {
	res, err := s.db.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
		WHERE id = ? AND event_id = ? AND discord_user_id = ? AND removed_at = 0`, now(), ActorUser, itemID, eventID, userID)
	if err != nil {
		return fmt.Errorf("remove potluck item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count removed: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// potluckDescriptionSection is the sheet for the description of Discord's own
// event, inside a rune budget: the counts and what is still needed, then who
// is bringing what by name — never a mention, which would ping on every
// rewrite — shortening the items, then dropping people off the end, before
// leaving the list out. "" when not even the counts fit.
func potluckDescriptionSection(sheet *PotluckSheet, roster []Signup, budget int) string {
	names := map[string]string{}
	for _, sg := range roster {
		names[sg.DiscordUserID] = sg.NameOnDiscord()
	}
	head := "\nPotluck: " + sheet.SummaryLine()
	if line := sheet.StillNeededLine(); line != "" {
		head += "\n" + line
	}
	if len([]rune(head)) > budget {
		return ""
	}
	list := func(itemLimit, shown int) string {
		var parts []string
		for _, c := range sheet.Counts() {
			for _, it := range c.Items {
				parts = append(parts, fmt.Sprintf("%s %s: %s", c.Category.Emoji, names[it.DiscordUserID], truncate(it.Item, itemLimit)))
			}
		}
		if len(parts) == 0 {
			return ""
		}
		if shown >= len(parts) {
			return "\n" + strings.Join(parts, "\n")
		}
		return "\n" + strings.Join(parts[:shown], "\n") + fmt.Sprintf("\n…and %d more", len(parts)-shown)
	}
	if l := list(potluckItemLimit, len(sheet.Items)); len([]rune(head+l)) <= budget {
		return head + l
	}
	for shown := len(sheet.Items); shown >= 1; shown-- {
		if l := list(40, shown); len([]rune(head+l)) <= budget {
			return head + l
		}
	}
	return head
}
