//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
	"golang.org/x/sys/unix"
)

// Outer-envelope regressions for rework-brief-02 (M1/M2). A real
// kernel-attested process and AF_UNIX peer drive run(status --json); the
// broker replies are hostile test evidence, never a healthy Qwen proof. Every
// case below must refuse with an empty readings set.

type outerEnvelopeEnv struct {
	execute                             func(resources map[string]any, mutateHello, mutateStatus func(map[string]any)) (int, string, string)
	resources                           map[string]any
	runtimeKey, profileDigest, endpoint string
	engineName, engineKey               string
	runtimePID                          int
}

// waitForCatalogFakeRuntime starts the fake runtime and waits until its
// /models endpoint serves the wanted model id. Fixture ports are content
// hashes in the ephemeral range, so a concurrent test process can briefly
// hold the port; a 200 without our model means an impostor, and the fake is
// restarted until the deadline. Returns the running command.
func waitForCatalogFakeRuntime(t *testing.T, executable string, argv []string, cwd, endpoint, model string) *exec.Cmd {
	t.Helper()
	var runtime *exec.Cmd
	t.Cleanup(func() {
		if runtime != nil && runtime.Process != nil {
			_ = runtime.Process.Kill()
			_ = runtime.Wait()
		}
	})
	start := func() {
		if runtime != nil && runtime.Process != nil {
			_ = runtime.Process.Kill()
			_ = runtime.Wait()
		}
		runtime = exec.Command(executable, argv...)
		runtime.Dir = cwd
		if err := runtime.Start(); err != nil {
			t.Fatal(err)
		}
	}
	start()
	deadline := time.Now().Add(10 * time.Second)
	for {
		switch probeCatalogFakeModel(endpoint, model) {
		case catalogFakeServing:
			return runtime
		case catalogFakePoisoned:
			start()
		default:
			if runtime.Process == nil || runtime.Process.Signal(syscall.Signal(0)) != nil {
				start()
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture runtime unavailable")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type catalogFakeProbe int

const (
	catalogFakeWaiting catalogFakeProbe = iota
	catalogFakeServing
	catalogFakePoisoned
)

func probeCatalogFakeModel(endpoint, model string) catalogFakeProbe {
	response, err := http.Get(endpoint + "/models")
	if err != nil {
		return catalogFakeWaiting
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return catalogFakePoisoned
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return catalogFakeWaiting
	}
	for _, item := range payload.Data {
		if item.ID == model {
			return catalogFakeServing
		}
	}
	return catalogFakePoisoned
}

func outerEnvelopeFixture(t *testing.T) *outerEnvelopeEnv {
	t.Helper()
	executable := buildCatalogFakeRuntime(t)
	entry := readingsCatalogEntry()
	entry.Executable = executable
	entry.Knobs = nil
	prepareCatalogCLIEntry(t, "readings", entry)
	cold, _ := runStatus(t, []string{"status", "--engine", "readings", "--json"})
	var argv []string
	for _, row := range cold.Readings.Facts {
		if row.Fact == "profile-local-argv" {
			if err := json.Unmarshal([]byte(row.Value), &argv); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(cold.Paths.RuntimeCWD, 0700); err != nil {
		t.Fatal(err)
	}
	runtime := waitForCatalogFakeRuntime(t, executable, argv, cold.Paths.RuntimeCWD, cold.Endpoint, entry.ServedModelID)
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", runtime.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	var kernelStat unix.Stat_t
	if err := unix.Stat(executable, &kernelStat); err != nil {
		t.Fatal(err)
	}
	record := infra.SharedRuntimeProcessRecord{PID: runtime.Process.Pid, PGID: int(kinfo.Eproc.Pgid), UID: kinfo.Eproc.Ucred.Uid, StartTime: infra.ProcessStartTime{Seconds: kinfo.Proc.P_starttime.Sec, Microseconds: kinfo.Proc.P_starttime.Usec}, Endpoint: cold.Endpoint, ExecutableIdentity: infra.FileIdentity{Dev: uint64(kernelStat.Dev), Ino: kernelStat.Ino, Size: stat.Size(), Mode: stat.Mode()}, Stage: "running"}
	self, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cold.Paths.RendezvousSocket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", cold.Paths.RendezvousSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cold.Paths.RendezvousSocket, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	env := &outerEnvelopeEnv{
		runtimeKey: cold.RuntimeKey, profileDigest: cold.ProfileDigest, endpoint: cold.Endpoint,
		engineName: cold.EngineIdentity.Name, engineKey: cold.EngineIdentity.Key,
		runtimePID: runtime.Process.Pid,
	}
	env.execute = func(resources map[string]any, mutateHello, mutateStatus func(map[string]any)) (int, string, string) {
		served := make(chan error, 1)
		go func() {
			connection, err := listener.Accept()
			if err != nil {
				served <- err
				return
			}
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			decoder, encoder := json.NewDecoder(bufio.NewReader(connection)), json.NewEncoder(connection)
			var hello map[string]json.RawMessage
			if err := decoder.Decode(&hello); err != nil {
				served <- err
				return
			}
			var identity infra.FileIdentity
			if err := json.Unmarshal(hello["client_exec"], &identity); err != nil {
				served <- err
				return
			}
			broker := infra.SharedBrokerIdentity{PID: os.Getpid(), StartTime: infra.ProcessStartTime{Seconds: self.Proc.P_starttime.Sec, Microseconds: self.Proc.P_starttime.Usec}, ExecutableIdentity: identity}
			reply := map[string]any{"type": "hello_ok", "protocol_version": 7, "runtime_key": hello["runtime_key"], "profile_digest": hello["profile_digest"], "engine_identity": hello["engine_identity"], "broker": broker, "runtime": record, "sharing_effective": hello["sharing_configured"]}
			if mutateHello != nil {
				mutateHello(reply)
			}
			// Transport-failure injection for the R1 regressions: a
			// sentinel key selects a truncated frame (the client reads
			// EOF) or a stall past the client's 1s hello deadline. The
			// sentinel never reaches the encoder.
			if mode, ok := reply["__fixture_hello_read_failure"].(string); ok {
				if mode == "partial" {
					_, _ = connection.Write([]byte(`{"type":"hello_ok","protocol_version":7`))
				} else {
					time.Sleep(1200 * time.Millisecond)
				}
				served <- nil
				return
			}
			if err := encoder.Encode(reply); err != nil {
				served <- err
				return
			}
			var request map[string]any
			if err := decoder.Decode(&request); err != nil {
				// A refused hello legitimately ends the session before the
				// status request; the client-side refusal is the assertion.
				if mutateHello != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
					served <- nil
					return
				}
				served <- err
				return
			}
			if request["type"] != "status" {
				served <- fmt.Errorf("unexpected request %v", request)
				return
			}
			reply = map[string]any{"type": "status", "state": "serving", "stage": "composed", "broker": broker, "runtime": record, "sharing_effective": hello["sharing_configured"], "resources": resources}
			if mutateStatus != nil {
				mutateStatus(reply)
			}
			// Status-phase counterpart of the hello injection above: the
			// hello exchange already succeeded, so only the status read
			// fails. These controls already refused before the R1 fix.
			if mode, ok := reply["__fixture_status_read_failure"].(string); ok {
				if mode == "partial" {
					_, _ = connection.Write([]byte(`{"type":"status","state":"serving"`))
				} else {
					time.Sleep(1200 * time.Millisecond)
				}
				served <- nil
				return
			}
			served <- encoder.Encode(reply)
		}()
		var stdout, stderr bytes.Buffer
		code := run([]string{"status", "--engine", "readings", "--json"}, &stdout, &stderr)
		if err := <-served; err != nil {
			t.Fatalf("fixture protocol: %v; status stderr=%s", err, &stderr)
		}
		return code, stdout.String(), stderr.String()
	}
	env.resources = map[string]any{"schema": infra.SharedRuntimeResourceStatusSchema, "source": "provider-observed", "state": "healthy", "admission": "admitted", "observed_at": time.Now().UTC(), "loaded_model_memory": map[string]any{"state": "healthy", "bytes": 100, "pressure_at_bytes": 1000, "recovery_at_bytes": 800}, "inference": map[string]any{"state": "idle"}, "policy": map[string]any{"mode": "provider", "eviction_grace_seconds": 0, "pressure_action": "refuse-new-drain-idle", "unknown_action": "refuse-new", "busy_action": "observe"}}
	return env
}

func requireOuterRefusal(t *testing.T, code int, stdout, stderr, wantCode string) {
	t.Helper()
	if code == 0 || stdout != "" {
		var report infra.SharedRuntimeStatus
		_ = json.Unmarshal([]byte(stdout), &report)
		t.Fatalf("malformed envelope admitted: exit=%d facts=%d stdout=%q stderr=%s", code, len(report.Readings.Facts), stdout, stderr)
	}
	if !strings.Contains(stderr, wantCode) {
		t.Fatalf("refusal stderr=%s want code %s", stderr, wantCode)
	}
}

func requireOuterValid(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 0 || !strings.Contains(stdout, `"readings"`) {
		t.Fatalf("valid envelope exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var report infra.SharedRuntimeStatus
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Readings.Facts) != 17 {
		t.Fatalf("valid envelope facts=%d want 17", len(report.Readings.Facts))
	}
}

func outerReplyMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func outerMutateSharing(t *testing.T, reply map[string]any, change func(map[string]any)) {
	t.Helper()
	sharing := outerReplyMap(t, reply["sharing_effective"])
	change(sharing)
	reply["sharing_effective"] = sharing
}

func outerProviderPressure() map[string]any {
	return map[string]any{"observation_path": "/observations", "observation_timeout_milliseconds": 1000, "pressure_threshold_bytes": 2000, "recovery_threshold_bytes": 1000, "eviction_grace_seconds": 5, "pressure_action": "refuse-new-drain-idle", "unknown_action": "refuse-new", "busy_action": "observe"}
}

func outerValidLease(env *outerEnvelopeEnv) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"lease_id": "0123456789abcdef0123456789abcdef", "lease_lifetime": "client_process",
		"engine_identity": map[string]any{"name": env.engineName, "key": env.engineKey},
		"client_key":      "test-client", "client_pid": os.Getpid(),
		"client_start_time": map[string]any{"seconds": time.Now().Unix(), "microseconds": 0},
		"runtime_key":       env.runtimeKey, "profile_digest": env.profileDigest, "endpoint": env.endpoint,
		"runtime_pid": env.runtimePID, "granted_at": now, "last_heartbeat_at": now,
		"state": "held", "age": 1000000, "heartbeat_age": 1000000,
	}
}

func outerMutateBroker(t *testing.T, reply map[string]any, change func(*infra.SharedBrokerIdentity)) {
	t.Helper()
	broker, ok := reply["broker"].(infra.SharedBrokerIdentity)
	if !ok {
		t.Fatalf("broker reply shape %T", reply["broker"])
	}
	change(&broker)
	reply["broker"] = broker
}

func outerMutateRuntime(t *testing.T, reply map[string]any, change func(*infra.SharedRuntimeProcessRecord)) {
	t.Helper()
	record, ok := reply["runtime"].(infra.SharedRuntimeProcessRecord)
	if !ok {
		t.Fatalf("runtime reply shape %T", reply["runtime"])
	}
	change(&record)
	reply["runtime"] = record
}

// M1 status path: every hostile status reply below must refuse through
// run(status --json) with an empty readings set.
func TestOuterStatusEnvelope(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) { reply["state"] = "draining" })
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, reply map[string]any)
	}{
		{"status_empty_runtime", func(t *testing.T, reply map[string]any) { reply["runtime"] = map[string]any{} }},
		{"status_empty_broker", func(t *testing.T, reply map[string]any) { reply["broker"] = map[string]any{} }},
		{"status_invalid_state", func(t *testing.T, reply map[string]any) { reply["state"] = "__invalid__" }},
		{"status_state_mutant", func(t *testing.T, reply map[string]any) { reply["state"] = "__mutant__" }},
		{"status_state_starting", func(t *testing.T, reply map[string]any) { reply["state"] = "starting" }},
		{"status_missing_state", func(t *testing.T, reply map[string]any) { delete(reply, "state") }},
		{"status_invalid_stage", func(t *testing.T, reply map[string]any) { reply["stage"] = "__invalid__" }},
		{"status_stage_mutant", func(t *testing.T, reply map[string]any) { reply["stage"] = "__mutant__" }},
		{"status_missing_stage", func(t *testing.T, reply map[string]any) { delete(reply, "stage") }},
		{"status_broker_pid_zero", func(t *testing.T, reply map[string]any) {
			outerMutateBroker(t, reply, func(broker *infra.SharedBrokerIdentity) { broker.PID = 0 })
		}},
		{"status_broker_pid_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateBroker(t, reply, func(broker *infra.SharedBrokerIdentity) { broker.PID = 999999 })
		}},
		{"status_broker_start_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateBroker(t, reply, func(broker *infra.SharedBrokerIdentity) { broker.StartTime.Seconds++ })
		}},
		{"status_broker_exec_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateBroker(t, reply, func(broker *infra.SharedBrokerIdentity) { broker.ExecutableIdentity.Ino++ })
		}},
		{"status_runtime_pid_zero", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.PID = 0 })
		}},
		{"status_runtime_pid_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.PID = 999999 })
		}},
		{"status_runtime_start_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.StartTime.Seconds++ })
		}},
		{"status_runtime_endpoint_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.Endpoint = "__mutant__" })
		}},
		{"status_runtime_exec_mismatch", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.ExecutableIdentity.Ino++ })
		}},
		{"status_runtime_stage_invalid", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.Stage = "__invalid__" })
		}},
		{"status_runtime_stage_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.Stage = "__mutant__" })
		}},
		{"status_state_case_alias", func(t *testing.T, reply map[string]any) { reply["State"] = "draining" }},
		{"status_broker_case_alias", func(t *testing.T, reply map[string]any) {
			broker := outerReplyMap(t, reply["broker"])
			broker["PID"] = broker["pid"]
			reply["broker"] = broker
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, func(reply map[string]any) { tc.change(t, reply) })
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
}

// M1 hello path: the hello reply's runtime stage and sharing are validated
// before the status round trip.
func TestOuterHelloEnvelope(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, reply map[string]any)
	}{
		{"hello_empty_sharing", func(t *testing.T, reply map[string]any) { reply["sharing_effective"] = map[string]any{} }},
		{"hello_invalid_stage", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.Stage = "__invalid__" })
		}},
		{"hello_stage_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateRuntime(t, reply, func(record *infra.SharedRuntimeProcessRecord) { record.Stage = "__mutant__" })
		}},
		{"hello_sharing_mode_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["mode"] = "__mutant__" })
		}},
		{"hello_broker_case_alias", func(t *testing.T, reply map[string]any) {
			broker := outerReplyMap(t, reply["broker"])
			broker["PID"] = broker["pid"]
			reply["broker"] = broker
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, func(reply map[string]any) { tc.change(t, reply) }, nil)
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
}

// Sharing envelope: every hostile sharing_effective member below must refuse.
// The provider branch rebuilds a valid provider policy and narrows one member.
func TestOuterSharingEnvelope(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	provider := func(t *testing.T, reply map[string]any, change func(map[string]any)) {
		outerMutateSharing(t, reply, func(sharing map[string]any) {
			sharing["resource_pressure_mode"] = "provider"
			pressure := outerProviderPressure()
			change(pressure)
			sharing["resource_pressure"] = pressure
		})
	}
	code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) { provider(t, reply, func(map[string]any) {}) })
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, reply map[string]any)
	}{
		{"status_empty_sharing", func(t *testing.T, reply map[string]any) { reply["sharing_effective"] = map[string]any{} }},
		{"sharing_mode_invalid", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["mode"] = "__invalid__" })
		}},
		{"sharing_mode_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["mode"] = "__mutant__" })
		}},
		{"sharing_linger", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["linger_seconds"] = -1 })
		}},
		{"sharing_max_leases", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["max_leases"] = 0 })
		}},
		{"sharing_max_segment_bytes", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["max_segment_bytes"] = 0 })
		}},
		{"sharing_max_segments", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["max_segments"] = 0 })
		}},
		{"sharing_heartbeat", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["heartbeat_interval_seconds"] = 0 })
		}},
		{"sharing_lease_stale", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["lease_stale_seconds"] = 0 })
		}},
		{"sharing_broker_timeout", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["broker_start_timeout_seconds"] = 0 })
		}},
		{"sharing_restart_limit", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["restart_limit"] = -1 })
		}},
		{"sharing_restart_initial_backoff", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["restart_initial_backoff_seconds"] = -1 })
		}},
		{"sharing_restart_max_backoff", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["restart_max_backoff_seconds"] = -1 })
		}},
		{"sharing_restart_initial_backoff_ms", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["restart_initial_backoff_milliseconds"] = -1 })
		}},
		{"sharing_restart_max_backoff_ms", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["restart_max_backoff_milliseconds"] = -1 })
		}},
		{"sharing_stable_run", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["stable_run_seconds"] = 0 })
		}},
		{"sharing_quarantine", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["quarantine_seconds"] = 0 })
		}},
		{"sharing_heartbeat_not_less_than_stale", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["heartbeat_interval_seconds"] = 30
				sharing["lease_stale_seconds"] = 30
			})
		}},
		{"sharing_restart_backoff_unordered", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["restart_max_backoff_seconds"] = 1
				sharing["restart_initial_backoff_seconds"] = 2
			})
		}},
		{"sharing_pressure_mode_mutant", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["resource_pressure_mode"] = "__mutant__" })
		}},
		{"sharing_provider_lacks_policy", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) {
				sharing["resource_pressure_mode"] = "provider"
				delete(sharing, "resource_pressure")
			})
		}},
		{"sharing_disabled_carries_policy", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["resource_pressure"] = outerProviderPressure() })
		}},
		{"pressure_observation_path", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["observation_path"] = "" })
		}},
		{"pressure_timeout_zero", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["observation_timeout_milliseconds"] = 0 })
		}},
		{"pressure_timeout_above_max", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["observation_timeout_milliseconds"] = 30001 })
		}},
		{"pressure_threshold_zero", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["pressure_threshold_bytes"] = 0 })
		}},
		{"pressure_recovery_zero", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["recovery_threshold_bytes"] = 0 })
		}},
		{"pressure_thresholds_unordered", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) {
				pressure["recovery_threshold_bytes"] = 1000
				pressure["pressure_threshold_bytes"] = 1000
			})
		}},
		{"pressure_eviction_grace", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["eviction_grace_seconds"] = -1 })
		}},
		{"pressure_action_mutant", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["pressure_action"] = "__mutant__" })
		}},
		{"pressure_unknown_action_mutant", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["unknown_action"] = "__mutant__" })
		}},
		{"pressure_busy_action_mutant", func(t *testing.T, reply map[string]any) {
			provider(t, reply, func(pressure map[string]any) { pressure["busy_action"] = "__mutant__" })
		}},
		{"sharing_mode_case_alias", func(t *testing.T, reply map[string]any) {
			outerMutateSharing(t, reply, func(sharing map[string]any) { sharing["Mode"] = "shared" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := env.execute(env.resources, nil, func(reply map[string]any) { tc.change(t, reply) })
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
}

// Lease rows: each hostile lease member must refuse; valid rows stay admitted.
func TestOuterLeaseEnvelope(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) {
		reply["leases"] = []any{outerValidLease(env)}
	})
	requireOuterValid(t, code, stdout, stderr)
	code, stdout, stderr = env.execute(env.resources, nil, func(reply map[string]any) {
		second := outerValidLease(env)
		second["lease_id"] = "ffffffffffffffffffffffffffffffff"
		reply["leases"] = []any{outerValidLease(env), second}
	})
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		name   string
		change func(lease map[string]any)
	}{
		{"lease_id_empty", func(lease map[string]any) { lease["lease_id"] = "" }},
		{"lease_token_mutant", func(lease map[string]any) { lease["lease_token"] = "__mutant__" }},
		{"lease_lifetime_mutant", func(lease map[string]any) { lease["lease_lifetime"] = "__mutant__" }},
		{"lease_engine_key_mutant", func(lease map[string]any) { lease["engine_identity"].(map[string]any)["key"] = "__mutant__" }},
		{"lease_runtime_key_mutant", func(lease map[string]any) { lease["runtime_key"] = "__mutant__" }},
		{"lease_profile_digest_mutant", func(lease map[string]any) { lease["profile_digest"] = "__mutant__" }},
		{"lease_endpoint_mutant", func(lease map[string]any) { lease["endpoint"] = "__mutant__" }},
		{"lease_runtime_pid_zero", func(lease map[string]any) { lease["runtime_pid"] = 0 }},
		{"lease_runtime_pid_mismatch", func(lease map[string]any) { lease["runtime_pid"] = 999999 }},
		{"lease_client_pid_zero", func(lease map[string]any) { lease["client_pid"] = 0 }},
		{"lease_client_start_zero", func(lease map[string]any) { lease["client_start_time"].(map[string]any)["seconds"] = 0 }},
		{"lease_state_mutant", func(lease map[string]any) { lease["state"] = "__mutant__" }},
		{"lease_state_released", func(lease map[string]any) { lease["state"] = "released" }},
		{"lease_state_case_alias", func(lease map[string]any) { lease["STATE"] = "held" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := outerValidLease(env)
			tc.change(lease)
			code, stdout, stderr := env.execute(env.resources, nil, func(reply map[string]any) {
				reply["leases"] = []any{lease}
			})
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
}

// M2: case-folded keys bypass the closed resource schema at every level.
func TestOuterResourceCaseAlias(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	for _, tc := range []struct {
		name   string
		change func(resources map[string]any)
	}{
		{"resource_case_alias", func(resources map[string]any) {
			resources["inference"] = json.RawMessage(`{"state":"busy","State":"idle"}`)
		}},
		{"resource_case_unknown_member", func(resources map[string]any) {
			resources["inference"] = json.RawMessage(`{"state":"idle","STATE":"idle"}`)
		}},
		{"resource_case_alias_flips_fact", func(resources map[string]any) {
			resources["inference"] = json.RawMessage(`{"state":"idle","STATE":"busy"}`)
		}},
		{"resource_memory_case_alias", func(resources map[string]any) {
			resources["loaded_model_memory"].(map[string]any)["State"] = "pressured"
		}},
		{"resource_policy_case_alias", func(resources map[string]any) {
			resources["policy"].(map[string]any)["Mode"] = "disabled"
		}},
		{"resource_top_case_alias", func(resources map[string]any) { resources["State"] = "busy" }},
		{"resource_inference_duplicate", func(resources map[string]any) {
			resources["inference"] = json.RawMessage(`{"state":"idle","state":"busy"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, err := json.Marshal(env.resources)
			if err != nil {
				t.Fatal(err)
			}
			var resources map[string]any
			if err := json.Unmarshal(original, &resources); err != nil {
				t.Fatal(err)
			}
			tc.change(resources)
			code, stdout, stderr := env.execute(resources, nil, nil)
			requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
		})
	}
}
