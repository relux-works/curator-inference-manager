package infra

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Each supported value emits one flag; absence emits none, and malformed or
// conflicting declarations retain a typed refusal instead of launching.
func TestEngineReasoningKnob(t *testing.T) {
	for _, value := range []string{"on", "off", "auto", ""} {
		knobs := &EngineCatalogKnobs{Reasoning: value}
		if err := validateEngineCatalogKnobs("llama-cpp", knobs); err != nil {
			t.Fatal(err)
		}
		got := engineCatalogKnobArgv("llama-cpp", knobs)
		var want []string
		if value != "" {
			want = []string{"--reasoning", value}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v want %v", value, got, want)
		}
	}
	for _, value := range []string{"yes", "OFF", "high"} {
		err := validateEngineCatalogKnobs("llama-cpp", &EngineCatalogKnobs{Reasoning: value})
		var typed *EngineCatalogError
		if !errors.As(err, &typed) || typed.State != EngineCatalogMalformed || typed.Site != "entry.knobs.reasoning" {
			t.Fatalf("invalid %q: %v", value, err)
		}
	}
	if err := validateEngineCatalogKnobs("mlx-lm", &EngineCatalogKnobs{Reasoning: "off"}); err == nil {
		t.Fatal("accepted non-llama reasoning")
	}
	for _, flag := range []string{"--reasoning", "-rea", "--reasoning=on", "-rea=on"} {
		entry := EngineCatalogEntry{Engine: "llama-cpp", Mode: "local", Model: "model.gguf", Executable: "/bin/echo", Argv: []string{"--host", "{host}", "--port", "{port}", flag, "on"}, Knobs: &EngineCatalogKnobs{Reasoning: "off"}}
		err := validateEngineCatalogEntry("model", entry)
		var typed *EngineCatalogError
		if !errors.As(err, &typed) || typed.Site != "entry.knobs.reasoning.argv" {
			t.Fatalf("%s: %v", flag, err)
		}
	}
}

// Checked catalogs preserve the reasoning refusal code/site, accept all three
// values in the argv table, and continue rejecting chat-template kwargs.
func TestEngineReasoningCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engines.toml")
	base := "schema_version=1\n[engines.model]\nengine='llama-cpp'\nmode='local'\nmodel='model.gguf'\nexecutable='/bin/echo'\nargv=['--host','{host}','--port','{port}']\n[engines.model.knobs]\nreasoning="
	for _, value := range []string{"'on'", "'off'", "'auto'", "'bad'", "''", "42"} {
		if err := os.WriteFile(path, []byte(base+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		report := CheckEngineFile(path, "model")
		refused := false
		for _, failure := range report.Failures {
			if failure.Code == "engine_catalog_malformed" && failure.Site == "entry.knobs.reasoning" {
				refused = true
			}
			if failure.Code == "argv_unsupported" {
				t.Fatalf("valid knob produced unsupported argv: %+v", report)
			}
		}
		want := value == "'bad'" || value == "''" || value == "42"
		if refused != want {
			t.Fatalf("%s: %+v", value, report)
		}
	}
	for _, flag := range []string{"--reasoning", "-rea", "--reasoning-budget", "--chat-template-kwargs"} {
		_, failures := parseEngineCheckArgs([]string{flag, "off"})
		if (len(failures) != 0) != strings.Contains(flag, "kwargs") {
			t.Fatalf("%s: %v", flag, failures)
		}
	}
}

// Semantic reasoning refusals belong to the selected entry; malformed values in
// a sibling do not block a valid selection. Selecting that sibling still refuses.
func TestEngineReasoningSelectedEntryIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engines.toml")
	config := `schema_version=1
[engines.good]
engine='llama-cpp'
mode='local'
model='model.gguf'
executable='/bin/echo'
argv=['--host','{host}','--port','{port}']
[engines.good.knobs]
reasoning='off'
[engines.bad]
engine='llama-cpp'
mode='local'
model='model.gguf'
executable='/bin/echo'
argv=['--host','{host}','--port','{port}']
[engines.bad.knobs]
reasoning='invalid'
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	entry, state, err := ReadEngineCatalogEntry(path, "good")
	if err != nil || state != EngineCatalogPresent || entry.Knobs.Reasoning != "off" {
		t.Fatalf("valid selection: %+v %s %v", entry, state, err)
	}
	_, state, err = ReadEngineCatalogEntry(path, "bad")
	var typed *EngineCatalogError
	if !errors.As(err, &typed) || state != EngineCatalogMalformed || typed.State != EngineCatalogMalformed || typed.Site != "entry.knobs.reasoning" {
		t.Fatalf("invalid selection lost typed refusal: %s %v", state, err)
	}
}

// The same token arity as check protects option-looking values from being
// mistaken for flags; actual reasoning options still conflict with the knob.
func TestEngineReasoningArgvConsumption(t *testing.T) {
	for _, flag := range []string{"--reasoning", "-rea"} {
		for _, valuePosition := range []bool{true, false} {
			entry := EngineCatalogEntry{Engine: "llama-cpp", Mode: "local", Model: "model.gguf", Executable: "/bin/echo", Argv: []string{"--host", "{host}", "--port", "{port}"}, Knobs: &EngineCatalogKnobs{Reasoning: "off"}}
			if valuePosition {
				entry.Argv = append(entry.Argv, "--chat-template", flag)
			} else {
				entry.Argv = append(entry.Argv, "--chat-template", "template", flag, "on")
			}
			err := validateEngineCatalogEntry("model", entry)
			if valuePosition {
				if err != nil {
					t.Fatalf("value %q refused: %v", flag, err)
				}
				values, failures := parseEngineCheckArgs(engineCatalogRuntimeArgv(entry, 18012, "model"))
				if len(failures) != 0 || singleEngineArg(values, "reasoning") != "off" || singleEngineArg(values, "template") != flag {
					t.Fatalf("rendered argv consumption: %v %v", values, failures)
				}
			} else {
				var typed *EngineCatalogError
				if !errors.As(err, &typed) || typed.State != EngineCatalogMalformed || typed.Site != "entry.knobs.reasoning.argv" {
					t.Fatalf("real flag %q: %v", flag, err)
				}
			}
		}
	}
}
