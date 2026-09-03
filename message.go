package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

// Discord message component types and button styles.
const (
	componentTypeActionRow = 1
	componentTypeButton    = 2
	// componentTypeTextInput is valid only inside a modal. Discord rejects it
	// in a message, which is why a free text field cannot live on the card
	// itself and needs the button-then-form round trip.
	componentTypeTextInput = 4

	textInputStyleShort = 1
	// textInputStyleParagraph is the multi-line box. Discord offers exactly
	// these two; there is no rich text and no markdown preview.
	textInputStyleParagraph = 2

	buttonStylePrimary   = 1
	buttonStyleSecondary = 2
)

// discordMessageContentLimit is Discord's hard cap on a message's content.
// Exceeding it is a 400, so the roster is trimmed to fit here — at the edge,
// where the presentation is, and nowhere earlier. Roster() itself always
// returns everyone.
const discordMessageContentLimit = 2000

// stateChange is one person's new state, for the projections that follow a
// write: the roles they should now hold, and the roster message.
type stateChange struct {
	UserID string
	State  string
}

// RenderForumCard is the same card without the discussion link: the forum
// post's first message IS the card, and a card pointing at its own post would
// read as a working link that goes nowhere new.
func RenderForumCard(ev *Event, roster []Signup) map[string]any {
	return renderSignupMessage(ev, roster)
}

func renderSignupMessage(ev *Event, roster []Signup) map[string]any {
	var b strings.Builder

	fmt.Fprintf(&b, "## %s\n", ev.Name)
	if ev.Description != "" {
		fmt.Fprintf(&b, "%s\n", ev.Description)
	}
	if ev.StartsAt > 0 {
		// Discord renders <t:unix:F> in each reader's own timezone, so no
		// timezone has to be chosen or stated here.
		fmt.Fprintf(&b, "\n🗓️ <t:%d:F>\n", ev.StartsAt)
	}

	switch {
	case ev.Status == StatusCancelled:
		b.WriteString("\n**Cancelled.**\n")
	case ev.Status == StatusCompleted:
		b.WriteString("\n**This event has finished.**\n")
	case ev.Status == StatusClosed:
		b.WriteString("\n**Signups are closed.**\n")
	case ev.Capacity == 0:
		fmt.Fprintf(&b, "\n**%d signed up** — no limit.\n", ev.AttendingCount)
	default:
		fmt.Fprintf(&b, "\n**%d/%d places taken**", ev.AttendingCount, ev.Capacity)
		if ev.WaitlistCount > 0 {
			fmt.Fprintf(&b, " · %d waiting", ev.WaitlistCount)
		}
		b.WriteString("\n")
	}

	attending, waiting := splitRoster(roster)
	if len(attending) > 0 {
		b.WriteString("\n**Going**\n")
		writeMentions(&b, attending)
	}
	if len(waiting) > 0 {
		b.WriteString("\n**Waitlist**\n")
		writeMentions(&b, waiting)
	}

	content := b.String()
	if len(content) > discordMessageContentLimit {
		const notice = "\n… list trimmed to fit Discord's message limit."
		content = content[:discordMessageContentLimit-len(notice)] + notice
	}

	payload := map[string]any{
		"content": content,
		// allowed_mentions empty: the roster is written with <@id> so names
		// render, but nobody wants a ping every time someone else signs up.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if ev.Status == StatusOpen {
		payload["components"] = signupComponents(ev.ID)
	} else {
		// An empty array, not a missing key. Omitting components on an edit
		// leaves the old buttons in place, so a closed event would keep a live
		// Join button under a message saying signups are closed.
		payload["components"] = []any{}
	}
	return payload
}

func signupComponents(eventID int64) []any {
	return []any{
		map[string]any{
			"type": componentTypeActionRow,
			"components": []any{
				map[string]any{
					"type":      componentTypeButton,
					"style":     buttonStylePrimary,
					"label":     "Join",
					"custom_id": JoinCustomID(eventID),
				},
				map[string]any{
					"type":      componentTypeButton,
					"style":     buttonStyleSecondary,
					"label":     "Leave",
					"custom_id": LeaveCustomID(eventID),
				},
				// Shown to everyone, because Discord cannot hide a component
				// from some readers. The click checks Manage Events and answers
				// privately, so an unauthorised press costs one ephemeral no.
				map[string]any{
					"type":      componentTypeButton,
					"style":     buttonStyleSecondary,
					"label":     "Edit",
					"custom_id": EditCustomID(eventID),
				},
			},
		},
	}
}

func splitRoster(roster []Signup) (attending, waiting []Signup) {
	for _, sg := range roster {
		switch sg.State {
		case StateAttending:
			attending = append(attending, sg)
		case StateWaitlisted:
			waiting = append(waiting, sg)
		}
	}
	return attending, waiting
}

func writeMentions(b *strings.Builder, signups []Signup) {
	for i, sg := range signups {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "<@%s>", sg.DiscordUserID)
	}
	b.WriteString("\n")
}

// applyRoles makes one person's roles match one state.
func (s *Server) applyRoles(ev *Event, change stateChange) error {
	// Roles are optional. An event with neither configured keeps its roster
	// only in this database, which is a legitimate way to run it.
	if ev.AttendingRoleID == "" && ev.WaitlistRoleID == "" {
		return nil
	}
	want := map[string]bool{}
	switch change.State {
	case StateAttending:
		want[ev.AttendingRoleID] = true
	case StateWaitlisted:
		want[ev.WaitlistRoleID] = true
	}

	var problems []string
	for _, roleID := range []string{ev.AttendingRoleID, ev.WaitlistRoleID} {
		if roleID == "" {
			continue
		}
		var err error
		if want[roleID] {
			err = s.discord.AddMemberRole(ev.GuildID, change.UserID, roleID)
		} else {
			err = s.discord.RemoveMemberRole(ev.GuildID, change.UserID, roleID)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("role %s: %v", roleID, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// notifyPromoted tells someone a place opened up for them.
//
// A DM is the right channel — it is about them and nobody else needs it — but
// it can be refused with error 50007 when they have DMs from server members
// off. That is common enough that falling back to a public mention is required,
// not optional: a promotion nobody hears about is a place nobody takes.
func (s *Server) notifyPromoted(ev *Event, promoted *Signup) {
	if s.discord == nil {
		return
	}
	content := fmt.Sprintf(
		"A place opened up for **%s** and you were next on the waitlist — you're in.",
		ev.Name)
	if ev.StartsAt > 0 {
		content += fmt.Sprintf("\n🗓️ <t:%d:F>", ev.StartsAt)
	}

	err := s.discord.SendDirectMessage(promoted.DiscordUserID, content)
	if err == nil {
		return
	}
	if !errors.Is(err, ErrCannotMessageUser) {
		log.Printf("[discord-signup] dm promoted user=%s event=%d: %v",
			promoted.DiscordUserID, ev.ID, err)
		return
	}

	log.Printf("[discord-signup] user=%s has DMs closed; falling back to a channel mention", promoted.DiscordUserID)
	fallback := map[string]any{
		"content": fmt.Sprintf("<@%s> %s", promoted.DiscordUserID, content),
		// This one DOES ping — it is the only way they will find out.
		"allowed_mentions": map[string]any{"users": []string{promoted.DiscordUserID}},
	}
	if _, err := s.discord.CreateMessage(ev.ChannelID, fallback); err != nil {
		log.Printf("[discord-signup] fallback mention user=%s event=%d: %v",
			promoted.DiscordUserID, ev.ID, err)
	}
}

// RenderHowToMessage is the standing post in the event-creation channel: what
// the buttons do, and the button that starts one.
//
// Written as a page rather than a caption because it is the only place the two
// counts get explained. People will otherwise compare Discord's Interested
// number with the roster, find they disagree, and reasonably assume something
// is broken.
func RenderHowToMessage(boardChannelID, timezone string) map[string]any {
	var b strings.Builder
	b.WriteString("## Make an event\n")
	fmt.Fprintf(&b, "Press **Create an event** and fill in the form. That is all there is "+
		"to it — your event turns up in <#%s> and people press **Join**.\n\n", boardChannelID)
	b.WriteString("If it fills up, everyone after that waits in line and moves up on their own.\n\n")
	fmt.Fprintf(&b, "-# Times are in %s and can be written `9/29 3` · `9/29 3:00` · "+
		"`9/29 3:00pm` · `tomorrow 6pm` · `friday noon`\n", timezone)

	return map[string]any{
		"content":          b.String(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components": []any{
			map[string]any{
				"type": componentTypeActionRow,
				"components": []any{
					map[string]any{
						"type":      componentTypeButton,
						"style":     buttonStylePrimary,
						"label":     "Create an event",
						"custom_id": CreateCustomID(),
					},
					// A shared message cannot mark "you are in this one" per
					// viewer — Discord renders it identically for everyone — so
					// this button is how that question gets answered: an
					// ephemeral reply, which IS per-viewer. It sits here rather
					// than on the table because it is about the reader.
					map[string]any{
						"type":      componentTypeButton,
						"style":     buttonStyleSecondary,
						"label":     "My events",
						"custom_id": myEventsButtonID,
					},
				},
			},
		},
	}
}
