package discordsignup

import (
	"fmt"
	"net/http"
	"strings"
)

// Pointing people at the potluck sheet and the roles when they get a place:
// the reply to Join, the DM when they move up or an organiser puts them down
// as going, and a Join on the web, which lands on the event page. And links
// to the sheet and the roles beside the event's name in #events.

// potluckAndRolesJoinNote is what goes under "you're in" for an event whose
// potluck sheet or roles take answers: bring something, the roles there are
// to sign up to, and what is still asked for. "" for any other event.
func potluckAndRolesJoinNote(ev *Event) string {
	if ev == nil {
		return ""
	}
	potluck := ev.Potluck != nil && potluckTakesAnswers(ev)
	roles := sheetRolesTakeAnswers(ev) && len(ev.Roles) > 0
	if !potluck && !roles {
		return ""
	}
	names := make([]string, len(ev.Roles))
	for i, r := range ev.Roles {
		names[i] = r.Name
	}
	var b strings.Builder
	switch {
	case potluck && roles:
		b.WriteString("\n\n🍲 **It's a potluck — please bring something.** Press **What I'm bringing** to say what")
		b.WriteString(", and sign up to help if you can: " + strings.Join(names, ", ") + ".")
	case potluck:
		b.WriteString("\n\n🍲 **It's a potluck — please bring something.** Press **What I'm bringing** to say what.")
	default:
		b.WriteString("\n\n" + sheetRoleEmoji + " **Sign up to help if you can:** " + strings.Join(names, ", ") + ".")
	}
	if potluck {
		if line := ev.Potluck.AskedForLine(); line != "" {
			b.WriteString("\n" + line)
		}
	}
	return b.String()
}

// potluckAndRolesJoinComponents are the buttons under that note: What I'm
// bringing when the sheet takes answers, a button per role when roles do,
// and the event page when the web pages are on. Nil when there is no note.
func (s *Server) potluckAndRolesJoinComponents(ev *Event) []any {
	if potluckAndRolesJoinNote(ev) == "" {
		return nil
	}
	var buttons []any
	if ev.Potluck != nil && potluckTakesAnswers(ev) {
		buttons = append(buttons, potluckButton(ev.ID))
	}
	if origin := s.webOrigin(); origin != "" {
		buttons = append(buttons, map[string]any{"type": componentTypeButton, "style": buttonStyleLink,
			"label": "Event page", "url": fmt.Sprintf("%s/events/%d", origin, ev.ID)})
	}
	var rows []any
	if len(buttons) > 0 {
		rows = append(rows, map[string]any{"type": componentTypeActionRow, "components": buttons})
	}
	return append(rows, sheetRoleRow(ev)...)
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

// potluckAndRolesLinks go under an event's name in #events, one to a line:
// the potluck list when the sheet takes answers, and signing up to the roles
// when they do. Both open the event's page at that part of it. "" for an
// event with neither.
func (s *Server) potluckAndRolesLinks(ev *Event) string {
	origin := s.webOrigin()
	if origin == "" {
		return ""
	}
	page := fmt.Sprintf("%s/events/%d", origin, ev.ID)
	// Words and an arrow, no emoji, for the reason webRosterLink gives.
	links := ""
	if potluckTakesAnswers(ev) {
		links += fmt.Sprintf("\n[Potluck list →](%s#potluck)", page)
	}
	if sheetRolesTakeAnswers(ev) && len(ev.Roles) > 0 {
		names := make([]string, len(ev.Roles))
		for i, r := range ev.Roles {
			names[i] = escapeMarkdown(strings.TrimSpace(strings.SplitN(r.Name, "(", 2)[0]))
		}
		links += fmt.Sprintf("\n[Sign up to %s →](%s#roles)", strings.Join(names, ", "), page)
	}
	return links
}
