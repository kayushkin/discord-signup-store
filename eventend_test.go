package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func managementLabels(ev *Event) string {
	out := []string{}
	for _, b := range managementButtons(ev) {
		out = append(out, b.(map[string]any)["label"].(string))
	}
	return strings.Join(out, ",")
}

// TestEndIsOnTheRowOnlyWhileTheEventIsUnderway.
func TestEndIsOnTheRowOnlyWhileTheEventIsUnderway(t *testing.T) {
	started := time.Now().Add(-time.Hour).Unix()
	ahead := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"open, underway", Event{ID: 1, Status: StatusOpen, StartsAt: started},
			"Edit,Repeat,Close signups,End,Cancel"},
		{"closed, underway", Event{ID: 1, Status: StatusClosed, StartsAt: started},
			"Edit,Repeat,Reopen signups,End,Cancel"},
		{"open, not started", Event{ID: 1, Status: StatusOpen, StartsAt: ahead},
			"Edit,Repeat,Close signups,Cancel"},
		{"no start time", Event{ID: 1, Status: StatusOpen},
			"Edit,Repeat,Close signups,Cancel"},
		{"completed", Event{ID: 1, Status: StatusCompleted, StartsAt: started},
			"Edit,Repeat,Cancel"},
		{"cancelled", Event{ID: 1, Status: StatusCancelled, StartsAt: started},
			"Edit,Repeat,Cancel"},
	}
	for _, c := range cases {
		if got := managementLabels(&c.ev); got != c.want {
			t.Errorf("%s: row = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestNoManagementRowHoldsMoreThanFiveButtons: five is all one action row
// holds, and a sixth makes Discord refuse the whole table page.
func TestNoManagementRowHoldsMoreThanFiveButtons(t *testing.T) {
	for _, status := range []string{StatusOpen, StatusClosed, StatusCompleted, StatusCancelled} {
		for _, startsAt := range []int64{0, time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()} {
			ev := &Event{ID: 1, Status: status, StartsAt: startsAt}
			if n := len(managementButtons(ev)); n > 5 {
				t.Errorf("status %s, starts %d: %d buttons on one row (%s)", status, startsAt, n, managementLabels(ev))
			}
		}
	}
}

// TestTheManagementTableCountsTheEndButton: a page of underway events costs
// one component more per row, the packer knows it, and no page goes over.
func TestTheManagementTableCountsTheEndButton(t *testing.T) {
	underway := rosterTableEvents(30)
	for i := range underway {
		underway[i].StartsAt = time.Now().Add(-time.Hour).Unix()
	}
	ahead := rosterTableEvents(30)
	for i := range ahead {
		ahead[i].StartsAt = time.Now().Add(time.Hour).Unix()
	}
	reserve := len(managementTrailing()) + 2
	underwayPages := packEventTable(underway, nil, managementButtons, nil, reserve)
	aheadPages := packEventTable(ahead, nil, managementButtons, nil, reserve)
	if len(underwayPages[0]) >= len(aheadPages[0]) {
		t.Errorf("underway rows carry End but a page still holds %d of them, the same as %d without it",
			len(underwayPages[0]), len(aheadPages[0]))
	}
	for i, page := range underwayPages {
		payload := RenderEventTablePage(page, i, len(underwayPages), managementLeading(), managementTrailing())
		rendered := countComponents(payload["components"].([]any))
		if rendered > eventTableComponentBudget {
			t.Errorf("page %d renders %d components, over %d", i, rendered, eventTableComponentBudget)
		}
		if !strings.Contains(fmtLabels(payload), "End") {
			t.Errorf("page %d has no End button", i)
		}
	}
}

// TestARowRendersTheButtonsItWasMeasuredWith: End can appear between packing
// and drawing when the start time passes in between. The page must draw the
// row it measured, or a full page goes over Discord's cap.
func TestARowRendersTheButtonsItWasMeasuredWith(t *testing.T) {
	calls := 0
	growing := func(ev *Event) []any {
		calls++
		buttons := []any{}
		for i := 0; i < calls; i++ {
			buttons = append(buttons, map[string]any{"type": componentTypeButton, "label": "b", "custom_id": "x"})
		}
		return buttons
	}
	events := rosterTableEvents(1)
	page := packEventTable(events, nil, growing, nil, 0)[0]
	body := RenderEventTablePage(page, 0, 1, nil, nil)["components"].([]any)[0].(map[string]any)["components"].([]any)
	row := body[1].(map[string]any)["components"].([]any)
	if len(row) != len(page[0].buttons) || page[0].components != 2+len(row) {
		t.Errorf("measured %d components with %d buttons, rendered a row of %d",
			page[0].components, len(page[0].buttons), len(row))
	}
}

func fmtLabels(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// TestTheStartOfAnEventMakesItsSurfacesStale: nothing is written when an event
// starts, so without this the row would not gain End until the next signup.
func TestTheStartOfAnEventMakesItsSurfacesStale(t *testing.T) {
	// The same event, signed a moment before its start and a moment after:
	// only the clock differs.
	ev := &Event{ID: 1, Status: StatusOpen, StartsAt: time.Now().Unix() + 1}
	before := eventPublishSignature(ev, nil)
	time.Sleep(time.Until(time.Unix(ev.StartsAt, 0)) + 50*time.Millisecond)
	if eventPublishSignature(ev, nil) == before {
		t.Error("an event that has started signs the same as one that has not")
	}
}

func underwayEvent(t *testing.T, store *Store, rule string) *Event {
	t.Helper()
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Games",
		Status: StatusOpen, StartsAt: time.Now().Add(-time.Hour).Unix(),
		EndsAt: time.Now().Add(time.Hour).Unix(), RecurrenceRule: rule, Timezone: "America/Los_Angeles",
		DiscordScheduledEventID: "native-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return ev
}

// TestEndingAOneOffEventCompletesItOnce.
func TestEndingAOneOffEventCompletesItOnce(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev := underwayEvent(t, store, "")

	result, err := srv.EndEventNow(ev, "u-admin")
	if err != nil || result.RolledTo != 0 || result.AlreadyEnded {
		t.Fatalf("end = %+v, %v", result, err)
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	// Somebody else — the time sweep, a second press — got there with the
	// copy they read before it ended.
	result, err = srv.EndEventNow(ev, "u-admin")
	if err != nil || !result.AlreadyEnded {
		t.Errorf("second end = %+v, %v; want already ended", result, err)
	}
	updates, err := store.EventUpdates(ev.ID)
	if err != nil {
		t.Fatalf("updates: %v", err)
	}
	logged := 0
	for _, u := range updates {
		if u.Field == "status" && u.ToValue == StatusCompleted && u.Actor == "u-admin" {
			logged++
		}
	}
	if logged != 1 {
		t.Errorf("want one logged end by u-admin, got %d in %+v", logged, updates)
	}
}

// TestAnEventThatHasNotStartedCannotBeEnded: Cancel is for that.
func TestAnEventThatHasNotStartedCannotBeEnded(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev, err := store.CreateEvent(Event{GuildID: "g1", ChannelID: "c1", Name: "Later",
		Status: StatusOpen, StartsAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.EndEventNow(ev, "u-admin"); err != errEventNotUnderway {
		t.Errorf("err = %v, want errEventNotUnderway", err)
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
}

// TestEndingARecurringEventEndsThisDateOnly: it moves on, like its end time
// passing would move it, and stays open.
func TestEndingARecurringEventEndsThisDateOnly(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev := underwayEvent(t, store, "FREQ=WEEKLY")

	result, err := srv.EndEventNow(ev, "u-admin")
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if result.RolledTo <= time.Now().Unix() {
		t.Fatalf("rolled to %d, want a date ahead", result.RolledTo)
	}
	if err := srv.settleEndedEvent(ev, result); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _ := store.GetEvent(ev.ID)
	if got.Status != StatusOpen || got.StartsAt != result.RolledTo {
		t.Errorf("after End: status %q starts %d, want open at %d", got.Status, got.StartsAt, result.RolledTo)
	}
}

// TestEndingTheNativeEventStartsItFirstIfItMustAndLeavesAnEndedOneAlone.
func TestEndingTheNativeEventStartsItFirstIfItMustAndLeavesAnEndedOneAlone(t *testing.T) {
	path := "/guilds/g1/scheduled-events/native-1"
	for _, c := range []struct {
		native  int
		patches []float64
	}{
		{discordEventScheduled, []float64{discordEventActive, discordEventCompleted}},
		{discordEventActive, []float64{discordEventCompleted}},
		{discordEventCompleted, nil},
		{discordEventCanceled, nil},
	} {
		fake := newFakeDiscord(t)
		status := c.native
		fake.on(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"id": "native-1", "status": status})
		})
		if err := fake.client().EndScheduledEvent("g1", "native-1"); err != nil {
			t.Fatalf("native status %d: %v", c.native, err)
		}
		var patches []float64
		for _, call := range fake.recorded() {
			if call.Method == http.MethodPatch && call.Path == path {
				patches = append(patches, call.Body["status"].(float64))
			}
		}
		if len(patches) != len(c.patches) {
			t.Errorf("native status %d: patched %v, want %v", c.native, patches, c.patches)
			continue
		}
		for i := range patches {
			if patches[i] != c.patches[i] {
				t.Errorf("native status %d: patched %v, want %v", c.native, patches, c.patches)
			}
		}
	}

	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Guild Scheduled Event","code":10070}`))
	})
	if err := fake.client().EndScheduledEvent("g1", "native-1"); err != nil {
		t.Errorf("a deleted native event: %v, want nothing to do", err)
	}
}

// TestTheEndButtonAsksThenEnds, through the same custom ids the row carries.
func TestTheEndButtonAsksThenEnds(t *testing.T) {
	store := testStore(t)
	fake := newFakeDiscord(t)
	fake.on(http.MethodGet, "/guilds/g1/scheduled-events/native-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "native-1", "status": discordEventActive})
	})
	srv := NewServer(store, nil, fake.client())
	ev := underwayEvent(t, store, "")

	action, id, ok := parseCustomID(EndCustomID(ev.ID))
	if !ok || action != "end" || id != ev.ID {
		t.Fatalf("End custom id parses as %q %d %v", action, id, ok)
	}
	rec := httptest.NewRecorder()
	srv.handleEndButton(rec, adminInteraction(t, EndCustomID(ev.ID), nil), ev.ID)
	if !strings.Contains(rec.Body.String(), EndConfirmCustomID(ev.ID)) {
		t.Fatalf("End did not ask first: %s", rec.Body.String())
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Fatalf("pressing End alone changed the status to %q", got.Status)
	}

	action, _, ok = parseCustomID(EndConfirmCustomID(ev.ID))
	if !ok || action != "end-confirm" {
		t.Fatalf("confirm custom id parses as %q %v", action, ok)
	}
	rec = httptest.NewRecorder()
	srv.applyEndConfirm(rec, adminInteraction(t, EndConfirmCustomID(ev.ID), nil), ev.ID)
	if !strings.Contains(replyText(rec), "has ended") {
		t.Errorf("confirm answered %q", replyText(rec))
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	// The native event is ended after the answer, off the press's clock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range fake.recorded() {
			if call.Method == http.MethodPatch && call.Path == "/guilds/g1/scheduled-events/native-1" &&
				call.Body["status"] == float64(discordEventCompleted) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the native event was never ended")
}

// TestOnlyAnEditorCanEnd.
func TestOnlyAnEditorCanEnd(t *testing.T) {
	store := testStore(t)
	srv := NewServer(store, nil, nil)
	ev := underwayEvent(t, store, "")
	var in Interaction
	json.Unmarshal([]byte(`{"guild_id":"g1","member":{"permissions":"0","user":{"id":"u-nobody"}}}`), &in)

	for _, press := range []func(http.ResponseWriter, *Interaction, int64){srv.handleEndButton, srv.applyEndConfirm} {
		rec := httptest.NewRecorder()
		press(rec, &in, ev.ID)
		if !strings.Contains(replyText(rec), "Manage Events") {
			t.Errorf("a member without Manage Events was answered %q", replyText(rec))
		}
	}
	if got, _ := store.GetEvent(ev.ID); got.Status != StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
}

// TestTheWebPageOffersEndOnlyWhileUnderway renders the real template.
func TestTheWebPageOffersEndOnlyWhileUnderway(t *testing.T) {
	srv := NewServer(testStore(t), nil, nil)
	session := &WebSession{DiscordUserID: "u-admin", DisplayName: "Admin"}
	for _, c := range []struct {
		underway, recurring bool
	}{{true, false}, {true, true}, {false, false}} {
		ev := &Event{ID: 7, GuildID: "g1", Name: "Games", Status: StatusOpen, StartsAt: time.Now().Unix()}
		if c.recurring {
			ev.RecurrenceRule = "FREQ=WEEKLY"
		}
		rec := httptest.NewRecorder()
		srv.render(rec, "detail.html", pageData{Title: ev.Name, Session: session, Event: ev,
			CanManage: true, EventUnderway: c.underway})
		page := rec.Body.String()
		hasForm := strings.Contains(page, `action="/events/7/end"`)
		if hasForm != c.underway {
			t.Errorf("underway=%v: End form present = %v", c.underway, hasForm)
		}
		if c.underway && c.recurring && !strings.Contains(page, "End this date now?") {
			t.Errorf("a recurring event's End does not say it ends this date only")
		}
	}
}
