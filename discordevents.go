package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Discord's own scheduled-event enumerations. Stored here as maps to our
// vocabulary rather than as if-chains at the call site, so adding a value means
// editing one table.
const (
	discordEventScheduled = 1
	discordEventActive    = 2
	discordEventCompleted = 3
	discordEventCanceled  = 4
)

// discordStatusToOurs maps Discord's lifecycle onto ours. Discord's COMPLETED
// maps to StatusCompleted rather than StatusClosed: the two are different
// facts, and flattening them here would lose the one the archive sorts on.
//
// ACTIVE deliberately maps to open, not closed. An event that has started is
// still one people turn up to late, and shutting signups the moment it begins
// would be a policy decision this service has no business making on its own.
// Close it explicitly if that is what you want.
var discordStatusToOurs = map[int]string{
	discordEventScheduled: StatusOpen,
	discordEventActive:    StatusOpen,
	discordEventCompleted: StatusCompleted,
	discordEventCanceled:  StatusCancelled,
}

var discordEntityTypeNames = map[int]string{
	1: "stage",
	2: "voice",
	3: "external",
}

var entityNameToDiscordType = map[string]int{
	"stage":    1,
	"voice":    2,
	"external": 3,
}

// DiscordScheduledEvent is the subset of Discord's scheduled event this service
// reads. Partial on purpose: every field decoded is one more that can break on
// a schema change Discord did not announce.
type DiscordScheduledEvent struct {
	ID                 string `json:"id"`
	GuildID            string `json:"guild_id"`
	ChannelID          string `json:"channel_id"`
	CreatorID          string `json:"creator_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	ScheduledStartTime string `json:"scheduled_start_time"`
	ScheduledEndTime   string `json:"scheduled_end_time"`
	Status             int    `json:"status"`
	EntityType         int    `json:"entity_type"`
	EntityMetadata     struct {
		Location string `json:"location"`
	} `json:"entity_metadata"`
	UserCount      int             `json:"user_count"`
	RecurrenceRule json.RawMessage `json:"recurrence_rule"`
}

// ListScheduledEvents reads a guild's native scheduled events.
//
// with_user_count is always on: Discord's Interested number is the one thing
// worth carrying over from the native event, precisely because it is the number
// this service cannot control and therefore has to display honestly next to its
// own.
func (c *DiscordClient) ListScheduledEvents(guildID string) ([]DiscordScheduledEvent, error) {
	raw, err := c.do(http.MethodGet,
		"/guilds/"+guildID+"/scheduled-events?with_user_count=true", nil)
	if err != nil {
		return nil, err
	}
	var out []DiscordScheduledEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode scheduled events: %w", err)
	}
	return out, nil
}

// CreateScheduledEvent publishes a local roster to Discord as a native event,
// so it appears in the server's event list and fires Discord's own start
// notification. Requires CREATE_EVENTS.
func (c *DiscordClient) CreateScheduledEvent(payload any) (*DiscordScheduledEvent, error) {
	raw, err := c.do(http.MethodPost, "/guilds/"+payloadGuildID(payload)+"/scheduled-events", payload)
	if err != nil {
		return nil, err
	}
	var out DiscordScheduledEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode created scheduled event: %w", err)
	}
	return &out, nil
}

// ModifyScheduledEvent edits a native event. Requires MANAGE_EVENTS for one the
// bot did not create — which is why the re-invite mattered: without it, an
// imported event keeps a live Interested button and nothing on the Discord side
// telling people the real roster is elsewhere.
func (c *DiscordClient) ModifyScheduledEvent(guildID, eventID string, payload any) error {
	_, err := c.do(http.MethodPatch,
		"/guilds/"+guildID+"/scheduled-events/"+eventID, payload)
	return err
}

func payloadGuildID(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["guild_id"].(string)
	return s
}

// SyncResult reports what one sync pass did.
type SyncResult struct {
	Imported  int      `json:"imported"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Posted    int      `json:"posted"`
	Published int      `json:"published"`
	Cancelled int      `json:"cancelled"`
	Problems  []string `json:"problems,omitempty"`
}

// SyncScheduledEvents pulls a guild's native events into the local store.
//
// Polling rather than the gateway, deliberately: GUILD_SCHEDULED_EVENT_CREATE
// is gateway-only, and event *definitions* change rarely enough that a few
// minutes of lag costs nothing. RSVPs are the opposite — they need the gateway,
// because the users endpoint returns subscribers ordered by user id (account
// age), which cannot reconstruct arrival order for a waitlist.
//
// An event that has VANISHED from Discord's list is left alone, not cancelled.
// Discord drops COMPLETED events out of the list endpoint, so absence means
// "not listed", never "deleted", and auto-cancelling on absence would quietly
// kill every event the day after it happened.
func (s *Server) SyncScheduledEvents(guildID, boardChannelID string) (*SyncResult, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	remote, err := s.discord.ListScheduledEvents(guildID)
	if err != nil {
		return nil, fmt.Errorf("list scheduled events: %w", err)
	}

	result := &SyncResult{}
	for _, r := range remote {
		changed, imported, err := s.syncOneScheduledEvent(r, boardChannelID)
		if err != nil {
			result.Problems = append(result.Problems,
				fmt.Sprintf("%s (%s): %v", r.Name, r.ID, err))
			continue
		}
		switch {
		case imported:
			result.Imported++
		case changed:
			result.Updated++
		default:
			result.Unchanged++
		}
	}
	posted, problems := s.postMissingCards(guildID)
	result.Posted = posted
	result.Problems = append(result.Problems, problems...)
	published, cancelledCount, reconcileProblems := s.reconcileWithNative(guildID, remote)
	result.Published = published
	result.Cancelled = cancelledCount
	result.Problems = append(result.Problems, reconcileProblems...)
	// Imported and edited events both change what the panel should say, and
	// the panel is one message, so one redraw covers all of them.
	if result.Imported > 0 || result.Updated > 0 {
		s.refreshEventTableQuietly(guildID)
	}
	return result, nil
}

// postMissingCards puts a signup card on the board for every imported event
// that has not got one yet.
//
// Only events with origin 'discord'. An event created here is deliberately not
// posted automatically: it has not been announced anywhere, so its author gets
// to look at it before it appears in a channel. One created in Discord is
// already public the moment it exists, so mirroring it costs nobody a surprise.
//
// Driven off an empty message_id rather than a flag, so it is naturally
// idempotent and naturally retries: a post that fails leaves the id empty and
// the next sync tries again.
func (s *Server) postMissingCards(guildID string) (posted int, problems []string) {
	events, err := s.store.ListEvents(guildID, "", 500)
	if err != nil {
		return 0, []string{"list events: " + err.Error()}
	}
	for _, ev := range events {
		if ev.Origin != OriginDiscord || ev.MessageID != "" {
			continue
		}
		// Nothing is posted for an event that is over. A card for last month's
		// event appearing on the board today reads as an announcement.
		if IsArchived(ev.Status) {
			continue
		}
		if _, err := s.PostSignupMessage(ev.ID); err != nil {
			problems = append(problems, fmt.Sprintf("post card for %q: %v", ev.Name, err))
			continue
		}
		log.Printf("[discord-signup] posted a signup card for imported event %d (%q)", ev.ID, ev.Name)
		posted++
	}
	return posted, problems
}

// SyncAllGuilds pulls native events from every server the bot is in.
//
// The guild list comes from Discord rather than from configuration, so adding
// the bot to another server is all it takes — there is no list here to forget
// to update, and no guild id written into a cron command.
func (s *Server) SyncAllGuilds() (*SyncResult, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	guilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return nil, fmt.Errorf("list bot guilds: %w", err)
	}
	total := &SyncResult{}
	for _, g := range guilds {
		result, err := s.SyncScheduledEvents(g.ID, s.boardChannelID)
		if err != nil {
			total.Problems = append(total.Problems, g.Name+": "+err.Error())
			continue
		}
		total.Imported += result.Imported
		total.Updated += result.Updated
		total.Unchanged += result.Unchanged
		total.Posted += result.Posted
		total.Published += result.Published
		total.Cancelled += result.Cancelled
		total.Problems = append(total.Problems, result.Problems...)
	}
	return total, nil
}

func (s *Server) syncOneScheduledEvent(r DiscordScheduledEvent, boardChannelID string) (changed, imported bool, err error) {
	status, ok := discordStatusToOurs[r.Status]
	if !ok {
		return false, false, fmt.Errorf("unknown discord status %d", r.Status)
	}
	startsAt := parseDiscordTime(r.ScheduledStartTime)
	endsAt := parseDiscordTime(r.ScheduledEndTime)

	existing, err := s.store.EventByDiscordScheduledEventID(r.ID)
	if errors.Is(err, ErrNotFound) {
		// An event this bot created, with no local roster pointing at it yet,
		// is almost always one we published seconds ago whose id has not been
		// written back yet — the gateway is faster than the round trip.
		// Importing it would create a second local event for the same native
		// one, which the unique index then refuses, leaving a duplicate and a
		// broken link. Skipping is right in the rare genuine-orphan case too:
		// an event we made and then lost the roster for is not something to
		// silently re-adopt.
		if r.CreatorID != "" && r.CreatorID == s.applicationUserID() {
			log.Printf("[discord-signup] skipping discord event %s (%q) — this bot created it "+
				"and no local roster points at it yet", r.ID, r.Name)
			return false, false, nil
		}
		// The board channel, not the event's own channel. A voice event's
		// channel_id is the voice room people talk in; posting a signup card
		// there would be invisible.
		channelID := boardChannelID
		if channelID == "" {
			channelID = r.ChannelID
		}
		created, err := s.store.CreateEvent(Event{
			GuildID:                 r.GuildID,
			ChannelID:               channelID,
			DiscordScheduledEventID: r.ID,
			Name:                    stripCapacityPrefix(r.Name),
			Description:             stripSignupPointer(r.Description),
			// Discord had no cap, so the imported event starts uncapped. Adding
			// one is a decision a person makes on the web page; inventing a
			// number here would silently waitlist people who were already in.
			Capacity:               0,
			Status:                 status,
			StartsAt:               startsAt,
			EndsAt:                 endsAt,
			Location:               stripLocationPlaceholder(r.EntityMetadata.Location),
			EntityType:             discordEntityTypeNames[r.EntityType],
			RecurrenceRule:         recurrenceRuleText(r.RecurrenceRule),
			Timezone:               recurrenceTimezone(r.RecurrenceRule),
			Origin:                 OriginDiscord,
			DiscordInterestedCount: r.UserCount,
			DiscordSyncedAt:        now(),
			CreatedBy:              r.CreatorID,
		})
		if err != nil {
			return false, false, err
		}
		log.Printf("[discord-signup] imported discord event %q as event %d", r.Name, created.ID)
		return true, true, nil
	}
	if err != nil {
		return false, false, err
	}

	// Only push fields Discord owns. Capacity, roles and message_id are ours
	// and must survive a sync — overwriting them here would reset the cap to
	// unlimited every few minutes.
	patch := EventPatch{}
	if incoming := stripCapacityPrefix(r.Name); existing.Name != incoming {
		patch.Name = &incoming
	}
	if incoming := stripSignupPointer(r.Description); existing.Description != incoming {
		patch.Description = &incoming
	}
	if existing.StartsAt != startsAt {
		patch.StartsAt = &startsAt
	}
	if existing.EndsAt != endsAt {
		patch.EndsAt = &endsAt
	}
	if existing.Status != status {
		patch.Status = &status
	}
	if incoming := stripLocationPlaceholder(r.EntityMetadata.Location); existing.Location != incoming {
		patch.Location = &incoming
	}
	if existing.DiscordInterestedCount != r.UserCount {
		count := r.UserCount
		patch.DiscordInterestedCount = &count
	}
	if patch == (EventPatch{}) {
		return false, false, nil
	}
	if _, err := s.store.UpdateEvent(existing.ID, patch); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// parseDiscordTime turns Discord's ISO 8601 timestamp into unix seconds. An
// empty or unparseable value becomes 0, which every read path already treats as
// "unknown" — the alternative, a zero time in 1 AD, renders as a real date.
func parseDiscordTime(s string) int64 {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		log.Printf("[discord-signup] unparseable discord timestamp %q: %v", s, err)
		return 0
	}
	return t.Unix()
}

// recurrenceRuleText renders Discord's structured recurrence_rule object back
// into an RRULE string, which is what this store keeps and what the web page
// edits. Discord accepts a subset of RFC 5545 but expresses it as JSON rather
// than as the rule text.
func recurrenceRuleText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var rr struct {
		Frequency  *int  `json:"frequency"`
		Interval   *int  `json:"interval"`
		ByWeekday  []int `json:"by_weekday"`
		ByMonth    []int `json:"by_month"`
		ByMonthDay []int `json:"by_month_day"`
	}
	if err := json.Unmarshal(raw, &rr); err != nil || rr.Frequency == nil {
		return ""
	}
	// Discord numbers frequency downward: 0 YEARLY, 1 MONTHLY, 2 WEEKLY, 3 DAILY.
	freqNames := map[int]string{0: "YEARLY", 1: "MONTHLY", 2: "WEEKLY", 3: "DAILY"}
	name, ok := freqNames[*rr.Frequency]
	if !ok {
		return ""
	}
	parts := []string{"FREQ=" + name}
	if rr.Interval != nil && *rr.Interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", *rr.Interval))
	}
	// Discord numbers weekdays from Monday=0; RFC 5545 names them.
	dayNames := []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}
	if len(rr.ByWeekday) > 0 {
		days := make([]string, 0, len(rr.ByWeekday))
		for _, d := range rr.ByWeekday {
			if d >= 0 && d < len(dayNames) {
				days = append(days, dayNames[d])
			}
		}
		if len(days) > 0 {
			parts = append(parts, "BYDAY="+strings.Join(days, ","))
		}
	}
	return strings.Join(parts, ";")
}

// recurrenceTimezone reports the zone a recurring Discord event runs in.
//
// Discord does not send one: its recurrence_rule carries no tzid, and the start
// time is an absolute instant. UTC is therefore the only honest answer for an
// imported rule — it is what Discord actually means — and the web page is where
// a real zone gets set. Returning "" instead would fail validateRecurrence and
// make the import refuse every recurring event.
func recurrenceTimezone(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return "UTC"
}

// PublishToDiscord creates a native scheduled event for a local roster, so it
// shows in the server's event list, and links the two by id.
//
// The description carries a pointer back to the signup message, because the
// native event's own Interested button cannot be capped or removed. Without
// that line, a full event still shows a live Interested button and nothing
// tells the person pressing it that it does not hold them a place.
func (s *Server) PublishToDiscord(eventID int64, boardChannelID string) (*Event, error) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	if ev.DiscordScheduledEventID != "" {
		return nil, fmt.Errorf("event %d is already linked to discord event %s",
			eventID, ev.DiscordScheduledEventID)
	}
	if ev.StartsAt == 0 {
		return nil, fmt.Errorf("%w: a discord scheduled event needs a start time", ErrInvalidEvent)
	}
	// EXTERNAL is the only entity type that does not need a voice or stage
	// channel, and it is the only one Discord requires an end time for.
	endsAt := ev.EndsAt
	if endsAt == 0 {
		// The same assumption the archive sweep makes. Two different guesses
		// about how long an event runs would eventually disagree in a way
		// nobody could explain.
		endsAt = ev.StartsAt + assumedRunTimeWithoutEndTime
	}
	location := ev.Location
	if location == "" {
		// Discord requires a non-empty location on an EXTERNAL event and
		// refuses the whole request without one. This is a label, not invented
		// data — and the import strips it back to empty, or it would come home
		// as a location somebody typed.
		location = locationPlaceholder
	}
	payload := map[string]any{
		"guild_id":             ev.GuildID,
		"name":                 nativeEventName(ev),
		"description":          ev.Description + signupPointer(ev, boardChannelID),
		"scheduled_start_time": time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339),
		"scheduled_end_time":   time.Unix(endsAt, 0).UTC().Format(time.RFC3339),
		"privacy_level":        2, // GUILD_ONLY, the only value Discord accepts
		"entity_type":          entityNameToDiscordType["external"],
		"entity_metadata":      map[string]any{"location": location},
	}
	created, err := s.discord.CreateScheduledEvent(payload)
	if err != nil {
		return nil, fmt.Errorf("create discord scheduled event: %w", err)
	}
	// Written back immediately. Everything between the line above and this one
	// is the window in which the gateway can see an event we own but cannot
	// recognise, which is why syncOneScheduledEvent checks the creator.
	return s.store.UpdateEvent(eventID, EventPatch{DiscordScheduledEventID: &created.ID})
}

// locationPlaceholder is what gets sent as a native event's location when the
// event has none — Discord refuses an EXTERNAL event without one. Like the
// badge and the pointer, it must be stripped on import or it round-trips: the
// placeholder comes back as if a person typed it, and then shows on every
// surface as a location that locates nothing. Found live, on two events.
const locationPlaceholder = "See the signup card"

// stripLocationPlaceholder maps our own filler back to the empty value it
// stands for.
func stripLocationPlaceholder(location string) string {
	if location == locationPlaceholder {
		return ""
	}
	return location
}

// capacityPrefixPattern matches the badge this service puts at the front of a
// native event's name, and nothing else.
//
// Anchored, and specific about the digits, because it is used to REMOVE the
// badge from a name read back from Discord. Miss it and the name round-trips:
// we push "[3/8] Games", the importer stores that as the name, and the next
// push produces "[4/8] [3/8] Games" — the same accumulating corruption the
// description pointer had, in a field capped at 100 characters, so it breaks
// sooner and louder.
var capacityPrefixPattern = regexp.MustCompile(`^\[\d+/\d+\]\s+`)

// discordEventNameLimit is Discord's cap on a scheduled event name.
const discordEventNameLimit = 100

// capacityPrefix is the "[3/8] " badge, or empty when the event has no limit.
//
// Only when there is a capacity: "[7/∞]" would be noise, and an event without a
// limit has no number worth putting in a title.
func capacityPrefix(ev *Event) string {
	if ev.Capacity <= 0 {
		return ""
	}
	return fmt.Sprintf("[%d/%d] ", ev.AttendingCount, ev.Capacity)
}

// stripCapacityPrefix removes this service's own badge from a name read back
// from Discord, so what is stored is the name a person actually gave it.
func stripCapacityPrefix(name string) string {
	return capacityPrefixPattern.ReplaceAllString(name, "")
}

// nativeEventName is the name to send Discord: the badge, then as much of the
// real name as fits.
//
// The name is trimmed rather than the whole string, so the badge always
// survives — a title truncated to "[3/8] Board game ni" is still readable,
// whereas one truncated the other way loses the number the badge exists for.
func nativeEventName(ev *Event) string {
	prefix := capacityPrefix(ev)
	room := discordEventNameLimit - len([]rune(prefix))
	return prefix + truncate(ev.Name, room)
}

// signupPointerMarker begins the line this service appends to a native event's
// description. It is matched on import to strip the line off again.
//
// Without that, the text round-trips: we append the pointer when publishing,
// the sync reads the whole description back as ours, and the next publish
// appends the pointer to a description that already ends in one. It grows by a
// paragraph every edit, forever, until Discord refuses the event for length.
// The marker deliberately stops before the destination: it has to match both
// the old form ("— Signups are in <#channel>") still sitting in descriptions
// Discord holds, and the forum form ("— Signups are in the forum: <url>"), or
// the strip misses one of them and the round-trip corruption returns.
const signupPointerMarker = "\n\n— Signups are in "

// stripSignupPointer removes this service's own footer from a description read
// back from Discord, so what is stored is what a person actually wrote.
func stripSignupPointer(description string) string {
	if i := strings.Index(description, signupPointerMarker); i >= 0 {
		return strings.TrimRight(description[:i], "\n ")
	}
	return description
}

// signupPointer is the line appended to a native event's description telling
// people where the real roster is. Says the number too, because "signups are
// elsewhere" is much less convincing than "20 places, 3 left".
func signupPointer(ev *Event, boardChannelID string) string {
	var b strings.Builder
	if ev.ForumPostID != "" {
		// The forum post is signups AND discussion in one place, so it wins
		// over the board channel whenever it exists.
		fmt.Fprintf(&b, "%sthe forum: https://discord.com/channels/%s/%s",
			signupPointerMarker, ev.GuildID, ev.ForumPostID)
	} else {
		b.WriteString(signupPointerMarker + "<#" + boardChannelID + ">")
	}
	if ev.Capacity > 0 {
		fmt.Fprintf(&b, " (%s", pluralise(ev.Capacity, "place"))
		if left := ev.Capacity - ev.AttendingCount; left > 0 {
			fmt.Fprintf(&b, ", %d left", left)
		} else {
			b.WriteString(", full — waitlist open")
		}
		b.WriteString(")")
	}
	b.WriteString(". Pressing Interested here does not hold you a place.")
	return b.String()
}

// DiscordEventURL is the deep link to a native scheduled event.
func DiscordEventURL(guildID, scheduledEventID string) string {
	if guildID == "" || scheduledEventID == "" {
		return ""
	}
	return "https://discord.com/events/" + url.PathEscape(guildID) + "/" + url.PathEscape(scheduledEventID)
}

// assumedRunTimeWithoutEndTime is how long an event with no end time is
// presumed to last before it counts as finished.
//
// This is a guess and is isolated here so it is visible as one. An end time is
// optional on the form, so some events genuinely do not carry the fact needed
// to answer "is it over"; the alternative to assuming is leaving last month's
// events sitting in the live list forever. Six hours is long enough that a
// normal evening event is not archived while people are still at it.
const assumedRunTimeWithoutEndTime = 6 * 3600

// finishedBy reports the instant an event stops being current.
func finishedBy(ev *Event) int64 {
	if ev.EndsAt > 0 {
		return ev.EndsAt
	}
	if ev.StartsAt > 0 {
		return ev.StartsAt + assumedRunTimeWithoutEndTime
	}
	// No times at all: nothing can be concluded, so it is never auto-archived.
	return 0
}

// CompleteFinishedEvents moves events whose time has passed to completed.
//
// Only touches open and closed events. A cancelled one is already archived and
// must not be relabelled as having happened — it did not.
//
// Recurring events are skipped: a rule means the event comes round again, so
// the row is not finished just because this occurrence is. Expanding a series
// into per-occurrence rows is a separate piece of work, and quietly archiving
// the parent would make the whole series vanish.
func (s *Store) CompleteFinishedEvents() ([]int64, error) {
	rows, err := s.db.Query(`SELECT `+eventColumns+`
		FROM events
		WHERE deleted_at = 0 AND status IN (?, ?) AND recurrence_rule = ''`,
		StatusOpen, StatusClosed)
	if err != nil {
		return nil, fmt.Errorf("scan for finished events: %w", err)
	}
	defer rows.Close()

	var finished []int64
	cutoff := now()
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if end := finishedBy(ev); end > 0 && end < cutoff {
			finished = append(finished, ev.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}

	for _, id := range finished {
		if _, err := s.db.Exec(
			`UPDATE events SET status = ?, updated_at = ? WHERE id = ?`,
			StatusCompleted, cutoff, id); err != nil {
			return nil, fmt.Errorf("complete event %d: %w", id, err)
		}
		log.Printf("[discord-signup] event %d finished; moved to the archive", id)
	}
	return finished, nil
}

// CompleteFinishedEvents archives events whose time has passed and rewrites
// their signup messages.
//
// The message rewrite is the point of doing this on the server rather than only
// in the web page's rendering: a finished event whose card still carries a live
// Join button will keep taking signups for something that already happened.
// RenderSignupMessage sends an empty components array for any non-open event,
// which removes the buttons rather than leaving the old ones in place.
func (s *Server) CompleteFinishedEvents() ([]int64, error) {
	finished, err := s.store.CompleteFinishedEvents()
	if err != nil {
		return nil, err
	}
	for _, id := range finished {
		// Refresh first: the card that moves should already say the event has
		// finished and have lost its buttons. Moving a stale card would put a
		// live Join button in the past-events channel.
		if err := s.RefreshSignupMessage(id); err != nil {
			log.Printf("[discord-signup] refresh finished event %d: %v", id, err)
			continue
		}
		if err := s.moveCardToPastEvents(id); err != nil {
			log.Printf("[discord-signup] move event %d to past events: %v", id, err)
		}
		// It has left the live list, so the panel has to stop showing it.
		// It has left the live list, so the table has to stop showing it and
		// its discussion closes with it.
		if ev, err := s.store.GetEvent(id); err == nil {
			s.refreshEventTableQuietly(ev.GuildID)
			// The forum post gets its finished tag and archives.
			s.refreshForumPostQuietly(ev)
			if ev.ThreadID != "" {
				if err := s.discord.ArchiveThread(ev.ThreadID); err != nil {
					log.Printf("[discord-signup] archive thread for event %d: %v", id, err)
				}
			}
		}
	}
	return finished, nil
}

// PushEditToDiscord updates the native scheduled event linked to a local
// roster, so the two do not drift apart after an edit.
//
// Silently does nothing when there is no link, which is the common case: an
// event that was never published has nothing to push to. A failure is returned
// rather than swallowed, but callers treat it as non-fatal — the roster is the
// source of truth and the native event is a copy of it.
func (s *Server) PushEditToDiscord(ev *Event) error {
	if s.discord == nil || ev.DiscordScheduledEventID == "" {
		return nil
	}
	endsAt := ev.EndsAt
	if endsAt == 0 {
		endsAt = ev.StartsAt + assumedRunTimeWithoutEndTime
	}
	location := ev.Location
	if location == "" {
		location = locationPlaceholder
	}
	payload := map[string]any{
		"name":                 nativeEventName(ev),
		"description":          ev.Description + signupPointer(ev, s.boardChannelID),
		"scheduled_start_time": time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339),
		"scheduled_end_time":   time.Unix(endsAt, 0).UTC().Format(time.RFC3339),
		"entity_metadata":      map[string]any{"location": location},
	}
	return s.discord.ModifyScheduledEvent(ev.GuildID, ev.DiscordScheduledEventID, payload)
}

// pluralise writes "1 place" and "6 places". A count is almost always rendered
// next to its noun, and "1 places" in a description that Discord shows to the
// whole server reads as carelessness.
func pluralise(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// GetScheduledEvent fetches one native event directly. exists=false means
// Discord no longer has it — the one signal the LIST endpoint cannot give,
// because completed events drop out of the list too, so absence there is
// ambiguous and absence HERE is not.
func (c *DiscordClient) GetScheduledEvent(guildID, eventID string) (ev *DiscordScheduledEvent, exists bool, err error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+guildID+"/scheduled-events/"+eventID, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	var out DiscordScheduledEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("decode scheduled event: %w", err)
	}
	return &out, true, nil
}

// DeleteScheduledEvent removes a native event.
func (c *DiscordClient) DeleteScheduledEvent(guildID, eventID string) error {
	_, err := c.do(http.MethodDelete, "/guilds/"+guildID+"/scheduled-events/"+eventID, nil)
	return err
}

// reconcileWithNative closes the gaps the import loop cannot see, in both
// directions, so the native list and this store cannot drift apart for longer
// than one sync interval:
//
//	local live, never published   → publish (heals web- and API-created events,
//	                                and retries any publish that once failed)
//	local live, native GONE       → cancel locally. Deleting the native event
//	                                is how a person cancels in Discord's own UI,
//	                                and a roster that outlives its event takes
//	                                signups for nothing.
//	local live, native CANCELED   → cancel locally
//	local live, native COMPLETED  → leave to the time sweep, which owns that
//	local CANCELLED, native alive → delete the native event, so cancelling on
//	                                any surface cancels everywhere
func (s *Server) reconcileWithNative(guildID string, remote []DiscordScheduledEvent) (published, cancelled int, problems []string) {
	remoteByID := map[string]DiscordScheduledEvent{}
	for _, r := range remote {
		remoteByID[r.ID] = r
	}
	events, err := s.store.ListEvents(guildID, "", 200)
	if err != nil {
		return 0, 0, []string{"list events: " + err.Error()}
	}
	for i := range events {
		ev := &events[i]
		switch {
		case !IsArchived(ev.Status) && ev.DiscordScheduledEventID == "":
			// Discord refuses a start time in the past, and an event that close
			// to done is hours from the archive anyway.
			if ev.StartsAt <= now() {
				continue
			}
			if _, err := s.PublishToDiscord(ev.ID, s.boardChannelID); err != nil {
				problems = append(problems, fmt.Sprintf("publish %q: %v", ev.Name, err))
				continue
			}
			published++

		case !IsArchived(ev.Status) && ev.DiscordScheduledEventID != "":
			if _, listed := remoteByID[ev.DiscordScheduledEventID]; listed {
				continue
			}
			native, exists, err := s.discord.GetScheduledEvent(guildID, ev.DiscordScheduledEventID)
			if err != nil {
				problems = append(problems, fmt.Sprintf("check %q: %v", ev.Name, err))
				continue
			}
			if exists && native.Status != discordEventCanceled {
				continue // completed or still scheduled; the time sweep owns those
			}
			if err := s.cancelEventEverywhere(ev, "its Discord event was deleted"); err != nil {
				problems = append(problems, fmt.Sprintf("cancel %q: %v", ev.Name, err))
				continue
			}
			cancelled++

		case ev.Status == StatusCancelled && ev.DiscordScheduledEventID != "":
			if _, listed := remoteByID[ev.DiscordScheduledEventID]; !listed {
				continue
			}
			if err := s.discord.DeleteScheduledEvent(guildID, ev.DiscordScheduledEventID); err != nil {
				problems = append(problems, fmt.Sprintf("delete native for %q: %v", ev.Name, err))
			}
		}
	}
	return published, cancelled, problems
}

// cancelEventEverywhere marks an event cancelled and pushes that fact onto
// every surface: the card loses its buttons, the table drops the row, the
// forum post gets the cancelled tag and archives, the thread closes.
func (s *Server) cancelEventEverywhere(ev *Event, why string) error {
	status := StatusCancelled
	updated, err := s.store.UpdateEvent(ev.ID, EventPatch{Status: &status})
	if err != nil {
		return err
	}
	log.Printf("[discord-signup] event %d (%q) cancelled: %s", ev.ID, ev.Name, why)
	if err := s.RefreshSignupMessage(ev.ID); err != nil {
		log.Printf("[discord-signup] refresh cancelled card %d: %v", ev.ID, err)
	}
	s.refreshEventTableQuietly(ev.GuildID)
	s.refreshForumPostQuietly(updated)
	if updated.ThreadID != "" {
		if err := s.discord.ArchiveThread(updated.ThreadID); err != nil {
			log.Printf("[discord-signup] archive thread for cancelled %d: %v", ev.ID, err)
		}
	}
	return nil
}
