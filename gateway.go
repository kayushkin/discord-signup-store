package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// GatewayListener turns Discord's Interested button into roster changes.
//
// This is the one part of the service that needs a persistent connection, and
// it is not optional for the feature: GUILD_SCHEDULED_EVENT_USER_ADD is
// delivered ONLY over the gateway. There is no webhook and no interaction for
// it, so an HTTP-only app cannot see an RSVP at all.
//
// Polling the subscriber list instead was considered and rejected. That
// endpoint returns users ascending by user_id — snowflake order, which is
// account-creation date — so two people who RSVP between two polls would be
// ordered by whose Discord account is older. For a waitlist that is not a
// cosmetic difference; it decides who gets the next free place.
//
// discordgo is used for the socket alone. Heartbeat timing, RESUME after a drop
// and session invalidation are fiddly and well solved there; the REST calls
// stay on this package's own client, which resolves its token from auth-store
// and retries a 401 by re-resolving.
type GatewayListener struct {
	session *discordgo.Session
	server  *Server

	mu      sync.Mutex
	started bool
}

// NewGatewayListener builds a listener. The token is resolved once here rather
// than lazily, because discordgo needs it to open the socket.
func NewGatewayListener(server *Server, resolveToken TokenResolver) (*GatewayListener, error) {
	token, err := resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve bot token for gateway: %w", err)
	}
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("build gateway session: %w", err)
	}
	// GUILD_SCHEDULED_EVENTS (1<<16) and nothing else. Not privileged, so no
	// approval and no verification. Deliberately narrow: this connection has no
	// business seeing messages, members or presences, and asking for intents it
	// does not need would be asking for data it should not hold.
	// Scheduled events for the Interested bridge, reactions for the ✅ join on
	// forum posts. Both standard intents — nothing privileged.
	session.Identify.Intents = discordgo.IntentGuildScheduledEvents |
		discordgo.IntentGuildMessageReactions

	listener := &GatewayListener{session: session, server: server}
	session.AddHandler(listener.onUserAdd)
	session.AddHandler(listener.onUserRemove)
	session.AddHandler(listener.onReactionAdd)
	session.AddHandler(listener.onReactionRemove)
	session.AddHandler(listener.onScheduledEventCreated)
	session.AddHandler(listener.onScheduledEventDeleted)
	session.AddHandler(listener.onScheduledEventChanged)
	session.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		log.Printf("[discord-signup] gateway ready as %s#%s, %d guild(s)",
			r.User.Username, r.User.Discriminator, len(r.Guilds))
	})
	session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		// Logged rather than acted on: discordgo reconnects and resumes on its
		// own. What matters is that a gap is visible, because events during one
		// are replayed on RESUME but lost if the session is invalidated.
		log.Print("[discord-signup] gateway disconnected; discordgo will resume")
	})
	return listener, nil
}

// Start opens the socket.
func (g *GatewayListener) Start() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return nil
	}
	if err := g.session.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	g.started = true
	return nil
}

// Close shuts the socket down.
func (g *GatewayListener) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		return nil
	}
	g.started = false
	return g.session.Close()
}

// onUserAdd handles someone pressing Interested.
func (g *GatewayListener) onUserAdd(_ *discordgo.Session, e *discordgo.GuildScheduledEventUserAdd) {
	ev, err := g.localEventFor(e.GuildScheduledEventID, e.GuildID)
	if err != nil {
		log.Printf("[discord-signup] interested: no local event for discord event %s: %v",
			e.GuildScheduledEventID, err)
		return
	}
	displayName := g.displayName(e.GuildID, e.UserID)

	result, err := g.server.store.MarkInterested(ev.ID, e.UserID, displayName)
	if err != nil {
		log.Printf("[discord-signup] mark interested event=%d user=%s: %v", ev.ID, e.UserID, err)
		return
	}
	log.Printf("[discord-signup] interested: event=%d user=%s -> %s", ev.ID, e.UserID, result.Outcome)

	if !result.RosterChanged() {
		return
	}
	// Reload so the counts on the refreshed message are the post-change ones.
	fresh, err := g.server.store.GetEvent(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] reload event %d: %v", ev.ID, err)
		return
	}
	// Roles AND the public signup message. The message refresh is the part that
	// makes Interested look like Join from the outside: without it the card
	// keeps the old count until somebody presses a button.
	g.server.syncAfterChange(fresh.ID, []stateChange{{UserID: e.UserID, State: result.Signup.State}})
	g.notifyInterestedOutcome(fresh, result)
}

// onUserRemove handles someone un-marking Interested.
func (g *GatewayListener) onUserRemove(_ *discordgo.Session, e *discordgo.GuildScheduledEventUserRemove) {
	ev, err := g.localEventFor(e.GuildScheduledEventID, e.GuildID)
	if err != nil {
		return
	}
	result, err := g.server.store.MarkNotInterested(ev.ID, e.UserID)
	if err != nil {
		log.Printf("[discord-signup] unmark interested event=%d user=%s: %v", ev.ID, e.UserID, err)
		return
	}
	log.Printf("[discord-signup] not interested: event=%d user=%s -> %s", ev.ID, e.UserID, result.Outcome)

	if !result.RosterChanged() {
		return
	}
	fresh, err := g.server.store.GetEvent(ev.ID)
	if err != nil {
		log.Printf("[discord-signup] reload event %d: %v", ev.ID, err)
		return
	}
	changes := []stateChange{{UserID: e.UserID, State: StateWithdrawn}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
		go g.server.notifyPromoted(fresh, result.Promoted)
	}
	g.server.syncAfterChange(fresh.ID, changes)
}

// onReactionAdd joins whoever clicked ✅ on a forum post.
//
// The reaction is the low-effort door: clickable from the forum's list view
// without opening the post, and — once the channel denies ADD_REACTIONS — the
// only reaction that exists to click. Any other emoji, any other message, any
// other clicker (including the bot seeding its own ✅) is ignored.
func (g *GatewayListener) onReactionAdd(_ *discordgo.Session, e *discordgo.MessageReactionAdd) {
	if e.Emoji.Name != joinReactionEmoji || e.UserID == g.server.applicationUserID() {
		return
	}
	ev, err := g.server.store.EventByForumPostID(e.MessageID)
	if err != nil {
		return // a reaction somewhere that is not an event post
	}
	displayName := g.displayName(e.GuildID, e.UserID)
	result, err := g.server.store.Join(ev.ID, e.UserID, displayName, JoinedViaReaction)
	if err != nil {
		if !errors.Is(err, ErrEventNotOpen) {
			log.Printf("[discord-signup] reaction join event=%d user=%s: %v", ev.ID, e.UserID, err)
		}
		return
	}
	log.Printf("[discord-signup] reaction join: event=%d user=%s -> %s", ev.ID, e.UserID, result.Signup.State)
	if result.AlreadySignedUp {
		return
	}
	fresh, err := g.server.store.GetEvent(ev.ID)
	if err != nil {
		return
	}
	g.server.syncAfterChange(fresh.ID, []stateChange{{UserID: e.UserID, State: result.Signup.State}})
	// A reaction carries no interaction token, so a waitlisted clicker can only
	// be told by DM — same shape as Interested, same fallback to reading the
	// card.
	if result.Signup.State == StateWaitlisted {
		go func() {
			body := fmt.Sprintf("**%s** is full, so your ✅ put you on the waitlist at number %d. "+
				"If someone drops out you move up automatically and I will message you."+
				"\n\nRemoving your ✅ takes you off.", fresh.Name, result.Signup.WaitlistPlace)
			if err := g.server.discord.SendDirectMessage(e.UserID, body); err != nil &&
				!errors.Is(err, ErrCannotMessageUser) {
				log.Printf("[discord-signup] dm reaction waitlist user=%s: %v", e.UserID, err)
			}
		}()
	}
}

// onReactionRemove is the other half: taking your ✅ off is leaving, however
// you originally joined — the rule the Interested bridge already follows.
//
// The bot itself removes reactions to reconcile state after a button Leave;
// those removals arrive here carrying the REACTION OWNER's id, land on someone
// already withdrawn, and fall through Leave's not-found path as a no-op — which
// is what breaks the loop.
func (g *GatewayListener) onReactionRemove(_ *discordgo.Session, e *discordgo.MessageReactionRemove) {
	if e.Emoji.Name != joinReactionEmoji || e.UserID == g.server.applicationUserID() {
		return
	}
	ev, err := g.server.store.EventByForumPostID(e.MessageID)
	if err != nil {
		return
	}
	result, err := g.server.store.Leave(ev.ID, e.UserID, ActorReaction)
	if err != nil {
		return // not on the roster; nothing to undo
	}
	log.Printf("[discord-signup] reaction leave: event=%d user=%s", ev.ID, e.UserID)
	fresh, err := g.server.store.GetEvent(ev.ID)
	if err != nil {
		return
	}
	changes := []stateChange{{UserID: e.UserID, State: StateWithdrawn}}
	if result.Promoted != nil {
		changes = append(changes, stateChange{UserID: result.Promoted.DiscordUserID, State: StateAttending})
		go g.server.notifyPromoted(fresh, result.Promoted)
	}
	g.server.syncAfterChange(fresh.ID, changes)
}

// onScheduledEventCreated imports a brand new native event and posts its card
// straight away.
//
// The ten-minute poll would get there eventually, but "eventually" is the wrong
// answer for something a person just created and is watching for. Someone can
// make an event in Discord and press Interested on it within seconds, and that
// first RSVP has nowhere to land until the import has happened.
func (g *GatewayListener) onScheduledEventCreated(_ *discordgo.Session, e *discordgo.GuildScheduledEventCreate) {
	if e.GuildScheduledEvent == nil {
		return
	}
	result, err := g.server.SyncScheduledEvents(e.GuildID, g.server.BoardChannelID())
	if err != nil {
		log.Printf("[discord-signup] import new discord event %s: %v", e.ID, err)
		return
	}
	log.Printf("[discord-signup] discord event %q created: %d imported, %d cards posted",
		e.Name, result.Imported, result.Posted)
}

// onScheduledEventDeleted cancels the local event the moment its native one is
// deleted — deleting is how a person cancels in Discord's own UI, and this is
// the event the poll cannot see (absence from the LIST is ambiguous; completed
// events drop out too). The poll's direct-GET reconciliation stays as the
// backstop for anything this misses while the socket is down.
func (g *GatewayListener) onScheduledEventDeleted(_ *discordgo.Session, e *discordgo.GuildScheduledEventDelete) {
	ev, err := g.server.store.EventByDiscordScheduledEventID(e.ID)
	if err != nil {
		return // not one of ours, or already gone
	}
	if IsArchived(ev.Status) {
		return // already settled; also breaks the loop when WE deleted it
	}
	if err := g.server.cancelEventEverywhere(ev, "its Discord event was deleted"); err != nil {
		log.Printf("[discord-signup] cancel after native delete %s: %v", e.ID, err)
	}
}

// onScheduledEventChanged keeps the local copy fresh when someone edits a
// native event, so the board does not show a stale time until the next poll.
func (g *GatewayListener) onScheduledEventChanged(_ *discordgo.Session, e *discordgo.GuildScheduledEventUpdate) {
	if e.GuildScheduledEvent == nil {
		return
	}
	if _, err := g.server.SyncScheduledEvents(e.GuildID, g.server.BoardChannelID()); err != nil {
		log.Printf("[discord-signup] resync after event update in %s: %v", e.GuildID, err)
	}
}

// localEventFor resolves a Discord scheduled event id to a local roster,
// importing it first if this is an event nobody has synced yet.
//
// The retry matters: someone can create an event in Discord and press
// Interested on it within seconds, long before the ten-minute sync job runs.
// Without importing on miss, that first RSVP would be dropped silently.
func (g *GatewayListener) localEventFor(discordEventID, guildID string) (*Event, error) {
	ev, err := g.server.store.EventByDiscordScheduledEventID(discordEventID)
	if err == nil {
		return ev, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := g.server.SyncScheduledEvents(guildID, g.server.BoardChannelID()); err != nil {
		return nil, fmt.Errorf("sync on miss: %w", err)
	}
	return g.server.store.EventByDiscordScheduledEventID(discordEventID)
}

// displayName looks up a readable name for the roster. Best effort: the gateway
// payload carries only a user id, and a blank name is far better than refusing
// to record the signup.
func (g *GatewayListener) displayName(guildID, userID string) string {
	if g.server.discord == nil {
		return ""
	}
	member, err := g.session.GuildMember(guildID, userID)
	if err != nil || member == nil {
		return ""
	}
	if member.Nick != "" {
		return member.Nick
	}
	if member.User == nil {
		return ""
	}
	if member.User.GlobalName != "" {
		return member.User.GlobalName
	}
	return member.User.Username
}

// notifyInterestedOutcome tells someone what their Interested press actually
// did.
//
// This is where Interested is unavoidably worse than the Join button, and the
// message says so rather than pretending otherwise. A button press carries an
// interaction token, so the answer is private, instant and cannot fail. An RSVP
// carries nothing, so the only channel is a DM — which bounces with error 50007
// for anyone who has direct messages from server members turned off. Somebody
// waitlisted who never learns it will turn up expecting a place.
func (g *GatewayListener) notifyInterestedOutcome(ev *Event, result *InterestResult) {
	if g.server.discord == nil || result.Signup == nil {
		return
	}
	var body string
	switch result.Outcome {
	case OutcomeJoined:
		body = fmt.Sprintf("You're on the list for **%s**.", ev.Name)
		if ev.Capacity > 0 {
			body += fmt.Sprintf(" %d/%d places taken.", ev.AttendingCount, ev.Capacity)
		}
	case OutcomeWaitlisted:
		body = fmt.Sprintf("**%s** is full, so you are on the waitlist at number %d. "+
			"If someone drops out you move up automatically and I will message you.",
			ev.Name, result.Signup.WaitlistPlace)
	default:
		return
	}
	body += "\n\nMarking yourself Interested on Discord signed you up. " +
		"To leave, use the Leave button on the signup message — un-marking Interested works too."

	go func() {
		err := g.server.discord.SendDirectMessage(result.Signup.DiscordUserID, body)
		if err == nil {
			return
		}
		if errors.Is(err, ErrCannotMessageUser) {
			// Deliberately not escalated to a channel ping. Being waitlisted is
			// visible on the signup card, which is public, so nobody is left
			// without a way to find out — unlike a promotion, which is news
			// that arrives at no particular moment and does get a fallback.
			log.Printf("[discord-signup] user=%s has DMs closed; they will have to read "+
				"their place off the signup card", result.Signup.DiscordUserID)
			return
		}
		log.Printf("[discord-signup] dm interested user=%s: %v", result.Signup.DiscordUserID, err)
	}()
}

// gatewayRetryDelay is how long to wait before retrying a failed first connect.
const gatewayRetryDelay = 30 * time.Second

// StartGatewayWithRetry opens the socket, retrying forever on failure.
//
// A failure here must not stop the service: the buttons, the roster, the web
// page and the interaction endpoint all work without a gateway. Only Interested
// stops feeding the roster, and that degradation is worth logging loudly and
// living with rather than refusing to boot over.
func StartGatewayWithRetry(server *Server, resolveToken TokenResolver) {
	for {
		listener, err := NewGatewayListener(server, resolveToken)
		if err == nil {
			if err = listener.Start(); err == nil {
				return
			}
		}
		log.Printf("[discord-signup] gateway connect failed, retrying in %s: %v",
			gatewayRetryDelay, err)
		time.Sleep(gatewayRetryDelay)
	}
}
