package contracttest_test

import (
	"encoding/json"
	"errors"
	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
	"github.com/relux-works/skill-agents-management/pkg/plugin"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fresh production report paths (with an in-memory attestation transport) must
// remain decodable by the pinned localruntime rules. This does not prove a live
// broker exchange; socket integration is tested separately by the producer.
func TestGeneratedStatusConsumerContract(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("status production paths require Darwin")
	}
	outputDir := t.TempDir()
	command := exec.Command("go", "test", "-count=1", "./internal/infra", "-run", "^TestWriteStatusConsumerContractFixtures$")
	command.Dir = ".."
	command.Env = append(os.Environ(), "CURATOR_ENGINE_STATUS_CONTRACT_OUT="+outputDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("producer fixture generation: %v\n%s", err, output)
	}
	for _, name := range []string{"verified-weights", "verified-weights-cleaned", "verified-weights-bin", "idle", "absent", "serving-with-leases", "quarantined", "refused-weights", "readings-failure", "readings-malformed", "readings-partial", "readings-stale", "record-starting", "record-serving", "record-lingering", "record-draining", "record-unverified-stale", "record-unknown-state", "record-", "record-absent", "record-starting-unverified", "profile-readings-failure"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(outputDir, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			engine := "profile"
			if strings.HasPrefix(name, "verified-weights") {
				engine = name
			}
			if name == "profile-readings-failure" {
				engine = "catalog"
			}
			if err := decodeConsumerStatus(raw, engine); err != nil {
				t.Fatalf("producer emitted undecodable %s: %v", name, err)
			}
			// Consumer decode.go:254-283 also admits the two historical producer
			// generations. Derive these by removing only the corresponding cohorts.
			fields := statusFields(t, raw)
			// The additive member remains decodable both present and absent.
			if fields["provenance"] == nil {
				fields["provenance"] = json.RawMessage(`{"contract_version":"status-provenance/v1","engines_config":"/catalog/engines.toml","engines_config_sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","cache_root":"/cache"}`)
				if err := decodeConsumerStatus(statusBytes(t, fields), engine); err != nil {
					t.Fatal("status with provenance", err)
				}
			}
			delete(fields, "provenance")
			if err := decodeConsumerStatus(statusBytes(t, fields), engine); err != nil {
				t.Fatal("status without provenance", err)
			}
			for _, group := range [][]string{{"restart_not_before", "half_open"}, {"restart_count", "quarantined_until", "last_readiness_match", "manual_quarantine"}} {
				for _, key := range group {
					delete(fields, key)
				}
				if err := decodeConsumerStatus(statusBytes(t, fields), engine); err != nil {
					t.Fatal("historical restart cohort", err)
				}
			}
			checkConsumerNegativeControls(t, raw, engine)
			checkReadingsShape(t, raw)
			if strings.HasPrefix(name, "verified-weights") {
				checkWeightArtifactValue(t, raw, name)
			}
		})
	}
}

func statusFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
func statusBytes(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Bounded violation plants prove the oracle detects an unknown joint pair,
// missing required members, wrong version, malformed runtime and partial groups.
// Every plant starts from the neighboring accepted real producer document.
func checkConsumerNegativeControls(t *testing.T, raw []byte, engine string) {
	t.Helper()
	changes := map[string]func(map[string]json.RawMessage){
		"pair": func(f map[string]json.RawMessage) {
			f["broker"] = json.RawMessage(`{"state":"refused","source":"record-derived-unverified"}`)
		},
		"impossible-pair": func(f map[string]json.RawMessage) {
			f["broker"] = json.RawMessage(`{"state":"serving","source":"determined"}`)
		},
		"version":            func(f map[string]json.RawMessage) { f["contract_version"] = json.RawMessage(`2`) },
		"fractional-version": func(f map[string]json.RawMessage) { f["contract_version"] = json.RawMessage(`1.5`) },
		"identity": func(f map[string]json.RawMessage) {
			f["engine_identity"] = json.RawMessage(`{"name":"other","key":"key"}`)
		},
		"max-leases": func(f map[string]json.RawMessage) { f["sharing"] = json.RawMessage(`{"configured":{"max_leases":-1}}`) },
		"runtime": func(f map[string]json.RawMessage) {
			f["runtime"] = json.RawMessage(`{"pid":1,"start_time":{"seconds":0,"microseconds":1000000}}`)
		},
		"runtime-required": func(f map[string]json.RawMessage) {
			f["runtime"] = json.RawMessage(`{"pid":1,"start_time":{"seconds":0}}`)
		},
		"restart-base":      func(f map[string]json.RawMessage) { delete(f, "manual_quarantine") },
		"restart-extension": func(f map[string]json.RawMessage) { delete(f, "half_open") },
		"restart-negative":  func(f map[string]json.RawMessage) { f["restart_count"] = json.RawMessage(`-1`) },
		"restart-bool":      func(f map[string]json.RawMessage) { f["half_open"] = json.RawMessage(`null`) },
		"restart-time":      func(f map[string]json.RawMessage) { f["restart_not_before"] = json.RawMessage(`"invalid"`) },
	}
	for _, key := range []string{"contract_version", "engine_identity", "runtime_key", "profile_digest", "broker", "sharing", "leases"} {
		changes["missing-"+key] = func(f map[string]json.RawMessage) { delete(f, key) }
	}
	for name, change := range changes {
		fields := statusFields(t, raw)
		change(fields)
		if err := decodeConsumerStatus(statusBytes(t, fields), engine); !errors.Is(err, ErrDecodeFailure) {
			t.Fatalf("plant %s: want ErrDecodeFailure, got %v", name, err)
		}
	}
}

// Consumer pkg/engineobservation/adapter.go:245-310, 344-389: v3's closed
// envelope/row members and outcome-dependent shapes accept failed observations.
func checkReadingsShape(t *testing.T, raw []byte) {
	t.Helper()
	fields := statusFields(t, raw)
	readings := statusFields(t, fields["readings"])
	if len(readings) != 2 || string(readings["contract_version"]) != `"observed-process/v3"` {
		t.Fatal("readings envelope")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(readings["facts"], &rows); err != nil || len(rows) != 17 {
		t.Fatal("readings facts", err)
	}
	allowed := map[string]bool{"fact": true, "outcome": true, "value": true, "cause": true, "reason": true, "source": true}
	for _, row := range rows {
		for key := range row {
			if !allowed[key] {
				t.Fatal("unknown readings row member", key)
			}
		}
		get := func(key string) string {
			var value string
			if err := json.Unmarshal(row[key], &value); err != nil || value == "" {
				t.Fatal("invalid readings string", key, err)
			}
			return value
		}
		get("fact")
		get("source")
		switch get("outcome") {
		case "observed-value":
			get("value")
			if row["cause"] != nil || row["reason"] != nil {
				t.Fatal("value with failure")
			}
		case "observed-absent":
			if row["value"] != nil || row["cause"] != nil || row["reason"] != nil {
				t.Fatal("absence with value/failure")
			}
		case "not-observed":
			get("reason")
			cause := get("cause")
			if row["value"] != nil || cause != "read-failure" && cause != "malformed" && cause != "unsupported" {
				t.Fatal("invalid failed reading", cause)
			}
		default:
			t.Fatal("unknown outcome")
		}
	}
}

// The consumer's actual value validator accepts cleaned paths and the status
// decoder accepts a degraded .bin fact. Probe only artifact/profile syntax;
// this does not assert complete runtime observation or readiness.
func checkWeightArtifactValue(t *testing.T, raw []byte, name string) {
	t.Helper()
	fields := statusFields(t, raw)
	var readings struct {
		Facts []struct{ Fact, Outcome, Value, Cause, Reason, Source string }
	}
	if err := json.Unmarshal(fields["readings"], &readings); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range readings.Facts {
		if row.Fact != "weight-artifact" {
			continue
		}
		found = true
		if name == "verified-weights-bin" {
			if row.Outcome != "not-observed" || row.Cause != "unsupported" || row.Value != "" || row.Reason == "" {
				t.Fatalf("invalid .bin degradation: %+v", row)
			}
			continue
		}
		if row.Outcome != "observed-value" || row.Source != "broker-verified-weights" {
			t.Fatal(row)
		}
		var shape struct {
			ModelPath string `json:"model_path"`
		}
		if err := json.Unmarshal([]byte(row.Value), &shape); err != nil || shape.ModelPath != "/models/fixture.gguf" {
			t.Fatalf("artifact path %q: %v", row.Value, err)
		}
		probe := func(value string) error {
			var facts []inferenceengine.Reading
			for _, definition := range inferenceengine.MeasuredFacts() {
				read := inferenceengine.ReadAbsent()
				switch definition.Fact {
				case inferenceengine.FactLocalExecutable:
					read = inferenceengine.ReadValue("/bin/echo")
				case inferenceengine.FactLocalArgv:
					read = inferenceengine.ReadValue(`["--port","18012"]`)
				case inferenceengine.FactWeightArtifact:
					read = inferenceengine.ReadValue(value)
				}
				facts = append(facts, inferenceengine.NewReading(definition.Fact, read))
			}
			_, err := inferenceengine.ValidateReadings(plugin.ID("fixture"), inferenceengine.EngineKindGGUFServer, facts)
			return err
		}
		if err := probe(row.Value); err != nil {
			t.Fatalf("consumer artifact contract: %v", err)
		}
		// Bounded plants prove this validator rejects both original violations.
		for _, invalid := range []string{`{"format":"gguf","model_path":"/models/model.bin"}`, `{"format":"gguf","model_path":"/models/../models/fixture.gguf"}`} {
			if err := probe(invalid); !errors.Is(err, inferenceengine.ErrObservationMalformed) {
				t.Fatalf("invalid artifact escaped consumer: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("artifact reading missing")
	}
}
