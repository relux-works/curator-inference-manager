//go:build darwin

package infra

import (
	"context"
	"errors"
	"os"
	"time"
)

// AcquireSharedRuntimeLease starts the extracted broker when needed and returns
// only after the broker has granted a process-bound lease.
func AcquireSharedRuntimeLease(ctx context.Context, options SharedRuntimeOperatorOptions, runID string, environ []string) (*SharedRuntimeLease, error) {
	resolved, state, err := resolveSharedRuntimeOperator(options)
	if err != nil {
		return nil, err
	}
	if err := CreateSharedRuntimeTree(resolved.Paths); err != nil {
		return nil, err
	}
	if environ == nil {
		environ = os.Environ()
	}
	client, err := acquireSharedRuntimeLease(resolved, state, runID, environ, options.HTTPClient, ctx)
	if err != nil {
		return nil, err
	}
	return &SharedRuntimeLease{
		record:  client.lease,
		monitor: client.monitor,
		close:   client.close,
	}, nil
}

// ReleaseSharedRuntimeLease authorizes release with the unguessable token
// returned by acquire. A lease ID alone is intentionally not sufficient.
func ReleaseSharedRuntimeLease(options SharedRuntimeOperatorOptions, token string) error {
	if token == "" {
		return sharedRuntimeError("protocol_violation", errors.New("lease token is required"))
	}
	resolved, state, err := resolveSharedRuntimeOperator(options)
	if err != nil {
		return err
	}
	attested, err := connectAndAttestSharedRuntime(resolved, state, "", options.HTTPClient, 2*time.Second)
	if err != nil {
		return err
	}
	defer attested.close()
	if err := writeSharedWireMessage(attested.connection, sharedWireMessage{Type: "release", LeaseToken: token}); err != nil {
		return sharedRuntimeError("broker_unreachable", err)
	}
	response, err := readSharedWireMessage(attested.reader)
	if err != nil {
		return sharedRuntimeError("broker_unreachable", err)
	}
	if response.Type == "refused" {
		return &SharedRuntimeError{Code: response.Code, Reason: response.Reason, Err: errors.New("broker refused lease release")}
	}
	if response.Type != "released" || response.LeaseID == "" {
		return sharedRuntimeError("protocol_violation", errors.New("broker release response is incomplete"))
	}
	return nil
}
