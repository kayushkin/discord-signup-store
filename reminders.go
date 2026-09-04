package discordsignup

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// Telling people their event is about to happen.
//
// Two messages per event: one an hour before, one when it starts. Both name
// everyone who has a place, and both are the only messages this service sends
// that deliberately ping — the point of them is to interrupt.
//
// Discord's own scheduled events notify at start time, but they notify whoever
// pressed Interested, and Interested is not the roster: somebody can be
// Interested from before the event was linked, or be Interested after leaving,
// or hold a place having never pressed it. Notifying the roster is the whole
// reason this exists.

const (
	// reminderLead is how far ahead the first message goes out.
	reminderLead = time.Hour

	// reminderGrace is how late a reminder may still be sent.
	//
	// This is the important number, and it is small on purpose. Without it, a
	// service that was down for a day would come back, find forty events whose
	// moment had passed, and ping everybody about all of them at once. A
	// reminder that missed its moment is not worth sending: the event has
	// already started and the people in it know.
	reminderGrace = 15 * time.Minute

	// mentionsPerMessage is Discord's cap on allowed_mentions.users.
	mentionsPerMessage = 100
)

// SendDueReminders posts whichever reminders have come due, once each.
//
// Idempotent by the two stamps on the event row, so running it every minute
// sends one message per event per stage no matter how often it is called, and a
// restart mid-run cannot double up.
func (s *Server) SendDueReminders() (sent int, err error) {
	if s.discord == nil {
		return 0, nil
	}
	guilds, err := s.store.GuildsWithEvents()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	for _, guildID := range guilds {
		if s.guildChannels(guildID).Reminder == "" {
			continue // reminders are off for this guild; nothing is stamped
		}
		events, err := s.liveEventsFor(guildID)
		if err != nil {
			log.Printf("[discord-signup] reminders for guild %s: %v", guildID, err)
			continue
		}
		for i := range events {
			sent += s.remindForEvent(&events[i], now)
		}
	}
	return sent, nil
}

// remindForEvent decides what is owed for one event and sends it.
func (s *Server) remindForEvent(ev *Event, now time.Time) int {
	if ev.StartsAt == 0 || ev.Status == StatusCancelled {
		return 0
	}
	starts := time.Unix(ev.StartsAt, 0)
	sent := 0

	// The hour-before message. Due once the event is within the hour, and dead
	// as soon as the event has started — at that point the starting message is
	// the one to send, and two pings a minute apart help nobody.
	if ev.RemindedBeforeAt == 0 {
		switch {
		case now.Before(starts.Add(-reminderLead)):
			// Not yet.
		case now.Before(starts):
			if s.sendReminder(ev, reminderStageBefore) {
				sent++
			}
		default:
			// The moment went by unsent — the service was down, or the event
			// was created inside the hour. Stamped without sending, so it
			// cannot fire later as a reminder about something already running.
			s.stampReminder(ev, reminderStageBefore, "the hour-before moment had passed")
		}
	}

	if ev.RemindedStartAt == 0 && !now.Before(starts) {
		if now.Sub(starts) <= reminderGrace {
			if s.sendReminder(ev, reminderStageStart) {
				sent++
			}
		} else {
			s.stampReminder(ev, reminderStageStart, "the start was more than the grace window ago")
		}
	}
	return sent
}

const (
	reminderStageBefore = "before"
	reminderStageStart  = "start"
)

// sendReminder writes one reminder and records that it went. Reports whether a
// message was actually posted.
func (s *Server) sendReminder(ev *Event, stage string) bool {
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		log.Printf("[discord-signup] reminder roster for event %d: %v", ev.ID, err)
		return false
	}
	attending, _ := splitRoster(roster)
	if len(attending) == 0 {
		// Nobody to tell. Stamped anyway: an empty event does not become worth
		// announcing later, and leaving it unstamped means checking it every
		// minute forever.
		s.stampReminder(ev, stage, "nobody has a place")
		return false
	}
	channelID := s.guildChannels(ev.GuildID).Reminder
	if channelID == "" {
		// Not stamped. Configuring the channel later must start reminding
		// about events that already exist, not find every one of them already
		// written off.
		return false
	}

	if _, err := s.discord.CreateMessage(channelID, renderReminder(ev, attending, stage)); err != nil {
		// Not stamped, so the next run tries again — within the grace window,
		// after which it gives up rather than pinging about something that has
		// already happened.
		log.Printf("[discord-signup] send %s reminder for event %d: %v", stage, ev.ID, err)
		return false
	}
	s.stampReminder(ev, stage, "")
	log.Printf("[discord-signup] %s reminder sent for event %d (%q) to %d people",
		stage, ev.ID, ev.Name, len(attending))
	return true
}

func (s *Server) stampReminder(ev *Event, stage, skipped string) {
	if err := s.store.StampReminder(ev.ID, stage); err != nil {
		log.Printf("[discord-signup] stamp %s reminder for event %d: %v", stage, ev.ID, err)
		return
	}
	if skipped != "" {
		log.Printf("[discord-signup] %s reminder for event %d (%q) skipped: %s",
			stage, ev.ID, ev.Name, skipped)
	}
}

// renderReminder writes the message. Mentions here are REAL — this is the one
// place the service means to ping, and allowed_mentions names each id rather
// than parsing the text, so it can never ping anybody the roster does not list.
func renderReminder(ev *Event, attending []Signup, stage string) map[string]any {
	var b strings.Builder
	if stage == reminderStageBefore {
		fmt.Fprintf(&b, "## %s starts in an hour\n", ev.Name)
	} else {
		fmt.Fprintf(&b, "## %s is starting now\n", ev.Name)
	}
	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, "🗓️ <t:%d:t>", ev.StartsAt)
		if ev.Location != "" {
			fmt.Fprintf(&b, "  ·  %s", ev.Location)
		}
		b.WriteString("\n")
	} else if ev.Location != "" {
		fmt.Fprintf(&b, "%s\n", ev.Location)
	}
	b.WriteString("\n")

	ids := make([]string, 0, len(attending))
	for i, sg := range attending {
		if i >= mentionsPerMessage {
			fmt.Fprintf(&b, " and %d more", len(attending)-mentionsPerMessage)
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "<@%s>", sg.DiscordUserID)
		ids = append(ids, sg.DiscordUserID)
	}

	return map[string]any{
		"content": b.String(),
		// The one deliberate ping. Named ids rather than "parse": ["users"],
		// because parsing would ping anything that LOOKS like a mention in a
		// name somebody chose.
		"allowed_mentions": map[string]any{"users": ids},
	}
}
