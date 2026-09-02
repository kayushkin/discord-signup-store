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
	if !session.IsMemberOf(ev.GuildID) {
		http.Error(w, "that event is in a server you are not in", http.StatusForbidden)
		return nil, false
	}
	return ev, session.CanManageEvent(ev)
}

// handleWebEventDetail shows one roster: the event, who is on it, and its
// history.
func (s *Server) handleWebEventDetail(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	// Before reading either list, so both render names rather than snowflakes.
	s.backfillDisplayNames(ev)

	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster %d: %v", ev.ID, err)
	}
	history, err := s.store.History(ev.ID, 200)
	if err != nil {
		log.Printf("[discord-signup] history %d: %v", ev.ID, err)
	}
	s.render(w, "detail.html", pageData{
		Title: ev.Name, Session: session, Event: ev, Roster: roster, History: history,
		CanManage:       canManage,
		DiscordEventURL: DiscordEventURL(ev.GuildID, ev.DiscordScheduledEventID),
		Notice:          r.URL.Query().Get("notice"),
	})
}

// handleWebEditForm shows the edit form for an existing roster.
func (s *Server) handleWebEditForm(w http.ResponseWriter, r *http.Request) {
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
	zone := ev.Timezone
	if zone == "" {
		zone = "UTC"
	}
	s.render(w, "form.html", pageData{
		Title: "Edit " + ev.Name, Session: session, Event: ev, CanManage: true,
		StartsLocal:     localTimeValue(ev.StartsAt, zone),
		EndsLocal:       localTimeValue(ev.EndsAt, zone),
		TimezoneValue:   zone,
		RecurrenceValue: ev.RecurrenceRule,
		Roles:           s.assignableRolesIn(ev.GuildID),
	})
}

// handleWebUpdateEvent accepts the edit form.
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
	zone := strings.TrimSpace(r.FormValue("timezone"))
	starts, err := parseLocalTime(r.FormValue("starts_at"), zone)
	if err != nil {
		s.webFormError(w, session, ev, err)
		return
	}
	ends, err := parseLocalTime(r.FormValue("ends_at"), zone)
	if err != nil {
		s.webFormError(w, session, ev, err)
		return
	}
	if err := requireStartTime(starts); err != nil {
		s.webFormError(w, session, ev, err)
		return
	}
	capacity, _ := strconv.Atoi(r.FormValue("capacity"))
	patch := EventPatch{
		Name:            strPtr(r.FormValue("name")),
		Description:     strPtr(r.FormValue("description")),
		Capacity:        &capacity,
		Status:          strPtr(r.FormValue("status")),
		StartsAt:        &starts,
		EndsAt:          &ends,
		Location:        strPtr(r.FormValue("location")),
		RecurrenceRule:  strPtr(r.FormValue("recurrence_rule")),
		Timezone:        strPtr(zone),
		AttendingRoleID: strPtr(r.FormValue("attending_role_id")),
		WaitlistRoleID:  strPtr(r.FormValue("waitlist_role_id")),
	}
	// Raising the limit here does exactly what raising it from Discord does,
	// because it is now the same function rather than a second copy of the
	// rule. The copy is what went wrong: it promoted people and redrew the
	// card, and never pushed the native scheduled event, so a name or a limit
	// changed on this page left Discord's own title stale until the next
	// signup happened to push it.
	_, promoted, err := s.applyEventEdit(ev, patch, "web:"+session.DiscordUserID)
	if err != nil {
		s.webFormError(w, session, ev, err)
		return
	}
	notice := "Saved."
	if len(promoted) > 0 {
		notice = fmt.Sprintf("Saved. %d came off the waitlist and have been messaged.",
			len(promoted))
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

func (s *Server) webFormError(w http.ResponseWriter, session *WebSession, ev *Event, err error) {
	zone := ev.Timezone
	if zone == "" {
		zone = "UTC"
	}
	s.render(w, "form.html", pageData{
		Title: "Edit " + ev.Name, Session: session, Event: ev, CanManage: true,
		Error:           err.Error(),
		StartsLocal:     localTimeValue(ev.StartsAt, zone),
		EndsLocal:       localTimeValue(ev.EndsAt, zone),
		TimezoneValue:   zone,
		RecurrenceValue: ev.RecurrenceRule,
		Roles:           s.assignableRolesIn(ev.GuildID),
	})
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
		go s.notifyPromoted(ev, result.Promoted)
	}
	go s.syncAfterChange(ev.ID, changes)
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// handleWebRosterAdd puts someone on by id, through the same rules as a click.
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
	result, err := s.store.Join(ev.ID, userID, "", JoinedViaOperator)
	if err != nil {
		s.redirectWithNotice(w, r, ev.ID, "Could not add them: "+err.Error())
		return
	}
	go s.syncAfterChange(ev.ID, []stateChange{{UserID: userID, State: result.Signup.State}})
	notice := "Added."
	if result.Signup.State == StateWaitlisted {
		notice = fmt.Sprintf("Event is full, so they went on the waitlist at number %d.",
			result.Signup.WaitlistPlace)
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}

// handleWebPostMessage posts or reposts the signup card.
func (s *Server) handleWebPostMessage(w http.ResponseWriter, r *http.Request) {
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
	if _, err := s.PostSignupMessage(ev.ID); err != nil {
		s.redirectWithNotice(w, r, ev.ID, "Could not post it: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, ev.ID, "Signup message posted.")
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
	if _, err := s.PublishToDiscord(ev.ID, s.boardChannelID); err != nil {
		s.redirectWithNotice(w, r, ev.ID, "Could not publish it: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, ev.ID,
		"Published. The Discord event points back here and says that pressing Interested does not hold a place.")
}

// handleWebSync pulls native Discord events into the store.
func (s *Server) handleWebSync(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	guilds, err := s.manageableGuilds(session)
	if err != nil {
		http.Redirect(w, r, "/?"+noticeQuery("Could not sync: "+err.Error()), http.StatusSeeOther)
		return
	}
	total := SyncResult{}
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
		total.Problems = append(total.Problems, result.Problems...)
	}
	notice := fmt.Sprintf("Pulled from Discord: %d new, %d updated, %d unchanged, %d cards posted.",
		total.Imported, total.Updated, total.Unchanged, total.Posted)
	if len(total.Problems) > 0 {
		notice += " Problems: " + strings.Join(total.Problems, "; ")
	}
	http.Redirect(w, r, "/?"+noticeQuery(notice), http.StatusSeeOther)
}

func (s *Server) redirectWithNotice(w http.ResponseWriter, r *http.Request, eventID int64, notice string) {
	http.Redirect(w, r, fmt.Sprintf("/events/%d?%s", eventID, noticeQuery(notice)),
		http.StatusSeeOther)
}

// noticeQuery renders a notice as a complete, correctly encoded query string.
// The notice is not always ours: handleWebSync builds it from Discord guild
// names and upstream error text, so it can hold any byte at all. Encoding it
// with url.Values is what keeps the message the reader gets identical to the
// message we sent — a hand-rolled replacement of the characters someone thought
// of loses the notice to a percent sign, truncates it at a hash, and drops it
// entirely at the "; " that joins two sync problems.
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
