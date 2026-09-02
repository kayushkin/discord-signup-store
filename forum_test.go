package discordsignup

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func forumFake(t *testing.T) (*fakeDiscord, *Store, *Server) {
	t.Helper()
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	srv.EnableWeb(nil, "board")
	if err := store.SetGuildForum(GuildForum{
		GuildID: "g1", ChannelID: "forum-ch",
		TagOpen: "t-open", TagFull: "t-full",
		TagFinished: "t-fin", TagCancelled: "t-can",
	}); err != nil {
		t.Fatalf("set forum: %v", err)
	}
	return fake, store, srv
}

// TestForumPostCarriesTheSameCardAndButtons: the first message is
// RenderSignupMessage verbatim, so one handler serves both surfaces.
func TestForumPostCarriesTheSameCardAndButtons(t *testing.T) {
	fake, store, srv := forumFake(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Capacity: 4, StartsAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.refreshForumPost(ev); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var created map[string]any
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/forum-ch/threads" {
			created = c.Body
		}
	}
	if created == nil {
		t.Fatal("no forum post created")
	}
	if tags, _ := created["applied_tags"].([]any); len(tags) != 1 || tags[0] != "t-open" {
		t.Errorf("applied_tags = %v, want the open tag", created["applied_tags"])
	}
	body := fmt.Sprint(created["message"])
	if !strings.Contains(body, JoinCustomID(ev.ID)) || !strings.Contains(body, EditCustomID(ev.ID)) {
		t.Error("the post's card is missing the standard buttons")
	}
	got, _ := store.GetEvent(ev.ID)
	if got.ForumPostID == "" {
		t.Error("the post id was not recorded")
	}
}

// TestForumTagFlipsWhenTheEventFills: full events wear the full tag, and the
// title badge follows the count.
func TestForumTagFlipsWhenTheEventFills(t *testing.T) {
	fake, store, srv := forumFake(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Capacity: 1, StartsAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.refreshForumPost(ev); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaButton); err != nil {
		t.Fatalf("join: %v", err)
	}
	full, _ := store.GetEvent(ev.ID)
	if err := srv.refreshForumPost(full); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	var patch map[string]any
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/channels/"+full.ForumPostID {
			patch = c.Body
		}
	}
	if patch == nil {
		t.Fatal("the post was never retitled")
	}
	if tags, _ := patch["applied_tags"].([]any); len(tags) != 1 || tags[0] != "t-full" {
		t.Errorf("applied_tags = %v, want the full tag", patch["applied_tags"])
	}
	// Full is the one state a title is allowed to announce: it flips rarely,
	// and a rename is rate-limited, so a live count here was always stale.
	if name, _ := patch["name"].(string); !strings.HasPrefix(name, "[Full] Games") {
		t.Errorf("title = %q, want [Full] on it", name)
	}
}

// TestFinishedEventsArchiveTheirForumPost, tagged finished; and an event that
// is already over never gets a post at all.
func TestFinishedEventsArchiveTheirForumPost(t *testing.T) {
	fake, store, srv := forumFake(t)
	past := time.Now().Add(-10 * time.Hour).Unix()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Over soon",
		StartsAt: past + 20*3600, EndsAt: past + 21*3600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.refreshForumPost(ev); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	withPost, _ := store.GetEvent(ev.ID)
	newStart, newEnd := past-3600, past
	if _, err := store.UpdateEvent(ev.ID, EventPatch{StartsAt: &newStart, EndsAt: &newEnd}); err != nil {
		t.Fatalf("re-date: %v", err)
	}
	if _, err := srv.CompleteFinishedEvents(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var archived, tagged bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPatch && c.Path == "/channels/"+withPost.ForumPostID {
			if v, _ := c.Body["archived"].(bool); v {
				archived = true
			}
			if tags, _ := c.Body["applied_tags"].([]any); len(tags) == 1 && tags[0] == "t-fin" {
				tagged = true
			}
		}
	}
	if !archived || !tagged {
		t.Errorf("archived=%v tagged-finished=%v, want both", archived, tagged)
	}

	// And a fresh event that is already archived gets no post.
	gone, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Ancient",
		Status: StatusCancelled, StartsAt: past})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := len(fake.recorded())
	if err := srv.refreshForumPost(gone); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, c := range fake.recorded()[before:] {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/threads") {
			t.Error("a post was opened for an event that is already over")
		}
	}
}

func TestForumPostTitle(t *testing.T) {
	zone := mustZone(t, "America/Los_Angeles")
	ev := &Event{Name: "Board games", Capacity: 8, AttendingCount: 3,
		Timezone: "America/Los_Angeles",
		StartsAt: time.Date(2026, 8, 29, 16, 0, 0, 0, zone).Unix()}
	if got := forumPostTitle(ev); got != "Board games — 8/29 4pm · 8 places" {
		t.Errorf("forumPostTitle = %q", got)
	}
	uncapped := &Event{Name: "Open house", StartsAt: ev.StartsAt, Timezone: ev.Timezone}
	if got := forumPostTitle(uncapped); got != "Open house — 8/29 4pm" {
		t.Errorf("forumPostTitle = %q, want no badge without a limit", got)
	}
}

// TestEventByForumPostIDJoinsOnTheStoredId: a reaction carries only the message
// it landed on, and this lookup is how it becomes an event.
func TestEventByForumPostIDJoinsOnTheStoredId(t *testing.T) {
	store := testStore(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		StartsAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	postID := "post-123"
	if _, err := store.UpdateEvent(ev.ID, EventPatch{ForumPostID: &postID}); err != nil {
		t.Fatalf("link: %v", err)
	}
	got, err := store.EventByForumPostID("post-123")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != ev.ID {
		t.Errorf("found event %d, want %d", got.ID, ev.ID)
	}
	if _, err := store.EventByForumPostID("no-such-post"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown post = %v, want ErrNotFound", err)
	}
}

// TestNewForumPostsAreSeededWithTheJoinReaction: without the seed, a channel
// that denies ADD_REACTIONS has nothing to click and the whole design is dead.
func TestNewForumPostsAreSeededWithTheJoinReaction(t *testing.T) {
	fake, store, srv := forumFake(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Capacity: 4, StartsAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.refreshForumPost(ev); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var seeded bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPut && strings.Contains(c.Path, "/reactions/") &&
			strings.HasSuffix(c.Path, "/@me") {
			seeded = true
		}
	}
	if !seeded {
		t.Error("the post was not seeded with the bot's own ✅")
	}
}

// TestLeavingByButtonClearsTheReaction keeps the ✅ truthful: a reaction that no
// longer means membership would teach everyone to distrust it.
func TestLeavingByButtonClearsTheReaction(t *testing.T) {
	fake, store, srv := forumFake(t)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "board", Name: "Games",
		Capacity: 4, StartsAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.refreshForumPost(ev); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := store.Join(ev.ID, "alice", "Alice", JoinedViaReaction); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := store.Leave(ev.ID, "alice", ActorUser); err != nil {
		t.Fatalf("leave: %v", err)
	}
	fresh, _ := store.GetEvent(ev.ID)
	srv.syncAfterChange(fresh.ID, []stateChange{{UserID: "alice", State: StateWithdrawn}})

	var cleared bool
	for _, c := range fake.recorded() {
		if c.Method == http.MethodDelete && strings.Contains(c.Path, "/reactions/") &&
			strings.HasSuffix(c.Path, "/alice") {
			cleared = true
		}
	}
	if !cleared {
		t.Error("alice's ✅ was left on the post after she left by button")
	}
}

// TestSurfacesLinkToTheForumPostExceptTheForumItself: the card and the table
// row point at the discussion; the forum post's own first message IS the card
// and must not link to itself.
func TestSurfacesLinkToTheForumPostExceptTheForumItself(t *testing.T) {
	ev := &Event{ID: 1, GuildID: "g1", Name: "Games", Capacity: 4, AttendingCount: 1,
		StartsAt: time.Now().Add(time.Hour).Unix(), Status: StatusOpen,
		ForumPostID: "post-9"}

	card := fmt.Sprint(RenderSignupMessage(ev, nil)["content"])
	if !strings.Contains(card, "<#post-9>") {
		t.Error("the board card does not link to the forum post")
	}
	forumCard := fmt.Sprint(RenderForumCard(ev, nil)["content"])
	if strings.Contains(forumCard, "post-9") {
		t.Error("the forum card links to its own post")
	}
	// The table and the My events dashboard share eventLine, so this one
	// assertion covers both — and it is the SAME reference the card uses.
	line := eventLine(ev)
	if !strings.Contains(line, "💬 <#post-9>") {
		t.Errorf("the table line does not reference the forum the way the card does: %q", line)
	}
	// The native event points at the forum as the DISCUSSION, not as the place
	// to sign up. Naming one surface as "where signups are" sent people away
	// from the Interested button already in front of them, which signs them up.
	ev.Description = "Real description."
	block := nativeEventDescription(ev, nil, "board")
	if !strings.Contains(block, "Chat about it: https://discord.com/channels/g1/post-9") {
		t.Errorf("the native description should link the forum as discussion: %q", block)
	}
	if !strings.Contains(block, "**Interested** here") {
		t.Errorf("the native description should say Interested signs you up: %q", block)
	}
	if strings.Contains(block, "Signups are in") {
		t.Errorf("the native description still calls the forum the place to sign up: %q", block)
	}
	// And the round-trip strip still removes the whole block, link included.
	if got := stripSignupPointer(block); got != "Real description." {
		t.Errorf("stripSignupPointer left %q", got)
	}

	// No post yet: no dangling links anywhere.
	bare := &Event{ID: 2, GuildID: "g1", Name: "Fresh", Status: StatusOpen}
	if strings.Contains(fmt.Sprint(RenderSignupMessage(bare, nil)["content"]), "💬") {
		t.Error("a card links to a post that does not exist")
	}
	if strings.Contains(eventLine(bare), "💬") {
		t.Error("a table line links to a post that does not exist")
	}
}
