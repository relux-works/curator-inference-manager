//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
	"golang.org/x/sys/unix"
)

// A real kernel-attested process and AF_UNIX peer drive run(status --json).
// The broker replies are hostile test evidence, never a healthy Qwen proof.
func readingsEnvelopeFixture(t *testing.T) (func(map[string]any) (int, string, string), map[string]any) {
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
	execute := func(resources map[string]any) (int, string, string) {
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
			if err := encoder.Encode(reply); err != nil {
				served <- err
				return
			}
			var request map[string]any
			if err := decoder.Decode(&request); err != nil {
				served <- err
				return
			}
			if request["type"] != "status" {
				served <- fmt.Errorf("unexpected request %v", request)
				return
			}
			reply = map[string]any{"type": "status", "state": "serving", "stage": "composed", "broker": broker, "runtime": record, "sharing_effective": hello["sharing_configured"], "resources": resources}
			served <- encoder.Encode(reply)
		}()
		var stdout, stderr bytes.Buffer
		code := run([]string{"status", "--engine", "readings", "--json"}, &stdout, &stderr)
		if err := <-served; err != nil {
			t.Fatalf("fixture protocol: %v; status stderr=%s", err, &stderr)
		}
		return code, stdout.String(), stderr.String()
	}
	resources := map[string]any{"schema": infra.SharedRuntimeResourceStatusSchema, "source": "provider-observed", "state": "healthy", "admission": "admitted", "observed_at": time.Now().UTC(), "loaded_model_memory": map[string]any{"state": "healthy", "bytes": 100, "pressure_at_bytes": 1000, "recovery_at_bytes": 800}, "inference": map[string]any{"state": "idle"}, "policy": map[string]any{"mode": "provider", "eviction_grace_seconds": 0, "pressure_action": "refuse-new-drain-idle", "unknown_action": "refuse-new", "busy_action": "observe"}}
	return execute, resources
}

// Semantic resource failures preserve status with typed failed facts; malformed
// wire shapes still refuse the entire response with protocol_violation.
func TestReviewMalformedResourceEnvelope(t *testing.T) {
	execute, resources := readingsEnvelopeFixture(t)
	if code, stdout, stderr := execute(resources); code != 0 || !strings.Contains(stdout, `"readings"`) {
		t.Fatalf("valid envelope exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	original, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		change     func(map[string]any)
	}{
		{"malformed_memory_enum", "readings_malformed", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["state"] = "__mutant__" }},
		{"malformed_resource_enum", "readings_malformed", func(r map[string]any) { r["state"] = "__mutant__" }},
		{"malformed_admission", "readings_malformed", func(r map[string]any) { r["admission"] = "__mutant__" }},
		{"missing_resource_reason", "readings_partial", func(r map[string]any) {
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["loaded_model_memory"] = map[string]any{"state": "unknown", "reason": "unknown", "pressure_at_bytes": 1000, "recovery_at_bytes": 800}
		}},
		{"unknown_memory_without_reason", "readings_partial", func(r map[string]any) {
			m := r["loaded_model_memory"].(map[string]any)
			m["state"] = "unknown"
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "resource_observation_unknown"
			delete(m, "bytes")
		}},
		{"unknown_memory_with_bytes", "readings_partial", func(r map[string]any) {
			m := r["loaded_model_memory"].(map[string]any)
			m["state"] = "unknown"
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "resource_observation_unknown"
			m["reason"] = "unknown"
		}},
		{"missing_memory_bytes", "readings_partial", func(r map[string]any) { delete(r["loaded_model_memory"].(map[string]any), "bytes") }},
		{"zero_memory_bytes", "readings_partial", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["bytes"] = 0 }},
		{"observed_memory_reason", "readings_partial", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["reason"] = "__mutant__" }},
		{"inference_enum", "readings_malformed", func(r map[string]any) { r["inference"].(map[string]any)["state"] = "__mutant__" }},
		{"unknown_inference_reason", "readings_partial", func(r map[string]any) {
			r["inference"].(map[string]any)["state"] = "unknown"
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "resource_observation_unknown"
		}},
		{"observed_inference_reason", "readings_malformed", func(r map[string]any) { r["inference"].(map[string]any)["reason"] = "__mutant__" }},
		{"policy_mode", "readings_malformed", func(r map[string]any) {
			inactiveReadingsEnvelope(r)
			r["admission"] = "refused"
			r["policy"].(map[string]any)["mode"] = "__mutant__"
		}},
		{"negative_grace", "readings_malformed", func(r map[string]any) { r["policy"].(map[string]any)["eviction_grace_seconds"] = -1 }},
		{"overflow_grace", "readings_malformed", func(r map[string]any) { r["policy"].(map[string]any)["eviction_grace_seconds"] = 9223372037 }},
		{"pressure_action", "readings_malformed", func(r map[string]any) { r["policy"].(map[string]any)["pressure_action"] = "__mutant__" }},
		{"unknown_action", "readings_malformed", func(r map[string]any) { r["policy"].(map[string]any)["unknown_action"] = "__mutant__" }},
		{"busy_action", "readings_malformed", func(r map[string]any) { r["policy"].(map[string]any)["busy_action"] = "__mutant__" }},
		{"pressure_threshold", "readings_malformed", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["pressure_at_bytes"] = 0 }},
		{"recovery_threshold", "readings_malformed", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["recovery_at_bytes"] = 0 }},
		{"threshold_order", "readings_malformed", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["recovery_at_bytes"] = 1000 }},
		{"provider_policy_mismatch", "readings_malformed", func(r map[string]any) {
			r["policy"] = map[string]any{"mode": "disabled", "eviction_grace_seconds": 0}
			r["state"] = "unknown"
			r["admission"] = "not-enforced"
			r["reason"] = "resource_pressure_disabled"
			r["loaded_model_memory"] = map[string]any{"state": "unknown", "reason": "unknown"}
			r["inference"] = map[string]any{"state": "unknown", "reason": "unknown"}
			m := r["loaded_model_memory"].(map[string]any)
			delete(m, "pressure_at_bytes")
			delete(m, "recovery_at_bytes")
		}},
		{"contradictory_state", "readings_malformed", func(r map[string]any) { r["state"] = "busy" }},
		{"contradictory_admission", "readings_malformed", func(r map[string]any) { r["admission"] = "not-enforced" }},
		{"malformed_schema", "readings_malformed", func(r map[string]any) { r["schema"] = "__mutant__" }},
		{"malformed_source", "readings_malformed", func(r map[string]any) { inactiveReadingsEnvelope(r); r["source"] = "__mutant__" }},
		{"failed_provider", "readings_read_failed", func(r map[string]any) {
			r["source"] = "provider-observation-failed"
			delete(r, "observed_at")
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "failed"
			r["loaded_model_memory"] = map[string]any{"state": "unknown", "reason": "failed", "pressure_at_bytes": 1000, "recovery_at_bytes": 800}
			r["inference"] = map[string]any{"state": "unknown", "reason": "failed"}
		}},
		{"missing_timestamp", "readings_partial", func(r map[string]any) { delete(r, "observed_at") }},
		{"stale_timestamp", "readings_stale", func(r map[string]any) { r["observed_at"] = time.Now().Add(-31 * time.Second) }},
		{"future_timestamp", "readings_stale", func(r map[string]any) { r["observed_at"] = time.Now().Add(time.Minute) }},
		{"memory_type", "protocol_violation", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["bytes"] = "100" }},
		{"policy_type", "protocol_violation", func(r map[string]any) { r["policy"].(map[string]any)["eviction_grace_seconds"] = "0" }},
		{"unknown_field", "protocol_violation", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["extra"] = true }},
		{"missing_memory", "protocol_violation", func(r map[string]any) { delete(r, "loaded_model_memory") }},
		{"missing_inference", "protocol_violation", func(r map[string]any) { delete(r, "inference") }},
		{"missing_policy", "protocol_violation", func(r map[string]any) { delete(r, "policy") }},
		{"missing_grace", "protocol_violation", func(r map[string]any) { delete(r["policy"].(map[string]any), "eviction_grace_seconds") }},
		{"null_grace", "protocol_violation", func(r map[string]any) { r["policy"].(map[string]any)["eviction_grace_seconds"] = nil }},
		{"null_inference", "protocol_violation", func(r map[string]any) { r["inference"] = nil }},
		{"missing_policy_action", "protocol_violation", func(r map[string]any) { delete(r["policy"].(map[string]any), "busy_action") }},
		{"duplicate_policy_member", "protocol_violation", func(r map[string]any) {
			r["policy"] = json.RawMessage(`{"mode":"provider","eviction_grace_seconds":0,"busy_action":"observe","busy_action":"__mutant__","pressure_action":"refuse-new-drain-idle","unknown_action":"refuse-new"}`)
		}},
		{"negative_bytes", "protocol_violation", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["bytes"] = -1 }},
		{"null_timestamp", "protocol_violation", func(r map[string]any) { r["observed_at"] = nil }},
		{"available_resource_reason", "readings_malformed", func(r map[string]any) { r["reason"] = "__mutant__" }},
		{"healthy_memory_above_pressure", "readings_malformed", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["bytes"] = 1000 }},
		{"pressured_memory_below_recovery", "readings_malformed", func(r map[string]any) {
			r["loaded_model_memory"].(map[string]any)["state"] = "pressured"
			r["state"] = "pressured"
			r["admission"] = "refused"
			r["reason"] = "loaded_model_memory_pressure"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r map[string]any
			if err := json.Unmarshal(original, &r); err != nil {
				t.Fatal(err)
			}
			r["observed_at"] = time.Now().UTC()
			tc.change(r)
			code, stdout, stderr := execute(r)
			if strings.HasPrefix(tc.code, "readings_") {
				requireCLIReadingsFailure(t, code, stdout, stderr, tc.code)
				return
			}
			if code != 1 || stdout != "" || !strings.Contains(stderr, `"code":"`+tc.code+`"`) {
				t.Fatalf("exit=%d stdout=%s stderr=%s want %s", code, stdout, stderr, tc.code)
			}
		})
	}
}

// Inactive resource contradictions fail all facts without dropping broker status.
func TestStatusReadingsInactiveResourceEnvelope(t *testing.T) {
	execute, _ := readingsEnvelopeFixture(t)
	baseline := map[string]any{"schema": infra.SharedRuntimeResourceStatusSchema, "source": "broker-config", "state": "unknown", "admission": "not-enforced", "reason": "resource_pressure_disabled", "loaded_model_memory": map[string]any{"state": "unknown", "reason": "resource_pressure_disabled"}, "inference": map[string]any{"state": "unknown", "reason": "resource_pressure_disabled"}, "policy": map[string]any{"mode": "disabled", "eviction_grace_seconds": 0}}
	if code, _, stderr := execute(baseline); code != 0 {
		t.Fatal(code, stderr)
	}
	for _, change := range []struct {
		name, field string
		value       any
		section     string
	}{
		{"grace", "eviction_grace_seconds", 1, "policy"},
		{"pressure_action", "pressure_action", "__mutant__", "policy"},
		{"unknown_action", "unknown_action", "__mutant__", "policy"},
		{"busy_action", "busy_action", "__mutant__", "policy"},
		{"pressure_threshold", "pressure_at_bytes", 1, "loaded_model_memory"},
		{"recovery_threshold", "recovery_at_bytes", 1, "loaded_model_memory"},
		{"timestamp", "observed_at", time.Now().UTC(), ""},
		{"memory", "state", "healthy", "loaded_model_memory"},
		{"inference", "state", "idle", "inference"},
	} {
		t.Run(change.name, func(t *testing.T) {
			raw, err := json.Marshal(baseline)
			if err != nil {
				t.Fatal(err)
			}
			var r map[string]any
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			target := r
			if change.section != "" {
				target = r[change.section].(map[string]any)
			}
			target[change.field] = change.value
			if change.name == "memory" {
				target["bytes"] = 100
				delete(target, "reason")
			}
			if change.name == "inference" {
				delete(target, "reason")
			}
			code, stdout, stderr := execute(r)
			requireCLIReadingsFailure(t, code, stdout, stderr, "readings_malformed")
		})
	}
}

func inactiveReadingsEnvelope(r map[string]any) {
	delete(r, "observed_at")
	r["state"] = "unknown"
	r["admission"] = "not-enforced"
	r["reason"] = "resource_pressure_disabled"
	r["loaded_model_memory"] = map[string]any{"state": "unknown", "reason": "resource_pressure_disabled"}
	r["inference"] = map[string]any{"state": "unknown", "reason": "resource_pressure_disabled"}
	r["policy"] = map[string]any{"mode": "disabled", "eviction_grace_seconds": 0}
	r["source"] = "broker-config"
}

func TestStatusReadingsResourceEnvelopeBranches(t *testing.T) {
	execute, resources := readingsEnvelopeFixture(t)
	original, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"idle", func(map[string]any) {}},
		{"busy", func(r map[string]any) { r["state"] = "busy"; r["inference"].(map[string]any)["state"] = "busy" }},
		{"pressured", func(r map[string]any) {
			r["state"] = "pressured"
			r["admission"] = "refused"
			r["reason"] = "loaded_model_memory_pressure"
			m := r["loaded_model_memory"].(map[string]any)
			m["state"] = "pressured"
			m["bytes"] = 1000
		}},
		{"healthy_hysteresis_band", func(r map[string]any) { r["loaded_model_memory"].(map[string]any)["bytes"] = 900 }},
		{"pressured_hysteresis_band", func(r map[string]any) {
			r["state"] = "pressured"
			r["admission"] = "refused"
			r["reason"] = "loaded_model_memory_pressure"
			m := r["loaded_model_memory"].(map[string]any)
			m["state"] = "pressured"
			m["bytes"] = 900
		}},
		{"unknown_memory", func(r map[string]any) {
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "resource_observation_unknown"
			m := r["loaded_model_memory"].(map[string]any)
			m["state"] = "unknown"
			m["reason"] = "unknown"
			delete(m, "bytes")
		}},
		{"unknown_inference", func(r map[string]any) {
			r["state"] = "unknown"
			r["admission"] = "refused"
			r["reason"] = "resource_observation_unknown"
			r["inference"] = map[string]any{"state": "unknown", "reason": "unknown"}
		}},
		{"draining", func(r map[string]any) {
			r["state"] = "draining"
			r["admission"] = "refused"
			r["reason"] = "broker_draining"
		}},
		{"unknown_policy", func(r map[string]any) {
			inactiveReadingsEnvelope(r)
			r["admission"] = "refused"
			r["policy"].(map[string]any)["mode"] = "unknown"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r map[string]any
			if err := json.Unmarshal(original, &r); err != nil {
				t.Fatal(err)
			}
			r["observed_at"] = time.Now().UTC()
			tc.change(r)
			code, stdout, stderr := execute(r)
			if code != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			var status infra.SharedRuntimeStatus
			if err := json.Unmarshal([]byte(stdout), &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Readings.Facts) != 17 {
				t.Fatal("partial readings", status.Readings)
			}
			for _, row := range status.Readings.Facts {
				if row.Fact == "inference-busy" {
					if tc.name == "unknown_inference" || tc.name == "unknown_policy" {
						if row.Outcome != "not-observed" || row.Reason == "" {
							t.Fatal(row)
						}
					} else {
						want := `{"busy":false}`
						if tc.name == "busy" {
							want = `{"busy":true}`
						}
						if row.Value != want {
							t.Fatal(row)
						}
					}
				}
			}
		})
	}
}
