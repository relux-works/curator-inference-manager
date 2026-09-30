//go:build darwin

package infra

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func replaceBrokerSystem(t *testing.T, update func(*sharedRuntimeBrokerSystem)) {
	t.Helper()
	previous := brokerSystem
	next := previous
	update(&next)
	brokerSystem = next
	t.Cleanup(func() { brokerSystem = previous })
}

func TestRunSharedRuntimeBrokerRefusesInjectedSystemFailuresAtProductionEntry(t *testing.T) {
	t.Run("session identity read failed", func(t *testing.T) {
		options := brokerIdentityFailureOptions(t)
		replaceBrokerSystem(t, func(system *sharedRuntimeBrokerSystem) {
			system.getSessionID = func(int) (int, error) { return os.Getpid(), os.ErrPermission }
		})
		if err := RunSharedRuntimeBroker(options); sharedRuntimeErrorCode(err) != "broker_not_session_leader" || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("session identity failure err=%v", err)
		}
	})

	t.Run("process is not a session leader", func(t *testing.T) {
		options := brokerIdentityFailureOptions(t)
		replaceBrokerSystem(t, func(system *sharedRuntimeBrokerSystem) {
			system.getSessionID = func(int) (int, error) { return os.Getpid() - 1, nil }
		})
		if err := RunSharedRuntimeBroker(options); sharedRuntimeErrorCode(err) != "broker_not_session_leader" {
			t.Fatalf("non-leader broker admission err=%v", err)
		}
	})

	t.Run("self process identity unavailable", func(t *testing.T) {
		options := brokerIdentityFailureOptions(t)
		replaceBrokerSystem(t, func(system *sharedRuntimeBrokerSystem) {
			system.getSessionID = func(int) (int, error) { return os.Getpid(), nil }
			system.inspectProcess = func(int) (sharedProcessObservation, error) { return sharedProcessObservation{}, os.ErrPermission }
		})
		if err := RunSharedRuntimeBroker(options); sharedRuntimeErrorCode(err) != "broker_identity_unavailable" || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("process identity failure err=%v", err)
		}
	})

	t.Run("self executable identity unavailable", func(t *testing.T) {
		options := brokerIdentityFailureOptions(t)
		replaceBrokerSystem(t, func(system *sharedRuntimeBrokerSystem) {
			system.getSessionID = func(int) (int, error) { return os.Getpid(), nil }
			system.ownExecutableIdentity = func() (string, FileIdentity, error) { return "", FileIdentity{}, os.ErrPermission }
		})
		if err := RunSharedRuntimeBroker(options); sharedRuntimeErrorCode(err) != "broker_identity_unavailable" || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("executable identity failure err=%v", err)
		}
	})

	for _, testCase := range []struct {
		name      string
		code      string
		cause     error
		configure func(*sharedRuntimeBrokerSystem)
	}{
		{
			name: "authorization pipe creation failed", code: "runtime_start_failed", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.startUnauthorizedRuntime = func(resolved sharedResolvedProfile, environ []string) (*exec.Cmd, *piProcessWait, *os.File, error) {
					dependencies := defaultUnauthorizedRuntimeSystem()
					dependencies.createPipe = func() (*os.File, *os.File, error) { return nil, nil, os.ErrPermission }
					return startUnauthorizedRuntimeWithSystem(resolved, environ, sharedSystemLogClock{}, func(*exec.Cmd) error { return errors.New("command start reached after the pipe refusal") }, dependencies)
				}
			},
		},
		{
			name: "launcher executable identity failed", code: "runtime_start_failed", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.startUnauthorizedRuntime = func(resolved sharedResolvedProfile, environ []string) (*exec.Cmd, *piProcessWait, *os.File, error) {
					dependencies := defaultUnauthorizedRuntimeSystem()
					dependencies.ownExecutableIdentity = func() (string, FileIdentity, error) { return "", FileIdentity{}, os.ErrPermission }
					return startUnauthorizedRuntimeWithSystem(resolved, environ, sharedSystemLogClock{}, func(*exec.Cmd) error { return errors.New("command start reached after executable identity refusal") }, dependencies)
				}
			},
		},
		{
			name: "launcher command start failed", code: "runtime_start_failed", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.startUnauthorizedRuntime = func(resolved sharedResolvedProfile, environ []string) (*exec.Cmd, *piProcessWait, *os.File, error) {
					return startUnauthorizedRuntimeWithDependencies(resolved, environ, sharedSystemLogClock{}, func(*exec.Cmd) error { return os.ErrPermission })
				}
			},
		},
		{
			name: "launcher identity wait failed", code: "runtime_start_failed", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.waitForProcessIdentity = func(int, time.Duration) (sharedProcessObservation, error) {
					return sharedProcessObservation{}, os.ErrPermission
				}
			},
		},
		{
			name: "runtime executable identity failed", code: "runtime_executable_invalid", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.fileIdentity = func(string) (FileIdentity, error) { return FileIdentity{}, os.ErrPermission }
			},
		},
		{
			name: "authorization frame write failed", code: "protocol_violation", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.writeAuthorizationFrame = func(io.Writer, sharedRuntimeAuthorizationFrame) error { return os.ErrPermission }
			},
		},
		{
			name: "authorization writer close failed", code: "protocol_violation", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.closeAuthorizationWriter = func(file *os.File) error {
					return errors.Join(file.Close(), os.ErrPermission)
				}
			},
		},
		{
			name: "rendezvous socket bind failed", code: "broker_rendezvous_bind_conflict", cause: syscall.EADDRINUSE,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.listenRendezvous = func(string, *net.UnixAddr) (*net.UnixListener, error) { return nil, syscall.EADDRINUSE }
			},
		},
		{
			name: "rendezvous socket permission failed", code: "shared_runtime_state_path_invalid", cause: os.ErrPermission,
			configure: func(system *sharedRuntimeBrokerSystem) {
				system.chmodPath = func(string, os.FileMode) error { return os.ErrPermission }
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var project, home, cache string
			var resolved sharedResolvedProfile
			if testCase.code == "broker_rendezvous_bind_conflict" || testCase.code == "shared_runtime_state_path_invalid" {
				project, home, cache, resolved = newBrokerReadyRuntimeProfile(t)
			} else {
				project, home, cache, resolved = newSharedIntegrationProfile(t)
			}
			replaceBrokerSystem(t, func(system *sharedRuntimeBrokerSystem) {
				system.getSessionID = func(int) (int, error) { return os.Getpid(), nil }
				testCase.configure(system)
			})
			signals := make(chan os.Signal, 1)
			if testCase.code == "shared_runtime_state_path_invalid" {
				signals <- syscall.SIGTERM
			}
			err := RunSharedRuntimeBroker(SharedRuntimeBrokerOptions{
				RuntimeKey: resolved.RuntimeKey, ProfileProject: project, ProfileName: resolved.ProfileName,
				HomeDir: home, CacheRoot: cache, Environ: []string{"HOME=" + home}, Signals: signals,
			})
			got := sharedRuntimeErrorCode(err)
			if got == "" {
				got = piErrorCode(err)
			}
			if got != testCase.code {
				t.Fatalf("production broker error code=%q err=%v, want %q", got, err, testCase.code)
			}
			if testCase.cause != nil && !errors.Is(err, testCase.cause) {
				t.Fatalf("production broker error=%v does not preserve cause %v", err, testCase.cause)
			}
		})
	}
}

func brokerIdentityFailureOptions(t *testing.T) SharedRuntimeBrokerOptions {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project := filepath.Join(root, "project")
	home := filepath.Join(root, "home")
	cache := filepath.Join(root, "cache")
	for _, directory := range []string{project, home, cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := strings.Repeat("a", 64)
	paths, err := ResolveSharedRuntimePaths(cache, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateSharedRuntimeTree(paths); err != nil {
		t.Fatal(err)
	}
	return SharedRuntimeBrokerOptions{
		RuntimeKey: key, ProfileProject: project, ProfileName: "missing", HomeDir: home, CacheRoot: cache,
		Environ: []string{"HOME=" + home},
	}
}

func newBrokerReadyRuntimeProfile(t *testing.T) (project, home, cache string, resolved sharedResolvedProfile) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project = filepath.Join(root, "project")
	home = filepath.Join(root, "home")
	cache = filepath.Join(home, "Library", "Caches")
	executable := filepath.Join(root, "fake-runtime")
	for _, directory := range []string{project, cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", executable, "../../cmd/curator-engines/testdata/fake-runtime")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real fake-runtime process: %v\n%s", err, output)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	if err := tcp.Close(); err != nil {
		t.Fatal(err)
	}
	writePiProjectConfig(t, project, sharedPiProfileTOML("profile", executable, port))
	resolved, err = resolveSharedProfile(project, home, cache, "profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateSharedRuntimeTree(resolved.Paths); err != nil {
		t.Fatal(err)
	}
	return project, home, cache, resolved
}

func TestSharedBrokerServeReturnsUnexpectedListenerAndFatalOutputRefusals(t *testing.T) {
	t.Run("unexpected accept failure", func(t *testing.T) {
		server, listener, runtimeWait := newBrokerServeFixture(t)
		server.acceptUnix = func() (*net.UnixConn, error) { return nil, os.ErrPermission }
		err := server.serve(runtimeWait, nil)
		if sharedRuntimeErrorCode(err) != "broker_unreachable" {
			t.Fatalf("unexpected accept failure code=%q err=%v", sharedRuntimeErrorCode(err), err)
		}
		_ = listener.Close()
	})
	t.Run("fatal output without process identity", func(t *testing.T) {
		server, listener, runtimeWait := newBrokerServeFixture(t)
		t.Cleanup(func() { _ = listener.Close() })
		output := newSharedRuntimeOutputWriter(io.Discard, []string{"fatal-marker"})
		_, _ = output.Write([]byte("fatal-marker"))
		runtimeWait.output = output.state
		runtimeWait.pid = 0
		terminated := false
		server.terminateRuntime = func(int, *piProcessWait, syscall.Signal, time.Duration) error {
			terminated = true
			return nil
		}
		err := server.serve(runtimeWait, nil)
		if sharedRuntimeErrorCode(err) != "runtime_fatal_output" || terminated {
			t.Fatalf("missing identity fatal-output refusal err=%v terminated=%t", err, terminated)
		}
	})
}

func TestSharedBrokerServeReturnsQuarantineWhenRestartLimitIsReached(t *testing.T) {
	server, listener, runtimeWait := newBrokerServeFixture(t)
	t.Cleanup(func() { _ = listener.Close() })
	server.resolved.Sharing.RestartLimit = 1
	server.resolved.Sharing.RestartInitialBackoffSeconds = 1
	server.resolved.Sharing.RestartMaxBackoffSeconds = 1
	server.resolved.Sharing.StableRunSeconds = 60
	server.resolved.Sharing.QuarantineSeconds = 30
	server.readyAt = time.Now()
	ledger := newSharedRuntimeRestartLedger(server.resolved.RuntimeKey, server.resolved.ProfileDigest)
	server.ledger = &ledger
	runtimeWait.err = errors.New("fixture runtime exit")
	runtimeWait.output = &sharedRuntimeOutputState{}
	close(runtimeWait.done)
	releaseAccept := make(chan struct{})
	server.acceptUnix = func() (*net.UnixConn, error) {
		<-releaseAccept
		return nil, net.ErrClosed
	}
	defer close(releaseAccept)
	err := server.serve(runtimeWait, nil)
	requireSharedPathRefusal(t, err, "shared_runtime_quarantined")
	if server.ledger.RestartCount != 1 || server.ledger.QuarantinedUntil == nil {
		t.Fatalf("quarantine ledger=%#v", server.ledger)
	}
}

func newBrokerServeFixture(t *testing.T) (*sharedBrokerServer, *net.UnixListener, *piProcessWait) {
	t.Helper()
	_, _, _, resolved := newSharedIntegrationProfile(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: resolved.Paths.RendezvousSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := &SharedBrokerRecord{State: "serving", Stage: "composed", ReadyAt: &now}
	ledger := &SharedRuntimeRestartLedger{RestartCount: 0}
	server := newSharedBrokerServer(resolved, record, ledger, listener)
	return server, listener, &piProcessWait{done: make(chan struct{}), pid: 1}
}
