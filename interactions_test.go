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
