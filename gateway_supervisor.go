package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The gateway supervisor keeps one gateway socket open for the life of the
// process, and says on /healthz whether it has one.
//
// It exists because discordgo's own reconnect cannot be trusted with that job.
// Measured on this host: on 2026-09-15 14:17 the running process logged its
// Disconnect after a run of "websocket: bad handshake" errors, and then held no
// TCP connection at all for five days while logging nothing. Discord's
// Interested button fed no roster for that whole time and nothing said so.
// discordgo's reconnect() is a goroutine nobody can see into or cancel: it logs
// its successes at a level that is off by default, it can be entered twice, and
// Open() reads Discord's reply while holding the session lock, so a reply that
// makes the library call Close() from inside Open() stops that session for
// good.
//
// So the library's reconnect is switched off (ShouldReconnectOnError = false)
// and the reopening is done here, where three things hold that did not before:
//
//   - every death of the socket is followed by an Open() that this code made
//     and whose result this code logs;
//   - an Open() that does not return, or a socket that says it is connected
//     while no heartbeat has been acknowledged, is given up on and replaced by a
//     new session rather than waited on forever;
//   - the state is readable from outside the process.
//
// Reopening the SAME session is what keeps RESUME: discordgo holds the session
// id and the sequence number, and Open() on a session that has them sends Op 6
// rather than Op 2, so events that arrived during the gap are replayed. Only an
// abandoned session loses that, and the log says so when it happens.

// GatewayState is what the supervisor last knew about the socket.
type GatewayState string

const (
	// GatewayDisabled: no supervisor is running (DISCORD_GATEWAY_DISABLED, or
	// Discord is not wired at all).
	GatewayDisabled GatewayState = "disabled"
	// GatewayConnecting: an Open() is in flight.
	GatewayConnecting GatewayState = "connecting"
	// GatewayConnected: the last Open() succeeded and no Disconnect has been
	// seen since.
	GatewayConnected GatewayState = "connected"
	// GatewayDown: the socket is closed and the supervisor is waiting to retry.
	GatewayDown GatewayState = "down"
)

// GatewayStatus is the supervisor's state as /healthz reports it.
type GatewayStatus struct {
	State GatewayState `json:"state"`
	// Since is when State last changed.
	Since time.Time `json:"since"`
	// LastConnectedAt is the last time an Open() succeeded; zero if none has.
	LastConnectedAt time.Time `json:"last_connected_at"`
	// LastError is why the socket is down; empty while connected.
	LastError string `json:"last_error,omitempty"`
	// Opens counts successful Open() calls since the process started, so a
	// socket that flaps is visible as a number that keeps rising.
	Opens int `json:"opens"`
	// AbandonedSessions counts sessions given up on as stuck. Each one lost
	// its RESUME, so events during that gap were not replayed.
	AbandonedSessions int `json:"abandoned_sessions"`
}

// gatewaySocket is the part of a gateway session the supervisor drives. The
// real one is a discordgo session; tests use a fake.
type gatewaySocket interface {
	Open() error
	Close() error
	// LastHeartbeatAck may block: discordgo guards the value with the session
	// lock, and a stuck Open() holds that lock forever.
	LastHeartbeatAck() time.Time
}

// errGatewaySocketAlreadyOpen is what Open() returns when the socket is open
// already. It is not a failure: a second Disconnect for one death (discordgo's
// listen and heartbeat goroutines can each close the socket) sends the
// supervisor round again after it has already reopened.
var errGatewaySocketAlreadyOpen = discordgo.ErrWSAlreadyOpen

// gatewaySupervisorTimings are the supervisor's waits, gathered so a test can
// shrink them.
type gatewaySupervisorTimings struct {
	// firstRetryDelay is the wait after the first failed Open(); it doubles on
	// each further failure up to maxRetryDelay.
	firstRetryDelay time.Duration
	maxRetryDelay   time.Duration
	// openTimeout is how long an Open() may take before the session is given
	// up on as stuck.
	openTimeout time.Duration
	// livenessCheckInterval is how often a connected socket's heartbeat is read.
	livenessCheckInterval time.Duration
	// heartbeatAckReadTimeout is how long reading the last heartbeat ack may
	// block before the session is given up on as stuck.
	heartbeatAckReadTimeout time.Duration
	// maxHeartbeatAckAge is how old the last heartbeat ack may be on a socket
	// that claims to be connected. Discord's heartbeat interval is about 41s
	// and discordgo closes the socket itself after five missed acks (~3.5
	// minutes), so this only trips when that goroutine is gone too.
	maxHeartbeatAckAge time.Duration
}

var productionGatewaySupervisorTimings = gatewaySupervisorTimings{
	firstRetryDelay:         5 * time.Second,
	maxRetryDelay:           5 * time.Minute,
	openTimeout:             2 * time.Minute,
	livenessCheckInterval:   time.Minute,
	heartbeatAckReadTimeout: 10 * time.Second,
	maxHeartbeatAckAge:      10 * time.Minute,
}

// GatewaySupervisor owns the gateway socket. Build one with
// NewGatewaySupervisor and call Run in a goroutine.
type GatewaySupervisor struct {
	// buildSocket makes a fresh session. notifyDisconnected is what the session
	// must call when its socket closes.
	buildSocket func(notifyDisconnected func()) (gatewaySocket, error)
	timings     gatewaySupervisorTimings
	now         func() time.Time

	mu     sync.Mutex
	status GatewayStatus
}

// NewGatewaySupervisor builds a supervisor whose sessions feed server.
func NewGatewaySupervisor(server *Server, resolveToken TokenResolver) *GatewaySupervisor {
	return newGatewaySupervisor(func(notifyDisconnected func()) (gatewaySocket, error) {
		listener, err := NewGatewayListener(server, resolveToken, notifyDisconnected)
		if err != nil {
			return nil, err
		}
		return discordgoGatewaySocket{session: listener.session}, nil
	}, productionGatewaySupervisorTimings)
}

func newGatewaySupervisor(buildSocket func(notifyDisconnected func()) (gatewaySocket, error),
	timings gatewaySupervisorTimings) *GatewaySupervisor {
	supervisor := &GatewaySupervisor{buildSocket: buildSocket, timings: timings, now: time.Now}
	supervisor.status = GatewayStatus{State: GatewayDown, Since: supervisor.now()}
	return supervisor
}

// Status is the supervisor's current state.
func (g *GatewaySupervisor) Status() GatewayStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

func (g *GatewaySupervisor) setState(state GatewayState, lastError string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.status.State != state {
		g.status.State = state
		g.status.Since = g.now()
	}
	g.status.LastError = lastError
	if state == GatewayConnected {
		g.status.LastConnectedAt = g.now()
	}
}

// Run keeps a gateway socket open until stop is closed. A failure here must not
// stop the service: the buttons, the roster, the web page and the interaction
// endpoint all work without a gateway. Only Interested stops feeding the roster,
// and that is worth logging loudly and showing on /healthz rather than refusing
// to boot over.
func (g *GatewaySupervisor) Run(stop <-chan struct{}) {
	retryDelay := g.timings.firstRetryDelay
	for {
		disconnected := make(chan struct{}, 1)
		socket, err := g.buildSocket(func() {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		})
		if err != nil {
			g.setState(GatewayDown, err.Error())
			log.Printf("[discord-signup] gateway session could not be built, retrying in %s: %v", retryDelay, err)
			if !sleepUnlessStopped(retryDelay, stop) {
				return
			}
			retryDelay = nextGatewayRetryDelay(retryDelay, g.timings.maxRetryDelay)
			continue
		}
		retryDelay = g.timings.firstRetryDelay

		abandonReason := g.keepSocketOpen(socket, disconnected, stop)
		if abandonReason == "" {
			// Stopped. Close in the foreground: the caller is shutting down and
			// wants the close frame sent.
			if err := socket.Close(); err != nil {
				log.Printf("[discord-signup] gateway close: %v", err)
			}
			return
		}

		g.mu.Lock()
		g.status.AbandonedSessions++
		g.mu.Unlock()
		g.setState(GatewayDown, abandonReason)
		log.Printf("[discord-signup] gateway session ABANDONED: %s. A new session follows; it cannot RESUME, "+
			"so Interested presses made during this gap were not replayed", abandonReason)
		// Closed in the background because a stuck session's Close() blocks on
		// the same lock its Open() holds. If it is not stuck this sends the
		// close frame, so Discord is not left with two sockets on one token.
		go func() {
			if err := socket.Close(); err != nil {
				log.Printf("[discord-signup] closing the abandoned gateway session: %v", err)
			}
		}()
	}
}

// keepSocketOpen opens socket and reopens it each time it closes. It returns ""
// when stop is closed, and otherwise the reason the session had to be given up.
func (g *GatewaySupervisor) keepSocketOpen(socket gatewaySocket, disconnected <-chan struct{}, stop <-chan struct{}) string {
	retryDelay := g.timings.firstRetryDelay
	for {
		g.setState(GatewayConnecting, "")
		err, returned := callWithin(g.timings.openTimeout, socket.Open)
		if !returned {
			return fmt.Sprintf("Open() had not returned after %s", g.timings.openTimeout)
		}
		if err != nil && !errors.Is(err, errGatewaySocketAlreadyOpen) {
			g.setState(GatewayDown, err.Error())
			log.Printf("[discord-signup] gateway open failed, retrying in %s: %v", retryDelay, err)
			if !sleepUnlessStopped(retryDelay, stop) {
				return ""
			}
			retryDelay = nextGatewayRetryDelay(retryDelay, g.timings.maxRetryDelay)
			continue
		}
		retryDelay = g.timings.firstRetryDelay
		g.setState(GatewayConnected, "")
		if err == nil {
			g.mu.Lock()
			g.status.Opens++
			opens := g.status.Opens
			g.mu.Unlock()
			log.Printf("[discord-signup] gateway connected (open %d of this process)", opens)
		}

		if reason := g.waitForDisconnect(socket, disconnected, stop); reason != gatewaySocketClosed {
			return reason
		}
		g.setState(GatewayDown, "the socket closed")
		log.Print("[discord-signup] gateway disconnected; reopening")
	}
}

// gatewaySocketClosed is waitForDisconnect's answer when the socket closed in
// the ordinary way and the same session can be reopened.
const gatewaySocketClosed = "closed"

// waitForDisconnect blocks while the socket is healthy. It returns
// gatewaySocketClosed when the socket closed, "" when stop is closed, and
// otherwise the reason the session is stuck.
func (g *GatewaySupervisor) waitForDisconnect(socket gatewaySocket, disconnected <-chan struct{}, stop <-chan struct{}) string {
	ticker := time.NewTicker(g.timings.livenessCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return ""
		case <-disconnected:
			return gatewaySocketClosed
		case <-ticker.C:
			var lastAck time.Time
			_, returned := callWithin(g.timings.heartbeatAckReadTimeout, func() error {
				lastAck = socket.LastHeartbeatAck()
				return nil
			})
			if !returned {
				return fmt.Sprintf("the session lock was still held after %s", g.timings.heartbeatAckReadTimeout)
			}
			if age := g.now().Sub(lastAck); age > g.timings.maxHeartbeatAckAge {
				return fmt.Sprintf("the socket reads as connected but Discord last acknowledged a heartbeat %s ago",
					age.Round(time.Second))
			}
		}
	}
}

// callWithin runs call and reports whether it returned within limit. A call
// that does not return keeps its goroutine; that is the cost of calling into a
// library that offers no context and no deadline.
func callWithin(limit time.Duration, call func() error) (err error, returned bool) {
	done := make(chan error, 1)
	go func() { done <- call() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, true
	case <-timer.C:
		return nil, false
	}
}

// sleepUnlessStopped waits for delay and reports false if stop closed first.
func sleepUnlessStopped(delay time.Duration, stop <-chan struct{}) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func nextGatewayRetryDelay(current, max time.Duration) time.Duration {
	if next := current * 2; next < max {
		return next
	}
	return max
}

// discordgoGatewaySocket is a discordgo session seen as a gatewaySocket.
type discordgoGatewaySocket struct {
	session *discordgo.Session
}

func (d discordgoGatewaySocket) Open() error  { return d.session.Open() }
func (d discordgoGatewaySocket) Close() error { return d.session.Close() }

func (d discordgoGatewaySocket) LastHeartbeatAck() time.Time {
	d.session.RLock()
	defer d.session.RUnlock()
	return d.session.LastHeartbeatAck
}
