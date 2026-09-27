package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// Forum post follows: getting a place on an event makes a person follow its
// forum post, so a question asked there reaches everyone going. Leaving takes
// them off it at once; a day after the event ends everyone is taken off. Only
// people this service added are ever taken off, and it records who they are
// in forum_post_follows. Each add and each removal leaves a line in the post
// that nobody can delete (Discord's own system messages, error 50021).

// forumFollowGrace is how long after an event ends — or after the date they
// were on rolls over — people stay on its forum post.
const forumFollowGrace = 24 * time.Hour

// forumFollow is one person this service made follow one event's post.
type forumFollow struct {
	EventID       int64
	DiscordUserID string
	FollowedAt    int64
}

func (s *Store) recordForumFollow(eventID int64, userID string) error {
	if _, err := s.db.Exec(`INSERT INTO forum_post_follows (event_id, discord_user_id, followed_at)
		VALUES (?,?,?) ON CONFLICT(event_id, discord_user_id)
		DO UPDATE SET followed_at = excluded.followed_at, unfollowed_at = 0`, eventID, userID, now()); err != nil {
		return fmt.Errorf("record %s following the post of %d: %w", userID, eventID, err)
	}
	return nil
}

func (s *Store) recordForumUnfollow(eventID int64, userID string) error {
	if _, err := s.db.Exec(`UPDATE forum_post_follows SET unfollowed_at = ?
		WHERE event_id = ? AND discord_user_id = ? AND unfollowed_at = 0`, now(), eventID, userID); err != nil {
		return fmt.Errorf("record %s unfollowing the post of %d: %w", userID, eventID, err)
	}
	return nil
}

func (s *Store) forumFollowIsLive(eventID int64, userID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM forum_post_follows
		WHERE event_id = ? AND discord_user_id = ? AND unfollowed_at = 0`, eventID, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("read forum follow: %w", err)
	}
	return n > 0, nil
}

func (s *Store) liveForumFollows() ([]forumFollow, error) {
	rows, err := s.db.Query(`SELECT event_id, discord_user_id, followed_at FROM forum_post_follows
		WHERE unfollowed_at = 0 ORDER BY event_id, discord_user_id`)
	if err != nil {
		return nil, fmt.Errorf("read forum follows: %w", err)
	}
	defer rows.Close()
	var out []forumFollow
	for rows.Next() {
		var f forumFollow
		if err := rows.Scan(&f.EventID, &f.DiscordUserID, &f.FollowedAt); err != nil {
			return nil, fmt.Errorf("scan forum follow: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// lastSignupUpdate is a person's newest update on an event, or nil.
func (s *Store) lastSignupUpdate(eventID int64, userID string) (*SignupUpdate, error) {
	var u SignupUpdate
	err := s.db.QueryRow(`SELECT id, event_id, discord_user_id, action, from_state, to_state, actor, at
		FROM signup_updates WHERE event_id = ? AND discord_user_id = ? ORDER BY at DESC, id DESC LIMIT 1`,
		eventID, userID).Scan(&u.ID, &u.EventID, &u.DiscordUserID, &u.Action, &u.FromState, &u.ToState, &u.Actor, &u.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last signup update: %w", err)
	}
	return &u, nil
}

// forumUnfollowDue says when someone this service made follow an event's
// post comes off it: 0 while they are going to a live event, a day after the
// event ended or was cancelled, a day after their date rolled over, and
// otherwise — they left, or went to the waitlist or Maybe — now.
func forumUnfollowDue(ev *Event, state string, last *SignupUpdate, at int64) int64 {
	grace := int64(forumFollowGrace / time.Second)
	switch ev.Status {
	case StatusCompleted:
		return finishedBy(ev) + grace
	case StatusCancelled:
		return ev.UpdatedAt + grace
	}
	if state == StateAttending {
		return 0
	}
	if last != nil && last.Actor == ActorRecurrence && last.ToState == StateWithdrawn {
		return last.At + grace
	}
	return at
}

// followForumPost makes each person follow the event's post, unless this
// service already did. A failure is logged and counted, and the rest are
// still tried.
func (s *Server) followForumPost(ev *Event, userIDs []string) (followed, failed int) {
	for _, userID := range userIDs {
		live, err := s.store.forumFollowIsLive(ev.ID, userID)
		if err != nil {
			log.Printf("[discord-signup] %v", err)
			failed++
			continue
		}
		if live {
			continue
		}
		if err := s.discord.AddThreadMember(ev.ForumPostID, userID); err != nil {
			log.Printf("[discord-signup] %s follow forum post %s of event %d: %v", userID, ev.ForumPostID, ev.ID, err)
			failed++
			continue
		}
		if err := s.store.recordForumFollow(ev.ID, userID); err != nil {
			log.Printf("[discord-signup] %v", err)
			failed++
			continue
		}
		followed++
	}
	return followed, failed
}

// unfollowForumPost takes someone this service added off the event's post.
// The thread must not be archived.
func (s *Server) unfollowForumPost(ev *Event, userID string) error {
	if err := s.discord.RemoveThreadMember(ev.ForumPostID, userID); err != nil {
		return fmt.Errorf("%s unfollow forum post %s of event %d: %w", userID, ev.ForumPostID, ev.ID, err)
	}
	return s.store.recordForumUnfollow(ev.ID, userID)
}

// settleForumFollows follows or unfollows the post for each change on a live
// event, at once: a place follows it, and leaving it unfollows. A date rolling
// over waits a day, for UnfollowDueForumPosts.
func (s *Server) settleForumFollows(ev *Event, roster []Signup, changes []stateChange) {
	var going []string
	for _, change := range changes {
		if change.State == StateAttending {
			going = append(going, change.UserID)
			continue
		}
		live, err := s.store.forumFollowIsLive(ev.ID, change.UserID)
		if err != nil || !live {
			if err != nil {
				log.Printf("[discord-signup] %v", err)
			}
			continue
		}
		last, err := s.store.lastSignupUpdate(ev.ID, change.UserID)
		if err != nil {
			log.Printf("[discord-signup] %v", err)
			continue
		}
		at := now()
		if due := forumUnfollowDue(ev, stateIn(roster, change.UserID), last, at); due == 0 || due > at {
			continue
		}
		if err := s.unfollowForumPost(ev, change.UserID); err != nil {
			log.Printf("[discord-signup] %v", err)
		}
	}
	s.followForumPost(ev, going)
}

// stateIn is someone's state on a roster, or "" when they are not on it.
func stateIn(roster []Signup, userID string) string {
	for _, sg := range roster {
		if sg.DiscordUserID == userID {
			return sg.State
		}
	}
	return ""
}

// UnfollowDueForumPosts takes off each person this service added to a post
// whose time to come off has passed. An archived post is reopened for it and
// archived again. An event that no longer exists is let go without touching
// Discord.
func (s *Server) UnfollowDueForumPosts() (removed, failed int, err error) {
	if s.discord == nil {
		return 0, 0, errors.New("unfollowing a forum post needs a Discord client")
	}
	follows, err := s.store.liveForumFollows()
	if err != nil {
		return 0, 0, err
	}
	byEvent := map[int64][]forumFollow{}
	var order []int64
	for _, f := range follows {
		if _, seen := byEvent[f.EventID]; !seen {
			order = append(order, f.EventID)
		}
		byEvent[f.EventID] = append(byEvent[f.EventID], f)
	}
	at := now()
	for _, eventID := range order {
		ev, err := s.store.GetEvent(eventID)
		if errors.Is(err, ErrNotFound) {
			for _, f := range byEvent[eventID] {
				if err := s.store.recordForumUnfollow(eventID, f.DiscordUserID); err != nil {
					return removed, failed, err
				}
			}
			continue
		}
		if err != nil {
			return removed, failed, err
		}
		roster, err := s.store.Roster(eventID, true)
		if err != nil {
			return removed, failed, err
		}
		var due []string
		for _, f := range byEvent[eventID] {
			last, err := s.store.lastSignupUpdate(eventID, f.DiscordUserID)
			if err != nil {
				return removed, failed, err
			}
			if when := forumUnfollowDue(ev, stateIn(roster, f.DiscordUserID), last, at); when != 0 && when <= at {
				due = append(due, f.DiscordUserID)
			}
		}
		if len(due) == 0 || ev.ForumPostID == "" {
			continue
		}
		archived := IsArchived(ev.Status)
		if archived {
			if err := s.discord.ModifyThread(ev.ForumPostID, map[string]any{"archived": false}); err != nil {
				log.Printf("[discord-signup] reopen forum post %s of event %d to unfollow: %v", ev.ForumPostID, ev.ID, err)
				failed += len(due)
				continue
			}
		}
		for _, userID := range due {
			if err := s.unfollowForumPost(ev, userID); err != nil {
				log.Printf("[discord-signup] %v", err)
				failed++
				continue
			}
			removed++
		}
		if archived {
			if err := s.discord.ModifyThread(ev.ForumPostID, map[string]any{"archived": true}); err != nil {
				log.Printf("[discord-signup] archive forum post %s of event %d again: %v", ev.ForumPostID, ev.ID, err)
			}
		}
	}
	return removed, failed, nil
}

// FollowAllForumPosts makes everyone going to a live event follow its forum
// post: for people who got their place before getting one did it.
func (s *Server) FollowAllForumPosts() (followed, failed int, err error) {
	if s.discord == nil {
		return 0, 0, errors.New("following a forum post needs a Discord client")
	}
	for _, status := range []string{StatusOpen, StatusClosed} {
		events, err := s.store.ListEvents("", status, 500)
		if err != nil {
			return followed, failed, err
		}
		for i := range events {
			ev := &events[i]
			if ev.ForumPostID == "" {
				continue
			}
			roster, err := s.store.Roster(ev.ID, false)
			if err != nil {
				return followed, failed, err
			}
			var going []string
			for _, sg := range roster {
				if sg.State == StateAttending {
					going = append(going, sg.DiscordUserID)
				}
			}
			// A quiet post is archived, and an archived thread takes no new
			// members; the same patch the card refresh sends reopens it.
			if err := s.discord.ModifyThread(ev.ForumPostID, map[string]any{"archived": false}); err != nil {
				log.Printf("[discord-signup] reopen forum post %s of event %d: %v", ev.ForumPostID, ev.ID, err)
			}
			f, x := s.followForumPost(ev, going)
			followed, failed = followed+f, failed+x
		}
	}
	return followed, failed, nil
}
