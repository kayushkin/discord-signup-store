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
	// buttonStyleLink opens a URL instead of sending an interaction.
	buttonStyleLink = 5
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
	// The host is whoever created the event. A mention rather than a name:
	// Discord renders it as their current server name whether or not they
	// are on the roster, and allowed_mentions below keeps it from pinging.
	if ev.CreatedBy != "" {
		fmt.Fprintf(&b, "**Host:** <@%s>\n", ev.CreatedBy)
	}
	if repeats := repeatsLabel(ev); repeats != "" {
		// Discord shows the rule on its own event; the card is the one place
		// the roster is, so it is the one place to say that the roster is for
		// this date only.
		fmt.Fprintf(&b, "%s — signups are for this date; they open again after it.\n", repeats)
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
	attending = hostFirst(attending, ev.CreatedBy)
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

// hostFirst moves the host — the event's creator, by user id — to the front
// of a going list, leaving everyone else in arrival order. For the lists
// people read; the numbered Details list keeps arrival order, since its
// numbers are places in line.
func hostFirst(attending []Signup, hostUserID string) []Signup {
	if hostUserID == "" {
		return attending
	}
	out := make([]Signup, 0, len(attending))
	for _, sg := range attending {
		if sg.DiscordUserID == hostUserID {
			out = append(out, sg)
		}
	}
	for _, sg := range attending {
		if sg.DiscordUserID != hostUserID {
			out = append(out, sg)
		}
	}
	return out
}

// maybeOf is the Maybe list, in the order people put themselves on it.
func maybeOf(roster []Signup) []Signup {
	var maybe []Signup
	for _, sg := range roster {
		if sg.State == StateMaybe {
			maybe = append(maybe, sg)
		}
	}
	return maybe
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
	s.tellTheyHaveAPlace(ev, promoted, fmt.Sprintf(
		"A place opened up for **%s** and you were next on the waitlist — you're in.", ev.Name))
}

// notifyGivenAPlace tells someone an organiser gave them a place — from the
// waitlist or the Maybe list — rather than one opening up in turn.
func (s *Server) notifyGivenAPlace(ev *Event, promoted *Signup) {
	s.tellTheyHaveAPlace(ev, promoted, fmt.Sprintf(
		"An organiser gave you a place at **%s** — you're in.", ev.Name))
}

// notifyPlacedOnList tells someone an organiser put them on a list. Going is
// news they must not miss, so it falls back to a channel mention as a
// promotion does; the waitlist and Maybe go by DM alone.
func (s *Server) notifyPlacedOnList(ev *Event, placed *Signup) {
	var content string
	switch placed.State {
	case StateAttending:
		s.tellTheyHaveAPlace(ev, placed, fmt.Sprintf("An organiser put you down as going to **%s** — you're in.", ev.Name))
		return
	case StateWaitlisted:
		content = fmt.Sprintf("An organiser put you on the waitlist for **%s**, at number %d. "+
			"If a place opens you move up automatically and I will message you.", ev.Name, placed.WaitlistPlace)
	case StateMaybe:
		content = fmt.Sprintf("An organiser put you down as Maybe for **%s**. It does not hold a place — "+
			"press Join on the event if you decide to go.", ev.Name)
	default:
		return
	}
	if s.discord == nil {
		return
	}
	if ev.StartsAt > 0 {
		content += fmt.Sprintf("\n🗓️ <t:%d:F>", ev.StartsAt)
	}
	if err := s.discord.SendDirectMessage(placed.DiscordUserID, content); err != nil {
		log.Printf("[discord-signup] dm placed user=%s event=%d: %v", placed.DiscordUserID, ev.ID, err)
	}
}

// tellTheyHaveAPlace sends the news by DM, and by a channel mention when
// their DMs are closed.
func (s *Server) tellTheyHaveAPlace(ev *Event, promoted *Signup, content string) {
	if s.discord == nil {
		return
	}
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
