//go:build darwin

package infra

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireSharedPathRefusal(t *testing.T, err error, wantCode string) {
	t.Helper()
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != wantCode {
		t.Fatalf("unsafe shared-runtime path returned %v, want %s", err, wantCode)
	}
}

func TestOpenSharedBrokerLockRejectsUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "valid.lock")
	lock, err := openSharedBrokerLock(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	missingParent := filepath.Join(root, "missing", "broker.lock")
	if _, err := openSharedBrokerLock(missingParent); err == nil {
		t.Fatal("broker lock under absent parent was created")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	symlinkTarget := filepath.Join(root, "target.lock")
	if err := os.WriteFile(symlinkTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.lock")
	if err := os.Symlink(symlinkTarget, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedBrokerLock(symlink); err == nil {
		t.Fatal("broker lock symlink was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	hardlinkTarget := filepath.Join(root, "hardlink-target.lock")
	hardlink := filepath.Join(root, "hardlink.lock")
	if err := os.WriteFile(hardlinkTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(hardlinkTarget, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedBrokerLock(hardlink); err == nil {
		t.Fatal("multiply linked broker lock was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	permissive := filepath.Join(root, "permissive.lock")
	if err := os.WriteFile(permissive, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedBrokerLock(permissive); err == nil {
		t.Fatal("broker lock with permissive mode was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	fifo := filepath.Join(root, "fifo.lock")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if lock, err := openSharedBrokerLock(fifo); err == nil {
		_ = lock.Close()
		t.Fatal("broker FIFO was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	heldPath := filepath.Join(root, "held.lock")
	held, err := openSharedBrokerLock(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Errorf("close held lock: %v", err)
		}
	}()
	if _, err := openSharedBrokerLock(heldPath); err == nil {
		t.Fatal("second exclusive broker lock holder was admitted")
	} else {
		var shared *SharedRuntimeError
		if !errors.As(err, &shared) || shared.Code != "broker_election_lost" {
			t.Fatalf("held lock returned %v, want broker_election_lost", err)
		}
	}
}

func TestOpenSharedLogRejectsUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	symlinkTarget := filepath.Join(root, "target.log")
	if err := os.WriteFile(symlinkTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.log")
	if err := os.Symlink(symlinkTarget, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedLog(symlink); err == nil {
		t.Fatal("managed log symlink was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	hardlinkTarget := filepath.Join(root, "hardlink-target.log")
	hardlink := filepath.Join(root, "hardlink.log")
	if err := os.WriteFile(hardlinkTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(hardlinkTarget, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedLog(hardlink); err == nil {
		t.Fatal("multiply linked managed log was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	permissive := filepath.Join(root, "permissive.log")
	if err := os.WriteFile(permissive, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openSharedLog(permissive); err == nil {
		t.Fatal("managed log with permissive mode was admitted")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}

	fifo := filepath.Join(root, "fifo.log")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		file, err := openSharedLog(fifo)
		if file != nil {
			_ = file.Close()
		}
		result <- err
	}()
	readerFD, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.Close(readerFD); err != nil {
			t.Errorf("close FIFO reader: %v", err)
		}
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO managed log was admitted")
		}
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	case <-time.After(2 * time.Second):
		t.Fatal("openSharedLog did not return after a FIFO reader connected")
	}
}

func TestReclaimSharedRuntimeRefusesUnknownKernelIdentity(t *testing.T) {
	original := sharedRuntimeReclaimSystem
	sharedRuntimeReclaimSystem = original
	killed := false
	record := &SharedRuntimeProcessRecord{
		PID: 12345, PGID: 12345, StartTime: ProcessStartTime{Seconds: 1},
		PreExec: ProcessExecShape{ExecPath: "/fixture/runtime", Argv: []string{"runtime", "serve"}},
	}
	sharedRuntimeReclaimSystem.inspectKernel = func(int) (sharedProcessObservation, error) {
		return sharedProcessObservation{PID: record.PID, UID: uint32(os.Geteuid()), StartTime: record.StartTime, PStat: 1}, os.ErrPermission
	}
	sharedRuntimeReclaimSystem.inspectProcess = func(int) (sharedProcessObservation, error) {
		return sharedProcessObservation{PID: record.PID, PGID: record.PGID, UID: uint32(os.Geteuid()), StartTime: record.StartTime, PStat: 1, ExecPath: record.PreExec.ExecPath, Argv: record.PreExec.Argv}, nil
	}
	sharedRuntimeReclaimSystem.kill = func(int, syscall.Signal) error { killed = true; return nil }
	sharedRuntimeReclaimSystem.waitGone = func(*SharedRuntimeProcessRecord, time.Duration) error { return nil }
	sharedRuntimeReclaimSystem.waitEndpointFree = func(string, time.Duration) error { return nil }
	t.Cleanup(func() { sharedRuntimeReclaimSystem = original })
	err := reclaimSharedRuntime(record, 1)
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_orphan_unidentifiable" {
		t.Fatalf("unknown kernel identity returned %v, want shared_runtime_orphan_unidentifiable", err)
	}
	if killed {
		t.Fatal("unknown kernel identity reached process signaling")
	}
}

func TestReleaseSharedRuntimeLeaseRejectsMissingToken(t *testing.T) {
	err := ReleaseSharedRuntimeLease(SharedRuntimeOperatorOptions{}, "")
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "protocol_violation" {
		t.Fatalf("empty bearer token returned %v, want protocol_violation", err)
	}
}

func reclaimRefusalFixture() (*SharedRuntimeProcessRecord, sharedProcessObservation) {
	shape := ProcessExecShape{ExecPath: "/fixture/runtime", Argv: []string{"/fixture/runtime", "serve"}}
	record := &SharedRuntimeProcessRecord{
		PID: 12345, PGID: 12345, UID: uint32(os.Geteuid()), StartTime: ProcessStartTime{Seconds: 11, Microseconds: 17},
		PreExec: shape, PostExec: shape, Endpoint: "http://127.0.0.1:1/v1",
	}
	observation := sharedProcessObservation{
		PID: record.PID, PGID: record.PGID, UID: record.UID, StartTime: record.StartTime,
		PStat: 1, ExecPath: shape.ExecPath, Argv: append([]string(nil), shape.Argv...),
	}
	return record, observation
}

func TestReclaimSharedRuntimeRefusesProcessInspectionFailure(t *testing.T) {
	original := sharedRuntimeReclaimSystem
	record, observation := reclaimRefusalFixture()
	killed := false
	sharedRuntimeReclaimSystem = original
	sharedRuntimeReclaimSystem.inspectKernel = func(int) (sharedProcessObservation, error) { return observation, nil }
	sharedRuntimeReclaimSystem.inspectProcess = func(int) (sharedProcessObservation, error) { return observation, os.ErrPermission }
	sharedRuntimeReclaimSystem.kill = func(int, syscall.Signal) error { killed = true; return nil }
	t.Cleanup(func() { sharedRuntimeReclaimSystem = original })
	err := reclaimSharedRuntime(record, 1)
	requireSharedPathRefusal(t, err, "shared_runtime_orphan_unidentifiable")
	if killed {
		t.Fatal("process inspection failure reached process signaling")
	}
}

func TestReclaimSharedRuntimeRefusesUnknownExecutableShapeBeforeSignal(t *testing.T) {
	for _, member := range []string{"pre-exec", "post-exec"} {
		t.Run(member, func(t *testing.T) {
			original := sharedRuntimeReclaimSystem
			record, observation := reclaimRefusalFixture()
			record.PreExec = ProcessExecShape{ExecPath: "/forged/pre", Argv: []string{"/forged/pre"}}
			record.PostExec = ProcessExecShape{ExecPath: "/forged/post", Argv: []string{"/forged/post"}}
			killed := false
			sharedRuntimeReclaimSystem = original
			sharedRuntimeReclaimSystem.inspectKernel = func(int) (sharedProcessObservation, error) { return observation, nil }
			sharedRuntimeReclaimSystem.inspectProcess = func(int) (sharedProcessObservation, error) { return observation, nil }
			sharedRuntimeReclaimSystem.kill = func(int, syscall.Signal) error { killed = true; return nil }
			sharedRuntimeReclaimSystem.waitGone = func(*SharedRuntimeProcessRecord, time.Duration) error { return nil }
			sharedRuntimeReclaimSystem.waitEndpointFree = func(string, time.Duration) error { return nil }
			t.Cleanup(func() { sharedRuntimeReclaimSystem = original })
			err := reclaimSharedRuntime(record, 1)
			requireSharedPathRefusal(t, err, "shared_runtime_orphan_unidentifiable")
			if killed {
				t.Fatalf("mismatched %s shape reached process signaling", member)
			}
		})
	}
}

func TestReclaimSharedRuntimeSignalsOnlyAfterMatchedPositiveKernelPGID(t *testing.T) {
	original := sharedRuntimeReclaimSystem
	record, observation := reclaimRefusalFixture()
	killedGroup := 0
	sharedRuntimeReclaimSystem = original
	sharedRuntimeReclaimSystem.inspectKernel = func(int) (sharedProcessObservation, error) { return observation, nil }
	sharedRuntimeReclaimSystem.inspectProcess = func(int) (sharedProcessObservation, error) { return observation, nil }
	sharedRuntimeReclaimSystem.kill = func(group int, _ syscall.Signal) error { killedGroup = group; return nil }
	sharedRuntimeReclaimSystem.waitGone = func(*SharedRuntimeProcessRecord, time.Duration) error { return nil }
	sharedRuntimeReclaimSystem.waitEndpointFree = func(string, time.Duration) error { return nil }
	t.Cleanup(func() { sharedRuntimeReclaimSystem = original })
	if record.PGID <= 0 || observation.PGID != record.PGID {
		t.Fatalf("fixture PGID record=%d kernel=%d", record.PGID, observation.PGID)
	}
	err := reclaimSharedRuntime(record, 1)
	if sharedRuntimeErrorCode(err) == "shared_runtime_orphan_unidentifiable" {
		t.Fatalf("matched live runtime returned shared_runtime_orphan_unidentifiable: %v", err)
	}
	if err != nil {
		t.Fatalf("matched live runtime refusal: %v", err)
	}
	if killedGroup != -record.PGID {
		t.Fatalf("signal group=%d want=%d", killedGroup, -record.PGID)
	}
}

func TestWaitRecordedRuntimeGoneDistinguishesKernelFailureAndTimeout(t *testing.T) {
	original := sharedRuntimeWaitRecordedInspectKernel
	t.Cleanup(func() { sharedRuntimeWaitRecordedInspectKernel = original })
	record, observation := reclaimRefusalFixture()
	t.Run("kernel inspection error", func(t *testing.T) {
		sharedRuntimeWaitRecordedInspectKernel = func(int) (sharedProcessObservation, error) { return observation, os.ErrPermission }
		err := waitRecordedRuntimeGone(record, 0)
		requireSharedPathRefusal(t, err, "shared_runtime_orphan_unidentifiable")
	})
	t.Run("live process timeout", func(t *testing.T) {
		calls := 0
		sharedRuntimeWaitRecordedInspectKernel = func(int) (sharedProcessObservation, error) {
			calls++
			if calls == 1 {
				return observation, nil
			}
			observation.PStat = darwinProcessStateZombie
			return observation, nil
		}
		err := waitRecordedRuntimeGone(record, 0)
		if sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" {
			t.Fatalf("live process timeout returned %v, want runtime_shutdown_timeout", err)
		}
	})
}

func TestWaitSharedEndpointFreeReturnsTimeoutForOccupiedEndpoint(t *testing.T) {
	original := sharedRuntimeWaitEndpointPreflight
	calls := 0
	sharedRuntimeWaitEndpointPreflight = func(string) error {
		calls++
		if calls == 1 {
			return syscall.EADDRINUSE
		}
		return nil
	}
	t.Cleanup(func() { sharedRuntimeWaitEndpointPreflight = original })
	err := waitSharedEndpointFree("http://127.0.0.1:1/v1", 0)
	if sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" {
		t.Fatalf("occupied endpoint returned %v, want runtime_shutdown_timeout", err)
	}
}

func TestRemoveStaleRendezvousRefusesNonEmptyDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rendezvous")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := removeStaleRendezvous(path)
	requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
}

func TestCleanupSharedRuntimeStateRefusesNonEmptyStatePath(t *testing.T) {
	_, _, _, resolved := newSharedIntegrationProfile(t)
	if err := os.Mkdir(resolved.Paths.BrokerState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolved.Paths.BrokerState, "child"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cleanupSharedRuntimeState(resolved.Paths)
	requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
}
