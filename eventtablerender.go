package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
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

// eventHeadlineParts is what an event is, when and where, each ready to
// print: the title in bold, the day and time in the event's own zone with the
// repeat in words, and the place after a pin. When or where is empty when the
// event has none.
func eventHeadlineParts(ev *Event) (title, when, where string) {
	title = "**" + escapeMarkdown(ev.Name) + "**"
	if ev.StartsAt > 0 {
		when = eventStartInItsZone(ev).Format("Mon") + " " + compactWhen(ev)
	}
	if ev.RecurrenceRule != "" {
		when = strings.TrimSpace(when + " (" + describeRepeat(ev.RecurrenceRule) + ")")
	}
	if ev.Location != "" {
		where = "📍 " + escapeMarkdown(ev.Location)
	}
	return title, when, where
}

// eventTableHeadline is the table's head for an event, one part to a line:
//
//	**Fall Celebration! <Hosted by Heidi>**
//	🗓️ Tue 9/22 7pm
//	📍 Heidi's House
//
// The calendar is the forum post's own, and each emoji leads its line — a
// clock face beside the time, mid-line, came out wider than the text and
// spaced it oddly. The forum post goes on the line after, placed by the
// caller.
func eventTableHeadline(ev *Event) string {
	title, _, _ := eventHeadlineParts(ev)
	return eventHeadlineUnder(ev, title)
}

// eventHeadlineUnder is the same headline under a title of the caller's
// choosing: #events prints the name in bold, #new-events as a heading.
func eventHeadlineUnder(ev *Event, title string) string {
	lines := []string{}
	_, when, where := eventHeadlineParts(ev)
	if when != "" {
		when = "🗓️ " + when
	}
	for _, part := range []string{title, when, where} {
		if part != "" {
			lines = append(lines, part)
		}
	}
	return strings.Join(lines, "\n")
}

// eventSummaryLine is the same on one line, for the past-events channel,
// where each event is a single line:
//
//	**Board Game Night** - Tue 9/22 5pm (weekly) 📍 Baldini's Casino
func eventSummaryLine(ev *Event) string {
	title, when, where := eventHeadlineParts(ev)
	line := title
	if when != "" {
		line += " - " + when
	}
	if where != "" {
		line += " " + where
	}
	return line
}

// markdownSpecial are the characters Discord reads as formatting. A title is
// shown as typed, so each is escaped: a name like "*secret* party" must not
// turn bold, and one starting with ">" must not turn into a quote.
var markdownSpecial = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`,
	"<", `\<`, ">", `\>`, "#", `\#`)

func escapeMarkdown(text string) string { return markdownSpecial.Replace(text) }

// namesWithin joins names inside a rune budget, dropping names off the end
// rather than cutting one in half. The host — the event's creator, by user
// id — is underlined, with their name escaped so its own underscores cannot
// break the underline.
func namesWithin(signups []Signup, hostUserID string, budget int) string {
	names := rosterNamesOnDiscord(signups)
	for i, sg := range signups {
		if hostUserID != "" && sg.DiscordUserID == hostUserID {
			names[i] = "__" + escapeMarkdown(names[i]) + "__"
		}
	}
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

// eventTableBlock is one event as it will appear: its text, its buttons, and
// what that costs.
//
// The buttons are kept rather than asked for again at render time because a
// row's buttons can change between the two: End appears the moment an event
// starts. Rendering a row with one more button than was measured could put a
// full page over Discord's component cap, and Discord refuses the whole
// message then.
type eventTableBlock struct {
	event      *Event
	text       string
	buttons    []any
	components int
	characters int
}

// buildEventTableBlock renders one event with its roster and measures it.
//
// The measuring is the point. The event table can say "five events per message"
// because every row is about the same size; here a row carrying twenty names is
// many times one carrying none, so a fixed count would either waste most of a
// message or overflow it.
func buildEventTableBlock(ev *Event, roster []Signup, first bool, buttons func(*Event) []any, nameLink func(*Event) string) eventTableBlock {
	title, _, _ := eventHeadlineParts(ev)
	if nameLink != nil {
		if link := nameLink(ev); link != "" {
			title += " " + link
		}
	}
	text := eventTableText(ev, roster, title, eventTableCharBudget, textDisplayLimit)

	// One text block, one action row, its buttons, and the separator that
	// divides this block from the one above it.
	rowButtons := buttons(ev)
	components := 2 + len(rowButtons)
	if !first {
		components++
	}
	return eventTableBlock{event: ev, text: text, buttons: rowButtons,
		components: components, characters: len([]rune(text))}
}

// eventTableText is how #events writes an event: the title, the time and
// place one to a line, the forum post, then who is going, who might and who
// is waiting. Also the whole of an event's message in #new-events, so the two
// read the same; only the title's styling differs, so it is passed in.
//
// budget sizes the name lists — half of it for Going, a quarter each for Maybe
// and Waitlist — and limit is the most the text may run to.
func eventTableText(ev *Event, roster []Signup, title string, budget, limit int) string {
	attending, waiting := splitRoster(roster)
	attending = hostFirst(attending, ev.CreatedBy)

	var b strings.Builder
	b.WriteString(eventHeadlineUnder(ev, title))
	if ev.ForumPostID != "" {
		fmt.Fprintf(&b, "\n<#%s>", ev.ForumPostID)
	}
	// Then the live count and who is going, who might, and who is waiting —
	// the last two only when someone is on them. Generous per-line budgets:
	// the packer decides how many blocks fit in a message, and a single block
	// only needs trimming when one event alone would fill one.
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, "\n✅ **Going** (%d/%d): ", ev.AttendingCount, ev.Capacity)
	} else {
		fmt.Fprintf(&b, "\n✅ **Going** (%d): ", ev.AttendingCount)
	}
	if len(attending) == 0 {
		b.WriteString("nobody yet")
	} else {
		b.WriteString(namesWithin(attending, ev.CreatedBy, budget/2))
	}
	if maybe := maybeOf(roster); len(maybe) > 0 {
		b.WriteString("\n🤷 **Maybe**: " + namesWithin(maybe, ev.CreatedBy, budget/4))
	}
	if len(waiting) > 0 {
		b.WriteString("\n❌ **Waitlist**: " + namesWithin(waiting, ev.CreatedBy, budget/4))
	}
	return trimTo(b.String(), limit)
}

// packEventTable fills each message as full as it will go and starts another
// when the next event does not fit.
//
// Returns at least one page, so an empty guild still gets a message saying
// there is nothing on rather than leaving whatever was there last week.
func packEventTable(events []Event, rosters map[int64][]Signup, buttons func(*Event) []any, nameLink func(*Event) string, reserve int) [][]eventTableBlock {
	pages := [][]eventTableBlock{}
	var page []eventTableBlock
	// The container itself is a component.
	components, characters := 1, 0

	for i := range events {
		ev := &events[i]
		block := buildEventTableBlock(ev, rosters[ev.ID], len(page) == 0, buttons, nameLink)
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
			block = buildEventTableBlock(ev, rosters[ev.ID], true, buttons, nameLink)
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
func RenderEventTablePage(page []eventTableBlock, index, total int, leading, trailing [][]any) map[string]any {
	body := []any{}
	if len(page) == 0 {
		body = append(body, textBlock("-# Nothing coming up."))
	}
	// Controls that also go at the very top of the table, on its first page,
	// above a rule — so a long table does not have to be scrolled to its end
	// to reach them. Left off an empty table, where the bottom row is right
	// there anyway.
	if index == 0 && len(leading) > 0 && len(page) > 0 {
		body = append(body, actionRows(leading)...)
		body = append(body, map[string]any{"type": componentTypeSeparator, "divider": true, "spacing": 2})
	}
	for i, block := range page {
		if i > 0 {
			body = append(body, map[string]any{
				"type": componentTypeSeparator, "divider": true, "spacing": 1,
			})
		}
		body = append(body, textBlock(block.text))
		body = append(body, map[string]any{
			"type": componentTypeActionRow, "components": block.buttons,
		})
	}
	if total > 1 {
		body = append(body, textBlock(fmt.Sprintf("-# Page %d of %d", index+1, total)))
	}
	// Controls that belong to the table as a whole go on its last page only,
	// under a rule, so Create does not read as one more button on the last
	// event's row.
	if index == total-1 && len(trailing) > 0 {
		body = append(body, map[string]any{"type": componentTypeSeparator, "divider": true, "spacing": 2})
		body = append(body, actionRows(trailing)...)
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
				"label": "Maybe", "custom_id": MaybeCustomID(ev.ID)},
			map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Leave", "custom_id": LeaveCustomID(ev.ID)})
	}
	buttons = append(buttons,
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID)})
	return buttons
}

// closeToggleSubject is what the Close/Reopen toggle opens or closes:
// "waitlist" when the event is full and has one, since a new signup can only
// be waitlisted, and "signups" otherwise.
func closeToggleSubject(ev *Event) string {
	if eventIsFull(ev) && !ev.WaitlistDisabled {
		return "waitlist"
	}
	return "signups"
}

// repeatButton opens the Repeat form. On an event that already repeats it
// says how — "Repeats weekly" — so the row reads the schedule at a glance
// and the button is where it is changed or stopped.
func repeatButton(ev *Event) map[string]any {
	button := map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
		"label": "Repeat", "custom_id": RepeatCustomID(ev.ID)}
	if ev.RecurrenceRule != "" {
		button["label"] = "Repeats " + describeRepeat(ev.RecurrenceRule)
		button["emoji"] = map[string]any{"name": "🔁"}
	}
	return button
}

// managementButtons is the row on the management table: what an organiser
// does to an event, and nothing a member does.
func managementButtons(ev *Event) []any {
	buttons := []any{
		map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Edit", "custom_id": EditCustomID(ev.ID)},
		repeatButton(ev),
	}
	// One toggle whose label says which way it goes. Closed means nobody new
	// can join while everyone on it stays; it is not cancelled. On a full
	// event the only way in is the waitlist, so the label names that.
	switch ev.Status {
	case StatusOpen:
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Close " + closeToggleSubject(ev), "custom_id": CloseCustomID(ev.ID)})
	case StatusClosed:
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Reopen " + closeToggleSubject(ev), "custom_id": CloseCustomID(ev.ID)})
	}
	// End only while it is underway; before then Cancel is the way to stop it.
	// With it the row is five buttons, which is all an action row holds —
	// anything new here has to take one of these off first.
	if eventIsUnderway(ev) {
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "End", "custom_id": EndCustomID(ev.ID)})
	}
	return append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleDanger,
		"label": "Cancel", "custom_id": CancelCustomID(ev.ID)})
}

// managementTrailing is the last page's row on the management table: making a
// new event. A thing a person does rather than a thing about one event, which
// is why it is not on a row.
// managementLeading is the same Create button at the top of the table's first
// page. Its own custom_id: Discord refuses a message whose components share
// one, and a one-page table holds both rows.
func managementLeading(webOrigin string) [][]any {
	return append([][]any{{
		map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
			"label": "Create an event", "custom_id": CreateAtTopCustomID()},
	}}, advancedSettingsRow(webOrigin)...)
}

func managementTrailing(webOrigin string) [][]any {
	return append([][]any{{
		map[string]any{"type": componentTypeButton, "style": buttonStylePrimary,
			"label": "Create an event", "custom_id": CreateCustomID()},
	}}, advancedSettingsRow(webOrigin)...)
}

// advancedSettingsRow is the row under Create an event: a link to the web
// pages, where rosters, invites, held places and regulars are managed. None
// when the web pages are off.
func advancedSettingsRow(webOrigin string) [][]any {
	if webOrigin == "" {
		return nil
	}
	return [][]any{{map[string]any{"type": componentTypeButton, "style": buttonStyleLink,
		"label": "Advanced Settings", "url": webOrigin + "/", "emoji": map[string]any{"name": "🌐"}}}}
}

// rowComponents counts the components some action rows take: each row, and
// each button in it.
func rowComponents(rows [][]any) int {
	n := 0
	for _, row := range rows {
		n += 1 + len(row)
	}
	return n
}

// actionRows wraps rows of buttons as action row components.
func actionRows(rows [][]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{"type": componentTypeActionRow, "components": row})
	}
	return out
}

// tableSurface is one channel's packed table: where it lives, what its rows
// can do, and where its pages are recorded. The public table and the
// management table are the same packer with different answers to those three.
type tableSurface struct {
	channelID string
	buttons   func(*Event) []any
	// nameLink goes after each event's name, or nil for nothing.
	nameLink func(*Event) string
	leading  [][]any
	trailing [][]any
	pages    func() ([]TablePage, error)
	setPage  func(page int, messageID string) error
	dropPage func(page int) error
}

// RefreshEventTable rewrites the public table in place.
func (s *Server) RefreshEventTable(guildID string) error {
	table, err := s.store.GuildTable(guildID)
	if errors.Is(err, ErrNotFound) || (err == nil && table.ChannelID == "") {
		return nil // a row that only carries the guild's channels has no table
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
		channelID: table.ManagementChannelID, buttons: managementButtons, nameLink: s.webRosterLink,
		leading: managementLeading(s.webOrigin()), trailing: managementTrailing(s.webOrigin()),
		pages:    func() ([]TablePage, error) { return s.store.ManagementPages(guildID) },
		setPage:  func(p int, m string) error { return s.store.SetManagementPage(guildID, p, m) },
		dropPage: func(p int) error { return s.store.DeleteManagementPage(guildID, p) },
	})
}

// webRosterLink goes after an event's name on the management table: a short
// link, "Advanced →", to its page on the web, where the roster, invites, held places and
// regulars are. In the text rather than a button, because the row already has
// the five buttons an action row holds. None when the web pages are off.
func (s *Server) webRosterLink(ev *Event) string {
	origin := s.webOrigin()
	if origin == "" {
		return ""
	}
	// Words and a plain arrow, no emoji: Discord does not make a masked link
	// of text holding one — "[🌐↗](…)" showed as bare text — and a globe
	// outside the brackets read as clickable when it was not. "Advanced"
	// matches the Advanced Settings button under Create an event.
	return fmt.Sprintf("[Advanced →](%s/events/%d)", origin, ev.ID)
}

// webOrigin is where the web pages are served, taken from the OAuth
// callback URL — the one address of them this service is told. "" when the
// web pages are off.
func (s *Server) webOrigin() string {
	if s.oauth == nil || s.oauth.RedirectURL == "" {
		return ""
	}
	u, err := url.Parse(s.oauth.RedirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("[discord-signup] web origin from redirect URL %q: %v", s.oauth.RedirectURL, err)
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// tableLock is the lock one guild's table redraws take in turn.
func (s *Server) tableLock(guildID string) *sync.Mutex {
	s.tableLocksMu.Lock()
	defer s.tableLocksMu.Unlock()
	lock, ok := s.tableLocks[guildID]
	if !ok {
		lock = &sync.Mutex{}
		s.tableLocks[guildID] = lock
	}
	return lock
}

func (s *Server) publishPackedTable(guildID string, surface tableSurface) error {
	if s.discord == nil {
		return nil
	}
	// Held from reading the recorded pages until the last one is recorded.
	lock := s.tableLock(guildID)
	lock.Lock()
	defer lock.Unlock()
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
	// Reserve: the trailing rows, their buttons and the divider above them,
	// and the same again for the leading rows. Every page keeps both, since
	// the packer does not know yet which pages are first and last. A table
	// with no trailing rows still keeps two, as it always has.
	reserve := max(rowComponents(surface.trailing), 1) + 1
	if len(surface.leading) > 0 {
		reserve += rowComponents(surface.leading) + 1
	}
	pages := packEventTable(events, rosters, surface.buttons, surface.nameLink, reserve)

	existing, err := surface.pages()
	if err != nil {
		return err
	}
	byPage := map[int]string{}
	for _, p := range existing {
		byPage[p.Page] = p.MessageID
	}

	for i, page := range pages {
		payload := RenderEventTablePage(page, i, len(pages), surface.leading, surface.trailing)
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
