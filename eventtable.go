package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The consolidated table is one Discord message per event, each holding one
// compact line and that event's buttons.
//
// One message per event rather than one message for the whole table, because a
// message carries at most five action rows and a button row holds five buttons.
// Everything in a single message therefore tops out around twelve events, and
// the buttons line up with nothing — they sit in a block underneath rather than
// beside the row they act on.
//
// Split across messages, each row gets its own five-button row, so the buttons
// are next to the event they belong to and there is no ceiling on how many
// events the channel can hold. It also makes updates cheap: a signup rewrites
// one line, not the whole table.
//
// What Discord will not do is reorder messages. Rows appear in the order they
// were posted, so a newly created event that starts sooner than existing ones
// lands at the bottom. Rebuild reposts everything in date order; refresh edits
// in place and keeps positions.

// GuildTable records where a guild's consolidated table lives.
type GuildTable struct {
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"` // the header
	UpdatedAt int64  `json:"updated_at"`
}

// SetGuildTable records the channel a guild's table lives in.
func (s *Store) SetGuildTable(guildID, channelID string) error {
	_, err := s.db.Exec(`
		INSERT INTO guild_tables (guild_id, channel_id, message_id, updated_at)
		VALUES (?,?,'',?)
		ON CONFLICT(guild_id) DO UPDATE SET channel_id = excluded.channel_id,
		                                    updated_at = excluded.updated_at`,
		guildID, channelID, now())
	if err != nil {
		return fmt.Errorf("set guild table: %w", err)
	}
	return nil
}

// SetGuildTableMessage records the header message.
func (s *Store) SetGuildTableMessage(guildID, messageID string) error {
	_, err := s.db.Exec(
		`UPDATE guild_tables SET message_id = ?, updated_at = ? WHERE guild_id = ?`,
		messageID, now(), guildID)
	if err != nil {
		return fmt.Errorf("set guild table message: %w", err)
	}
	return nil
}

// GuildTable reads where a guild's table lives.
func (s *Store) GuildTable(guildID string) (*GuildTable, error) {
	var t GuildTable
	err := s.db.QueryRow(
		`SELECT guild_id, channel_id, message_id, updated_at FROM guild_tables WHERE guild_id = ?`,
		guildID).Scan(&t.GuildID, &t.ChannelID, &t.MessageID, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read guild table: %w", err)
	}
	return &t, nil
}

// TablePage is one message of the consolidated table.
type TablePage struct {
	Page      int
	MessageID string
}

// TablePages lists a guild's pages in order.
func (s *Store) TablePages(guildID string) ([]TablePage, error) {
	rows, err := s.db.Query(
		`SELECT page, message_id FROM table_pages WHERE guild_id = ? ORDER BY page ASC`, guildID)
	if err != nil {
		return nil, fmt.Errorf("list table pages: %w", err)
	}
	defer rows.Close()
	var out []TablePage
	for rows.Next() {
		var p TablePage
		if err := rows.Scan(&p.Page, &p.MessageID); err != nil {
			return nil, fmt.Errorf("scan table page: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetTablePage records the message holding one page.
func (s *Store) SetTablePage(guildID string, page int, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO table_pages (guild_id, page, message_id, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(guild_id, page) DO UPDATE SET message_id = excluded.message_id,
		                                          updated_at = excluded.updated_at`,
		guildID, page, messageID, now())
	if err != nil {
		return fmt.Errorf("set table page: %w", err)
	}
	return nil
}

// DeleteTablePage forgets a page that is no longer needed.
func (s *Store) DeleteTablePage(guildID string, page int) error {
	_, err := s.db.Exec(`DELETE FROM table_pages WHERE guild_id = ? AND page = ?`, guildID, page)
	if err != nil {
		return fmt.Errorf("delete table page: %w", err)
	}
	return nil
}

const componentTypeTextDisplay = 10

// textDisplayLimit is the cap on one block's content.
const textDisplayLimit = 4000

// buildDetailsModal shows an event as three blocks of plain text: what it is,
// who is going, and who is waiting — in that order, because that is the order
// the questions get asked.
//
// Every component is a Text Display. Nothing here is an input, so nothing looks
// editable, and there is no submit to explain away.
// buildDetailsModal deliberately carries NO forum reference, unlike every
// other surface: a modal resolves neither <#…> mentions nor markdown links, so
// the reference would render as dead text — a raw id in angle brackets.
// buildDetailsModal renders an event for somebody looking at it, and for
// somebody about to change it.
//
// An editor gets ONE text block rather than the three a viewer gets. Discord
// documents no total component limit for modals — only messages have the
// documented 40 — so the editor's version, which must carry five text inputs
// whatever else it holds, keeps everything else to a single block instead of
// discovering that limit in production.
func buildDetailsModal(ev *Event, roster []Signup, canEdit bool, zone string) map[string]any {
	if canEdit {
		return buildEditModalWithRoster(ev, roster, zone)
	}
	return buildViewOnlyDetailsModal(ev, roster)
}

func buildViewOnlyDetailsModal(ev *Event, roster []Signup) map[string]any {
	text := func(content string) map[string]any {
		return map[string]any{"type": componentTypeTextDisplay, "content": trimTo(content, textDisplayLimit)}
	}
	components := []any{}

	var head strings.Builder
	if ev.Description != "" {
		head.WriteString(ev.Description)
	} else {
		head.WriteString("_No description._")
	}
	// The when and where go under the description as small text, so the thing
	// asked for first is read first.
	var meta []string
	if ev.StartsAt > 0 {
		zone := ev.Timezone
		if zone == "" {
			zone = "UTC"
		}
		// Spelled out rather than Discord's <t:…> markup, which renders as
		// literal text in a modal instead of localising.
		meta = append(meta, FormatEventTime(ev.StartsAt, zone)+" ("+zone+")")
	}
	if ev.Location != "" {
		meta = append(meta, ev.Location)
	}
	if ev.Status != StatusOpen {
		meta = append(meta, "signups "+ev.Status)
	}
	if len(meta) > 0 {
		head.WriteString("\n-# " + strings.Join(meta, "  ·  "))
	}
	components = append(components, text(head.String()))

	attending, waiting := splitRoster(roster)
	going := fmt.Sprintf("**Going — %d**", len(attending))
	if ev.Capacity > 0 {
		going = fmt.Sprintf("**Going — %d of %d**", len(attending), ev.Capacity)
	}
	if len(attending) == 0 {
		going += "\n-# Nobody yet."
	} else {
		// A BLANK line, not a single newline. "1." at the start of a line is
		// Discord's ordered-list syntax, and a list that follows a paragraph
		// with no blank line between them gets pulled up onto that paragraph's
		// last line — which is why the details view read
		// "**Going — 2 of 7** 1. Domonation" with no break.
		going += "\n\n" + rosterNames(attending)
	}
	components = append(components, text(going))

	// Only when there is one: a permanently empty heading reads as a fault.
	if len(waiting) > 0 {
		components = append(components, text(fmt.Sprintf("**Waitlist — %d**\n\n%s",
			len(waiting), rosterNames(waiting))))
	}

	if ev.DiscordScheduledEventID != "" && len(components) < 5 {
		components = append(components, text(fmt.Sprintf(
			"-# Discord's own event shows **%d interested** — a different number from the "+
				"list above, and always will be. Discord counts people who asked to be "+
				"notified; this counts people who have a place.",
			ev.DiscordInterestedCount)))
	}

	return map[string]any{
		"custom_id":  DetailsModalCustomID(ev.ID),
		"title":      truncate(ev.Name, 45),
		"components": components,
	}
}

// rosterNames lists people one per line, by display name.
//
// Names rather than <@id> mentions: a modal does not resolve a mention, so one
// would show as a raw snowflake in angle brackets. This is the surface the
// display-name backfill exists for.
func rosterNames(signups []Signup) string {
	if len(signups) == 0 {
		return ""
	}
	var b strings.Builder
	for i, sg := range signups {
		name := sg.DisplayName
		if name == "" {
			name = sg.DiscordUserID
		}
		place := i + 1
		if sg.State == StateWaitlisted {
			place = sg.WaitlistPlace
		}
		fmt.Fprintf(&b, "%d. %s\n", place, name)
	}
	return strings.TrimRight(b.String(), "\n")
}

func trimTo(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// handleDetailsButton opens the details modal.
// handleDetailsButton opens the one modal that both shows an event and edits
// it.
//
// Two modals used to do this and the split was arbitrary from where somebody
// was standing: Details said who is going, Edit changed the event, and anybody
// allowed to do the second wanted the first in front of them while they did it.
// Now there is one button. What it opens depends on whether the person pressing
// may edit — Discord renders a component to everybody or nobody, so the check
// has to happen on the press, and this is the press.
//
// Buttons cannot go in a modal at all (Discord lists Button as message-only),
// which is why this merges the two modals rather than putting Edit inside
// Details.
func (s *Server) handleDetailsButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster for details of %d: %v", ev.ID, err)
	}
	canEdit, _ := s.mayEdit(in, ev)
	zone := ev.Timezone
	if zone == "" {
		zone = s.DefaultTimezone()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": buildDetailsModal(ev, roster, canEdit, zone),
	})
}

// Components V2 constants for the table.
const (
	componentTypeContainer = 17

	// panelAccentColour is the bar down the left of each page.
	panelAccentColour = 0x5865F2

	// messageFlagComponentsV2 opts a message into the component system with
	// containers in it. With the flag set the message must carry NO content
	// field — every word lives in a component.
	messageFlagComponentsV2 = 1 << 15

	componentTypeSeparator = 14

	// eventsPerPage is how many events fit in one message under the measured
	// 40-component budget. Each event costs a text block, an action row and
	// four buttons (6), and each gap between events costs a separator: the
	// container + 5×6 + 4 separators = 35, while six events would need 42.
	//
	// The five-action-row limit that caps an ordinary message does NOT apply
	// here — Components V2 replaces it with the total budget, which is what
	// makes five rows of buttons in one message possible at all.
	eventsPerPage = 5
)

const tableRebuildButtonID = customIDPrefix + ":table-rebuild:0"

// RenderTablePage draws up to eventsPerPage events as one message: one line of
// text and one row of buttons each, wrapped in a container.
//
// firstIndex numbers the rows continuously across pages, so the second message
// starts at 7 rather than beginning again at 1.
//
// The table is about events, and only events. "My events" used to hang off the
// last page, which put a control about the viewer on a message about everybody;
// it lives on the standing how-to message now.
func RenderTablePage(events []Event, page, totalPages int) map[string]any {
	body := []any{}
	// No heading. The channel is called what it is, and a line saying how many
	// events there are and how they are sorted is a component spent restating
	// what the rows already show.
	if len(events) == 0 && page == 0 {
		body = append(body, textBlock("-# Nothing coming up."))
	}

	for i := range events {
		ev := &events[i]
		if i > 0 {
			body = append(body, map[string]any{
				"type": componentTypeSeparator, "divider": true, "spacing": 1,
			})
		}
		body = append(body, textBlock(eventLine(ev)))
		body = append(body, map[string]any{
			"type": componentTypeActionRow, "components": eventButtons(ev),
		})
	}
	if totalPages > 1 {
		body = append(body, textBlock(fmt.Sprintf("-# Page %d of %d", page+1, totalPages)))
	}
	return map[string]any{
		"flags": messageFlagComponentsV2,
		"components": []any{map[string]any{
			"type": componentTypeContainer, "accent_color": panelAccentColour,
			"components": body,
		}},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

func textBlock(content string) map[string]any {
	return map[string]any{"type": componentTypeTextDisplay, "content": trimTo(content, textDisplayLimit)}
}

// eventLine is the whole of an event, in one text component:
//
//	{slots} {title} {location} {time}
//
// One component, not four — each counts against the 40 a message gets, and a
// per-field split would read identically while holding half as much.
//
// An uncapped event shows no count at all. A bare "1" read as nothing — is it a
// count, a limit, a rank? — and the whole line exists to be glanced at.
func eventLine(ev *Event) string {
	var parts []string
	if ev.Capacity > 0 {
		parts = append(parts, fmt.Sprintf("`%d/%d`", ev.AttendingCount, ev.Capacity))
	}
	parts = append(parts, fmt.Sprintf("**%s**", ev.Name))
	if ev.Location != "" {
		parts = append(parts, ev.Location)
	}
	if ev.StartsAt > 0 {
		parts = append(parts, compactWhen(ev))
	}
	if ev.ForumPostID != "" {
		// The same reference the #events card uses: a 💬 and a thread mention.
		// A mention renders the post's full title, which repeats much of this
		// line — the price of every surface referencing the forum identically,
		// chosen deliberately over a terser masked link that looked different
		// everywhere it appeared.
		parts = append(parts, fmt.Sprintf("💬 <#%s>", ev.ForumPostID))
	}
	return strings.Join(parts, "  ·  ")
}

// compactWhen renders "8/29 4pm" — formatted here, deliberately NOT Discord's
// <t:…> markup. That markup localises per reader, which is the right default
// everywhere else, but it expands to "August 29, 2026 4:00 PM" and there is no
// short form Discord offers that is not still a mouthful. A table row is for
// glancing, so it trades the per-reader timezone for eight characters; the
// event's zone is whatever it was scheduled in, and Details still carries the
// localised form for anyone who needs certainty.
func compactWhen(ev *Event) string {
	zone := ev.Timezone
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	t := time.Unix(ev.StartsAt, 0).In(loc)
	clock := strings.ToLower(t.Format("3:04pm"))
	clock = strings.Replace(clock, ":00", "", 1)
	return fmt.Sprintf("%d/%d %s", int(t.Month()), t.Day(), clock)
}

func eventButtons(ev *Event) []any {
	buttons := []any{}
	// A closed event keeps Details and Edit and loses Join and Leave. A button
	// that cannot act is a trap rather than a disabled affordance.
	if ev.Status == StatusOpen {
		buttons = append(buttons,
			map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
				"label": "Join", "custom_id": JoinCustomID(ev.ID)},
			map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Leave", "custom_id": LeaveCustomID(ev.ID)})
	}
	buttons = append(buttons,
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID)},
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Edit", "custom_id": EditCustomID(ev.ID)})
	return buttons
}

// paginate splits events into messages of eventsPerPage.
func paginate(events []Event) [][]Event {
	if len(events) == 0 {
		return [][]Event{nil} // one page, saying there is nothing
	}
	var pages [][]Event
	for start := 0; start < len(events); start += eventsPerPage {
		end := start + eventsPerPage
		if end > len(events) {
			end = len(events)
		}
		pages = append(pages, events[start:end])
	}
	return pages
}

// RefreshEventTable redraws a guild's whole table, editing each page in place.
//
// Every page is rewritten, which is what keeps the table sorted without ever
// reposting: events move BETWEEN pages while the messages stay where they are.
// A page is only ever posted when the table grows past its current page count,
// and a new message goes to the bottom, which is exactly where a new last page
// belongs.
func (s *Server) RefreshEventTable(guildID string) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	events, err := s.liveEventsFor(guildID)
	if err != nil {
		return err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].StartsAt < events[j].StartsAt })
	pages := paginate(events)

	existing, err := s.store.TablePages(guildID)
	if err != nil {
		return err
	}
	byPage := map[int]string{}
	for _, p := range existing {
		byPage[p.Page] = p.MessageID
	}

	for i, chunk := range pages {
		payload := RenderTablePage(chunk, i, len(pages))
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

	// Pages that are no longer needed. Deleted from the end so the remaining
	// ones keep their positions.
	for i := len(existing) - 1; i >= len(pages); i-- {
		if err := s.discord.DeleteMessage(table.ChannelID, existing[i].MessageID); err != nil {
			log.Printf("[discord-signup] could not delete surplus table page %d: %v", i, err)
		}
		if err := s.store.DeleteTablePage(guildID, existing[i].Page); err != nil {
			return err
		}
	}
	return nil
}

// refreshEventTableQuietly redraws the table as a side effect of something that
// already succeeded, so a failure is logged rather than propagated.
func (s *Server) refreshEventTableQuietly(guildID string) {
	if err := s.RefreshEventTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh event table for %s: %v", guildID, err)
	}
}

// liveEventsFor returns a guild's current events, archived ones excluded.
func (s *Server) liveEventsFor(guildID string) ([]Event, error) {
	events, err := s.store.ListEvents(guildID, "", 200)
	if err != nil {
		return nil, err
	}
	live, _ := splitByArchived(events)
	return live, nil
}

// handleTableAction routes the table's own controls.
//
// Only Rebuild, and it is now a repair tool rather than the way to sort:
// pages are rewritten in place on every change, so the table sorts itself.
// Rebuild exists for when the messages and the database disagree — someone
// deleted one by hand, or a post failed midway.
func (s *Server) handleTableAction(w http.ResponseWriter, in *Interaction, action string) {
	if action != "table-rebuild" {
		s.replyEphemeral(w, "Unknown table action.")
		return
	}
	if !in.canManageEvents() {
		s.replyEphemeral(w, "Rebuilding deletes and reposts the table, so it needs Manage Events.")
		return
	}
	guildID := in.GuildID
	// Answered first: deleting and reposting will not finish inside Discord's
	// three-second interaction window.
	s.replyEphemeral(w, "Rebuilding the table — it will settle in a moment.")
	go func() {
		if err := s.RebuildEventTable(guildID); err != nil {
			log.Printf("[discord-signup] rebuild table for %s: %v", guildID, err)
		}
	}()
}

// RebuildEventTable deletes every page and draws the table again.
//
// Not needed for ordering — a redraw already sorts. This is the repair path for
// when the recorded messages and the channel disagree.
func (s *Server) RebuildEventTable(guildID string) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	pages, err := s.store.TablePages(guildID)
	if err != nil {
		return err
	}
	for _, p := range pages {
		if err := s.discord.DeleteMessage(table.ChannelID, p.MessageID); err != nil {
			log.Printf("[discord-signup] rebuild: could not delete page %d: %v", p.Page, err)
		}
		if err := s.store.DeleteTablePage(guildID, p.Page); err != nil {
			return err
		}
	}
	pages, err = s.store.RosterTablePages(guildID)
	if err != nil {
		return err
	}
	for _, p := range pages {
		if err := s.discord.DeleteMessage(table.ChannelID, p.MessageID); err != nil {
			log.Printf("[discord-signup] rebuild: could not delete roster page %d: %v", p.Page, err)
		}
		if err := s.store.DeleteRosterTablePage(guildID, p.Page); err != nil {
			return err
		}
	}
	if err := s.RefreshEventTable(guildID); err != nil {
		return err
	}
	return s.RefreshRosterTable(guildID)
}
