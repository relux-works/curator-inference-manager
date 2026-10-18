package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func weightsFixture(t *testing.T) (string, infra.EngineCatalogEntry, string) {
	t.Helper()
	dir := t.TempDir()
	writeCheckGGUF(t, dir, "qwen3", false)
	model := filepath.Join(dir, "model.gguf")
	data, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	// The expected digest is independent of the streaming production helper.
	id := fmt.Sprintf("gguf-sha256:%x", sha256.Sum256(data))
	argv := changeCheckArg(checkFixtureArgv(), "-m")
	argv = changeCheckArg(argv, "--alias")
	argv = append(argv, "--host", "{host}", "--port", "{port}")
	return dir, infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "local", Model: "model.gguf", ServedModelID: "fixture-model", Executable: "/bin/echo", Argv: argv, Weights: &infra.EngineCatalogWeights{ID: id}}, id
}

func writeWeightsCatalog(t *testing.T, dir string, entry infra.EngineCatalogEntry) string {
	t.Helper()
	data, err := infra.MarshalEngineCatalog(infra.EngineCatalogDocument{SchemaVersion: 1, Engines: map[string]infra.EngineCatalogEntry{"fixture": entry}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "engines.toml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A declared id round-trips exactly; compute hashes full fixture bytes read-only,
// check verifies them, missing models stay unknown, and legacy entries stay unpinned.
func TestWeightsRoundTripComputeAndCheck(t *testing.T) {
	dir, entry, id := weightsFixture(t)
	path := writeWeightsCatalog(t, dir, entry)
	before, _ := os.ReadFile(path)
	got, state, err := infra.ReadEngineCatalogEntry(path, "fixture")
	if err != nil || state != infra.EngineCatalogPresent || got.Weights == nil || got.Weights.ID != id {
		t.Fatalf("round trip: %+v %s %v", got, state, err)
	}
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"weights", "compute", "--config", path, "--engine", "fixture"}
		if jsonOutput {
			args = append(args, "--json")
		}
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 0 {
			t.Fatalf("compute=%d %s", code, &stderr)
		}
		if jsonOutput {
			var observed infra.EngineWeightsObservation
			if err := json.Unmarshal(out.Bytes(), &observed); err != nil || observed.ID != id || observed.ObservedAt.IsZero() {
				t.Fatalf("compute: %s %v", &out, err)
			}
		} else if out.String() != id+"\n" {
			t.Fatalf("compute=%q want %q", out.String(), id)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("compute edited catalog")
	}
	report := infra.CheckEngineFile(path, "fixture")
	if len(report.Failures) != 0 || report.Weights.Verification != "verified" || report.Weights.ObservedID != id || report.Weights.ObservedAt == nil {
		t.Fatalf("check=%+v", report)
	}
	if err := os.Remove(filepath.Join(dir, "model.gguf")); err != nil {
		t.Fatal(err)
	}
	report = infra.CheckEngineFile(path, "fixture")
	if len(report.Failures) != 0 || report.Weights.Verification != "unknown" {
		t.Fatalf("absent check=%+v", report)
	}
	entry.Weights = nil
	writeWeightsCatalog(t, dir, entry)
	if got := infra.CheckEngineFile(path, "fixture"); len(got.Failures) != 0 || got.Weights.Verification != "unpinned" {
		t.Fatalf("legacy check=%+v", got)
	}
}

// A recorded/actual mismatch reports both ids and never starts the fake runtime.
func TestWeightsCheckMismatch(t *testing.T) {
	dir, entry, actual := weightsFixture(t)
	entry.Executable = buildCatalogFakeRuntime(t)
	marker := filepath.Join(dir, "runtime.pid")
	t.Setenv("CATALOG_FAKE_PID_FILE", marker)
	entry.Weights.ID = "gguf-sha256:" + strings.Repeat("a", 64)
	path := writeWeightsCatalog(t, dir, entry)
	var out, stderr bytes.Buffer
	code := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &out, &stderr)
	var report infra.EngineCheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 1 || len(report.Failures) != 1 || report.Failures[0].Code != "engine_weights_mismatch" || report.Weights.Verification != "mismatch" || !strings.Contains(report.Failures[0].Message, actual) || !strings.Contains(report.Failures[0].Message, entry.Weights.ID) {
		t.Fatalf("check=%d %+v stderr=%s", code, report, &stderr)
	}
	assertWeightsNoLaunch(t, marker)
}

// Compute refuses specific file failures and unsupported engine scope, without
// trying an executable; the successful compute control is above.
func TestWeightsComputeRefusals(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"missing", "engine_weights_file_missing"}, {"directory", "engine_weights_not_regular"}, {"unreadable", "engine_weights_read_failed"}, {"other_engine", "engine_catalog_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, entry, _ := weightsFixture(t)
			entry.Weights = nil
			model := filepath.Join(dir, "model.gguf")
			switch tc.name {
			case "missing":
				if err := os.Remove(model); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(model); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(model, 0700); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root can read mode-000 files")
				}
				if err := os.Chmod(model, 0); err != nil {
					t.Fatal(err)
				}
			case "other_engine":
				entry.Engine = "mlx-lm"
			}
			path := writeWeightsCatalog(t, dir, entry)
			var out, stderr bytes.Buffer
			code := run([]string{"weights", "compute", "--config", path, "--engine", "fixture", "--json"}, &out, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), `"code":"`+tc.code+`"`) || out.Len() != 0 {
				t.Fatalf("compute=%d stdout=%s stderr=%s", code, &out, &stderr)
			}
		})
	}
}

// Compute does not fall back when config selection is absent, malformed or
// names no entry; each refusal retains its catalog type and emits no id.
func TestWeightsComputeSelectionRefusals(t *testing.T) {
	for _, name := range []string{"absent_config", "malformed_config", "missing_entry"} {
		t.Run(name, func(t *testing.T) {
			dir, entry, _ := weightsFixture(t)
			path := writeWeightsCatalog(t, dir, entry)
			engine := "fixture"
			site := "weights.entry"
			switch name {
			case "absent_config":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "malformed_config":
				if err := os.WriteFile(path, []byte("["), 0600); err != nil {
					t.Fatal(err)
				}
				site = "catalog.decode"
			case "missing_entry":
				engine = "absent"
			}
			var out, stderr bytes.Buffer
			code := run([]string{"weights", "compute", "--config", path, "--engine", engine}, &out, &stderr)
			if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), `"code":"engine_catalog_malformed"`) || !strings.Contains(stderr.String(), `"site":"`+site+`"`) {
				t.Fatalf("compute=%d out=%s err=%s", code, &out, &stderr)
			}
		})
	}
}

func buildCatalogFakeRuntime(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "catalog-fake-runtime")
	command := exec.Command("go", "build", "-o", executable, "./testdata/fake-runtime")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build catalog fake runtime: %v\n%s", err, output)
	}
	return executable
}

func assertWeightsNoLaunch(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("refused request started runtime: %v", err)
	}
}

type weightsGGUFRefusal struct {
	name, model, code string
	data              []byte
}

func weightsGGUFRefusals(t *testing.T, valid []byte) []weightsGGUFRefusal {
	t.Helper()
	withSplit := func(key string, value uint16) []byte {
		data := bytes.NewBuffer(bytes.Clone(valid))
		binary.LittleEndian.PutUint64(data.Bytes()[16:24], binary.LittleEndian.Uint64(valid[16:24])+1)
		for _, field := range []any{uint64(len(key)), []byte(key), uint32(2), value} {
			if err := binary.Write(data, binary.LittleEndian, field); err != nil {
				t.Fatal(err)
			}
		}
		return data.Bytes()
	}
	unsupported := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(unsupported[4:8], 4)
	return []weightsGGUFRefusal{
		{"text", "model.gguf", "engine_weights_not_gguf", []byte("not a GGUF model")},
		{"truncated", "model.gguf", "engine_weights_not_gguf", []byte("GGUF\x03\x00")},
		{"unsupported_version", "model.gguf", "engine_weights_not_gguf", unsupported},
		{"truncated_metadata", "model.gguf", "engine_weights_not_gguf", valid[:len(valid)-1]},
		{"split_count", "renamed.gguf", "engine_weights_split_unsupported", withSplit("split.count", 2)},
		{"split_no_zero", "renamed.gguf", "engine_weights_split_unsupported", withSplit("split.no", 0)},
	}
}

// Compute and check refuse invalid headers and metadata-declared splits even
// when the recorded digest matches; neither command may start the fake runtime.
func TestWeightsGGUFComputeAndCheckRefusals(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	dir, _, _ := weightsFixture(t)
	valid, err := os.ReadFile(filepath.Join(dir, "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range weightsGGUFRefusals(t, valid) {
		t.Run(tc.name, func(t *testing.T) {
			dir, entry, _ := weightsFixture(t)
			entry.Executable, entry.Model = fake, tc.model
			entry.Weights.ID = fmt.Sprintf("gguf-sha256:%x", sha256.Sum256(tc.data))
			if err := os.WriteFile(filepath.Join(dir, tc.model), tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			path := writeWeightsCatalog(t, dir, entry)
			marker := filepath.Join(dir, "runtime.pid")
			t.Setenv("CATALOG_FAKE_PID_FILE", marker)
			var out, stderr bytes.Buffer
			exit := run([]string{"weights", "compute", "--config", path, "--engine", "fixture", "--json"}, &out, &stderr)
			var refusal struct{ Code string }
			if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || exit != 1 || out.Len() != 0 || refusal.Code != tc.code {
				t.Fatalf("compute=%d stdout=%s stderr=%s decode=%v", exit, &out, &stderr, err)
			}
			assertWeightsNoLaunch(t, marker)
			out.Reset()
			stderr.Reset()
			exit = run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &out, &stderr)
			var report infra.EngineCheckReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || exit != 1 || report.Weights.Verification != "unknown" || report.Weights.ObservedID != "" || report.Weights.ObservedAt != nil {
				t.Fatalf("check=%d stdout=%s stderr=%s decode=%v", exit, &out, &stderr, err)
			}
			found := false
			for _, failure := range report.Failures {
				if failure.Code == tc.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("check lost %s: %+v", tc.code, report)
			}
			assertWeightsNoLaunch(t, marker)
		})
	}
}

// Check preserves weights catalog codes/sites while unrelated catalog failures
// retain engine_config_malformed. Valid catalog/check controls are above.
func TestWeightsCheckCatalogRefusals(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	for _, tc := range []struct{ name, table, code, site string }{
		{"malformed", `id = "gguf-sha256:bad"`, "engine_catalog_malformed", "entry.weights.id"},
		{"unsupported_id", `id = "sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.id.unsupported"},
		{"unknown_member", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"` + "\nversion = 1", "engine_catalog_malformed", "catalog.weights.keys"},
		{"not_table", `"invalid"`, "engine_catalog_malformed", "catalog.weights"},
		{"mlx", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.scope"},
		{"ssh", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.scope"},
		{"other_catalog_error", "", "engine_config_malformed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, entry, _ := weightsFixture(t)
			entry.Weights, entry.Executable = nil, fake
			if tc.name == "mlx" {
				entry.Engine = "mlx-lm"
			}
			if tc.name == "ssh" {
				entry = infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "ssh", SSHExecutable: fake, SSHTarget: "fixture", RemoteExecutable: "/fixture/runtime", RemoteProfile: "fixture", RemoteHost: "127.0.0.1", RemotePort: 18012, ServedModelID: "fixture-model"}
			}
			path := writeWeightsCatalog(t, dir, entry)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "other_catalog_error":
				data = append(data, []byte("\nunknown = true\n")...)
			case "not_table":
				data = append(data, []byte("\nweights = "+tc.table+"\n")...)
			default:
				data = append(data, []byte("\n[engines.fixture.weights]\n"+tc.table+"\n")...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "runtime.pid")
			t.Setenv("CATALOG_FAKE_PID_FILE", marker)
			var out, stderr bytes.Buffer
			exit := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &out, &stderr)
			var report struct {
				Failures []struct{ Code, Site string }
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || exit != 1 || len(report.Failures) != 1 {
				t.Fatalf("check=%d stdout=%s stderr=%s decode=%v", exit, &out, &stderr, err)
			}
			if failure := report.Failures[0]; failure.Code != tc.code || failure.Site != tc.site {
				t.Fatalf("failure=%+v want code=%s site=%s", failure, tc.code, tc.site)
			}
			assertWeightsNoLaunch(t, marker)
		})
	}
}

// Single-file scope follows metadata: a conventional shard-looking filename
// with valid single-file contents still computes and verifies the full digest.
func TestWeightsComputeSingleFileRegardlessOfName(t *testing.T) {
	dir, entry, id := weightsFixture(t)
	model := "model-00001-of-00002.gguf"
	if err := os.Rename(filepath.Join(dir, entry.Model), filepath.Join(dir, model)); err != nil {
		t.Fatal(err)
	}
	entry.Model = model
	path := writeWeightsCatalog(t, dir, entry)
	var out, stderr bytes.Buffer
	if exit := run([]string{"weights", "compute", "--config", path, "--engine", "fixture"}, &out, &stderr); exit != 0 || out.String() != id+"\n" {
		t.Fatalf("compute=%d stdout=%s stderr=%s", exit, &out, &stderr)
	}
	report := infra.CheckEngineFile(path, "fixture")
	if len(report.Failures) != 0 || report.Weights.Verification != "verified" || report.Weights.ObservedID != id {
		t.Fatalf("check=%+v", report)
	}
}
