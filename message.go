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

// RenderSignupMessage builds the public message that carries the buttons.
//
// It shows two numbers, both labelled, and deliberately does not try to
// reconcile them with Discord's own Interested count on the linked scheduled
// event. Those two will disagree — Discord counts notification subscribers and
// this counts places — and a message that quietly picks one is worse than one
// that is clear about which it means.
func RenderSignupMessage(ev *Event, roster []Signup) map[string]any {
	return renderSignupMessage(ev, roster, true)
}

// RenderForumCard is the same card without the discussion link: the forum
// post's first message IS the card, and a card pointing at its own post would
// read as a working link that goes nowhere new.
func RenderForumCard(ev *Event, roster []Signup) map[string]any {
	return renderSignupMessage(ev, roster, false)
}

func renderSignupMessage(ev *Event, roster []Signup, withForumLink bool) map[string]any {
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

	if withForumLink && ev.ForumPostID != "" {
		// A thread mention: renders as the post's clickable name and needs no
		// guild id or URL building.
		fmt.Fprintf(&b, "\n💬 <#%s>\n", ev.ForumPostID)
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

// PostSignupMessage posts an event's signup message and records its id.
//
// The id matters beyond convenience: a button click arrives naming the message
// it sits on, and EventByMessage is how that click is resolved back to a roster
// even if the custom_id has been copied elsewhere.
func (s *Server) PostSignupMessage(eventID int64) (*Event, error) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	roster, err := s.store.Roster(eventID, false)
	if err != nil {
		return nil, err
	}
	messageID, err := s.discord.CreateMessage(ev.ChannelID, RenderSignupMessage(ev, roster))
	if err != nil {
		return nil, fmt.Errorf("post signup message: %w", err)
	}
	updated, err := s.store.UpdateEvent(eventID, EventPatch{MessageID: &messageID})
	if err != nil {
		return nil, err
	}
	s.ensureEventThread(updated)
	return updated, nil
}

// ensureEventThread makes sure a live event's card carries a discussion
// thread, exactly once.
//
// Hung off RefreshSignupMessage rather than only event creation, because that
// is the choke point every maintained card passes through — so events that
// existed before threads did grow one on their next activity, with no
// backfill pass to write or forget. Idempotent via thread_id, so the cost on
// every later refresh is one column read.
func (s *Server) ensureEventThread(ev *Event) {
	if s.discord == nil || ev.MessageID == "" || ev.ThreadID != "" || ev.Status != StatusOpen {
		return
	}
	threadID, err := s.discord.CreateThreadFromMessage(ev.ChannelID, ev.MessageID, truncate(ev.Name, 100))
	if err != nil {
		// Loud and non-fatal: the card and roster are the product, the thread
		// is a place to talk about them.
		log.Printf("[discord-signup] create thread for event %d: %v", ev.ID, err)
		return
	}
	if _, err := s.store.UpdateEvent(ev.ID, EventPatch{ThreadID: &threadID}); err != nil {
		log.Printf("[discord-signup] record thread for event %d: %v", ev.ID, err)
		return
	}
	// A seed so the thread opens with its purpose rather than empty. Best
	// effort; the thread exists either way.
	if _, err := s.discord.CreateMessage(threadID, map[string]any{
		"content":          fmt.Sprintf("Chat about **%s** here. Signups stay on the card above.", ev.Name),
		"allowed_mentions": map[string]any{"parse": []string{}},
	}); err != nil {
		log.Printf("[discord-signup] seed thread for event %d: %v", ev.ID, err)
	}
	log.Printf("[discord-signup] opened thread %s for event %d (%q)", threadID, ev.ID, ev.Name)
	ev.ThreadID = threadID
}

// RefreshSignupMessage rewrites the public message to match the roster.
func (s *Server) RefreshSignupMessage(eventID int64) error {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		return err
	}
	if ev.MessageID == "" {
		return nil // never posted; nothing to refresh
	}
	if s.discord == nil {
		return nil
	}
	roster, err := s.store.Roster(eventID, false)
	if err != nil {
		return err
	}
	s.ensureEventThread(ev)
	return s.discord.EditMessage(ev.ChannelID, ev.MessageID, RenderSignupMessage(ev, roster))
}

// syncAfterChange pushes what the database now says out to Discord: the roles
// each affected person should hold, and the refreshed roster message.
//
// Runs after the reply, never before it. The person who clicked must not wait
// on Discord's API for their answer, and a role that fails to apply must not
// make a successful signup look failed. Every failure here is logged loudly and
// none of them roll anything back — the roster is the source of truth and the
// roles are a projection of it, so the fix is to re-run the projection.
func (s *Server) syncAfterChange(ev *Event, changes []stateChange) {
	if s.discord == nil {
		return
	}
	for _, change := range changes {
		// Someone who left keeps their ✅ on the forum post otherwise, and a
		// reaction that no longer means membership would teach everyone to
		// distrust it. Removing it fires a gateway remove event, which lands on
		// an already-withdrawn row and no-ops.
		if change.State == StateWithdrawn && ev.ForumPostID != "" {
			if err := s.discord.RemoveUserReaction(ev.ForumPostID, ev.ForumPostID,
				joinReactionEmoji, change.UserID); err != nil {
				log.Printf("[discord-signup] clear ✅ for %s on event %d: %v", change.UserID, ev.ID, err)
			}
		}
		if err := s.applyRoles(ev, change); err != nil {
			log.Printf("[discord-signup] role sync event=%d user=%s state=%s: %v",
				ev.ID, change.UserID, change.State, err)
		}
	}
	if err := s.RefreshSignupMessage(ev.ID); err != nil {
		log.Printf("[discord-signup] refresh message event=%d: %v", ev.ID, err)
	}
	// The table row is a second view of the same roster. Refreshed from here
	// because this is the one place every roster change passes through — join,
	// leave, Interested, promotion, operator override — so there is no path
	// that updates a card and forgets the row. One message, not the whole
	// table: this runs on every signup.
	s.refreshEventTableQuietly(ev.GuildID)
	s.refreshForumPostQuietly(ev)
	// The native event's title carries the count, so it goes stale on every
	// signup. Pushed through the same function an edit uses rather than a
	// second, lighter one: two paths that both write the native event would
	// eventually disagree about what they write.
	if err := s.PushEditToDiscord(ev); err != nil {
		log.Printf("[discord-signup] push title for event %d: %v", ev.ID, err)
	}
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
	b.WriteString("## Making an event\n")
	b.WriteString("Press **Create an event** below. A form opens with five boxes:\n\n")
	b.WriteString("**Name** · **Starts** · **Ends** · **Places** · **Location**\n\n")
	fmt.Fprintf(&b, "Times are typed as `2026-09-05 19:00` and read in **%s**.\n", timezone)
	b.WriteString("**Places** is how many people fit — put `0` for no limit.\n\n")

	fmt.Fprintf(&b, "The event then appears in <#%s> with **Join** and **Leave** buttons.\n\n", boardChannelID)

	b.WriteString("## What the buttons do\n")
	b.WriteString("**Join** takes a place. If the event is full you go on the waitlist " +
		"instead, and you are told your number. When someone drops out the person who has " +
		"waited longest moves up automatically and gets a message.\n\n")
	b.WriteString("**Leave** gives your place up. It goes to whoever is next in line.\n\n")
	b.WriteString("**Edit** changes the event. Only the person who made it, or someone with " +
		"Manage Events, can use it — everyone else gets a polite no.\n\n")

	b.WriteString("## Discord's own events\n")
	b.WriteString("An event made through Discord's normal event feature is picked up " +
		"automatically and gets a card here too. Pressing **Interested** on one signs you up " +
		"exactly like Join does.\n\n")
	b.WriteString("⚠️ The two numbers will not match, and they cannot be made to. Discord " +
		"counts everyone who asked to be notified; the card counts everyone who has a place. " +
		"Discord has no way to cap its own list or to remove anyone from it, which is why " +
		"the card exists. **The card is the real roster.**\n\n")

	b.WriteString("Everything else — description, repeating events, roles — is on the web page, " +
		"because a Discord form holds five boxes and no more.")

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
				},
			},
		},
	}
}

// PostHowToMessage puts the standing how-to in a channel and pins it.
//
// Pinned because it is the one message in that channel and it must stay
// reachable; a pin failure is logged rather than fatal, since an unpinned
// how-to in an otherwise empty channel is still the first thing anyone sees.
func (s *Server) PostHowToMessage(channelID string) (string, error) {
	if s.discord == nil {
		return "", errors.New("no discord client configured")
	}
	messageID, err := s.discord.CreateMessage(channelID,
		RenderHowToMessage(s.BoardChannelID(), s.DefaultTimezone()))
	if err != nil {
		return "", fmt.Errorf("post how-to: %w", err)
	}
	if err := s.discord.PinMessage(channelID, messageID); err != nil {
		// Almost always a 403 for want of PIN_MESSAGES. Discord split that out
		// of MANAGE_MESSAGES into its own permission (bit 51), so a bot invited
		// before the split — or with an invite link that predates it — can
		// delete messages and still not pin one. Logged rather than fatal: an
		// unpinned how-to in an otherwise empty channel is still the first
		// thing anyone reads.
		log.Printf("[discord-signup] could not pin the how-to (needs PIN_MESSAGES, "+
			"which is separate from MANAGE_MESSAGES): %v", err)
	}
	return messageID, nil
}
