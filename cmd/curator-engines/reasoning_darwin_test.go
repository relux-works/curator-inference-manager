package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
)

// Ensure rejects invalid/conflicting thinking controls before starting a process;
// the valid control resolves to the same single rendered reasoning option.
func TestReasoningEnsureRefusals(t *testing.T) {
	for _, value := range []string{"on", "off", "auto", "invalid", "conflict", "short-conflict"} {
		t.Run(value, func(t *testing.T) {
			entry := infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "local", Model: "model.gguf", Executable: "/bin/echo", Argv: []string{"--host", "{host}", "--port", "{port}"}, Knobs: &infra.EngineCatalogKnobs{Reasoning: "off"}}
			prepareCatalogCLIEntry(t, "fixture", entry)
			if value == "on" || value == "auto" || value == "invalid" {
				entry.Knobs.Reasoning = value
			}
			if value == "conflict" {
				entry.Argv = append(entry.Argv, "--reasoning", "on")
			}
			if value == "short-conflict" {
				entry.Argv = append(entry.Argv, "-rea", "on")
			}
			argv, err := json.Marshal(entry.Argv)
			if err != nil {
				t.Fatal(err)
			}
			config := fmt.Sprintf("schema_version=1\n[engines.fixture]\nengine='llama-cpp'\nmode='local'\nmodel='fixture-model'\nserved_model_id='fixture-model'\nexecutable='/bin/echo'\nargv=%s\n[engines.fixture.knobs]\nreasoning=%q\n", argv, entry.Knobs.Reasoning)
			if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if value == "on" || value == "off" || value == "auto" {
				// Status uses the same production resolver as ensure, including argv
				// rendering; transport/process startup is not part of this control.
				status, err := infra.SharedRuntimeStatusReport(infra.SharedRuntimeOperatorOptions{ProjectDir: currentProjectDir(), Profile: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				var rendered []string
				for _, row := range status.Readings.Facts {
					if row.Fact == inferenceengine.FactLocalArgv {
						if err := json.Unmarshal([]byte(row.Value), &rendered); err != nil {
							t.Fatal(err)
						}
					}
				}
				count := 0
				for i, token := range rendered {
					if token == "--reasoning" || token == "-rea" {
						count++
						if token != "--reasoning" || i+1 >= len(rendered) || rendered[i+1] != value {
							t.Fatalf("wrong reasoning pair in %v", rendered)
						}
					}
				}
				if count != 1 {
					t.Fatalf("reasoning pairs=%d in rendered argv %v", count, rendered)
				}
				return
			}
			site := "entry.knobs.reasoning"
			if value != "invalid" {
				site += ".argv"
			}
			assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--json"}, "engine_catalog_malformed", site, "")
		})
	}
}
