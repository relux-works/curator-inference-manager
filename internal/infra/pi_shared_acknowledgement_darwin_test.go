//go:build darwin

package infra

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// The real lifecycle consumer cannot overtake a committed stop/release reply,
// including forced stop and token release to both the owner and requester.
// Socketpairs avoid a filesystem bind; the hook pauses after the event enqueue.
func TestSharedBrokerLifecycleAcknowledgementOrdering(t *testing.T) {
	for _, kind := range []string{"stop", "force-stop", "release", "token-release"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			server, client := sharedCodeSocketPair(t)
			wrapped := &sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)}
			message := sharedWireMessage{Type: "stop", Force: kind == "force-stop"}
			want := "stopping"
			var ownerClient *net.UnixConn
			if kind == "release" || kind == "token-release" {
				want = "released"
				message = sharedWireMessage{Type: "release", LeaseID: "lease"}
				owner := wrapped
				if kind == "token-release" {
					ownerServer, peer := sharedCodeSocketPair(t)
					ownerClient = peer
					owner = &sharedBrokerConnection{connection: ownerServer, reader: bufio.NewReader(ownerServer)}
					fixture.server.connections[owner] = true
					message.LeaseToken = "token"
				}
				owner.leaseID = "lease"
				fixture.server.leases["lease"] = &SharedLeaseRecord{LeaseID: "lease", LeaseToken: "token"}
				fixture.server.hadLease = true
				fixture.server.resolved.Sharing.LingerSeconds = 0
			}
			entered := make(chan struct{})
			resume := make(chan struct{})
			wrapped.beforeSend = func(message sharedWireMessage) {
				if message.Type == want {
					close(entered)
					<-resume
				}
			}
			t.Cleanup(func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			})
			done := startSharedAcknowledgementLifecycle(t, fixture.server, wrapped)
			reader := sharedAcknowledgementHello(t, client, fixture.hello)
			if err := writeSharedWireMessage(client, message); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not reach committed reply")
			}
			// Keep the reply paused while the real buffered-event consumer can
			// begin draining. Only shutdown frames/socket closure must wait.
			select {
			case err := <-done:
				t.Fatalf("shutdown overtook acknowledgement: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(resume)
			response, err := readSharedWireMessage(reader)
			if err != nil || response.Type != want {
				t.Fatalf("response=%+v err=%v want=%s", response, err, want)
			}
			if ownerClient != nil {
				response, err := readSharedWireMessage(bufio.NewReader(ownerClient))
				if err != nil || response.Type != "released" {
					t.Fatalf("owner response=%+v err=%v", response, err)
				}
			}
			requireSharedAcknowledgementShutdown(t, done)
			if fixture.server.currentState() != "draining" || fixture.server.forcedStop() != message.Force {
				t.Fatal("committed effect did not finish")
			}
			response, err = readSharedWireMessage(reader)
			if err == nil {
				expectedShutdown := "shutting_down"
				if message.Force {
					expectedShutdown = "lease_revoked"
				}
				if response.Type != expectedShutdown {
					t.Fatalf("unexpected frame after reply: %+v", response)
				}
				_, err = readSharedWireMessage(reader)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("client did not close after reply: %v", err)
			}
		})
	}
}

// A disconnected requester makes acknowledgement writing fail, but the deferred
// completion signal still permits both stop variants to finish within the bound.
func TestSharedBrokerLifecycleAcknowledgementClosedClient(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			server, client := sharedCodeSocketPair(t)
			wrapped := &sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)}
			wrapped.beforeSend = func(message sharedWireMessage) {
				if message.Type == "stopping" {
					_ = client.Close()
				}
			}
			done := startSharedAcknowledgementLifecycle(t, fixture.server, wrapped)
			sharedAcknowledgementHello(t, client, fixture.hello)
			if err := writeSharedWireMessage(client, sharedWireMessage{Type: "stop", Force: force}); err != nil {
				t.Fatal(err)
			}
			requireSharedAcknowledgementShutdown(t, done)
			if fixture.server.currentState() != "draining" || fixture.server.forcedStop() != force {
				t.Fatal("failed reply cancelled committed stop")
			}
		})
	}
}

// Even a handler stalled before writing cannot hold the lifecycle indefinitely:
// timeout closes its socket, preventing a later reply or shutdown frame overtaking it.
// A peer that disappears while the handler is stalled must obey the same bound.
func TestSharedBrokerLifecycleAcknowledgementTimeout(t *testing.T) {
	for _, disappears := range []bool{false, true} {
		t.Run(fmt.Sprintf("disappears=%t", disappears), func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			server, client := sharedCodeSocketPair(t)
			wrapped := &sharedBrokerConnection{connection: server, reader: bufio.NewReader(server)}
			entered, resume, handlerReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			wrapped.beforeSend = func(message sharedWireMessage) {
				if message.Type == "stopping" {
					close(entered)
					<-resume
					close(handlerReturned)
				}
			}
			t.Cleanup(func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			})
			done := startSharedAcknowledgementLifecycle(t, fixture.server, wrapped)
			reader := sharedAcknowledgementHello(t, client, fixture.hello)
			if err := writeSharedWireMessage(client, sharedWireMessage{Type: "stop"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not reach committed reply")
			}
			started := time.Now()
			if disappears {
				_ = client.Close()
			}
			requireSharedAcknowledgementShutdown(t, done)
			if elapsed := time.Since(started); elapsed > sharedBrokerAcknowledgementTimeout+750*time.Millisecond {
				t.Fatalf("shutdown exceeded one overall acknowledgement bound: %s", elapsed)
			}
			if !disappears {
				if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
					t.Fatalf("timeout did not close pending reply socket: %v", err)
				}
			}
			close(resume)
			select {
			case <-handlerReturned:
			case <-time.After(time.Second):
				t.Fatal("stalled handler did not resume")
			}
		})
	}
}

func sharedAcknowledgementHello(t *testing.T, client *net.UnixConn, hello sharedWireMessage) *bufio.Reader {
	t.Helper()
	if err := writeSharedWireMessage(client, hello); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	if response, err := readSharedWireMessage(reader); err != nil || response.Type != "hello_ok" {
		t.Fatalf("hello=%+v err=%v", response, err)
	}
	return reader
}

func requireSharedAcknowledgementShutdown(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(sharedBrokerAcknowledgementTimeout + time.Second):
		t.Fatal("lifecycle did not shut down within acknowledgement bound plus scheduling allowance")
	}
}

func startSharedAcknowledgementLifecycle(t *testing.T, server *sharedBrokerServer, connections ...*sharedBrokerConnection) <-chan error {
	t.Helper()
	return startSharedAcknowledgementLifecycleAtShutdown(t, server, nil, connections...)
}

func startSharedAcknowledgementLifecycleAtShutdown(t *testing.T, server *sharedBrokerServer, beforeShutdown func(), connections ...*sharedBrokerConnection) <-chan error {
	t.Helper()
	// Let serve own connection registration/dispatch. Its acceptor uses real
	// socketpairs, then blocks until cleanup, like a listener being closed.
	if cap(server.events) != 32 {
		t.Fatal("test must retain the production buffered event queue")
	}
	server.readyAt = time.Now()
	server.resolved.Sharing.StableRunSeconds = 60
	server.resolved.Sharing.BrokerStartTimeoutSeconds = 60
	server.listener = &net.UnixListener{}
	accepted := 0
	acceptClosed, acceptReturned := make(chan struct{}), make(chan struct{})
	server.acceptUnix = func() (*net.UnixConn, error) {
		if accepted < len(connections) {
			connection := connections[accepted]
			accepted++
			return connection.connection, nil
		}
		<-acceptClosed
		close(acceptReturned)
		return nil, net.ErrClosed
	}
	// serve creates the wrapper itself, so install the per-connection hook at
	// hello admission, before the first send and before any stop/release request.
	identity := server.admission.peerIdentity
	server.admission.peerIdentity = func(c *net.UnixConn) (uint32, int, sharedAuditToken, error) {
		server.mu.Lock()
		for wrapped := range server.connections {
			for _, connection := range connections {
				if wrapped.connection == c && connection.connection == c {
					wrapped.beforeSend = connection.beforeSend
					wrapped.leaseID = connection.leaseID
				}
			}
		}
		server.mu.Unlock()
		return identity(c)
	}
	done := make(chan error, 1)
	signals := make(chan os.Signal, 1)
	go func() {
		err := server.serve(&piProcessWait{done: make(chan struct{})}, signals)
		close(acceptClosed)
		<-acceptReturned
		if beforeShutdown != nil {
			beforeShutdown()
		}
		if server.forcedStop() {
			server.closeConnections("lease_revoked", "operator_force_stop")
		} else {
			server.closeConnections("shutting_down", "broker_draining")
		}
		done <- err
	}()
	t.Cleanup(func() {
		signals <- os.Interrupt
		for _, connection := range connections {
			_ = connection.connection.Close()
		}
	})
	return done
}

// With the production buffered queue, an earlier release observes the later
// final release/stop effect. Shutdown must wait for both replies, even though
// the later event is still queued. An independent observer receives shutdown
// only after the paused reply; request handlers may close after their replies.
// Two stalled replies (one peer disappears) share one overall timeout, which
// closes only their sockets and still permits the observer shutdown frame.
func TestSharedBrokerLifecycleAcknowledgementQueuedEffects(t *testing.T) {
	for _, later := range []string{"release", "stop", "timeout"} {
		t.Run(later, func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			server := fixture.server
			server.resolved.Sharing.LingerSeconds = 0
			a, clientA := sharedCodeSocketPair(t)
			b, clientB := sharedCodeSocketPair(t)
			observer, observerClient := sharedCodeSocketPair(t)
			server.connections[&sharedBrokerConnection{connection: observer}] = true
			first := &sharedBrokerConnection{connection: a, leaseID: "a"}
			second := &sharedBrokerConnection{connection: b}
			server.leases["a"] = &SharedLeaseRecord{LeaseID: "a"}
			server.hadLease = true
			want := "stopping"
			messageB := sharedWireMessage{Type: "stop"}
			if later == "release" {
				want = "released"
				second.leaseID = "b"
				server.leases["b"] = &SharedLeaseRecord{LeaseID: "b"}
				messageB = sharedWireMessage{Type: "release", LeaseID: "b"}
			}
			enteredA, resumeA := sharedAcknowledgementPause(t, first, "released")
			enteredB, resumeB := sharedAcknowledgementPause(t, second, want)
			eventEntered, resumeEvent := make(chan struct{}), make(chan struct{})
			server.beforeEvent = func(event sharedBrokerEvent) {
				if event.kind == "lease-change" {
					close(eventEntered)
					<-resumeEvent
				}
			}
			defer close(resumeEvent)
			shutdownEntered := make(chan struct{})
			done := startSharedAcknowledgementLifecycleAtShutdown(t, server, func() { close(shutdownEntered) }, first, second)
			readerA := sharedAcknowledgementHello(t, clientA, fixture.hello)
			readerB := sharedAcknowledgementHello(t, clientB, fixture.hello)
			if err := writeSharedWireMessage(clientA, sharedWireMessage{Type: "release", LeaseID: "a"}); err != nil {
				t.Fatal(err)
			}
			awaitSharedAcknowledgementSignal(t, enteredA)
			awaitSharedAcknowledgementSignal(t, eventEntered)
			// A is dequeued but cannot inspect leases yet. Commit B in parallel
			// with A's pending write and leave B's event in the buffered queue.
			if err := writeSharedWireMessage(clientB, messageB); err != nil {
				t.Fatal(err)
			}
			awaitSharedAcknowledgementSignal(t, enteredB)
			if len(server.events) != 1 {
				t.Fatalf("later effect not queued: %d events", len(server.events))
			}
			if later == "timeout" {
				started := time.Now()
				resumeEvent <- struct{}{}
				_ = clientB.Close()
				requireSharedAcknowledgementShutdown(t, done)
				if elapsed := time.Since(started); elapsed > sharedBrokerAcknowledgementTimeout+750*time.Millisecond {
					t.Fatalf("multiple replies extended the overall timeout: %s", elapsed)
				}
				if _, err := readerA.ReadByte(); !errors.Is(err, io.EOF) {
					t.Fatalf("pending release socket not closed before shutdown: %v", err)
				}
				if _, err := b.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("disappeared peer's pending reply socket not closed: %v", err)
				}
				requireSharedAcknowledgementFrame(t, bufio.NewReader(observerClient), "shutting_down")
				close(resumeA)
				close(resumeB)
				return
			}
			close(resumeA)
			requireSharedAcknowledgementFrame(t, readerA, "released")
			// Release the consumer without closing the deferred cleanup signal.
			resumeEvent <- struct{}{}
			awaitSharedAcknowledgementSignal(t, shutdownEntered)
			select {
			case err := <-done:
				t.Fatalf("earlier event bypassed later pending reply: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			deadline := time.Now().Add(time.Second)
			for server.currentState() != "draining" {
				if time.Now().After(deadline) {
					t.Fatal("real lifecycle consumer did not begin shutdown")
				}
				time.Sleep(time.Millisecond)
			}
			_ = observerClient.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			readerObserver := bufio.NewReader(observerClient)
			if message, err := readSharedWireMessage(readerObserver); err == nil {
				t.Fatalf("shutdown frame overtook later acknowledgement: %+v", message)
			} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatalf("observer closed before acknowledgement: %v", err)
			}
			_ = observerClient.SetReadDeadline(time.Time{})
			close(resumeB)
			requireSharedAcknowledgementFrame(t, readerB, want)
			requireSharedAcknowledgementShutdown(t, done)
			requireSharedAcknowledgementFrame(t, readerObserver, "shutting_down")
			if _, err := readerObserver.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("observer did not close after shutdown: %v", err)
			}
			server.mu.Lock()
			pending := len(server.acknowledgements)
			server.mu.Unlock()
			if pending != 0 {
				t.Fatalf("reply registration leaked: %d", pending)
			}
		})
	}
}

// Once the real lifecycle seals admission, later requests receive the typed
// shutdown refusal without registering a reply or committing another effect.
// The already-committed stop remains a positive control and completes normally.
func TestSharedBrokerLifecycleAcknowledgementRefusesLateEffects(t *testing.T) {
	for _, kind := range []string{"acquire", "heartbeat", "release", "token-release", "stop"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			a, clientA := sharedCodeSocketPair(t)
			b, clientB := sharedCodeSocketPair(t)
			first := &sharedBrokerConnection{connection: a}
			second := &sharedBrokerConnection{connection: b}
			entered, resume := sharedAcknowledgementPause(t, first, "stopping")
			done := startSharedAcknowledgementLifecycle(t, fixture.server, first, second)
			readerA := sharedAcknowledgementHello(t, clientA, fixture.hello)
			readerB := sharedAcknowledgementHello(t, clientB, fixture.hello)
			if err := writeSharedWireMessage(clientA, sharedWireMessage{Type: "stop"}); err != nil {
				t.Fatal(err)
			}
			awaitSharedAcknowledgementSignal(t, entered)
			deadline := time.Now().Add(time.Second)
			for fixture.server.currentState() != "draining" {
				if time.Now().After(deadline) {
					t.Fatal("lifecycle did not seal admission")
				}
				time.Sleep(time.Millisecond)
			}
			message := sharedWireMessage{Type: kind, LeaseID: "missing"}
			if kind == "token-release" {
				message.Type, message.LeaseToken = "release", "missing-token"
			}
			if err := writeSharedWireMessage(clientB, message); err != nil {
				t.Fatal(err)
			}
			response, err := readSharedWireMessage(readerB)
			if err != nil || response.Type != "refused" || response.Code != "shared_runtime_shutting_down" {
				t.Fatalf("late request response=%+v err=%v", response, err)
			}
			if _, err := readerB.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("late request connection not closed: %v", err)
			}
			fixture.server.mu.Lock()
			pending, leases := len(fixture.server.acknowledgements), len(fixture.server.leases)
			fixture.server.mu.Unlock()
			if pending != 1 || leases != 0 || len(fixture.server.events) != 0 {
				t.Fatalf("late effect committed/registered: pending=%d leases=%d events=%d", pending, leases, len(fixture.server.events))
			}
			close(resume)
			requireSharedAcknowledgementFrame(t, readerA, "stopping")
			requireSharedAcknowledgementShutdown(t, done)
		})
	}
}

func sharedAcknowledgementPause(t *testing.T, connection *sharedBrokerConnection, kind string) (<-chan struct{}, chan struct{}) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	connection.beforeSend = func(message sharedWireMessage) {
		if message.Type == kind {
			close(entered)
			<-resume
		}
	}
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	return entered, resume
}

func awaitSharedAcknowledgementSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("handler/lifecycle did not reach scheduling boundary")
	}
}

func requireSharedAcknowledgementFrame(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	if response, err := readSharedWireMessage(reader); err != nil || response.Type != want {
		t.Fatalf("response=%+v err=%v want=%s", response, err, want)
	}
}

// Forced stop also waits for pending acquire/heartbeat/provider-status effects,
// not just releases and stops. Successful replies prove these requests are not
// rejected wholesale.
func TestSharedBrokerLifecycleAcknowledgementOtherEffects(t *testing.T) {
	for _, kind := range []string{"acquire", "heartbeat", "status"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newSharedBrokerAdmissionFixture(t)
			server := fixture.server
			a, clientA := sharedCodeSocketPair(t)
			b, clientB := sharedCodeSocketPair(t)
			first, second := &sharedBrokerConnection{connection: a}, &sharedBrokerConnection{connection: b}
			want := map[string]string{"acquire": "lease", "heartbeat": "heartbeat_ok", "status": "status"}[kind]
			message := sharedWireMessage{Type: kind}
			if kind == "heartbeat" {
				first.leaseID, message.LeaseID = "held", "held"
				server.leases["held"] = &SharedLeaseRecord{LeaseID: "held"}
			}
			if kind == "status" {
				configureSharedBrokerResourceFixture(&fixture, observedSharedRuntimeResources(800, "idle"))
			}
			entered, resume := sharedAcknowledgementPause(t, first, want)
			done := startSharedAcknowledgementLifecycle(t, server, first, second)
			readerA := sharedAcknowledgementHello(t, clientA, fixture.hello)
			readerB := sharedAcknowledgementHello(t, clientB, fixture.hello)
			if err := writeSharedWireMessage(clientA, message); err != nil {
				t.Fatal(err)
			}
			awaitSharedAcknowledgementSignal(t, entered)
			server.mu.Lock()
			pending := len(server.acknowledgements)
			server.mu.Unlock()
			if pending != 1 {
				t.Fatalf("committed %s did not register acknowledgement: %d", kind, pending)
			}
			if err := writeSharedWireMessage(clientB, sharedWireMessage{Type: "stop", Force: true}); err != nil {
				t.Fatal(err)
			}
			requireSharedAcknowledgementFrame(t, readerB, "stopping")
			select {
			case err := <-done:
				t.Fatalf("forced stop bypassed pending %s reply: %v", kind, err)
			case <-time.After(50 * time.Millisecond):
			}
			close(resume)
			requireSharedAcknowledgementFrame(t, readerA, want)
			requireSharedAcknowledgementShutdown(t, done)
			requireSharedAcknowledgementFrame(t, readerA, "lease_revoked")
		})
	}
}

// During a pending shutdown, provider status preserves draining diagnostics
// without observing the provider, changing generations, or registering an effect.
func TestSharedBrokerLifecycleAcknowledgementDrainingStatusReadOnly(t *testing.T) {
	fixture := newSharedBrokerAdmissionFixture(t)
	configureSharedBrokerResourceFixture(&fixture)
	server := fixture.server
	a, clientA := sharedCodeSocketPair(t)
	b, clientB := sharedCodeSocketPair(t)
	first, second := &sharedBrokerConnection{connection: a}, &sharedBrokerConnection{connection: b}
	entered, resume := sharedAcknowledgementPause(t, first, "stopping")
	done := startSharedAcknowledgementLifecycle(t, server, first, second)
	readerA := sharedAcknowledgementHello(t, clientA, fixture.hello)
	readerB := sharedAcknowledgementHello(t, clientB, fixture.hello)
	if err := writeSharedWireMessage(clientA, sharedWireMessage{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	awaitSharedAcknowledgementSignal(t, entered)
	deadline := time.Now().Add(time.Second)
	for server.currentState() != "draining" {
		if time.Now().After(deadline) {
			t.Fatal("lifecycle did not begin shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	if err := writeSharedWireMessage(clientB, sharedWireMessage{Type: "status"}); err != nil {
		t.Fatal(err)
	}
	response, err := readSharedWireMessage(readerB)
	if err != nil || response.Type != "status" || response.State != "draining" || response.Resources == nil || response.Resources.State != "draining" || response.Resources.Admission != "refused" {
		t.Fatalf("draining status=%+v err=%v", response, err)
	}
	server.mu.Lock()
	pending, admission, status := len(server.acknowledgements), server.resourceAdmissionGeneration, server.resourceStatusGeneration
	server.mu.Unlock()
	if pending != 1 || admission != 0 || status != 0 {
		t.Fatalf("draining diagnostic committed an effect: pending=%d admission=%d status=%d", pending, admission, status)
	}
	close(resume)
	requireSharedAcknowledgementFrame(t, readerA, "stopping")
	requireSharedAcknowledgementShutdown(t, done)
}
