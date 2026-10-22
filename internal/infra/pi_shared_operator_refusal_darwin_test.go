//go:build darwin

package infra

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopSharedRuntimeRefusesWireAndResponseFailures(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		site     string
		writeErr error
		readErr  error
		response sharedWireMessage
		wantCode string
	}{
		{name: "write failure", writeErr: errors.New("write failure"), response: sharedWireMessage{Type: "stopping"}, wantCode: "broker_unreachable"},
		{name: "read failure", response: sharedWireMessage{}, readErr: errors.New("read failure"), wantCode: "broker_unreachable"},
		{name: "unexpected response", response: sharedWireMessage{Type: "unexpected"}, wantCode: "protocol_violation"},
		{name: "refused response", site: "broker refused stop", response: sharedWireMessage{Type: "refused", Code: "runtime_stop_refused", Reason: "active_leases"}, wantCode: "runtime_stop_refused"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			served := fixture.answerOnce(t, nil)
			oldWrite, oldRead := sharedRuntimeStopWriteMessage, sharedRuntimeStopReadMessage
			oldWaitBroker, oldWaitRuntime, oldWaitEndpoint := sharedRuntimeStopWaitBrokerGone, sharedRuntimeStopWaitRuntimeGone, sharedRuntimeStopWaitEndpointFree
			t.Cleanup(func() {
				sharedRuntimeStopWriteMessage, sharedRuntimeStopReadMessage = oldWrite, oldRead
				sharedRuntimeStopWaitBrokerGone, sharedRuntimeStopWaitRuntimeGone, sharedRuntimeStopWaitEndpointFree = oldWaitBroker, oldWaitRuntime, oldWaitEndpoint
			})
			sharedRuntimeStopWriteMessage = func(io.Writer, sharedWireMessage) error { return testCase.writeErr }
			sharedRuntimeStopReadMessage = func(io.Reader) (sharedWireMessage, error) {
				return testCase.response, testCase.readErr
			}
			sharedRuntimeStopWaitBrokerGone = func(SharedBrokerIdentity, time.Time) error { return nil }
			sharedRuntimeStopWaitRuntimeGone = func(*SharedRuntimeProcessRecord, time.Duration) error { return nil }
			sharedRuntimeStopWaitEndpointFree = func(string, time.Duration) error { return nil }
			options := sharedRuntimeOperatorOptionsForFixture(fixture)
			_, err := StopSharedRuntime(options, false, time.Second)
			requireSharedAttestationCode(t, err, testCase.wantCode)
			if serverErr := <-served; serverErr != nil {
				t.Fatalf("attestation exchange: %v", serverErr)
			}
		})
	}
}

func TestStopSharedRuntimeChecksBothShutdownDeadlines(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		remaining []time.Duration
	}{
		{name: "before runtime verification", remaining: []time.Duration{0, time.Second}},
		{name: "before endpoint verification", remaining: []time.Duration{time.Second, 0}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			served := fixture.answerOnce(t, nil)
			oldWrite, oldRead := sharedRuntimeStopWriteMessage, sharedRuntimeStopReadMessage
			oldWaitBroker, oldWaitRuntime, oldWaitEndpoint := sharedRuntimeStopWaitBrokerGone, sharedRuntimeStopWaitRuntimeGone, sharedRuntimeStopWaitEndpointFree
			oldRemaining := sharedRuntimeStopTimeRemaining
			t.Cleanup(func() {
				sharedRuntimeStopWriteMessage, sharedRuntimeStopReadMessage = oldWrite, oldRead
				sharedRuntimeStopWaitBrokerGone, sharedRuntimeStopWaitRuntimeGone, sharedRuntimeStopWaitEndpointFree = oldWaitBroker, oldWaitRuntime, oldWaitEndpoint
				sharedRuntimeStopTimeRemaining = oldRemaining
			})
			sharedRuntimeStopWriteMessage = func(io.Writer, sharedWireMessage) error { return nil }
			sharedRuntimeStopReadMessage = func(io.Reader) (sharedWireMessage, error) { return sharedWireMessage{Type: "stopping"}, nil }
			sharedRuntimeStopWaitBrokerGone = func(SharedBrokerIdentity, time.Time) error { return nil }
			sharedRuntimeStopWaitRuntimeGone = func(*SharedRuntimeProcessRecord, time.Duration) error { return nil }
			sharedRuntimeStopWaitEndpointFree = func(string, time.Duration) error { return nil }
			checks := 0
			sharedRuntimeStopTimeRemaining = func(time.Time) time.Duration {
				remaining := testCase.remaining[checks]
				checks++
				return remaining
			}
			_, err := StopSharedRuntime(sharedRuntimeOperatorOptionsForFixture(fixture), false, time.Second)
			requireSharedAttestationCode(t, err, "runtime_shutdown_timeout")
			if serverErr := <-served; serverErr != nil {
				t.Fatalf("attestation exchange: %v", serverErr)
			}
		})
	}
}

func TestStopSharedRuntimeRefusesInvalidTimeoutAndRequiresForceWhenBrokerIsAbsent(t *testing.T) {
	if _, err := StopSharedRuntime(SharedRuntimeOperatorOptions{}, false, 0); sharedRuntimeErrorCode(err) != "invalid_runtime_stop_timeout" {
		t.Fatalf("zero timeout refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := StopSharedRuntime(sharedRuntimeOperatorOptionsForFixture(fixture), false, time.Second)
	requireSharedAttestationCode(t, err, "shared_runtime_broker_unreachable_use_force")
}

func TestStopSharedRuntimeForceStopRefusesHeldUnrecordedOwner(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	if err := fixture.listener.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := openSharedBrokerLock(fixture.resolved.Paths.BrokerLock)
	if err != nil {
		t.Fatal(err)
	}
	held, acquired, probeErr := probeSharedBrokerLock(fixture.resolved.Paths)
	if probeErr != nil || !held || acquired != nil {
		t.Fatalf("held broker lock probe: held=%v acquired=%v err=%v", held, acquired != nil, probeErr)
	}
	closed := false
	oldNow, oldSleep := sharedRuntimeForceStopNow, sharedRuntimeForceStopSleep
	t.Cleanup(func() {
		sharedRuntimeForceStopNow, sharedRuntimeForceStopSleep = oldNow, oldSleep
		if !closed {
			_ = lock.Close()
		}
	})
	base := time.Now()
	nowCalls := 0
	sharedRuntimeForceStopNow = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return base
		}
		return base.Add(2 * time.Second)
	}
	sharedRuntimeForceStopSleep = func(time.Duration) {
		if !closed {
			closed = true
			_ = lock.Close()
		}
	}
	result, err := StopSharedRuntime(sharedRuntimeOperatorOptionsForFixture(fixture), true, time.Second)
	if err == nil && result.State != "absent" {
		t.Fatalf("force stop state=%q", result.State)
	}
	requireSharedAttestationCode(t, err, "shared_runtime_owner_unidentifiable")
}

func sharedRuntimeOperatorOptionsForFixture(fixture sharedAttestationFixture) SharedRuntimeOperatorOptions {
	return SharedRuntimeOperatorOptions{
		ProjectDir: fixture.resolved.Project,
		HomeDir:    fixture.resolved.HomeDir,
		CacheRoot:  fixture.resolved.Paths.CanonicalCacheRoot,
		Profile:    fixture.resolved.ProfileName,
	}
}

func TestReadSharedLeaseMirrorsRefusesDirectoryFileAndJSONFailures(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSharedLeaseMirrors(notDirectory); sharedRuntimeErrorCode(err) != "shared_runtime_state_unreadable" {
		t.Fatalf("directory read error code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
	for _, testCase := range []struct {
		name string
		make func(string) error
	}{
		{name: "file read", make: func(path string) error { return os.Symlink(filepath.Join(root, "missing-target"), path) }},
		{name: "malformed JSON", make: func(path string) error { return os.WriteFile(path, []byte(`{"lease_id":`), 0o600) }},
		{name: "wrong-typed JSON", make: func(path string) error { return os.WriteFile(path, []byte(`{"lease_id":17}`), 0o600) }},
		{name: "trailing JSON", make: func(path string) error { return os.WriteFile(path, []byte(`{} {}`), 0o600) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := testCase.make(filepath.Join(directory, "lease.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := readSharedLeaseMirrors(directory); sharedRuntimeErrorCode(err) != "shared_runtime_state_unreadable" {
				t.Fatalf("lease mirror refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
			}
		})
	}
}

func TestProbeSharedBrokerLockRefusesInvalidRootPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := probeSharedBrokerLock(SharedRuntimePaths{Root: root, BrokerLock: filepath.Join(root, "broker.lock")})
	requireSharedAttestationCode(t, err, "shared_runtime_state_path_invalid")

	rootDir := t.TempDir()
	loop := filepath.Join(rootDir, "root-loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	_, acquired, err := probeSharedBrokerLock(SharedRuntimePaths{Root: loop, BrokerLock: filepath.Join(rootDir, "broker.lock")})
	if acquired != nil {
		_ = acquired.Close()
		t.Fatal("invalid root unexpectedly acquired broker lock")
	}
	requireSharedAttestationCode(t, err, "shared_runtime_state_path_invalid")
}

func TestSharedRuntimeBrokerCandidatesRefuseSystemReadFailures(t *testing.T) {
	oldProcesses, oldIdentity := sharedRuntimeCandidateProcesses, sharedRuntimeCandidateOwnIdentity
	t.Cleanup(func() { sharedRuntimeCandidateProcesses, sharedRuntimeCandidateOwnIdentity = oldProcesses, oldIdentity })
	sharedRuntimeCandidateProcesses = func() ([]unix.KinfoProc, error) { return nil, os.ErrPermission }
	if _, err := sharedRuntimeBrokerCandidates(sharedResolvedProfile{}); sharedRuntimeErrorCode(err) != "broker_identity_unavailable" {
		t.Fatalf("process list error code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
	sharedRuntimeCandidateProcesses = func() ([]unix.KinfoProc, error) { return nil, nil }
	sharedRuntimeCandidateOwnIdentity = func() (string, FileIdentity, error) { return "", FileIdentity{}, os.ErrPermission }
	if _, err := sharedRuntimeBrokerCandidates(sharedResolvedProfile{}); sharedRuntimeErrorCode(err) != "broker_identity_unavailable" {
		t.Fatalf("own executable error code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestWaitRecordedBrokerGoneRefusesInspectionFailureAndDeadline(t *testing.T) {
	oldInspect, oldNow, oldSleep := sharedRuntimeWaitBrokerInspectKernel, sharedRuntimeForceStopNow, sharedRuntimeForceStopSleep
	t.Cleanup(func() {
		sharedRuntimeWaitBrokerInspectKernel, sharedRuntimeForceStopNow, sharedRuntimeForceStopSleep = oldInspect, oldNow, oldSleep
	})
	record := SharedBrokerIdentity{PID: 321, StartTime: ProcessStartTime{Seconds: 7}}
	sharedRuntimeWaitBrokerInspectKernel = func(int) (sharedProcessObservation, error) {
		return sharedProcessObservation{PID: record.PID, PStat: 1, StartTime: record.StartTime}, os.ErrPermission
	}
	if err := waitRecordedBrokerGone(record, time.Now().Add(time.Second)); sharedRuntimeErrorCode(err) != "broker_stop_identity_mismatch" {
		t.Fatalf("inspection failure code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
	base := time.Now()
	calls := 0
	sharedRuntimeWaitBrokerInspectKernel = func(int) (sharedProcessObservation, error) {
		calls++
		if calls == 1 {
			return sharedProcessObservation{PID: record.PID, PStat: 1, StartTime: record.StartTime}, nil
		}
		return sharedProcessObservation{}, syscall.ESRCH
	}
	sharedRuntimeForceStopNow = func() time.Time { return base.Add(2 * time.Second) }
	sharedRuntimeForceStopSleep = func(time.Duration) {}
	if err := waitRecordedBrokerGone(record, base.Add(time.Second)); sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" {
		t.Fatalf("deadline refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestStopRecordedSharedRuntimeRequiresBrokerLockBeforeCleanup(t *testing.T) {
	_, _, _, resolved := newSharedIntegrationProfile(t)
	lock, err := openSharedBrokerLock(resolved.Paths.BrokerLock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	identity, err := fileIdentity(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	start := ProcessStartTime{Seconds: 19}
	observation := sharedProcessObservation{
		PID: 1001, PGID: 1001, SID: 1001, UID: uint32(os.Geteuid()), StartTime: start,
		PStat: 1, ExecPath: os.Args[0], Argv: []string{os.Args[0], "runtime", "broker"},
	}
	record := SharedBrokerRecord{Broker: SharedBrokerIdentity{
		PID: observation.PID, PGID: observation.PGID, SID: observation.SID, UID: observation.UID,
		StartTime: start, ExecPath: observation.ExecPath, Argv: observation.Argv, ExecutableIdentity: identity,
	}}
	system := sharedRuntimeOperatorDependencies{
		inspectProcess:        func(int) (sharedProcessObservation, error) { return observation, nil },
		ownExecutableIdentity: func() (string, FileIdentity, error) { return observation.ExecPath, identity, nil },
		processExecIdentity:   func(sharedProcessObservation) (FileIdentity, error) { return identity, nil },
		kill:                  func(int, syscall.Signal) error { return nil },
	}
	oldInspect := sharedRuntimeWaitBrokerInspectKernel
	sharedRuntimeWaitBrokerInspectKernel = func(int) (sharedProcessObservation, error) { return sharedProcessObservation{}, syscall.ESRCH }
	t.Cleanup(func() { sharedRuntimeWaitBrokerInspectKernel = oldInspect })
	_, err = stopRecordedSharedRuntimeWithDependencies(resolved, &record, time.Now().Add(-time.Second), system)
	requireSharedAttestationCode(t, err, "runtime_shutdown_timeout")
	if _, err := os.Stat(resolved.Paths.Root); err != nil {
		t.Fatalf("held broker lock refusal removed runtime state root: %v", err)
	}
}

// Identity failures, including code mismatch/absence, cannot signal or remove state.
func TestStopRecordedSharedRuntimeWithDependenciesRefusesEveryIdentityMismatchBeforeSignal(t *testing.T) {
	tests := []struct {
		name, site            string
		inspectErr, ownErr    error
		processIdentityErr    error
		mutateObservation     func(*sharedProcessObservation)
		mutateProcessIdentity func(*FileIdentity)
	}{
		{name: "process inspection error", site: "broker_stop_identity_mismatch", inspectErr: errors.New("process inspection failed")},
		{name: "process not live", site: "broker_stop_identity_mismatch", mutateObservation: func(observation *sharedProcessObservation) { observation.PStat = darwinProcessStateZombie }},
		{name: "uid differs", site: "broker_stop_identity_mismatch", mutateObservation: func(observation *sharedProcessObservation) { observation.UID++ }},
		{name: "start time differs", site: "broker_stop_identity_mismatch", mutateObservation: func(observation *sharedProcessObservation) { observation.StartTime.Seconds++ }},
		{name: "argv differs", site: "broker_stop_identity_mismatch", mutateObservation: func(observation *sharedProcessObservation) { observation.Argv = append(observation.Argv, "--other") }},
		{name: "own executable inspection error", site: "broker_stop_identity_mismatch", ownErr: errors.New("own executable inspection failed")},
		{name: "process executable inspection error", site: "broker_stop_identity_mismatch", processIdentityErr: errors.New("process executable inspection failed")},
		{name: "code identity differs", site: "broker_stop_identity_mismatch", processIdentityErr: validateSharedCodeHashes(sharedCodeHash{1}, sharedCodeHash{2})},
		{name: "code identity absent", site: "broker_stop_identity_mismatch", processIdentityErr: validateSharedCodeHashes(sharedCodeHash{1}, sharedCodeHash{})},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, resolved := newSharedIntegrationProfile(t)
			identity, err := fileIdentity(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			observation := sharedProcessObservation{
				PID: 1001, PGID: 1001, SID: 1001, UID: uint32(os.Geteuid()),
				StartTime: ProcessStartTime{Seconds: 19}, PStat: 1, ExecPath: os.Args[0],
				Argv: []string{os.Args[0], "runtime", "broker"},
			}
			if testCase.mutateObservation != nil {
				testCase.mutateObservation(&observation)
			}
			processIdentity := identity
			if testCase.mutateProcessIdentity != nil {
				testCase.mutateProcessIdentity(&processIdentity)
			}
			record := SharedBrokerRecord{Broker: SharedBrokerIdentity{
				PID: observation.PID, PGID: 1001, SID: 1001, UID: uint32(os.Geteuid()),
				StartTime: ProcessStartTime{Seconds: 19}, ExecPath: os.Args[0], Argv: []string{os.Args[0], "runtime", "broker"},
				ExecutableIdentity: identity,
			}}
			killCalls := 0
			system := sharedRuntimeOperatorDependencies{
				inspectProcess: func(int) (sharedProcessObservation, error) { return observation, testCase.inspectErr },
				ownExecutableIdentity: func() (string, FileIdentity, error) {
					return os.Args[0], identity, testCase.ownErr
				},
				processExecIdentity: func(sharedProcessObservation) (FileIdentity, error) {
					return processIdentity, testCase.processIdentityErr
				},
				kill: func(int, syscall.Signal) error { killCalls++; return nil },
			}
			_, err = stopRecordedSharedRuntimeWithDependencies(resolved, &record, time.Now().Add(time.Second), system)
			requireSharedAttestationCode(t, err, "broker_stop_identity_mismatch")
			if killCalls != 0 {
				t.Fatalf("identity refusal sent %d signals", killCalls)
			}
			if _, err := os.Stat(resolved.Paths.Root); err != nil {
				t.Fatalf("identity refusal removed runtime state root: %v", err)
			}
		})
	}
}

func TestStopRecordedSharedRuntimeWithDependenciesRefusesHeldBrokerLockWithoutCleanup(t *testing.T) {
	_, _, _, resolved := newSharedIntegrationProfile(t)
	lock, err := openSharedBrokerLock(resolved.Paths.BrokerLock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	identity, err := fileIdentity(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	start := ProcessStartTime{Seconds: 19}
	observation := sharedProcessObservation{
		PID: 1001, PGID: 1001, SID: 1001, UID: uint32(os.Geteuid()), StartTime: start,
		PStat: 1, ExecPath: os.Args[0], Argv: []string{os.Args[0], "runtime", "broker"},
	}
	record := SharedBrokerRecord{Broker: SharedBrokerIdentity{
		PID: observation.PID, PGID: observation.PGID, SID: observation.SID, UID: observation.UID,
		StartTime: start, ExecPath: observation.ExecPath, Argv: observation.Argv, ExecutableIdentity: identity,
	}}
	oldInspect := sharedRuntimeWaitBrokerInspectKernel
	sharedRuntimeWaitBrokerInspectKernel = func(int) (sharedProcessObservation, error) { return sharedProcessObservation{}, syscall.ESRCH }
	t.Cleanup(func() { sharedRuntimeWaitBrokerInspectKernel = oldInspect })
	system := sharedRuntimeOperatorDependencies{
		inspectProcess:        func(int) (sharedProcessObservation, error) { return observation, nil },
		ownExecutableIdentity: func() (string, FileIdentity, error) { return observation.ExecPath, identity, nil },
		processExecIdentity:   func(sharedProcessObservation) (FileIdentity, error) { return identity, nil },
		kill:                  func(int, syscall.Signal) error { return nil },
	}
	_, err = stopRecordedSharedRuntimeWithDependencies(resolved, &record, time.Now().Add(-time.Second), system)
	requireSharedAttestationCode(t, err, "runtime_shutdown_timeout")
}

func TestPreflightPiListenerRefusesMalformedAndOccupiedEndpoints(t *testing.T) {
	for _, endpoint := range []string{":", "http:///v1/models", "http://127.0.0.1:99999/v1/models"} {
		if err := preflightPiListener(endpoint); sharedRuntimeErrorCode(err) != "runtime_listener_check_failed" {
			t.Fatalf("endpoint %q refusal code=%q err=%v", endpoint, sharedRuntimeErrorCode(err), err)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close fixture listener: %v", err)
		}
	}()
	endpoint := "http://" + listener.Addr().String() + "/v1/models"
	if err := preflightPiListener(endpoint); sharedRuntimeErrorCode(err) != "runtime_listener_occupied" {
		t.Fatalf("occupied endpoint refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}
