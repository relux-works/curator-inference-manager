//go:build darwin

package infra

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// R1 stage table (rework-brief-04): only a failure before a peer is
// established (no socket, connection refused) means "no broker". Every
// post-peer failure — hello write, hello EOF/reset/timeout, malformed
// frame — keeps its transport code but must not classify as absence, so
// status refuses instead of falling back to a record-status success.

// TestSharedBrokerUnreachableClassifiesPrePeerAbsenceOnly pins the
// classifier on synthetic errors: error codes and text alone never mean
// absence.
func TestSharedBrokerUnreachableClassifiesPrePeerAbsenceOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not absence", nil, false},
		{"missing socket is absence", &sharedConnectError{err: syscall.ENOENT}, true},
		{"refused socket is absence", &sharedConnectError{err: syscall.ECONNREFUSED}, true},
		{"other connect errno is not absence", &sharedConnectError{err: syscall.EACCES}, false},
		{"broker_unreachable code is not absence", sharedRuntimeError("broker_unreachable", os.ErrDeadlineExceeded), false},
		{"shared_runtime_unavailable code is not absence", sharedRuntimeError("shared_runtime_unavailable", io.EOF), false},
		{"protocol_violation is not absence", sharedRuntimeError("protocol_violation", io.ErrUnexpectedEOF), false},
		{"generic error is not absence", io.ErrUnexpectedEOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSharedBrokerUnreachable(tc.err); got != tc.want {
				t.Fatalf("isSharedBrokerUnreachable(%v)=%v want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConnectAndAttestPostPeerFailuresNeverMeanAbsence drives the real
// attestation path: pre-peer dial absence classifies as absence, while
// every injected post-peer transport failure keeps its pinned code and
// classifies as a failed observation.
func TestConnectAndAttestPostPeerFailuresNeverMeanAbsence(t *testing.T) {
	t.Run("absent socket", func(t *testing.T) {
		fixture := newSharedAttestationFixture(t)
		if err := fixture.listener.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-stage-absent", nil, time.Second)
		if err == nil {
			t.Fatal("absent socket unexpectedly attested")
		}
		if !isSharedBrokerUnreachable(err) {
			t.Fatalf("absent socket error %v does not classify as absence", err)
		}
	})
	t.Run("refused socket", func(t *testing.T) {
		fixture := newSharedAttestationFixture(t)
		if err := fixture.listener.Close(); err != nil {
			t.Fatal(err)
		}
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
		_, err = connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-stage-refused", nil, time.Second)
		if err == nil {
			t.Fatal("refused socket unexpectedly attested")
		}
		if !isSharedBrokerUnreachable(err) {
			t.Fatalf("refused socket error %v does not classify as absence", err)
		}
	})
	postPeer := []struct {
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
		{
			name: "malformed frame read", code: "protocol_violation",
			mutate: func() func() {
				original := sharedRuntimeAttestationReadMessage
				sharedRuntimeAttestationReadMessage = func(io.Reader) (sharedWireMessage, error) {
					return sharedWireMessage{}, sharedRuntimeError("protocol_violation", io.ErrUnexpectedEOF)
				}
				return func() { sharedRuntimeAttestationReadMessage = original }
			},
		},
	}
	for _, tc := range postPeer {
		t.Run("post-peer/"+tc.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			var served <-chan error
			if tc.answerWithoutClientHello {
				served = answerAttestationWithoutClientHello(t, fixture)
			} else {
				served = fixture.answerOnce(t, nil)
			}
			restore := tc.mutate()
			t.Cleanup(restore)
			_, err := connectAndAttestSharedRuntime(fixture.resolved, fixture.state, "RUN-stage", nil, time.Second)
			if err == nil {
				t.Fatalf("post-peer %s unexpectedly attested", tc.name)
			}
			requireSharedAttestationCode(t, err, tc.code)
			if isSharedBrokerUnreachable(err) {
				t.Fatalf("post-peer %s error %v laundered into absence", tc.name, err)
			}
			select {
			case <-served:
			case <-time.After(2 * time.Second):
				t.Fatal("fixture did not observe refused attestation")
			}
		})
	}
}

// TestSharedConnectErrorHasOnePrePeerConstructionSite pins the stage
// discipline the classifier depends on: the absence marker is built only
// at the pre-peer dial site. A second construction site must update this
// guard consciously.
func TestSharedConnectErrorHasOnePrePeerConstructionSite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	site := ""
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			// &sharedConnectError{...} parses as a unary address of a
			// composite literal whose type is the plain identifier.
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if ident, ok := literal.Type.(*ast.Ident); ok && ident.Name == "sharedConnectError" {
				found++
				site = name
			}
			return true
		})
	}
	if found != 1 || site != "pi_shared_client_darwin.go" {
		t.Fatalf("sharedConnectError construction sites=%d in %q, want exactly 1 in pi_shared_client_darwin.go", found, site)
	}
}
