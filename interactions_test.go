package discordsignup

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signedRequest builds a request signed the way Discord signs one: over the
// timestamp header concatenated with the raw body.
func signedRequest(t *testing.T, priv ed25519.PrivateKey, timestamp, body string) *http.Request {
	t.Helper()
	sig := ed25519.Sign(priv, []byte(timestamp+body))
	req := httptest.NewRequest(http.MethodPost, "/interactions", strings.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", timestamp)
	return req
}

func testServer(t *testing.T) (*Server, ed25519.PrivateKey, *Store) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier, err := NewInteractionVerifier(hex.EncodeToString(pub))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	store := testStore(t)
	// nil Discord client: the roster must work with nothing to push to.
	return NewServer(store, verifier, nil), priv, store
}

// TestPingIsAnsweredWithPong covers the handshake Discord performs when the
// Interactions Endpoint URL is saved. Fail this and the URL will not save.
func TestPingIsAnsweredWithPong(t *testing.T) {
	srv, priv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", `{"type":1}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Type int `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Type != callbackTypePong {
		t.Errorf("type = %d, want %d (PONG)", out.Type, callbackTypePong)
	}
}

// TestBadSignatureIsRejectedWith401 is not only a security control. Discord
// probes a registered endpoint with deliberately invalid signatures and refuses
// to accept one that answers anything but 401, so this is what makes the
// endpoint installable at all.
func TestBadSignatureIsRejectedWith401(t *testing.T) {
	srv, priv, _ := testServer(t)

	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"body swapped after signing", func(r *http.Request) {
			// The signature covers the bytes, so a different body must fail
			// even though it is valid JSON and the headers are untouched.
			r.Body = io.NopCloser(strings.NewReader(`{"type":1,"tampered":true}`))
		}},
		{"wrong signature", func(r *http.Request) {
			r.Header.Set("X-Signature-Ed25519", strings.Repeat("00", ed25519.SignatureSize))
		}},
		{"signature not hex", func(r *http.Request) {
			r.Header.Set("X-Signature-Ed25519", "not-hex-at-all")
		}},
		{"missing signature header", func(r *http.Request) {
			r.Header.Del("X-Signature-Ed25519")
		}},
		{"missing timestamp header", func(r *http.Request) {
			r.Header.Del("X-Signature-Timestamp")
		}},
		{"replayed against a different timestamp", func(r *http.Request) {
			r.Header.Set("X-Signature-Timestamp", "1700009999")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := signedRequest(t, priv, "1700000000", `{"type":1}`)
			tc.mut(req)
			rec := httptest.NewRecorder()
			srv.HandleInteraction(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestButtonClickJoinsAndAnswersPrivately walks the path a real signup takes.
func TestButtonClickJoinsAndAnswersPrivately(t *testing.T) {
	srv, priv, store := testServer(t)
	ev := testEvent(t, store, 1)

	click := func(action, userID string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{
			"type": 3,
			"guild_id": "g1",
			"channel_id": "c1",
			"data": {"custom_id": "signup:%s:%d"},
			"member": {"nick": "", "user": {"id": %q, "username": %q}}
		}`, action, ev.ID, userID, userID)
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", body))
		return rec
	}

	rec := click("join", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var reply struct {
		Type int `json:"type"`
		Data struct {
			Content string `json:"content"`
			Flags   int    `json:"flags"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.Data.Flags != messageFlagEphemeral {
		t.Errorf("flags = %d, want %d — the answer must be private to the clicker",
			reply.Data.Flags, messageFlagEphemeral)
	}
	if !strings.Contains(reply.Data.Content, "You're in") {
		t.Errorf("content = %q, want a confirmation", reply.Data.Content)
	}

	// The second person hits the cap and must be told their place in line, not
	// left to assume they got in.
	rec = click("join", "bob")
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(reply.Data.Content, "waitlist") {
		t.Errorf("content = %q, want the waitlist position stated", reply.Data.Content)
	}

	roster, err := store.Roster(ev.ID, false)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("roster has %d people, want 2", len(roster))
	}
	if roster[0].State != StateAttending || roster[1].State != StateWaitlisted {
		t.Errorf("roster states = %q, %q; want attending, waitlisted",
			roster[0].State, roster[1].State)
	}
}

// TestForeignComponentIsIgnoredNotErrored covers another bot's button on a
// message this service can see. A 500 here would make Discord retry the
// delivery, so the answer has to be a polite 200.
func TestForeignComponentIsIgnoredNotErrored(t *testing.T) {
	srv, priv, _ := testServer(t)
	body := `{"type":3,"data":{"custom_id":"someotherbot:do-a-thing"},
	          "member":{"user":{"id":"alice","username":"alice"}}}`
	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", body))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a non-200 makes Discord retry", rec.Code)
	}
}

func TestParseCustomID(t *testing.T) {
	cases := []struct {
		in        string
		action    string
		eventID   int64
		shouldFit bool
	}{
		{"signup:join:42", "join", 42, true},
		{"signup:leave:7", "leave", 7, true},
		{"signup:join:notanumber", "", 0, false},
		{"otherbot:join:42", "", 0, false},
		{"signup:join", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range cases {
		action, eventID, ok := parseCustomID(tc.in)
		if ok != tc.shouldFit {
			t.Errorf("parseCustomID(%q) ok = %v, want %v", tc.in, ok, tc.shouldFit)
			continue
		}
		if ok && (action != tc.action || eventID != tc.eventID) {
			t.Errorf("parseCustomID(%q) = %q,%d; want %q,%d",
				tc.in, action, eventID, tc.action, tc.eventID)
		}
	}
}

func TestVerifierRejectsMalformedPublicKeys(t *testing.T) {
	for _, key := range []string{"", "zzzz", "abcd"} {
		if _, err := NewInteractionVerifier(key); err == nil {
			t.Errorf("public key %q was accepted", key)
		}
	}
}

// permissionsWithManageEvents is what Discord sends for someone who may edit
// events: a decimal string, because the bit field is wider than a JSON number.
const permissionsWithManageEvents = "8589934592" // 1 << 33

// TestEditButtonOpensAPrefilledFormAndSaves walks the whole round trip through
// the signed HTTP handler: click, form, submit, changed event.
func TestEditButtonOpensAPrefilledFormAndSaves(t *testing.T) {
	srv, priv, store := testServer(t)
	srv.SetDefaultTimezone("America/Los_Angeles")

	start := time.Date(2026, 9, 5, 19, 0, 0, 0, time.UTC).Unix()
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "c1", Name: "Playtest", Capacity: 1,
		StartsAt: start, EndsAt: start + 7200, Location: "The shed",
		Description: "Bring dice.", Timezone: "America/Los_Angeles",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, u := range []string{"alice", "bob", "carol"} {
		if _, err := store.Join(ev.ID, u, u, JoinedViaButton); err != nil {
			t.Fatalf("join %s: %v", u, err)
		}
	}

	send := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	reply := send(fmt.Sprintf(`{
		"type": 3, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q},
		"member": {"permissions": %q, "user": {"id": "boss", "username": "boss"}}
	}`, EditCustomID(ev.ID), permissionsWithManageEvents))

	if int(reply["type"].(float64)) != callbackTypeModal {
		t.Fatalf("type = %v, want %d (a modal)", reply["type"], callbackTypeModal)
	}
	data := reply["data"].(map[string]any)
	if len([]rune(data["title"].(string))) > 45 {
		t.Error("modal title is over Discord's 45-character limit")
	}
	rows := data["components"].([]any)
	if len(rows) != 5 {
		t.Fatalf("modal has %d fields, want 5 (Discord's maximum)", len(rows))
	}
	for _, r := range rows {
		f := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		if f["custom_id"].(string) == "ends" {
			t.Error("the form still carries an end time field")
		}
	}
	prefilled := map[string]string{}
	for _, r := range rows {
		f := r.(map[string]any)["components"].([]any)[0].(map[string]any)
		prefilled[f["custom_id"].(string)] = f["value"].(string)
	}
	// Every field opens with what is already there, so a small change is a
	// small edit rather than retyping the event.
	for field, want := range map[string]string{
		fieldName: "Playtest", fieldCapacity: "1", fieldLocation: "The shed",
		fieldDescription: "Bring dice.",
		fieldStartsAt:    "2026-09-05 12:00", // 19:00 UTC in Los Angeles
	} {
		if prefilled[field] != want {
			t.Errorf("%s prefilled with %q, want %q", field, prefilled[field], want)
		}
	}

	reply = send(fmt.Sprintf(`{
		"type": 5, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q, "components": [
			{"components": [{"custom_id": "name", "value": "Playtest night"}]},
			{"components": [{"custom_id": "starts", "value": "2026-09-06 18:30"}]},
			{"components": [{"custom_id": "capacity", "value": "3"}]},
			{"components": [{"custom_id": "location", "value": "The big shed"}]},
			{"components": [{"custom_id": "description", "value": "Bring dice and snacks."}]}
		]},
		"member": {"permissions": %q, "user": {"id": "boss", "username": "boss"}}
	}`, EditModalCustomID(ev.ID), permissionsWithManageEvents))

	content := reply["data"].(map[string]any)["content"].(string)
	for _, want := range []string{"Playtest night", "3", "waitlist"} {
		if !strings.Contains(content, want) {
			t.Errorf("reply = %q, want it to mention %q", content, want)
		}
	}

	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Name != "Playtest night" || got.Capacity != 3 || got.Location != "The big shed" {
		t.Errorf("event = %q / cap %d / %q, want the edited values",
			got.Name, got.Capacity, got.Location)
	}
	if got.Description != "Bring dice and snacks." {
		t.Errorf("description = %q, want the edited value", got.Description)
	}
	// The form has no end time field, so editing from Discord must leave the
	// one set on the web page exactly where it was. Sending zero for a field
	// the form never collected is how that gets silently wiped.
	if got.EndsAt != start+7200 {
		t.Errorf("ends_at = %d, want it untouched at %d — a field the Discord form does "+
			"not collect must not be cleared by it", got.EndsAt, start+7200)
	}
	if got.AttendingCount != 3 || got.WaitlistCount != 0 {
		t.Errorf("%d attending / %d waiting, want 3 / 0 — raising the limit should have "+
			"admitted the queue", got.AttendingCount, got.WaitlistCount)
	}
}

// TestSomeoneWithoutManageEventsCannotEdit covers the button being on a public
// message. Discord cannot hide a component from some readers, so the check has
// to be on the press — and on the submit, which is a separate request.
func TestSomeoneWithoutManageEventsCannotEdit(t *testing.T) {
	srv, priv, store := testServer(t)
	srv.SetDefaultTimezone("UTC")
	ev, err := store.CreateEvent(Event{
		GuildID: "g1", ChannelID: "c1", Name: "Playtest", Capacity: 2,
		StartsAt: time.Date(2026, 9, 5, 19, 0, 0, 0, time.UTC).Unix(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	press := func(permissions string) string {
		rec := httptest.NewRecorder()
		srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
			"type": 3, "guild_id": "g1", "channel_id": "c1",
			"data": {"custom_id": %q},
			"member": {"permissions": %q, "user": {"id": "rando", "username": "rando"}}
		}`, EditCustomID(ev.ID), permissions)))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if int(out["type"].(float64)) == callbackTypeModal {
			return "MODAL OPENED"
		}
		return out["data"].(map[string]any)["content"].(string)
	}
	if got := press("0"); got == "MODAL OPENED" {
		t.Error("someone with no permissions was offered the edit form")
	}
	if got := press("2048"); got == "MODAL OPENED" { // SEND_MESSAGES only
		t.Error("an ordinary member was offered the edit form")
	}

	// Forging the submit without ever opening the form must not work either.
	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 5, "guild_id": "g1", "channel_id": "c1",
		"data": {"custom_id": %q, "components": [
			{"components": [{"custom_id": "name", "value": "Hijacked"}]},
			{"components": [{"custom_id": "starts", "value": "2026-09-05 19:00"}]},
			{"components": [{"custom_id": "capacity", "value": "9999"}]}
		]},
		"member": {"permissions": "2048", "user": {"id": "rando", "username": "rando"}}
	}`, EditModalCustomID(ev.ID))))

	got, err := store.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Name != "Playtest" || got.Capacity != 2 {
		t.Errorf("a forged modal submit changed the event to %q / cap %d", got.Name, got.Capacity)
	}
}

// TestCreateButtonMakesAnEventAndPostsItsCard covers the how-to channel button.
func TestCreateButtonMakesAnEventAndPostsItsCard(t *testing.T) {
	fake := newFakeDiscord(t)
	store := testStore(t)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	verifier, err := NewInteractionVerifier(hex.EncodeToString(pub))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	srv := NewServer(store, verifier, fake.client())
	srv.EnableWeb(nil, "board-channel")
	srv.SetDefaultTimezone("America/Los_Angeles")

	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 5, "guild_id": "g1", "channel_id": "howto-channel",
		"data": {"custom_id": %q, "components": [
			{"components": [{"custom_id": "name", "value": "Board game night"}]},
			{"components": [{"custom_id": "starts", "value": "2026-09-05 19:00"}]},
			{"components": [{"custom_id": "capacity", "value": "8"}]},
			{"components": [{"custom_id": "location", "value": "The pub"}]},
			{"components": [{"custom_id": "description", "value": "Catan and beer."}]}
		]},
		"member": {"permissions": %q, "user": {"id": "boss", "username": "boss"}}
	}`, CreateModalCustomID(), permissionsWithManageEvents)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	events, err := store.ListEvents("g1", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("created %d events, want 1", len(events))
	}
	created := events[0]
	if created.Name != "Board game night" || created.Capacity != 8 || created.Location != "The pub" {
		t.Errorf("event = %q / cap %d / %q", created.Name, created.Capacity, created.Location)
	}
	if created.Description != "Catan and beer." {
		t.Errorf("description = %q", created.Description)
	}
	// The creator is recorded by id, which is what lets them edit it later
	// without server-wide Manage Events.
	if created.CreatedBy != "boss" {
		t.Errorf("created_by = %q, want \"boss\"", created.CreatedBy)
	}
	// The card goes to the board, not to the channel the button was pressed in.
	if created.MessageID == "" {
		t.Error("no signup card was posted")
	}
	var boardPosts int
	for _, c := range fake.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/board-channel/messages" {
			boardPosts++
		}
	}
	if boardPosts != 1 {
		t.Errorf("%d posts to the board, want exactly the card", boardPosts)
	}
}

// TestCreatingNeedsPermission stops the how-to button being an open door.
func TestCreatingNeedsPermission(t *testing.T) {
	srv, priv, store := testServer(t)
	srv.SetDefaultTimezone("UTC")

	rec := httptest.NewRecorder()
	srv.HandleInteraction(rec, signedRequest(t, priv, "1700000000", fmt.Sprintf(`{
		"type": 5, "guild_id": "g1", "channel_id": "howto",
		"data": {"custom_id": %q, "components": [
			{"components": [{"custom_id": "name", "value": "Sneaky"}]},
			{"components": [{"custom_id": "starts", "value": "2026-09-05 19:00"}]},
			{"components": [{"custom_id": "capacity", "value": "5"}]}
		]},
		"member": {"permissions": "2048", "user": {"id": "rando", "username": "rando"}}
	}`, CreateModalCustomID())))

	events, err := store.ListEvents("g1", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("an ordinary member created %d events", len(events))
	}
}
