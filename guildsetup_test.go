package discordsignup

import (
	"fmt"
	"net/http"
	"testing"
)

// TestSettingUpAServerMakesWhatIsMissingAndReusesWhatIsThere: a server that
// already has #events keeps it; the rest is created under an Events category;
// the row, the forum and the how-to all land; and a second run creates
// nothing.
func TestSettingUpAServerMakesWhatIsMissingAndReusesWhatIsThere(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	srv := NewServer(store, nil, fake.client())
	created := 0
	fake.on(http.MethodGet, "/guilds/g7/channels", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"ch-events","name":"Events","type":0},{"id":"ch-general","name":"general","type":0}]`)
	})
	fake.on(http.MethodPost, "/guilds/g7/channels", func(w http.ResponseWriter, r *http.Request) {
		// The fake has already read the body into the recorded call.
		calls := fake.recorded()
		in := calls[len(calls)-1].Body
		created++
		fmt.Fprintf(w, `{"id":"new-%s","name":"%s","type":%v}`, in["name"], in["name"], in["type"])
	})
	// The forum, once created, is read back for its tags.
	fake.on(http.MethodGet, "/channels/new-event-forum", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"new-event-forum","type":15,"available_tags":[{"id":"t1","name":"open"},{"id":"t2","name":"full"},{"id":"t3","name":"finished"},{"id":"t4","name":"cancelled"}]}`)
	})
	fake.on(http.MethodGet, "/channels/ch-events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"ch-events","guild_id":"g7","type":0}`)
	})

	table, err := srv.SetUpGuild("g7")
	if err != nil {
		t.Fatalf("set up: %v", err)
	}
	if table.BoardChannelID != "ch-events" || table.ChannelID != "ch-events" {
		t.Errorf("board/table = %s/%s, want the existing #events reused", table.BoardChannelID, table.ChannelID)
	}
	if table.ManagementChannelID != "new-event-management" || table.PastChannelID != "new-past-events" || table.ReminderChannelID != "new-event-reminders" {
		t.Errorf("row = %+v; want the missing channels created", table)
	}
	forum, err := store.GuildForum("g7")
	if err != nil || forum.ChannelID != "new-event-forum" || forum.TagFinished != "t3" {
		t.Errorf("forum = %+v, %v", forum, err)
	}
	if created != 5 { // category, management, past, reminders, forum
		t.Errorf("%d channels created, want 5", created)
	}
	var parented int
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/guilds/g7/channels" && c.Body["parent_id"] == "new-Events" {
			parented++
		}
	}
	if parented != 4 {
		t.Errorf("%d channels created under the category, want 4", parented)
	}
}
