package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
)

// The personal dashboard: a per-viewer table with the buttons conditioned on
// who is looking.
//
// What it is NOT, because Discord does not offer it: a channel that greets you.
// There is no "user opened a channel" event — no gateway event, no webhook,
// nothing. A bot cannot tell that someone is looking at a channel, and an
// ephemeral message can only be CREATED in response to an interaction. So the
// nearest reachable shape is one click away: a standing message with a single
// button, and everything after that click is genuinely per-viewer.
//
// After the click it does everything a shared message cannot:
//
//   - each event shows Join OR Leave, depending on whether the viewer is in it
//   - Edit appears only for people who may edit that event
//   - the viewer's own state is written on the row ("going", "waitlist #2")
//   - pressing a button UPDATES the dashboard in place (callback type 7),
//     so it behaves like a little app rather than a chain of replies
const (
	// callbackTypeUpdateMessage edits the message the component sits on instead
	// of creating a new reply. On an ephemeral message this is what makes the
	// dashboard feel stateful: click Join, the row flips to Leave.
	callbackTypeUpdateMessage = 7

	// dashboardEventLimit caps the events shown. A row costs up to five
	// components (text, action row, three buttons) plus a separator between
	// rows: six rows is 6×5 + 5 separators + the container + a truncation note
	// = 37 of the measured 40. Seven was 43 — the budget test caught the first
	// draft of this comment claiming it fit.
	dashboardEventLimit = 6
)

// myEventsButtonID opens the per-viewer dashboard. It sits on the standing
// how-to message, which is the one place in the channel that is about starting
// something rather than about one event.
const myEventsButtonID = customIDPrefix + ":my-events:0"

// renderMyEventsDashboard draws the per-viewer table.
func (s *Server) renderMyEventsDashboard(in *Interaction) (map[string]any, error) {
	userID, _ := in.actor()
	events, err := s.liveEventsFor(in.GuildID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].StartsAt < events[j].StartsAt })

	mine := map[int64]Signup{}
	signups, err := s.store.UserSignupsInGuild(in.GuildID, userID)
	if err != nil {
		return nil, err
	}
	for _, u := range signups {
		mine[u.Event.ID] = u.Signup
	}
	canManageAll := in.canManageEvents()

	body := []any{}
	if len(events) == 0 {
		body = append(body, textBlock("-# Nothing coming up."))
	}
	shown := events
	if len(shown) > dashboardEventLimit {
		shown = shown[:dashboardEventLimit]
	}
	for i := range shown {
		ev := &shown[i]
		if i > 0 {
			body = append(body, map[string]any{
				"type": componentTypeSeparator, "divider": true, "spacing": 1,
			})
		}
		sg, in := mine[ev.ID]

		line := eventLine(ev)
		switch {
		case in && sg.State == StateWaitlisted:
			line += fmt.Sprintf("\n-# you are **waitlist #%d**", sg.WaitlistPlace)
		case in:
			line += "\n-# you are **going**"
		}
		body = append(body, textBlock(line))

		buttons := []any{}
		if ev.Status == StatusOpen {
			if in {
				buttons = append(buttons, map[string]any{
					"type": componentTypeButton, "style": buttonStyleSecondary,
					"label": "Leave", "custom_id": dashLeaveCustomID(ev.ID)})
			} else {
				buttons = append(buttons, map[string]any{
					"type": componentTypeButton, "style": buttonStylePrimary,
					"label": "Join", "custom_id": dashJoinCustomID(ev.ID)})
			}
		}
		buttons = append(buttons, map[string]any{
			"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Details", "custom_id": DetailsCustomID(ev.ID)})
		if canManageAll || (ev.CreatedBy != "" && ev.CreatedBy == userID) {
			buttons = append(buttons, map[string]any{
				"type": componentTypeButton, "style": buttonStyleSecondary,
				"label": "Edit", "custom_id": EditCustomID(ev.ID)})
		}
		body = append(body, map[string]any{"type": componentTypeActionRow, "components": buttons})
	}
	if len(events) > len(shown) {
		body = append(body, textBlock(fmt.Sprintf(
			"-# Showing %d of %d — the rest are on the web page.", len(shown), len(events))))
	}

	return map[string]any{
		"flags": messageFlagEphemeral | messageFlagComponentsV2,
		"components": []any{map[string]any{
			"type": componentTypeContainer, "accent_color": panelAccentColour,
			"components": body,
		}},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, nil
}

func dashJoinCustomID(eventID int64) string {
	return fmt.Sprintf("%s:dash-join:%d", customIDPrefix, eventID)
}

func dashLeaveCustomID(eventID int64) string {
	return fmt.Sprintf("%s:dash-leave:%d", customIDPrefix, eventID)
}

// handleMyEventsButton opens the dashboard, ephemerally.
func (s *Server) handleMyEventsButton(w http.ResponseWriter, in *Interaction) {
	payload, err := s.renderMyEventsDashboard(in)
	if err != nil {
		log.Printf("[discord-signup] render dashboard: %v", err)
		s.replyEphemeral(w, "Something went wrong reading your signups.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeChannelMessageWithSrc, "data": payload,
	})
}

// handleDashboardAction performs a join or leave and re-renders the dashboard
// in place, so the row the viewer just acted on flips under their cursor.
func (s *Server) handleDashboardAction(w http.ResponseWriter, in *Interaction, action string, eventID int64) {
	userID, displayName := in.actor()
	switch action {
	case "dash-join":
		result, err := s.store.Join(eventID, userID, displayName, JoinedViaButton)
		if err != nil && !errors.Is(err, ErrEventNotOpen) {
			log.Printf("[discord-signup] dash join event=%d user=%s: %v", eventID, userID, err)
			s.replyEphemeral(w, "Something went wrong. Nothing was changed — try again.")
			return
		}
		if err == nil {
			go s.syncAfterChange(eventID, []stateChange{{UserID: userID, State: result.Signup.State}})
		}
	case "dash-leave":
		result, err := s.store.Leave(eventID, userID, ActorUser)
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("[discord-signup] dash leave event=%d user=%s: %v", eventID, userID, err)
			s.replyEphemeral(w, "Something went wrong. Nothing was changed — try again.")
			return
		}
		if err == nil {
			if ev, err := s.store.GetEvent(eventID); err == nil {
				changes := []stateChange{{UserID: userID, State: StateWithdrawn}}
				if result.Promoted != nil {
					changes = append(changes,
						stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
					go s.notifyPromoted(ev, result.Promoted)
				}
				go s.syncAfterChange(eventID, changes)
			}
		}
	default:
		s.replyEphemeral(w, "Unknown dashboard action.")
		return
	}

	payload, err := s.renderMyEventsDashboard(in)
	if err != nil {
		log.Printf("[discord-signup] re-render dashboard: %v", err)
		s.replyEphemeral(w, "Done — but the view could not be redrawn. Press My events again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeUpdateMessage, "data": payload,
	})
}
