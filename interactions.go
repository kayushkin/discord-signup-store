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
		// Values is populated when a select menu is used: the chosen option's
		// value. The table's menus put the event id here rather than in the
		// custom_id, because one menu covers every event.
		Values []string `json:"values"`
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

	// Table actions are handled before anything looks the event up by message.
	// Their message is the table, which belongs to no single event, and their
	// chosen event arrives in the select's value instead.
	if strings.HasPrefix(action, "table-") {
		s.handleTableAction(w, in, action)
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
	case "edit", "capacity":
		// "capacity" is the old id. Cards posted before the button was widened
		// are still sitting in channels, and their button must keep working
		// rather than answering "not one of mine" forever.
		s.handleEditButton(w, in, eventID)
	case "create":
		s.handleCreateButton(w, in)
	case "details":
		s.handleDetailsButton(w, in, eventID)
	case "my-events":
		// The routing bug this fixes: "my-events" matched no case and no
		// "table-" prefix, so the button shipped answering "Unknown signup
		// action." Routed explicitly now, and pinned by a test that presses it
		// through the signed handler.
		s.handleMyEventsButton(w, in)
	case "dash-join", "dash-leave":
		s.handleDashboardAction(w, in, action, eventID)
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
	go s.syncAfterChange(ev.ID, []stateChange{{UserID: userID, State: result.Signup.State}})
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
	// Synced by id, so a failed reload above no longer costs the whole sync —
	// the roster changed whether or not this process could read it back.
	go s.syncAfterChange(eventID, changes)
	if ev != nil && result.Promoted != nil {
		go s.notifyPromoted(ev, result.Promoted)
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

// EditCustomID builds the custom_id for an event's edit button.
func EditCustomID(eventID int64) string {
	return fmt.Sprintf("%s:edit:%d", customIDPrefix, eventID)
}

// EditModalCustomID builds the custom_id for the form that button opens.
func EditModalCustomID(eventID int64) string {
	return fmt.Sprintf("%s:edit-modal:%d", customIDPrefix, eventID)
}

// DetailsModalCustomID builds the custom_id for the details modal. Its submit
// changes nothing — Discord has no read-only text input, so the only way to
// show text in an overlay is to prefill editable boxes and refuse the save.
func DetailsModalCustomID(eventID int64) string {
	return fmt.Sprintf("%s:details-modal:%d", customIDPrefix, eventID)
}

// DetailsCustomID builds the custom_id for a table row's Details button.
func DetailsCustomID(eventID int64) string {
	return fmt.Sprintf("%s:details:%d", customIDPrefix, eventID)
}

// CreateCustomID is the button on the how-to message. It carries event id 0,
// because there is no event yet — the id slot is kept so one parser handles
// every custom_id this service issues.
func CreateCustomID() string {
	return fmt.Sprintf("%s:create:0", customIDPrefix)
}

// CreateModalCustomID is the form that button opens.
func CreateModalCustomID() string {
	return fmt.Sprintf("%s:create-modal:0", customIDPrefix)
}

// permissionCreateEvents is Discord's own CREATE_EVENTS bit. Someone who may
// create a native event may create one here; the two are the same act.
const permissionCreateEvents = 1 << 44

func (i *Interaction) canCreateEvents() bool {
	bits, err := strconv.ParseUint(i.Member.Permissions, 10, 64)
	if err != nil {
		return false
	}
	return bits&permissionAdministrator != 0 ||
		bits&permissionManageEvents != 0 ||
		bits&permissionCreateEvents != 0
}

// mayEdit reports whether the person clicking may change this event, and
// explains why not when they may not.
func (s *Server) mayEdit(in *Interaction, ev *Event) (bool, string) {
	userID, _ := in.actor()
	if in.canManageEvents() || (ev.CreatedBy != "" && ev.CreatedBy == userID) {
		return true, ""
	}
	return false, "Only someone with Manage Events, or whoever created this event, can edit it."
}

// handleEditButton opens the edit form.
//
// The button is on a message everyone can see, because Discord cannot show a
// component to some readers and not others. So the check happens on the press
// and an unauthorised click costs one private no, rather than a form that fails
// after it has been filled in.
func (s *Server) handleEditButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That signup list no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	zone := ev.Timezone
	if zone == "" {
		zone = s.DefaultTimezone()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": buildEventModal(EditModalCustomID(eventID), "Edit "+ev.Name, ev, zone),
	})
}

// handleCreateButton opens the same form, empty.
func (s *Server) handleCreateButton(w http.ResponseWriter, in *Interaction) {
	if !in.canCreateEvents() {
		s.replyEphemeral(w, "You need Create Events or Manage Events in this server "+
			"to make an event.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeModal,
		"data": buildEventModal(CreateModalCustomID(), "New event", nil, s.DefaultTimezone()),
	})
}

// handleModalSubmit takes the typed values from either form.
func (s *Server) handleModalSubmit(w http.ResponseWriter, in *Interaction) {
	action, eventID, ok := parseCustomID(in.Data.CustomID)
	if !ok {
		s.replyEphemeral(w, "That form is not one of mine.")
		return
	}
	form := EventForm{
		Name:        in.fieldValue(fieldName),
		StartsAt:    in.fieldValue(fieldStartsAt),
		Capacity:    in.fieldValue(fieldCapacity),
		Location:    in.fieldValue(fieldLocation),
		Description: in.fieldValue(fieldDescription),
	}
	switch action {
	case "edit-modal", "capacity-modal":
		s.applyEditForm(w, in, eventID, form)
	case "create-modal":
		s.applyCreateForm(w, in, form)
	case "details-modal":
		// A viewer's details modal holds no inputs, so there is nothing to
		// save. It still has a submit button — every modal does — and Discord
		// requires an answer to it. Somebody who may edit gets the edit form in
		// the same modal, which submits as "edit-modal" and never lands here.
		s.replyEphemeral(w, "Nothing to save — that was just the roster.")
	default:
		s.replyEphemeral(w, "That form is not one of mine.")
	}
}

// applyEditForm saves an edit and reports what followed from it.
func (s *Server) applyEditForm(w http.ResponseWriter, in *Interaction, eventID int64, form EventForm) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That signup list no longer exists.")
		return
	}
	// Checked again here, not only on the press that opened the form. The two
	// are separate requests and nothing stops the second arriving on its own.
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	zone := ev.Timezone
	if zone == "" {
		zone = s.DefaultTimezone()
	}
	values, err := form.Validate(zone)
	if err != nil {
		s.replyEphemeral(w, plainError(err))
		return
	}

	userID, _ := in.actor()
	updated, promoted, err := s.ApplyEventForm(ev, values, zone, "discord:"+userID)
	if err != nil {
		log.Printf("[discord-signup] edit event=%d: %v", eventID, err)
		s.replyEphemeral(w, "Something went wrong. Nothing was changed.")
		return
	}
	s.replyEphemeral(w, describeEdit(ev, updated, promoted, zone))
}

// applyCreateForm makes a new event and puts its card on the board.
func (s *Server) applyCreateForm(w http.ResponseWriter, in *Interaction, form EventForm) {
	if !in.canCreateEvents() {
		s.replyEphemeral(w, "You need Create Events or Manage Events in this server "+
			"to make an event.")
		return
	}
	zone := s.DefaultTimezone()
	values, err := form.Validate(zone)
	if err != nil {
		s.replyEphemeral(w, plainError(err))
		return
	}
	userID, displayName := in.actor()
	ev, err := s.createEventAndJoinOrganiser(Event{
		GuildID:     in.GuildID,
		ChannelID:   s.BoardChannelID(),
		Name:        values.Name,
		Description: values.Description,
		Capacity:    values.Capacity,
		StartsAt:    values.StartsAt,
		Location:    values.Location,
		Timezone:    zone,
		Origin:      OriginLocal,
		CreatedBy:   userID,
	}, displayName)
	if err != nil {
		log.Printf("[discord-signup] create from discord: %v", err)
		s.replyEphemeral(w, plainError(err))
		return
	}
	if _, err := s.PostSignupMessage(ev.ID); err != nil {
		// The event exists; only the card failed. Say so rather than implying
		// nothing happened, or they will create it a second time.
		log.Printf("[discord-signup] post card for new event %d: %v", ev.ID, err)
		s.replyEphemeral(w, fmt.Sprintf("**%s** was created, but its signup card could not "+
			"be posted to <#%s>. Nothing is lost — the next sync will try again.",
			ev.Name, s.BoardChannelID()))
		return
	}
	// Also published as a native Discord event, so it appears in the server's
	// own event list and fires Discord's start notification. Best effort: the
	// roster and its card already exist and are the real thing, so a failure
	// here is reported rather than allowed to undo them.
	go s.refreshEventTableQuietly(ev.GuildID)

	published := true
	if _, err := s.PublishToDiscord(ev.ID, s.BoardChannelID()); err != nil {
		log.Printf("[discord-signup] publish event %d to discord: %v", ev.ID, err)
		published = false
	}
	reply := fmt.Sprintf("**%s** is up in <#%s>.\n%s",
		ev.Name, s.BoardChannelID(), describeEventLine(ev, values.Capacity))
	if published {
		reply += "\n\nIt is also in the server's own event list. Pressing **Interested** " +
			"there signs people up here too."
	} else {
		reply += "\n\n⚠️ It could not be added to the server's event list — the signup " +
			"card is unaffected. Use Publish on the web page to try again."
	}
	reply += "\n\nAnyone can press Join. Press Edit on the card to change any of this."
	s.replyEphemeral(w, reply)
}

func describeEventLine(ev *Event, capacity int) string {
	line := fmt.Sprintf("🗓️ <t:%d:F>", ev.StartsAt)
	if ev.Location != "" {
		line += " · " + ev.Location
	}
	if capacity > 0 {
		line += fmt.Sprintf(" · %d places", capacity)
	} else {
		line += " · no limit"
	}
	return line
}

// describeEdit says what changed, field by field, and what followed from it.
//
// A bare "Saved." would hide the two consequences people are most surprised by:
// raising a limit admits a queue in one go, and lowering it below the roster
// removes nobody.
func describeEdit(before, after *Event, promoted []Signup, zone string) string {
	var changes []string
	if before.Name != after.Name {
		changes = append(changes, fmt.Sprintf("name → **%s**", after.Name))
	}
	if before.StartsAt != after.StartsAt {
		changes = append(changes, fmt.Sprintf("starts → <t:%d:F>", after.StartsAt))
	}
	if before.Description != after.Description {
		changes = append(changes, "description updated")
	}
	if before.Capacity != after.Capacity {
		if after.Capacity == 0 {
			changes = append(changes, "limit removed")
		} else {
			changes = append(changes, fmt.Sprintf("limit → **%d** places", after.Capacity))
		}
	}
	if before.Location != after.Location {
		if after.Location == "" {
			changes = append(changes, "location removed")
		} else {
			changes = append(changes, "location → "+after.Location)
		}
	}

	var b strings.Builder
	if len(changes) == 0 {
		b.WriteString("Nothing changed.")
	} else {
		b.WriteString("Saved: " + strings.Join(changes, ", ") + ".")
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
		fmt.Fprintf(&b, "\n\n%d people are already in, over the new limit. Nobody was removed — "+
			"they were told they had a place. The limit applies to the next person to sign up.",
			after.AttendingCount)
	}
	b.WriteString("\n\nDescription, recurrence and roles are on the web page — a Discord form " +
		"holds five fields and no more.")
	return b.String()
}

// plainError strips the internal error wrapper so the person reads the reason
// rather than the plumbing.
func plainError(err error) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "invalid event: ", "")
	return strings.ToUpper(msg[:1]) + msg[1:]
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

// truncate trims to a rune-safe length. Discord counts characters, not bytes,
// and cutting mid-rune would send invalid UTF-8 and be rejected outright.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
