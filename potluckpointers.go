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

// potluckSheetLinks go under an event's name in #events, one to a line: the
// potluck list, and, when the sheet has roles, signing up to them. Both
// open the event's page at that part of it. "" for an event without a
// sheet taking answers.
func (s *Server) potluckSheetLinks(ev *Event) string {
	origin := s.webOrigin()
	if origin == "" || !potluckTakesAnswers(ev) {
		return ""
	}
	page := fmt.Sprintf("%s/events/%d", origin, ev.ID)
	// Words and an arrow, no emoji, for the reason webRosterLink gives.
	links := fmt.Sprintf("\n[Potluck list →](%s#potluck)", page)
	if ev.Potluck != nil && len(ev.Potluck.Roles) > 0 {
		names := make([]string, len(ev.Potluck.Roles))
		for i, r := range ev.Potluck.Roles {
			names[i] = escapeMarkdown(strings.TrimSpace(strings.SplitN(r.Name, "(", 2)[0]))
		}
		links += fmt.Sprintf("\n[Sign up to %s →](%s#roles)", strings.Join(names, ", "), page)
	}
	return links
}
