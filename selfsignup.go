package discordsignup

import "log"

// A person joining or leaving an event themselves — the Join and Leave buttons
// on Discord, the My events dashboard and the web home page all come here, so
// that each does the same projection afterwards: roles, the public copies,
// and a message to whoever a leave moved up.

// joinAsThemselves puts someone on an event's roster, then syncs Discord in
// the background.
func (s *Server) joinAsThemselves(eventID int64, userID, displayName, via string) (*JoinResult, error) {
	result, err := s.store.Join(eventID, userID, displayName, via)
	if err != nil {
		return nil, err
	}
	s.inBackground(func() { s.syncAfterChange(eventID, []stateChange{{UserID: userID, State: result.Signup.State}}) })
	return result, nil
}

// leaveAsThemselves takes someone off an event's roster, syncs Discord in the
// background and tells whoever took their place.
func (s *Server) leaveAsThemselves(eventID int64, userID string) (*LeaveResult, error) {
	result, err := s.store.Leave(eventID, userID, ActorUser)
	if err != nil {
		return nil, err
	}
	changes := []stateChange{{UserID: userID, State: StateWithdrawn}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
	}
	// Synced by id, so a failed reload below does not cost the sync: the
	// roster changed whether or not this process can read it back.
	s.inBackground(func() { s.syncAfterChange(eventID, changes) })
	if result.Promoted != nil {
		ev, err := s.store.GetEvent(eventID)
		if err != nil {
			log.Printf("[discord-signup] reload event=%d to tell %s they moved up: %v", eventID, result.Promoted.DiscordUserID, err)
		} else {
			s.inBackground(func() { s.notifyPromoted(ev, result.Promoted) })
		}
	}
	return result, nil
}
