package discordsignup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
)

// Publishing an event to Discord, and doing it exactly once at a time.
//
// Discord stores message text. There is no live rendering: a bot writes a
// message and Discord shows those words until somebody writes different ones.
// So every public surface this service has — the signup card, the forum post,
// the consolidated table line, the native scheduled event's title — is a copy
// of the roster that is only as current as the last write, and the whole job
// here is making sure the last write is the newest state.
//
// It was not. Every roster change spawned its own goroutine and they raced:
// four Interested clicks in thirty-three seconds started four syncs, each
// making five or six sequential Discord calls, and whichever finished last
// won. On 2 September the one that finished last had read a roster of three
// people five seconds before the one that had read two, and every Discord
// surface sat on 3/7 while the database and both web pages said 2/7. The
// single-retry-on-429 in discord.go made it likelier rather than safer: the
// sync most likely to be delayed is the one already behind.
//
// Two rules fix it, and neither is a workaround:
//
//	one writer per event   a second change arriving mid-sync does not start a
//	                       second sync; it marks the run dirty and the writer
//	                       does one more pass when it finishes. The re-read
//	                       happens inside that pass, so the last write always
//	                       carries the newest committed state, and a burst of
//	                       clicks costs two passes rather than one per click.
//
//	write only what changed a signature over everything that feeds a rendered
//	                       surface is stored when a sync fully succeeds. A pass
//	                       whose signature already matches makes no Discord
//	                       calls at all, which is what lets the ten-minute
//	                       reconcile re-check every live event for nothing and
//	                       repair the one whose write was lost.

// eventSyncQueue serialises Discord writes per event.
type eventSyncQueue struct {
	mu       sync.Mutex
	inFlight map[int64]*eventSyncRun
}

// eventSyncRun is one event's writer. dirty means another pass is owed, and is
// separate from changes because an edit that promoted nobody still has to be
// published.
type eventSyncRun struct {
	dirty   bool
	changes []stateChange
}

func newEventSyncQueue() *eventSyncQueue {
	return &eventSyncQueue{inFlight: map[int64]*eventSyncRun{}}
}

// enqueue registers a request to publish an event. It returns true if the
// caller is now the writer for that event, and false if a writer already has
// it — in which case the request has been handed to that writer and the caller
// is done.
func (q *eventSyncQueue) enqueue(eventID int64, changes []stateChange) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if run, running := q.inFlight[eventID]; running {
		run.dirty = true
		run.changes = append(run.changes, changes...)
		return false
	}
	q.inFlight[eventID] = &eventSyncRun{dirty: true, changes: changes}
	return true
}

// claim hands the writer the changes banked since its last pass. When it
// reports false there is nothing left owed and the event is released, so the
// next enqueue starts a fresh writer.
func (q *eventSyncQueue) claim(eventID int64) ([]stateChange, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	run, running := q.inFlight[eventID]
	if !running || !run.dirty {
		delete(q.inFlight, eventID)
		return nil, false
	}
	changes := run.changes
	run.changes, run.dirty = nil, false
	return changes, true
}

// syncAfterChange publishes what the database now says: the roles each affected
// person should hold, and every copy of the roster Discord is holding.
//
// Runs after the reply, never before it. The person who clicked must not wait
// on Discord's API for their answer, and a role that fails to apply must not
// make a successful signup look failed. Every failure here is logged loudly and
// none of them roll anything back — the roster is the source of truth and the
// Discord surfaces are projections of it, so the fix is to re-run the
// projection, which the ten-minute reconcile does by itself.
//
// Callers spawn this with `go`. Only the first caller for a given event does
// any work; the rest hand their changes over and return.
func (s *Server) syncAfterChange(eventID int64, changes []stateChange) {
	if s.discord == nil {
		return
	}
	if !s.syncs.enqueue(eventID, changes) {
		return
	}
	for {
		batch, owed := s.syncs.claim(eventID)
		if !owed {
			return
		}
		s.publishEventToDiscord(eventID, batch)
	}
}

// publishEventToDiscord is one pass: read the event, then write every surface
// that is out of date.
func (s *Server) publishEventToDiscord(eventID int64, changes []stateChange) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		log.Printf("[discord-signup] publish event=%d: reload: %v", eventID, err)
		return
	}
	roster, err := s.store.Roster(eventID, false)
	if err != nil {
		log.Printf("[discord-signup] publish event=%d: roster: %v", eventID, err)
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

	// Two separate questions. Has anything a reader can see changed since the
	// last successful publish? And, separately, is a throttled title rename now
	// due? The second can be yes while the first is no: five minutes pass with
	// nobody joining, and the count the title shows is still the one from
	// before the last three signups.
	signature := eventPublishSignature(ev, roster)
	wantNative := nativeEventName(ev)
	rename := titleRenameDue(ev, wantNative, now())
	if signature == ev.PublishedSignature && !rename {
		return
	}
	if signature == ev.PublishedSignature {
		// Title only. The card, the table and the description are already
		// current; only the two renames go, and only the titles are recorded.
		if err := s.renameForumPostOnly(ev); err != nil {
			log.Printf("[discord-signup] rename forum post for event %d: %v", ev.ID, err)
			return
		}
		if err := s.PushEditToDiscord(ev, roster, true); err != nil {
			log.Printf("[discord-signup] rename native event %d: %v", ev.ID, err)
			return
		}
		if err := s.store.SetTitlesWritten(ev.ID, wantNative, forumPostTitle(ev)); err != nil {
			log.Printf("[discord-signup] record titles for event %d: %v", ev.ID, err)
		}
		return
	}

	published := true
	if err := s.RefreshSignupMessage(ev.ID); err != nil {
		log.Printf("[discord-signup] refresh message event=%d: %v", ev.ID, err)
		published = false
	}
	// The table row is a second view of the same roster. One message, not the
	// whole table: this runs on every signup.
	s.refreshTablesQuietly(ev.GuildID)
	if err := s.refreshForumPost(ev, rename); err != nil {
		log.Printf("[discord-signup] refresh forum post for event %d: %v", ev.ID, err)
		published = false
	}
	// The native event's description carries the live count and names and
	// goes every time; its title goes only when titleRenameDue says so. Pushed
	// through the same function an edit uses rather than a second, lighter
	// one: two paths that both write the native event would eventually
	// disagree about what they write.
	if err := s.PushEditToDiscord(ev, roster, rename); err != nil {
		log.Printf("[discord-signup] push title for event %d: %v", ev.ID, err)
		published = false
	}

	// Only a clean sweep is recorded. A partial one leaves the old signature
	// in place, so the next reconcile tries the whole set again rather than
	// declaring an event published that is half written.
	if !published {
		return
	}
	if err := s.store.SetPublishedSignature(ev.ID, signature); err != nil {
		log.Printf("[discord-signup] record published signature for event %d: %v", ev.ID, err)
	}
	if rename {
		if err := s.store.SetTitlesWritten(ev.ID, wantNative, forumPostTitle(ev)); err != nil {
			log.Printf("[discord-signup] record titles for event %d: %v", ev.ID, err)
		}
	}
}

// publishFormatVersion is what the surfaces LOOK like, as opposed to what they
// say. Bump it in the same commit as any change to a renderer that writes to
// Discord — the card, the forum title, the native title or description.
//
// Without it a wording change never reaches anybody. The signature is taken
// over the event's inputs, so improving a sentence leaves every signature
// matching, every publish skipped, and the old words sitting on Discord until
// somebody happens to join. That is not hypothetical: the description rewrite
// that added the roster shipped, deployed, and would have changed nothing.
//
//	1  the original card, title badge and "Signups are in …" pointer
//	2  the roster listed in the native description, and the pointer reworded to
//	   stop calling the forum the place to sign up
//	3  the roster table, drawn beside the event table in the same channel
//	4  roster table rows link their thread instead of restating its title
//	5  Edit back on those rows: the merged Details+Edit modal was rejected
//	6  Edit off the roster table rows: Details is the edit form
//	7  one table, with the count in the row; titles carry [Full] or the limit
//	   instead of a live count
//	8  titles carry [X/Y] at the end again, renamed at most every five minutes,
//	   and [Full] at the front at once
const publishFormatVersion = 8

// eventPublishSignature covers everything that feeds a surface Discord stores.
//
// Deliberately taken over the INPUTS rather than the rendered output. A
// signature over the finished strings would have to be kept in step with every
// renderer, and the day somebody adds a field to the card and forgets this, the
// sweep stops repairing that field and nothing says so. Over the inputs it can
// only ever be too eager, which costs a redundant write.
//
// Two fields are deliberately absent, and both were in it once. ThreadID is
// written by ensureEventThread DURING a publish, so including it made every
// first publish dirty its own signature and write a second time. Nothing
// renders it — the card links the forum post, not the thread. And
// DiscordInterestedCount only ever appears in the details modal, which is built
// per click and cannot go stale, so an import moving it would have rewritten
// every card for a number no card carries.
func eventPublishSignature(ev *Event, roster []Signup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%d\x00", publishFormatVersion)
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		ev.Name, ev.Description, ev.Status, ev.Capacity, ev.StartsAt, ev.EndsAt,
		ev.Location, ev.Timezone, ev.MessageID, ev.ChannelID,
		ev.ForumPostID, ev.DiscordScheduledEventID)
	for _, sg := range roster {
		fmt.Fprintf(&b, "\x01%s\x00%s\x00%s", sg.DiscordUserID, sg.DisplayName, sg.State)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// RepublishStaleEvents re-checks every live event in a guild and rewrites the
// ones whose Discord copies do not match the roster.
//
// The backstop. A write can fail — a Discord 500, a network drop, a restart
// mid-sync — and until now a failed write left that surface wrong until the
// next time somebody happened to join. Nothing noticed and nothing retried.
// This runs from the same ten-minute reconcile that already repairs missing
// cards and cancelled events, and costs one query per event when everything
// agrees, which is almost always.
func (s *Server) RepublishStaleEvents(guildID string) {
	if s.discord == nil {
		return
	}
	events, err := s.liveEventsFor(guildID)
	if err != nil {
		log.Printf("[discord-signup] republish sweep for guild %s: %v", guildID, err)
		return
	}
	for i := range events {
		ev := &events[i]
		roster, err := s.store.Roster(ev.ID, false)
		if err != nil {
			log.Printf("[discord-signup] sweep event=%d: roster: %v", ev.ID, err)
			continue
		}
		stale := eventPublishSignature(ev, roster) != ev.PublishedSignature
		renameDue := titleRenameDue(ev, nativeEventName(ev), now())
		if !stale && !renameDue {
			continue
		}
		if !stale {
			// Not a discrepancy — a throttled count rename has come due, which
			// is the sweep doing its other job. Quiet, because it happens every
			// five minutes on every busy event and would drown the line below.
			s.syncAfterChange(ev.ID, nil)
			continue
		}
		// The one line worth reading in this log. Reaching here means a write
		// this service believed it had made is not the write Discord is
		// holding — a failed edit, a restart mid-publish, a rate limit that
		// outlived its retry. Named loudly and individually, because the point
		// of a repair loop nobody watches is that it leaves a record of how
		// often it had to repair anything.
		log.Printf("[discord-signup] sweep: event %d (%q) is stale — Discord was last written "+
			"%s, the roster now reads %s; republishing",
			ev.ID, ev.Name, describePublishState(ev), describeRosterCounts(ev))
		s.syncAfterChange(ev.ID, nil)
	}
}

// RepublishAllGuilds runs the sweep for every guild that has a live event.
//
// The guild list comes from the database, not from Discord. This runs every
// minute, and asking Discord who we are in every time would spend a call a
// minute to learn something that changes about once a year.
func (s *Server) RepublishAllGuilds() error {
	guilds, err := s.store.GuildsWithEvents()
	if err != nil {
		return err
	}
	for _, guildID := range guilds {
		s.RepublishStaleEvents(guildID)
	}
	return nil
}

// describePublishState says when this event was last successfully published, in
// the only terms available: whether it ever was, and which fingerprint.
func describePublishState(ev *Event) string {
	if ev.PublishedSignature == "" {
		return "never (or its last publish failed part way)"
	}
	return "as " + ev.PublishedSignature[:12]
}

// describeRosterCounts is the human half of the same line.
func describeRosterCounts(ev *Event) string {
	if ev.Capacity == 0 {
		return fmt.Sprintf("%d signed up, no limit, %d waiting",
			ev.AttendingCount, ev.WaitlistCount)
	}
	return fmt.Sprintf("%d/%d places taken, %d waiting",
		ev.AttendingCount, ev.Capacity, ev.WaitlistCount)
}
