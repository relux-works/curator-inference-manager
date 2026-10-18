//go:build darwin

package infra

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type sharedRuntimeLeaseClient struct {
	connection *net.UnixConn
	reader     *bufio.Reader
	writeMu    sync.Mutex
	lease      SharedLeaseRecord
	broker     SharedBrokerIdentity
	runtime    SharedRuntimeProcessRecord
	effective  PiRuntimeSharing
	configured PiRuntimeSharing
	closed     chan struct{}
	closeOnce  sync.Once
}

type sharedAttestedRuntime struct {
	connection *net.UnixConn
	reader     *bufio.Reader
	broker     SharedBrokerIdentity
	runtime    SharedRuntimeProcessRecord
	effective  PiRuntimeSharing
	gates      []SharedRuntimeGateOutcome
}

func (attested *sharedAttestedRuntime) close() {
	if attested != nil && attested.connection != nil {
		_ = attested.connection.Close()
	}
}

func (client *sharedRuntimeLeaseClient) send(message sharedWireMessage) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return writeSharedWireMessage(client.connection, message)
}

func (client *sharedRuntimeLeaseClient) close() {
	client.closeOnce.Do(func() {
		if client.lease.LeaseID != "" {
			_ = client.send(sharedWireMessage{Type: "release", LeaseID: client.lease.LeaseID})
		}
		_ = client.connection.Close()
		close(client.closed)
	})
}

func (client *sharedRuntimeLeaseClient) monitor() <-chan error {
	result := make(chan error, 1)
	go func() {
		interval := time.Duration(client.effective.HeartbeatIntervalSeconds) * time.Second
		for {
			_ = client.connection.SetReadDeadline(time.Now().Add(interval))
			message, err := readSharedWireMessage(client.reader)
			if err != nil {
				var networkError net.Error
				if errors.As(err, &networkError) && networkError.Timeout() {
					if err := client.send(sharedWireMessage{Type: "heartbeat", LeaseID: client.lease.LeaseID}); err != nil {
						result <- sharedRuntimeError("broker_terminated", err)
						return
					}
					continue
				}
				select {
				case <-client.closed:
					result <- nil
				default:
					result <- sharedRuntimeError("broker_terminated", err)
				}
				return
			}
			switch message.Type {
			case "heartbeat_ok":
				continue
			case "shutting_down", "lease_revoked":
				result <- &SharedRuntimeError{Code: "broker_terminated", Reason: message.Reason, Err: errors.New("broker ended the lease")}
				return
			case "runtime_exited":
				result <- sharedRuntimeError("runtime_exited_early", errors.New("shared runtime exited"))
				return
			case "released":
				result <- nil
				return
			default:
				result <- sharedRuntimeError("protocol_violation", fmt.Errorf("unexpected broker message %q", message.Type))
				return
			}
		}
	}()
	return result
}

// sharedRuntimeLaunchOwnership is issued only by a successful broker launch.
// A child losing election conveys no authority over the elected peer's runtime.
type sharedRuntimeLaunchOwnership struct {
	brokerPID       int
	brokerStartTime ProcessStartTime
}

func (ownership sharedRuntimeLaunchOwnership) matches(broker SharedBrokerIdentity) bool {
	return ownership.brokerPID > 0 && ownership.brokerPID == broker.PID && ownership.brokerStartTime == broker.StartTime
}

type sharedBrokerChild struct {
	command   *exec.Cmd
	done      chan error
	ownership sharedRuntimeLaunchOwnership
}

func (child *sharedBrokerChild) poll() (bool, error) {
	select {
	case err := <-child.done:
		return true, err
	default:
		return false, nil
	}
}

func acquireSharedRuntimeLease(resolved sharedResolvedProfile, state PiStatePaths, runID string, environ []string, client *http.Client, ctx context.Context) (*sharedRuntimeLeaseClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	startedAt := time.Now()
	deadline := startedAt.Add(time.Duration(resolved.Sharing.BrokerStartTimeoutSeconds) * time.Second)
	handoffRetryDeadline := startedAt.Add(sharedRuntimeHandoffRetryWindow(resolved))
	if handoffRetryDeadline.After(deadline) {
		handoffRetryDeadline = deadline
	}
	backoff := 25 * time.Millisecond
	electionBackoff := 2 * time.Second
	handoffBackoff := 250 * time.Millisecond
	nextAttempt := time.Now()
	electionLostSeen := false
	transientRetries := 0
	var brokerChild *sharedBrokerChild
	var observedRecord *SharedBrokerRecord
	for {
		if err := ctx.Err(); err != nil {
			return nil, piError("pi_deadline_exceeded", err)
		}
		var ownership sharedRuntimeLaunchOwnership
		if brokerChild != nil {
			ownership = brokerChild.ownership
		}
		lease, err := connectAndAcquireSharedRuntimeWithOwnership(resolved, state, runID, client, ownership)
		if err == nil {
			return lease, nil
		}
		if !isSharedConnectAbsence(err) {
			var shared *SharedRuntimeError
			if errors.As(err, &shared) && shared.Code == "broker_executable_identity_mismatch" && time.Now().Before(handoffRetryDeadline) {
				nextAttempt = time.Now().Add(handoffBackoff)
				handoffBackoff = nextSharedRuntimeHandoffBackoff(handoffBackoff)
			} else if errors.As(err, &shared) && (shared.Code == "shared_runtime_shutting_down" || shared.Code == "shared_runtime_unavailable") && transientRetries < 3 {
				transientRetries++
			} else {
				return nil, err
			}
		}
		record, present, readErr := readSharedBrokerRecord(resolved.Paths.BrokerState)
		if readErr != nil {
			return nil, readErr
		}
		if present {
			observedRecord = record
		}

		if brokerChild != nil {
			exited, waitErr := brokerChild.poll()
			if exited {
				exitCode := 0
				if waitErr != nil {
					var exitError *exec.ExitError
					if errors.As(waitErr, &exitError) {
						exitCode = exitError.ExitCode()
					} else {
						exitCode = -1
					}
				}
				if exitCode == sharedRuntimeExitElectionLost {
					electionLostSeen = true
					brokerChild = nil
					nextAttempt = time.Now().Add(electionBackoff)
					electionBackoff *= 2
					if electionBackoff > 30*time.Second {
						electionBackoff = 30 * time.Second
					}
				} else if exitCode == sharedRuntimeExitListenerBusy && time.Now().Before(handoffRetryDeadline) {
					brokerChild = nil
					nextAttempt = time.Now().Add(handoffBackoff)
					handoffBackoff = nextSharedRuntimeHandoffBackoff(handoffBackoff)
				} else if exitCode == sharedRuntimeExitQuarantined {
					ledger, ledgerErr := readSharedRuntimeRestartLedger(resolved.Paths.RestartLedger, resolved.RuntimeKey, resolved.ProfileDigest)
					if ledgerErr != nil {
						return nil, ledgerErr
					}
					return nil, &SharedRuntimeError{Code: "shared_runtime_quarantined", Details: map[string]any{
						"restart_count": ledger.RestartCount, "quarantined_until": ledger.QuarantinedUntil, "manual": ledger.ManualQuarantine,
					}, Err: errors.New("shared runtime broker is quarantined")}
				} else if exitCode == 0 {
					brokerChild = nil
					nextAttempt = time.Now()
				} else {
					failed, present, readErr := readSharedBrokerRecord(resolved.Paths.BrokerState)
					if readErr != nil {
						return nil, readErr
					}
					if present && failed.Broker.PID == brokerChild.ownership.brokerPID && failed.Broker.StartTime == brokerChild.ownership.brokerStartTime && failed.StartupFailure != nil {
						if failed.StartupFailure.Code == "engine_weights_mismatch" && failed.StartupFailure.Details != nil {
							failed.StartupFailure.Details["expected_weights_id"] = resolved.ExpectedWeightsID
						}
						return nil, &SharedRuntimeError{Code: failed.StartupFailure.Code, Details: failed.StartupFailure.Details, Err: errors.New(failed.StartupFailure.Message)}
					}
					ledger, ledgerErr := readSharedRuntimeRestartLedger(resolved.Paths.RestartLedger, resolved.RuntimeKey, resolved.ProfileDigest)
					if ledgerErr != nil {
						return nil, ledgerErr
					}
					if ledger.RestartCount == 0 || ledger.ManualQuarantine || ledger.QuarantinedUntil != nil {
						return nil, &SharedRuntimeError{Code: "broker_start_failed", Details: map[string]any{"exit_status": exitCode, "broker_log_tail": sharedBrokerLogTail(resolved.Paths.BrokerLog)}, Err: waitErr}
					}
					brokerChild = nil
					nextAttempt = time.Now().Add(sharedRuntimeRestartDelay(ledger, time.Now().UTC()))
				}
			}
		}

		if brokerChild == nil && !time.Now().Before(nextAttempt) {
			entry := resolved.CatalogObservation
			if resolved.ExpectedWeightsID != "" && entry != nil && entry.Weights != nil && resolved.ExpectedWeightsID != entry.Weights.ID {
				observed, _, hashErr := hashEngineWeights(engineWeightsModelPath(*entry, resolved.Profile.Source))
				if hashErr != nil {
					return nil, hashErr
				}
				return nil, engineWeightsMismatch(resolved.ExpectedWeightsID, entry.Weights.ID, observed.ID)
			}
			child, err := sharedRuntimeStartBroker(resolved, environ)
			if err != nil {
				return nil, err
			}
			brokerChild = child
		}
		if present {
			observation, processErr := inspectSharedProcessKernel(record.Broker.PID)
			if sharedProcessGone(processErr) || (processErr == nil && !observation.live()) {
				electionBackoff = 2 * time.Second
				nextAttempt = time.Now()
			}
		}
		if !time.Now().Before(deadline) {
			if brokerChild != nil {
				return nil, &SharedRuntimeError{Code: "broker_start_timeout", Details: map[string]any{"broker_child_pid": brokerChild.command.Process.Pid, "wait4_wnohang": "still-running", "broker_state": "unknown"}, Err: errors.New("shared runtime acquisition deadline elapsed")}
			}
			if electionLostSeen {
				details := map[string]any{"configured_timeout_seconds": resolved.Sharing.BrokerStartTimeoutSeconds, "broker_state": "unknown"}
				if observedRecord != nil {
					details["broker_pid"] = observedRecord.Broker.PID
					details["broker_start_time"] = observedRecord.Broker.StartTime
					if observedRecord.Sharing != nil {
						details["effective_timeout_seconds"] = observedRecord.Sharing.BrokerStartTimeoutSeconds
					}
				}
				return nil, &SharedRuntimeError{Code: "shared_runtime_peer_start_timeout", Details: details, Err: errors.New("peer broker never became connectable")}
			}
			return nil, &SharedRuntimeError{Code: "broker_start_timeout", Details: map[string]any{"broker_state": "unknown"}, Err: errors.New("shared runtime acquisition deadline elapsed")}
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, piError("pi_deadline_exceeded", ctx.Err())
		}
		backoff = backoff * 3 / 2
		if backoff > 250*time.Millisecond {
			backoff = 250 * time.Millisecond
		}
	}
}

func sharedRuntimeHandoffRetryWindow(resolved sharedResolvedProfile) time.Duration {
	linger := time.Duration(resolved.Sharing.LingerSeconds) * time.Second
	shutdown := time.Duration(resolved.Profile.Runtime.ShutdownTimeoutSeconds) * time.Second
	return linger + shutdown + time.Duration(sharedRuntimeHandoffGraceSeconds)*time.Second
}

func nextSharedRuntimeHandoffBackoff(current time.Duration) time.Duration {
	current *= 2
	if current > 2*time.Second {
		return 2 * time.Second
	}
	return current
}

type sharedConnectError struct{ err error }

func (e *sharedConnectError) Error() string { return e.err.Error() }
func (e *sharedConnectError) Unwrap() error { return e.err }

func isSharedConnectAbsence(err error) bool {
	var connectError *sharedConnectError
	return errors.As(err, &connectError) && (errors.Is(connectError.err, syscall.ENOENT) || errors.Is(connectError.err, syscall.ECONNREFUSED))
}

func connectAndAcquireSharedRuntime(resolved sharedResolvedProfile, state PiStatePaths, runID string, httpClient *http.Client) (*sharedRuntimeLeaseClient, error) {
	return connectAndAcquireSharedRuntimeWithOwnership(resolved, state, runID, httpClient, sharedRuntimeLaunchOwnership{})
}

func connectAndAcquireSharedRuntimeWithOwnership(resolved sharedResolvedProfile, state PiStatePaths, runID string, httpClient *http.Client, ownership sharedRuntimeLaunchOwnership) (*sharedRuntimeLeaseClient, error) {
	attested, err := connectAndAttestSharedRuntimeWithOwnership(resolved, state, runID, httpClient, 2*time.Second, ownership)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			attested.close()
		}
	}()
	if err := validateExpectedEngineWeights(resolved, attested.runtime.Weights); err != nil {
		return nil, err
	}
	clientKey := exactStateKey(state.ProjectStateKey + "\x00" + state.ProfileStateKey + "\x00" + state.RunStateKey)
	if err := sharedRuntimeAcquireWriteMessage(attested.connection, sharedWireMessage{Type: "acquire", ClientKey: clientKey, RequestedAt: time.Now().UTC()}); err != nil {
		return nil, sharedRuntimeError("broker_unreachable", err)
	}
	leaseResponse, err := sharedRuntimeAcquireReadMessage(attested.reader)
	if err != nil {
		return nil, sharedRuntimeError("shared_runtime_unavailable", err)
	}
	if leaseResponse.Type == "refused" {
		return nil, &SharedRuntimeError{Code: leaseResponse.Code, Reason: leaseResponse.Reason, MismatchField: leaseResponse.MismatchField, Details: map[string]any{"effective_max_leases": leaseResponse.EffectiveMaxLeases, "configured_max_leases": leaseResponse.ConfiguredMaxLeases, "broker_pid": leaseResponse.BrokerPID, "broker_start_time": leaseResponse.BrokerStartTime, "resources": leaseResponse.Resources, "configured_sharing": leaseResponse.ConfiguredSharing, "effective_sharing": leaseResponse.EffectiveSharing}, Err: errors.New("broker refused lease")}
	}
	if leaseResponse.Type != "lease" || leaseResponse.Lease == nil || leaseResponse.Runtime == nil ||
		leaseResponse.Lease.LeaseToken == "" || leaseResponse.Lease.LeaseToken == leaseResponse.Lease.LeaseID ||
		leaseResponse.Lease.LeaseLifetime != "client_process" {
		return nil, sharedRuntimeError("protocol_violation", errors.New("lease response is incomplete"))
	}
	_ = attested.connection.SetDeadline(time.Time{})
	closeOnError = false
	return &sharedRuntimeLeaseClient{
		connection: attested.connection, reader: attested.reader, lease: *leaseResponse.Lease,
		broker: attested.broker, runtime: attested.runtime,
		effective: attested.effective, configured: resolved.Sharing,
		closed: make(chan struct{}),
	}, nil
}

// connectAndAttestSharedRuntime is the single production call site for the
// client-side section 7.2 attestation chain. Operator commands deliberately use
// the same chain as lease acquisition before trusting a reachable broker.
func connectAndAttestSharedRuntime(resolved sharedResolvedProfile, state PiStatePaths, runID string, httpClient *http.Client, timeout time.Duration) (*sharedAttestedRuntime, error) {
	return connectAndAttestSharedRuntimeWithOwnership(resolved, state, runID, httpClient, timeout, sharedRuntimeLaunchOwnership{})
}

func connectAndAttestSharedRuntimeWithOwnership(resolved sharedResolvedProfile, state PiStatePaths, runID string, httpClient *http.Client, timeout time.Duration, ownership sharedRuntimeLaunchOwnership) (*sharedAttestedRuntime, error) {
	if resolved.NotRunnableCode != "" {
		return nil, engineCatalogNotRunnableError(resolved)
	}
	system := sharedRuntimeAttestationSystem
	passedGates := make([]SharedRuntimeGateOutcome, 0, 13)
	passed := func(gate string) {
		passedGates = append(passedGates, SharedRuntimeGateOutcome{Gate: gate, Outcome: "passed", Source: "attested"})
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	rawConnection, err := dialer.Dial("unix", resolved.Paths.RendezvousSocket)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, &sharedConnectError{err: err}
		}
		return nil, sharedRuntimeError("broker_unreachable", err)
	}
	connection, ok := rawConnection.(*net.UnixConn)
	if !ok {
		_ = rawConnection.Close()
		return nil, sharedRuntimeError("broker_unreachable", errors.New("rendezvous connection is not AF_UNIX"))
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = connection.Close()
		}
	}()
	reader := bufio.NewReaderSize(connection, sharedRuntimeMaxFrameBytes+1)
	if err := sharedRuntimeAttestationSetDeadline(connection, time.Now().Add(timeout)); err != nil {
		return nil, sharedRuntimeError("broker_unreachable", err)
	}
	uid, peerPID, err := system.peerIdentity(connection)
	if err != nil || uid != uint32(os.Geteuid()) {
		if err == nil {
			err = errors.New("broker peer uid differs")
		}
		return nil, sharedRuntimeError("broker_peer_untrusted", err)
	}
	passed("peer_uid")
	peer, err := system.inspectProcess(peerPID)
	if err != nil || !peer.live() {
		if err == nil {
			err = errors.New("broker peer is a zombie")
		}
		return nil, sharedRuntimeError("broker_identity_unavailable", err)
	}
	passed("peer_pid_liveness")
	_, ownIdentity, err := system.ownExecutableIdentity()
	if err != nil {
		return nil, sharedRuntimeError("broker_identity_unavailable", err)
	}
	peerIdentity, err := system.processExecIdentity(peer)
	if err != nil {
		return nil, sharedRuntimeError("broker_identity_unavailable", err)
	}
	if peerIdentity.Dev != ownIdentity.Dev || peerIdentity.Ino != ownIdentity.Ino {
		return nil, sharedRuntimeError("broker_executable_identity_mismatch", errors.New("broker executable inode differs"))
	}
	passed("broker_executable")
	hello := sharedWireMessage{
		Type: "hello", ProtocolVersion: SharedRuntimeProtocolVersion, ClientPID: os.Getpid(),
		ClientExec: ownIdentity, RuntimeKey: resolved.RuntimeKey, ProfileDigest: resolved.ProfileDigest,
		EngineIdentity: &resolved.EngineIdentity,
		ProjectKey:     state.ProjectStateKey, ProfileKey: state.ProfileStateKey, RunID: runID,
		ConfiguredSharing: &resolved.Sharing,
	}
	if err := sharedRuntimeAttestationWriteMessage(connection, hello); err != nil {
		return nil, sharedRuntimeError("broker_unreachable", err)
	}
	response, err := sharedRuntimeAttestationReadMessage(reader)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
			return nil, sharedRuntimeError("shared_runtime_unavailable", err)
		}
		var networkError net.Error
		if errors.As(err, &networkError) {
			return nil, sharedRuntimeError("broker_unreachable", err)
		}
		return nil, err
	}
	if response.Type == "refused" {
		return nil, &SharedRuntimeError{Code: response.Code, Reason: response.Reason, Details: map[string]any{"effective_max_leases": response.EffectiveMaxLeases, "configured_max_leases": response.ConfiguredMaxLeases, "broker_pid": response.BrokerPID, "broker_start_time": response.BrokerStartTime}, Err: errors.New("broker refused hello")}
	}
	if response.Type != "hello_ok" || response.Broker == nil || response.Runtime == nil || response.EffectiveSharing == nil || response.EngineIdentity == nil {
		return nil, sharedRuntimeError("protocol_violation", errors.New("broker hello response is incomplete"))
	}
	if response.Broker.ExecutableIdentity != ownIdentity {
		return nil, sharedRuntimeError("broker_build_identity_mismatch", errors.New("broker startup binary identity differs"))
	}
	passed("broker_build")
	if response.Broker.PID != peerPID || response.Broker.StartTime != peer.StartTime {
		return nil, sharedRuntimeError("broker_identity_mismatch", errors.New("announced broker identity differs from kernel"))
	}
	passed("broker_start_time")
	if response.ProtocolVersion != SharedRuntimeProtocolVersion {
		return nil, sharedRuntimeError("broker_protocol_version_mismatch", errors.New("broker protocol version differs"))
	}
	passed("protocol_version")
	if response.RuntimeKey != resolved.RuntimeKey {
		return nil, sharedRuntimeError("shared_runtime_identity_mismatch", errors.New("broker runtime key differs"))
	}
	passed("runtime_key")
	if response.EngineIdentity.Key != resolved.EngineIdentity.Key {
		return nil, sharedRuntimeError("shared_runtime_identity_mismatch", errors.New("broker engine identity differs"))
	}
	passed("engine_identity")
	if response.ProfileDigest != resolved.ProfileDigest {
		return nil, sharedRuntimeError("shared_runtime_profile_mismatch", errors.New("broker profile digest differs"))
	}
	passed("profile_digest")
	if response.Runtime.Endpoint != resolved.Profile.BaseURL {
		return nil, sharedRuntimeError("shared_runtime_endpoint_mismatch", errors.New("broker endpoint differs"))
	}
	passed("endpoint")
	runtimeIdentity, err := system.fileIdentity(resolved.Profile.Runtime.Executable)
	if err != nil || runtimeIdentity != response.Runtime.ExecutableIdentity {
		if err == nil {
			err = errors.New("runtime executable identity differs")
		}
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, piError("runtime_executable_invalid", err))
	}
	passed("runtime_executable")
	runtimeProcess, err := system.inspectProcess(response.Runtime.PID)
	if err != nil {
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, sharedRuntimeError("runtime_identity_mismatch", err))
	}
	wantArgv := append([]string{resolved.Profile.Runtime.Executable}, resolved.Profile.Runtime.Argv...)
	if runtimeProcess.UID != uint32(os.Geteuid()) || runtimeProcess.StartTime != response.Runtime.StartTime || runtimeProcess.ExecPath != resolved.Profile.Runtime.Executable || !equalStrings(runtimeProcess.Argv, wantArgv) {
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, sharedRuntimeError("runtime_identity_mismatch", errors.New("kernel runtime identity differs")))
	}
	passed("runtime_process")
	if !runtimeProcess.live() {
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, piError("runtime_exited_early", errors.New("runtime is a zombie")))
	}
	passed("runtime_liveness")
	wantModel := resolved.Profile.Model
	if resolved.Profile.Runtime.DFlash != nil {
		wantModel = resolved.Profile.Runtime.DFlash.TargetModel
	}
	if err := system.checkModel(httpClient, resolved.Profile.BaseURL+resolved.Profile.Runtime.ReadinessPath, wantModel); err != nil {
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, err)
	}
	passed("model_discovery")
	// Envelope shape is a refusal-only gate and adds no attestation outcome;
	// the passed gate set stays frozen.
	if err := validateAttestedHelloEnvelope(response, resolved.Profile.Runtime); err != nil {
		return nil, ownership.stopAfterAttestationFailure(connection, reader, *response.Broker, err)
	}
	closeOnError = false
	return &sharedAttestedRuntime{
		connection: connection,
		reader:     reader,
		broker:     *response.Broker,
		runtime:    *response.Runtime,
		effective:  *response.EffectiveSharing,
		gates:      passedGates,
	}, nil
}

// Cleanup uses the already attested broker connection and launch identity. It
// cannot stop a pre-existing peer or a runtime leased by a concurrent client.
func (ownership sharedRuntimeLaunchOwnership) stopAfterAttestationFailure(connection *net.UnixConn, reader *bufio.Reader, broker SharedBrokerIdentity, cause error) error {
	if !ownership.matches(broker) {
		return cause
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return errors.Join(cause, fmt.Errorf("request broker cleanup after runtime attestation failure: %w", err))
	}
	if err := sharedRuntimeAttestationWriteMessage(connection, sharedWireMessage{Type: "stop", Force: false, TimeoutSeconds: 2}); err != nil {
		return errors.Join(cause, fmt.Errorf("request broker cleanup after runtime attestation failure: %w", err))
	}
	response, err := sharedRuntimeAttestationReadMessage(reader)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("broker cleanup acknowledgement after runtime attestation failure: %w", err))
	}
	if response.Type == "refused" && response.Code == "shared_runtime_leases_active" {
		return cause
	}
	if response.Type != "stopping" {
		return errors.Join(cause, fmt.Errorf("broker did not stop after runtime attestation failure: %s", response.Type))
	}
	return cause
}

type sharedRuntimeAttestationDependencies struct {
	peerIdentity          func(*net.UnixConn) (uint32, int, error)
	inspectProcess        func(int) (sharedProcessObservation, error)
	ownExecutableIdentity func() (string, FileIdentity, error)
	processExecIdentity   func(sharedProcessObservation) (FileIdentity, error)
	fileIdentity          func(string) (FileIdentity, error)
	checkModel            func(*http.Client, string, string) error
}

var sharedRuntimeAttestationSystem = sharedRuntimeAttestationDependencies{
	peerIdentity:          sharedUnixPeerIdentity,
	inspectProcess:        inspectSharedProcess,
	ownExecutableIdentity: ownResolvedExecutableIdentity,
	processExecIdentity:   processExecIdentity,
	fileIdentity:          fileIdentity,
	checkModel:            checkSharedRuntimeModel,
}

var sharedRuntimeStartBroker = startSharedRuntimeBroker
var sharedRuntimeStartOwnExecutableIdentity = ownResolvedExecutableIdentity
var sharedRuntimeStartOpenDevNull = func() (*os.File, error) { return os.Open(os.DevNull) }
var sharedRuntimeAcquireWriteMessage = writeSharedWireMessage
var sharedRuntimeAcquireReadMessage = readSharedWireMessage
var sharedRuntimeAttestationSetDeadline = func(connection *net.UnixConn, deadline time.Time) error {
	return connection.SetDeadline(deadline)
}
var sharedRuntimeAttestationWriteMessage = writeSharedWireMessage
var sharedRuntimeAttestationReadMessage = readSharedWireMessage

func startSharedRuntimeBroker(resolved sharedResolvedProfile, environ []string) (*sharedBrokerChild, error) {
	executable, _, err := sharedRuntimeStartOwnExecutableIdentity()
	if err != nil {
		return nil, sharedRuntimeError("broker_start_failed", err)
	}
	logFile, err := openSharedLog(resolved.Paths.BrokerLog)
	if err != nil {
		return nil, err
	}
	devNull, err := sharedRuntimeStartOpenDevNull()
	if err != nil {
		_ = logFile.Close()
		return nil, sharedRuntimeError("broker_start_failed", err)
	}
	args := []string{
		"runtime", "broker", "--runtime-key", resolved.RuntimeKey,
		"--profile-project", resolved.Project, "--profile", resolved.ProfileName,
		"--home-dir", resolved.HomeDir, "--cache-root", resolved.Paths.CanonicalCacheRoot,
	}
	command := exec.Command(executable, args...)
	command.Dir = resolved.Paths.RuntimeCWD
	command.Stdin = devNull
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = scrubSharedRuntimeEnvironment(environ)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		_ = devNull.Close()
		_ = logFile.Close()
		return nil, sharedRuntimeError("broker_start_failed", err)
	}
	_ = devNull.Close()
	_ = logFile.Close()
	child := &sharedBrokerChild{command: command, done: make(chan error, 1)}
	if observation, err := inspectSharedProcessKernel(command.Process.Pid); err == nil && observation.live() {
		child.ownership = sharedRuntimeLaunchOwnership{brokerPID: command.Process.Pid, brokerStartTime: observation.StartTime}
	}
	go func() { child.done <- command.Wait() }()
	return child, nil
}

func sharedBrokerLogTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	const maximum = 4096
	if len(data) > maximum {
		data = data[len(data)-maximum:]
	}
	return string(data)
}

func checkSharedRuntimeModel(client *http.Client, endpoint, model string) error {
	if client == nil {
		client = &http.Client{
			Timeout:   time.Second,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	response, err := client.Get(endpoint)
	if err != nil {
		return piError("runtime_model_unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != http.StatusOK {
		if err == nil {
			err = fmt.Errorf("readiness status %d", response.StatusCode)
		}
		return piError("runtime_readiness_invalid", err)
	}
	var payload struct {
		Object string `json:"object"`
		Data   *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Object != "list" || payload.Data == nil {
		if err == nil {
			err = errors.New("readiness response is not an OpenAI model list")
		}
		return piError("runtime_readiness_invalid", err)
	}
	if model == "" {
		var servedModel string
		for _, item := range *payload.Data {
			if item.ID == "" {
				continue
			}
			if servedModel != "" && servedModel != item.ID {
				return piError("runtime_readiness_invalid", errors.New("readiness response has multiple served model ids and the catalog does not name one"))
			}
			servedModel = item.ID
		}
		if servedModel == "" {
			return piError("runtime_model_unavailable", errors.New("readiness response contains no non-empty served model id"))
		}
		return nil
	}
	for _, item := range *payload.Data {
		if item.ID == model {
			return nil
		}
	}
	return piError("runtime_model_unavailable", fmt.Errorf("exact model %q is absent", model))
}
