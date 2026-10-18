//go:build darwin

package infra

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func answerSharedAcquire(t *testing.T, fixture sharedAttestationFixture, response sharedWireMessage, readAcquire bool) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		connection, err := fixture.listener.AcceptUnix()
		if err != nil {
			result <- err
			return
		}
		defer func() {
			if err := connection.Close(); err != nil {
				t.Errorf("close broker connection: %v", err)
			}
		}()
		reader := bufio.NewReaderSize(connection, sharedRuntimeMaxFrameBytes+1)
		if _, err := readSharedWireMessage(reader); err != nil {
			result <- err
			return
		}
		effective := fixture.resolved.Sharing
		hello := sharedWireMessage{
			Type: "hello_ok", ProtocolVersion: SharedRuntimeProtocolVersion,
			RuntimeKey: fixture.resolved.RuntimeKey, ProfileDigest: fixture.resolved.ProfileDigest,
			EngineIdentity: &fixture.resolved.EngineIdentity, Broker: &fixture.broker,
			Runtime: &fixture.record, EffectiveSharing: &effective,
		}
		if err := writeSharedWireMessage(connection, hello); err != nil {
			result <- err
			return
		}
		if readAcquire {
			if _, err := readSharedWireMessage(reader); err != nil {
				result <- err
				return
			}
		}
		result <- writeSharedWireMessage(connection, response)
	}()
	return result
}

func answerAttestationWithoutClientHello(t *testing.T, fixture sharedAttestationFixture) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		connection, err := fixture.listener.AcceptUnix()
		if err != nil {
			result <- err
			return
		}
		defer func() {
			if err := connection.Close(); err != nil {
				t.Errorf("close broker connection: %v", err)
			}
		}()
		effective := fixture.resolved.Sharing
		response := sharedWireMessage{
			Type: "hello_ok", ProtocolVersion: SharedRuntimeProtocolVersion,
			RuntimeKey: fixture.resolved.RuntimeKey, ProfileDigest: fixture.resolved.ProfileDigest,
			EngineIdentity: &fixture.resolved.EngineIdentity, Broker: &fixture.broker,
			Runtime: &fixture.record, EffectiveSharing: &effective,
		}
		if err := writeSharedWireMessage(connection, response); err != nil {
			result <- err
			return
		}
		// Keep the peer alive through identity inspection and the injected hello
		// write. Only the client's refusal/close ends this fixture connection.
		_, err = io.Copy(io.Discard, connection)
		result <- err
	}()
	return result
}

func validSharedLeaseResponse(fixture sharedAttestationFixture) sharedWireMessage {
	return sharedWireMessage{
		Type:    "lease",
		Lease:   &SharedLeaseRecord{LeaseID: "fixture-lease", LeaseToken: "fixture-token", LeaseLifetime: "client_process"},
		Runtime: &fixture.record,
	}
}

func TestConnectAndAcquireSharedRuntimeRefusesEveryMalformedLeaseResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sharedWireMessage)
	}{
		{"wrong type", func(message *sharedWireMessage) { message.Type = "other" }},
		{"missing lease", func(message *sharedWireMessage) { message.Lease = nil }},
		{"missing runtime", func(message *sharedWireMessage) { message.Runtime = nil }},
		{"empty token", func(message *sharedWireMessage) { message.Lease.LeaseToken = "" }},
		{"token equals id", func(message *sharedWireMessage) { message.Lease.LeaseToken = message.Lease.LeaseID }},
		{"unknown lease lifetime", func(message *sharedWireMessage) { message.Lease.LeaseLifetime = "broker_process" }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			response := validSharedLeaseResponse(fixture)
			testCase.mutate(&response)
			served := answerSharedAcquire(t, fixture, response, true)
			_, err := connectAndAcquireSharedRuntime(fixture.resolved, fixture.state, "RUN-lease", nil)
			requireSharedAttestationCode(t, err, "protocol_violation")
			if err := <-served; err != nil {
				t.Fatalf("fixture exchange: %v", err)
			}
		})
	}
}

func TestConnectAndAcquireSharedRuntimeRefusesAcquireWriteFailure(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	served := answerSharedAcquire(t, fixture, validSharedLeaseResponse(fixture), false)
	original := sharedRuntimeAcquireWriteMessage
	sharedRuntimeAcquireWriteMessage = func(_ io.Writer, message sharedWireMessage) error {
		if message.Type == "acquire" {
			return os.ErrPermission
		}
		return original(io.Discard, message)
	}
	t.Cleanup(func() { sharedRuntimeAcquireWriteMessage = original })
	_, err := connectAndAcquireSharedRuntime(fixture.resolved, fixture.state, "RUN-acquire-write", nil)
	requireSharedAttestationCode(t, err, "broker_unreachable")
	if err := <-served; err != nil {
		t.Fatalf("fixture exchange: %v", err)
	}
}

func TestConnectAndAcquireSharedRuntimeRefusesAcquireReadFailureAfterValidFrame(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	served := answerSharedAcquire(t, fixture, validSharedLeaseResponse(fixture), true)
	original := sharedRuntimeAcquireReadMessage
	sharedRuntimeAcquireReadMessage = func(reader io.Reader) (sharedWireMessage, error) {
		message, err := original(reader)
		if err != nil {
			return message, err
		}
		return message, os.ErrPermission
	}
	t.Cleanup(func() { sharedRuntimeAcquireReadMessage = original })
	_, err := connectAndAcquireSharedRuntime(fixture.resolved, fixture.state, "RUN-acquire-read", nil)
	requireSharedAttestationCode(t, err, "shared_runtime_unavailable")
	if err := <-served; err != nil {
		t.Fatalf("fixture exchange: %v", err)
	}
}

func TestConnectAndAcquireSharedRuntimePreservesBrokerRefusal(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	served := answerSharedAcquire(t, fixture, sharedWireMessage{Type: "refused", Code: "lease_limit_reached"}, true)
	_, err := connectAndAcquireSharedRuntime(fixture.resolved, fixture.state, "RUN-refused", nil)
	requireSharedAttestationCode(t, err, "lease_limit_reached")
	if err := <-served; err != nil {
		t.Fatalf("fixture exchange: %v", err)
	}
}

func TestReadSharedWireMessageRejectsMalformedAndTrailingFrames(t *testing.T) {
	for name, frame := range map[string]string{
		"malformed":       "not-json\n",
		"trailing object": "{\"type\":\"hello\"} {\"type\":\"hello\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readSharedWireMessage(strings.NewReader(frame))
			requireSharedAttestationCode(t, err, "protocol_violation")
		})
	}
}

func TestWriteSharedWireMessageRejectsOversizedFrame(t *testing.T) {
	var output bytes.Buffer
	err := writeSharedWireMessage(&output, sharedWireMessage{Type: "refused", Reason: strings.Repeat("x", sharedRuntimeMaxFrameBytes)})
	requireSharedAttestationCode(t, err, "protocol_violation")
	if output.Len() != 0 {
		t.Fatalf("oversized frame wrote %d bytes", output.Len())
	}
}

func TestConnectAndAttestSharedRuntimeClassifiesAbsentAndRefusedSocketsForAcquisition(t *testing.T) {
	for _, name := range []string{"absent", "refused"} {
		t.Run(name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			if err := fixture.listener.Close(); err != nil {
				t.Fatal(err)
			}
			if name == "refused" {
				address, err := net.ResolveUnixAddr("unix", fixture.resolved.Paths.RendezvousSocket)
				if err != nil {
					t.Fatal(err)
				}
				listener, err := net.ListenUnix("unix", address)
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-connect-error", nil, time.Second)
			if sharedRuntimeErrorCode(err) == "broker_unreachable" {
				t.Fatalf("absent or refused AF_UNIX socket was reclassified as broker_unreachable: %v", err)
			}
			var connectError *sharedConnectError
			if !errors.As(err, &connectError) {
				t.Fatalf("connect error=%v want sharedConnectError", err)
			}
			want := syscall.ENOENT
			if name == "refused" {
				want = syscall.ECONNREFUSED
			}
			if !errors.Is(connectError, want) {
				t.Fatalf("connect cause=%v want %v", connectError, want)
			}
		})
	}
}

func TestConnectAndAttestSharedRuntimeRefusesSetDeadlineFailure(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	served := fixture.answerOnce(t, nil)
	original := sharedRuntimeAttestationSetDeadline
	sharedRuntimeAttestationSetDeadline = func(*net.UnixConn, time.Time) error { return os.ErrPermission }
	t.Cleanup(func() { sharedRuntimeAttestationSetDeadline = original })
	_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-deadline", nil, time.Second)
	requireSharedAttestationCode(t, err, "broker_unreachable")
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("fixture did not observe or close the refused connection")
	}
}

func TestConnectAndAttestSharedRuntimeClassifiesHelloWriteAndReadFailures(t *testing.T) {
	helloWriteAttempted := false
	tests := []struct {
		name                     string
		mutate                   func() func()
		code                     string
		answerWithoutClientHello bool
	}{
		{
			name: "hello write", code: "broker_unreachable",
			answerWithoutClientHello: true,
			mutate: func() func() {
				original := sharedRuntimeAttestationWriteMessage
				sharedRuntimeAttestationWriteMessage = func(_ io.Writer, message sharedWireMessage) error {
					if message.Type == "hello" {
						helloWriteAttempted = true
						return os.ErrPermission
					}
					return original(io.Discard, message)
				}
				return func() { sharedRuntimeAttestationWriteMessage = original }
			},
		},
		{
			name: "eof read", code: "shared_runtime_unavailable",
			mutate: func() func() {
				original := sharedRuntimeAttestationReadMessage
				sharedRuntimeAttestationReadMessage = func(io.Reader) (sharedWireMessage, error) { return sharedWireMessage{}, io.EOF }
				return func() { sharedRuntimeAttestationReadMessage = original }
			},
		},
		{
			name: "connection reset read", code: "shared_runtime_unavailable",
			mutate: func() func() {
				original := sharedRuntimeAttestationReadMessage
				sharedRuntimeAttestationReadMessage = func(io.Reader) (sharedWireMessage, error) { return sharedWireMessage{}, syscall.ECONNRESET }
				return func() { sharedRuntimeAttestationReadMessage = original }
			},
		},
		{
			name: "network read", code: "broker_unreachable",
			mutate: func() func() {
				original := sharedRuntimeAttestationReadMessage
				sharedRuntimeAttestationReadMessage = func(io.Reader) (sharedWireMessage, error) {
					return sharedWireMessage{}, &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}
				}
				return func() { sharedRuntimeAttestationReadMessage = original }
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			var served <-chan error
			if testCase.answerWithoutClientHello {
				served = answerAttestationWithoutClientHello(t, fixture)
			} else {
				served = fixture.answerOnce(t, nil)
			}
			restore := testCase.mutate()
			t.Cleanup(restore)
			_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-io", nil, time.Second)
			if testCase.answerWithoutClientHello && !helloWriteAttempted {
				t.Fatal("connection refused before the injected hello write")
			}
			requireSharedAttestationCode(t, err, testCase.code)
			select {
			case <-served:
			case <-time.After(time.Second):
				t.Fatal("fixture did not observe refused attestation")
			}
		})
	}
}

func TestConnectAndAttestSharedRuntimeRefusesEveryIdentityInspectionError(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		mutate func(*sharedRuntimeAttestationDependencies, sharedAttestationFixture)
	}{
		{"peer identity error", "broker_peer_untrusted", func(system *sharedRuntimeAttestationDependencies, fixture sharedAttestationFixture) {
			system.peerIdentity = func(*net.UnixConn) (uint32, int, error) {
				return fixture.broker.UID, fixture.broker.PID, os.ErrPermission
			}
		}},
		{"inspect process error", "broker_identity_unavailable", func(system *sharedRuntimeAttestationDependencies, fixture sharedAttestationFixture) {
			original := system.inspectProcess
			system.inspectProcess = func(pid int) (sharedProcessObservation, error) {
				observation, err := original(pid)
				if pid == fixture.broker.PID {
					return sharedProcessObservation{PID: pid, UID: fixture.broker.UID, PStat: 0, StartTime: fixture.broker.StartTime}, os.ErrPermission
				}
				return observation, err
			}
		}},
		{"own executable identity error", "broker_identity_unavailable", func(system *sharedRuntimeAttestationDependencies, _ sharedAttestationFixture) {
			system.ownExecutableIdentity = func() (string, FileIdentity, error) {
				return "/bin/true", FileIdentity{Dev: 1, Ino: 2}, os.ErrPermission
			}
		}},
		{"peer executable identity error", "broker_identity_unavailable", func(system *sharedRuntimeAttestationDependencies, fixture sharedAttestationFixture) {
			system.processExecIdentity = func(sharedProcessObservation) (FileIdentity, error) {
				return fixture.broker.ExecutableIdentity, os.ErrPermission
			}
		}},
		{"runtime executable identity error", "runtime_executable_invalid", func(system *sharedRuntimeAttestationDependencies, fixture sharedAttestationFixture) {
			identity, err := fileIdentity(fixture.resolved.Profile.Runtime.Executable)
			if err != nil {
				t.Fatal(err)
			}
			system.fileIdentity = func(string) (FileIdentity, error) { return identity, os.ErrPermission }
		}},
		{"runtime process inspection error with matching witness", "runtime_identity_mismatch", func(system *sharedRuntimeAttestationDependencies, fixture sharedAttestationFixture) {
			original := system.inspectProcess
			system.inspectProcess = func(pid int) (sharedProcessObservation, error) {
				observation, err := original(pid)
				if pid == fixture.runtime.Process.Pid {
					return observation, os.ErrPermission
				}
				return observation, err
			}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			original := sharedRuntimeAttestationSystem
			mutated := original
			testCase.mutate(&mutated, fixture)
			sharedRuntimeAttestationSystem = mutated
			t.Cleanup(func() { sharedRuntimeAttestationSystem = original })
			served := fixture.answerOnce(t, nil)
			_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-identity", nil, time.Second)
			requireSharedAttestationCode(t, err, testCase.code)
			select {
			case <-served:
			case <-time.After(time.Second):
				t.Fatal("fixture did not observe refused attestation")
			}
		})
	}
}

func TestStartSharedRuntimeBrokerRefusesExecutableNullAndCommandStartFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(sharedAttestationFixture)
	}{
		{
			name: "executable identity",
			mutate: func(sharedAttestationFixture) {
				sharedRuntimeStartOwnExecutableIdentity = func() (string, FileIdentity, error) { return "/usr/bin/true", FileIdentity{}, os.ErrPermission }
			},
		},
		{
			name: "dev null open",
			mutate: func(sharedAttestationFixture) {
				sharedRuntimeStartOpenDevNull = func() (*os.File, error) { return nil, os.ErrPermission }
			},
		},
		{
			name: "command start",
			mutate: func(sharedAttestationFixture) {
				sharedRuntimeStartOwnExecutableIdentity = func() (string, FileIdentity, error) {
					return filepath.Join(os.TempDir(), "missing-curator-executable"), FileIdentity{}, nil
				}
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			oldIdentity, oldOpen := sharedRuntimeStartOwnExecutableIdentity, sharedRuntimeStartOpenDevNull
			t.Cleanup(func() { sharedRuntimeStartOwnExecutableIdentity, sharedRuntimeStartOpenDevNull = oldIdentity, oldOpen })
			if err := os.MkdirAll(fixture.resolved.Paths.RuntimeCWD, 0o700); err != nil {
				t.Fatal(err)
			}
			testCase.mutate(fixture)
			child, err := startSharedRuntimeBroker(fixture.resolved, nil)
			if child != nil {
				select {
				case <-child.done:
				case <-time.After(2 * time.Second):
					_ = child.command.Process.Kill()
					t.Fatal("startSharedRuntimeBroker test child did not exit")
				}
			}
			requireSharedAttestationCode(t, err, "broker_start_failed")
		})
	}
}

func startTestBrokerChild(t *testing.T, script string) (*sharedBrokerChild, *exec.Cmd) {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", script)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	waited := make(chan struct{})
	go func() {
		done <- command.Wait()
		close(waited)
	}()
	child := &sharedBrokerChild{command: command, done: done}
	t.Cleanup(func() {
		// The acquisition path may already have consumed done's single result.
		// Wait completion, rather than Cmd.ProcessState, is safe to observe here.
		select {
		case <-waited:
		default:
			_ = command.Process.Kill()
			select {
			case <-waited:
			case <-time.After(2 * time.Second):
				t.Errorf("test child %d did not exit", command.Process.Pid)
			}
		}
	})
	return child, command
}

// Cleanup reaps a running child and tolerates an exit result already consumed
// by acquisition; -race checks that cleanup never races with Cmd.Wait.
func TestStartTestBrokerChildCleanup(t *testing.T) {
	for _, exited := range []bool{false, true} {
		name, script := "running", "exec sleep 30"
		if exited {
			name, script = "exit result consumed", "exit 42"
		}
		var command *exec.Cmd
		if !t.Run(name, func(t *testing.T) {
			var child *sharedBrokerChild
			child, command = startTestBrokerChild(t, script)
			if exited {
				select {
				case err := <-child.done:
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 42 {
						t.Fatalf("child exit=%v, want exit status 42", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("test child did not exit")
				}
			}
		}) {
			continue
		}
		// Subtest cleanup has synchronized with Wait before this state read.
		if command.ProcessState == nil || command.ProcessState.Success() {
			t.Fatalf("cleanup did not reap the killed/failed child: %v", command.ProcessState)
		}
	}
}

func TestAcquireSharedRuntimeLeaseRefusesBrokerChildFailureLedgerStates(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*SharedRuntimeRestartLedger)
	}{
		{"zero restart count", func(*SharedRuntimeRestartLedger) {}},
		{"manual quarantine", func(ledger *SharedRuntimeRestartLedger) { ledger.RestartCount = 1; ledger.ManualQuarantine = true }},
		{"automatic quarantine", func(ledger *SharedRuntimeRestartLedger) {
			ledger.RestartCount = 1
			until := time.Now().Add(time.Hour).UTC()
			ledger.QuarantinedUntil = &until
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			if err := fixture.listener.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.resolved.Sharing.BrokerStartTimeoutSeconds = 1
			ledger := newSharedRuntimeRestartLedger(fixture.resolved.RuntimeKey, fixture.resolved.ProfileDigest)
			testCase.configure(&ledger)
			if err := writeSharedRuntimeRestartLedger(fixture.resolved.Paths.RestartLedger, ledger); err != nil {
				t.Fatal(err)
			}
			child, _ := startTestBrokerChild(t, "exit 42")
			original := sharedRuntimeStartBroker
			sharedRuntimeStartBroker = func(sharedResolvedProfile, []string) (*sharedBrokerChild, error) { return child, nil }
			t.Cleanup(func() { sharedRuntimeStartBroker = original })
			_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-start-failed", nil, nil, context.Background())
			requireSharedAttestationCode(t, err, "broker_start_failed")
		})
	}
}

func TestAcquireSharedRuntimeLeaseReportsRunningChildAtDeadline(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.resolved.Sharing.BrokerStartTimeoutSeconds = 1
	child, command := startTestBrokerChild(t, "sleep 3")
	original := sharedRuntimeStartBroker
	sharedRuntimeStartBroker = func(sharedResolvedProfile, []string) (*sharedBrokerChild, error) { return child, nil }
	t.Cleanup(func() { sharedRuntimeStartBroker = original })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-start-timeout", nil, nil, ctx)
	requireSharedAttestationCode(t, err, "broker_start_timeout")
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Details["broker_child_pid"] != command.Process.Pid {
		t.Fatalf("timeout details=%#v want child pid %d", shared, command.Process.Pid)
	}
}

func TestAcquireSharedRuntimeLeaseDistinguishesPeerElectionTimeout(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.resolved.Sharing.BrokerStartTimeoutSeconds = 1
	child, _ := startTestBrokerChild(t, fmt.Sprintf("exit %d", sharedRuntimeExitElectionLost))
	original := sharedRuntimeStartBroker
	sharedRuntimeStartBroker = func(sharedResolvedProfile, []string) (*sharedBrokerChild, error) { return child, nil }
	t.Cleanup(func() { sharedRuntimeStartBroker = original })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-election-timeout", nil, nil, ctx)
	requireSharedAttestationCode(t, err, "shared_runtime_peer_start_timeout")
}

func TestAcquireSharedRuntimeLeaseHonorsCancelledContextWithoutElection(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-cancelled", nil, nil, ctx)
	if piErrorCode(err) != "pi_deadline_exceeded" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition error=%v", err)
	}
	if _, statErr := os.Stat(fixture.resolved.Paths.BrokerLock); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled acquisition created a broker lock: %v", statErr)
	}
}

func TestAcquireSharedRuntimeLeaseRefusesCancelledContextWhenBrokerIsReachable(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	served := answerSharedAcquire(t, fixture, validSharedLeaseResponse(fixture), true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-cancelled-reachable", nil, nil, ctx)
	if closeErr := fixture.listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("fixture did not finish the reachable-broker exchange")
	}
	if piErrorCode(err) != "pi_deadline_exceeded" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reachable acquisition error=%v", err)
	}
}

func TestAcquireSharedRuntimeLeaseReportsCancellationWhileWaitingForBroker(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	child, _ := startTestBrokerChild(t, "sleep 3")
	original := sharedRuntimeStartBroker
	sharedRuntimeStartBroker = func(sharedResolvedProfile, []string) (*sharedBrokerChild, error) { return child, nil }
	t.Cleanup(func() { sharedRuntimeStartBroker = original })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	defer cancel()
	_, err := acquireSharedRuntimeLease(fixture.resolved, fixture.state, "RUN-cancel-wait", nil, nil, ctx)
	if piErrorCode(err) != "pi_deadline_exceeded" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiting acquisition error=%v", err)
	}
}
