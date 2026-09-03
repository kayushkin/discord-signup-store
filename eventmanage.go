package discordsignup

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

// The management row's two actions beyond Edit: shutting signups, and
// cancelling. Both check permission on the press, because Discord shows a
// button to everybody or nobody.

// buttonStyleDanger is Discord's red button. Reserved for the one thing on
// a row that cannot be undone.
const buttonStyleDanger = 4

// handleCloseToggle flips signups between open and closed.
//
// Closed is not cancelled: the event still happens, everyone on the roster
// keeps their place, and nobody new can join. The label on the button says
// which way the press will go, so there is no confirm — it is reversible with
// the same button.
func (s *Server) handleCloseToggle(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	var next, said string
	switch ev.Status {
	case StatusOpen:
		next, said = StatusClosed, fmt.Sprintf("Signups for **%s** are closed. Nobody new can join; everyone on it stays.", ev.Name)
	case StatusClosed:
		next, said = StatusOpen, fmt.Sprintf("Signups for **%s** are open again.", ev.Name)
	default:
		s.replyEphemeral(w, fmt.Sprintf("**%s** is %s, so there are no signups to open or close.", ev.Name, ev.Status))
		return
	}
	userID, _ := in.actor()
	// Through the one edit path, so it is logged in event_updates and every
	// copy is republished — the card and the row lose or regain Join.
	if _, _, err := s.applyEventEdit(ev, EventPatch{Status: &next}, userID); err != nil {
		log.Printf("[discord-signup] toggle signups for event %d: %v", ev.ID, err)
		s.replyEphemeral(w, "Something went wrong. Nothing was changed.")
		return
	}
	s.replyEphemeral(w, said)
}

// handleCancelButton opens the confirm. Cancelling deletes the native Discord
// event and cannot be undone, so the confirm is typing the event's name back —
// the one gesture Discord offers that cannot be done by accident.
func (s *Server) handleCancelButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	if IsArchived(ev.Status) {
		s.replyEphemeral(w, fmt.Sprintf("**%s** is already %s.", ev.Name, ev.Status))
		return
	}
	modalID := CancelModalCustomID(ev.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": map[string]any{
			"custom_id": modalID,
			"title":     truncate("Cancel "+ev.Name, 45),
			"components": []any{map[string]any{"type": componentTypeActionRow, "components": []any{
				// Reuses the name field, scoped to this modal, so the ordinary
				// submit path delivers it as form.Name.
				modalTextInput(fieldName+"@"+modalID, "Type the event's name to cancel it",
					"", ev.Name, textInputStyleShort, true, 100),
			}}},
		},
	})
}

// applyCancelConfirm cancels if the name typed matches, and refuses otherwise.
func (s *Server) applyCancelConfirm(w http.ResponseWriter, in *Interaction, eventID int64, form EventForm) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(form.Name), strings.TrimSpace(ev.Name)) {
		s.replyEphemeral(w, fmt.Sprintf("That did not match **%s** — nothing was cancelled.", ev.Name))
		return
	}
	userID, _ := in.actor()
	if err := s.cancelEventEverywhere(ev, "cancelled by "+userID); err != nil {
		log.Printf("[discord-signup] cancel event %d: %v", ev.ID, err)
		s.replyEphemeral(w, fmt.Sprintf("Something went wrong — **%s** is NOT cancelled.", ev.Name))
		return
	}
	// cancelEventEverywhere writes the status straight to the store rather
	// than through applyEventEdit, because it is also what the reconcile
	// calls when Discord deleted the event; a person doing it deserves a row
	// in the history saying so.
	if after, err := s.store.GetEvent(ev.ID); err == nil {
		if err := s.store.LogEventUpdates(ev, after, userID); err != nil {
			log.Printf("[discord-signup] log cancel of event %d: %v", ev.ID, err)
		}
	}
	s.replyEphemeral(w, fmt.Sprintf("**%s** is cancelled. Its Discord event is gone and nobody can join.", ev.Name))
}

// handleRepeatButton opens the Repeat form: how often, and when it ends.
func (s *Server) handleRepeatButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	zone := ev.Timezone
	if zone == "" {
		zone = s.DefaultTimezone()
	}
	modalID := RepeatModalCustomID(ev.ID)
	row := func(field map[string]any) map[string]any {
		return map[string]any{"type": componentTypeActionRow, "components": []any{field}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": map[string]any{
			"custom_id": modalID,
			"title":     truncate("Repeat "+ev.Name, 45),
			"components": []any{
				row(modalTextInput(fieldRepeats+"@"+modalID, "Repeats — weekly, every 2 weeks, monthly, or never",
					describeRepeat(ev.RecurrenceRule), "weekly", textInputStyleShort, true, 40)),
				// This occurrence's end, not the series' — Discord has no series
				// end a client can set, so there is none to offer. The label
				// says which, because "Ends" on a form titled Repeat reads as
				// the other one.
				row(modalTextInput(fieldEndsAt+"@"+modalID, "Each occurrence ends — "+zone,
					FormatEventTime(ev.EndsAt, zone), "9/29 5pm   (blank for none)", textInputStyleShort, false, 40)),
			},
		},
	})
}

// applyRepeatForm stores the rule and pushes it to Discord through the one
// edit path, so it is logged and every copy republishes.
func (s *Server) applyRepeatForm(w http.ResponseWriter, in *Interaction, eventID int64, repeats, ends string) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	zone := ev.Timezone
	if zone == "" {
		zone = s.DefaultTimezone()
	}
	rule, err := repeatWordToRRule(repeats, startInZone(ev, zone))
	if err != nil {
		s.replyEphemeral(w, plainError(err))
		return
	}
	endsAt, err := ParseEventTime(ends, zone)
	if err != nil {
		s.replyEphemeral(w, plainError(err))
		return
	}
	userID, _ := in.actor()
	// A rule needs a zone to mean anything across a clock change, so one is
	// stamped if the event had none.
	patch := EventPatch{RecurrenceRule: &rule, EndsAt: &endsAt}
	if ev.Timezone == "" {
		patch.Timezone = &zone
	}
	if _, _, err := s.applyEventEdit(ev, patch, userID); err != nil {
		log.Printf("[discord-signup] repeat form for event %d: %v", ev.ID, err)
		s.replyEphemeral(w, plainError(err))
		return
	}
	if rule == "" {
		s.replyEphemeral(w, fmt.Sprintf("**%s** does not repeat.", ev.Name))
		return
	}
	s.replyEphemeral(w, fmt.Sprintf("**%s** repeats %s. Discord's event repeats with it. "+
		"When a date has passed the next one takes its place and signups open again from nobody.",
		ev.Name, describeRepeat(rule)))
}
