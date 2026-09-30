//go:build darwin

package infra

import (
	"bufio"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestReleaseSharedRuntimeLeaseClassifiesWireFailuresAndResponses(t *testing.T) {
	fixture := newSharedAttestationFixture(t)
	options := SharedRuntimeOperatorOptions{
		ProjectDir: fixture.resolved.Project,
		HomeDir:    fixture.resolved.HomeDir,
		CacheRoot:  fixture.resolved.Paths.CanonicalCacheRoot,
		Profile:    fixture.resolved.ProfileName,
	}
	originalWrite := sharedRuntimeReleaseWriteMessage
	originalRead := sharedRuntimeReleaseReadMessage
	t.Cleanup(func() {
		sharedRuntimeReleaseWriteMessage = originalWrite
		sharedRuntimeReleaseReadMessage = originalRead
	})

	tests := []struct {
		name              string
		mutateHello       func(*sharedWireMessage)
		releaseResponse   *sharedWireMessage
		writeFailure      bool
		wantCode          string
		wantAnyRefusal    bool
		wantRefusalReason string
	}{
		{
			name: "attestation_failure",
			mutateHello: func(response *sharedWireMessage) {
				response.Type = "unexpected"
			},
			wantAnyRefusal: true,
		},
		{
			name:         "release_write_failure",
			writeFailure: true,
			wantCode:     "broker_unreachable",
		},
		{
			name:            "release_read_failure",
			releaseResponse: nil,
			wantCode:        "broker_unreachable",
		},
		{
			name: "broker_refused",
			releaseResponse: &sharedWireMessage{
				Type: "refused", Code: "fixture-refused", Reason: "release rejected by fixture",
			},
			wantCode:          "fixture-refused",
			wantRefusalReason: "release rejected by fixture",
		},
		{
			name:            "unexpected_response_type",
			releaseResponse: &sharedWireMessage{Type: "unexpected", LeaseID: "lease-fixture"},
			wantCode:        "protocol_violation",
		},
		{
			name:            "released_without_lease_id",
			releaseResponse: &sharedWireMessage{Type: "released"},
			wantCode:        "protocol_violation",
		},
		{
			name:            "successful_release",
			releaseResponse: &sharedWireMessage{Type: "released", LeaseID: "lease-fixture"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sharedRuntimeReleaseWriteMessage = originalWrite
			sharedRuntimeReleaseReadMessage = originalRead
			if test.writeFailure {
				sharedRuntimeReleaseWriteMessage = func(io.Writer, sharedWireMessage) error { return os.ErrPermission }
				sharedRuntimeReleaseReadMessage = func(io.Reader) (sharedWireMessage, error) {
					return sharedWireMessage{Type: "released", LeaseID: "lease-fixture"}, nil
				}
			}
			serverDone := answerSharedRuntimeRelease(t, fixture, test.mutateHello, test.releaseResponse, !test.writeFailure && test.mutateHello == nil)
			err := ReleaseSharedRuntimeLease(options, "fixture-token")
			if test.wantAnyRefusal {
				if sharedRuntimeErrorCode(err) == "" {
					t.Fatalf("release attestation error=%v, want typed refusal", err)
				}
			} else if test.wantCode == "" {
				if err != nil {
					t.Fatalf("release error=%v, want success", err)
				}
			} else {
				var refusal *SharedRuntimeError
				if !errors.As(err, &refusal) || refusal.Code != test.wantCode || refusal.Reason != test.wantRefusalReason {
					t.Fatalf("release error=%v, want code=%q reason=%q", err, test.wantCode, test.wantRefusalReason)
				}
			}
			select {
			case serverErr := <-serverDone:
				if serverErr != nil {
					t.Fatalf("release fixture server: %v", serverErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("release fixture server did not complete")
			}
		})
	}
}

func answerSharedRuntimeRelease(t *testing.T, fixture sharedAttestationFixture, mutateHello func(*sharedWireMessage), releaseResponse *sharedWireMessage, expectRelease bool) <-chan error {
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
				t.Errorf("close release fixture connection: %v", err)
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
		if mutateHello != nil {
			mutateHello(&hello)
		}
		if err := writeSharedWireMessage(connection, hello); err != nil {
			result <- err
			return
		}
		request, err := readSharedWireMessage(reader)
		if !expectRelease {
			if err != nil {
				result <- nil
				return
			}
			result <- errors.New("release request was sent after the refusal")
			return
		}
		if err != nil {
			result <- err
			return
		}
		if request.Type != "release" || request.LeaseToken != "fixture-token" {
			result <- errors.New("release request omitted its type or bearer token")
			return
		}
		if releaseResponse == nil {
			result <- nil
			return
		}
		if err := writeSharedWireMessage(connection, *releaseResponse); err != nil {
			result <- err
			return
		}
		result <- nil
	}()
	return result
}
