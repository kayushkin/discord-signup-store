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
		"/guilds/"+escapePathSegment(guildID)+"/scheduled-events?with_user_count=true", nil)
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
//
// The guild id is a parameter, like it is on every sibling method here, rather
// than something dug back out of the payload map. Discord's create body has no
// guild_id field at all — the guild is the route — so reading one out of the
// body was archaeology on a value the caller already held.
//
// An empty guild id is refused rather than sent. "/guilds//scheduled-events"
// is a different route, not a bad argument, so whatever Discord answers would
// describe the request and never the mistake that built it.
func (c *DiscordClient) CreateScheduledEvent(guildID string, payload any) (*DiscordScheduledEvent, error) {
	if guildID == "" {
		return nil, errors.New("create scheduled event: no guild id")
	}
	raw, err := c.do(http.MethodPost, "/guilds/"+escapePathSegment(guildID)+"/scheduled-events", payload)
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
		"/guilds/"+escapePathSegment(guildID)+"/scheduled-events/"+escapePathSegment(eventID), payload)
	return err
}

// SyncResult reports what one sync pass did.
type SyncResult struct {
	Imported  int `json:"imported"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Published int `json:"published"`
	Cancelled int `json:"cancelled"`
	// Finished counts events Discord says are over — somebody pressed End on
	// the native event — that this run settled locally.
	Finished int      `json:"finished"`
	Problems []string `json:"problems,omitempty"`
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
func (s *Server) SyncScheduledEvents(guildID string) (*SyncResult, error) {
	boardChannelID := s.guildChannels(guildID).Board
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
	published, cancelledCount, finishedCount, reconcileProblems := s.reconcileWithNative(guildID, remote)
	result.Published = published
	result.Cancelled = cancelledCount
	result.Finished = finishedCount
	result.Problems = append(result.Problems, reconcileProblems...)
	// Imported and edited events both change what the panel should say, and
	// the panel is one message, so one redraw covers all of them.
	if result.Imported > 0 || result.Updated > 0 {
		s.refreshTablesQuietly(guildID)
	}
	return result, nil
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
		result, err := s.SyncScheduledEvents(g.ID)
		if err != nil {
			total.Problems = append(total.Problems, g.Name+": "+err.Error())
			continue
		}
		total.Imported += result.Imported
		total.Updated += result.Updated
		total.Unchanged += result.Unchanged
		total.Published += result.Published
		total.Cancelled += result.Cancelled
		total.Finished += result.Finished
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
			Name:                    stripTitleDecorations(r.Name),
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
			Timezone:               recurrenceTimezone(r.RecurrenceRule, s.DefaultTimezone()),
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

	// Discord holds a recurring event as one object whose start slides to the
	// next occurrence once the current one ends (measured 2026-09-03: same id,
	// start a week on). A start that moved forward past an occurrence that had
	// already ended is that slide, and the roster it carried was for the date
	// that has gone. Rolled here, through the same path the sweep uses, so
	// whichever of the two notices first does the whole job and the other
	// finds nothing left to do. A moved date that has not happened yet is an
	// organiser's edit and is patched like any other.
	if existing.RecurrenceRule != "" && startsAt > existing.StartsAt && finishedBy(existing) < now() {
		s.rollOverOccurrence(existing, startsAt, endsAt)
		if existing, err = s.store.GetEvent(existing.ID); err != nil {
			return false, false, err
		}
	}

	// Only push fields Discord owns. Capacity, roles and message_id are ours
	// and must survive a sync — overwriting them here would reset the cap to
	// unlimited every few minutes.
	patch := EventPatch{}
	if incoming := stripTitleDecorations(r.Name); existing.Name != incoming {
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
	// Closed is a decision made HERE that Discord cannot represent: its own
	// statuses are scheduled, active, completed and cancelled, and a closed
	// event is still scheduled as far as Discord knows. So a native update —
	// including the one our own publish triggers seconds after every edit —
	// comes back "scheduled", maps to open, and used to silently reopen
	// signups somebody had just shut. Discord's word overrides ours only when
	// it is saying something it alone can know: the event ran, or was deleted.
	if existing.Status != status && !(existing.Status == StatusClosed && status == StatusOpen) {
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
		ByNWeekday []struct {
			N   int `json:"n"`
			Day int `json:"day"`
		} `json:"by_n_weekday"`
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
	// A monthly rule is "the nth weekday": BYDAY=2TU, which is how the form's
	// "monthly" is stored and how it comes back.
	if len(rr.ByNWeekday) == 1 && rr.ByNWeekday[0].Day >= 0 && rr.ByNWeekday[0].Day < len(dayNames) {
		parts = append(parts, fmt.Sprintf("BYDAY=%d%s", rr.ByNWeekday[0].N, dayNames[rr.ByNWeekday[0].Day]))
	}
	// A yearly rule is one date.
	if len(rr.ByMonth) == 1 && len(rr.ByMonthDay) == 1 {
		parts = append(parts, fmt.Sprintf("BYMONTH=%d;BYMONTHDAY=%d", rr.ByMonth[0], rr.ByMonthDay[0]))
	}
	return strings.Join(parts, ";")
}

// recurrenceTimezone reports the zone a recurring Discord event runs in.
//
// Discord does not send one: its recurrence_rule carries no tzid, and the
// start time is an absolute instant. The server's default zone is the one a
// person made the event in, and it is the zone the next occurrence is worked
// out in — UTC, which this returned until 2026-09-04, put a Friday-evening
// Los Angeles event on Saturday. Returning "" instead would fail
// validateRecurrence and make the import refuse every recurring event.
func recurrenceTimezone(raw json.RawMessage, defaultZone string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return defaultZone
}

// PublishToDiscord creates a native scheduled event for a local roster, so it
// shows in the server's event list, and links the two by id.
//
// The description carries a pointer back to the signup message, because the
// native event's own Interested button cannot be capped or removed. Without
// that line, a full event still shows a live Interested button and nothing
// tells the person pressing it that it does not hold them a place.
func (s *Server) PublishToDiscord(eventID int64) (*Event, error) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	boardChannelID := s.guildChannels(ev.GuildID).Board
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
	roster, err := s.store.Roster(eventID, false)
	if err != nil {
		return nil, err
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
		"name":                 nativeEventName(ev),
		"description":          nativeEventDescription(ev, roster, boardChannelID),
		"scheduled_start_time": time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339),
		"scheduled_end_time":   time.Unix(endsAt, 0).UTC().Format(time.RFC3339),
		"privacy_level":        2, // GUILD_ONLY, the only value Discord accepts
		"entity_type":          entityNameToDiscordType["external"],
		"entity_metadata":      map[string]any{"location": location},
	}
	if rule, ok := discordRecurrenceRule(ev, s.DefaultTimezone()); ok && rule != nil {
		payload["recurrence_rule"] = rule
	} else if !ok {
		log.Printf("[discord-signup] event %d: rule %q cannot be expressed to Discord; published without it", ev.ID, ev.RecurrenceRule)
	}
	created, err := s.discord.CreateScheduledEvent(ev.GuildID, payload)
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

// Decorating a title with its capacity — and, since 2026-09-02, doing it as
// rarely as the surfaces can bear.
//
// The title used to carry "[3/8]": a live count, at the front of the forum
// post's name and the native event's name. Both are renames, and Discord
// rate-limits thread renames to about two per ten minutes — so under signups
// the number in the title was the number from two renames ago, and a count
// that is usually wrong is worse than none. The live count lives in the table
// row and the card, which are message edits and have no such limit.
//
// What stays in the title is what changes rarely: whether it is full, and if
// it is not, how many places there are at all.
//
//	Board game night — 8/29 4pm [3/8]           capped, room left
//	[Full] Board game night — 8/29 4pm          capped, no room
//	Open house — 8/29 4pm                       no limit
//
// The count IS in the title again — at the end — but a title is a rename and
// renames are rate-limited, so a count change alone is renamed at most every
// ten minutes. Becoming full renames at once: that is the change a reader most
// needs and it is rare. Stopping being full does NOT — a place that opens is
// usually taken within minutes, and a title that says open when the place has
// already gone sends people to a waitlist they were told did not exist. Seeing
// "[Full]" for ten minutes after someone leaves is the smaller lie. A moved
// date waits its turn the same way. titleRenameDue is the whole of that
// decision, and the budget is why it is ten and not five: Discord allows a
// thread about two renames per ten minutes, and one count rename every five
// plus one fill inside the same window is three, which is a 429 and a writer
// asleep for as long as Discord says — every change to that event queued
// behind it. One count rename per ten minutes leaves the second slot free for
// the fill, whenever it comes.
//
// Every form this service has ever written is stripped on the way back,
// because a name read from Discord is stored as the event's own: miss one and
// it round-trips, and the next publish decorates a name already carrying last
// week's decoration. The same failure the description pointer had, in a field
// capped at 100 characters, so it breaks sooner and louder.

// titleDecorationPrefixes match anything this service has put at the FRONT of
// a title. The numeric badge is retired but still sitting on Discord.
var titleDecorationPrefixes = regexp.MustCompile(`^(\[\d+/\d+\]|\[Full\])\s+`)

// titleDecorationSuffix matches anything this service has put at the END of a
// title: the current "[3/8]", and the " · 8 places" form it briefly wrote.
var titleDecorationSuffix = regexp.MustCompile(`(\s+·\s+\d+ places?|\s+\[\d+/\d+\])$`)

// titleRenameInterval is how often a title is renamed for anything short of
// becoming full or being renamed by a person: a count change, a place opening
// up, a moved date. Ten minutes spends one of the roughly two renames Discord
// allows a thread per ten minutes, leaving the other for becoming full.
const titleRenameInterval = 10 * 60

// discordEventNameLimit is Discord's cap on a scheduled event name and a
// thread name alike.
const discordEventNameLimit = 100

// titlePrefix is "[Full] " for a capped event with no room, else empty.
func titlePrefix(ev *Event) string {
	if ev.Capacity > 0 && ev.AttendingCount >= ev.Capacity {
		return "[Full] "
	}
	return ""
}

// titleSuffix is " [3/8]" for a capped event with room, else empty. A full
// event carries "[Full]" at the front instead, and an uncapped one has no
// number worth putting in a title.
func titleSuffix(ev *Event) string {
	if ev.Capacity > 0 && ev.AttendingCount < ev.Capacity {
		return fmt.Sprintf(" [%d/%d]", ev.AttendingCount, ev.Capacity)
	}
	return ""
}

// titleIsFull reports whether a title, as written or as wanted, says Full.
func titleIsFull(title string) bool { return strings.HasPrefix(title, "[Full] ") }

// titleRenameDue decides whether the titles are renamed on this publish. One
// decision for both titles — the native event's and the forum post's — because
// they are renamed together and recorded together.
//
// A rename goes at once when a title has never been written, when the event
// became full, or when a person renamed it. Everything else that changes a
// title — the count moving, a place opening up, the date moving — waits until
// titleRenameInterval has passed since the last rename. Meanwhile the card,
// the table and the native description carry the live state.
//
// The forum title is what carries the date, and the native title is what
// carries the name without one, so the two are compared to their own records:
// the native pair says whether the name changed, the forum pair whether
// anything a forum reader sees did.
func titleRenameDue(ev *Event, wantNative, wantForum string, at int64) bool {
	writtenNative, writtenForum := ev.NativeTitleWritten, ev.ForumTitleWritten
	switch {
	case writtenNative == "" && writtenForum == "":
		return true
	case wantNative == writtenNative && wantForum == writtenForum:
		return false
	case titleIsFull(wantNative) && !titleIsFull(writtenNative):
		return true
	case stripTitleDecorations(wantNative) != stripTitleDecorations(writtenNative):
		return true
	case !titleIsFull(writtenNative) && titleLimit(wantNative) != titleLimit(writtenNative):
		// The limit changed, or a limit appeared or went: an organiser's edit,
		// as rare as a rename and as deliberate. It used to wait its ten
		// minutes like a signup, so a limit set on an uncapped event showed
		// nothing in the title for ten minutes while the roster filled. Not
		// when the written title says Full: it carries no limit to compare,
		// and a place opening up is the one change that must wait.
		return true
	default:
		return at-ev.TitleWrittenAt >= titleRenameInterval
	}
}

// titleLimit is the Y of a "[X/Y]" suffix, or "full" for a [Full] title, or
// "" for an uncapped one — the part of a decoration that only an edit moves.
func titleLimit(title string) string {
	if titleIsFull(title) {
		return "full"
	}
	if m := titleCountSuffix.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

// titleCountSuffix captures the limit out of a trailing "[3/8]".
var titleCountSuffix = regexp.MustCompile(`\[\d+/(\d+)\]$`)

// decorateTitle wraps a title in its decorations inside a length limit.
//
// The core is trimmed, never the decorations: a title cut to "[Full] Board
// game ni" still says the one thing the prefix exists for, whereas one cut the
// other way loses it.
func decorateTitle(core string, ev *Event, limit int) string {
	prefix, suffix := titlePrefix(ev), titleSuffix(ev)
	room := limit - len([]rune(prefix)) - len([]rune(suffix))
	return prefix + truncate(core, room) + suffix
}

// stripTitleDecorations removes everything this service has ever added to a
// title, so what is stored is the name a person actually gave it.
func stripTitleDecorations(name string) string {
	name = titleDecorationPrefixes.ReplaceAllString(name, "")
	return titleDecorationSuffix.ReplaceAllString(name, "")
}

// nativeEventName is the name to send Discord for the scheduled event.
func nativeEventName(ev *Event) string {
	return decorateTitle(ev.Name, ev, discordEventNameLimit)
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
const signupPointerMarker = "\n\n— Sign up with "

// retiredPointerMarkers are the forms this service used to append and that are
// still sitting in descriptions Discord is holding.
//
// Never pruned. A description written last month is stripped by the marker that
// was current last month, and dropping that line here does not edit Discord —
// it just stops the strip finding our own text, which then round-trips and
// grows a copy of itself on every publish.
var retiredPointerMarkers = []string{
	"\n\n— Signups are in ",
}

// stripSignupPointer removes this service's own footer from a description read
// back from Discord, so what is stored is what a person actually wrote.
func stripSignupPointer(description string) string {
	cut := -1
	for _, marker := range append([]string{signupPointerMarker}, retiredPointerMarkers...) {
		// Matched without the leading newlines as well: Discord trims leading
		// whitespace, so a block appended to an EMPTY description comes back
		// starting at the dash, the marker with its "\n\n" never matches, and
		// the block is stored as the organiser's own text — after which every
		// publish appends a second copy. Measured 2026-09-04 on an imported
		// event whose native description carried the block twice.
		bare := strings.TrimLeft(marker, "\n")
		if i := strings.Index(description, bare); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return description
	}
	return strings.TrimRight(description[:cut], "\n ")
}

// nativeDescriptionLimit is Discord's cap on a scheduled event's description.
const nativeDescriptionLimit = 1000

// signupBlock is what this service appends to a native event's description: how
// to sign up, how full it is, and who is going.
//
// It used to say "Signups are in the forum", which was misleading in the one
// place it mattered. Signing up is possible from wherever somebody happens to
// be — Interested on this very event, Join on the card, ✅ on the forum post —
// and naming one of them as *the* place sends people away from the button
// already in front of them. The forum link stays, as what it is: the discussion.
//
// The roster is listed by NAME, not by mention. Discord's own Interested list
// is not the roster and cannot be made into one, so an event that shows only
// that number shows a number nobody can check. Names are checkable. Mentions
// would ping every attendee each time the description is rewritten, which is
// every signup.
func signupBlock(ev *Event, roster []Signup, boardChannelID string, budget int) string {
	var b strings.Builder
	b.WriteString(signupPointerMarker + "**Interested** here")
	if ev.MessageID != "" || boardChannelID != "" {
		fmt.Fprintf(&b, ", or **Join** in <#%s>", boardChannelID)
	}
	b.WriteString(".")

	if ev.Capacity > 0 {
		fmt.Fprintf(&b, " %d of %d places taken", ev.AttendingCount, ev.Capacity)
		if ev.AttendingCount >= ev.Capacity {
			b.WriteString(" — full, waitlist open")
		}
		b.WriteString(".")
	} else if ev.AttendingCount > 0 {
		fmt.Fprintf(&b, " %s so far.", pluralise(ev.AttendingCount, "person"))
	}

	attending, waiting := splitRoster(roster)
	// The lists come last so that trimming them for length costs the least:
	// what goes first is how to sign up, which is the only part somebody has to
	// have.
	remaining := budget - len([]rune(b.String()))
	if line := rosterLine("Going", attending, remaining); line != "" {
		b.WriteString(line)
		remaining -= len([]rune(line))
	}
	if line := rosterLine("Waitlist", waiting, remaining); line != "" {
		b.WriteString(line)
		remaining -= len([]rune(line))
	}
	if ev.ForumPostID != "" {
		chat := fmt.Sprintf("\nChat about it: https://discord.com/channels/%s/%s",
			ev.GuildID, ev.ForumPostID)
		if len([]rune(chat)) <= remaining {
			b.WriteString(chat)
		}
	}
	return b.String()
}

// rosterLine writes "Going: Alice, Bob, Carol" inside a rune budget, dropping
// names off the end rather than cutting one in half.
//
// Returns "" when it cannot fit even the shortest honest version, because a
// heading with nobody under it says less than nothing.
func rosterLine(heading string, signups []Signup, budget int) string {
	if len(signups) == 0 {
		return ""
	}
	full := "\n" + heading + ": " + strings.Join(rosterDisplayNames(signups), ", ")
	if len([]rune(full)) <= budget {
		return full
	}
	names := rosterDisplayNames(signups)
	for shown := len(names) - 1; shown >= 1; shown-- {
		line := fmt.Sprintf("\n%s: %s and %d more", heading,
			strings.Join(names[:shown], ", "), len(names)-shown)
		if len([]rune(line)) <= budget {
			return line
		}
	}
	short := fmt.Sprintf("\n%s: %d", heading, len(names))
	if len([]rune(short)) <= budget {
		return short
	}
	return ""
}

// rosterDisplayNames is the names to print, falling back to the id only when
// there is genuinely no name — which shows as a raw number and is meant to,
// since inventing a name would be worse.
func rosterDisplayNames(signups []Signup) []string {
	out := make([]string, 0, len(signups))
	for _, sg := range signups {
		name := sg.DisplayName
		if name == "" {
			name = sg.DiscordUserID
		}
		out = append(out, name)
	}
	return out
}

// nativeEventDescription is what gets sent to Discord: what somebody wrote,
// then this service's block, inside Discord's limit.
//
// The person's own words are never trimmed to make room for ours. If they have
// written 990 characters there is no block, which is the right way round: the
// block is useful and the description is theirs.
func nativeEventDescription(ev *Event, roster []Signup, boardChannelID string) string {
	written := []rune(ev.Description)
	if len(written) >= nativeDescriptionLimit {
		return string(written[:nativeDescriptionLimit])
	}
	block := signupBlock(ev, roster, boardChannelID, nativeDescriptionLimit-len(written))
	if len(written)+len([]rune(block)) > nativeDescriptionLimit {
		return ev.Description
	}
	return ev.Description + block
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

// finishedBy reports the instant an event stops being current. Every event
// has a start — CreateEvent refuses one without — so there is always an
// answer.
func finishedBy(ev *Event) int64 {
	if ev.EndsAt > 0 {
		return ev.EndsAt
	}
	return ev.StartsAt + assumedRunTimeWithoutEndTime
}

// CompleteFinishedEvents moves events whose time has passed to completed.
//
// Only touches open and closed events. A cancelled one is already archived and
// must not be relabelled as having happened — it did not.
//
// Recurring events are not completed here: a rule means the event comes round
// again, so an ended occurrence rolls the row forward instead — see
// FinishedRecurringOccurrences and RollOverOccurrence.
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
		if finishedBy(ev) < cutoff {
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
// RenderForumCard sends an empty components array for any non-open event,
// which removes the buttons rather than leaving the old ones in place.
func (s *Server) CompleteFinishedEvents() ([]int64, error) {
	finished, err := s.store.CompleteFinishedEvents()
	if err != nil {
		return nil, err
	}
	for _, id := range finished {
		s.finishEventEverywhere(id)
	}
	// The same tick moves recurring events on to their next date. Reported
	// separately in the log and not in the count: they did not finish.
	if _, err := s.rollOverFinishedOccurrences(); err != nil {
		log.Printf("[discord-signup] roll over finished occurrences: %v", err)
	}
	return finished, nil
}

// finishEventEverywhere settles a finished event on every surface: the card
// loses its buttons and moves to past events, the tables drop the row, the
// forum post gets its finished tag, the thread closes.
//
// Its own function because finishing happens two ways and both must look the
// same afterwards: the event's end time passing, and somebody pressing End on
// the native Discord event. The second used to do nothing at all until the
// first caught up.
func (s *Server) finishEventEverywhere(id int64) {
	if err := s.postPastEventLine(id); err != nil {
		log.Printf("[discord-signup] move event %d to past events: %v", id, err)
	}
	// It has left the live list, so both tables have to stop showing it and its
	// discussion closes with it.
	ev, err := s.store.GetEvent(id)
	if err != nil {
		return
	}
	s.refreshTablesQuietly(ev.GuildID)
	// The forum post gets its finished tag and archives.
	s.refreshForumPostQuietly(ev)
	if ev.ThreadID != "" {
		if err := s.discord.ArchiveThread(ev.ThreadID); err != nil {
			log.Printf("[discord-signup] archive thread for event %d: %v", id, err)
		}
	}
}

// PushEditToDiscord updates the native scheduled event linked to a local
// roster, so the two do not drift apart after an edit.
//
// Silently does nothing when there is no link, which is the common case: an
// event that was never published has nothing to push to. A failure is returned
// rather than swallowed, but callers treat it as non-fatal — the roster is the
// source of truth and the native event is a copy of it.
func (s *Server) PushEditToDiscord(ev *Event, roster []Signup, rename bool) error {
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
		"description": nativeEventDescription(ev, roster, s.guildChannels(ev.GuildID).Board),
	}
	// A location is an EXTERNAL event's; a voice or stage event lives in its
	// channel and Discord refuses entity_metadata on it outright
	// (GUILD_SCHEDULED_EVENT_ENTITY_METADATA_UNSUPPORTED, measured 2026-09-04
	// on two imported voice events, which then failed every publish). Events
	// this service creates are external and carry no entity type of their
	// own, so "" counts as external.
	if ev.EntityType == "" || ev.EntityType == "external" {
		payload["entity_metadata"] = map[string]any{"location": location}
	}
	// The times go only while the event is still ahead. Discord refuses a
	// start in the past (GUILD_SCHEDULED_EVENT_SCHEDULE_PAST) and any start on
	// an event that is running (…INVALID_START_BY_STATUS), whether or not the
	// value changed — measured 2026-09-04, when every roster change on three
	// running events failed the whole PATCH and the sweep retried each of them
	// every minute. Once it has begun there is nothing about its time left to
	// push: the description and the count are what still move.
	if ev.StartsAt > now() {
		payload["scheduled_start_time"] = time.Unix(ev.StartsAt, 0).UTC().Format(time.RFC3339)
		payload["scheduled_end_time"] = time.Unix(endsAt, 0).UTC().Format(time.RFC3339)
	}
	// The name is a rename and renames are throttled; the description carries
	// the live count and names and is not, so it goes every time.
	if rename {
		payload["name"] = nativeEventName(ev)
	}
	// The rule goes every time too — null clears it, which is how "never"
	// reaches Discord. A rule Discord cannot express is left out rather than
	// sent and refused, which would lose the whole edit.
	if rule, ok := discordRecurrenceRule(ev, s.DefaultTimezone()); ok {
		payload["recurrence_rule"] = rule
	} else {
		log.Printf("[discord-signup] event %d: rule %q cannot be expressed to Discord; pushed without it", ev.ID, ev.RecurrenceRule)
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
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/scheduled-events/"+escapePathSegment(eventID), nil)
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
	_, err := c.do(http.MethodDelete, "/guilds/"+escapePathSegment(guildID)+"/scheduled-events/"+escapePathSegment(eventID), nil)
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
func (s *Server) reconcileWithNative(guildID string, remote []DiscordScheduledEvent) (published, cancelled, finished int, problems []string) {
	remoteByID := map[string]DiscordScheduledEvent{}
	for _, r := range remote {
		remoteByID[r.ID] = r
	}
	events, err := s.store.ListEvents(guildID, "", 200)
	if err != nil {
		return 0, 0, 0, []string{"list events: " + err.Error()}
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
			if _, err := s.PublishToDiscord(ev.ID); err != nil {
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
			// Absent from the list is ambiguous: Discord drops COMPLETED events
			// from it as well as deleted ones, which is why this asks about the
			// event directly rather than reading anything into the silence.
			if exists && native.Status == discordEventCompleted {
				// Somebody pressed End on the native event. That is an explicit
				// act and it means now, not at the scheduled end time — this
				// used to `continue` and leave it to the time sweep, so an
				// event ended early stayed on the board, kept its Join button
				// and never reached past events until its original end passed.
				completed := StatusCompleted
				if _, err := s.store.UpdateEvent(ev.ID, EventPatch{Status: &completed}); err != nil {
					problems = append(problems, fmt.Sprintf("complete %q: %v", ev.Name, err))
					continue
				}
				s.finishEventEverywhere(ev.ID)
				finished++
				continue
			}
			if exists && native.Status != discordEventCanceled {
				continue // still scheduled or running; the time sweep owns those
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
	return published, cancelled, finished, problems
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
	s.refreshTablesQuietly(ev.GuildID)
	s.refreshForumPostQuietly(updated)
	if updated.ThreadID != "" {
		if err := s.discord.ArchiveThread(updated.ThreadID); err != nil {
			log.Printf("[discord-signup] archive thread for cancelled %d: %v", ev.ID, err)
		}
	}
	return nil
}
