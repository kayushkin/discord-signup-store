package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	// ManagementChannelID is where the management table lives: the same
	// events with Edit on each row and Create on the end. Empty means there
	// is not one.
	ManagementChannelID string `json:"management_channel_id"`
	UpdatedAt           int64  `json:"updated_at"`
}

// SetGuildManagementChannel points the management table at a channel.
func (s *Store) SetGuildManagementChannel(guildID, channelID string) error {
	res, err := s.db.Exec(`UPDATE guild_tables SET management_channel_id = ?, updated_at = ? WHERE guild_id = ?`,
		channelID, now(), guildID)
	if err != nil {
		return fmt.Errorf("set management channel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound // no event table yet; the management one hangs off it
	}
	return nil
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
		`SELECT guild_id, channel_id, message_id, management_channel_id, updated_at FROM guild_tables WHERE guild_id = ?`,
		guildID).Scan(&t.GuildID, &t.ChannelID, &t.MessageID, &t.ManagementChannelID, &t.UpdatedAt)
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

// componentTypeLabel wraps one input in a modal, carrying its wording.
// Discord's reference: "Label is recommended for use over an Action Row in
// modals", and "Action Row with Text Inputs in modals are now deprecated".
const componentTypeLabel = 18

// textDisplayLimit is the cap on one block's content.
const textDisplayLimit = 4000

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
// handleDetailsButton shows who is going. Read-only, for everybody: editing
// lives on the management table, whose rows carry Edit and nothing a member
// does. The two were one modal for an afternoon; splitting them again is a
// choice about where controls live, not about what Discord allows.
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
		"data": buildRosterOnlyModal(ev, roster),
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
	if table.ManagementChannelID != "" {
		mpages, err := s.store.ManagementPages(guildID)
		if err != nil {
			return err
		}
		for _, p := range mpages {
			if err := s.discord.DeleteMessage(table.ManagementChannelID, p.MessageID); err != nil {
				log.Printf("[discord-signup] rebuild: could not delete management page %d: %v", p.Page, err)
			}
			if err := s.store.DeleteManagementPage(guildID, p.Page); err != nil {
				return err
			}
		}
	}
	if err := s.RefreshEventTable(guildID); err != nil {
		return err
	}
	return s.RefreshManagementTable(guildID)
}
