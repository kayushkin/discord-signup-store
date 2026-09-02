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
	"time"
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

var recurrenceChoices = []recurrenceChoice{
	{"Every day", "FREQ=DAILY"},
	{"Every week", "FREQ=WEEKLY"},
	{"Every other week", "FREQ=WEEKLY;INTERVAL=2"},
	{"Every weekday", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"},
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

	Roster  []Signup
	History []Transition

	CanManage        bool
	DiscordEventURL  string
	ManageableGuilds []Guild
	Roles            []Role

	StartsLocal       string
	EndsLocal         string
	TimezoneValue     string
	RecurrenceValue   string
	RecurrenceChoices []recurrenceChoice
	Zones             []string
	Statuses          []string
}

var templates = template.Must(template.New("").Funcs(template.FuncMap{
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
	"localTime": func(unix int64) template.HTML {
		if unix == 0 {
			return template.HTML("—")
		}
		t := time.Unix(unix, 0).UTC()
		return template.HTML(fmt.Sprintf(`<time class="ts" datetime="%s">%s</time>`,
			t.Format(time.RFC3339), t.Format("Mon 2 Jan 2006, 15:04")+" UTC"))
	},
	// transitionText drops the resulting state when the action already names
	// it, so "waitlisted → waitlisted" reads as "waitlisted". Where the two
	// differ the arrow carries real information and stays.
	"transitionText": func(action, toState string) string {
		if action == toState {
			return action
		}
		return action + " → " + toState
	},
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
	data.Statuses = ValidStatuses()
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
	var visible []Event
	for guildID := range session.GuildPermissions {
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
	guilds, err := s.manageableGuilds(session)
	if err != nil {
		data.Error = err.Error()
	}
	data.ManageableGuilds = guilds
	if len(guilds) == 0 && data.Error == "" {
		data.Error = "You do not have Manage Events in any server this bot is in."
	}
	if len(guilds) > 0 {
		data.Roles = s.assignableRolesIn(guilds[0].ID)
	}
	s.render(w, "form.html", data)
}

// manageableGuilds intersects the servers the bot is in with the ones the
// caller may manage events in. Both halves matter: the bot cannot post to a
// server it is not in, and the user must not create rosters where they have no
// standing.
func (s *Server) manageableGuilds(session *WebSession) ([]Guild, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	botGuilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return nil, fmt.Errorf("list bot guilds: %w", err)
	}
	var out []Guild
	for _, g := range botGuilds {
		if session.CanManageEventsIn(g.ID) {
			out = append(out, g)
		}
	}
	return out, nil
}

// assignableRolesIn returns only roles the bot can actually grant. Offering one
// it cannot would produce a 403 at the first signup, long after the choice.
func (s *Server) assignableRolesIn(guildID string) []Role {
	if s.discord == nil {
		return nil
	}
	roles, err := s.discord.ListGuildRoles(guildID)
	if err != nil {
		log.Printf("[discord-signup] list roles for %s: %v", guildID, err)
		return nil
	}
	botRoles, err := s.discord.GuildMemberRoleIDs(guildID, s.applicationUserID())
	if err != nil {
		log.Printf("[discord-signup] read bot roles in %s: %v", guildID, err)
		return nil
	}
	return AssignableRoles(roles, botRoles)
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
	if !session.CanManageEventsIn(guildID) {
		http.Error(w, "you do not have Manage Events in that server", http.StatusForbidden)
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

	ev, err := s.createEventAndJoinOrganiser(Event{
		GuildID:         guildID,
		ChannelID:       s.boardChannelID,
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
	guilds, _ := s.manageableGuilds(session)
	s.render(w, "form.html", pageData{
		Title: "New event", Session: session, Event: ev,
		Error: err.Error(), ManageableGuilds: guilds,
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
