package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

// The roster table: the event table with everyone's names on it.
//
// It sits in the same channel as the event table and is drawn from the same
// events, so the two can be read side by side and compared. The event table
// answers "what is on"; this answers "who is going", which is the question the
// Details button exists for — and Details is per-viewer, so nobody can see the
// answer without pressing something.
//
// Names, never mentions. A mention would ping every attendee each time the
// table is redrawn, which is every signup, and this message is redrawn more
// often than any other. allowed_mentions suppresses parsing as well, so even a
// display name that looks like a mention cannot reach anybody.

const (
	// rosterTableComponentBudget is Discord's cap on components in one
	// Components V2 message.
	rosterTableComponentBudget = 40

	// rosterTableCharBudget is the cap across every text block in one message.
	// Held a little under Discord's 4000 so that a block measured here and
	// serialised slightly differently there does not lose the whole message.
	rosterTableCharBudget = 3800
)

// rosterTableHeadline is the event's line, minus everything its forum post's
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
func rosterTableHeadline(ev *Event) string {
	if ev.ForumPostID == "" {
		return eventLine(ev)
	}
	line := fmt.Sprintf("💬 <#%s>", ev.ForumPostID)
	if ev.Location != "" {
		line += "  ·  " + ev.Location
	}
	return line
}

// rosterBlock is one event as it will appear: its text, and what that costs.
type rosterBlock struct {
	event      *Event
	text       string
	components int
	characters int
}

// buildRosterBlock renders one event with its roster and measures it.
//
// The measuring is the point. The event table can say "five events per message"
// because every row is about the same size; here a row carrying twenty names is
// many times one carrying none, so a fixed count would either waste most of a
// message or overflow it.
func buildRosterBlock(ev *Event, roster []Signup, first bool) rosterBlock {
	attending, waiting := splitRoster(roster)

	var b strings.Builder
	b.WriteString(rosterTableHeadline(ev))
	// Generous per-line budgets: the packer's job is to decide how many blocks
	// fit in a message, and a single block only needs trimming when one event
	// alone would fill one.
	if line := rosterLine("Going", attending, rosterTableCharBudget/2); line != "" {
		b.WriteString(line)
	} else if len(attending) == 0 {
		b.WriteString("\n-# Nobody yet.")
	}
	if line := rosterLine("Waiting", waiting, rosterTableCharBudget/4); line != "" {
		b.WriteString(line)
	}
	text := trimTo(b.String(), textDisplayLimit)

	// One text block, one action row, its buttons, and the separator that
	// divides this block from the one above it.
	components := 2 + len(rosterTableButtons(ev))
	if !first {
		components++
	}
	return rosterBlock{event: ev, text: text, components: components, characters: len([]rune(text))}
}

// packRosterTable fills each message as full as it will go and starts another
// when the next event does not fit.
//
// Returns at least one page, so an empty guild still gets a message saying
// there is nothing on rather than leaving whatever was there last week.
func packRosterTable(events []Event, rosters map[int64][]Signup) [][]rosterBlock {
	pages := [][]rosterBlock{}
	var page []rosterBlock
	// The container itself is a component.
	components, characters := 1, 0

	for i := range events {
		ev := &events[i]
		block := buildRosterBlock(ev, rosters[ev.ID], len(page) == 0)
		overComponents := components+block.components > rosterTableComponentBudget
		overCharacters := characters+block.characters > rosterTableCharBudget
		if len(page) > 0 && (overComponents || overCharacters) {
			pages = append(pages, page)
			page = nil
			components, characters = 1, 0
			// Re-measured as the first block on its new page, which is one
			// component cheaper: no separator above it.
			block = buildRosterBlock(ev, rosters[ev.ID], true)
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

// RenderRosterTablePage draws one packed page.
func RenderRosterTablePage(page []rosterBlock, index, total int) map[string]any {
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
			"type": componentTypeActionRow, "components": rosterTableButtons(block.event),
		})
	}
	if total > 1 {
		body = append(body, textBlock(fmt.Sprintf("-# Page %d of %d", index+1, total)))
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

// rosterTableButtons drops Edit, because Details is the edit form now for
// anybody allowed to use it. One button fewer per row is also one component
// fewer, which is more events per message.
func rosterTableButtons(ev *Event) []any {
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

// RefreshRosterTable rewrites the roster table in place.
//
// Posted into the event table's own channel: it is a second view of the same
// events, meant to be read beside the first, not a second thing to configure.
func (s *Server) RefreshRosterTable(guildID string) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) {
		return nil // no event table means no channel to put this beside
	}
	if err != nil {
		return err
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
	pages := packRosterTable(events, rosters)

	existing, err := s.store.RosterTablePages(guildID)
	if err != nil {
		return err
	}
	byPage := map[int]string{}
	for _, p := range existing {
		byPage[p.Page] = p.MessageID
	}

	for i, page := range pages {
		payload := RenderRosterTablePage(page, i, len(pages))
		if messageID, ok := byPage[i]; ok {
			if err := s.discord.EditMessage(table.ChannelID, messageID, payload); err != nil {
				return fmt.Errorf("edit roster table page %d: %w", i, err)
			}
			continue
		}
		messageID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post roster table page %d: %w", i, err)
		}
		if err := s.store.SetRosterTablePage(guildID, i, messageID); err != nil {
			return err
		}
	}
	// Pages that are no longer needed are deleted rather than left saying
	// something that was true last week.
	for _, p := range existing {
		if p.Page < len(pages) {
			continue
		}
		if err := s.discord.DeleteMessage(table.ChannelID, p.MessageID); err != nil {
			log.Printf("[discord-signup] delete spare roster table page %d: %v", p.Page, err)
		}
		if err := s.store.DeleteRosterTablePage(guildID, p.Page); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) refreshRosterTableQuietly(guildID string) {
	if err := s.RefreshRosterTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh roster table for %s: %v", guildID, err)
	}
}

// RosterTablePages lists the messages the roster table currently occupies.
func (s *Store) RosterTablePages(guildID string) ([]TablePage, error) {
	rows, err := s.db.Query(
		`SELECT page, message_id FROM roster_table_pages WHERE guild_id = ? ORDER BY page ASC`,
		guildID)
	if err != nil {
		return nil, fmt.Errorf("list roster table pages: %w", err)
	}
	defer rows.Close()
	var out []TablePage
	for rows.Next() {
		var p TablePage
		if err := rows.Scan(&p.Page, &p.MessageID); err != nil {
			return nil, fmt.Errorf("scan roster table page: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetRosterTablePage records the message holding one page.
func (s *Store) SetRosterTablePage(guildID string, page int, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO roster_table_pages (guild_id, page, message_id, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(guild_id, page) DO UPDATE SET message_id = excluded.message_id,
		                                          updated_at = excluded.updated_at`,
		guildID, page, messageID, now())
	if err != nil {
		return fmt.Errorf("set roster table page: %w", err)
	}
	return nil
}

// DeleteRosterTablePage forgets a page the table has shrunk past.
func (s *Store) DeleteRosterTablePage(guildID string, page int) error {
	_, err := s.db.Exec(
		`DELETE FROM roster_table_pages WHERE guild_id = ? AND page = ?`, guildID, page)
	if err != nil {
		return fmt.Errorf("delete roster table page: %w", err)
	}
	return nil
}
