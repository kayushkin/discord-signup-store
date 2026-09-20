package discordsignup

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeGatewaySocket is a gateway session whose every answer the test decides.
type fakeGatewaySocket struct {
	mu                 sync.Mutex
	openResults        []error // consumed one per Open(); nil once exhausted
	openCalls          int
	closeCalls         int
	openBlocksForever  bool
	ackBlocksForever   bool
	lastHeartbeatAck   func() time.Time
	notifyDisconnected func()
}

func (f *fakeGatewaySocket) Open() error {
	f.mu.Lock()
	f.openCalls++
	blocks := f.openBlocksForever
	var result error
	if len(f.openResults) > 0 {
		result, f.openResults = f.openResults[0], f.openResults[1:]
	}
	f.mu.Unlock()
	if blocks {
		select {}
	}
	return result
}

func (f *fakeGatewaySocket) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeGatewaySocket) LastHeartbeatAck() time.Time {
	f.mu.Lock()
	blocks, read := f.ackBlocksForever, f.lastHeartbeatAck
	f.mu.Unlock()
	if blocks {
		select {}
	}
	if read != nil {
		return read()
	}
	return time.Now()
}

func (f *fakeGatewaySocket) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openCalls
}

func (f *fakeGatewaySocket) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

var testGatewaySupervisorTimings = gatewaySupervisorTimings{
	firstRetryDelay:         2 * time.Millisecond,
	maxRetryDelay:           8 * time.Millisecond,
	openTimeout:             50 * time.Millisecond,
	livenessCheckInterval:   5 * time.Millisecond,
	heartbeatAckReadTimeout: 50 * time.Millisecond,
	maxHeartbeatAckAge:      time.Hour,
}

// superviseFakeSockets runs a supervisor that hands out sockets in order and
// fails the test if it asks for more than it was given.
func superviseFakeSockets(t *testing.T, timings gatewaySupervisorTimings, sockets ...*fakeGatewaySocket) (*GatewaySupervisor, func() int) {
	t.Helper()
	var mu sync.Mutex
	built := 0
	supervisor := newGatewaySupervisor(func(notifyDisconnected func()) (gatewaySocket, error) {
		mu.Lock()
		defer mu.Unlock()
		if built >= len(sockets) {
			return nil, errors.New("the test gave the supervisor no further socket")
		}
		socket := sockets[built]
		built++
		socket.mu.Lock()
		socket.notifyDisconnected = notifyDisconnected
		socket.mu.Unlock()
		return socket, nil
	}, timings)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		supervisor.Run(stop)
		close(finished)
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("the supervisor did not return after stop was closed")
		}
	})
	return supervisor, func() int {
		mu.Lock()
		defer mu.Unlock()
		return built
	}
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

// The defect this file exists for: the socket closed on 2026-09-15 and nothing
// ever opened it again. A closed socket must be reopened, and on the SAME
// session, because that is what lets discordgo RESUME and replay the gap.
func TestAClosedGatewaySocketIsReopenedOnTheSameSession(t *testing.T) {
	socket := &fakeGatewaySocket{}
	supervisor, built := superviseFakeSockets(t, testGatewaySupervisorTimings, socket)

	waitUntil(t, "the first open", func() bool { return supervisor.Status().State == GatewayConnected })
	socket.notifyDisconnected()
	waitUntil(t, "the reopen", func() bool { return socket.opens() == 2 && supervisor.Status().Opens == 2 })

	if got := built(); got != 1 {
		t.Errorf("sessions built = %d, want 1: a new session cannot RESUME", got)
	}
	if status := supervisor.Status(); status.State != GatewayConnected || status.AbandonedSessions != 0 {
		t.Errorf("status after the reopen = %+v", status)
	}
}

func TestAFailedGatewayOpenIsRetriedAndTheReasonIsReported(t *testing.T) {
	handshake := errors.New("websocket: bad handshake")
	// Enough failures that the state is observable while it is down.
	socket := &fakeGatewaySocket{openResults: []error{handshake, handshake, handshake, handshake, handshake, handshake}}
	supervisor, _ := superviseFakeSockets(t, testGatewaySupervisorTimings, socket)

	waitUntil(t, "the failure shows in the status", func() bool {
		status := supervisor.Status()
		return status.State == GatewayDown && status.LastError == handshake.Error()
	})
	waitUntil(t, "the open that works", func() bool { return supervisor.Status().State == GatewayConnected })

	status := supervisor.Status()
	if status.LastError != "" {
		t.Errorf("a connected gateway still reports an error: %q", status.LastError)
	}
	if status.Opens != 1 || socket.opens() != 7 {
		t.Errorf("opens counted = %d after %d calls, want 1 after 7", status.Opens, socket.opens())
	}
}

// discordgo's Open() holds the session lock while it reads Discord's reply, and
// a reply that makes it call Close() from inside Open() never returns. Waiting
// on that session is waiting forever, so it is replaced.
func TestAGatewayOpenThatNeverReturnsIsAbandonedForANewSession(t *testing.T) {
	stuck := &fakeGatewaySocket{openBlocksForever: true}
	fresh := &fakeGatewaySocket{}
	supervisor, built := superviseFakeSockets(t, testGatewaySupervisorTimings, stuck, fresh)

	waitUntil(t, "the second session connects", func() bool {
		return supervisor.Status().State == GatewayConnected && fresh.opens() == 1
	})
	if got := built(); got != 2 {
		t.Errorf("sessions built = %d, want 2", got)
	}
	if got := supervisor.Status().AbandonedSessions; got != 1 {
		t.Errorf("abandoned sessions = %d, want 1", got)
	}
	// The abandoned session is still closed, so that if it was NOT stuck
	// Discord is not left with two sockets on one token.
	waitUntil(t, "the abandoned session is closed", func() bool { return stuck.closes() == 1 })
}

func TestAConnectedGatewayWithNoRecentHeartbeatAckIsAbandoned(t *testing.T) {
	timings := testGatewaySupervisorTimings
	timings.maxHeartbeatAckAge = time.Minute
	silent := &fakeGatewaySocket{lastHeartbeatAck: func() time.Time { return time.Now().Add(-2 * time.Minute) }}
	fresh := &fakeGatewaySocket{}
	supervisor, _ := superviseFakeSockets(t, timings, silent, fresh)

	waitUntil(t, "the second session connects", func() bool { return fresh.opens() == 1 })
	if got := supervisor.Status().AbandonedSessions; got != 1 {
		t.Errorf("abandoned sessions = %d, want 1", got)
	}
}

func TestAGatewaySessionWhoseLockIsHeldForeverIsAbandoned(t *testing.T) {
	locked := &fakeGatewaySocket{ackBlocksForever: true}
	fresh := &fakeGatewaySocket{}
	supervisor, _ := superviseFakeSockets(t, testGatewaySupervisorTimings, locked, fresh)

	waitUntil(t, "the second session connects", func() bool { return fresh.opens() == 1 })
	if got := supervisor.Status().AbandonedSessions; got != 1 {
		t.Errorf("abandoned sessions = %d, want 1", got)
	}
}

// Known-negative control for the three tests above: a healthy socket is left
// alone. Without it, a supervisor that abandoned every session would pass them.
func TestAHealthyGatewaySessionIsNeitherReopenedNorAbandoned(t *testing.T) {
	socket := &fakeGatewaySocket{}
	supervisor, built := superviseFakeSockets(t, testGatewaySupervisorTimings, socket)

	waitUntil(t, "the first open", func() bool { return supervisor.Status().State == GatewayConnected })
	time.Sleep(20 * testGatewaySupervisorTimings.livenessCheckInterval)

	status := supervisor.Status()
	if socket.opens() != 1 || built() != 1 || status.Opens != 1 || status.AbandonedSessions != 0 || status.State != GatewayConnected {
		t.Errorf("a healthy session was disturbed: %d open calls, %d sessions, status %+v", socket.opens(), built(), status)
	}
}

// discordgo's listen and heartbeat goroutines can each close the socket, so one
// death can announce itself twice. The second announcement finds the socket
// already reopened; that is not a failure and not a new connection.
func TestASecondDisconnectForOneDeathIsNotCountedAsAFailure(t *testing.T) {
	socket := &fakeGatewaySocket{openResults: []error{nil, errGatewaySocketAlreadyOpen}}
	supervisor, _ := superviseFakeSockets(t, testGatewaySupervisorTimings, socket)

	waitUntil(t, "the first open", func() bool { return supervisor.Status().State == GatewayConnected })
	socket.notifyDisconnected()
	waitUntil(t, "the second open call", func() bool {
		return socket.opens() == 2 && supervisor.Status().State == GatewayConnected
	})
	if status := supervisor.Status(); status.Opens != 1 || status.LastError != "" {
		t.Errorf("status = %+v, want one open and no error", status)
	}
}

func TestStoppingTheGatewaySupervisorClosesTheSocket(t *testing.T) {
	socket := &fakeGatewaySocket{}
	supervisor := newGatewaySupervisor(func(notifyDisconnected func()) (gatewaySocket, error) {
		return socket, nil
	}, testGatewaySupervisorTimings)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		supervisor.Run(stop)
		close(finished)
	}()
	waitUntil(t, "the first open", func() bool { return supervisor.Status().State == GatewayConnected })
	close(stop)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after stop was closed")
	}
	if socket.closes() != 1 {
		t.Errorf("close calls = %d, want 1", socket.closes())
	}
}

func TestHealthzReportsTheGatewayState(t *testing.T) {
	server, _, _ := testServer(t)
	mux := http.NewServeMux()
	server.RegisterHandlers(mux)

	read := func() GatewayStatus {
		t.Helper()
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("/healthz = %d, want 200 whatever the gateway's state", recorder.Code)
		}
		var body struct {
			Gateway GatewayStatus `json:"gateway"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /healthz: %v", err)
		}
		return body.Gateway
	}

	if got := read().State; got != GatewayDisabled {
		t.Errorf("with no supervisor, gateway.state = %q, want %q", got, GatewayDisabled)
	}

	server.ReportGatewayStatus(func() GatewayStatus {
		return GatewayStatus{State: GatewayDown, LastError: "websocket: bad handshake", Opens: 3}
	})
	got := read()
	if got.State != GatewayDown || got.LastError != "websocket: bad handshake" || got.Opens != 3 {
		t.Errorf("gateway = %+v", got)
	}
}
