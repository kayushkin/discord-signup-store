package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// The names page: one place to set the short name each person is shown by on
// Discord. It lists everyone on any list — going, maybe or waitlisted — in the
// servers where the viewer may edit every event, and only lets them name those
// people: naming someone changes how every table shows them, which is an
// organiser's call, not any member's.

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

// nameableBy is the servers whose people a viewer may name, and those people.
func (s *Server) nameableBy(session *WebSession) ([]Guild, []namedPerson, error) {
	guilds, err := s.guildsWhereMayEditAll(session)
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
	data := pageData{Title: "Names", Session: session, Notice: r.URL.Query().Get("notice")}
	guilds, people, err := s.nameableBy(session)
	switch {
	case err != nil:
		data.Error = "Could not load the names: " + err.Error()
	case len(guilds) == 0:
		data.Error = "Names can be set by whoever may edit every event in a server, and you may not in any this bot is in."
	}
	data.NamePeople = people
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
	_, people, err := s.nameableBy(session)
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
	if person == nil {
		http.Error(w, "that person is not on a list in a server where you may edit every event", http.StatusForbidden)
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
