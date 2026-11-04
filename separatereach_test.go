package discordsignup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRemindersPingSeparateRolePeople: someone in the audience, not going,
// is pinged by the reminders after those going, and named in its
// allowed mentions; someone going and in the audience is pinged once.
func TestRemindersPingSeparateRolePeople(t *testing.T) {
	srv, store, fake := reminderServer(t)
	ev := reminderEvent(t, store, 30*time.Minute, "alice")
	switchRolesOn(t, store, ev.ID)
	audience, _ := store.AddSheetRole(ev.ID, "Audience", 6, false, "test")
	store.ToggleSheetRole(ev.ID, audience.ID, "jason", "Jason")
	store.ToggleSheetRole(ev.ID, audience.ID, "alice", "")

	if sent, err := srv.SendDueReminders(); err != nil || sent != 1 {
		t.Fatalf("sent %d, %v", sent, err)
	}
	msg := posted(fake)[0]
	content := msg.Body["content"].(string)
	if !strings.Contains(content, "<@alice> <@jason>") || strings.Count(content, "<@alice>") != 1 {
		t.Errorf("content = %q; want alice once, then jason", content)
	}
	if users, _ := msg.Body["allowed_mentions"].(map[string]any)["users"].([]any); len(users) != 2 {
		t.Errorf("allowed_mentions.users = %v", users)
	}
}

// TestSeparateRolePeopleFollowTheForumPost: taking a separate role follows
// the event's post as a place does, and leaving it takes them off at the
// next sweep, unless they are going too.
func TestSeparateRolePeopleFollowTheForumPost(t *testing.T) {
	fake, store, srv := forumFake(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		StartsAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	switchRolesOn(t, store, ev.ID)
	store.Join(ev.ID, "ann", "Ann", JoinedViaButton)
	audience, _ := store.AddSheetRole(ev.ID, "Audience", 6, false, "test")
	store.ToggleSheetRole(ev.ID, audience.ID, "jason", "Jason")
	store.ToggleSheetRole(ev.ID, audience.ID, "ann", "")
	if err := srv.refreshForumPost(mustEvent(t, store, ev.ID), true); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	ev = mustEvent(t, store, ev.ID)
	roster, _ := store.Roster(ev.ID, false)
	srv.settleForumFollows(ev, roster, nil)
	member := "/channels/msg-1/thread-members/"
	if len(callsTo(fake, http.MethodPut, member+"jason")) != 1 {
		t.Fatal("jason, in the audience, was not made to follow the post")
	}

	store.ToggleSheetRole(ev.ID, audience.ID, "jason", "")
	store.ToggleSheetRole(ev.ID, audience.ID, "ann", "")
	if _, _, err := srv.UnfollowDueForumPosts(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(callsTo(fake, http.MethodDelete, member+"jason")) != 1 {
		t.Error("jason left the audience and is still on the post")
	}
	if len(callsTo(fake, http.MethodDelete, member+"ann")) != 0 {
		t.Error("ann left the audience but is going, and was taken off the post")
	}
}
