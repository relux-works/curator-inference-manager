//go:build darwin

package infra

import (
	"bytes"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const legacyPresenceBase = "[profiles.qwen]\nmode='local'\nengine='mlx-swift'\nmodel='org/qwen'\nexecutable='/usr/bin/false'\nargv=['serve','--host','{host}','--port','{port}']\n"
const legacyPresenceKnobs = "[profiles.qwen.knobs]\nkv_context_tokens='32768'\nprefill_chunk_tokens='2048'\nreasoning_effort='medium'\n"
const legacyPresenceSupervision = "[profiles.qwen.supervision]\nrestart_on_failure=true\nmax_restarts=3\nrestart_window_seconds=60\nrestart_delay_milliseconds=100\nfatal_output_substrings=['fatal']\n"

func TestImportLegacyPerFieldPresencePreviewApplyRollback(t *testing.T) {
	for _, tc := range []struct {
		name, source                string
		context, prefill, reasoning string
		restart                     bool
		delay                       int
	}{
		{"absent_table", legacyPresenceBase, "32768", "2048", "medium", true, 100},
		{"empty_table", legacyPresenceBase + "[profiles.qwen.knobs]\n", "32768", "2048", "medium", true, 100},
		{"present_value", legacyPresenceBase + "[profiles.qwen.knobs]\nkv_context_tokens='65536'\n", "65536", "2048", "medium", true, 100},
		{"explicit_false_zero", legacyPresenceBase + "[profiles.qwen.supervision]\nrestart_on_failure=false\nrestart_delay_milliseconds=0\n", "32768", "2048", "medium", false, 0},
		{"whole_profile_absence", "[profiles]\n", "32768", "2048", "medium", true, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := legacyImportFixture(t)
			write := func(source string) {
				t.Helper()
				if err := os.WriteFile(options.ConfigPath, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(legacyPresenceBase + legacyPresenceKnobs + legacyPresenceSupervision)
			legacyApply(t, options)
			initial := legacyReadTarget(t, options, legacyCatalogTarget)
			write(tc.source)
			before := legacyTree(t, options.HomeDir)
			preview, err := ImportLegacyEngineState(options, "preview")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("preview wrote destination")
			}
			applied, err := ImportLegacyEngineState(options, "apply")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(preview.FieldPlan, applied.FieldPlan) || !reflect.DeepEqual(preview.Targets, applied.Targets) {
				t.Fatalf("preview/apply plans differ: %+v %+v", preview, applied)
			}
			if tc.name == "absent_table" && preview.FieldPlan["qwen"]["knobs.kv_context_size"] != "preserve" {
				t.Fatalf("absent field plan: %+v", preview.FieldPlan)
			}
			if tc.name == "present_value" && preview.FieldPlan["qwen"]["knobs.kv_context_size"] != "set" {
				t.Fatalf("present field plan: %+v", preview.FieldPlan)
			}
			if tc.name == "explicit_false_zero" && (preview.FieldPlan["qwen"]["supervision.restart_on_failure"] != "set" || preview.FieldPlan["qwen"]["supervision.restart_delay_milliseconds"] != "set") {
				t.Fatalf("false/zero field plan: %+v", preview.FieldPlan)
			}
			if tc.name != "whole_profile_absence" && len(preview.FieldPlan["qwen"]) == 0 {
				t.Fatal("missing per-field plan")
			}
			path := filepath.Join(options.HomeDir, ".curator", "engines.toml")
			entry, _, err := ReadEngineCatalogEntry(path, "qwen")
			if err != nil {
				t.Fatal(err)
			}
			if entry.Knobs == nil || entry.Knobs.KVContextTokens != tc.context || entry.Knobs.PrefillChunkTokens != tc.prefill || entry.Knobs.ReasoningEffort != tc.reasoning {
				t.Fatalf("presence lost: %+v", entry.Knobs)
			}
			if entry.Supervision == nil || entry.Supervision.RestartOnFailure != tc.restart || entry.Supervision.RestartDelayMilliseconds != tc.delay || entry.Supervision.MaxRestarts != 3 || entry.Supervision.RestartWindowSeconds != 60 || !reflect.DeepEqual(entry.Supervision.FatalOutputSubstrings, []string{"fatal"}) {
				t.Fatalf("nested presence lost: %+v", entry.Supervision)
			}
			repeatedBefore := legacyTree(t, options.HomeDir)
			repeat, err := ImportLegacyEngineState(options, "apply")
			if err != nil || len(repeat.Targets) != 0 {
				t.Fatalf("repeat: %+v %v", repeat, err)
			}
			if !reflect.DeepEqual(repeatedBefore, legacyTree(t, options.HomeDir)) {
				t.Fatal("repeat changed bytes or metadata")
			}
			if len(applied.Targets) > 0 {
				if _, err := ImportLegacyEngineState(options, "rollback"); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(initial, legacyReadTarget(t, options, legacyCatalogTarget)) {
					t.Fatal("rollback did not restore pre-merge bytes")
				}
			}
		})
	}
}

func TestImportLegacyPerFieldUnownedDivergenceRefuses(t *testing.T) {
	options := legacyImportFixture(t)
	if err := os.WriteFile(options.ConfigPath, []byte(legacyPresenceBase+legacyPresenceKnobs), 0600); err != nil {
		t.Fatal(err)
	}
	legacyApply(t, options)
	path := filepath.Join(options.HomeDir, ".curator", "engines.toml")
	original := legacyReadTarget(t, options, legacyCatalogTarget)
	// Ownership is digest-bound; changing a comment preserves valid data but loses ownership.
	if err := os.WriteFile(path, append([]byte("# user-owned\n"), original...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(legacyTransactionRoot(options.HomeDir)); err != nil {
		t.Fatal(err)
	}
	// Absent knobs are neutral even in an unowned destination.
	if err := os.WriteFile(options.ConfigPath, []byte(legacyPresenceBase), 0600); err != nil {
		t.Fatal(err)
	}
	before := legacyTree(t, options.HomeDir)
	for _, action := range []string{"preview", "apply"} {
		report, err := ImportLegacyEngineState(options, action)
		if err != nil || len(report.Targets) != 0 {
			t.Fatalf("neutral %s: %+v %v", action, report, err)
		}
	}
	for _, model := range []string{"org/other", "org/another"} {
		source := bytes.ReplaceAll([]byte(legacyPresenceBase), []byte("org/qwen"), []byte(model))
		if err := os.WriteFile(options.ConfigPath, source, 0600); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"preview", "apply"} {
			_, err := ImportLegacyEngineState(options, action)
			requireLegacySite(t, err, "destination.conflict")
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("refusal wrote destination")
			}
		}
	}
}

func TestImportLegacyEveryMappedFieldAbsenceProperty(t *testing.T) {
	fixture, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(fixture, &raw); err != nil {
		t.Fatal(err)
	}
	for name, value := range raw["profiles"].(map[string]any) {
		profile := value.(map[string]any)
		for _, path := range legacyPresenceFixturePaths(profile, "") {
			t.Run(name+"/"+path, func(t *testing.T) {
				options := legacyImportFixture(t)
				initial, err := toml.Marshal(map[string]any{"profiles": map[string]any{name: profile}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(options.ConfigPath, initial, 0600); err != nil {
					t.Fatal(err)
				}
				legacyApply(t, options)
				before := legacyTree(t, options.HomeDir)
				var reduced map[string]any
				if err := toml.Unmarshal(initial, &reduced); err != nil {
					t.Fatal(err)
				}
				node := reduced["profiles"].(map[string]any)[name].(map[string]any)
				parts := strings.Split(path, ".")
				for _, part := range parts[:len(parts)-1] {
					node = node[part].(map[string]any)
				}
				key := parts[len(parts)-1]
				delete(node, key)
				source, err := toml.Marshal(reduced)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(options.ConfigPath, source, 0600); err != nil {
					t.Fatal(err)
				}
				for _, action := range []string{"preview", "apply", "apply"} {
					report, err := ImportLegacyEngineState(options, action)
					if err != nil || len(report.Targets) != 0 {
						t.Fatalf("absent %s changed plan: %+v %v", path, report, err)
					}
					if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
						t.Fatalf("absent %s changed destination", path)
					}
				}
			})
		}
	}
}

func TestImportLegacyPerFieldInvalidPresentValueRefuses(t *testing.T) {
	options := legacyImportFixture(t)
	if err := os.WriteFile(options.ConfigPath, []byte(legacyPresenceBase+legacyPresenceSupervision), 0600); err != nil {
		t.Fatal(err)
	}
	legacyApply(t, options)
	before := legacyTree(t, options.HomeDir)
	if err := os.WriteFile(options.ConfigPath, []byte("[profiles.qwen.supervision]\nmax_restarts=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"preview", "apply"} {
		_, err := ImportLegacyEngineState(options, action)
		if err == nil || !strings.Contains(err.Error(), "entry.supervision.max_restarts") {
			t.Fatalf("invalid present zero admitted: %v", err)
		}
		if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
			t.Fatal("refusal wrote destination")
		}
	}
}

// Enumerate the normative fixture keys independently of production presence
// collection, including table disappearance and each individual source member.
func legacyPresenceFixturePaths(table map[string]any, prefix string) []string {
	var paths []string
	for key, value := range table {
		path := prefix + key
		paths = append(paths, path)
		if nested, ok := value.(map[string]any); ok {
			paths = append(paths, legacyPresenceFixturePaths(nested, path+".")...)
		}
	}
	return paths
}

func TestImportLegacyEveryMappedPresentValueProperty(t *testing.T) {
	fixture, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(fixture, &raw); err != nil {
		t.Fatal(err)
	}
	for name, value := range raw["profiles"].(map[string]any) {
		profile := value.(map[string]any)
		for _, path := range legacyPresenceFixturePaths(profile, "") {
			t.Run(name+"/"+path, func(t *testing.T) {
				initial, err := toml.Marshal(map[string]any{"profiles": map[string]any{name: profile}})
				if err != nil {
					t.Fatal(err)
				}
				var changed map[string]any
				if err := toml.Unmarshal(initial, &changed); err != nil {
					t.Fatal(err)
				}
				node := changed["profiles"].(map[string]any)[name].(map[string]any)
				parts := strings.Split(path, ".")
				for _, part := range parts[:len(parts)-1] {
					node = node[part].(map[string]any)
				}
				key := parts[len(parts)-1]
				switch value := node[key].(type) {
				case map[string]any:
					return // Member changes exercise the table.
				case int64:
					node[key] = value + 1
				case bool:
					node[key] = !value
				case []any:
					values := append([]any(nil), value...)
					values[0] = values[0].(string) + "-changed"
					node[key] = values
				case string:
					switch key {
					case "mode", "remote_host":
						return // Closed single-value/locality grammar, covered by validator negatives.
					case "engine":
						if value == "llama-cpp" {
							return
						} // Switching away would invalidate its retained speculative knob.
						if value == "mlx-lm" {
							node[key] = "mlx-swift"
						} else {
							node[key] = "mlx-lm"
						}
					case "kv_context_tokens", "prefill_chunk_tokens":
						if value == "unbounded" {
							node[key] = "32768"
						} else {
							size, err := strconv.Atoi(value)
							if err != nil {
								t.Fatal(err)
							}
							node[key] = strconv.Itoa(size * 2)
						}
					case "reasoning_effort":
						if value == "low" {
							node[key] = "xhigh"
						} else {
							node[key] = "low"
						}
					case "speculative_decoding":
						node[key] = "ngram"
					case "commit":
						node[key] = strings.Repeat("f", 40)
					default:
						node[key] = value + "-changed"
					}
				default:
					t.Fatalf("fixture type needs coverage: %s %T", path, value)
				}
				source, err := toml.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				want, _, err := ReadLegacyEngineProfiles(source)
				if err != nil {
					t.Fatalf("invalid change for %s: %v", path, err)
				}
				options := legacyImportFixture(t)
				if err := os.WriteFile(options.ConfigPath, initial, 0600); err != nil {
					t.Fatal(err)
				}
				legacyApply(t, options)
				if err := os.WriteFile(options.ConfigPath, source, 0600); err != nil {
					t.Fatal(err)
				}
				before := legacyTree(t, options.HomeDir)
				preview, err := ImportLegacyEngineState(options, "preview")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
					t.Fatal("preview wrote destination")
				}
				applied, err := ImportLegacyEngineState(options, "apply")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(preview.FieldPlan, applied.FieldPlan) {
					t.Fatal("field plans differ")
				}
				got, _, err := ReadEngineCatalogEntry(filepath.Join(options.HomeDir, ".curator", "engines.toml"), name)
				if err != nil || !legacySameEntry(got, want.Engines[name]) {
					t.Fatalf("present %s failed precedence: %+v %v", path, got, err)
				}
			})
		}
	}
}
