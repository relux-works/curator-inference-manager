//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
	"github.com/relux-works/skill-agents-management/pkg/plugin"
)

func readingsCatalogEntry() infra.EngineCatalogEntry {
	return infra.EngineCatalogEntry{Engine: "mlx-lm", Mode: "local", Model: "org/qwen", ServedModelID: "qwen", Executable: "/missing/mlx", Argv: []string{"serve", "--host", "{host}", "--port", "{port}", "--model", "{model}"}, Knobs: &infra.EngineCatalogKnobs{KVContextTokens: "8192", PrefillChunkTokens: "512"}, Stress: &infra.EngineCatalogStressPolicy{PromptTokens: 1024, MaxOutputTokens: 1, StartupTimeoutSeconds: 1, RequestTimeoutSeconds: 1, SampleIntervalMilliseconds: 50}, Supervision: &infra.EngineCatalogSupervisionPolicy{FatalOutputSubstrings: []string{"fatal"}, RestartOnFailure: true, MaxRestarts: 1, RestartWindowSeconds: 1, RestartDelayMilliseconds: 0}}
}

func statusReadings(t *testing.T, entry infra.EngineCatalogEntry) infra.EngineReadings {
	t.Helper()
	prepareCatalogCLIEntry(t, "readings", entry)
	report, raw := runStatus(t, []string{"status", "--engine", "readings", "--json"})
	if report.Broker.State != "absent" || report.ContractVersion != 1 {
		t.Fatal("status fields changed", report)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["readings"]; !present {
		t.Fatal("CLI did not encode readings")
	}
	if report.Readings.ContractVersion != inferenceengine.ContractVersionV3 || len(report.Readings.Facts) != len(inferenceengine.MeasuredFacts()) {
		t.Fatal("version/inventory mismatch", report.Readings)
	}
	for i, definition := range inferenceengine.MeasuredFacts() {
		if report.Readings.Facts[i].Fact != definition.Fact {
			t.Fatalf("missing/out-of-order fact %s", definition.Fact)
		}
	}
	return report.Readings
}

// Runtime absence in a schema-only control is not healthy-engine evidence.
func schemaReadings(report infra.EngineReadings, preserveUnknown bool) []inferenceengine.Reading {
	var readings []inferenceengine.Reading
	for _, row := range report.Facts {
		outcome := inferenceengine.ReadAbsent()
		switch row.Outcome {
		case inferenceengine.OutcomeObservedValue:
			outcome = inferenceengine.ReadValue(row.Value)
		case inferenceengine.OutcomeNotObserved:
			if preserveUnknown {
				outcome = inferenceengine.ReadFailure(row.Cause, row.Reason)
			}
		}
		readings = append(readings, inferenceengine.NewReading(row.Fact, outcome))
	}
	return readings
}

func TestMainCatalogAdmitsPoliciesPinnedObservationSchemaCanRepresent(t *testing.T) {
	entry := readingsCatalogEntry()
	report := statusReadings(t, entry)
	resolution, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, false), inferenceengine.ContractVersionV3)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Contract.Version != inferenceengine.ContractVersionV3 {
		t.Fatal(resolution)
	}
	for _, row := range report.Facts {
		switch row.Fact {
		case inferenceengine.FactContextArgv:
			if row.Value != `["--max-kv-size","8192"]` {
				t.Fatal(row)
			}
		case inferenceengine.FactPrefillArgv:
			if row.Value != `["--prefill-step-size","512"]` {
				t.Fatal(row)
			}
		case inferenceengine.FactLocalExecutable:
			if row.Value != entry.Executable {
				t.Fatal(row)
			}
		case inferenceengine.FactLocalArgv:
			var argv []string
			if err := json.Unmarshal([]byte(row.Value), &argv); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(argv[len(argv)-4:], []string{"--max-kv-size", "8192", "--prefill-step-size", "512"}) {
				t.Fatal(argv)
			}
		}
	}
}

func TestPinnedStressSchemaAcceptsExactCatalogFieldsV3(t *testing.T) {
	report := statusReadings(t, readingsCatalogEntry())
	for _, row := range report.Facts {
		if row.Fact == inferenceengine.FactStressPolicy && row.Value != `{"max_output_tokens":1,"prompt_tokens":1024,"request_timeout_seconds":1,"sample_interval_milliseconds":50,"startup_timeout_seconds":1}` {
			t.Fatal(row)
		}
	}
	if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, false), inferenceengine.ContractVersionV3); err != nil {
		t.Fatal(err)
	}
	// The additive release must not loosen observed-process/v2.
	if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, false), inferenceengine.ContractVersion); !errors.Is(err, inferenceengine.ErrObservationMalformed) {
		t.Fatalf("v3-shaped policy admitted under v2: %v", err)
	}
}

func TestPinnedRestartSchemaAcceptsCatalogZeroDelayAndAbsentSectionsV3(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(map[bool]string{true: "configured", false: "not configured"}[configured], func(t *testing.T) {
			entry := readingsCatalogEntry()
			if !configured {
				entry.Stress = nil
				entry.Supervision = nil
			}
			report := statusReadings(t, entry)
			for _, row := range report.Facts {
				if row.Fact == inferenceengine.FactRestartSupervisionPolicy {
					want := `{"fatal_output_substrings":["fatal"],"max_restarts":1,"restart_delay_milliseconds":0,"restart_on_failure":true,"restart_window_seconds":1}`
					if !configured {
						want = `{"configured":false}`
					}
					if row.Value != want {
						t.Fatal(row)
					}
				}
				if !configured && row.Fact == inferenceengine.FactStressPolicy && row.Value != `{"configured":false}` {
					t.Fatal(row)
				}
			}
			if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, false), inferenceengine.ContractVersionV3); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPinnedNotObservedCannotCountAsPassingConformanceV3(t *testing.T) {
	report := statusReadings(t, readingsCatalogEntry())
	for _, row := range report.Facts {
		if row.Source == "unavailable" && (row.Outcome != inferenceengine.OutcomeNotObserved || row.Reason == "") {
			t.Fatal(row)
		}
	}
	if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, true), inferenceengine.ContractVersionV3); !errors.Is(err, inferenceengine.ErrObservationUnsupported) {
		t.Fatalf("unknown engine claimed conformance: %v", err)
	}
}

func TestStatusReadingsSSHProfileExpansion(t *testing.T) {
	entry := infra.EngineCatalogEntry{Engine: "mlx-lm", Mode: "ssh", SSHTarget: "host", RemoteExecutable: "/bin/model-harness", RemoteProfile: "qwen", RemoteHost: "127.0.0.1", RemotePort: 18012}
	report := statusReadings(t, entry)
	for _, row := range report.Facts {
		if (row.Fact == inferenceengine.FactLocalExecutable || row.Fact == inferenceengine.FactLocalArgv) && row.Outcome != inferenceengine.OutcomeObservedAbsent {
			t.Fatal(row)
		}
		if row.Fact == inferenceengine.FactSSHForwarding {
			var forwarding struct {
				Host                  string `json:"host"`
				LocalPort, RemotePort int
			}
			if err := json.Unmarshal([]byte(row.Value), &forwarding); err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal([]byte(row.Value), &value); err != nil {
				t.Fatal(err)
			}
			if value["host"] != entry.SSHTarget || value["remote_port"] != float64(entry.RemotePort) {
				t.Fatal(value)
			}
		}
	}
	if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), inferenceengine.EngineKindNativeTransformer, schemaReadings(report, false), inferenceengine.ContractVersionV3); err != nil {
		t.Fatal(err)
	}
}

// Read-only retained-engine probe. Unknown is a skipped conformance run, not a
// passing healthy round trip. It never acquires a lease or changes the engine.
func TestRetainedLocalEngineReadingsConformanceV3(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("unknown: host home unavailable: %v", err)
	}
	retained := filepath.Join(home, ".local-engine")
	project := filepath.Join(retained, "engine-project")
	if _, err := os.Stat(project); err != nil {
		t.Skipf("unknown: retained engine project unavailable: %v", err)
	}
	t.Setenv("HOME", retained)
	t.Chdir(project)
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Skipf("unknown: retained engine status unavailable (exit %d): %s", code, stderr.String())
	}
	var report infra.SharedRuntimeStatus
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Readings.Facts) != len(inferenceengine.MeasuredFacts()) || report.Readings.ContractVersion != inferenceengine.ContractVersionV3 {
		t.Fatal("live readings version/inventory malformed")
	}
	for _, row := range report.Readings.Facts {
		if row.Outcome == inferenceengine.OutcomeNotObserved {
			t.Skipf("unknown: %s: %s", row.Fact, row.Reason)
		}
	}
	if _, err := inferenceengine.ValidateReadings(plugin.ID(report.EngineIdentity.Name), inferenceengine.EngineKindNativeTransformer, schemaReadings(report.Readings, true), inferenceengine.ContractVersionV3); err != nil {
		t.Fatal(err)
	}
}

// Invalid profile values produce typed failed facts while cold broker status survives.
func TestStatusReadingsRefusesCatalogValuesOutsidePinnedSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*infra.EngineCatalogEntry)
	}{
		{"non-clean executable", func(e *infra.EngineCatalogEntry) { e.Executable = "/missing/../mlx" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := readingsCatalogEntry()
			tc.change(&entry)
			prepareCatalogCLIEntry(t, "readings", entry)
			var stdout, stderr bytes.Buffer
			code := run([]string{"status", "--engine", "readings", "--json"}, &stdout, &stderr)
			requireCLIReadingsFailure(t, code, stdout.String(), stderr.String(), "readings_malformed")
		})
	}
}

func TestPanelContextAlias(t *testing.T) {
	for _, flag := range []string{"--ctx-size", "-c"} {
		t.Run(flag, func(t *testing.T) {
			entry := readingsCatalogEntry()
			entry.Engine = "llama-cpp"
			entry.Knobs = nil
			entry.Argv = append(entry.Argv, flag, "4096")
			report := statusReadings(t, entry)
			for _, row := range report.Facts {
				if row.Fact == inferenceengine.FactContextArgv && row.Value != `["`+flag+`","4096"]` {
					t.Fatal(row)
				}
			}
		})
	}
}

func TestPanelPrefillAlias(t *testing.T) {
	for _, flag := range []string{"--ubatch-size", "-ub"} {
		t.Run(flag, func(t *testing.T) {
			entry := readingsCatalogEntry()
			entry.Engine = "llama-cpp"
			entry.Knobs = nil
			entry.Argv = append(entry.Argv, flag, "512")
			report := statusReadings(t, entry)
			for _, row := range report.Facts {
				if row.Fact == inferenceengine.FactPrefillArgv && row.Value != `["`+flag+`","512"]` {
					t.Fatal(row)
				}
			}
		})
	}
}

func TestStatusReadingsCapacityValueIsNotAnOption(t *testing.T) {
	entry := readingsCatalogEntry()
	entry.Knobs = nil
	entry.Argv = append(entry.Argv, "--chat-template", "-c", "--threads", "-ub")
	report := statusReadings(t, entry)
	for _, row := range report.Facts {
		if (row.Fact == inferenceengine.FactContextArgv || row.Fact == inferenceengine.FactPrefillArgv) && row.Outcome != inferenceengine.OutcomeObservedAbsent {
			t.Fatal(row)
		}
	}
}

// A model-less mlx-swift or llama-cpp entry resolves to a runtime argv with an
// empty --model-id or --alias token. The entry is designed NotRunnable and
// status must keep working (the legacy-import admission read depends on it),
// so the unrepresentable argv degrades to an explicit NotObserved/malformed
// row instead of failing the report. The degraded set still refuses every
// consumer ValidateReadings call.
func TestStatusReadingsDegradesUnrepresentableResolvedArgv(t *testing.T) {
	for _, engine := range []string{"mlx-swift", "llama-cpp"} {
		t.Run(engine, func(t *testing.T) {
			entry := infra.EngineCatalogEntry{Engine: engine, Mode: "local", Executable: "/missing/mlx", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}, Knobs: &infra.EngineCatalogKnobs{KVContextTokens: "unbounded"}}
			if engine == "llama-cpp" {
				entry.Knobs = nil
			}
			prepareCatalogCLIEntry(t, "readings", entry)
			report, raw := runStatus(t, []string{"status", "--engine", "readings", "--json"})
			if strings.Contains(raw, "argv unrepresentable in closed schema") {
				t.Fatal("degradation probe leaked into publication")
			}
			seen := map[inferenceengine.Fact]infra.EngineReading{}
			for _, row := range report.Readings.Facts {
				seen[row.Fact] = row
			}
			argv := seen[inferenceengine.FactLocalArgv]
			if argv.Outcome != inferenceengine.OutcomeNotObserved || argv.Cause != inferenceengine.NotObservedMalformed || argv.Source != "engines.toml/v1" {
				t.Fatal(argv)
			}
			if !strings.Contains(argv.Reason, "token 6 empty") || !strings.Contains(argv.Reason, "served_model_id_missing") {
				t.Fatal(argv)
			}
			if executable := seen[inferenceengine.FactLocalExecutable]; executable.Outcome != inferenceengine.OutcomeObservedValue || executable.Value != entry.Executable {
				t.Fatal(executable)
			}
			shape := schemaReadings(report.Readings, false)
			for i, definition := range inferenceengine.MeasuredFacts() {
				if definition.Fact == inferenceengine.FactLocalArgv {
					shape[i] = inferenceengine.NewReading(inferenceengine.FactLocalArgv, inferenceengine.ReadFailure(argv.Cause, argv.Reason))
				}
			}
			kind := inferenceengine.EngineKindNativeTransformer
			if engine == "llama-cpp" {
				kind = inferenceengine.EngineKindGGUFServer
			}
			if _, err := inferenceengine.ValidateReadings(plugin.ID("readings"), kind, shape, inferenceengine.ContractVersionV3); !errors.Is(err, inferenceengine.ErrObservationMalformed) || !strings.Contains(err.Error(), string(inferenceengine.FactLocalArgv)) {
				t.Fatalf("degraded set admitted by consumer gate: %v", err)
			}
		})
	}
}

// The profile-shape gate still guards every published value when local argv
// degrades: a second entry-own value outside the schema fails all readings.
func TestStatusReadingsDegradedArgvKeepsGateOnOtherValues(t *testing.T) {
	entry := infra.EngineCatalogEntry{Engine: "mlx-swift", Mode: "local", Executable: "/missing/../mlx", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}}
	prepareCatalogCLIEntry(t, "readings", entry)
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--engine", "readings", "--json"}, &stdout, &stderr)
	requireCLIReadingsFailure(t, code, stdout.String(), stderr.String(), "readings_malformed")
}

// A model-carrying swift entry resolves to a schema-valid argv and publishes
// a local-argv value: degradation fires only on the expansion artifact.
func TestStatusReadingsModelCarryingSwiftPublishesLocalArgv(t *testing.T) {
	entry := infra.EngineCatalogEntry{Engine: "mlx-swift", Mode: "local", Model: "org/qwen", ServedModelID: "qwen", Executable: "/missing/mlx", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}}
	report := statusReadings(t, entry)
	for _, row := range report.Facts {
		if row.Fact == inferenceengine.FactLocalArgv {
			var argv []string
			if err := json.Unmarshal([]byte(row.Value), &argv); err != nil {
				t.Fatal(err)
			}
			if row.Outcome != inferenceengine.OutcomeObservedValue || !reflect.DeepEqual(argv[len(argv)-2:], []string{"--model-id", "qwen"}) {
				t.Fatal(argv)
			}
		}
	}
}

func requireCLIReadingsFailure(t *testing.T, code int, stdout, stderr, reason string) {
	t.Helper()
	var status infra.SharedRuntimeStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil || code != 0 || stderr != "" {
		t.Fatalf("exit=%d out=%s err=%s decode=%v", code, stdout, stderr, err)
	}
	if err := infra.ValidateSharedRuntimeStatusJSON([]byte(stdout), status.EngineIdentity.Name); err != nil {
		t.Fatal(err)
	}
	if len(status.Readings.Facts) != len(inferenceengine.MeasuredFacts()) {
		t.Fatal("partial readings")
	}
	for _, row := range status.Readings.Facts {
		if row.Outcome != inferenceengine.OutcomeNotObserved || row.Reason != reason || row.Value != "" {
			t.Fatalf("failed readings escaped: %#v", row)
		}
	}
}
