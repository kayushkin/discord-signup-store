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
	var parts []string
	// The count lives here now. It used to be read off the thread title, and
	// thread renames are rate-limited to about two per ten minutes — so under
	// signups the number people were reading was the one from two renames ago.
	// A message edit has no such limit, so the count belongs in the message.
	if ev.Capacity > 0 {
		parts = append(parts, fmt.Sprintf("`%d/%d`", ev.AttendingCount, ev.Capacity))
	} else if ev.AttendingCount > 0 {
		parts = append(parts, fmt.Sprintf("`%d`", ev.AttendingCount))
	}
	parts = append(parts, fmt.Sprintf("💬 <#%s>", ev.ForumPostID))
	if ev.Location != "" {
		parts = append(parts, ev.Location)
	}
	return strings.Join(parts, "  ·  ")
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
func buildEventTableBlock(ev *Event, roster []Signup, first bool) eventTableBlock {
	attending, waiting := splitRoster(roster)

	var b strings.Builder
	b.WriteString(eventTableHeadline(ev))
	// Generous per-line budgets: the packer's job is to decide how many blocks
	// fit in a message, and a single block only needs trimming when one event
	// alone would fill one.
	if line := rosterLine("Going", attending, eventTableCharBudget/2); line != "" {
		b.WriteString(line)
	} else if len(attending) == 0 {
		b.WriteString("\n-# Nobody yet.")
	}
	if line := rosterLine("Waiting", waiting, eventTableCharBudget/4); line != "" {
		b.WriteString(line)
	}
	text := trimTo(b.String(), textDisplayLimit)

	// One text block, one action row, its buttons, and the separator that
	// divides this block from the one above it.
	components := 2 + len(eventTableButtons(ev))
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
func packEventTable(events []Event, rosters map[int64][]Signup) [][]eventTableBlock {
	pages := [][]eventTableBlock{}
	var page []eventTableBlock
	// The container itself is a component.
	components, characters := 1, 0

	for i := range events {
		ev := &events[i]
		block := buildEventTableBlock(ev, rosters[ev.ID], len(page) == 0)
		overComponents := components+block.components > eventTableComponentBudget
		overCharacters := characters+block.characters > eventTableCharBudget
		if len(page) > 0 && (overComponents || overCharacters) {
			pages = append(pages, page)
			page = nil
			components, characters = 1, 0
			// Re-measured as the first block on its new page, which is one
			// component cheaper: no separator above it.
			block = buildEventTableBlock(ev, rosters[ev.ID], true)
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
func RenderEventTablePage(page []eventTableBlock, index, total int) map[string]any {
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
			"type": componentTypeActionRow, "components": eventTableButtons(block.event),
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

// RefreshEventTable rewrites the table in place.
func (s *Server) RefreshEventTable(guildID string) error {
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
	pages := packEventTable(events, rosters)

	existing, err := s.store.TablePages(guildID)
	if err != nil {
		return err
	}
	byPage := map[int]string{}
	for _, p := range existing {
		byPage[p.Page] = p.MessageID
	}

	for i, page := range pages {
		payload := RenderEventTablePage(page, i, len(pages))
		if messageID, ok := byPage[i]; ok {
			if err := s.discord.EditMessage(table.ChannelID, messageID, payload); err != nil {
				return fmt.Errorf("edit table page %d: %w", i, err)
			}
			continue
		}
		messageID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post table page %d: %w", i, err)
		}
		if err := s.store.SetTablePage(guildID, i, messageID); err != nil {
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
			log.Printf("[discord-signup] delete spare table page %d: %v", p.Page, err)
		}
		if err := s.store.DeleteTablePage(guildID, p.Page); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) refreshEventTableQuietly(guildID string) {
	if err := s.RefreshEventTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh table for %s: %v", guildID, err)
	}
}
