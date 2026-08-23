package discordsignup

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Discord interaction types. Only PING and MESSAGE_COMPONENT are handled here;
// this service has no slash commands, because a button on a standing message is
// the surface people actually use to sign up.
const (
	interactionTypePing             = 1
	interactionTypeMessageComponent = 3
	interactionTypeModalSubmit      = 5
)

// Discord interaction callback types.
const (
	callbackTypePong                  = 1
	callbackTypeChannelMessageWithSrc = 4
	// callbackTypeModal opens a form. It is the ONLY way Discord offers a free
	// text field: a message component cannot contain one, so anything a person
	// has to type has to arrive through a modal.
	callbackTypeModal = 9
)

// messageFlagEphemeral makes a reply visible only to the person who clicked.
//
// This is the reason the button belongs to us rather than to Discord's own
// Interested list. A click carries an interaction token, so the answer —
// "you're in, 18/20" or "waitlisted, 3rd" — is delivered instantly, privately,
// and with no way to fail. Telling the same person by DM instead can bounce
// with error 50007 when they have DMs from server members turned off, and a
// person who never learns they were waitlisted turns up expecting a place.
const messageFlagEphemeral = 1 << 6

// customIDPrefix namespaces this service's buttons so a component from some
// other bot on the same message can never be mistaken for one of ours.
const customIDPrefix = "signup"

// Interaction is the subset of Discord's interaction payload this service
// reads. Deliberately partial: the full object is large, changes often, and
// every field decoded here is one more thing that can break on a schema change
// Discord did not announce.
type Interaction struct {
	Type      int    `json:"type"`
	ID        string `json:"id"`
	Token     string `json:"token"`
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`

	Data struct {
		CustomID string `json:"custom_id"`
		// Components is populated on a modal submit: one action row per field,
		// each holding one text input carrying the typed value.
		Components []struct {
			Components []struct {
				CustomID string `json:"custom_id"`
				Value    string `json:"value"`
			} `json:"components"`
		} `json:"components"`
	} `json:"data"`

	Message struct {
		ID string `json:"id"`
	} `json:"message"`

	// Member is present for an interaction in a guild; User for one in a DM.
	// Both are decoded because reading only Member yields an empty user id in
	// a DM, and an empty id would be written to the roster as a real one.
	Member struct {
		Nick string `json:"nick"`
		// Permissions is what this user may do IN THIS CHANNEL, already
		// computed by Discord including role inheritance and per-channel
		// overwrites. Sent as a decimal string because the bit field exceeds
		// what a JSON number holds exactly.
		Permissions string `json:"permissions"`
		User        struct {
			ID          string `json:"id"`
			Username    string `json:"username"`
			GlobalName  string `json:"global_name"`
			Discriminat string `json:"discriminator"`
		} `json:"user"`
	} `json:"member"`
	User struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	} `json:"user"`
}

// canManageEvents reports whether the person who clicked may change an event.
//
// Read from the interaction rather than fetched: Discord has already computed
// the answer for this channel, so trusting it costs no API call and cannot go
// stale between the click and the check.
func (i *Interaction) canManageEvents() bool {
	bits, err := strconv.ParseUint(i.Member.Permissions, 10, 64)
	if err != nil {
		// An unreadable permission field means no permissions, never all of
		// them. Failing open here would hand the capacity control to everyone.
		return false
	}
	return bits&permissionAdministrator != 0 || bits&permissionManageEvents != 0
}

// actor returns the Discord user id behind an interaction, and the best
// display name available. The id is the only thing stored as a key.
func (i *Interaction) actor() (userID, displayName string) {
	if i.Member.User.ID != "" {
		name := i.Member.Nick
		if name == "" {
			name = i.Member.User.GlobalName
		}
		if name == "" {
			name = i.Member.User.Username
		}
		return i.Member.User.ID, name
	}
	name := i.User.GlobalName
	if name == "" {
		name = i.User.Username
	}
	return i.User.ID, name
}

// InteractionVerifier checks that a request really came from Discord.
type InteractionVerifier struct {
	publicKey ed25519.PublicKey
}

// NewInteractionVerifier parses the application's public key, as shown in the
// Developer Portal, from hex.
func NewInteractionVerifier(publicKeyHex string) (*InteractionVerifier, error) {
	publicKeyHex = strings.TrimSpace(publicKeyHex)
	if publicKeyHex == "" {
		return nil, errors.New("discord application public key is empty")
	}
	raw, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode public key hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return &InteractionVerifier{publicKey: ed25519.PublicKey(raw)}, nil
}

// Verify reports whether the signature headers match the body.
//
// The signed message is the timestamp header concatenated with the raw request
// body — raw, byte for byte. Re-encoding the parsed JSON and verifying that
// instead fails, because Go's marshaller will not reproduce Discord's key order
// or spacing.
func (v *InteractionVerifier) Verify(signatureHex, timestamp string, body []byte) bool {
	sig, err := hex.DecodeString(signatureHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := make([]byte, 0, len(timestamp)+len(body))
	msg = append(msg, timestamp...)
	msg = append(msg, body...)
	return ed25519.Verify(v.publicKey, msg, sig)
}

// HandleInteraction is the endpoint registered as the application's
// "Interactions Endpoint URL" in the Developer Portal.
//
// Discord validates this URL when you save it and keeps probing afterward,
// deliberately sending requests with bad signatures. An endpoint that answers
// anything but 401 to those is rejected and the URL will not save — so the
// verification below is not only a security control, it is what makes the
// endpoint installable at all.
func (s *Server) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	if sig == "" || ts == "" || !s.verifier.Verify(sig, ts, body) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var in Interaction
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "malformed interaction", http.StatusBadRequest)
		return
	}

	switch in.Type {
	case interactionTypePing:
		writeJSON(w, http.StatusOK, map[string]any{"type": callbackTypePong})
	case interactionTypeMessageComponent:
		s.handleComponent(w, &in)
	case interactionTypeModalSubmit:
		s.handleModalSubmit(w, &in)
	default:
		// Not ours to answer, but a non-200 here makes Discord retry. Reply
		// with a harmless ephemeral note instead of failing the delivery.
		s.replyEphemeral(w, "This app only handles signup buttons.")
	}
}

// handleComponent routes a button press.
func (s *Server) handleComponent(w http.ResponseWriter, in *Interaction) {
	action, eventID, ok := parseCustomID(in.Data.CustomID)
	if !ok {
		// Someone else's component on a message we happen to see. Say nothing
		// meaningful, but do not error — a 500 here would make Discord retry.
		s.replyEphemeral(w, "That button is not one of mine.")
		return
	}

	userID, displayName := in.actor()
	if userID == "" {
		s.replyEphemeral(w, "Could not read your Discord user id from that click. Nothing was changed.")
		return
	}

	// The event id travels in the custom_id, but the message the button sits on
	// is the stronger claim: a custom_id can be copied onto another message. If
	// the message is one we know, it wins.
	if ev, err := s.store.EventByMessage(in.Message.ID); err == nil {
		eventID = ev.ID
	}

	switch action {
	case "join":
		s.handleJoin(w, in, eventID, userID, displayName)
	case "leave":
		s.handleLeave(w, in, eventID, userID)
	case "capacity":
		s.handleCapacityButton(w, in, eventID)
	default:
		s.replyEphemeral(w, "Unknown signup action.")
	}
}

func (s *Server) handleJoin(w http.ResponseWriter, in *Interaction, eventID int64, userID, displayName string) {
	result, err := s.store.Join(eventID, userID, displayName, JoinedViaButton)
	if errors.Is(err, ErrNotFound) {
		s.replyEphemeral(w, "That signup list no longer exists.")
		return
	}
	if errors.Is(err, ErrEventNotOpen) {
		s.replyEphemeral(w, "Signups for this event are closed.")
		return
	}
	if err != nil {
		// Fail loud in the log, and tell the person plainly rather than
		// leaving the click looking like it worked.
		log.Printf("[discord-signup] join event=%d user=%s: %v", eventID, userID, err)
		s.replyEphemeral(w, "Something went wrong signing you up. Nothing was changed — try again.")
		return
	}

	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		log.Printf("[discord-signup] reload event=%d: %v", eventID, err)
		s.replyEphemeral(w, describeJoin(result, nil))
		return
	}
	s.replyEphemeral(w, describeJoin(result, ev))

	// Roles and the public roster message are both projections of what was just
	// written. They happen after the reply because the person clicking must not
	// wait on Discord's API for their answer, and because a failure to sync a
	// role must not make a successful signup look failed.
	go s.syncAfterChange(ev, []stateChange{{UserID: userID, State: result.Signup.State}})
}

func (s *Server) handleLeave(w http.ResponseWriter, in *Interaction, eventID int64, userID string) {
	result, err := s.store.Leave(eventID, userID, ActorUser)
	if errors.Is(err, ErrNotFound) {
		s.replyEphemeral(w, "You were not signed up for this one.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] leave event=%d user=%s: %v", eventID, userID, err)
		s.replyEphemeral(w, "Something went wrong. Nothing was changed — try again.")
		return
	}

	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		log.Printf("[discord-signup] reload event=%d: %v", eventID, err)
	}
	s.replyEphemeral(w, "You are off the list. Your place has gone to the next person waiting.")

	changes := []stateChange{{UserID: userID, State: StateWithdrawn}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
	}
	if ev != nil {
		go s.syncAfterChange(ev, changes)
		if result.Promoted != nil {
			go s.notifyPromoted(ev, result.Promoted)
		}
	}
}

// describeJoin renders the private answer a person gets for pressing Join.
func describeJoin(r *JoinResult, ev *Event) string {
	var b strings.Builder
	switch {
	case r.AlreadySignedUp && r.Signup.State == StateAttending:
		b.WriteString("You are already signed up — no change.")
	case r.AlreadySignedUp && r.Signup.State == StateWaitlisted:
		fmt.Fprintf(&b, "You are already on the waitlist, at number %d.", r.Signup.WaitlistPlace)
	case r.Signup.State == StateAttending:
		b.WriteString("You're in.")
	case r.Signup.State == StateWaitlisted:
		fmt.Fprintf(&b, "This one is full, so you are on the waitlist at number %d. "+
			"If someone drops out you move up automatically and I will send you a message.",
			r.Signup.WaitlistPlace)
	}
	if ev != nil && ev.Capacity > 0 {
		fmt.Fprintf(&b, "\n\n**%s** — %d/%d places taken", ev.Name, ev.AttendingCount, ev.Capacity)
		if ev.WaitlistCount > 0 {
			fmt.Fprintf(&b, ", %d waiting", ev.WaitlistCount)
		}
		b.WriteString(".")
	}
	return b.String()
}

// parseCustomID splits "signup:join:42" into its action and event id.
func parseCustomID(customID string) (action string, eventID int64, ok bool) {
	parts := strings.Split(customID, ":")
	if len(parts) != 3 || parts[0] != customIDPrefix {
		return "", 0, false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return parts[1], id, true
}

// JoinCustomID builds the custom_id for an event's Join button.
func JoinCustomID(eventID int64) string {
	return fmt.Sprintf("%s:join:%d", customIDPrefix, eventID)
}

// LeaveCustomID builds the custom_id for an event's Leave button.
func LeaveCustomID(eventID int64) string {
	return fmt.Sprintf("%s:leave:%d", customIDPrefix, eventID)
}

func (s *Server) replyEphemeral(w http.ResponseWriter, content string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeChannelMessageWithSrc,
		"data": map[string]any{
			"content": content,
			"flags":   messageFlagEphemeral,
		},
	})
}

// capacityFieldID names the text input inside the capacity modal.
const capacityFieldID = "capacity"

// handleCapacityButton answers a click on the limit button with a modal.
//
// The button is on a message everyone can see, because Discord has no way to
// show a component to some readers and not others. So the permission check
// happens here, on the click, and someone without it gets a private no rather
// than a form that fails on submit.
func (s *Server) handleCapacityButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That signup list no longer exists.")
		return
	}
	userID, _ := in.actor()
	if !in.canManageEvents() && ev.CreatedBy != userID {
		s.replyEphemeral(w, "Only someone with Manage Events, or whoever created this event, "+
			"can change the limit.")
		return
	}

	current := strconv.Itoa(ev.Capacity)
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": map[string]any{
			"custom_id": CapacityModalCustomID(eventID),
			// Discord caps a modal title at 45 characters and rejects the whole
			// interaction if it is longer, so the event name is trimmed to fit
			// rather than sent whole and refused.
			"title": truncate("Limit for "+ev.Name, 45),
			"components": []any{
				map[string]any{
					"type": componentTypeActionRow,
					"components": []any{
						map[string]any{
							"type":        componentTypeTextInput,
							"custom_id":   capacityFieldID,
							"label":       "How many places? 0 means no limit",
							"style":       textInputStyleShort,
							"value":       current,
							"min_length":  1,
							"max_length":  6,
							"required":    true,
							"placeholder": "20",
						},
					},
				},
			},
		},
	})
}

// handleModalSubmit takes the typed value.
func (s *Server) handleModalSubmit(w http.ResponseWriter, in *Interaction) {
	action, eventID, ok := parseCustomID(in.Data.CustomID)
	if !ok || action != "capacity-modal" {
		s.replyEphemeral(w, "That form is not one of mine.")
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That signup list no longer exists.")
		return
	}
	userID, _ := in.actor()
	// Checked again on submit, not only on the click that opened the form. The
	// two are separate requests and nothing stops the second being sent on its
	// own.
	if !in.canManageEvents() && ev.CreatedBy != userID {
		s.replyEphemeral(w, "Only someone with Manage Events, or whoever created this event, "+
			"can change the limit.")
		return
	}

	capacity, err := ParseCapacity(in.fieldValue(capacityFieldID))
	if err != nil {
		// The message names what was wrong with what they typed, because the
		// modal is gone by now and they have to retype it from memory.
		s.replyEphemeral(w, strings.TrimPrefix(err.Error(), "invalid event: "))
		return
	}

	updated, promoted, err := s.SetCapacity(eventID, capacity, "discord:"+userID)
	if err != nil {
		log.Printf("[discord-signup] set capacity event=%d: %v", eventID, err)
		s.replyEphemeral(w, "Something went wrong. The limit was not changed.")
		return
	}
	s.replyEphemeral(w, describeCapacityChange(ev, updated, promoted))
}

// fieldValue pulls one typed field out of a modal submission.
func (i *Interaction) fieldValue(customID string) string {
	for _, row := range i.Data.Components {
		for _, field := range row.Components {
			if field.CustomID == customID {
				return field.Value
			}
		}
	}
	return ""
}

// describeCapacityChange says what the new number did, not just what it is.
//
// The consequence is the part worth reporting: raising a limit can admit a
// queue of people in one go, and lowering it below the current roster does not
// remove anyone — both are surprising if you only see the number.
func describeCapacityChange(before, after *Event, promoted []Signup) string {
	var b strings.Builder
	switch {
	case after.Capacity == 0:
		b.WriteString("Limit removed — **" + after.Name + "** now takes anyone.")
	case before.Capacity == after.Capacity:
		fmt.Fprintf(&b, "Limit unchanged at **%d**.", after.Capacity)
	default:
		fmt.Fprintf(&b, "Limit set to **%d** places.", after.Capacity)
	}

	switch len(promoted) {
	case 0:
	case 1:
		fmt.Fprintf(&b, "\n<@%s> came off the waitlist and has been messaged.",
			promoted[0].DiscordUserID)
	default:
		fmt.Fprintf(&b, "\n%d people came off the waitlist and have been messaged.", len(promoted))
	}

	if after.Capacity > 0 && after.AttendingCount > after.Capacity {
		fmt.Fprintf(&b, "\n\n%d people are already in, which is over the new limit. "+
			"Nobody has been removed — they were told they had a place. The limit applies "+
			"to the next person to sign up.", after.AttendingCount)
	} else if after.Capacity > 0 {
		fmt.Fprintf(&b, "\n\nNow %d/%d taken", after.AttendingCount, after.Capacity)
		if after.WaitlistCount > 0 {
			fmt.Fprintf(&b, ", %d still waiting", after.WaitlistCount)
		}
		b.WriteString(".")
	}
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// CapacityCustomID builds the custom_id for an event's limit button.
func CapacityCustomID(eventID int64) string {
	return fmt.Sprintf("%s:capacity:%d", customIDPrefix, eventID)
}

// CapacityModalCustomID builds the custom_id for the form that button opens.
func CapacityModalCustomID(eventID int64) string {
	return fmt.Sprintf("%s:capacity-modal:%d", customIDPrefix, eventID)
}
