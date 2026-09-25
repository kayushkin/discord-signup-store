package discordsignup

import (
	"log"
	"regexp"
	"strings"
)

// Who did it, in words. The history records an actor as the service saw
// them: "web:<id>" for a web page, "discord:<id>" for a Discord form, a bare
// id for a button, or a word — user, interested, reaction, promotion, api,
// discord-event-screen, discord-event-sync —
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

// historyActorNames maps each actor in a history that is a person to who
// they are: their short name, if one is set, and their name in the event's
// server. An actor Discord cannot name and nobody gave a short name —
// someone who has left — is left out, and the page shows it raw.
func (s *Server) historyActorNames(guildID string, actors []string) map[string]actorName {
	names := map[string]actorName{}
	readable := map[string]string{}
	if set, err := s.store.ReadableNames(); err != nil {
		log.Printf("[discord-signup] read short names for a history: %v", err)
	} else {
		for _, n := range set {
			readable[n.DiscordUserID] = n.ReadableName
		}
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
		display, seen := byID[userID]
		if !seen && s.discord != nil {
			looked, err := s.discord.GuildMemberDisplayName(guildID, userID)
			if err != nil {
				log.Printf("[discord-signup] name history actor %s in %s: %v", userID, guildID, err)
			}
			display = looked
			byID[userID] = display
		}
		if display == "" && readable[userID] == "" {
			continue
		}
		names[actor] = actorName{ReadableName: readable[userID], DisplayName: display, UserID: userID, Via: via}
	}
	return names
}
