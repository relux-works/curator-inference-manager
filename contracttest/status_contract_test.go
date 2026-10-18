package contracttest_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	for _, name := range []string{"idle", "absent", "serving-with-leases", "quarantined", "refused-weights", "readings-failure", "readings-malformed", "readings-partial", "readings-stale", "record-starting", "record-serving", "record-lingering", "record-draining", "record-unverified-stale", "record-unknown-state", "record-", "record-absent", "record-starting-unverified", "profile-readings-failure"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(outputDir, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			engine := "profile"
			if name == "profile-readings-failure" {
				engine = "catalog"
			}
			if err := decodeConsumerStatus(raw, engine); err != nil {
				t.Fatalf("producer emitted undecodable %s: %v", name, err)
			}
			// Consumer decode.go:254-283 also admits the two historical producer
			// generations. Derive these by removing only the corresponding cohorts.
			fields := statusFields(t, raw)
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
