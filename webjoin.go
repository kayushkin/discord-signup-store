package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
)

// Join and Leave on the web home page: the same as the buttons on Discord,
// for someone who is in the event's server. The name recorded is what the
// server calls them now, read from Discord, which also proves they are still
// in it.

// eventCard is one event on the home page, as this viewer sees it.
type eventCard struct {
	Event
	// MayOpen is whether the viewer may open the event's page: whoever may
	// edit it. Everyone else sees the card and cannot open it.
	MayOpen bool
	// MyState is the viewer's place on the roster: attending, waitlisted,
	// maybe, or "" when they are not on it.
	MyState string
	// MayJoin is whether the card offers Join or Leave: signups are open and
	// the viewer is in the event's server.
	MayJoin bool
	// Full is whether a capped event has no free place.
	Full bool
	// PictureSignature is the address version of the event's picture of who
	// is going, "" when there is none to show.
	PictureSignature string
	// Faces are the people going with an avatar, at most four, by Discord
	// user id; Blanks stand for up to three more going without one.
	Faces  []string
	Blanks []struct{}
}

// SignupStatesOf is where a person is on each event they are on.
func (s *Store) SignupStatesOf(discordUserID string) (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT event_id, state FROM signups WHERE discord_user_id = ? AND state != ?`,
		discordUserID, StateWithdrawn)
	if err != nil {
		return nil, fmt.Errorf("read %s's signups: %w", discordUserID, err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var eventID int64
		var state string
		if err := rows.Scan(&eventID, &state); err != nil {
			return nil, fmt.Errorf("scan signup: %w", err)
		}
		out[eventID] = state
	}
	return out, rows.Err()
}

func homeRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/?"+noticeQuery(notice), http.StatusSeeOther)
}

// homeRedirectWithMascotReaction goes home with the notice, and has the
// mascot play one of mascotReactions there.
func homeRedirectWithMascotReaction(w http.ResponseWriter, r *http.Request, notice, reaction string) {
	http.Redirect(w, r, "/?"+noticeQuery(notice)+"&"+mascotReactionQuery(reaction), http.StatusSeeOther)
}

// eventForSelfSignup reads the event a Join or Leave names and checks the
// viewer is in its server, answering the request itself when not.
func (s *Server) eventForSelfSignup(w http.ResponseWriter, r *http.Request, session *WebSession) *Event {
	eventID, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	ev, err := s.store.GetEvent(eventID)
	if errors.Is(err, ErrNotFound) {
		homeRedirect(w, r, "That event no longer exists.")
		return nil
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if !session.IsMemberOf(ev.GuildID) {
		http.Error(w, "you are not in this event's server", http.StatusForbidden)
		return nil
	}
	return ev
}

func (s *Server) handleWebJoin(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev := s.eventForSelfSignup(w, r, session)
	if ev == nil {
		return
	}
	if s.discord == nil {
		http.Error(w, "no Discord client, so your name in the server cannot be read", http.StatusServiceUnavailable)
		return
	}
	member, err := s.discord.GuildMember(ev.GuildID, session.DiscordUserID)
	if errors.Is(err, ErrUnknownMember) {
		homeRedirect(w, r, "Discord says you are no longer in this event's server.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web join event=%d: read member %s: %v", ev.ID, session.DiscordUserID, err)
		homeRedirect(w, r, "Could not read your name in the server from Discord, so nothing was changed. Try again.")
		return
	}
	result, err := s.joinAsThemselves(ev.ID, session.DiscordUserID, member.DisplayName, JoinedViaWebPage)
	switch {
	case errors.Is(err, ErrEventNotOpen):
		homeRedirect(w, r, fmt.Sprintf("Signups for %s are closed.", ev.Name))
		return
	case errors.Is(err, ErrEventFull):
		homeRedirect(w, r, fmt.Sprintf("%s is full, and its organiser turned the waitlist off.", ev.Name))
		return
	case err != nil:
		log.Printf("[discord-signup] web join event=%d user=%s: %v", ev.ID, session.DiscordUserID, err)
		homeRedirect(w, r, "Something went wrong signing you up. Nothing was changed. Try again.")
		return
	}
	switch {
	case result.AlreadySignedUp && result.Signup.State == StateWaitlisted:
		homeRedirect(w, r, fmt.Sprintf("You are already on the waitlist for %s, at number %d.", ev.Name, result.Signup.WaitlistPlace))
	case result.AlreadySignedUp:
		homeRedirect(w, r, fmt.Sprintf("You are already going to %s.", ev.Name))
	case result.Signup.State == StateWaitlisted:
		homeRedirectWithMascotReaction(w, r, fmt.Sprintf("%s is full, so you are on the waitlist at number %d. If someone drops out you move up and get a message on Discord.", ev.Name, result.Signup.WaitlistPlace), "waitlisted")
	default:
		homeRedirectWithMascotReaction(w, r, fmt.Sprintf("You're going to %s.", ev.Name), "joined")
	}
}

func (s *Server) handleWebLeave(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev := s.eventForSelfSignup(w, r, session)
	if ev == nil {
		return
	}
	result, err := s.leaveAsThemselves(ev.ID, session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		homeRedirect(w, r, fmt.Sprintf("You were not on the list for %s.", ev.Name))
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web leave event=%d user=%s: %v", ev.ID, session.DiscordUserID, err)
		homeRedirect(w, r, "Something went wrong. Nothing was changed. Try again.")
		return
	}
	homeRedirectWithMascotReaction(w, r, ev.Name+": "+describeLeave(result), "left")
}
