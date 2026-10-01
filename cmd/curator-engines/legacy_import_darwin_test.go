//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func TestCuratorEnginesImportLegacyProductionCLIAndAliasesWithoutInfra(t *testing.T) {
	if err := os.MkdirAll("/tmp/.temp", 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp/.temp", "cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	cache := filepath.Join(root, "cache")
	project := filepath.Join(root, "project")
	for _, path := range []string{home, cache, project} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(project)
	t.Setenv("HOME", home)
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatal("agents-infra unexpectedly exists")
	}
	config := filepath.Join(root, "legacy.toml")
	script := filepath.Join(root, "console-script")
	marker := filepath.Join(root, "launched")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := `[profiles.qwen]
mode="local"
engine="mlx-swift"
model="org/qwen"
executable="` + script + `"
argv=["serve","--host","{host}","--port","{port}"]
[profiles.qwen.stress]
prompt_tokens=1024
max_output_tokens=1
startup_timeout_seconds=2
request_timeout_seconds=2
sample_interval_milliseconds=100
`
	if err := os.WriteFile(config, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	for _, action := range []string{"preview", "apply", "apply"} {
		stdout.Reset()
		stderr.Reset()
		code := run([]string{"import-legacy", action, "--home", home, "--config", config, "--cache-root", cache}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s exit=%d %s", action, code, stderr.String())
		}
		var result infra.LegacyImportReport
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.NotRunnable["qwen"] != "script_executable_unsupported" {
			t.Fatalf("report=%+v", result)
		}
	}
	for _, alias := range []string{"qwen-infra", "model-harness"} {
		for _, command := range []string{"print-config", "render", "doctor", "diagnostics", "run", "stress"} {
			stdout.Reset()
			stderr.Reset()
			args := []string{"compat", alias, command, "--engine", "qwen", "--home", home, "--cache-root", cache, "--config", config}
			code := run(args, &stdout, &stderr)
			if command == "run" || command == "stress" {
				if code == 0 || !strings.Contains(stderr.String(), "script_executable_unsupported") {
					t.Fatalf("%v did not reach manager lease refusal: %d %s", args, code, stderr.String())
				}
			} else if code != 0 {
				t.Fatalf("%v exit=%d %s", args, code, stderr.String())
			}
			if command == "doctor" || command == "diagnostics" {
				if !strings.Contains(stdout.String(), `"engine_availability":"unknown"`) {
					t.Fatalf("diagnostic guessed passing: %s", stdout.String())
				}
			}
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("preview or nonrunnable command started script")
	}
	for _, args := range [][]string{{"compat", "qwen-infra", "render"}, {"compat", "deprecated-engine", "render", "--engine", "qwen"}, {"compat", "qwen-infra", "render", "--engine", "absent"}, {"import-legacy", "unexpected", "--home", home}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("accepted %v", args)
		}
		if len(args) > 1 && args[1] == "deprecated-engine" && !strings.Contains(stderr.String(), "migrate to curator-engines") {
			t.Fatal("deprecated alias omitted migration guide", stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"import-legacy", "rollback", "--home", home, "--config", config, "--cache-root", cache}, &stdout, &stderr); code != 0 {
		t.Fatalf("rollback %d %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".curator", "engines.toml")); !os.IsNotExist(err) {
		t.Fatal("rollback left created catalog")
	}
}
func TestCuratorEnginesCompatibilityShellAdaptersUseManagerEntrypoint(t *testing.T) {
	// Exercise the shipped shell shims as processes. The manager CLI is real;
	// there is no executable with an agents-infra name in the scratch PATH.
	root, err := os.MkdirTemp("/tmp/.temp", "shim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	executable := filepath.Join(root, "curator-engines")
	command := exec.Command("go", "build", "-o", executable, ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build %v %s", err, output)
	}
	home := filepath.Join(root, "home")
	_ = os.Mkdir(home, 0o700)
	_ = os.Mkdir(filepath.Join(home, ".curator"), 0o700)
	document := infra.EngineCatalogDocument{SchemaVersion: 1, Engines: map[string]infra.EngineCatalogEntry{"explicit": {Mode: "local", Engine: "mlx-lm", Model: "org/model", ServedModelID: "org/model", Executable: "/usr/bin/false", Argv: []string{"{host}", "{port}"}}}}
	data, err := infra.MarshalEngineCatalog(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".curator", "engines.toml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"qwen-infra", "model-harness"} {
		adapter := filepath.Join("../../scripts", alias)
		command = exec.Command(adapter, "render", "--engine", "explicit", "--home", home)
		command.Env = append(os.Environ(), "PATH="+root+":/usr/bin:/bin", "HOME="+home)
		output, err = command.CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte(`"engine":"explicit"`)) {
			t.Fatalf("%s %v %s", alias, err, output)
		}
	}
}

// Import preserves legacy argv and reports the landed catalog check verbatim;
// it cannot silently turn missing K0 settings into an admitted profile.
func TestCuratorEnginesImportLegacyPreviewReportsCatalogCheckWithoutRewriting(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(strconv.FormatBool(admitted), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp/.temp", "ic-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			t.Chdir(root)
			home, cache := filepath.Join(root, "home"), filepath.Join(root, "cache")
			for _, dir := range []string{home, cache, filepath.Join(home, ".curator")} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			catalogDir := filepath.Join(home, ".curator")
			writeCheckGGUF(t, catalogDir, "qwen3", false)
			argv := []string{"--host", "{host}", "--port", "{port}"}
			if admitted {
				argv = append(changeCheckArg(checkFixtureArgv(), "-m"), argv...)
			}
			encodedArgv, err := json.Marshal(argv)
			if err != nil {
				t.Fatal(err)
			}
			source := []byte("[profiles.fixture]\nmode='local'\nengine='llama-cpp'\nmodel='model.gguf'\nexecutable='/usr/bin/false'\nargv=" + string(encodedArgv) + "\n")
			config := filepath.Join(root, "legacy.toml")
			if err := os.WriteFile(config, source, 0o600); err != nil {
				t.Fatal(err)
			}
			catalog := filepath.Join(catalogDir, "engines.toml")
			var preview infra.EngineCheckReport
			for _, action := range []string{"preview", "apply", "apply"} {
				var stdout, stderr bytes.Buffer
				if code := run([]string{"import-legacy", action, "--home", home, "--config", config, "--cache-root", cache}, &stdout, &stderr); code != 0 {
					status, statusErr := infra.SharedRuntimeStatusReport(infra.SharedRuntimeOperatorOptions{HomeDir: home, CacheRoot: cache, ProjectDir: root, Profile: "fixture"})
					t.Fatalf("%s exit=%d: %s status=%+v err=%v", action, code, &stderr, status, statusErr)
				}
				var report infra.LegacyImportReport
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				got, present := report.Checks["fixture"]
				if !present || (len(got.Failures) == 0) != admitted {
					t.Fatalf("missing or altered K0 report: %+v", report)
				}
				if action == "preview" {
					preview = got
					if _, err := os.Stat(catalog); !os.IsNotExist(err) {
						t.Fatal("preview wrote catalog", err)
					}
					if _, err := os.Stat(filepath.Join(catalogDir, ".legacy-import")); !os.IsNotExist(err) {
						t.Fatal("preview staged a transaction", err)
					}
				} else {
					entry, state, err := infra.ReadEngineCatalogEntry(catalog, "fixture")
					if err != nil || state != infra.EngineCatalogPresent || !reflect.DeepEqual(entry.Argv, argv) {
						t.Fatalf("import rewrote argv: %+v %s %v", entry, state, err)
					}
					var checkOut, checkErr bytes.Buffer
					exit := run([]string{"check", "--config", catalog, "--engine", "fixture", "--json"}, &checkOut, &checkErr)
					var checked infra.EngineCheckReport
					if err := json.Unmarshal(checkOut.Bytes(), &checked); err != nil {
						t.Fatal(err, &checkErr)
					}
					wantExit := 1
					if admitted {
						wantExit = 0
					}
					if exit != wantExit || !reflect.DeepEqual(preview, checked) || !reflect.DeepEqual(got, checked) {
						t.Fatalf("preview/check disagreement: exit=%d preview=%+v check=%+v", exit, preview, checked)
					}
				}
			}
			if data, err := os.ReadFile(config); err != nil || !bytes.Equal(data, source) {
				t.Fatal("source changed", err)
			}
		})
	}
}
