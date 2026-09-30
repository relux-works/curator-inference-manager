//go:build darwin

package infra

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Invariant: an attestation refusal never invalidates an existing lease or
// evicts a busy engine. Only the failing acquisition's own, unleased launch
// is cleaned. The class matrix covers every catalog kind/locality and every
// caller of the common attestation path, including the legacy Pi caller.
func TestAttestationRefusalOwnershipProperty(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "engine")
	build := exec.Command("go", "build", "-o", fake, "../../cmd/curator-engines/testdata/fake-runtime")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native fake engine: %v\n%s", err, output)
	}
	for _, kind := range []string{"legacy", "llama-cpp", "mlx-lm", "mlx-swift"} {
		modes := []string{"local", "ssh"}
		if kind == "legacy" {
			modes = modes[:1]
		}
		for _, mode := range modes {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				t.Run("existing-lease", func(t *testing.T) {
					options, environ, _ := ownershipPropertyProfile(t, fake, kind, mode)
					lease, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-owner", environ)
					if err != nil {
						t.Fatal(err)
					}
					defer lease.Close()
					before, err := SharedRuntimeStatusReport(options)
					if err != nil || before.Runtime == nil || len(before.Leases) != 1 {
						t.Fatalf("initial lease was not serving: state=%s error=%v", before.Broker.State, err)
					}
					resolved, state, err := resolveSharedRuntimeOperator(options)
					if err != nil {
						t.Fatal(err)
					}
					callers := []struct {
						name string
						call func(SharedRuntimeOperatorOptions) error
					}{
						{"status", func(o SharedRuntimeOperatorOptions) error { _, err := SharedRuntimeStatusReport(o); return err }},
						{"second-acquire", func(o SharedRuntimeOperatorOptions) error {
							second, err := AcquireSharedRuntimeLease(context.Background(), o, "RUN-second", environ)
							if second != nil {
								second.Close()
							}
							return err
						}},
						{"rejected-connection", func(o SharedRuntimeOperatorOptions) error {
							connection, err := connectAndAcquireSharedRuntime(resolved, state, "RUN-connection", o.HTTPClient)
							if connection != nil {
								connection.close()
							}
							return err
						}},
						{"release", func(o SharedRuntimeOperatorOptions) error {
							return ReleaseSharedRuntimeLease(o, lease.record.LeaseToken)
						}},
						{"operator-stop", func(o SharedRuntimeOperatorOptions) error {
							_, err := StopSharedRuntime(o, true, time.Second)
							return err
						}},
					}
					for _, caller := range callers {
						for _, fault := range []string{"network", "model-membership", "hello-refusal"} {
							t.Run(caller.name+"/"+fault, func(t *testing.T) {
								original := sharedRuntimeAttestationWriteMessage
								stops := 0
								sharedRuntimeAttestationWriteMessage = func(writer io.Writer, message sharedWireMessage) error {
									if message.Type == "stop" {
										stops++
									}
									if fault == "hello-refusal" && message.Type == "hello" {
										message.RuntimeKey = "not-the-broker-key"
									}
									return original(writer, message)
								}
								defer func() { sharedRuntimeAttestationWriteMessage = original }()
								broken := options
								switch fault {
								case "network":
									broken.HTTPClient = &http.Client{Transport: reviewFailTransport{}}
								case "model-membership":
									broken.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
										return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"unrelated"}]}`))}, nil
									})}
								}
								err := caller.call(broken)
								if err == nil {
									t.Fatal("attestation refusal was hidden")
								}
								if stops != 0 {
									t.Fatalf("unowned caller sent %d cleanup stop requests", stops)
								}
								// Restore the wire fault before the independent healthy API probe.
								sharedRuntimeAttestationWriteMessage = original
								assertOwnershipLeaseStillServing(t, options, before, lease)
							})
						}
					}
					for _, witness := range []string{"wrong-pid", "wrong-start-time"} {
						t.Run(witness, func(t *testing.T) {
							ownership := sharedRuntimeLaunchOwnership{brokerPID: before.Broker.PID, brokerStartTime: *before.Broker.StartTime}
							if witness == "wrong-pid" {
								ownership.brokerPID++
							} else {
								ownership.brokerStartTime.Microseconds++
							}
							original := sharedRuntimeAttestationWriteMessage
							stops := 0
							sharedRuntimeAttestationWriteMessage = func(writer io.Writer, message sharedWireMessage) error {
								if message.Type == "stop" {
									stops++
								}
								return original(writer, message)
							}
							defer func() { sharedRuntimeAttestationWriteMessage = original }()
							_, err := connectAndAcquireSharedRuntimeWithOwnership(resolved, state, "RUN-invalid-ownership", &http.Client{Transport: reviewFailTransport{}}, ownership)
							if err == nil || stops != 0 {
								t.Fatalf("invalid launch identity sent cleanup: stops=%d error=%v", stops, err)
							}
							sharedRuntimeAttestationWriteMessage = original
							assertOwnershipLeaseStillServing(t, options, before, lease)
						})
					}
					// An election attempt against this broker returns a DIFFERENT
					// launch identity. Carrying it cannot authorize cleanup of the winner.
					t.Run("election-loser", func(t *testing.T) {
						loser, err := startSharedRuntimeBroker(resolved, environ)
						if err != nil {
							t.Fatal(err)
						}
						select {
						case err := <-loser.done:
							var exit *exec.ExitError
							if !errors.As(err, &exit) || exit.ExitCode() != sharedRuntimeExitElectionLost {
								t.Fatalf("loser exit=%v", err)
							}
						case <-time.After(5 * time.Second):
							_ = loser.command.Process.Kill()
							t.Fatal("election loser did not exit")
						}
						_, err = connectAndAcquireSharedRuntimeWithOwnership(resolved, state, "RUN-loser", &http.Client{Transport: reviewFailTransport{}}, loser.ownership)
						if err == nil {
							t.Fatal("loser attestation refusal was hidden")
						}
						assertOwnershipLeaseStillServing(t, options, before, lease)
					})
				})
				t.Run("fresh-own-acquisition", func(t *testing.T) {
					options, environ, pidFile := ownershipPropertyProfile(t, fake, kind, mode)
					options.HTTPClient = &http.Client{Transport: reviewFailTransport{}}
					_, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-fresh-failure", environ)
					if piErrorCode(err) != "runtime_model_unavailable" {
						t.Fatalf("fresh acquisition refusal=%v", err)
					}
					data, err := os.ReadFile(pidFile)
					if err != nil {
						t.Fatalf("failed acquisition did not launch its runtime: %v", err)
					}
					pid, err := strconv.Atoi(string(data))
					if err != nil {
						t.Fatal(err)
					}
					deadline := time.Now().Add(5 * time.Second)
					for {
						observation, err := inspectSharedProcessKernel(pid)
						if sharedProcessGone(err) || err == nil && !observation.live() {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("own runtime %d remained live after refusal: %v", pid, err)
						}
						time.Sleep(20 * time.Millisecond)
					}
					options.HTTPClient = nil
					deadline = time.Now().Add(5 * time.Second)
					for {
						after, err := SharedRuntimeStatusReport(options)
						if err == nil && after.Broker.State == "absent" && after.Runtime == nil {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("own launch was not cleaned: state=%s error=%v", after.Broker.State, err)
						}
						time.Sleep(20 * time.Millisecond)
					}
				})
				t.Run("fresh-own-acquisition-peer-leased-first", func(t *testing.T) {
					options, environ, _ := ownershipPropertyProfile(t, fake, kind, mode)
					var peer *SharedRuntimeLease
					var before SharedRuntimeStatus
					broken := options
					broken.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
						var err error
						peer, err = AcquireSharedRuntimeLease(context.Background(), options, "RUN-peer-before-owner-attests", environ)
						if err != nil {
							return nil, err
						}
						before, err = SharedRuntimeStatusReport(options)
						if err != nil {
							return nil, err
						}
						return nil, errors.New("owner attestation failed after peer leased")
					})}
					_, err := AcquireSharedRuntimeLease(context.Background(), broken, "RUN-owner-refused", environ)
					if peer != nil {
						defer peer.Close()
					}
					if piErrorCode(err) != "runtime_model_unavailable" || peer == nil {
						t.Fatalf("owner/peer race error=%v peer-present=%t", err, peer != nil)
					}
					assertOwnershipLeaseStillServing(t, options, before, peer)
				})
			})
		}
	}
}

func ownershipPropertyProfile(t *testing.T, fake, kind, mode string) (SharedRuntimeOperatorOptions, []string, string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "own-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project, home := filepath.Join(root, "p"), filepath.Join(root, "h")
	cache := filepath.Join(home, "Library", "Caches")
	for _, dir := range []string{project, cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "ownership-property"
	if kind == "legacy" {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		writePiProjectConfig(t, project, sharedPiProfileTOML(name, fake, port))
	} else {
		entry := EngineCatalogEntry{Mode: mode, Engine: kind, Model: "Model", ServedModelID: "Model", Executable: fake, Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}}
		if mode == "ssh" {
			entry.Executable, entry.Argv, entry.Model = "", nil, ""
			entry.SSHExecutable, entry.SSHTarget = fake, "fixture-host"
			entry.RemoteExecutable, entry.RemoteProfile = "/fixture/model-harness", "remote-profile"
			entry.RemoteHost, entry.RemotePort = "127.0.0.1", 18011
		}
		writeEngineCatalog(t, home, map[string]EngineCatalogEntry{name: entry})
	}
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: name}
	t.Cleanup(func() { _, _ = StopSharedRuntime(options, true, 3*time.Second) })
	pidFile := filepath.Join(root, "runtime.pid")
	environ := append(os.Environ(), "CATALOG_FAKE_SERVED_MODEL_ID=Model", "CATALOG_FAKE_PID_FILE="+pidFile)
	return options, environ, pidFile
}

func assertOwnershipLeaseStillServing(t *testing.T, options SharedRuntimeOperatorOptions, before SharedRuntimeStatus, lease *SharedRuntimeLease) {
	t.Helper()
	after, err := SharedRuntimeStatusReport(options)
	if err != nil || before.Runtime == nil || after.Runtime == nil || after.Runtime.PID != before.Runtime.PID || after.Broker.PID != before.Broker.PID || after.Broker.State != "serving" {
		t.Fatalf("attestation refusal evicted busy engine: before=%s after=%s error=%v", before.Broker.State, after.Broker.State, err)
	}
	if len(after.Leases) != 1 || after.Leases[0].LeaseID != lease.record.LeaseID {
		t.Fatal("attestation refusal invalidated existing lease")
	}
}

// A stop acknowledged while idle can race a lease reservation before serve
// consumes the event. The production lifecycle boundary must recheck under
// the admission mutex rather than acting on the earlier snapshot.
func TestBrokerCleanupRechecksLeasesAtLifecycleBoundary(t *testing.T) {
	server, listener, _ := newEngineCatalogBrokerServeFixture(t)
	defer func() { _ = listener.Close() }()
	server.leases["peer"] = &SharedLeaseRecord{LeaseID: "peer"}
	server.setState("lingering")
	server.notify("stop", false)
	server.notify("lease-change", false)
	wait := &piProcessWait{done: make(chan struct{})}
	result := make(chan error, 1)
	signals := make(chan os.Signal, 1)
	go func() { result <- server.serve(wait, signals) }()
	// FIFO lease-change is a barrier behind the stop event: it must be handled
	// and restore serving instead of stopping the late lease.
	deadline := time.Now().Add(time.Second)
	for server.currentState() != "serving" {
		select {
		case err := <-result:
			t.Fatalf("busy engine stopped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			signals <- os.Interrupt
			t.Fatal("late lease event was not handled")
		}
		time.Sleep(time.Millisecond)
	}
	signals <- os.Interrupt
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("broker serve did not exit")
	}
}
