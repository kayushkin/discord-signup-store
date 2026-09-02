package discordsignup

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
)

// Server is the HTTP surface: one public route for Discord's interaction
// callbacks, and an admin API that stays on loopback.
//
// The split matters and is enforced by the deployment, not by a flag here.
// cmd/ binds 127.0.0.1 and nginx proxies ONLY /interactions from the internet,
// so the roster-editing routes are unreachable from outside this host. Binding
// the wildcard would publish the admin API to anything that can route here.
type Server struct {
	store    *Store
	verifier *InteractionVerifier
	discord  *DiscordClient

	// oauth is nil until EnableWeb is called. Nil means the browser routes
	// answer 501 rather than half-working: a login page that cannot complete a
	// login is worse than one that says it is not set up.
	oauth *OAuthConfig
	// botID is the bot's own Discord user id, cached after the first SUCCESSFUL
	// lookup. Needed to read which roles the bot holds, which is what decides
	// what it can grant, and to recognise the bot's own reactions and events.
	//
	// Guarded by botIDMu rather than a sync.Once because a sync.Once runs its
	// body once whether or not the body succeeded: one transient /users/@me
	// failure would leave this empty for the life of the process with nothing
	// ever looking again. A failure is not an answer.
	botID   string
	botIDMu sync.Mutex
	// defaultTimezone is the IANA zone a time typed into a Discord form is read
	// in. One per deployment rather than per event, because a modal holds five
	// fields and a timezone picker is not worth one of them. Printed on the
	// form's own label so nobody has to guess which zone they are typing in.
	defaultTimezone string
	// pastChannelID is where a card goes once its event is over. Empty means
	// finished cards stay on the board, which is a legitimate way to run it —
	// the archive is a tidiness feature, not a correctness one.
	pastChannelID string
	// boardChannelID is the channel signup cards are posted to. Imported
	// Discord events land here rather than in their own channel, which for a
	// voice event is the room people talk in and where a card would be unread.
	boardChannelID string
	// syncs serialises Discord writes per event. Without it, two roster
	// changes seconds apart raced and the older one could land last, leaving
	// every public surface showing a count that was already wrong.
	syncs *eventSyncQueue
}

// EnableWeb turns on the browser surface at YOUR_DOMAIN.
//
// Separate from NewServer because the roster, the buttons and the interaction
// endpoint all work without it — the web page is management, not the product.
func (s *Server) EnableWeb(oauth *OAuthConfig, boardChannelID string) {
	s.oauth = oauth
	s.boardChannelID = boardChannelID
}

// SetPastChannelID names the channel finished cards move to.
func (s *Server) SetPastChannelID(channelID string) { s.pastChannelID = channelID }

// PastChannelID reports it.
func (s *Server) PastChannelID() string { return s.pastChannelID }

// SetDefaultTimezone names the zone Discord forms are read in.
func (s *Server) SetDefaultTimezone(zone string) { s.defaultTimezone = zone }

// DefaultTimezone reports it, falling back to UTC only when nothing is
// configured — which the service logs loudly at boot, because a time read in
// the wrong zone is wrong in a way nobody notices until the day itself.
func (s *Server) DefaultTimezone() string {
	if s.defaultTimezone == "" {
		return "UTC"
	}
	return s.defaultTimezone
}

// BoardChannelID reports where signup cards are posted.
func (s *Server) BoardChannelID() string { return s.boardChannelID }

// NewServer wires the pieces. discord may be nil, in which case the roster
// still works and nothing is pushed to Discord — useful in tests and for a
// first run before the bot token is filed in auth-store.
func NewServer(store *Store, verifier *InteractionVerifier, discord *DiscordClient) *Server {
	return &Server{store: store, verifier: verifier, discord: discord, syncs: newEventSyncQueue()}
}

// RegisterHandlers mounts every route on mux.
//
// Three surfaces, and which nginx vhost reaches which is the security boundary:
//
//	/interactions  — public on YOUR_EXISTING_DOMAIN. Ed25519-verified.
//	/api/*         — the machine API. NO auth of its own, so it must never be
//	                 proxied from any vhost. Loopback only, by deployment.
//	everything else— the browser surface on YOUR_DOMAIN. Every route
//	                 requires a Discord login and checks guild membership.
//
// The /api prefix exists precisely so the public vhost can proxy "/" without
// also publishing roster editing. Before it, GET /events was both the JSON API
// and a page path, and one nginx rule would have exposed the first.
func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("POST /interactions", s.HandleInteraction)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Machine API — loopback only.
	mux.HandleFunc("GET /api/events", s.handleListEvents)
	mux.HandleFunc("POST /api/events", s.handleCreateEvent)
	mux.HandleFunc("GET /api/events/{id}", s.handleGetEvent)
	mux.HandleFunc("PATCH /api/events/{id}", s.handleUpdateEvent)
	mux.HandleFunc("DELETE /api/events/{id}", s.handleDeleteEvent)
	mux.HandleFunc("POST /api/events/{id}/message", s.handlePostMessage)
	mux.HandleFunc("POST /api/events/{id}/publish", s.handlePublish)
	mux.HandleFunc("GET /api/events/{id}/signups", s.handleRoster)
	mux.HandleFunc("POST /api/events/{id}/signups", s.handleAdminJoin)
	mux.HandleFunc("DELETE /api/events/{id}/signups/{userID}", s.handleAdminLeave)
	mux.HandleFunc("GET /api/events/{id}/history", s.handleHistory)
	mux.HandleFunc("POST /api/guilds/{guildID}/sync", s.handleSyncGuild)
	mux.HandleFunc("POST /api/sync", s.handleSyncAllGuilds)
	mux.HandleFunc("POST /api/channels/{channelID}/how-to", s.handlePostHowTo)
	mux.HandleFunc("PUT /api/guilds/{guildID}/table", s.handleSetGuildTable)
	mux.HandleFunc("POST /api/guilds/{guildID}/table/refresh", s.handleRefreshGuildTable)
	mux.HandleFunc("PUT /api/guilds/{guildID}/forum", s.handleSetGuildForum)
	mux.HandleFunc("POST /api/events/complete-finished", s.handleCompleteFinished)
	mux.HandleFunc("POST /api/republish", s.handleRepublish)
	mux.HandleFunc("POST /api/reminders", s.handleSendReminders)
	mux.HandleFunc("POST /api/guilds/{guildID}/table/rebuild", s.handleRebuildGuildTable)
	mux.HandleFunc("POST /api/tables/rebuild", s.handleRebuildAllTables)

	// Browser surface — session-gated.
	mux.HandleFunc("GET /", s.handleWebIndex)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleOAuthCallback)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /events/new", s.handleWebNewEventForm)
	mux.HandleFunc("POST /events/new", s.handleWebCreateEvent)
	mux.HandleFunc("GET /events/{id}", s.handleWebEventDetail)
	mux.HandleFunc("POST /events/{id}", s.handleWebUpdateEvent)
	mux.HandleFunc("GET /events/{id}/edit", s.handleWebEditForm)
	mux.HandleFunc("POST /events/{id}/roster/remove", s.handleWebRosterRemove)
	mux.HandleFunc("POST /events/{id}/roster/add", s.handleWebRosterAdd)
	mux.HandleFunc("POST /events/{id}/post-message", s.handleWebPostMessage)
	mux.HandleFunc("POST /events/{id}/publish", s.handleWebPublish)
	mux.HandleFunc("POST /sync", s.handleWebSync)
}

// handleSetGuildTable points a guild's consolidated table at a channel and
// draws it. Calling it again with the same channel redraws in place; with a
// different one, the next refresh posts a fresh message there.
func (s *Server) handleSetGuildTable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON"})
		return
	}
	guildID := r.PathValue("guildID")
	if err := s.store.SetGuildTable(guildID, in.ChannelID); err != nil {
		writeStoreError(w, err)
		return
	}
	// Drawn from scratch, because pointing the table at a channel means there
	// is nothing in it yet.
	if err := s.RefreshEventTable(guildID); err != nil {
		writeStoreError(w, err)
		return
	}
	table, err := s.store.GuildTable(guildID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, table)
}

// handleSetGuildForum adopts a forum channel as the guild's forum surface:
// managed tags are added if missing, and every live event gets a post.
func (s *Server) handleSetGuildForum(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON"})
		return
	}
	forum, err := s.AdoptForum(r.PathValue("guildID"), in.ChannelID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, forum)
}

// handleRefreshGuildTable deletes every row and reposts them in date order.
// Named rebuild rather than refresh because that is what it does — individual
// rows refresh themselves on every change without anyone asking.
func (s *Server) handleRefreshGuildTable(w http.ResponseWriter, r *http.Request) {
	if err := s.RefreshEventTable(r.PathValue("guildID")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePostHowTo puts the standing how-to and its Create button in a channel.
func (s *Server) handlePostHowTo(w http.ResponseWriter, r *http.Request) {
	// adopt_message_id takes over a how-to that is already pinned — one posted
	// before this service recorded where it put things. Optional, and only
	// used once per channel: after that the id is stored.
	var in struct {
		AdoptMessageID string `json:"adopt_message_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	messageID, err := s.PublishHowToMessage(r.PathValue("channelID"), in.AdoptMessageID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message_id": messageID})
}

// handleSyncAllGuilds pulls native events from every server the bot is in and
// posts cards for the new ones. This is what the scheduler job calls: it names
// no guild, so adding the bot to another server needs no change here or there.
func (s *Server) handleSyncAllGuilds(w http.ResponseWriter, r *http.Request) {
	result, err := s.SyncAllGuilds()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleRepublish rewrites every Discord copy that disagrees with its roster.
//
// Separate from POST /api/sync on purpose, and not a second copy of it. That
// route starts by listing a guild's native scheduled events, and when that call
// is rate-limited it returns before reaching anything else — so repair hung off
// it was skipped exactly when the API was busiest, which is when writes are
// most likely to have been lost. This one touches Discord only for an event
// that is actually stale.
func (s *Server) handleRepublish(w http.ResponseWriter, r *http.Request) {
	if err := s.RepublishAllGuilds(); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRebuildGuildTable deletes the table's messages and posts them again.
//
// Discord caps how many times a message older than an hour may be edited
// (429 code 30046), and the table is edited on every signup. Once that cap is
// hit the table stops accepting edits and goes stale until the window rolls.
// Reposting on the hour means the message is never old enough for the cap to
// apply. It costs the table its place in the channel, which is the trade: it
// moves to the bottom, and it is correct.
func (s *Server) handleRebuildGuildTable(w http.ResponseWriter, r *http.Request) {
	if err := s.RebuildEventTable(r.PathValue("guildID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSendReminders posts whichever event reminders have come due.
//
// Called every minute. It is idempotent by the stamps on the event row, so
// calling it more often sends nothing extra and calling it less often only
// makes reminders late — and a reminder late enough to have missed its grace
// window is dropped rather than sent about an event already under way.
func (s *Server) handleSendReminders(w http.ResponseWriter, r *http.Request) {
	sent, err := s.SendDueReminders()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent})
}

// handleRebuildAllTables reposts every guild's table.
//
// The endpoint the hourly job calls, so that job does not have to name a guild.
// A cron line carrying a guild id would be a second place the deployment's
// identity lives, and the wrong place: this service already knows which guilds
// it holds events for.
func (s *Server) handleRebuildAllTables(w http.ResponseWriter, r *http.Request) {
	guilds, err := s.store.GuildsWithEvents()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var problems []string
	for _, guildID := range guilds {
		if err := s.RebuildEventTable(guildID); err != nil {
			log.Printf("[discord-signup] rebuild table for guild %s: %v", guildID, err)
			problems = append(problems, guildID+": "+err.Error())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"guilds": len(guilds), "problems": problems})
}

// handleCompleteFinished archives events whose time has passed. Exposed as well
// as run on a ticker so it can be triggered and tested without waiting.
func (s *Server) handleCompleteFinished(w http.ResponseWriter, r *http.Request) {
	finished, err := s.CompleteFinishedEvents()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"completed": len(finished), "event_ids": finished,
	})
}

// handleSyncGuild pulls a guild's native Discord events into the store. This is
// the endpoint the scheduler job calls.
func (s *Server) handleSyncGuild(w http.ResponseWriter, r *http.Request) {
	result, err := s.SyncScheduledEvents(r.PathValue("guildID"), s.boardChannelID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this cannot become an error
		// response. Log it rather than let a truncated body look like success.
		log.Printf("[discord-signup] encode response: %v", err)
	}
}

// writeStoreError maps a store error onto a status code. Every branch is
// explicit: an unmapped error becomes a 500 with the real message in the log,
// never a 200 with an empty body.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInvalidEvent):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrEventNotOpen):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		log.Printf("[discord-signup] %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"data_dir":        s.store.DataDir(),
		"discord_wired":   s.discord != nil,
		"signature_ready": s.verifier != nil,
	})
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents(r.URL.Query().Get("guild_id"), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	var in Event
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON: " + err.Error()})
		return
	}
	ev, err := s.store.CreateEvent(in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	ev, err := s.store.GetEvent(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	var patch EventPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON: " + err.Error()})
		return
	}
	before, err := s.store.GetEvent(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The third copy of the edit rule, and now the same one the Discord modal
	// and the web form take. On its own it saved the row, refreshed the card
	// and pushed the title — and promoted nobody, so raising a limit through
	// the API left the waitlist sitting behind places that were already free.
	ev, _, err := s.applyEventEdit(before, patch, "api")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	if err := s.store.DeleteEvent(id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	ev, err := s.PostSignupMessage(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// handlePublish creates a native Discord scheduled event for a local roster.
// The browser has had this since the web page existed; the machine API had not,
// which made the two surfaces disagree about what was possible.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	ev, err := s.PublishToDiscord(id, s.boardChannelID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleRoster(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	includeWithdrawn := r.URL.Query().Get("include_withdrawn") == "true"
	roster, err := s.store.Roster(id, includeWithdrawn)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signups": roster})
}

// handleAdminJoin adds someone by id, for the case where a person cannot press
// the button themselves. It goes through exactly the same Join path as a click,
// so the cap and the waitlist ordering apply to an operator too.
func (s *Server) handleAdminJoin(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	var in struct {
		DiscordUserID string `json:"discord_user_id"`
		DisplayName   string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON: " + err.Error()})
		return
	}
	result, err := s.store.Join(id, in.DiscordUserID, in.DisplayName, JoinedViaOperator)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	go s.syncAfterChange(id, []stateChange{{UserID: in.DiscordUserID, State: result.Signup.State}})
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAdminLeave(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	userID := r.PathValue("userID")
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		actor = "operator"
	}
	result, err := s.store.Leave(id, userID, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if ev, err := s.store.GetEvent(id); err == nil {
		changes := []stateChange{{UserID: userID, State: StateWithdrawn}}
		if result.Promoted != nil {
			changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
		}
		go s.syncAfterChange(id, changes)
		if result.Promoted != nil {
			go s.notifyPromoted(ev, result.Promoted)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	history, err := s.store.History(id, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": history})
}
