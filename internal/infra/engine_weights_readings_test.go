package infra

import (
	"encoding/json"
	"testing"

	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
)

// Only verified weights upgrade artifact shape; every other verification state
// keeps NotObserved. The successful row retains the existing closed envelope.
func TestEngineWeightsReadingsObservation(t *testing.T) {
	for _, state := range []string{"verified", "unknown", "unpinned", "mismatch"} {
		readings := EngineReadings{ContractVersion: inferenceengine.ContractVersionV3, Facts: []EngineReading{{Fact: inferenceengine.FactWeightArtifact, Outcome: inferenceengine.OutcomeNotObserved, Cause: inferenceengine.NotObservedUnsupported, Reason: "no verified weights", Source: "unavailable"}}}
		observeEngineWeightsReadings(&readings, EngineWeightsStatus{Verification: state}, "/models/model.gguf")
		row := readings.Facts[0]
		if state == "verified" {
			if row.Outcome != inferenceengine.OutcomeObservedValue || row.Cause != "" || row.Reason != "" || row.Source != "broker-verified-weights" {
				t.Fatalf("verified row: %+v", row)
			}
			var artifact struct {
				Format    string `json:"format"`
				ModelPath string `json:"model_path"`
			}
			if err := json.Unmarshal([]byte(row.Value), &artifact); err != nil || artifact.Format != "gguf" || artifact.ModelPath != "/models/model.gguf" {
				t.Fatalf("artifact %s: %v", row.Value, err)
			}
		} else if row.Outcome != inferenceengine.OutcomeNotObserved || row.Value != "" {
			t.Fatalf("%s upgraded artifact: %+v", state, row)
		}
		raw, err := json.Marshal(readings)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateStatusReadings(raw); err != nil {
			t.Fatal(err)
		}
	}
}

// Verified byte identity does not imply a representable artifact path: clean
// absolute GGUF paths publish values; .bin and relative paths degrade one fact.
func TestEngineWeightsReadingsPathContract(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/models/model.gguf", "/models/model.gguf"},
		{"/models/../models/model.gguf", "/models/model.gguf"},
		{"/models/model.GGUF", "/models/model.GGUF"},
		{"/models/model.bin", ""},
		{"models/model.gguf", ""},
	} {
		readings := EngineReadings{ContractVersion: inferenceengine.ContractVersionV3, Facts: []EngineReading{{Fact: inferenceengine.FactWeightArtifact}}}
		observeEngineWeightsReadings(&readings, EngineWeightsStatus{Verification: "verified"}, tc.path)
		row := readings.Facts[0]
		if tc.want == "" {
			if row.Outcome != inferenceengine.OutcomeNotObserved || row.Value != "" || row.Cause != inferenceengine.NotObservedUnsupported || row.Reason == "" {
				t.Fatalf("%s published invalid artifact: %+v", tc.path, row)
			}
		} else {
			var value struct {
				ModelPath string `json:"model_path"`
			}
			if err := json.Unmarshal([]byte(row.Value), &value); err != nil || value.ModelPath != tc.want || row.Outcome != inferenceengine.OutcomeObservedValue {
				t.Fatalf("%s: %+v %v", tc.path, row, err)
			}
		}
	}
}
