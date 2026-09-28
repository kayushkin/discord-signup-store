package discordsignup

import (
	"log"
	"net/http"
	"strings"
)

// People on the create form: the same picker as an event page's Add someone,
// so an organiser can invite people, or put them on a list, as the event is
// made. The event has no id while the form is open, so the search names the
// server instead, and the people are handled once the event exists.

// handleWebNewEventMemberSearch is the create form's search: members of the
// server picked on the form, for someone who may create events there.
func (s *Server) handleWebNewEventMemberSearch(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	guildID := strings.TrimSpace(r.URL.Query().Get("guild_id"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if viewable, err := s.mayViewGuild(session, guildID); err != nil || !viewable {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you are not in that server"})
		return
	}
	mayCreate, err := s.mayCreateEventsIn(session.editActor(guildID))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not check whether you may create events there: " + err.Error()})
		return
	}
	if !mayCreate {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you may not create events in that server"})
		return
	}
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"members": []memberSuggestion{}})
		return
	}
	if s.discord == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Discord client configured"})
		return
	}
	matches, err := s.findMembers(guildID, query, memberSearchLimit)
	if err != nil {
		log.Printf("[discord-signup] member search in %s for %q: %v", guildID, query, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Discord member search failed: " + err.Error()})
		return
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
		out = append(out, memberSuggestion{MemberMatch: m, ReadableName: readable[m.UserID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// peopleOnCreate handles the people picked on the create form once the event
// exists: an invite each, or a place on the list picked, as an event page's
// Add someone does. It says what happened to each, or "" when nobody was
// picked.
func (s *Server) peopleOnCreate(r *http.Request, ev *Event, session *WebSession) string {
	userIDs := pickedUserIDs(r)
	if len(userIDs) == 0 {
		return ""
	}
	action := r.FormValue("people_action")
	lines := make([]string, 0, len(userIDs))
	switch action {
	case "invite":
		if s.discord == nil {
			return "No invites were sent: there is no Discord client configured."
		}
		for _, userID := range userIDs {
			lines = append(lines, s.invitePerson(ev, session, userID, ""))
		}
	case StateAttending, StateMaybe:
		for _, userID := range userIDs {
			lines = append(lines, s.placePerson(ev, session, userID, action))
		}
	default:
		return "Nobody picked was invited or added: choose what to do with them."
	}
	return strings.Join(lines, " ")
}
