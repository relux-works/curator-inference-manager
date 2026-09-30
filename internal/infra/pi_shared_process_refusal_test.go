//go:build !windows

package infra

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestTerminateProcessGroupWithSignalReportsForcedKill(t *testing.T) {
	oldSignal, oldWaitGone, oldDrain, oldNow := sharedRuntimeProcessGroupSignal, sharedRuntimeProcessGroupWaitGone, sharedRuntimeProcessDrain, sharedRuntimeProcessNow
	t.Cleanup(func() {
		sharedRuntimeProcessGroupSignal, sharedRuntimeProcessGroupWaitGone, sharedRuntimeProcessDrain, sharedRuntimeProcessNow = oldSignal, oldWaitGone, oldDrain, oldNow
	})
	sharedRuntimeProcessGroupSignal = func(int, syscall.Signal) error { return nil }
	sharedRuntimeProcessGroupWaitGone = func(int, time.Duration) error { return nil }
	sharedRuntimeProcessDrain = func(*piProcessWait, time.Duration) error { return nil }
	base := time.Now()
	calls := 0
	sharedRuntimeProcessNow = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(time.Second)
	}
	err := terminateProcessGroupWithSignal(55, &piProcessWait{done: make(chan struct{})}, syscall.SIGTERM, 0)
	if sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" {
		t.Fatalf("forced-kill result code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestWaitForPiProcessDrainRefusesUndrainedChild(t *testing.T) {
	wait := &piProcessWait{done: make(chan struct{})}
	err := waitForPiProcessDrain(wait, time.Millisecond)
	if sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" {
		t.Fatalf("undrained child result code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestWaitProcessGroupGoneRefusesGroupAtExpiredDeadline(t *testing.T) {
	oldSignal, oldNow, oldSleep := sharedRuntimeProcessGroupSignal, sharedRuntimeProcessNow, sharedRuntimeProcessSleep
	t.Cleanup(func() {
		sharedRuntimeProcessGroupSignal, sharedRuntimeProcessNow, sharedRuntimeProcessSleep = oldSignal, oldNow, oldSleep
	})
	calls := 0
	sharedRuntimeProcessGroupSignal = func(int, syscall.Signal) error {
		calls++
		if calls == 1 {
			return nil
		}
		return syscall.ESRCH
	}
	base := time.Now()
	nowCalls := 0
	sharedRuntimeProcessNow = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return base
		}
		return base.Add(time.Second)
	}
	sharedRuntimeProcessSleep = func(time.Duration) {}
	err := waitProcessGroupGone(77, 0)
	if sharedRuntimeErrorCode(err) != "runtime_shutdown_timeout" && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("expired process group result code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}
