package discordsignup

// The closed vocabularies this service accepts. They live here as sets rather
// than as if-chains at each call site so that adding a state means editing one
// line, and so a typo in a request body is a 400 naming the allowed values
// rather than a row with an unreachable state in it.

// Signup states. A row is in exactly one of these at any moment.
const (
	// StateAttending means the person holds a place.
	StateAttending = "attending"
	// StateWaitlisted means they signed up after the cap was reached and hold
	// a position in line. Promotion is automatic when a place frees up.
	StateWaitlisted = "waitlisted"
	// StateWithdrawn means they left. The row stays so the transition history
	// stays joinable and so a re-join can be told from a first join.
	StateWithdrawn = "withdrawn"
)

var validStates = map[string]bool{
	StateAttending:  true,
	StateWaitlisted: true,
	StateWithdrawn:  true,
}

// Transition actions, written to the append-only log.
const (
	ActionJoined     = "joined"     // signed up and got a place
	ActionWaitlisted = "waitlisted" // signed up and went into line
	ActionWithdrew   = "withdrew"   // left, by button or by override
	ActionPromoted   = "promoted"   // moved off the waitlist into a freed place
	ActionRejoined   = "rejoined"   // signed up again after withdrawing
)

var validActions = map[string]bool{
	ActionJoined:     true,
	ActionWaitlisted: true,
	ActionWithdrew:   true,
	ActionPromoted:   true,
	ActionRejoined:   true,
}

// Event lifecycle.
const (
	// StatusOpen accepts signups.
	StatusOpen = "open"
	// StatusClosed refuses new signups but keeps the roster readable. Use this
	// rather than deleting: the roster is the record of who turned up.
	StatusClosed = "closed"
	// StatusCompleted means it already happened.
	//
	// Deliberately not folded into StatusClosed. Closed means signups are shut
	// on an event that has not happened yet — a thing you still want on screen,
	// because people ask about it. Completed means the date has passed. Using
	// one word for both would either bury an upcoming event in an archive or
	// leave last month's in the live list.
	StatusCompleted = "completed"
	// StatusCancelled means the event is not happening.
	StatusCancelled = "cancelled"
)

var validStatuses = map[string]bool{
	StatusOpen:      true,
	StatusClosed:    true,
	StatusCompleted: true,
	StatusCancelled: true,
}

// archivedStatuses are the ones the web page collapses out of the main list.
//
// Closed is deliberately NOT among them, and that is a confirmed decision
// rather than an oversight. Cancelled and completed both mean the event is
// over. Closed means signups are shut on one that has not happened yet — the
// date is still coming, people still ask about it, and having it disappear into
// an archive would be a worse surprise than seeing it with a pill that says the
// door is shut. Pinned by TestClosedEventsStayInTheLiveList.
var archivedStatuses = map[string]bool{
	StatusCompleted: true,
	StatusCancelled: true,
}

// IsArchived reports whether an event belongs in the collapsed archive.
func IsArchived(status string) bool { return archivedStatuses[status] }

// ActorUser is the actor recorded when the transition came from someone
// pressing a button themselves.
const ActorUser = "user"

// ActorPromotion is the actor recorded when this service moved someone off the
// waitlist on its own. Distinguishing it from ActorUser is what lets the log
// answer "did they choose this or did we do it to them".
const ActorPromotion = "promotion"

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Insertion order from a map is random, and an error message that reorders
	// itself between calls is a bad error message.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ValidStates returns the accepted signup states, sorted, for error messages.
func ValidStates() []string { return sortedKeys(validStates) }

// ValidActions returns the accepted transition actions, sorted.
func ValidActions() []string { return sortedKeys(validActions) }

// ValidStatuses returns the accepted event statuses, sorted.
func ValidStatuses() []string { return sortedKeys(validStatuses) }

// Where an event came from.
const (
	// OriginLocal means it was created here — on the web page, by a slash
	// command, or through the API.
	OriginLocal = "local"
	// OriginDiscord means it was imported from a native Discord scheduled
	// event. Not derivable from DiscordScheduledEventID being set: a locally
	// created event that gets published to Discord also carries one. This
	// records who owns the thing, not whether a link exists.
	OriginDiscord = "discord"
)

var validOrigins = map[string]bool{
	OriginLocal:   true,
	OriginDiscord: true,
}

// ValidOrigins returns the accepted origins, sorted.
func ValidOrigins() []string { return sortedKeys(validOrigins) }

// How someone arrived on a roster.
const (
	// JoinedViaButton is the Join button on the signup message.
	JoinedViaButton = "button"
	// JoinedViaInterested is Discord's own Interested button on a linked
	// scheduled event. The weakest signal of the three: it means "notify me",
	// not "hold me a place", and it is the only one that can be withdrawn on
	// Discord's side without this service being able to do anything about it.
	JoinedViaInterested = "interested"
	// JoinedViaOperator is a person added through the web page or the API.
	JoinedViaOperator = "operator"
	// JoinedViaReaction is the ✅ on a forum post — clickable from the forum's
	// list view without opening the post, which is the point of it.
	JoinedViaReaction = "reaction"
)

var validJoinedVia = map[string]bool{
	JoinedViaButton:     true,
	JoinedViaInterested: true,
	JoinedViaOperator:   true,
	JoinedViaReaction:   true,
}

// ValidJoinedVia returns the accepted arrival routes, sorted.
func ValidJoinedVia() []string { return sortedKeys(validJoinedVia) }

// ActorReaction is the actor recorded when a change came from the ✅ reaction
// on a forum post.
const ActorReaction = "reaction"

// ActorInterested is the actor recorded when a change came from Discord's own
// Interested button rather than from this service's Join button. Kept distinct
// from ActorUser so the history can answer which surface someone used — the two
// behave differently and a support question will eventually turn on it.
const ActorInterested = "interested"
