package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

// The event table: every upcoming event, with who is going.
//
// One table, not two. It ran for an evening as a second table beside the first
// so the shapes could be compared, and the comparison settled it — a row that
// names the people going answers the question the first table made you press
// Details for, so keeping both meant two messages saying the same thing and
// disagreeing while they updated.
//
// Names, never mentions. A mention would ping every attendee each time the
// table is redrawn, which is every signup, and this message is redrawn more
// often than any other. allowed_mentions suppresses parsing as well, so even a
// display name that looks like a mention cannot reach anybody.

const (
	// eventTableComponentBudget is Discord's cap on components in one
	// Components V2 message.
	eventTableComponentBudget = 40

	// eventTableCharBudget is the cap across every text block in one message.
	// Held a little under Discord's 4000 so that a block measured here and
	// serialised slightly differently there does not lose the whole message.
	eventTableCharBudget = 3800
)

// eventTableHeadline is the event's line, minus everything its forum post's
// title already says.
//
// That title is "[3/8] Board game night — 8/29 4pm", and Discord renders
// <#post> as exactly those words — so printing the badge, the name and the time
// beside the link was saying all of it twice and spending the room this table
// needs for names.
//
// Location stays: it is the one thing the title does not carry. An event with
// no forum post gets the full line, because then there is nothing to click and
// nothing else saying what it is.
//
// ⚠️ The count now reaches the reader through that title, and Discord
// rate-limits thread renames to about two per ten minutes — so under fast
// signups the badge can lag. The names below it cannot: they are written into
// this message, which has no such limit.
func eventTableHeadline(ev *Event) string {
	if ev.ForumPostID == "" {
		return eventLine(ev)
	}
	// Discord renders <#post> as the post's title, which already carries the
	// name, the date and the throttled count. The live count is on the next
	// line, where a message edit can keep it current.
	line := fmt.Sprintf("<#%s>", ev.ForumPostID)
	if ev.Location != "" {
		line += "  📍  " + ev.Location
	}
	return line
}

// namesWithin joins display names inside a rune budget, dropping names off
// the end rather than cutting one in half.
func namesWithin(signups []Signup, budget int) string {
	names := rosterDisplayNames(signups)
	full := strings.Join(names, ", ")
	if len([]rune(full)) <= budget {
		return full
	}
	for shown := len(names) - 1; shown >= 1; shown-- {
		line := fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
		if len([]rune(line)) <= budget {
			return line
		}
	}
	return pluralise(len(names), "person")
}

// eventTableBlock is one event as it will appear: its text, and what that costs.
type eventTableBlock struct {
	event      *Event
	text       string
	components int
	characters int
}

// buildEventTableBlock renders one event with its roster and measures it.
//
// The measuring is the point. The event table can say "five events per message"
// because every row is about the same size; here a row carrying twenty names is
// many times one carrying none, so a fixed count would either waste most of a
// message or overflow it.
func buildEventTableBlock(ev *Event, roster []Signup, first bool, buttons func(*Event) []any) eventTableBlock {
	attending, waiting := splitRoster(roster)

	var b strings.Builder
	b.WriteString(eventTableHeadline(ev))
	// Second line: the live count, then who. Generous per-line budgets: the
	// packer decides how many blocks fit in a message, and a single block only
	// needs trimming when one event alone would fill one.
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "\n%d/%d 👥 ", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "\n%d 👥 ", ev.AttendingCount)
	}
	if len(attending) == 0 {
		b.WriteString("Nobody yet.")
	} else {
		b.WriteString(namesWithin(attending, eventTableCharBudget/2))
	}
	if len(waiting) > 0 {
		b.WriteString("\n⏳ " + namesWithin(waiting, eventTableCharBudget/4))
	}
	text := trimTo(b.String(), textDisplayLimit)

	// One text block, one action row, its buttons, and the separator that
	// divides this block from the one above it.
	components := 2 + len(buttons(ev))
	if !first {
		components++
	}
	return eventTableBlock{event: ev, text: text, components: components, characters: len([]rune(text))}
}

// packEventTable fills each message as full as it will go and starts another
// when the next event does not fit.
//
// Returns at least one page, so an empty guild still gets a message saying
// there is nothing on rather than leaving whatever was there last week.
func packEventTable(events []Event, rosters map[int64][]Signup, buttons func(*Event) []any, reserve int) [][]eventTableBlock {
	pages := [][]eventTableBlock{}
	var page []eventTableBlock
	// The container itself is a component.
	components, characters := 1, 0

	for i := range events {
		ev := &events[i]
		block := buildEventTableBlock(ev, rosters[ev.ID], len(page) == 0, buttons)
		// reserve holds room for a trailing action row on whichever page turns
		// out to be last; nobody knows which that is while packing, so every
		// page keeps it. It is three components on the management table and
		// none on the public one.
		overComponents := components+block.components > eventTableComponentBudget-reserve
		overCharacters := characters+block.characters > eventTableCharBudget
		if len(page) > 0 && (overComponents || overCharacters) {
			pages = append(pages, page)
			page = nil
			components, characters = 1, 0
			// Re-measured as the first block on its new page, which is one
			// component cheaper: no separator above it.
			block = buildEventTableBlock(ev, rosters[ev.ID], true, buttons)
		}
		page = append(page, block)
		components += block.components
		characters += block.characters
	}
	if len(page) > 0 || len(pages) == 0 {
		pages = append(pages, page)
	}
	return pages
}

// RenderEventTablePage draws one packed page.
func RenderEventTablePage(page []eventTableBlock, index, total int, buttons func(*Event) []any, trailing []any) map[string]any {
	body := []any{}
	if len(page) == 0 {
		body = append(body, textBlock("-# Nothing coming up."))
	}
	for i, block := range page {
		if i > 0 {
			body = append(body, map[string]any{
				"type": componentTypeSeparator, "divider": true, "spacing": 1,
			})
		}
		body = append(body, textBlock(block.text))
		body = append(body, map[string]any{
			"type": componentTypeActionRow, "components": buttons(block.event),
		})
	}
	if total > 1 {
		body = append(body, textBlock(fmt.Sprintf("-# Page %d of %d", index+1, total)))
	}
	// Controls that belong to the table as a whole go on its last page only.
	if index == total-1 && len(trailing) > 0 {
		body = append(body, map[string]any{"type": componentTypeActionRow, "components": trailing})
	}
	return map[string]any{
		"flags": messageFlagComponentsV2,
		"components": []any{map[string]any{
			"type": componentTypeContainer, "accent_color": panelAccentColour,
			"components": body,
		}},
		// Names are written as plain text, and this suppresses parsing as well,
		// so a display name that happens to look like a mention still cannot
		// ping anybody. This message is rewritten on every signup.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// eventTableButtons drops Edit, because Details is the edit form now for
// anybody allowed to use it. One button fewer per row is also one component
// fewer, which is more events per message.
func eventTableButtons(ev *Event) []any {
	buttons := []any{}
	if ev.Status == StatusOpen {
		buttons = append(buttons,
			map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
				"label": "Join", "custom_id": JoinCustomID(ev.ID)},
			map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Leave", "custom_id": LeaveCustomID(ev.ID)})
	}
	buttons = append(buttons,
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID)})
	return buttons
}

// managementButtons is the row on the management table: what an organiser
// does to an event, and nothing a member does.
func managementButtons(ev *Event) []any {
	buttons := []any{
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Edit", "custom_id": EditCustomID(ev.ID)},
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Repeat", "custom_id": RepeatCustomID(ev.ID)},
	}
	// One toggle whose label says which way it goes. Closed means nobody new
	// can join while everyone on it stays; it is not cancelled.
	switch ev.Status {
	case StatusOpen:
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Close signups", "custom_id": CloseCustomID(ev.ID)})
	case StatusClosed:
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Reopen signups", "custom_id": CloseCustomID(ev.ID)})
	}
	return append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleDanger,
		"label": "Cancel", "custom_id": CancelCustomID(ev.ID)})
}

// managementTrailing is the last page's row on the management table: making a
// new event. A thing a person does rather than a thing about one event, which
// is why it is not on a row.
func managementTrailing() []any {
	return []any{
		map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
			"label": "Create an event", "custom_id": CreateCustomID()},
	}
}

// tableSurface is one channel's packed table: where it lives, what its rows
// can do, and where its pages are recorded. The public table and the
// management table are the same packer with different answers to those three.
type tableSurface struct {
	channelID string
	buttons   func(*Event) []any
	trailing  []any
	pages     func() ([]TablePage, error)
	setPage   func(page int, messageID string) error
	dropPage  func(page int) error
}

// RefreshEventTable rewrites the public table in place.
func (s *Server) RefreshEventTable(guildID string) error {
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.publishPackedTable(guildID, tableSurface{
		channelID: table.ChannelID, buttons: eventTableButtons,
		pages:    func() ([]TablePage, error) { return s.store.TablePages(guildID) },
		setPage:  func(p int, m string) error { return s.store.SetTablePage(guildID, p, m) },
		dropPage: func(p int) error { return s.store.DeleteTablePage(guildID, p) },
	})
}

// RefreshManagementTable rewrites the management table in place: the same
// events, with Edit on each row and Create on the end.
func (s *Server) RefreshManagementTable(guildID string) error {
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) || (err == nil && table.ManagementChannelID == "") {
		return nil
	}
	if err != nil {
		return err
	}
	return s.publishPackedTable(guildID, tableSurface{
		channelID: table.ManagementChannelID, buttons: managementButtons, trailing: managementTrailing(),
		pages:    func() ([]TablePage, error) { return s.store.ManagementPages(guildID) },
		setPage:  func(p int, m string) error { return s.store.SetManagementPage(guildID, p, m) },
		dropPage: func(p int) error { return s.store.DeleteManagementPage(guildID, p) },
	})
}

func (s *Server) publishPackedTable(guildID string, surface tableSurface) error {
	if s.discord == nil {
		return nil
	}
	events, err := s.liveEventsFor(guildID)
	if err != nil {
		return err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].StartsAt < events[j].StartsAt })

	rosters := map[int64][]Signup{}
	for i := range events {
		roster, err := s.store.Roster(events[i].ID, false)
		if err != nil {
			return fmt.Errorf("roster for event %d: %w", events[i].ID, err)
		}
		rosters[events[i].ID] = roster
	}
	pages := packEventTable(events, rosters, surface.buttons, len(surface.trailing)+1)

	existing, err := surface.pages()
	if err != nil {
		return err
	}
	byPage := map[int]string{}
	for _, p := range existing {
		byPage[p.Page] = p.MessageID
	}

	for i, page := range pages {
		payload := RenderEventTablePage(page, i, len(pages), surface.buttons, surface.trailing)
		if messageID, ok := byPage[i]; ok {
			if err := s.discord.EditMessage(surface.channelID, messageID, payload); err != nil {
				return fmt.Errorf("edit table page %d: %w", i, err)
			}
			continue
		}
		messageID, err := s.discord.CreateMessage(surface.channelID, payload)
		if err != nil {
			return fmt.Errorf("post table page %d: %w", i, err)
		}
		if err := surface.setPage(i, messageID); err != nil {
			return err
		}
	}
	// Pages that are no longer needed are deleted rather than left saying
	// something that was true last week.
	for _, p := range existing {
		if p.Page < len(pages) {
			continue
		}
		if err := s.discord.DeleteMessage(surface.channelID, p.MessageID); err != nil {
			log.Printf("[discord-signup] delete spare table page %d: %v", p.Page, err)
		}
		if err := surface.dropPage(p.Page); err != nil {
			return err
		}
	}
	return nil
}

// refreshTablesQuietly redraws both tables. One trigger for both, so the
// public one and the management one cannot show different rosters.
func (s *Server) refreshTablesQuietly(guildID string) {
	if err := s.RefreshEventTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh table for %s: %v", guildID, err)
	}
	if err := s.RefreshManagementTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh management table for %s: %v", guildID, err)
	}
}

// ManagementPages lists the messages the management table currently occupies.
func (s *Store) ManagementPages(guildID string) ([]TablePage, error) {
	rows, err := s.db.Query(
		`SELECT page, message_id FROM management_pages WHERE guild_id = ? ORDER BY page ASC`, guildID)
	if err != nil {
		return nil, fmt.Errorf("list management pages: %w", err)
	}
	defer rows.Close()
	var out []TablePage
	for rows.Next() {
		var p TablePage
		if err := rows.Scan(&p.Page, &p.MessageID); err != nil {
			return nil, fmt.Errorf("scan management page: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetManagementPage records the message holding one page.
func (s *Store) SetManagementPage(guildID string, page int, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO management_pages (guild_id, page, message_id, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(guild_id, page) DO UPDATE SET message_id = excluded.message_id,
		                                          updated_at = excluded.updated_at`,
		guildID, page, messageID, now())
	if err != nil {
		return fmt.Errorf("set management page: %w", err)
	}
	return nil
}

// DeleteManagementPage forgets a page the table has shrunk past.
func (s *Store) DeleteManagementPage(guildID string, page int) error {
	_, err := s.db.Exec(`DELETE FROM management_pages WHERE guild_id = ? AND page = ?`, guildID, page)
	if err != nil {
		return fmt.Errorf("delete management page: %w", err)
	}
	return nil
}
