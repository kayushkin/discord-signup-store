package discordsignup

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

// An event's ref is its id written so another store can cite it and a chat
// can draw it as a chip: discord_event_000126. kanban-store's entity-type
// registry carries the pattern, and dash resolves a ref through
// GET /api/discord-events/{ref}. The ref is the integer id in another form,
// never a second id: it is made from the id on every read and stored nowhere.
const eventRefPrefix = "discord_event_"

var eventRefPattern = regexp.MustCompile(`^discord_event_(\d{6,})$`)

// EventRef writes an event id as its ref.
func EventRef(id int64) string {
	return fmt.Sprintf("%s%06d", eventRefPrefix, id)
}

// ParseEventRef reads the id back out of a ref. Anything that is not a ref,
// including a bare number, is an error.
func ParseEventRef(ref string) (int64, error) {
	match := eventRefPattern.FindStringSubmatch(ref)
	if match == nil {
		return 0, fmt.Errorf("%q is not an event ref (%s000001)", ref, eventRefPrefix)
	}
	return strconv.ParseInt(match[1], 10, 64)
}

// handleGetEventByRef answers GET /api/discord-events/{ref} with the same
// body as GET /api/events/{id}.
func (s *Server) handleGetEventByRef(w http.ResponseWriter, r *http.Request) {
	id, err := ParseEventRef(r.PathValue("ref"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	event, err := s.store.GetEvent(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}
