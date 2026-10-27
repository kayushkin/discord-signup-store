package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
)

// Pointing people at the potluck sheet when they get a place: the reply to
// Join, the DM when they move up or an organiser puts them down as going,
// and a Join on the web, which lands on the event page. And a link to the
// sheet beside the event's name in #events.

// potluckJoinNote is what goes under "you're in" for an event with a sheet
// taking answers: bring something, the roles there are, and what is still
// asked for. "" for any other event.
func potluckJoinNote(ev *Event) string {
	if ev == nil || ev.Potluck == nil || !potluckTakesAnswers(ev) {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n🍲 **It's a potluck — please bring something.** Press **What I'm bringing** to say what")
	if len(ev.Potluck.Roles) > 0 {
		names := make([]string, len(ev.Potluck.Roles))
		for i, r := range ev.Potluck.Roles {
			names[i] = r.Name
		}
		b.WriteString(", and sign up to help if you can: " + strings.Join(names, ", "))
	}
	b.WriteString(".")
	if line := ev.Potluck.AskedForLine(); line != "" {
		b.WriteString("\n" + line)
	}
	return b.String()
}

// potluckJoinComponents are the buttons under that note: What I'm bringing,
// and the event page when the web pages are on. Nil for an event without a
// sheet taking answers.
func (s *Server) potluckJoinComponents(ev *Event) []any {
	if potluckJoinNote(ev) == "" {
		return nil
	}
	buttons := []any{potluckButton(ev.ID)}
	if origin := s.webOrigin(); origin != "" {
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleLink,
			"label": "Event page", "url": fmt.Sprintf("%s/events/%d", origin, ev.ID)})
	}
	return []any{map[string]any{"type": componentTypeActionRow, "components": buttons}}
}

// replyEphemeralWithComponents is replyEphemeral with buttons under it.
func (s *Server) replyEphemeralWithComponents(w http.ResponseWriter, content string, components []any) {
	if components == nil {
		s.replyEphemeral(w, content)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeChannelMessageWithSrc,
		"data": map[string]any{
			"content":          content,
			"flags":            messageFlagEphemeral,
			"allowed_mentions": map[string]any{"parse": []string{}},
			"components":       components,
		},
	})
}

// potluckSheetLink goes after an event's name in #events: a link to its
// page, where the sheet and its roles are, for an event with a sheet.
func (s *Server) potluckSheetLink(ev *Event) string {
	origin := s.webOrigin()
	if origin == "" || !potluckTakesAnswers(ev) {
		return ""
	}
	// Words and an arrow, no emoji, for the reason webRosterLink gives.
	return fmt.Sprintf("[Potluck sheet →](%s/events/%d)", origin, ev.ID)
}
