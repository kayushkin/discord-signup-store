package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
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

// handleTableAction routes the panel's own controls.
//
// The panel has one: a select that picks an event to read. A Components V2
// Section takes exactly one accessory, so Join is the only button that fits
// beside a row and everything else has to come from a menu underneath.
func (s *Server) handleTableAction(w http.ResponseWriter, in *Interaction, action string) {
	if strings.HasSuffix(action, "-select") {
		s.handleTableSelect(w, in, strings.TrimSuffix(action, "-select"))
		return
	}
	s.replyEphemeral(w, "Unknown table action.")
}

// handleTableSelect acts on the event chosen from a menu under the table.
//
// The event id travels in the select's chosen value rather than its custom_id,
// because one menu covers every event on the table.
func (s *Server) handleTableSelect(w http.ResponseWriter, in *Interaction, want string) {
	if len(in.Data.Values) == 0 {
		s.replyEphemeral(w, "Nothing was selected.")
		return
	}
	eventID, err := strconv.ParseInt(in.Data.Values[0], 10, 64)
	if err != nil {
		s.replyEphemeral(w, "That row does not point at an event any more.")
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		// The table is a snapshot. An event deleted since it was drawn is a
		// stale row, not an error, and saying so tells them what to do.
		s.replyEphemeral(w, "That event is gone — press Rebuild to redraw the table.")
		return
	}

	switch want {
	case "table-details":
		s.handleDetailsButton(w, eventID)
	default:
		s.replyEphemeral(w, "Unknown menu.")
	}
	_ = ev
}

func mustActorID(in *Interaction) string {
	id, _ := in.actor()
	return id
}

// Components V2 lets one message hold a button beside every row: a Section
// carries text plus a single accessory, and Sections do not count against the
// five-action-row limit that caps an ordinary message.
//
// What they do count against is a total of 40 components per message, and
// everything nested counts — the container, each section, the text inside each
// section, every separator, and every option in every select menu. Measured
// against the live API rather than inferred: with separators and one menu, nine
// events fit; without separators, eleven; with neither, eighteen.
//
// So the panel degrades in that order rather than being rejected: separators
// go first because they are decoration, then the menu, then events are dropped
// and the message says how many.
const componentTypeStringSelect = 3

const (
	componentTypeSection   = 9
	componentTypeSeparator = 14
	componentTypeContainer = 17

	// messageComponentBudget is Discord's total, measured. Kept a little under
	// 40 so a description that grows by a component later does not silently
	// start rejecting the whole message.
	messageComponentBudget = 38

	// panelAccentColour is the bar down the left of the container.
	panelAccentColour = 0x5865F2
)

const tableDetailsSelectID = customIDPrefix + ":table-details-select:0"

// panelLayout is what fitted inside the budget.
type panelLayout struct {
	Events     []Event
	Separators bool
	Menu       bool
	Dropped    int
}

// planPanel decides what fits, giving up decoration before information.
//
// The order is spelled out rather than nested loops, because nesting them puts
// the priority in the loop order where it is easy to get backwards — and it was
// backwards, dropping the menu to keep the separators. Separators are lines;
// the menu is the only way to read an event's description and roster.
func planPanel(events []Event) panelLayout {
	for _, attempt := range []panelLayout{
		{Separators: true, Menu: true},   // everything
		{Separators: false, Menu: true},  // lose the lines, keep the menu
		{Separators: false, Menu: false}, // lose the menu too
	} {
		if panelComponentCount(len(events), attempt.Separators, attempt.Menu) <= messageComponentBudget {
			attempt.Events = events
			return attempt
		}
	}
	// Nothing fits whole, so drop events — the last resort, and said out loud.
	for n := len(events) - 1; n > 0; n-- {
		if panelComponentCount(n, false, false) <= messageComponentBudget {
			return panelLayout{Events: events[:n], Dropped: len(events) - n}
		}
	}
	return panelLayout{Events: nil, Dropped: len(events)}
}

// panelComponentCount mirrors Discord's own accounting.
func panelComponentCount(events int, separators, menu bool) int {
	count := 1 + 1 // the container and its heading
	if separators {
		count++ // the rule under the heading
		if events > 1 {
			count += events - 1 // one between each pair
		}
	}
	count += events * 2 // each section, and the text inside it
	if menu {
		count += 2 + events // the action row, the select, and one option each
	}
	return count
}

// RenderEventPanel draws every live event as one message.
//
// Redrawn whole on every change, which is why it needs no rebuild: a single
// message is always in whatever order it was written in, so date order is free.
// The row-per-message version could not sort itself because Discord will not
// reorder messages.
func RenderEventPanel(events []Event) map[string]any {
	sort.SliceStable(events, func(i, j int) bool { return events[i].StartsAt < events[j].StartsAt })
	layout := planPanel(events)

	text := func(content string) map[string]any {
		return map[string]any{"type": componentTypeTextDisplay, "content": trimTo(content, textDisplayLimit)}
	}
	separator := func(spacing int) map[string]any {
		return map[string]any{"type": componentTypeSeparator, "divider": true, "spacing": spacing}
	}

	var heading strings.Builder
	heading.WriteString("## Events\n")
	switch {
	case len(events) == 0:
		heading.WriteString("-# Nothing coming up.")
	case layout.Dropped > 0:
		fmt.Fprintf(&heading, "-# Showing %d of %d. Discord allows 40 components in a message "+
			"and each row costs two — the rest are on the web page.",
			len(layout.Events), len(events))
	default:
		fmt.Fprintf(&heading, "-# %s, soonest first.", pluralise(len(events), "event"))
	}

	panel := []any{text(heading.String())}
	if layout.Separators && len(layout.Events) > 0 {
		panel = append(panel, separator(2))
	}
	for i := range layout.Events {
		panel = append(panel, panelSection(&layout.Events[i]))
		if layout.Separators && i < len(layout.Events)-1 {
			panel = append(panel, separator(1))
		}
	}

	components := []any{map[string]any{
		"type": componentTypeContainer, "accent_color": panelAccentColour, "components": panel,
	}}
	if layout.Menu && len(layout.Events) > 0 {
		options := make([]any, 0, len(layout.Events))
		for i := range layout.Events {
			ev := &layout.Events[i]
			options = append(options, map[string]any{
				"label":       truncate(fmt.Sprintf("%d. %s", i+1, ev.Name), 100),
				"value":       strconv.FormatInt(ev.ID, 10),
				"description": truncate(panelWhen(ev), 100),
			})
		}
		components = append(components, map[string]any{
			"type": componentTypeActionRow,
			"components": []any{map[string]any{
				"type": componentTypeStringSelect, "custom_id": tableDetailsSelectID,
				"placeholder": "Details — description, who is going, the waitlist",
				"min_values":  1, "max_values": 1, "options": options,
			}},
		})
	}

	return map[string]any{
		// Components V2. The flag is required, and with it set the message must
		// carry no content field at all — every word lives in a component.
		"flags":            messageFlagComponentsV2,
		"components":       components,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// messageFlagComponentsV2 opts a message into the component system that has
// sections, containers and separators in it.
const messageFlagComponentsV2 = 1 << 15

// panelSection is one event: a name, when and where, the count, and Join.
//
// A Section takes exactly ONE accessory, so Join is the only button that fits
// beside a row. Everything else is the menu underneath, which is the whole
// trade this layout makes.
func panelSection(ev *Event) map[string]any {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n", ev.Name)
	b.WriteString("-# " + panelWhen(ev))
	if ev.Location != "" {
		b.WriteString("  ·  " + ev.Location)
	}
	b.WriteString("\n")
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "**%d/%d** taken", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "**%d** signed up · no limit", ev.AttendingCount)
	}
	if ev.WaitlistCount > 0 {
		fmt.Fprintf(&b, " · %s waiting", pluralise(ev.WaitlistCount, "person"))
	}
	if ev.Status != StatusOpen {
		fmt.Fprintf(&b, " · **signups %s**", ev.Status)
	}

	section := map[string]any{
		"type":       componentTypeSection,
		"components": []any{map[string]any{"type": componentTypeTextDisplay, "content": b.String()}},
	}
	// A closed event gets Details instead of Join: the accessory is required,
	// and a Join button on something not taking signups is a trap.
	if ev.Status == StatusOpen {
		section["accessory"] = map[string]any{
			"type": componentTypeButton, "style": buttonStylePrimary,
			"label": "Join", "custom_id": JoinCustomID(ev.ID),
		}
	} else {
		section["accessory"] = map[string]any{
			"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID),
		}
	}
	return section
}

// panelWhen renders the start time using Discord's own markup, so it lands in
// each reader's timezone rather than the host's.
func panelWhen(ev *Event) string {
	if ev.StartsAt == 0 {
		return "no date set"
	}
	return fmt.Sprintf("<t:%d:f>", ev.StartsAt)
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

// RefreshEventPanel rewrites a guild's panel in place, posting it the first
// time. Edited rather than reposted so it keeps its position in the channel.
func (s *Server) RefreshEventPanel(guildID string) error {
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
	live, err := s.liveEventsFor(guildID)
	if err != nil {
		return err
	}
	payload := RenderEventPanel(live)

	if table.MessageID == "" {
		messageID, err := s.discord.CreateMessage(table.ChannelID, payload)
		if err != nil {
			return fmt.Errorf("post event panel: %w", err)
		}
		return s.store.SetGuildTableMessage(guildID, messageID)
	}
	if err := s.discord.EditMessage(table.ChannelID, table.MessageID, payload); err != nil {
		return fmt.Errorf("edit event panel: %w", err)
	}
	return nil
}

// refreshEventPanelQuietly updates the panel as a side effect of something that
// already succeeded, so a failure is logged rather than propagated.
func (s *Server) refreshEventPanelQuietly(guildID string) {
	if err := s.RefreshEventPanel(guildID); err != nil {
		log.Printf("[discord-signup] refresh event panel for %s: %v", guildID, err)
	}
}

// componentTypeTextDisplay is read-only text. Discord allows it in a modal, so
// the details view needs no text inputs at all — which matters because there is
// no read-only text input, and prefilled boxes look editable and are not.
//
// A modal made ENTIRELY of these is valid, with no interactive component in it
// at all. Discord's documentation says a modal takes "between 1 and 5
// components" and lists Text Display among the valid ones, but does not say
// whether one can be text-only — so this was settled by opening one, on
// 2026-08-23, and it opens. Recorded because the obvious defensive move is to
// pad the modal with a throwaway input, and that would put back exactly the
// editable-looking box this replaced.
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
