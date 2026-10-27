package discordsignup

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// The members' event page: what anyone in an event's server sees when they
// open it and may not edit it. The event's details, who is going, Join or
// Leave, and the potluck sheet, where someone going says what they will
// bring. Built from its own template, so nothing an organiser sees — the
// log, DM replies, invites, who has not answered the sheet — can reach it.

// renderMemberEventPage draws the members' page.
func (s *Server) renderMemberEventPage(w http.ResponseWriter, session *WebSession, ev *Event, notice string) {
	s.backfillDisplayNames(ev)
	var problems []string
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] roster %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the roster: "+err.Error())
	}
	avatars, err := s.store.ChosenAvatarUserIDs()
	if err != nil {
		log.Printf("[discord-signup] chosen avatars: %v", err)
		problems = append(problems, "Could not read who has an avatar: "+err.Error())
	}
	data := pageData{
		Title: ev.Name, Session: session, Event: ev, Roster: roster, AvatarUserIDs: avatars,
		EventFull: eventIsFull(ev), FreePlaces: max(0, ev.Capacity-ev.AttendingCount-ev.HeldCount),
		Notice: notice, PotluckCategories: potluckCategories,
	}
	for _, sg := range roster {
		switch sg.State {
		case StateAttending:
			data.Going = append(data.Going, sg)
		case StateWaitlisted:
			data.Waiting = append(data.Waiting, sg)
		case StateMaybe:
			data.Maybes = append(data.Maybes, sg)
		}
		if sg.DiscordUserID == session.DiscordUserID {
			data.MyState = sg.State
		}
	}
	data.Going = hostFirst(data.Going, ev.CreatedBy)
	organisers, err := s.store.EventOrganisers(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] organisers %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the organisers: "+err.Error())
	}
	data.OrganiserIDs = organiserIDsOf(ev, organisers)
	if data.EventPicture, err = s.eventPicturePanelOf(ev); err != nil {
		log.Printf("[discord-signup] event picture %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the picture of who is going: "+err.Error())
	}
	if data.Potluck, err = s.potluckPageOf(ev, data.Going, session.DiscordUserID, false); err != nil {
		log.Printf("[discord-signup] potluck page %d: %v", ev.ID, err)
		problems = append(problems, "Could not read the potluck sheet: "+err.Error())
	}
	if ev.Potluck != nil {
		data.MyPotluck = ev.Potluck.ItemsOf(session.DiscordUserID)
		data.MySaidNothing = ev.Potluck.SaidNothing(session.DiscordUserID)
	}
	data.Error = strings.Join(problems, " ")
	s.render(w, "member.html", data)
}

// memberPotluckEvent loads the event a member's potluck form names, for
// anyone in its server, or writes the response and returns nil.
func (s *Server) memberPotluckEvent(w http.ResponseWriter, r *http.Request) (*Event, *WebSession) {
	session := s.requireSession(w, r)
	if session == nil {
		return nil, nil
	}
	ev, _ := s.webEventForMember(w, r, session)
	if ev == nil {
		return nil, nil
	}
	return ev, session
}

// potluckWebRefusal says plainly why the sheet did not take a change.
func potluckWebRefusal(err error) string {
	switch {
	case errors.Is(err, ErrPotluckOff), errors.Is(err, ErrPotluckClosed):
		return "Nothing was saved: " + err.Error() + "."
	case errors.Is(err, ErrNotGoing):
		return "Nothing was saved: the sheet is for people going. Join first, then say what you're bringing."
	case errors.Is(err, ErrInvalidPotluck):
		return "Nothing was saved: " + strings.TrimPrefix(err.Error(), ErrInvalidPotluck.Error()+": ") + "."
	}
	return "Nothing was saved: " + err.Error()
}

// handleWebMyPotluckAdd adds one thing the viewer will bring.
func (s *Server) handleWebMyPotluckAdd(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberPotluckEvent(w, r)
	if ev == nil {
		return
	}
	item, err := s.store.AddPotluckItem(ev.ID, session.DiscordUserID, r.FormValue("category"), r.FormValue("item"))
	if err != nil {
		if !errors.Is(err, ErrPotluckOff) && !errors.Is(err, ErrPotluckClosed) && !errors.Is(err, ErrNotGoing) &&
			!errors.Is(err, ErrInvalidPotluck) {
			log.Printf("[discord-signup] web potluck add event=%d user=%s: %v", ev.ID, session.DiscordUserID, err)
		}
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Added: "+item.Item+".")
}

// handleWebMyPotluckNothing says the viewer is bringing nothing.
func (s *Server) handleWebMyPotluckNothing(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberPotluckEvent(w, r)
	if ev == nil {
		return
	}
	if err := s.store.SayBringingNothing(ev.ID, session.DiscordUserID); err != nil {
		s.redirectWithNotice(w, r, ev.ID, potluckWebRefusal(err))
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Saved: you're not bringing anything.")
}

// handleWebMyPotluckRemove takes one of the viewer's own items off, or
// their "bringing nothing".
func (s *Server) handleWebMyPotluckRemove(w http.ResponseWriter, r *http.Request) {
	ev, session := s.memberPotluckEvent(w, r)
	if ev == nil {
		return
	}
	itemID, err := strconv.ParseInt(r.FormValue("item_id"), 10, 64)
	if err != nil {
		http.Error(w, "item_id must be an integer", http.StatusBadRequest)
		return
	}
	err = s.store.RemoveOwnPotluckItem(ev.ID, itemID, session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		s.redirectWithNotice(w, r, ev.ID, "That was already off the sheet.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] web potluck remove event=%d user=%s: %v", ev.ID, session.DiscordUserID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was removed: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Taken off the sheet.")
}
