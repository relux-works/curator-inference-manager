//go:build !darwin

package infra

import "context"

func AcquireSharedRuntimeLease(context.Context, SharedRuntimeOperatorOptions, string, []string) (*SharedRuntimeLease, error) {
	return nil, unsupportedSharedRuntimePlatform()
}

func ReleaseSharedRuntimeLease(SharedRuntimeOperatorOptions, string) error {
	return unsupportedSharedRuntimePlatform()
}
