package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Discord's component ceilings, which decide the whole shape of this feature.
//
// A message carries at most five action rows. A row holds either up to five
// buttons OR exactly one select menu — never both. So "a Join button on every
// row of a table" tops out at about twelve events and produces a grid of
// buttons that lines up with nothing.
//
// Four selects and a button is the arrangement that fits: the table itself is
// text, and each select is one column of actions over it.
const (
	componentTypeStringSelect = 3

	// maxSelectOptions is Discord's limit per menu, and therefore the number of
	// events one table can act on. Anything past it is listed in the text and
	// said to be unselectable, rather than silently dropped.
	maxSelectOptions = 25

	// selectOptionLabelLimit and selectOptionDescriptionLimit are Discord's
	// per-option caps. Exceeding either is a 400 on the whole message.
	selectOptionLabelLimit       = 100
	selectOptionDescriptionLimit = 100
)

// GuildTable records where a guild's consolidated table lives.
type GuildTable struct {
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	UpdatedAt int64  `json:"updated_at"`
}

// SetGuildTable records the channel a guild's table lives in, keeping any
// message id already there.
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

// SetGuildTableMessage records which message to rewrite from now on.
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

// GuildsWithTables lists every guild that has one, for the refresh path.
func (s *Store) GuildsWithTables() ([]GuildTable, error) {
	rows, err := s.db.Query(
		`SELECT guild_id, channel_id, message_id, updated_at FROM guild_tables`)
	if err != nil {
		return nil, fmt.Errorf("list guild tables: %w", err)
	}
	defer rows.Close()
	var out []GuildTable
	for rows.Next() {
		var t GuildTable
		if err := rows.Scan(&t.GuildID, &t.ChannelID, &t.MessageID, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan guild table: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Table select custom_ids. The event id travels in the chosen value rather than
// the custom_id, because one menu covers every event.
const (
	tableJoinSelectID    = customIDPrefix + ":table-join:0"
	tableLeaveSelectID   = customIDPrefix + ":table-leave:0"
	tableDetailsSelectID = customIDPrefix + ":table-details:0"
	tableEditSelectID    = customIDPrefix + ":table-edit:0"
	tableRefreshButtonID = customIDPrefix + ":table-refresh:0"
)

// RenderEventTable builds the consolidated message.
//
// The table is a code block, which is the only way Discord renders anything in
// a fixed-width font. Without it the columns wander with every proportional
// glyph and the thing stops being a table.
func RenderEventTable(events []Event) map[string]any {
	var b strings.Builder
	b.WriteString("# Events\n")

	if len(events) == 0 {
		b.WriteString("_Nothing coming up._")
		return map[string]any{
			"content":          b.String(),
			"allowed_mentions": map[string]any{"parse": []string{}},
			"components":       []any{},
		}
	}

	shown := events
	if len(shown) > maxSelectOptions {
		shown = shown[:maxSelectOptions]
	}

	// Widths from the data, so a column is never wider than it needs to be and
	// the whole thing has the best chance of fitting the 2000-character cap.
	nameWidth := 4
	for _, ev := range shown {
		if w := utf8.RuneCountInString(ev.Name); w > nameWidth {
			nameWidth = w
		}
	}
	if nameWidth > 28 {
		nameWidth = 28
	}

	b.WriteString("```\n")
	fmt.Fprintf(&b, "%-3s %-*s %-16s %-9s %s\n", "#", nameWidth, "EVENT", "WHEN", "TAKEN", "WAITING")
	b.WriteString(strings.Repeat("─", 3+1+nameWidth+1+16+1+9+1+7) + "\n")
	for i, ev := range shown {
		taken := fmt.Sprintf("%d", ev.AttendingCount)
		if ev.Capacity > 0 {
			taken = fmt.Sprintf("%d/%d", ev.AttendingCount, ev.Capacity)
		} else {
			taken += " (∞)"
		}
		waiting := "-"
		if ev.WaitlistCount > 0 {
			waiting = strconv.Itoa(ev.WaitlistCount)
		}
		fmt.Fprintf(&b, "%-3d %-*s %-16s %-9s %s\n",
			i+1, nameWidth, padRunes(ev.Name, nameWidth), shortWhen(ev), taken, waiting)
	}
	b.WriteString("```\n")

	if len(events) > len(shown) {
		fmt.Fprintf(&b, "_Showing the first %d of %d. Discord allows 25 options in a menu, "+
			"so the rest are not selectable here — they are on the web page._\n",
			len(shown), len(events))
	}
	b.WriteString("Pick from a menu below. **Details** shows the full description and who is going.")

	content := b.String()
	if len(content) > discordMessageContentLimit {
		content = content[:discordMessageContentLimit-3] + "```"
	}
	return map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       eventTableComponents(shown),
	}
}

// shortWhen renders a date compactly. Deliberately NOT Discord's <t:> markup:
// that expands to a long localised string at render time, which would blow the
// column alignment apart inside a code block.
func shortWhen(ev Event) string {
	if ev.StartsAt == 0 {
		return "no date"
	}
	zone := ev.Timezone
	if zone == "" {
		zone = "UTC"
	}
	return FormatEventTime(ev.StartsAt, zone)
}

// padRunes pads or trims to a width counted in runes, not bytes. A name with an
// emoji in it is fewer runes than bytes, and padding by bytes would leave the
// column short by exactly the difference.
func padRunes(s string, width int) string {
	runes := []rune(s)
	if len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return s
}

func eventTableComponents(events []Event) []any {
	var joinable, leavable, editable, describable []any
	for i, ev := range events {
		value := strconv.FormatInt(ev.ID, 10)
		label := truncate(fmt.Sprintf("%d. %s", i+1, ev.Name), selectOptionLabelLimit)
		describable = append(describable, map[string]any{
			"label": label, "value": value,
			"description": truncate(shortWhen(ev), selectOptionDescriptionLimit),
		})
		editable = append(editable, map[string]any{"label": label, "value": value})
		if ev.Status != StatusOpen {
			continue
		}
		state := "open"
		if ev.Capacity > 0 && ev.AttendingCount >= ev.Capacity {
			state = "full — you would join the waitlist"
		}
		joinable = append(joinable, map[string]any{
			"label": label, "value": value,
			"description": truncate(state, selectOptionDescriptionLimit),
		})
		leavable = append(leavable, map[string]any{"label": label, "value": value})
	}

	selectRow := func(customID, placeholder string, options []any) any {
		// A select with no options is a 400, not an empty menu, so a menu with
		// nothing to offer is left out entirely.
		if len(options) == 0 {
			return nil
		}
		return map[string]any{
			"type": componentTypeActionRow,
			"components": []any{map[string]any{
				"type": componentTypeStringSelect, "custom_id": customID,
				"placeholder": placeholder, "min_values": 1, "max_values": 1,
				"options": options,
			}},
		}
	}
	rows := []any{}
	for _, row := range []any{
		selectRow(tableJoinSelectID, "Join an event", joinable),
		selectRow(tableLeaveSelectID, "Leave an event", leavable),
		selectRow(tableDetailsSelectID, "Details — description and who is going", describable),
		selectRow(tableEditSelectID, "Edit an event", editable),
	} {
		if row != nil {
			rows = append(rows, row)
		}
	}
	// The fifth row, and only if there is space. Buttons and selects cannot
	// share a row, so this costs a whole one.
	if len(rows) < 5 {
		rows = append(rows, map[string]any{
			"type": componentTypeActionRow,
			"components": []any{map[string]any{
				"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Refresh", "custom_id": tableRefreshButtonID,
			}},
		})
	}
	return rows
}

// RefreshEventTable rewrites a guild's table in place, posting it first if it
// has never been posted.
//
// Rewritten rather than reposted: a table that reposted itself every time
// somebody joined would walk down the channel and lose its place in everyone's
// client. Editing keeps it exactly where people scrolled to.
func (s *Server) RefreshEventTable(guildID string) error {
	if s.discord == nil {
		return nil
	}
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) {
		return nil // no table configured for this guild
	}
	if err != nil {
		return err
	}
	events, err := s.store.ListEvents(guildID, "", 200)
	if err != nil {
		return err
	}
	live, _ := splitByArchived(events)
	payload := RenderEventTable(live)

	if table.MessageID == "" {
		messageID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post event table: %w", err)
		}
		return s.store.SetGuildTableMessage(guildID, messageID)
	}
	if err := s.discord.EditMessage(table.ChannelID, table.MessageID, payload); err != nil {
		return fmt.Errorf("edit event table: %w", err)
	}
	return nil
}

// RefreshEventTablesFor rewrites the table for one guild, swallowing nothing
// but logging rather than propagating — every caller is a side effect of some
// other action that has already succeeded.
func (s *Server) refreshEventTableQuietly(guildID string) {
	if err := s.RefreshEventTable(guildID); err != nil {
		log.Printf("[discord-signup] refresh event table for %s: %v", guildID, err)
	}
}

// handleTableAction routes a select or button on the consolidated table.
func (s *Server) handleTableAction(w http.ResponseWriter, in *Interaction, action string) {
	if action == "table-refresh" {
		s.refreshEventTableQuietly(in.GuildID)
		s.replyEphemeral(w, "Refreshed.")
		return
	}
	if len(in.Data.Values) == 0 {
		s.replyEphemeral(w, "Nothing was selected.")
		return
	}
	eventID, err := strconv.ParseInt(in.Data.Values[0], 10, 64)
	if err != nil {
		s.replyEphemeral(w, "That row does not point at an event any more — press Refresh.")
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		// The table is a snapshot. An event deleted since it was drawn is not
		// an error, it is a stale row, and saying so tells them what to do.
		s.replyEphemeral(w, "That event is gone — press Refresh to redraw the table.")
		return
	}

	switch action {
	case "table-join":
		s.handleJoin(w, in, eventID, mustActorID(in), mustActorName(in))
	case "table-leave":
		s.handleLeave(w, in, eventID, mustActorID(in))
	case "table-details":
		s.replyTableDetails(w, ev)
	case "table-edit":
		if ok, why := s.mayEdit(in, ev); !ok {
			s.replyEphemeral(w, why)
			return
		}
		zone := ev.Timezone
		if zone == "" {
			zone = s.DefaultTimezone()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": callbackTypeModal,
			"data": buildEventModal(EditModalCustomID(eventID), "Edit "+ev.Name, ev, zone),
		})
		return
	default:
		s.replyEphemeral(w, "Unknown table action.")
		return
	}
	// Join and leave already refresh the event's own card; the table is a
	// second view of the same fact and has to follow.
	go s.refreshEventTableQuietly(in.GuildID)
}

// replyTableDetails is the dropdown the table exists to make possible: the
// description and the roster, which will not fit in a row.
func (s *Server) replyTableDetails(w http.ResponseWriter, ev *Event) {
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster for details of %d: %v", ev.ID, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", ev.Name)
	if ev.Description != "" {
		b.WriteString(ev.Description + "\n")
	}
	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, "\n🗓️ <t:%d:F>", ev.StartsAt)
	}
	if ev.Location != "" {
		b.WriteString(" · " + ev.Location)
	}
	b.WriteString("\n")
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "\n**%d/%d taken**", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "\n**%d signed up** — no limit", ev.AttendingCount)
	}
	if ev.WaitlistCount > 0 {
		fmt.Fprintf(&b, " · %d waiting", ev.WaitlistCount)
	}
	b.WriteString("\n")

	attending, waiting := splitRoster(roster)
	if len(attending) > 0 {
		b.WriteString("\n**Going**\n")
		writeMentions(&b, attending)
	}
	if len(waiting) > 0 {
		b.WriteString("\n**Waitlist**\n")
		writeMentions(&b, waiting)
	}
	if ev.DiscordScheduledEventID != "" {
		fmt.Fprintf(&b, "\n[Discord's own event](%s) shows %d interested — a different "+
			"number, and always will be. This list is the roster.",
			DiscordEventURL(ev.GuildID, ev.DiscordScheduledEventID), ev.DiscordInterestedCount)
	}

	content := b.String()
	if len(content) > discordMessageContentLimit {
		content = content[:discordMessageContentLimit-1] + "…"
	}
	s.replyEphemeral(w, content)
}

func mustActorID(in *Interaction) string {
	id, _ := in.actor()
	return id
}

func mustActorName(in *Interaction) string {
	_, name := in.actor()
	return name
}
