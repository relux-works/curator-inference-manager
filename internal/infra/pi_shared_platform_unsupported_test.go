//go:build !darwin

package infra

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSharedRuntimePublicOperationsRefuseUnsupportedPlatforms(t *testing.T) {
	options := SharedRuntimeOperatorOptions{}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "broker", run: func() error { return RunSharedRuntimeBroker(SharedRuntimeBrokerOptions{}) }},
		{name: "launcher", run: func() error { return RunSharedRuntimeLauncher(SharedRuntimeLauncherOptions{}) }},
		{name: "status", run: func() error { _, err := SharedRuntimeStatusReport(options); return err }},
		{name: "stop", run: func() error { _, err := StopSharedRuntime(options, false, time.Second); return err }},
		{name: "quarantine", run: func() error { _, err := SetSharedRuntimeManualQuarantine(options, true); return err }},
		{name: "acquire_lease", run: func() error {
			_, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-unsupported", nil)
			return err
		}},
		{name: "release_lease", run: func() error { return ReleaseSharedRuntimeLease(options, "lease-token") }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			var shared *SharedRuntimeError
			if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
				t.Fatalf("operation returned %v, want shared_runtime_platform_unsupported", err)
			}
		})
	}
}
