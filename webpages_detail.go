package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// webEvent loads an event and the caller's standing on it, or writes the
// response and returns nil.
func (s *Server) webEvent(w http.ResponseWriter, r *http.Request, session *WebSession) (*Event, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "id must be an integer", http.StatusBadRequest)
		return nil, false
	}
	ev, err := s.store.GetEvent(id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		log.Printf("[discord-signup] load event %d: %v", id, err)
		http.Error(w, "could not load that event", http.StatusInternalServerError)
		return nil, false
	}
	// Membership is the read gate. Someone who is not in the server has no
	// business seeing who signed up for its events.
	if viewable, err := s.mayViewGuild(session, ev.GuildID); err != nil || !viewable {
		http.Error(w, "that event is in a server you are not in", http.StatusForbidden)
		return nil, false
	}
	canManage, err := s.mayEditEvent(session.editActor(ev.GuildID), ev)
	if err != nil {
		log.Printf("[discord-signup] check edit rights on event %d for %s: %v", ev.ID, session.DiscordUserID, err)
		http.Error(w, "could not check whether you may edit this event: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	return ev, canManage
}

// handleWebEventDetail shows one event: its fields — editable, for whoever
// may edit it — its roster, the tools for adding people, and its log.
func (s *Server) handleWebEventDetail(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	s.renderEventPage(w, session, ev, canManage, nil, r.URL.Query().Get("notice"), "")
}

// eventFormValues are the edit fields as the page shows them: text, because
// a form that failed to save shows back what was typed, not what was stored.
type eventFormValues struct {
	Name, Description, StartsAt, EndsAt, Timezone, Capacity, Location string
	RecurrenceRule, AttendingRoleID, WaitlistRoleID                   string
	WaitlistDisabled                                                  bool
}

func eventFormFromEvent(ev *Event) eventFormValues {
	zone := ev.Timezone
	if zone == "" {
		zone = "UTC"
	}
	return eventFormValues{
		Name: ev.Name, Description: ev.Description,
		StartsAt: FormatEventTime(ev.StartsAt, zone), EndsAt: FormatEventTime(ev.EndsAt, zone),
		Timezone: zone, Capacity: strconv.Itoa(ev.Capacity), Location: ev.Location,
		RecurrenceRule: ev.RecurrenceRule, AttendingRoleID: ev.AttendingRoleID,
		WaitlistRoleID: ev.WaitlistRoleID, WaitlistDisabled: ev.WaitlistDisabled,
	}
}

func eventFormFromRequest(r *http.Request, ev *Event) eventFormValues {
	values := eventFormFromEvent(ev)
	values.Name, values.Description = r.FormValue("name"), r.FormValue("description")
	values.StartsAt, values.EndsAt = r.FormValue("starts_at"), r.FormValue("ends_at")
	values.Timezone, values.Capacity = r.FormValue("timezone"), r.FormValue("capacity")
	values.Location, values.RecurrenceRule = r.FormValue("location"), r.FormValue("recurrence_rule")
	values.AttendingRoleID, values.WaitlistRoleID = r.FormValue("attending_role_id"), r.FormValue("waitlist_role_id")
	if r.Form.Has("waitlist") {
		values.WaitlistDisabled = r.FormValue("waitlist") == "off"
	}
	return values
}

// renderEventPage draws the event page. submitted is the edit form as typed
// when saving it failed, nil otherwise.
func (s *Server) renderEventPage(w http.ResponseWriter, session *WebSession, ev *Event, canManage bool,
	submitted *eventFormValues, notice, errorText string) {
	// Before reading the lists, so they render names rather than snowflakes.
	s.backfillDisplayNames(ev)
	var problems []string
	if errorText != "" {
		problems = append(problems, errorText)
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the roster: "+err.Error())
	}
	signupUpdates, err := s.store.History(ev.ID, 1000)
	if err != nil {
		log.Printf("[discord-signup] history %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the signup history: "+err.Error())
	}
	edits, err := s.store.EventUpdates(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] event updates %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the edit history: "+err.Error())
	}
	invites, err := s.store.Invites(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] invites %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the invites: "+err.Error())
	}

	actors := []string{}
	editsNameRoles := false
	for _, u := range signupUpdates {
		actors = append(actors, u.Actor)
	}
	for _, u := range edits {
		actors = append(actors, u.Actor)
		switch u.Field {
		case "created_by":
			// A host is recorded as a bare id, which names like an actor.
			actors = append(actors, u.FromValue, u.ToValue)
		case "attending_role_id", "waitlist_role_id":
			editsNameRoles = true
		}
	}
	for _, inv := range invites {
		actors = append(actors, inv.InvitedBy)
	}
	names := eventLogNames{actors: s.historyActorNames(ev.GuildID, actors), people: map[string]actorName{}, roles: map[string]string{}}
	for actor, name := range names.actors {
		if snowflake.MatchString(actor) {
			names.people[actor] = name
		}
	}
	if editsNameRoles && s.discord != nil {
		// Every role, not only the ones the bot can grant: a role in the log
		// may be one it could grant once.
		if roles, err := s.discord.ListGuildRoles(ev.GuildID); err != nil {
			log.Printf("[discord-signup] name roles in %s: %v", ev.GuildID, err)
		} else {
			for _, role := range roles {
				names.roles[role.ID] = role.Name
			}
		}
	}

	data := pageData{
		Title: ev.Name, Session: session, Event: ev, Roster: roster, Invites: invites,
		EventLog:        buildEventLog(signupUpdates, edits, invites, names),
		CanManage:       canManage,
		EventUnderway:   eventIsUnderway(ev),
		EventFull:       eventIsFull(ev),
		DiscordEventURL: DiscordEventURL(ev.GuildID, ev.DiscordScheduledEventID),
		Notice:          notice,
		Error:           strings.Join(problems, " "),
	}
	if canManage {
		data.Roles = s.assignableRolesIn(ev.GuildID)
		data.Form = eventFormFromEvent(ev)
		if submitted != nil {
			data.Form = *submitted
		}
	}
	s.render(w, "detail.html", data)
}

// handleWebEditForm is where the edit form used to be. The fields are on the
// event page now; an old link or bookmark lands there.
func (s *Server) handleWebEditForm(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/events/"+r.PathValue("id"), http.StatusMovedPermanently)
}

// handleWebUpdateEvent saves the event page's fields.
func (s *Server) handleWebUpdateEvent(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	submitted := eventFormFromRequest(r, ev)
	failed := func(err error) {
		s.renderEventPage(w, session, ev, true, &submitted, "", "Not saved: "+plainError(err))
	}
	zone := strings.TrimSpace(r.FormValue("timezone"))
	starts, err := ParseEventTime(r.FormValue("starts_at"), zone)
	if err != nil {
		failed(err)
		return
	}
	ends, err := ParseEventTime(r.FormValue("ends_at"), zone)
	if err != nil {
		failed(err)
		return
	}
	if err := requireStartTime(starts); err != nil {
		failed(err)
		return
	}
	capacity := 0
	if text := strings.TrimSpace(r.FormValue("capacity")); text != "" {
		if capacity, err = strconv.Atoi(text); err != nil {
			failed(fmt.Errorf("%w: the limit must be a whole number, or 0 for no limit", ErrInvalidEvent))
			return
		}
	}
	patch := EventPatch{
		Name:            strPtr(r.FormValue("name")),
		Description:     strPtr(r.FormValue("description")),
		Capacity:        &capacity,
		StartsAt:        &starts,
		EndsAt:          &ends,
		Location:        strPtr(r.FormValue("location")),
		RecurrenceRule:  strPtr(r.FormValue("recurrence_rule")),
		Timezone:        strPtr(zone),
		AttendingRoleID: strPtr(r.FormValue("attending_role_id")),
		WaitlistRoleID:  strPtr(r.FormValue("waitlist_role_id")),
	}
	// Status has its own buttons on the page; a form that still sends it —
	// an old tab — is honoured.
	if r.Form.Has("status") {
		patch.Status = strPtr(r.FormValue("status"))
	}
	if r.Form.Has("waitlist") {
		patch.WaitlistDisabled = &submitted.WaitlistDisabled
	}
	// Raising the limit here does exactly what raising it from Discord does,
	// because it is now the same function rather than a second copy of the
	// rule. The copy is what went wrong: it promoted people and redrew the
	// card, and never pushed the native scheduled event, so a name or a limit
	// changed on this page left Discord's own title stale until the next
	// signup happened to push it.
	_, promoted, err := s.applyEventEdit(ev, patch, "web:"+session.DiscordUserID)
	if err != nil {
		failed(err)
		return
	}
	notice := "Saved."
	if len(promoted) > 0 {
		notice = fmt.Sprintf("Saved. %d came off the waitlist and have been messaged.",
			len(promoted))
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// handleWebToggleSignups is the page's Open signups / Close signups button,
// the same toggle as the management row's.
func (s *Server) handleWebToggleSignups(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	said, err := s.toggleSignups(ev, "web:"+session.DiscordUserID)
	if errors.Is(err, errSignupsNotToggleable) {
		s.redirectWithNotice(w, r, ev.ID, "It is "+ev.Status+", so there are no signups to open or close.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web toggle signups for event %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, ev.ID, strings.ReplaceAll(said, "**", ""))
}

// handleWebCancelEvent cancels, as the management row's Cancel does: the
// name typed back is the confirm, and the native Discord event is deleted.
func (s *Server) handleWebCancelEvent(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	if IsArchived(ev.Status) {
		s.redirectWithNotice(w, r, ev.ID, "It is already "+ev.Status+".")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm_name")), strings.TrimSpace(ev.Name)) {
		s.redirectWithNotice(w, r, ev.ID, "That did not match the event's name — nothing was cancelled.")
		return
	}
	actor := "web:" + session.DiscordUserID
	if err := s.cancelEventEverywhere(ev, "cancelled by "+actor); err != nil {
		log.Printf("[discord-signup] web cancel event %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "It is NOT cancelled: "+err.Error())
		return
	}
	// cancelEventEverywhere writes the status straight to the store, as the
	// Discord confirm notes, so the person who did it is logged here.
	if after, err := s.store.GetEvent(ev.ID); err == nil {
		if err := s.store.LogEventUpdates(ev, after, actor); err != nil {
			log.Printf("[discord-signup] log cancel of event %d: %v", ev.ID, err)
		}
	}
	s.redirectWithNotice(w, r, ev.ID, "Cancelled. Its Discord event is gone and nobody can join.")
}

// handleWebRosterPromote gives someone on the waitlist or the Maybe list a
// place, even past the limit — the organiser's call — and tells them.
func (s *Server) handleWebRosterPromote(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	userID := r.FormValue("discord_user_id")
	promoted, from, err := s.store.GiveAPlace(ev.ID, userID, "web:"+session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		s.redirectWithNotice(w, r, ev.ID, "They are not on the waitlist or the Maybe list.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web promote %s on %d: %v", userID, ev.ID, err)
		http.Error(w, "could not promote them", http.StatusInternalServerError)
		return
	}
	s.inBackground(func() { s.notifyGivenAPlace(ev, promoted) })
	s.inBackground(func() {
		s.syncAfterChange(ev.ID, []stateChange{{UserID: userID, State: StateAttending}})
	})
	notice := fmt.Sprintf("%s is going now (was %s), and was messaged.", promoted.NameOnDiscord(), from)
	if after, err := s.store.GetEvent(ev.ID); err == nil && after.Capacity > 0 && after.AttendingCount > after.Capacity {
		notice += fmt.Sprintf(" That takes it to %d/%d, over the limit.", after.AttendingCount, after.Capacity)
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// handleWebRosterRemove takes someone off, promoting whoever is next.
func (s *Server) handleWebRosterRemove(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	userID := r.FormValue("discord_user_id")
	// The actor is the person who did it, by Discord id, so the history can
	// answer "who removed them" rather than a flat "operator".
	result, err := s.store.Leave(ev.ID, userID, "web:"+session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		s.redirectWithNotice(w, r, ev.ID, "They were not on the roster.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web remove %s from %d: %v", userID, ev.ID, err)
		http.Error(w, "could not remove them", http.StatusInternalServerError)
		return
	}
	changes := []stateChange{{UserID: userID, State: StateWithdrawn}}
	notice := "Removed."
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
		notice = "Removed. The next person on the waitlist moved up and was messaged."
		s.inBackground(func() { s.notifyPromoted(ev, result.Promoted) })
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, changes) })
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// memberSearchLimit is how many people the add box offers at once. Enough to
// find anyone by the first two or three letters of their name, few enough to
// read.
const memberSearchLimit = 10

// memberSuggestion is one line in the add box's list.
type memberSuggestion struct {
	MemberMatch
	// ReadableName is the short name set for them, if any.
	ReadableName string `json:"readable_name,omitempty"`
	// OnRoster is their current state on this event — attending or
	// waitlisted — or empty when adding them would be new.
	OnRoster string `json:"on_roster,omitempty"`
}

// handleWebMemberSearch answers the add box as someone types: the server's
// members whose name starts with what they typed. The picked person travels
// back to roster/add as their Discord user id, never as the name — two
// people in one server can share a display name.
func (s *Server) handleWebMemberSearch(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"members": []memberSuggestion{}})
		return
	}
	if s.discord == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Discord client configured"})
		return
	}
	matches, err := s.discord.SearchGuildMembers(ev.GuildID, query, memberSearchLimit)
	if err != nil {
		log.Printf("[discord-signup] member search in %s for %q: %v", ev.GuildID, query, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Discord member search failed: " + err.Error()})
		return
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	state := map[string]string{}
	for _, sg := range roster {
		state[sg.DiscordUserID] = sg.State
	}
	named, err := s.store.ReadableNames()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	readable := map[string]string{}
	for _, n := range named {
		readable[n.DiscordUserID] = n.ReadableName
	}
	out := make([]memberSuggestion, 0, len(matches))
	for _, m := range matches {
		out = append(out, memberSuggestion{MemberMatch: m, OnRoster: state[m.UserID], ReadableName: readable[m.UserID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// handleWebRosterAdd puts someone on the list the organiser picks — going,
// maybe or the waitlist — by their Discord user id, and tells them. Going
// can take the event past its limit; that is the organiser's call. The id
// comes from the name picked in the box, or is pasted in directly.
//
// The id is checked against the server before anyone is added: a typo in a
// pasted id used to put a stranger's snowflake on the roster, where it sat as
// a bare number nobody could name. The same lookup gives the name the roster
// shows.
func (s *Server) handleWebRosterAdd(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	if userID == "" {
		s.redirectWithNotice(w, r, ev.ID, "Nobody was added: pick a person from the list under the box.")
		return
	}
	displayName := ""
	if s.discord != nil {
		name, err := s.discord.GuildMemberDisplayName(ev.GuildID, userID)
		if err != nil {
			s.redirectWithNotice(w, r, ev.ID, "Nobody was added: "+userID+" is not a member of this server ("+err.Error()+").")
			return
		}
		displayName = name
	}
	result, err := s.store.PlaceOnList(ev.ID, userID, displayName, r.FormValue("list"), "web:"+session.DiscordUserID)
	switch {
	case errors.Is(err, ErrEventNotOpen):
		s.redirectWithNotice(w, r, ev.ID, "Nobody was added: the event is "+ev.Status+".")
		return
	case errors.Is(err, ErrInvalidEvent):
		s.redirectWithNotice(w, r, ev.ID, "Nobody was added: "+plainError(err))
		return
	case err != nil:
		log.Printf("[discord-signup] web add %s to %d: %v", userID, ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Could not add them: "+err.Error())
		return
	}
	// Without a Discord client there is no name to say; only tests run so.
	who := "They"
	if displayName != "" {
		who = displayName
	}
	if result.Unchanged {
		s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("%s is already %s — no change.", who, stateWords(result.Signup)))
		return
	}
	changes := []stateChange{{UserID: userID, State: result.Signup.State}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, changes) })
	s.inBackground(func() { s.notifyPlacedOnList(ev, &result.Signup) })
	notice := fmt.Sprintf("%s is %s now, and was messaged.", who, stateWords(result.Signup))
	if result.Promoted != nil {
		s.inBackground(func() { s.notifyPromoted(ev, result.Promoted) })
		notice += " Their place went to the next person on the waitlist, who was messaged too."
	}
	if after, err := s.store.GetEvent(ev.ID); err == nil && after.Capacity > 0 && after.AttendingCount > after.Capacity {
		notice += fmt.Sprintf(" That takes it to %d/%d, over the limit.", after.AttendingCount, after.Capacity)
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// stateWords says where someone is, as the page says it.
func stateWords(sg Signup) string {
	switch sg.State {
	case StateAttending:
		return "going"
	case StateWaitlisted:
		return fmt.Sprintf("on the waitlist, at number %d", sg.WaitlistPlace)
	case StateMaybe:
		return "down as maybe"
	}
	return sg.State
}

// handleWebPublish creates a native Discord scheduled event for this roster.
func (s *Server) handleWebPublish(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	if _, err := s.PublishToDiscord(ev.ID); err != nil {
		s.redirectWithNotice(w, r, ev.ID, "Could not publish it: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, ev.ID,
		"Published. The Discord event points back here and says that pressing Interested does not hold a place.")
}

func (s *Server) redirectWithNotice(w http.ResponseWriter, r *http.Request, eventID int64, notice string) {
	http.Redirect(w, r, fmt.Sprintf("/events/%d?%s", eventID, noticeQuery(notice)),
		http.StatusSeeOther)
}

// noticeQuery renders a notice as a complete, correctly encoded query string.
// The notice is not always ours: it carries event names, people's names and
// upstream error text, so it can hold any byte at all. Encoding it with
// url.Values is what keeps the message the reader gets identical to the
// message we sent — a hand-rolled replacement of the characters someone thought
// of loses the notice to a percent sign, truncates it at a hash, and drops it
// entirely at a "; ".
func noticeQuery(notice string) string {
	return url.Values{"notice": {notice}}.Encode()
}

func strPtr(s string) *string { return &s }

// backfillDisplayNames fills in names for anyone on the roster who was recorded
// as a bare id — added through the API, or seen over the gateway when the
// member lookup failed.
//
// Runs on the detail page rather than as a sweep because that is where the ids
// are actually read, and it writes what it finds, so any given person costs one
// Discord call once and never again. A failure is logged and skipped: a roster
// showing a snowflake is worse than one showing a name, and far better than a
// page that will not load because Discord is slow.
func (s *Server) backfillDisplayNames(ev *Event) {
	if s.discord == nil {
		return
	}
	missing, err := s.store.UserIDsMissingDisplayName(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] find missing names for %d: %v", ev.ID, err)
		return
	}
	for _, userID := range missing {
		name, err := s.discord.GuildMemberDisplayName(ev.GuildID, userID)
		if err != nil {
			// Most often this is someone who has left the server. Their id is
			// all that is left of them and the history must still show it.
			log.Printf("[discord-signup] no member record for %s in %s: %v", userID, ev.GuildID, err)
			continue
		}
		if err := s.store.SetDisplayName(ev.ID, userID, name); err != nil {
			log.Printf("[discord-signup] store display name for %s: %v", userID, err)
		}
	}
}
