package discordsignup

import (
	"log"
	"regexp"
	"strings"
)

// Who did it, in words. The history records an actor as the service saw
// them: "web:<id>" for a web page, "discord:<id>" for a Discord form, a bare
// id for a button, or a word — user, interested, reaction, promotion, api —
// for everything that is not one person. The ids are shown as that person's
// name in the server, which is what an organiser recognises; the words are
// shown as they are.

var snowflake = regexp.MustCompile(`^\d{15,21}$`)

// actorUserID pulls the Discord user id out of an actor, and says which
// surface it came through. Empty when the actor is not a person.
func actorUserID(actor string) (userID, via string) {
	for _, prefix := range []string{"web:", "discord:"} {
		if rest, ok := strings.CutPrefix(actor, prefix); ok && snowflake.MatchString(rest) {
			return rest, strings.TrimSuffix(prefix, ":")
		}
	}
	if snowflake.MatchString(actor) {
		return actor, ""
	}
	return "", ""
}

// historyActorNames maps each actor in a history that is a person to their
// name in the event's server, as "Mal (web)". An actor Discord cannot name —
// someone who has left — is left out, and the page shows it raw.
func (s *Server) historyActorNames(guildID string, actors []string) map[string]string {
	names := map[string]string{}
	if s.discord == nil {
		return names
	}
	byID := map[string]string{}
	for _, actor := range actors {
		if _, done := names[actor]; done {
			continue
		}
		userID, via := actorUserID(actor)
		if userID == "" {
			continue
		}
		name, seen := byID[userID]
		if !seen {
			looked, err := s.discord.GuildMemberDisplayName(guildID, userID)
			if err != nil {
				log.Printf("[discord-signup] name history actor %s in %s: %v", userID, guildID, err)
			}
			name = looked
			byID[userID] = name
		}
		if name == "" {
			continue
		}
		if via != "" {
			name += " (" + via + ")"
		}
		names[actor] = name
	}
	return names
}
