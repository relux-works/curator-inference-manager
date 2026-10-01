//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func TestCuratorEnginesImportLegacyF1AbsentKnobsCompiledCLI(t *testing.T) {
	if err := os.MkdirAll("/tmp/.temp", 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp/.temp", "f1-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	exe := filepath.Join(root, "curator-engines")
	build := exec.Command("go", "build", "-o", exe, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	home, cache, project := filepath.Join(root, "home"), filepath.Join(root, "cache"), filepath.Join(root, "project")
	for _, dir := range []string{home, cache, project} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "legacy.toml")
	base := "[profiles.qwen]\nmode='local'\nengine='mlx-swift'\nmodel='org/qwen'\nexecutable='/usr/bin/false'\nargv=['serve','--host','{host}','--port','{port}']\n"
	knobs := "[profiles.qwen.knobs]\nkv_context_tokens='32768'\nprefill_chunk_tokens='2048'\nreasoning_effort='medium'\n"
	invoke := func(action string) infra.LegacyImportReport {
		t.Helper()
		command := exec.Command(exe, "import-legacy", action, "--home", home, "--cache-root", cache, "--config", config)
		command.Dir = project
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", action, err, output)
		}
		var report infra.LegacyImportReport
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatal(err, string(output))
		}
		return report
	}
	if err := os.WriteFile(config, []byte(base+knobs), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("apply")
	catalog := filepath.Join(home, ".curator", "engines.toml")
	before, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"preview", "apply", "apply"} {
		report := invoke(action)
		if len(report.Targets) != 0 {
			t.Fatalf("absent knobs planned destructive update: %+v", report)
		}
		after, err := os.ReadFile(catalog)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("F1 removed imported fields: %v\n%s", err, after)
		}
		entry, _, err := infra.ReadEngineCatalogEntry(catalog, "qwen")
		if err != nil || entry.Knobs == nil || entry.Knobs.KVContextTokens != "32768" || entry.Knobs.PrefillChunkTokens != "2048" || entry.Knobs.ReasoningEffort != "medium" {
			t.Fatalf("F1: %+v %v", entry.Knobs, err)
		}
	}
}
