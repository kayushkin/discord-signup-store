package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
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

// TableRowMessageID reports which message holds an event's row, or "" if it has
// none yet.
func (s *Store) TableRowMessageID(eventID int64) (string, error) {
	var messageID string
	err := s.db.QueryRow(
		`SELECT message_id FROM event_table_rows WHERE event_id = ?`, eventID).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read table row: %w", err)
	}
	return messageID, nil
}

// SetTableRowMessageID records it.
func (s *Store) SetTableRowMessageID(eventID int64, guildID, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO event_table_rows (event_id, guild_id, message_id, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(event_id) DO UPDATE SET message_id = excluded.message_id,
		                                    updated_at = excluded.updated_at`,
		eventID, guildID, messageID, now())
	if err != nil {
		return fmt.Errorf("set table row: %w", err)
	}
	return nil
}

// DeleteTableRow forgets an event's row.
func (s *Store) DeleteTableRow(eventID int64) error {
	_, err := s.db.Exec(`DELETE FROM event_table_rows WHERE event_id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("delete table row: %w", err)
	}
	return nil
}

// TableRowsFor lists every row message in a guild, for a rebuild to clear.
func (s *Store) TableRowsFor(guildID string) (map[int64]string, error) {
	rows, err := s.db.Query(
		`SELECT event_id, message_id FROM event_table_rows WHERE guild_id = ?`, guildID)
	if err != nil {
		return nil, fmt.Errorf("list table rows: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var messageID string
		if err := rows.Scan(&id, &messageID); err != nil {
			return nil, fmt.Errorf("scan table row: %w", err)
		}
		out[id] = messageID
	}
	return out, rows.Err()
}

const tableRebuildButtonID = customIDPrefix + ":table-rebuild:0"

// RenderTableHeader is the one message above the rows.
func RenderTableHeader(count int) map[string]any {
	var b strings.Builder
	b.WriteString("# Events\n")
	switch count {
	case 0:
		b.WriteString("_Nothing coming up._")
	case 1:
		b.WriteString("**1 event.** Join, leave or read the details from the buttons on its row.")
	default:
		fmt.Fprintf(&b, "**%d events**, soonest first. Join, leave or read the details "+
			"from the buttons on each row.", count)
	}
	b.WriteString("\n-# Rows appear in the order they were added. Rebuild sorts them by date.")
	return map[string]any{
		"content":          b.String(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components": []any{map[string]any{
			"type": componentTypeActionRow,
			"components": []any{map[string]any{
				"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Rebuild", "custom_id": tableRebuildButtonID,
			}},
		}},
	}
}

// RenderTableRow is one event as one line plus its buttons.
//
// The count leads, in a code span so it is monospaced and the eye can run down
// the column even though Discord will not align anything across messages. The
// time uses Discord's own <t:…> markup, which renders in each reader's timezone
// — the same reason the web page defers formatting to the browser.
func RenderTableRow(ev *Event) map[string]any {
	var b strings.Builder

	count := fmt.Sprintf("%d", ev.AttendingCount)
	if ev.Capacity > 0 {
		count = fmt.Sprintf("%d/%d", ev.AttendingCount, ev.Capacity)
	} else {
		count += "/∞"
	}
	fmt.Fprintf(&b, "`%-7s` **%s**", count, ev.Name)

	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, " · <t:%d:f>", ev.StartsAt)
	}
	if ev.Location != "" {
		b.WriteString(" · " + ev.Location)
	}
	if ev.WaitlistCount > 0 {
		fmt.Fprintf(&b, " · %d waiting", ev.WaitlistCount)
	}
	if ev.Status != StatusOpen {
		fmt.Fprintf(&b, " · **%s**", ev.Status)
	}

	return map[string]any{
		"content":          b.String(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       tableRowComponents(ev),
	}
}

// tableRowComponents are the same buttons the full card carries, plus Details.
//
// Deliberately the same custom_ids: a row and a card are two views of one
// event, and giving them separate ids would mean two handlers that have to be
// kept in step. Only Details is new, because a description does not fit on a
// line and belongs in an ephemeral reply anyway.
func tableRowComponents(ev *Event) []any {
	buttons := []any{}
	if ev.Status == StatusOpen {
		buttons = append(buttons,
			map[string]any{
				"type": componentTypeButton, "style": buttonStylePrimary,
				"label": "Join", "custom_id": JoinCustomID(ev.ID),
			},
			map[string]any{
				"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Leave", "custom_id": LeaveCustomID(ev.ID),
			})
	}
	buttons = append(buttons,
		map[string]any{
			"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID),
		},
		map[string]any{
			"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Edit", "custom_id": EditCustomID(ev.ID),
		})
	return []any{map[string]any{"type": componentTypeActionRow, "components": buttons}}
}

// liveEventsFor returns a guild's current events, soonest first.
func (s *Server) liveEventsFor(guildID string) ([]Event, error) {
	events, err := s.store.ListEvents(guildID, "", 200)
	if err != nil {
		return nil, err
	}
	live, _ := splitByArchived(events)
	return live, nil
}

// RefreshTableRow rewrites one event's row, posting it if it has none.
//
// Edited rather than reposted so it keeps its place in the channel. This is the
// hot path — every signup runs it — and it touches exactly one message.
func (s *Server) RefreshTableRow(ev *Event) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(ev.GuildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	messageID, err := s.store.TableRowMessageID(ev.ID)
	if err != nil {
		return err
	}
	payload := RenderTableRow(ev)

	if messageID == "" {
		newID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post table row: %w", err)
		}
		if err := s.store.SetTableRowMessageID(ev.ID, ev.GuildID, newID); err != nil {
			return err
		}
		return s.refreshTableHeader(table)
	}
	if err := s.discord.EditMessage(table.ChannelID, messageID, payload); err != nil {
		return fmt.Errorf("edit table row: %w", err)
	}
	return nil
}

// RemoveTableRow takes an event out of the table, for one that has finished.
func (s *Server) RemoveTableRow(ev *Event) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(ev.GuildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	messageID, err := s.store.TableRowMessageID(ev.ID)
	if err != nil || messageID == "" {
		return err
	}
	if err := s.discord.DeleteMessage(table.ChannelID, messageID); err != nil {
		log.Printf("[discord-signup] could not delete table row for event %d: %v", ev.ID, err)
	}
	if err := s.store.DeleteTableRow(ev.ID); err != nil {
		return err
	}
	return s.refreshTableHeader(table)
}

func (s *Server) refreshTableHeader(table *GuildTable) error {
	live, err := s.liveEventsFor(table.GuildID)
	if err != nil {
		return err
	}
	payload := RenderTableHeader(len(live))
	if table.MessageID == "" {
		newID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post table header: %w", err)
		}
		return s.store.SetGuildTableMessage(table.GuildID, newID)
	}
	if err := s.discord.EditMessage(table.ChannelID, table.MessageID, payload); err != nil {
		return fmt.Errorf("edit table header: %w", err)
	}
	return nil
}

// RebuildEventTable deletes every message and reposts them in date order.
//
// The only way to sort the table: Discord orders messages by when they were
// posted and offers no way to move one. Expensive and visibly noisy, so it is
// a button somebody presses rather than something that happens on its own.
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
	existing, err := s.store.TableRowsFor(guildID)
	if err != nil {
		return err
	}
	for eventID, messageID := range existing {
		if err := s.discord.DeleteMessage(table.ChannelID, messageID); err != nil {
			log.Printf("[discord-signup] rebuild: could not delete row for %d: %v", eventID, err)
		}
		if err := s.store.DeleteTableRow(eventID); err != nil {
			return err
		}
	}
	if table.MessageID != "" {
		if err := s.discord.DeleteMessage(table.ChannelID, table.MessageID); err != nil {
			log.Printf("[discord-signup] rebuild: could not delete header: %v", err)
		}
		if err := s.store.SetGuildTableMessage(guildID, ""); err != nil {
			return err
		}
		table.MessageID = ""
	}

	live, err := s.liveEventsFor(guildID)
	if err != nil {
		return err
	}
	// Header first so it sits above the rows.
	if err := s.refreshTableHeader(table); err != nil {
		return err
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].StartsAt < live[j].StartsAt })
	for i := range live {
		if err := s.RefreshTableRow(&live[i]); err != nil {
			log.Printf("[discord-signup] rebuild: row for %d: %v", live[i].ID, err)
		}
	}
	return nil
}

// refreshTableRowQuietly updates one row as a side effect of something that has
// already succeeded, so a failure is logged rather than propagated.
func (s *Server) refreshTableRowQuietly(ev *Event) {
	if err := s.RefreshTableRow(ev); err != nil {
		log.Printf("[discord-signup] refresh table row for event %d: %v", ev.ID, err)
	}
}

// handleTableAction routes the header's Rebuild button.
func (s *Server) handleTableAction(w http.ResponseWriter, in *Interaction, action string) {
	if action != "table-rebuild" {
		s.replyEphemeral(w, "Unknown table action.")
		return
	}
	if !in.canManageEvents() {
		s.replyEphemeral(w, "Rebuilding deletes and reposts every row, so it needs Manage Events.")
		return
	}
	guildID := in.GuildID
	// Answered first: a rebuild deletes and reposts every message and will not
	// finish inside Discord's three-second window.
	s.replyEphemeral(w, "Rebuilding the table in date order — it will settle in a moment.")
	go func() {
		if err := s.RebuildEventTable(guildID); err != nil {
			log.Printf("[discord-signup] rebuild table for %s: %v", guildID, err)
		}
	}()
}

// componentTypeTextDisplay is read-only text. Discord allows it in a modal, so
// the details view needs no text inputs at all — which matters because there is
// no read-only text input, and prefilled boxes look editable and are not.
const componentTypeTextDisplay = 10

// textDisplayLimit is the cap on one block's content.
const textDisplayLimit = 4000

// buildDetailsModal shows an event as three blocks of plain text: what it is,
// who is going, and who is waiting — in that order, because that is the order
// the questions get asked.
//
// Every component is a Text Display. Nothing here is an input, so nothing looks
// editable, and there is no submit to explain away.
func buildDetailsModal(ev *Event, roster []Signup) map[string]any {
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
		going += "\n" + rosterNames(attending)
	}
	components = append(components, text(going))

	// Only when there is one: a permanently empty heading reads as a fault.
	if len(waiting) > 0 {
		components = append(components, text(fmt.Sprintf("**Waitlist — %d**\n%s",
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
func (s *Server) handleDetailsButton(w http.ResponseWriter, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster for details of %d: %v", ev.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": buildDetailsModal(ev, roster),
	})
}
