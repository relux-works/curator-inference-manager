//go:build !darwin

package infra

import (
	"context"
	"errors"
	"testing"
)

func TestAcquireSharedRuntimeLeaseRejectsUnsupportedPlatform(t *testing.T) {
	_, err := AcquireSharedRuntimeLease(context.Background(), SharedRuntimeOperatorOptions{}, "fixture-run", nil)
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("unsupported acquire returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestReleaseSharedRuntimeLeaseRejectsUnsupportedPlatform(t *testing.T) {
	err := ReleaseSharedRuntimeLease(SharedRuntimeOperatorOptions{}, "fixture-token")
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("unsupported release returned %v, want shared_runtime_platform_unsupported", err)
	}
}
