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

	// background counts the work a request starts and does not wait for —
	// publishing to Discord, telling someone they were promoted — so a test
	// can wait for it before removing the database under it.
	background sync.WaitGroup

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
	// syncs serialises Discord writes per event. Without it, two roster
	// changes seconds apart raced and the older one could land last, leaving
	// every public surface showing a count that was already wrong.
	syncs *eventSyncQueue
	// tableLocks serialises redraws of one guild's tables. A redraw reads
	// which messages it owns, posts any it lacks and records them; two at once
	// both post the same page and one record overwrites the other, leaving a
	// message nothing edits or deletes again. That happened on 2026-09-04.
	tableLocks   map[string]*sync.Mutex
	tableLocksMu sync.Mutex
	// gatewayStatus reads the gateway supervisor's state for /healthz. Nil
	// means no supervisor was started, which /healthz reports as "disabled".
	gatewayStatus func() GatewayStatus
}

// ReportGatewayStatus makes /healthz report the gateway's state from status.
// Call it before the server starts serving.
func (s *Server) ReportGatewayStatus(status func() GatewayStatus) {
	s.gatewayStatus = status
}

// EnableWeb turns on the browser surface at YOUR_DOMAIN.
//
// Separate from NewServer because the roster, the buttons and the interaction
// endpoint all work without it — the web page is management, not the product.
func (s *Server) EnableWeb(oauth *OAuthConfig) {
	s.oauth = oauth
}

// guildChannels is where a guild's cards, past-events lines and reminders go.
// Per guild since 2026-09-04; before that one env var each, and a second
// server posted into the first server's channels. A guild with nothing
// recorded gets empty strings, and every caller treats empty as "not here".
func (s *Server) guildChannels(guildID string) GuildChannels {
	ch, err := s.store.GuildChannels(guildID)
	if err != nil {
		log.Printf("[discord-signup] read channels for guild %s: %v", guildID, err)
	}
	return ch
}

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

// NewServer wires the pieces. discord may be nil, in which case the roster
// still works and nothing is pushed to Discord — useful in tests and for a
// first run before the bot token is filed in auth-store.
func NewServer(store *Store, verifier *InteractionVerifier, discord *DiscordClient) *Server {
	return &Server{store: store, verifier: verifier, discord: discord, syncs: newEventSyncQueue(),
		tableLocks: map[string]*sync.Mutex{}}
}

// inBackground runs work a request does not wait for, counted so
// WaitForBackgroundWork can wait for it.
func (s *Server) inBackground(work func()) {
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		work()
	}()
}

// WaitForBackgroundWork blocks until everything inBackground started has
// finished. Tests call it before their temporary database is removed; the
// running service never needs to.
func (s *Server) WaitForBackgroundWork() { s.background.Wait() }

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
	mux.HandleFunc("POST /api/events/{id}/publish", s.handlePublish)
	mux.HandleFunc("GET /api/events/{id}/signups", s.handleRoster)
	mux.HandleFunc("POST /api/events/{id}/signups", s.handleAdminJoin)
	mux.HandleFunc("DELETE /api/events/{id}/signups/{userID}", s.handleAdminLeave)
	mux.HandleFunc("POST /api/events/{id}/maybe", s.handleAdminMaybe)
	mux.HandleFunc("GET /api/site-admins", s.handleListSiteAdmins)
	mux.HandleFunc("PUT /api/site-admins/{userID}", s.handleAddSiteAdmin)
	mux.HandleFunc("DELETE /api/site-admins/{userID}", s.handleRemoveSiteAdmin)
	mux.HandleFunc("GET /api/readable-names", s.handleListReadableNames)
	mux.HandleFunc("PUT /api/readable-names/{userID}", s.handleSetReadableName)
	mux.HandleFunc("DELETE /api/readable-names/{userID}", s.handleDeleteReadableName)
	mux.HandleFunc("GET /api/events/{id}/history", s.handleHistory)
	mux.HandleFunc("GET /api/events/{id}/updates", s.handleEventUpdates)
	mux.HandleFunc("POST /api/guilds/{guildID}/sync", s.handleSyncGuild)
	mux.HandleFunc("POST /api/sync", s.handleSyncAllGuilds)
	mux.HandleFunc("PUT /api/guilds/{guildID}/table", s.handleSetGuildTable)
	mux.HandleFunc("PUT /api/guilds/{guildID}/management", s.handleSetGuildManagement)
	mux.HandleFunc("PUT /api/guilds/{guildID}/channels", s.handleSetGuildChannels)
	mux.HandleFunc("POST /api/guilds/{guildID}/setup", s.handleSetUpGuild)
	mux.HandleFunc("GET /api/guilds/{guildID}/channels", s.handleGetGuildChannels)
	mux.HandleFunc("POST /api/guilds/{guildID}/table/refresh", s.handleRefreshGuildTable)
	mux.HandleFunc("PUT /api/guilds/{guildID}/forum", s.handleSetGuildForum)
	mux.HandleFunc("GET /api/guilds/{guildID}/editing", s.handleGetGuildEditing)
	mux.HandleFunc("PUT /api/guilds/{guildID}/editing", s.handleSetGuildEditing)
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
	mux.HandleFunc("POST /events/{id}/roster/promote", s.handleWebRosterPromote)
	mux.HandleFunc("POST /events/{id}/roster/add", s.handleWebRosterAdd)
	mux.HandleFunc("POST /events/{id}/invite", s.handleWebInvite)
	mux.HandleFunc("POST /events/{id}/waitlist/move", s.handleWebWaitlistMove)
	mux.HandleFunc("POST /events/{id}/holds/release", s.handleWebReleaseHold)
	mux.HandleFunc("POST /events/{id}/signups", s.handleWebToggleSignups)
	mux.HandleFunc("POST /events/{id}/cancel", s.handleWebCancelEvent)
	mux.HandleFunc("GET /events/{id}/members", s.handleWebMemberSearch)
	mux.HandleFunc("POST /events/{id}/publish", s.handleWebPublish)
	mux.HandleFunc("POST /events/{id}/end", s.handleWebEndEvent)
	mux.HandleFunc("POST /preferences/home-server", s.handleWebSetHomeServer)
	mux.HandleFunc("GET /names", s.handleWebNames)
	mux.HandleFunc("GET /names/members", s.handleWebNameSearch)
	mux.HandleFunc("POST /names", s.handleWebSetName)
}

// handleSetGuildManagement points a guild's management table at a channel and
// draws it. The management table hangs off the event table, so this is a 404
// until PUT .../table has been called.
func (s *Server) handleSetGuildManagement(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ChannelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "channel_id is required"})
		return
	}
	guildID := r.PathValue("guildID")
	if err := s.store.SetGuildManagementChannel(guildID, in.ChannelID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.RefreshManagementTable(guildID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// handleSetGuildChannels records where a guild's cards, past-events lines,
// reminders and new-events lines go. All four every time — a PUT is the whole
// value — and an empty reminder or new-events channel turns that off for the
// guild.
func (s *Server) handleSetGuildChannels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BoardChannelID     string `json:"board_channel_id"`
		PastChannelID      string `json:"past_channel_id"`
		ReminderChannelID  string `json:"reminder_channel_id"`
		NewEventsChannelID string `json:"new_events_channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON"})
		return
	}
	if in.BoardChannelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "board_channel_id is required: it is where cards are posted"})
		return
	}
	guildID := r.PathValue("guildID")
	if err := s.store.SetGuildChannels(guildID, GuildChannels{
		Board: in.BoardChannelID, Past: in.PastChannelID, Reminder: in.ReminderChannelID,
		NewEvents: in.NewEventsChannelID}); err != nil {
		writeStoreError(w, err)
		return
	}
	s.handleGetGuildChannels(w, r)
}

// handleGetGuildChannels reads the guild's row back: table, management and the
// three channels.
func (s *Server) handleGetGuildChannels(w http.ResponseWriter, r *http.Request) {
	table, err := s.store.GuildTable(r.PathValue("guildID"))
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

// handleEventUpdates returns what has been changed about an event, and by whom.
//
// Separate from /history, which is the roster's: one answers "who is going and
// how did that change", the other "what is this event and how did that change".
func (s *Server) handleEventUpdates(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	updates, err := s.store.EventUpdates(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_updates": updates})
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
	result, err := s.SyncScheduledEvents(r.PathValue("guildID"))
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
	case errors.Is(err, ErrEventNotOpen), errors.Is(err, ErrEventFull):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		log.Printf("[discord-signup] %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

// handleHealth answers 200 whatever the gateway's state, on purpose: the
// buttons, the rosters and the web pages all work without the gateway, and a
// failing health route is how a watcher decides to restart a unit. A Discord
// outage must not become a restart loop. A reader that cares about Interested
// reads gateway.state.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	gateway := GatewayStatus{State: GatewayDisabled}
	if s.gatewayStatus != nil {
		gateway = s.gatewayStatus()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"data_dir":        s.store.DataDir(),
		"discord_wired":   s.discord != nil,
		"signature_ready": s.verifier != nil,
		"gateway":         gateway,
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

// handlePublish creates a native Discord scheduled event for a local roster.
// The browser has had this since the web page existed; the machine API had not,
// which made the two surfaces disagree about what was possible.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	ev, err := s.PublishToDiscord(id)
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
// handleAdminMaybe puts someone on the Maybe list by id, through the same
// store call as the Maybe button.
func (s *Server) handleAdminMaybe(w http.ResponseWriter, r *http.Request) {
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
	result, err := s.store.MarkMaybe(id, in.DiscordUserID, in.DisplayName, JoinedViaOperator)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	changes := []stateChange{{UserID: in.DiscordUserID, State: StateMaybe}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
		if ev, err := s.store.GetEvent(id); err == nil {
			s.inBackground(func() { s.notifyPromoted(ev, result.Promoted) })
		}
	}
	s.inBackground(func() { s.syncAfterChange(id, changes) })
	writeJSON(w, http.StatusOK, result)
}

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
	s.inBackground(func() { s.syncAfterChange(id, []stateChange{{UserID: in.DiscordUserID, State: result.Signup.State}}) })
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
		s.inBackground(func() { s.syncAfterChange(id, changes) })
		if result.Promoted != nil {
			s.inBackground(func() { s.notifyPromoted(ev, result.Promoted) })
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
	writeJSON(w, http.StatusOK, map[string]any{"signup_updates": history})
}
