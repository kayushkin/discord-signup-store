package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// The names page: one place to set the short name each person is shown by on
// Discord and on these pages. Only site admins and a server's owner may open
// it — anyone else gets a 404, and the home page does not link it. A short
// name is one per person across every server, so it is the call of whoever
// runs the bot, or owns the server, and not of every organiser. A site admin
// sees everyone on a list in every server; an owner, the people in theirs.

// namedPerson is one row on the names page.
type namedPerson struct {
	DiscordUserID string
	DisplayName   string
	ReadableName  string
	Events        int
}

// PeopleOnListsIn is everyone going, maybe or waitlisted on any event in the
// given servers, with the short name set for them, if any.
func (s *Store) PeopleOnListsIn(guildIDs []string) ([]namedPerson, error) {
	if len(guildIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(guildIDs)), ",")
	args := []any{StateWithdrawn}
	for _, id := range guildIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`
		SELECT s.discord_user_id, MAX(s.display_name), COALESCE(r.readable_name, ''),
		       COUNT(DISTINCT s.event_id)
		FROM signups s
		JOIN events e ON e.id = s.event_id AND e.deleted_at = 0
		LEFT JOIN readable_names r ON r.discord_user_id = s.discord_user_id
		WHERE s.state != ? AND e.guild_id IN (`+placeholders+`)
		GROUP BY s.discord_user_id
		ORDER BY LOWER(COALESCE(NULLIF(r.readable_name, ''), MAX(s.display_name)))`, args...)
	if err != nil {
		return nil, fmt.Errorf("list people on lists: %w", err)
	}
	defer rows.Close()
	var out []namedPerson
	for rows.Next() {
		var p namedPerson
		if err := rows.Scan(&p.DiscordUserID, &p.DisplayName, &p.ReadableName, &p.Events); err != nil {
			return nil, fmt.Errorf("scan person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// guildsWhereMayName is every server the bot is in for a site admin, and
// for anyone else the ones they own.
func (s *Server) guildsWhereMayName(session *WebSession) ([]Guild, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	admin, err := s.store.IsSiteAdmin(session.DiscordUserID)
	if err != nil {
		return nil, err
	}
	botGuilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return nil, fmt.Errorf("list bot guilds: %w", err)
	}
	var out []Guild
	for _, g := range botGuilds {
		if admin {
			out = append(out, g)
			continue
		}
		// Discord reports every permission for a server's owner, so anyone
		// without Administrator is not the owner and costs no lookup.
		if !session.IsMemberOf(g.ID) || session.GuildPermissions[g.ID]&permissionAdministrator == 0 {
			continue
		}
		ownerID, err := s.discord.GuildOwnerID(g.ID)
		if err != nil {
			return nil, fmt.Errorf("read the owner of %s: %w", g.Name, err)
		}
		if ownerID == session.DiscordUserID {
			out = append(out, g)
		}
	}
	return out, nil
}

// nameableBy is the servers whose people a viewer may name, and those people.
func (s *Server) nameableBy(session *WebSession) ([]Guild, []namedPerson, error) {
	guilds, err := s.guildsWhereMayName(session)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(guilds))
	for _, g := range guilds {
		ids = append(ids, g.ID)
	}
	people, err := s.store.PeopleOnListsIn(ids)
	return guilds, people, err
}

func (s *Server) handleWebNames(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	data := pageData{Title: "Names", Session: session, Notice: r.URL.Query().Get("notice"), MayName: true}
	guilds, people, err := s.nameableBy(session)
	if err == nil && len(guilds) == 0 {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		data.Error = "Could not load the names: " + err.Error()
	}
	data.NamePeople = people
	data.NameableGuilds = guilds
	s.render(w, "names.html", data)
}

// handleWebSetName saves one short name; an empty one removes it, so their
// Discord display name shows again.
func (s *Server) handleWebSetName(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	name := strings.TrimSpace(r.FormValue("readable_name"))
	guilds, people, err := s.nameableBy(session)
	if err != nil {
		http.Error(w, "could not check whether you may name them: "+err.Error(), http.StatusBadGateway)
		return
	}
	var person *namedPerson
	for i := range people {
		if people[i].DiscordUserID == userID {
			person = &people[i]
		}
	}
	// Someone on no list yet can be named too, from the search box: then the
	// form names the server, which must be one the viewer may edit every
	// event in, and Discord must list them as its member.
	if guildID := strings.TrimSpace(r.FormValue("guild_id")); person == nil && guildID != "" && s.discord != nil {
		for _, g := range guilds {
			if g.ID != guildID {
				continue
			}
			if name, err := s.discord.GuildMemberDisplayName(guildID, userID); err == nil {
				person = &namedPerson{DiscordUserID: userID, DisplayName: name}
			}
		}
	}
	if person == nil && len(guilds) == 0 {
		http.NotFound(w, r)
		return
	}
	if person == nil {
		http.Error(w, "that person is not in a server where you may name people", http.StatusForbidden)
		return
	}
	var notice string
	if name == "" {
		err = s.store.DeleteReadableName(userID)
		if errors.Is(err, ErrNotFound) {
			err = nil
		}
		notice = fmt.Sprintf("%s is shown by their Discord name again.", person.DisplayName)
	} else {
		_, err = s.store.SetReadableName(userID, name)
		notice = fmt.Sprintf("%s is now shown as %s.", person.DisplayName, name)
	}
	if err != nil {
		log.Printf("[discord-signup] set readable name for %s: %v", userID, err)
		http.Redirect(w, r, "/names?"+noticeQuery("Could not save it: "+err.Error()), http.StatusSeeOther)
		return
	}
	log.Printf("[discord-signup] readable name for %s set to %q by web:%s", userID, name, session.DiscordUserID)
	http.Redirect(w, r, "/names?"+noticeQuery(notice+" Tables update within a minute."), http.StatusSeeOther)
}

// handleWebNameSearch answers the names page's search box: members of one
// server the viewer may name people in, with the short name set for each.
func (s *Server) handleWebNameSearch(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	guildID := strings.TrimSpace(r.URL.Query().Get("guild_id"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	guilds, _, err := s.nameableBy(session)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not check your servers: " + err.Error()})
		return
	}
	allowed := false
	for _, g := range guilds {
		allowed = allowed || g.ID == guildID
	}
	if len(guilds) == 0 {
		http.NotFound(w, r)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you may not name people in that server"})
		return
	}
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"members": []any{}})
		return
	}
	matches, err := s.discord.SearchGuildMembers(guildID, query, memberSearchLimit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Discord member search failed: " + err.Error()})
		return
	}
	named, err := s.store.ReadableNames()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	readable := map[string]string{}
	for _, n := range named {
		readable[n.DiscordUserID] = n.ReadableName
	}
	type suggestion struct {
		MemberMatch
		ReadableName string `json:"readable_name,omitempty"`
	}
	out := make([]suggestion, 0, len(matches))
	for _, m := range matches {
		out = append(out, suggestion{MemberMatch: m, ReadableName: readable[m.UserID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}
