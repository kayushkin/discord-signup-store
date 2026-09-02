package discordsignup

import "log"

// createEventAndJoinOrganiser creates an event and puts whoever made it on its
// roster.
//
// Somebody who fills in the form is going to their own event. Making them press
// Join afterwards is a step that exists only because the software did not think
// of it, and an organiser who forgets it leaves a card reading "0 places taken"
// under an event they are running.
//
// The join is not allowed to fail the create. If it fails, the event still
// exists exactly as it was typed and its roster is one person short — visible
// on the card, fixed with one press — whereas returning an error here would
// throw away everything they entered.
//
// Only the two surfaces where a person filled in THIS system's form call it.
// An event imported from a native Discord one deliberately does not: its
// creator made a Discord event, not a roster, and putting them on a signup list
// they never filled in is a different act. The machine API does not either —
// `created_by` there is whatever a script sent, and a script is not going.
func (s *Server) createEventAndJoinOrganiser(ev Event, displayName string) (*Event, error) {
	created, err := s.store.CreateEvent(ev)
	if err != nil {
		return nil, err
	}
	if created.CreatedBy == "" {
		return created, nil
	}
	if _, err := s.store.Join(created.ID, created.CreatedBy, displayName, JoinedViaOrganiser); err != nil {
		log.Printf("[discord-signup] put organiser %s on their own new event %d: %v",
			created.CreatedBy, created.ID, err)
		return created, nil
	}
	// Re-read. AttendingCount is filled by the read path, so the struct
	// CreateEvent handed back still says nobody is going — and the caller's
	// very next move is to render a card from it.
	return s.store.GetEvent(created.ID)
}
