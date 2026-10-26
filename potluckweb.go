package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// The potluck sheet on the organiser's event page: how many of each category
// the event wants, who is bringing what, who going has not answered, and the
// Nudge button. The switch itself is the page's ordinary event edit
// (potluck=on|off).

// potluckPage is the sheet as the event page shows it.
type potluckPage struct {
	Summary      string
	StillNeeded  string
	Categories   []potluckPageCategory
	Nothing      []Signup
	Unanswered   []potluckPageUnanswered
	TakesAnswers bool
}

type potluckPageCategory struct {
	Key, Label, Emoji string
	Wanted, Have      int
	Items             []potluckPageItem
}

type potluckPageItem struct {
	ID     int64
	Item   string
	Person Signup
}

type potluckPageUnanswered struct {
	Person Signup
	// LastAsked is when a reminder or nudge last reached them; 0 if never.
	LastAsked int64
	// LastFailed says why the last one did not reach them, or "".
	LastFailed string
}

// potluckPageOf builds the event page's sheet from the event and its people
// going. Nil when the event has no sheet. Who has not answered, and who is
// bringing nothing, are read only forOrganisers.
func (s *Server) potluckPageOf(ev *Event, going []Signup, forOrganisers bool) (*potluckPage, error) {
	if ev.Potluck == nil {
		return nil, nil
	}
	sheet := ev.Potluck
	people := map[string]Signup{}
	for _, sg := range going {
		people[sg.DiscordUserID] = sg
	}
	page := &potluckPage{Summary: sheet.SummaryLine(), StillNeeded: sheet.StillNeededLine(),
		TakesAnswers: potluckTakesAnswers(ev)}
	for _, c := range potluckCategories {
		pc := potluckPageCategory{Key: c.Key, Label: c.Label, Emoji: c.Emoji, Wanted: sheet.Needs[c.Key]}
		for _, it := range sheet.ItemsIn(c.Key) {
			pc.Items = append(pc.Items, potluckPageItem{ID: it.ID, Item: it.Item, Person: people[it.DiscordUserID]})
		}
		pc.Have = len(pc.Items)
		page.Categories = append(page.Categories, pc)
	}
	if !forOrganisers {
		return page, nil
	}
	for _, id := range sheet.BringingNothing {
		page.Nothing = append(page.Nothing, people[id])
	}
	reminders, err := s.store.PotluckRemindersFor(ev.ID)
	if err != nil {
		return nil, err
	}
	for _, sg := range going {
		if sheet.Answered(sg.DiscordUserID) {
			continue
		}
		row := potluckPageUnanswered{Person: sg}
		for _, r := range reminders {
			if r.DiscordUserID != sg.DiscordUserID {
				continue
			}
			if r.Outcome == potluckReminderSent {
				row.LastAsked, row.LastFailed = r.SentAt, ""
			} else if r.Outcome == potluckReminderDMsClosed {
				row.LastFailed = "DMs closed"
			} else {
				row.LastFailed = "Discord refused the DM"
			}
		}
		page.Unanswered = append(page.Unanswered, row)
	}
	return page, nil
}

// handleWebPotluckNeeds saves how many of each category the event wants.
func (s *Server) handleWebPotluckNeeds(w http.ResponseWriter, r *http.Request) {
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
	needs := map[string]int{}
	for _, c := range potluckCategories {
		text := strings.TrimSpace(r.FormValue("wanted_" + c.Key))
		if text == "" {
			continue
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			s.redirectWithNotice(w, r, ev.ID, fmt.Sprintf("Not saved: %s must be a whole number.", c.Label))
			return
		}
		needs[c.Key] = n
	}
	if err := s.store.SetPotluckNeeds(ev.ID, needs, "web:"+session.DiscordUserID); err != nil {
		if errors.Is(err, ErrInvalidPotluck) {
			s.redirectWithNotice(w, r, ev.ID, "Not saved: "+strings.TrimPrefix(err.Error(), ErrInvalidPotluck.Error()+": ")+".")
			return
		}
		log.Printf("[discord-signup] potluck needs %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Not saved: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Saved what the potluck needs.")
}

// handleWebPotluckRemove takes one thing off the sheet, for an organiser.
func (s *Server) handleWebPotluckRemove(w http.ResponseWriter, r *http.Request) {
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
	itemID, err := strconv.ParseInt(r.FormValue("item_id"), 10, 64)
	if err != nil {
		http.Error(w, "item_id must be an integer", http.StatusBadRequest)
		return
	}
	err = s.store.RemovePotluckItem(ev.ID, itemID, "web:"+session.DiscordUserID)
	if errors.Is(err, ErrNotFound) {
		s.redirectWithNotice(w, r, ev.ID, "That was already off the sheet.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] potluck remove %d item %d: %v", ev.ID, itemID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was removed: "+err.Error())
		return
	}
	s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	s.redirectWithNotice(w, r, ev.ID, "Taken off the sheet.")
}
