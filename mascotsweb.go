package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------- the mascot a page shows

// siteMascot is the site's own mascot, Maleeha, built into the binary: what
// a page shows when no server is chosen, or the server has none of its own.
var siteMascot = mascotView{
	Small: "/art/maleeha-128.webp",
	Large: "/art/maleeha-360.webp",
	Alt:   "Maleeha in her jester hood, tongue out, waving",
}

// mascotView is the mascot a page shows. Reactions maps each of
// mascotReactions to its loop; it is empty for a mascot that cannot be posed
// or whose loops are not printed yet, and the page then moves the still
// drawing instead.
type mascotView struct {
	GuildID   string
	Small     string
	Large     string
	Alt       string
	Reactions map[string]string
}

// mascotReactionQuery is the query that has the next page's mascot play a
// reaction.
func mascotReactionQuery(reaction string) string {
	return url.Values{"mascot": {reaction}}.Encode()
}

// pageMascot is the mascot for a page: that of the server the page names —
// the one being set up on the mascot page, or the event's — or else of the
// server the viewer chose for their home page.
func (s *Server) pageMascot(data pageData) (mascotView, error) {
	guildID := data.MascotGuildID
	if guildID == "" && data.Event != nil {
		guildID = data.Event.GuildID
	}
	if guildID == "" && data.Session != nil {
		home, err := s.store.HomeGuildOf(data.Session.DiscordUserID)
		if err != nil {
			return siteMascot, err
		}
		guildID = home
	}
	if guildID == "" {
		return siteMascot, nil
	}
	mascot, err := s.store.GuildMascotOf(guildID)
	if errors.Is(err, ErrNotFound) {
		return siteMascot, nil
	}
	if err != nil {
		return siteMascot, err
	}
	base := "/mascots/" + url.PathEscape(guildID) + "/"
	version := "?v=" + strconv.FormatInt(mascot.SetAt, 10)
	view := mascotView{GuildID: guildID, Small: base + "portrait.webp" + version, Large: base + "portrait.webp" + version,
		Alt: "This server's mascot", Reactions: map[string]string{}}
	if mascot.ReactionsReady() {
		for _, reaction := range mascotReactions {
			view.Reactions[reaction] = base + reaction + ".webp" + version
		}
	}
	return view, nil
}

// handleMascotImage serves a server's mascot to anyone, at
// /mascots/{guildID}/portrait.webp, full.webp or {reaction}.webp: it is on
// every page of that server.
func (s *Server) handleMascotImage(w http.ResponseWriter, r *http.Request) {
	print, isWebP := strings.CutSuffix(r.PathValue("file"), ".webp")
	if !isWebP {
		http.NotFound(w, r)
		return
	}
	image, err := s.store.GuildMascotImage(r.PathValue("guildID"), print)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The address carries the time the mascot was set, so a new mascot is a
	// new address; short all the same, as the mascot can be cleared.
	writeWebPImage(w, image, "public, max-age=300")
}

// ---------------------------------------------------------------- who may set it

// mayManageGuildMascot is whether someone may set a server's mascot: its
// owner, or a site admin.
func (s *Server) mayManageGuildMascot(session *WebSession, guildID string) (bool, error) {
	admin, err := s.store.IsSiteAdmin(session.DiscordUserID)
	if err != nil || admin {
		return admin, err
	}
	if !session.IsMemberOf(guildID) {
		return false, nil
	}
	ownerID, err := s.store.BotGuildOwnerID(guildID)
	if err != nil {
		return false, err
	}
	return ownerID == session.DiscordUserID, nil
}

// mascotGuildsOf is the servers whose mascot someone may set.
func (s *Server) mascotGuildsOf(session *WebSession) ([]Guild, error) {
	guilds, err := s.store.BotGuilds()
	if err != nil {
		return nil, err
	}
	var out []Guild
	for _, g := range guilds {
		ok, err := s.mayManageGuildMascot(session, g.ID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", g.Name, err)
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- the mascot page

// mascotPage is what the mascot page shows for one server.
type mascotPage struct {
	Guild         Guild
	Guilds        []Guild
	Current       *GuildMascot
	Drawings      []MascotDrawing
	MemberAvatars []MascotAvatarChoice
	Request       *MascotRequest
	PhotosReady   bool
	DrawingsLeft  int
	PerDay        int
	CommentLimit  int
	Reactions     []string
}

func mascotRedirect(w http.ResponseWriter, r *http.Request, guildID, notice string) {
	http.Redirect(w, r, "/mascot?"+url.Values{"guild_id": {guildID}, "notice": {notice}}.Encode(), http.StatusSeeOther)
}

// guildForMascotForm reads the server a mascot form names and checks the
// viewer may set its mascot, answering the request itself when not.
func (s *Server) guildForMascotForm(w http.ResponseWriter, r *http.Request, session *WebSession) (string, bool) {
	guildID := strings.TrimSpace(r.FormValue("guild_id"))
	if guildID == "" {
		http.Error(w, "guild_id is required", http.StatusBadRequest)
		return "", false
	}
	ok, err := s.mayManageGuildMascot(session, guildID)
	if err != nil {
		http.Error(w, "could not check who may set this server's mascot: "+err.Error(), http.StatusBadGateway)
		return "", false
	}
	if !ok {
		http.Error(w, "only the server's owner can set its mascot", http.StatusForbidden)
		return "", false
	}
	return guildID, true
}

func (s *Server) handleWebMascot(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	data := pageData{Title: "Server mascot", Session: session, Notice: r.URL.Query().Get("notice")}
	guilds, err := s.mascotGuildsOf(session)
	if err != nil {
		http.Error(w, "could not check whose mascots you may set: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(guilds) == 0 {
		http.Error(w, "only a server's owner can set its mascot, and you own none of the servers this bot is in", http.StatusForbidden)
		return
	}
	// The server asked for, or the one on their home page, or the first.
	wanted := r.URL.Query().Get("guild_id")
	if wanted == "" {
		if wanted, err = s.store.HomeGuildOf(session.DiscordUserID); err != nil {
			data.Error = err.Error()
		}
	}
	page := &mascotPage{Guilds: guilds, Guild: guilds[0], PerDay: mascotDrawingsPerDay,
		CommentLimit: avatarCommentMaximumCharacters, Reactions: mascotReactions, PhotosReady: s.files != nil}
	for _, g := range guilds {
		if g.ID == wanted {
			page.Guild = g
		}
	}
	if r.URL.Query().Get("guild_id") != "" && page.Guild.ID != wanted {
		http.Error(w, "only the server's owner can set its mascot", http.StatusForbidden)
		return
	}
	data.MascotGuildID = page.Guild.ID
	var problems []string
	if data.Error != "" {
		problems = append(problems, data.Error)
	}
	if page.Current, err = s.store.GuildMascotOf(page.Guild.ID); err != nil && !errors.Is(err, ErrNotFound) {
		problems = append(problems, "Could not load the mascot: "+err.Error())
	}
	if page.Drawings, err = s.store.MascotDrawingsOf(page.Guild.ID, false); err != nil {
		problems = append(problems, "Could not load the mascot drawings: "+err.Error())
	}
	alsoMember := ""
	if session.IsMemberOf(page.Guild.ID) {
		alsoMember = session.DiscordUserID
	}
	if page.MemberAvatars, err = s.store.MascotAvatarChoices(page.Guild.ID, alsoMember); err != nil {
		problems = append(problems, "Could not load members' avatars: "+err.Error())
	}
	if page.Request, err = s.store.LatestMascotRequestOf(page.Guild.ID); err != nil && !errors.Is(err, ErrNotFound) {
		problems = append(problems, "Could not load the last request: "+err.Error())
	}
	if page.DrawingsLeft, err = s.mascotDrawingsLeftToday(page.Guild.ID); err != nil {
		problems = append(problems, "Could not count today's drawings: "+err.Error())
	}
	data.MascotPage = page
	data.Error = strings.Join(problems, " ")
	s.render(w, "mascot.html", data)
}

// mascotDrawingsLeftToday is how many more mascot drawings the server may ask
// for.
func (s *Server) mascotDrawingsLeftToday(guildID string) (int, error) {
	asked, err := s.store.MascotDrawingsAskedForSince(guildID, now()-24*3600)
	if err != nil {
		return 0, err
	}
	return max(0, mascotDrawingsPerDay-asked), nil
}

// handleWebMascotStatus answers the mascot page's script, which asks every
// few seconds while a drawing is under way.
func (s *Server) handleWebMascotStatus(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFrom(r)
	if session == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again"})
		return
	}
	guildID := r.URL.Query().Get("guild_id")
	ok, err := s.mayManageGuildMascot(session, guildID)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the server's owner can see this"})
		return
	}
	request, err := s.store.LatestMascotRequestOf(guildID)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"request": nil})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": request})
}

// handleWebMascotRequest takes a request for a mascot drawing: from words, a
// photo, or a change to one of the server's drawings.
func (s *Server) handleWebMascotRequest(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, avatarPhotoMaximumBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, fmt.Sprintf("that upload could not be read; a photo may be at most %d MB (%v)", avatarPhotoMaximumBytes>>20, err), http.StatusBadRequest)
		return
	}
	guildID, ok := s.guildForMascotForm(w, r, session)
	if !ok {
		return
	}
	asked := mascotRequestAsked{Kind: r.FormValue("kind"), Comment: strings.TrimSpace(r.FormValue("comment"))}
	if utf8.RuneCountInString(asked.Comment) > avatarCommentMaximumCharacters {
		mascotRedirect(w, r, guildID, fmt.Sprintf("Keep the description to %d characters.", avatarCommentMaximumCharacters))
		return
	}
	if asked.Kind == MascotRequestEditDrawing {
		var err error
		if asked.BaseDrawingID, err = strconv.ParseInt(r.FormValue("base_drawing_id"), 10, 64); err != nil {
			mascotRedirect(w, r, guildID, "Choose the drawing to change.")
			return
		}
	}
	left, err := s.mascotDrawingsLeftToday(guildID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if left == 0 {
		mascotRedirect(w, r, guildID, fmt.Sprintf("This server has asked for %d mascot drawings in the last day, which is the most. Try again tomorrow.", mascotDrawingsPerDay))
		return
	}
	if asked.Kind == MascotRequestFromPhoto {
		if s.files == nil {
			http.Error(w, ErrFileStoreNotConfigured.Error(), http.StatusServiceUnavailable)
			return
		}
		if r.FormValue("consent") != "yes" {
			mascotRedirect(w, r, guildID, "Tick the box to say you may use the photo, and that anyone in it agreed.")
			return
		}
		fileID, notice := s.uploadMascotPhoto(r, guildID)
		if fileID == "" {
			mascotRedirect(w, r, guildID, notice)
			return
		}
		asked.PhotoFileID = fileID
	}
	if _, err := s.store.RequestMascotDrawing(guildID, session.DiscordUserID, asked); err != nil {
		if asked.PhotoFileID != "" {
			s.purgeMascotPhoto(0, asked.PhotoFileID)
		}
		mascotRedirect(w, r, guildID, "Could not ask for the drawing: "+err.Error())
		return
	}
	log.Printf("[discord-signup] mascot %s of guild %s asked for by web:%s", asked.Kind, guildID, session.DiscordUserID)
	mascotRedirect(w, r, guildID, "The mascot is being drawn. It appears here when it is done, usually within ten minutes.")
}

// uploadMascotPhoto reads the uploaded photo and keeps it in file-store until
// the drawing is done. It returns the file id, or "" and what to tell them.
func (s *Server) uploadMascotPhoto(r *http.Request, guildID string) (string, string) {
	file, _, err := r.FormFile("photo")
	if err != nil {
		return "", "Choose a photo to upload."
	}
	defer file.Close()
	photo, err := io.ReadAll(io.LimitReader(file, avatarPhotoMaximumBytes+1))
	if err != nil {
		return "", "Could not read the upload: " + err.Error()
	}
	if len(photo) > avatarPhotoMaximumBytes {
		return "", fmt.Sprintf("That photo is over %d MB.", avatarPhotoMaximumBytes>>20)
	}
	contentType := http.DetectContentType(photo)
	filename, readable := avatarPhotoTypes[contentType]
	if !readable {
		return "", "That file is not a JPEG, PNG or WebP photo."
	}
	fileID, err := s.files.UploadMascotPhoto(guildID, filename, contentType, photo)
	if err != nil {
		log.Printf("[discord-signup] mascot photo for guild %s: %v", guildID, err)
		return "", "Could not keep the photo: " + err.Error()
	}
	return fileID, ""
}

// purgeMascotPhoto destroys a request's photo once it is not needed, and
// forgets its id when the request (requestID, 0 for none recorded) named it.
// A photo that cannot be purged stays named on the request, and is logged.
func (s *Server) purgeMascotPhoto(requestID int64, fileID string) {
	if fileID == "" {
		return
	}
	if s.files == nil {
		log.Printf("[discord-signup] mascot photo %s of request %d kept: %v", fileID, requestID, ErrFileStoreNotConfigured)
		return
	}
	if err := s.files.PurgePhoto(fileID); err != nil {
		log.Printf("[discord-signup] purge mascot photo %s of request %d: %v", fileID, requestID, err)
		return
	}
	if requestID != 0 {
		if err := s.store.ForgetMascotRequestPhoto(requestID); err != nil {
			log.Printf("[discord-signup] mascot request %d: %v", requestID, err)
		}
	}
}

// handleWebMascotChoose sets the server's mascot: one of its drawings
// (source=drawing), a member's shown avatar (source=avatar), or the site's
// own (source=site).
func (s *Server) handleWebMascotChoose(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	guildID, ok := s.guildForMascotForm(w, r, session)
	if !ok {
		return
	}
	source := r.FormValue("source")
	drawingID, idErr := strconv.ParseInt(r.FormValue("drawing_id"), 10, 64)
	if source != "site" && idErr != nil {
		http.Error(w, "drawing_id is required", http.StatusBadRequest)
		return
	}
	var err error
	switch source {
	case "drawing":
		err = s.store.SetGuildMascotToMascotDrawing(guildID, session.DiscordUserID, drawingID)
	case "avatar":
		alsoMember := ""
		if session.IsMemberOf(guildID) {
			alsoMember = session.DiscordUserID
		}
		err = s.store.SetGuildMascotToAvatar(guildID, session.DiscordUserID, alsoMember, drawingID)
	case "site":
		err = s.store.ClearGuildMascot(guildID, session.DiscordUserID)
	default:
		http.Error(w, "source must be drawing, avatar or site", http.StatusBadRequest)
		return
	}
	if err != nil {
		mascotRedirect(w, r, guildID, "Could not set the mascot: "+err.Error())
		return
	}
	if source == "site" {
		mascotRedirect(w, r, guildID, "The server shows the site's own mascot again.")
		return
	}
	mascotRedirect(w, r, guildID, "That is the server's mascot now.")
}

func (s *Server) handleWebMascotDeleteDrawing(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	guildID, ok := s.guildForMascotForm(w, r, session)
	if !ok {
		return
	}
	drawingID, err := strconv.ParseInt(r.FormValue("drawing_id"), 10, 64)
	if err != nil {
		http.Error(w, "drawing_id is required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteMascotDrawing(guildID, session.DiscordUserID, drawingID); err != nil {
		mascotRedirect(w, r, guildID, "Could not delete it: "+err.Error())
		return
	}
	mascotRedirect(w, r, guildID, "That drawing is deleted.")
}

// handleWebMascotDrawing shows one of a server's mascot drawings, at
// /mascot/drawings/{id}.webp or {id}-full.webp, to whoever may set its
// mascot. Anyone else gets a 404.
func (s *Server) handleWebMascotDrawing(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	idText, isWebP := strings.CutSuffix(r.PathValue("file"), ".webp")
	idText, fullBody := strings.CutSuffix(idText, "-full")
	drawingID, err := strconv.ParseInt(idText, 10, 64)
	if !isWebP || err != nil {
		http.NotFound(w, r)
		return
	}
	guildID, image, err := s.store.MascotDrawingImage(drawingID, fullBody)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ok, err := s.mayManageGuildMascot(session, guildID); err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	writeWebPImage(w, image, "private, max-age=86400")
}

// ---------------------------------------------------------------- machine API

// handleGetGuildMascot is GET /api/guilds/{guildID}/mascot: the mascot, 404
// when the server shows the site's own.
func (s *Server) handleGetGuildMascot(w http.ResponseWriter, r *http.Request) {
	mascot, err := s.store.GuildMascotOf(r.PathValue("guildID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mascot)
}

// mascotRequestToDraw is a mascot request with what the drawer needs.
type mascotRequestToDraw struct {
	MascotRequest
	GuildName         string `json:"guild_name"`
	BaseDrawingCode   string `json:"base_drawing_code,omitempty"`
	BaseDrawingFormat string `json:"base_drawing_format,omitempty"`
	HasPhoto          bool   `json:"has_photo"`
}

func (s *Server) handleMascotRequestsToDraw(w http.ResponseWriter, r *http.Request) {
	requests, err := s.store.MascotRequestsToDraw()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	guilds, err := s.store.BotGuilds()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	names := map[string]string{}
	for _, g := range guilds {
		names[g.ID] = g.Name
	}
	out := []mascotRequestToDraw{}
	for _, request := range requests {
		item := mascotRequestToDraw{MascotRequest: request, GuildName: names[request.GuildID], HasPhoto: request.PhotoFileID != ""}
		if request.BaseDrawingID != 0 {
			base, err := s.store.MascotDrawingByID(request.BaseDrawingID)
			if err != nil {
				// Deleted since it was asked for: there is nothing to change.
				log.Printf("[discord-signup] mascot request %d: base drawing %d: %v", request.ID, request.BaseDrawingID, err)
				continue
			}
			item.BaseDrawingCode, item.BaseDrawingFormat = base.DrawingCode, base.Format
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// handleMascotRequestPhoto gives the drawer the request's photo, only while
// the drawing is under way.
func (s *Server) handleMascotRequestPhoto(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	request, err := s.store.MascotRequestByID(requestID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if request.State != AvatarRequestDrawing {
		writeAvatarError(w, fmt.Errorf("%w (the photo is read only while a drawing is under way)", ErrAvatarState))
		return
	}
	if request.PhotoFileID == "" {
		writeStoreError(w, fmt.Errorf("%w: the request has no photo", ErrNotFound))
		return
	}
	if s.files == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": ErrFileStoreNotConfigured.Error()})
		return
	}
	photo, contentType, err := s.files.PhotoContent(request.PhotoFileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(photo)
}

func (s *Server) handleMascotRequestStarted(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.StartMascotRequest(requestID); err != nil {
		writeAvatarError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMascotRequestDrawing(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	drawing, ok := decodeDrawing(w, r, nil)
	if !ok {
		return
	}
	drawingID, photoFileID, err := s.store.FinishMascotRequest(requestID, drawing)
	if err != nil {
		writeAvatarError(w, err)
		return
	}
	s.purgeMascotPhoto(requestID, photoFileID)
	writeJSON(w, http.StatusOK, map[string]int64{"drawing_id": drawingID})
}

func (s *Server) handleMascotRequestFailed(w http.ResponseWriter, r *http.Request) {
	requestID, err := requestIDFrom(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"reason\": \"…\"} is required"})
		return
	}
	photoFileID, err := s.store.FailMascotRequest(requestID, body.Reason)
	if err != nil {
		writeAvatarError(w, err)
		return
	}
	s.purgeMascotPhoto(requestID, photoFileID)
	w.WriteHeader(http.StatusNoContent)
}

// handleMascotsToReact lists the character mascots whose reaction loops are
// still to print, and the reactions to print.
func (s *Server) handleMascotsToReact(w http.ResponseWriter, r *http.Request) {
	mascots, err := s.store.MascotsToReact()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mascots": mascots, "reactions": mascotReactions})
}

// mascotReactionsBody names the drawing printed, so prints of a mascot
// replaced meanwhile are refused.
type mascotReactionsBody struct {
	MascotDrawingID int64 `json:"mascot_drawing_id"`
	AvatarDrawingID int64 `json:"avatar_drawing_id"`
	// Prints maps each reaction to its loop, base64 in JSON.
	Prints map[string][]byte `json:"prints"`
	Reason string            `json:"reason"`
}

func writeMascotError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrMascotChanged) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeStoreError(w, err)
}

// handleSaveMascotReactions is PUT /api/guilds/{guildID}/mascot/reactions.
func (s *Server) handleSaveMascotReactions(w http.ResponseWriter, r *http.Request) {
	var body mascotReactionsBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(len(mascotReactions))*4*avatarImageMaximumBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"mascot_drawing_id\", \"avatar_drawing_id\", \"prints\"} is required"})
		return
	}
	for reaction, image := range body.Prints {
		if !isWebP(image) || len(image) > 2*avatarImageMaximumBytes {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%s must be a WebP image of at most %d bytes", reaction, 2*avatarImageMaximumBytes)})
			return
		}
	}
	if err := s.store.SaveMascotReactions(r.PathValue("guildID"), body.MascotDrawingID, body.AvatarDrawingID, body.Prints); err != nil {
		writeMascotError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMascotReactionsFailed is POST /api/guilds/{guildID}/mascot/reactions-failed.
func (s *Server) handleMascotReactionsFailed(w http.ResponseWriter, r *http.Request) {
	var body mascotReactionsBody
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" || body.Prints != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a body {\"mascot_drawing_id\", \"avatar_drawing_id\", \"reason\"} is required"})
		return
	}
	if err := s.store.FailMascotReactions(r.PathValue("guildID"), body.MascotDrawingID, body.AvatarDrawingID, body.Reason); err != nil {
		writeMascotError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
