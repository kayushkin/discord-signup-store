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
	// botID is the bot's own Discord user id, looked up once. Needed to read
	// which roles the bot holds, which is what decides what it can grant.
	botID     string
	botIDOnce sync.Once
	// boardChannelID is the channel signup cards are posted to. Imported
	// Discord events land here rather than in their own channel, which for a
	// voice event is the room people talk in and where a card would be unread.
	boardChannelID string
}

// EnableWeb turns on the browser surface at YOUR_DOMAIN.
//
// Separate from NewServer because the roster, the buttons and the interaction
// endpoint all work without it — the web page is management, not the product.
func (s *Server) EnableWeb(oauth *OAuthConfig, boardChannelID string) {
	s.oauth = oauth
	s.boardChannelID = boardChannelID
}

// BoardChannelID reports where signup cards are posted.
func (s *Server) BoardChannelID() string { return s.boardChannelID }

// NewServer wires the pieces. discord may be nil, in which case the roster
// still works and nothing is pushed to Discord — useful in tests and for a
// first run before the bot token is filed in auth-store.
func NewServer(store *Store, verifier *InteractionVerifier, discord *DiscordClient) *Server {
	return &Server{store: store, verifier: verifier, discord: discord}
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
	mux.HandleFunc("GET /api/events/{id}/signups", s.handleRoster)
	mux.HandleFunc("POST /api/events/{id}/signups", s.handleAdminJoin)
	mux.HandleFunc("DELETE /api/events/{id}/signups/{userID}", s.handleAdminLeave)
	mux.HandleFunc("GET /api/events/{id}/history", s.handleHistory)
	mux.HandleFunc("POST /api/guilds/{guildID}/sync", s.handleSyncGuild)
	mux.HandleFunc("POST /api/events/complete-finished", s.handleCompleteFinished)

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
	ev, err := s.store.UpdateEvent(id, patch)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Capacity or status may have changed, so the public message is now stale.
	go func() {
		if err := s.RefreshSignupMessage(id); err != nil {
			log.Printf("[discord-signup] refresh after update event=%d: %v", id, err)
		}
	}()
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
	if ev, err := s.store.GetEvent(id); err == nil {
		go s.syncAfterChange(ev, []stateChange{{UserID: in.DiscordUserID, State: result.Signup.State}})
	}
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
		go s.syncAfterChange(ev, changes)
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
