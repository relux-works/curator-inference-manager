//go:build darwin

package infra

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
)

// Uses the production report/record/readings paths with only the attestation
// transport substituted. It proves serialization, not kernel/socket attestation.
func TestWriteStatusConsumerContractFixtures(t *testing.T) {
	project, home, cache, resolved := newSharedIntegrationProfile(t)
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: "profile"}
	oldConnect, oldWrite, oldRead, oldNow := sharedRuntimeStatusConnect, sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage, sharedRuntimeReadingsNow
	t.Cleanup(func() {
		sharedRuntimeStatusConnect, sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage, sharedRuntimeReadingsNow = oldConnect, oldWrite, oldRead, oldNow
	})
	now := time.Now().UTC()
	sharedRuntimeReadingsNow = func() time.Time { return now }
	sharedRuntimeStatusConnect = func(sharedResolvedProfile, PiStatePaths, string, *http.Client, time.Duration) (*sharedAttestedRuntime, error) {
		return nil, &sharedConnectError{err: syscall.ENOENT}
	}
	emit := func(name string) SharedRuntimeStatus {
		t.Helper()
		report, err := SharedRuntimeStatusReport(options)
		if err != nil {
			t.Fatal(name, err)
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateSharedRuntimeStatusJSON(data, options.Profile); err != nil {
			t.Fatal(name, err)
		}
		if out := os.Getenv("CURATOR_ENGINE_STATUS_CONTRACT_OUT"); out != "" {
			if err := os.WriteFile(filepath.Join(out, name+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return report
	}
	emit("absent")
	if _, err := SetSharedRuntimeManualQuarantine(options, true); err != nil {
		t.Fatal(err)
	}
	if report := emit("quarantined"); !report.ManualQuarantine {
		t.Fatal("quarantine lost")
	}
	if _, err := SetSharedRuntimeManualQuarantine(options, false); err != nil {
		t.Fatal(err)
	}
	own, err := inspectSharedProcessKernel(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	broker := SharedBrokerIdentity{PID: own.PID, UID: own.UID, StartTime: own.StartTime}
	if !sharedRecordedBrokerIsLive(broker) {
		t.Fatal("fixture must exercise a still-live broker record")
	}
	for _, state := range []string{"starting", "serving", "lingering", "draining", "unverified-stale", "refused", "unknown-state", "", "absent", "starting-unverified"} {
		record := SharedBrokerRecord{State: state, Broker: broker, RuntimeKey: resolved.RuntimeKey, ProfileDigest: resolved.ProfileDigest}
		if state == "refused" {
			record.StartupFailure = &engineWeightsFailureRecord{Code: "engine_weights_mismatch"}
		}
		if err := writeSharedJSONAtomic(resolved.Paths.BrokerState, record); err != nil {
			t.Fatal(err)
		}
		name := "record-" + state
		if state == "refused" {
			name = "refused-weights"
		}
		report := emit(name)
		if state == "refused" && (report.Broker.State != "unverified-stale" || report.Broker.Reason != "engine_weights_mismatch" || report.Weights.Verification != "mismatch") {
			t.Fatalf("refused live record: %#v", report)
		}
		if state == "unknown-state" && (report.Broker.State != "unverified-stale" || report.Broker.Reason != "broker_state_unrecognized") {
			t.Fatal(report.Broker)
		}
	}
	runtime := SharedRuntimeProcessRecord{PID: own.PID, StartTime: own.StartTime, Stage: "running", Endpoint: resolved.Profile.BaseURL}
	attested := &sharedAttestedRuntime{broker: broker, runtime: runtime, effective: resolved.Sharing}
	sharedRuntimeStatusConnect = func(sharedResolvedProfile, PiStatePaths, string, *http.Client, time.Duration) (*sharedAttestedRuntime, error) {
		return attested, nil
	}
	sharedRuntimeStatusWriteMessage = func(io.Writer, sharedWireMessage) error { return nil }
	resources := validReadingsResourceStatus(now, SharedRuntimeInferenceIdle)
	message := sharedWireMessage{Type: "status", State: "serving", Stage: "composed", Broker: &broker, Runtime: &runtime, EffectiveSharing: &resolved.Sharing, Resources: &resources}
	sharedRuntimeStatusReadMessage = func(io.Reader) (sharedWireMessage, error) { return message, nil }
	if report := emit("idle"); len(report.Leases) != 0 {
		t.Fatal("idle leases")
	}
	message.Leases = []SharedLeaseStatus{{SharedLeaseRecord: SharedLeaseRecord{
		LeaseID: "fixture-lease", LeaseLifetime: "client_process", EngineIdentity: resolved.EngineIdentity,
		RuntimeKey: resolved.RuntimeKey, ProfileDigest: resolved.ProfileDigest, Endpoint: resolved.Profile.BaseURL,
		RuntimePID: runtime.PID, ClientPID: own.PID, ClientStartTime: own.StartTime,
	}, State: "held"}}
	if report := emit("serving-with-leases"); len(report.Leases) != 1 {
		t.Fatal("lease lost")
	}
	good := resources
	for _, tc := range []struct {
		name, code string
		change     func(*SharedRuntimeResourceStatus)
	}{
		{"readings-failure", "readings_read_failed", func(r *SharedRuntimeResourceStatus) { r.Source = "provider-observation-failed" }},
		{"readings-malformed", "readings_malformed", func(r *SharedRuntimeResourceStatus) { r.Schema = "invalid" }},
		{"readings-partial", "readings_partial", func(r *SharedRuntimeResourceStatus) { r.ObservedAt = nil }},
		{"readings-stale", "readings_stale", func(r *SharedRuntimeResourceStatus) { stamp := now.Add(-31 * time.Second); r.ObservedAt = &stamp }},
	} {
		resources = good
		tc.change(&resources)
		report := emit(tc.name)
		if report.Broker.Source != "attested" || report.Broker.State != "serving" || report.Runtime == nil || len(report.Leases) != 1 {
			t.Fatal("readings failure lost status")
		}
		requireFailedStatusReadings(t, report, tc.code)
	}
	// An invalid broker envelope still refuses, even when resources also fail.
	message.State = "refused"
	_, err = SharedRuntimeStatusReport(options)
	requireSharedAttestationCode(t, err, "protocol_violation")
	// A runnable, non-clean executable path fails only the readings projection.
	// Attestation must still complete without replacing failed profile facts.
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"catalog": {
		Mode: "local", Engine: "mlx-lm", Model: "model", ServedModelID: "model",
		Executable: "/bin/../bin/echo", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}})
	options.Profile = "catalog"
	resolved, err = resolveSharedProfile(project, home, cache, options.Profile)
	if err != nil || resolved.NotRunnableCode != "" {
		t.Fatalf("runnable profile control: %v %s", err, resolved.NotRunnableCode)
	}
	runtime.Endpoint = resolved.Profile.BaseURL
	attested.runtime = runtime
	resources = good
	message.State = "serving"
	message.Leases = nil
	report := emit("profile-readings-failure")
	if report.Broker.Source != "attested" || report.Runtime == nil || report.EngineAvailability != "available" {
		t.Fatal("profile readings failure lost attested status")
	}
	requireFailedStatusReadings(t, report, "readings_malformed")

	// Verification may succeed for renamed GGUF bytes. Publish only paths that
	// satisfy the consumer artifact shape, independently of digest verification.
	id := "gguf-sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, path, wantPath string }{
		{"verified-weights", "/models/fixture.gguf", "/models/fixture.gguf"},
		{"verified-weights-cleaned", "/models/../models/fixture.gguf", "/models/fixture.gguf"},
		{"verified-weights-bin", "/models/model.bin", ""},
	} {
		writeEngineCatalog(t, home, map[string]EngineCatalogEntry{tc.name: {
			Mode: "local", Engine: "llama-cpp", Model: tc.path, ServedModelID: "fixture-model", Executable: "/bin/echo",
			Argv: []string{"--host", "{host}", "--port", "{port}"}, Weights: &EngineCatalogWeights{ID: id},
		}})
		options.Profile = tc.name
		resolved, err = resolveSharedProfile(project, home, cache, options.Profile)
		if err != nil {
			t.Fatal(err)
		}
		runtime.Endpoint = resolved.Profile.BaseURL
		runtime.Weights = &EngineWeightsObservation{ID: id, ObservedAt: now}
		attested.runtime = runtime
		message.EffectiveSharing = &resolved.Sharing
		report = emit(tc.name)
		if report.Weights.Verification != "verified" {
			t.Fatal(report.Weights)
		}
		found := false
		for _, row := range report.Readings.Facts {
			if row.Fact != inferenceengine.FactWeightArtifact {
				continue
			}
			found = true
			if tc.wantPath == "" {
				if row.Outcome != inferenceengine.OutcomeNotObserved || row.Cause != inferenceengine.NotObservedUnsupported || row.Value != "" || row.Reason == "" {
					t.Fatalf("unrepresentable artifact: %+v", row)
				}
			} else {
				var shape struct {
					ModelPath string `json:"model_path"`
				}
				if err := json.Unmarshal([]byte(row.Value), &shape); err != nil || shape.ModelPath != tc.wantPath || row.Outcome != inferenceengine.OutcomeObservedValue || row.Source != "broker-verified-weights" {
					t.Fatalf("verified artifact: %+v %v", row, err)
				}
			}
		}
		if !found {
			t.Fatal("artifact reading missing")
		}
	}
}

func requireFailedStatusReadings(t *testing.T, report SharedRuntimeStatus, code string) {
	t.Helper()
	if report.Readings.ContractVersion != inferenceengine.ContractVersionV3 || len(report.Readings.Facts) != len(inferenceengine.MeasuredFacts()) {
		t.Fatal("incomplete failed readings")
	}
	cause := inferenceengine.NotObservedReadFailure
	if code == "readings_partial" || code == "readings_malformed" {
		cause = inferenceengine.NotObservedMalformed
	}
	for _, row := range report.Readings.Facts {
		if row.Outcome != inferenceengine.OutcomeNotObserved || row.Reason != code || row.Value != "" || row.Cause != cause {
			t.Fatalf("failure published a value or wrong code: %#v", row)
		}
	}
}
