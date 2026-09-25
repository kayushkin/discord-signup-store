package discordsignup

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed templates/*.html
var templateFiles embed.FS

// commonZones seeds the timezone picker. Free text is still accepted — this is
// a shortcut, not an allowlist, and the real check is time.LoadLocation.
var commonZones = []string{
	"America/Los_Angeles", "America/Denver", "America/Chicago", "America/New_York",
	"Europe/London", "Europe/Dublin", "Europe/Paris", "Europe/Berlin", "Europe/Moscow",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Singapore", "Asia/Tokyo",
	"Australia/Sydney", "Pacific/Auckland", "UTC",
}

// recurrenceChoice is one option in the Repeats dropdown.
//
// A fixed list rather than a free-text RRULE box: typing RFC 5545 by hand is
// how you end up with BYDAY=3TU when you meant BYDAY=TU;BYSETPOS=3, which read
// alike and diverge in months that start on a Tuesday.
type recurrenceChoice struct {
	Label string
	Rule  string
}

// Exactly the shapes Discord's own form can make, so every choice here can
// be sent, described and rolled. The day, week and date come from the start.
var recurrenceChoices = []recurrenceChoice{
	{"Every day", "FREQ=DAILY"},
	{"Every weekday", "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR"},
	{"Every week", "FREQ=WEEKLY"},
	{"Every other week", "FREQ=WEEKLY;INTERVAL=2"},
	{"Every month", "FREQ=MONTHLY"},
	{"Every year", "FREQ=YEARLY"},
}

// pageData is what every template gets. One struct rather than per-page ones so
// the layout can always reach Session, Error and Notice.
type pageData struct {
	Title   string
	Session *WebSession
	Error   string
	Notice  string

	Events []Event
	// Archived is the collapsed tail: events that are over. Split here rather
	// than filtered in the template so the counts in the summary are right and
	// the two lists can be sorted differently — soonest-first for what is
	// coming, most-recent-first for what is done.
	Archived []Event
	Event    *Event

	Roster []Signup
	// Going, Waiting and Maybes are the roster split into its three lists,
	// each in its own order, for the event page.
	Going, Waiting, Maybes []Signup
	Invites                []EventInvite
	// EventLog is the event page's log: signups, edits and invites, newest
	// first.
	EventLog []eventLogEntry
	// Form is the event page's edit fields.
	Form eventFormValues
	// PlaceFreesOnLeave is whether someone going leaving would bring in the
	// next person waiting: somebody is waiting, and the event is not over
	// its limit. The page words its questions by it.
	PlaceFreesOnLeave bool
	// EventFull is whether a capped event has no free place, which is when
	// the waitlist can be added to.
	EventFull bool
	// MayName is whether the viewer may open the names page.
	MayName bool

	CanManage bool
	// EventUnderway offers End on the detail page: started, not yet over.
	EventUnderway        bool
	DiscordEventURL      string
	GuildsWhereMayCreate []Guild
	// HomeGuildChoices are the servers the home page can be narrowed to, and
	// HomeGuildID the one chosen ("" for all).
	HomeGuildChoices []Guild
	HomeGuildID      string
	// NameableGuilds are the servers whose members the viewer may name.
	NameableGuilds []Guild
	// NamePeople is the names page's rows.
	NamePeople []namedPerson

	StartsLocal       string
	EndsLocal         string
	TimezoneValue     string
	RecurrenceValue   string
	RecurrenceChoices []recurrenceChoice
	Zones             []string
}

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	// add turns a zero-based range index into a place in line. The roster comes
	// back in arrival order, so the number IS the row's position and does not
	// need storing beside it.
	"add": func(a, b int) int { return a + b },
	// repeats is the rule in the words the forms use, so a page says "weekly"
	// rather than FREQ=WEEKLY;BYDAY=TU.
	"repeats": describeRepeat,
	// localTime emits the instant and lets the browser format it.
	//
	// The server does not know the reader's timezone and must not guess: an
	// Accept-Language header does not carry one, and the host's own zone is
	// nobody's but the host's. So the markup carries an unambiguous RFC 3339
	// instant and a script rewrites it with Intl.DateTimeFormat, which uses the
	// reader's actual zone.
	//
	// The text inside the element is the no-JavaScript fallback and says UTC
	// out loud. A time shown without its zone is the bug being fixed here, so
	// the degraded path must not reintroduce it.
	"localTime": localTimeHTML,
	// person shows someone by their short name with their Discord name
	// behind it.
	"person": personHTML,
	// toggleSubject is what the Open/Close button opens or closes: the
	// waitlist, on a full event that has one, or signups.
	"toggleSubject": closeToggleSubject,
	// isArchived lets a template dim a card without restating which statuses
	// count as over — that answer lives in vocabulary.go and nowhere else.
	"isArchived": IsArchived,
	// who renders a person the way the server shows them, falling back to the
	// raw id only when there is genuinely no name — someone who left the guild,
	// so Discord has no member record left to ask about.
	"who": func(displayName, userID string) string {
		if displayName != "" {
			return displayName
		}
		return userID
	},
}).ParseFS(templateFiles, "templates/*.html"))

// splitByArchived divides events into what is coming and what is over.
//
// Live events sort soonest-first, because the next one is the one being looked
// for. Archived sort most-recent-first, for the same reason in reverse. Events
// with no start time sort last among the live rather than first: a missing date
// is unknown, not imminent.
func splitByArchived(events []Event) (live, archived []Event) {
	for _, ev := range events {
		if IsArchived(ev.Status) {
			archived = append(archived, ev)
			continue
		}
		live = append(live, ev)
	}
	sort.SliceStable(live, func(i, j int) bool {
		if (live[i].StartsAt == 0) != (live[j].StartsAt == 0) {
			return live[j].StartsAt == 0
		}
		return live[i].StartsAt < live[j].StartsAt
	})
	sort.SliceStable(archived, func(i, j int) bool {
		return archived[i].StartsAt > archived[j].StartsAt
	})
	return live, archived
}

func (s *Server) render(w http.ResponseWriter, page string, data pageData) {
	data.Zones = commonZones
	data.RecurrenceChoices = recurrenceChoices
	tmpl, err := templates.Clone()
	if err != nil {
		log.Printf("[discord-signup] clone templates: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if _, err := tmpl.ParseFS(templateFiles, "templates/"+page); err != nil {
		log.Printf("[discord-signup] parse %s: %v", page, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// The response is already partly written, so this cannot become an
		// error page. Log it rather than serve half a page silently.
		log.Printf("[discord-signup] render %s: %v", page, err)
	}
}

// requireSession is the gate on every browser route. Anonymous callers are sent
// to log in and returned to where they were going.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) *WebSession {
	session := s.sessionFrom(r)
	if session == nil {
		http.Redirect(w, r, "/login?next="+template.URLQueryEscaper(r.URL.Path), http.StatusFound)
		return nil
	}
	return session
}

// handleIndex lists every roster in servers the caller belongs to.
func (s *Server) handleWebIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	session := s.sessionFrom(r)
	data := pageData{Title: "Events", Session: session, Notice: r.URL.Query().Get("notice")}
	if session == nil {
		s.render(w, "index.html", data)
		return
	}
	guildIDs := map[string]bool{}
	for guildID := range session.GuildPermissions {
		guildIDs[guildID] = true
	}
	// A site admin sees every server the bot is in, member or not.
	if admin, err := s.store.IsSiteAdmin(session.DiscordUserID); err != nil {
		data.Error = err.Error()
	} else if admin && s.discord != nil {
		botGuilds, err := s.discord.ListBotGuilds()
		if err != nil {
			data.Error = "list the bot's servers: " + err.Error()
		}
		for _, g := range botGuilds {
			guildIDs[g.ID] = true
		}
	}
	// The saved filter narrows to one server, if it is still one they may see.
	home, err := s.store.HomeGuildOf(session.DiscordUserID)
	if err != nil {
		data.Error = err.Error()
	}
	if home != "" && guildIDs[home] {
		guildIDs = map[string]bool{home: true}
		data.HomeGuildID = home
	}
	if choices, err := s.viewableGuilds(session); err != nil {
		log.Printf("[discord-signup] home page server choices for %s: %v", session.DiscordUserID, err)
	} else {
		data.HomeGuildChoices = choices
	}
	if guilds, err := s.guildsWhereMayName(session); err != nil {
		log.Printf("[discord-signup] may %s open the names page: %v", session.DiscordUserID, err)
	} else {
		data.MayName = len(guilds) > 0
	}
	var visible []Event
	for guildID := range guildIDs {
		events, err := s.store.ListEvents(guildID, "", 200)
		if err != nil {
			data.Error = err.Error()
			break
		}
		visible = append(visible, events...)
	}
	data.Events, data.Archived = splitByArchived(visible)
	s.render(w, "index.html", data)
}

// handleNewEventForm shows the create form.
func (s *Server) handleWebNewEventForm(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	data := pageData{Title: "New event", Session: session, TimezoneValue: "UTC"}
	guilds, err := s.guildsWhereMayCreate(session)
	if err != nil {
		data.Error = err.Error()
	}
	data.GuildsWhereMayCreate = guilds
	if len(guilds) == 0 && data.Error == "" {
		data.Error = "You may not create events in any server this bot is in."
	}
	s.render(w, "form.html", data)
}

// guildsWhere intersects the servers the bot is in with the ones the caller
// belongs to and passes allowed in. Both halves matter: the bot cannot post
// to a server it is not in, and the user must have standing in it.
func (s *Server) guildsWhere(session *WebSession, allowed func(editActor) (bool, error)) ([]Guild, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	botGuilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return nil, fmt.Errorf("list bot guilds: %w", err)
	}
	var out []Guild
	for _, g := range botGuilds {
		viewable, err := s.mayViewGuild(session, g.ID)
		if err != nil {
			return nil, err
		}
		if !viewable {
			continue
		}
		ok, err := allowed(session.editActor(g.ID))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", g.Name, err)
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// guildsWhereMayCreate is where the caller may create events.
func (s *Server) guildsWhereMayCreate(session *WebSession) ([]Guild, error) {
	return s.guildsWhere(session, s.mayCreateEventsIn)
}

// guildsWhereMayEditAll is where the caller may edit every event — the
// standing needed to pull a server's events in from Discord.
func (s *Server) guildsWhereMayEditAll(session *WebSession) ([]Guild, error) {
	return s.guildsWhere(session, s.mayEditAllEventsIn)
}

// applicationUserID is the bot's own user id, cached after the first successful
// lookup and retried after a failed one. Empty means it is still unknown.
//
// Only a success is cached. A transient /users/@me failure — a 500, a timeout,
// a bot token auth-store has not resolved yet at boot — must not become the
// answer for the rest of the process's life, because every caller of this reads
// an empty id as a fact about Discord rather than as a lookup that never
// happened. The lock is held across the request so concurrent callers share one
// lookup instead of each firing their own; the gateway asks for this on every
// reaction it sees. Same shape as DiscordClient.token, for the same reason.
func (s *Server) applicationUserID() string {
	s.botIDMu.Lock()
	defer s.botIDMu.Unlock()
	if s.botID != "" {
		return s.botID
	}
	if s.discord == nil {
		return ""
	}
	id, err := s.discord.CurrentUserID()
	if err != nil {
		log.Printf("[discord-signup] read own user id: %v", err)
		return ""
	}
	s.botID = id
	return s.botID
}

// handleCreateEvent accepts the create form.
func (s *Server) handleWebCreateEvent(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	guildID := r.FormValue("guild_id")
	if viewable, err := s.mayViewGuild(session, guildID); err != nil || !viewable {
		http.Error(w, "you are not in that server", http.StatusForbidden)
		return
	}
	mayCreate, err := s.mayCreateEventsIn(session.editActor(guildID))
	if err != nil {
		http.Error(w, "could not check whether you may create events there: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !mayCreate {
		http.Error(w, "you may not create events in that server", http.StatusForbidden)
		return
	}
	zone := strings.TrimSpace(r.FormValue("timezone"))
	starts, err := ParseEventTime(r.FormValue("starts_at"), zone)
	if err != nil {
		s.renderFormError(w, session, nil, err)
		return
	}
	if err := requireStartTime(starts); err != nil {
		s.renderFormError(w, session, nil, err)
		return
	}
	ends, err := ParseEventTime(r.FormValue("ends_at"), zone)
	if err != nil {
		s.renderFormError(w, session, nil, err)
		return
	}
	capacity, _ := strconv.Atoi(r.FormValue("capacity"))

	boardChannelID := s.guildChannels(guildID).Board
	if boardChannelID == "" {
		s.renderFormError(w, session, nil, fmt.Errorf("%w: this server has no board channel set up yet", ErrInvalidEvent))
		return
	}
	ev, err := s.createEventAndJoinOrganiser(Event{
		GuildID:         guildID,
		ChannelID:       boardChannelID,
		Name:            r.FormValue("name"),
		Description:     r.FormValue("description"),
		Capacity:        capacity,
		StartsAt:        starts,
		EndsAt:          ends,
		Location:        r.FormValue("location"),
		RecurrenceRule:  r.FormValue("recurrence_rule"),
		Timezone:        zone,
		AttendingRoleID: r.FormValue("attending_role_id"),
		WaitlistRoleID:  r.FormValue("waitlist_role_id"),
		Origin:          OriginLocal,
		CreatedBy:       session.DiscordUserID,
	}, session.DisplayName)
	if err != nil {
		s.renderFormError(w, session, nil, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/events/%d", ev.ID), http.StatusSeeOther)
}

func (s *Server) renderFormError(w http.ResponseWriter, session *WebSession, ev *Event, err error) {
	guilds, _ := s.guildsWhereMayCreate(session)
	s.render(w, "form.html", pageData{
		Title: "New event", Session: session, Event: ev,
		Error: err.Error(), GuildsWhereMayCreate: guilds,
	})
}

// requireStartTime refuses an event with no start.
//
// Checked here rather than only with the form's `required` attribute, which any
// client can skip. The rule exists because an event without a start time cannot
// be published to Discord — PublishToDiscord refuses it — so allowing one to be
// created just moves the failure to the moment someone tries to use it, which
// is later and less obvious.
func requireStartTime(startsAt int64) error {
	if startsAt == 0 {
		return fmt.Errorf("%w: a start time is required — without one the event "+
			"cannot be published to Discord", ErrInvalidEvent)
	}
	return nil
}
