package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func checkFixtureArgv() []string {
	return []string{"-m", "model.gguf", "--alias", "qwen-local-engine", "--ctx-size", "16384", "--parallel", "1", "--no-kv-unified", "--cache-ram", "0", "--no-cache-idle-slots", "--jinja", "--chat-template-file", "qwen38-chat-template.jinja", "--cache-type-k", "f16", "--cache-type-v", "f16"}
}

func TestEngineCheckCatalogV1(t *testing.T) {
	for _, name := range []string{"golden", "missing_idle", "swallowed_idle", "missing_alias", "mlx", "ssh", "schema_invalid", "entry_absent", "unknown_key", "permissions"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeCheckGGUF(t, dir, "qwen3", false)
			argv := changeCheckArg(checkFixtureArgv(), "-m")
			argv = changeCheckArg(argv, "--ctx-size")
			argv = append(argv, "--host", "{host}", "--port", "{port}")
			entry := infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "local", Executable: "/fixture/llama-server", Model: "model.gguf", ServedModelID: "fixture-model", Argv: argv, Knobs: &infra.EngineCatalogKnobs{KVContextTokens: "16384"}}
			wantExit, wantCode := 0, ""
			switch name {
			case "missing_idle", "swallowed_idle":
				entry.Argv = changeCheckArg(argv, "--no-cache-idle-slots")
				if name == "swallowed_idle" {
					entry.Argv = changeCheckArg(entry.Argv, "--alias", "--alias", "--no-cache-idle-slots")
				}
				wantExit, wantCode = 1, "k0_idle_slots"
			case "missing_alias":
				entry.Model, entry.Knobs = "", nil
				entry.Argv = append(checkFixtureArgv(), "--host", "{host}", "--port", "{port}", "--alias")
				wantExit, wantCode = 1, "argv_missing_value"
			case "mlx":
				entry.Engine, entry.Knobs = "mlx-lm", nil
				entry.Argv = []string{"--host", "{host}", "--port", "{port}"}
			case "ssh":
				entry = infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "ssh", ServedModelID: "remote-model", SSHTarget: "fixture-host", RemoteExecutable: "/fixture/model-harness", RemoteProfile: "fixture", RemoteHost: "127.0.0.1", RemotePort: 18012}
			case "schema_invalid", "unknown_key", "permissions":
				wantExit, wantCode = 1, "engine_config_malformed"
			case "entry_absent":
				wantExit, wantCode = 1, "engine_entry_absent"
			}
			document := infra.EngineCatalogDocument{SchemaVersion: 1, Engines: map[string]infra.EngineCatalogEntry{"fixture": entry}}
			if name == "entry_absent" {
				document.Engines = map[string]infra.EngineCatalogEntry{"other": entry}
			}
			data, err := infra.MarshalEngineCatalog(document)
			if err != nil {
				t.Fatal(err)
			}
			if name == "schema_invalid" {
				data = bytes.Replace(data, []byte("schema_version = 1"), []byte("schema_version = 2"), 1)
			}
			if name == "unknown_key" {
				data = append(data, []byte("\nunknown_key = true\n")...)
			}
			if name == "schema_invalid" {
				// A legacy decoy cannot rescue a refused or absent catalog entry.
				legacy, err := os.ReadFile(writeCheckConfig(t, dir, checkFixtureArgv()))
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, legacy...)
				wantCode = "engine_config_malformed" // closed v1 schema rejects mixed documents
			}
			path := filepath.Join(dir, "engines.toml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "permissions" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			exit := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &stdout, &stderr)
			var report infra.EngineCheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("invalid report: %v; stderr=%s", err, &stderr)
			}
			if exit != wantExit || (wantCode != "" && (len(report.Failures) != 1 || report.Failures[0].Code != wantCode)) {
				t.Fatalf("exit=%d want=%d code=%q report=%+v", exit, wantExit, wantCode, report)
			}
			if name == "golden" && (report.KVRAM.Bytes == nil || *report.KVRAM.Bytes != 2147483648) {
				t.Fatalf("catalog model/knob transcription lost: %+v", report)
			}
			if (name == "mlx" || name == "ssh") && (len(report.Failures) != 0 || report.KVRAM.ReasonCode != "engine_kind_unsupported") {
				t.Fatalf("non-local-llama catalog entry subjected to K0: %+v", report)
			}
		})
	}
}

func TestEngineCheckOptionValueIsNotIdlePolicy(t *testing.T) {
	argv := changeCheckArg(checkFixtureArgv(), "--no-cache-idle-slots")
	argv = changeCheckArg(argv, "--alias")
	argv = append(argv, "--alias", "--no-cache-idle-slots")
	report, exit := runEngineCheck(t, t.TempDir(), argv)
	if exit != 1 || len(report.Failures) != 1 || report.Failures[0].Code != "k0_idle_slots" {
		t.Fatalf("option value counted as idle policy: exit=%d report=%+v", exit, report)
	}
	// An option-looking alias remains valid data when the real knob is present.
	argv = append(argv, "--no-cache-idle-slots")
	if report, exit = runEngineCheck(t, t.TempDir(), argv); exit != 0 {
		t.Fatalf("option-looking alias data refused: exit=%d report=%+v", exit, report)
	}
}

func TestEngineCheckGGUFMalformedHeader(t *testing.T) {
	var badMagic, hugeCount bytes.Buffer
	badMagic.WriteString("FAIL")
	hugeCount.WriteString("GGUF")
	for _, fixture := range []struct {
		buffer *bytes.Buffer
		count  uint64
	}{
		{&badMagic, 0}, {&hugeCount, ^uint64(0)},
	} {
		for _, value := range []any{uint32(3), uint64(0), fixture.count} {
			if err := binary.Write(fixture.buffer, binary.LittleEndian, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"truncated", []byte("GGUF")}, {"corrupt_magic", badMagic.Bytes()}, {"huge_count", hugeCount.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "model.gguf"), tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			report, exit := runEngineCheck(t, dir, checkFixtureArgv())
			if exit != 1 || len(report.Failures) != 1 || report.Failures[0].Code != "gguf_invalid" {
				t.Fatalf("unsafe header admitted: exit=%d report=%+v", exit, report)
			}
		})
	}
}

func TestEngineCheckNonLlamaKinds(t *testing.T) {
	for _, kind := range []string{"mlx", "ssh"} {
		t.Run(kind, func(t *testing.T) {
			path := writeCheckConfig(t, t.TempDir(), []string{"--unsupported", "--sleep-idle-seconds", "1"})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`family = "llama.cpp"`), []byte("family = "+strconv.Quote(kind)), 1)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			exit := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &stdout, &stderr)
			var report infra.EngineCheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if exit != 0 || len(report.Failures) != 0 || report.KVRAM.ReasonCode != "engine_kind_unsupported" {
				t.Fatalf("non-llama entry subjected to K0: exit=%d report=%+v", exit, report)
			}
		})
	}
}

// Generate the cases from the exact table the production parser uses. The
// fixed count pins the documented pass-through subset against accidental loss.
func TestEngineCheckMissingPassthroughValues(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../../internal/infra/engine_check.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "engineCheckFlags" {
			return true
		}
		for _, element := range spec.Values[0].(*ast.CompositeLit).Elts {
			pair := element.(*ast.KeyValueExpr)
			fields := pair.Value.(*ast.CompositeLit).Elts
			var name, takesValue string
			for _, field := range fields {
				keyed := field.(*ast.KeyValueExpr)
				if keyed.Key.(*ast.Ident).Name == "name" {
					name = keyed.Value.(*ast.BasicLit).Value
				}
				if keyed.Key.(*ast.Ident).Name == "value" {
					takesValue = keyed.Value.(*ast.Ident).Name
				}
			}
			if name == `"other"` && takesValue == "true" {
				flag, err := strconv.Unquote(pair.Key.(*ast.BasicLit).Value)
				if err != nil {
					t.Fatal(err)
				}
				flags = append(flags, flag)
			}
		}
		return false
	})
	if len(flags) != 14 {
		t.Fatalf("documented pass-through subset: want 14 spellings, got %d", len(flags))
	}
	sort.Strings(flags)
	for _, flag := range flags {
		t.Run(flag, func(t *testing.T) {
			argv := append(checkFixtureArgv(), flag)
			report, exit := runEngineCheck(t, t.TempDir(), argv)
			if exit != 1 || len(report.Failures) != 1 || report.Failures[0].Code != "argv_missing_value" {
				t.Fatalf("missing value admitted: exit=%d report=%+v", exit, report)
			}
		})
	}
}

func TestEngineCheckKVContextPaddingOverflow(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		ctx, layers, heads, dim uint64
		known                   bool
	}{
		{"uint64_max", ^uint64(0), 32, 8, 128, false},
		{"padding_wrap", ^uint64(0) - 254, 32, 8, 128, false},
		{"context_limit", 1 << 40, 1, 1, 32, true},
		{"context_past_limit", (1 << 40) + 1, 1, 1, 32, false},
		{"payload_product_overflow", 1 << 40, 10000, 10000, 10000, false},
		{"dimension_past_limit", 256, 1, 1, 10001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var data bytes.Buffer
			write := func(value any) {
				if err := binary.Write(&data, binary.LittleEndian, value); err != nil {
					t.Fatal(err)
				}
			}
			str := func(value string) { write(uint64(len(value))); data.WriteString(value) }
			data.WriteString("GGUF")
			write(uint32(3))
			write(uint64(999))
			write(uint64(6))
			str("general.architecture")
			write(uint32(8))
			str("llama")
			for _, field := range []struct {
				name  string
				value uint64
			}{
				{"context_length", tc.ctx}, {"block_count", tc.layers}, {"attention.head_count_kv", tc.heads},
				{"attention.key_length", tc.dim}, {"attention.value_length", tc.dim},
			} {
				str("llama." + field.name)
				write(uint32(10))
				write(field.value)
			}
			if err := os.WriteFile(filepath.Join(dir, "model.gguf"), data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			argv := changeCheckArg(checkFixtureArgv(), "--ctx-size", "--ctx-size", "0")
			for _, layout := range []string{"--kv-unified", "--no-kv-unified"} {
				report, exit := runEngineCheck(t, dir, changeCheckArg(argv, "--no-kv-unified", layout))
				if exit != 0 {
					t.Fatalf("exit=%d report=%+v", exit, report)
				}
				if tc.known {
					if report.KVRAM.Bytes == nil || *report.KVRAM.Bytes != (1<<40)*128 {
						t.Fatalf("context limit not computed exactly: %+v", report.KVRAM)
					}
				} else if report.KVRAM.Status != "unknown" || report.KVRAM.ReasonCode != "kv_size_overflow" {
					t.Fatalf("unsafe arithmetic not bounded: %+v", report.KVRAM)
				}
			}
		})
	}
}

func changeCheckArg(argv []string, flag string, replacement ...string) []string {
	result := []string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == flag {
			result = append(result, replacement...)
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
		} else {
			result = append(result, argv[i])
		}
	}
	return result
}

func writeCheckConfig(t *testing.T, dir string, argv []string) string {
	t.Helper()
	var quoted []string
	for _, arg := range argv {
		quoted = append(quoted, strconv.Quote(arg))
	}
	path := filepath.Join(dir, "entry.toml")
	data := fmt.Sprintf("[agents.pi.profiles.fixture]\nfamily = \"llama.cpp\"\n[agents.pi.profiles.fixture.runtime]\nargv = [%s]\n", strings.Join(quoted, ","))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runEngineCheck(t *testing.T, dir string, argv []string) (infra.EngineCheckReport, int) {
	t.Helper()
	path := writeCheckConfig(t, dir, argv)
	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &stdout, &stderr)
	var report infra.EngineCheckReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("exit=%d stderr=%q output=%q: %v", code, stderr.String(), stdout.String(), err)
	}
	return report, code
}

func TestEngineCheckRequiredKnobs(t *testing.T) {
	base := checkFixtureArgv()
	cases := []struct {
		name, code string
		argv       []string
	}{
		{"parallel_missing", "k0_parallel", changeCheckArg(base, "--parallel")},
		{"parallel_auto", "k0_parallel", changeCheckArg(base, "--parallel", "--parallel", "-1")},
		{"parallel_zero", "k0_parallel", changeCheckArg(base, "--parallel", "--parallel", "0")},
		{"parallel_duplicate", "k0_parallel", append(append([]string{}, base...), "-np", "1")},
		{"layout_missing", "k0_layout", changeCheckArg(base, "--no-kv-unified")},
		{"layout_contradiction", "k0_layout", append(append([]string{}, base...), "--kv-unified")},
		{"cache_ram_missing", "k0_cache_ram", changeCheckArg(base, "--cache-ram")},
		{"cache_ram_enabled", "k0_cache_ram", changeCheckArg(base, "--cache-ram", "--cache-ram", "8192")},
		{"idle_slots_missing", "k0_idle_slots", changeCheckArg(base, "--no-cache-idle-slots")},
		{"idle_slots_contradiction", "k0_idle_slots", append(append([]string{}, base...), "--cache-idle-slots")},
		{"template_missing", "k0_template", changeCheckArg(base, "--chat-template-file")},
		{"template_empty", "k0_template", changeCheckArg(base, "--chat-template-file", "--chat-template-file", "")},
		{"template_override", "k0_template", append(append([]string{}, base...), "--in-prefix", "prefix")},
		{"template_conflict", "k0_template", append(append([]string{}, base...), "--chat-template", "chatml")},
		{"type_k_missing", "k0_cache_type_k", changeCheckArg(base, "--cache-type-k")},
		{"type_k_invalid", "k0_cache_type_k", changeCheckArg(base, "--cache-type-k", "--cache-type-k", "banana")},
		{"type_v_missing", "k0_cache_type_v", changeCheckArg(base, "--cache-type-v")},
		{"type_v_invalid", "k0_cache_type_v", changeCheckArg(base, "--cache-type-v", "--cache-type-v", "banana")},
		{"sleep_zero", "k0_sleep_idle", append(append([]string{}, base...), "--sleep-idle-seconds", "0")},
		{"sleep_present", "k0_sleep_idle", append(append([]string{}, base...), "--sleep-idle-seconds", "30")},
		{"router_directory", "k0_router", append(append([]string{}, base...), "--models-dir", "router")},
		{"router_preset", "k0_router", append(append([]string{}, base...), "--models-preset", "preset.ini")},
		{"router_no_model", "k0_router", changeCheckArg(base, "-m")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, exit := runEngineCheck(t, t.TempDir(), tc.argv)
			if exit != 1 {
				t.Fatalf("unsafe argv admitted: exit=%d report=%+v", exit, report)
			}
			if len(report.Failures) != 1 || report.Failures[0].Code != tc.code {
				t.Fatalf("want only %s, got %+v", tc.code, report.Failures)
			}
		})
	}
}

func TestEngineCheckAllRefusalsReported(t *testing.T) {
	report, code := runEngineCheck(t, t.TempDir(), []string{"--sleep-idle-seconds", "0", "--models-dir", "router"})
	if code != 1 || len(report.Failures) != 9 {
		t.Fatalf("want 9 independent K0 refusals, exit=%d got=%+v", code, report.Failures)
	}
}

// Every combination must preserve each independent refusal, rather than letting
// an earlier failure or a missing model hide another K0 gate.
func TestEngineCheckK0RefusalProperties(t *testing.T) {
	knobs := []struct{ flag, code string }{
		{"--parallel", "k0_parallel"}, {"--no-kv-unified", "k0_layout"}, {"--cache-ram", "k0_cache_ram"},
		{"--no-cache-idle-slots", "k0_idle_slots"}, {"--chat-template-file", "k0_template"},
		{"--cache-type-k", "k0_cache_type_k"}, {"--cache-type-v", "k0_cache_type_v"},
	}
	dir := t.TempDir()
	for mask := 0; mask < 1<<len(knobs); mask++ {
		argv := checkFixtureArgv()
		want := map[string]bool{}
		for index, knob := range knobs {
			if mask&(1<<index) != 0 {
				argv = changeCheckArg(argv, knob.flag)
				want[knob.code] = true
			}
		}
		report, exit := runEngineCheck(t, dir, argv)
		if (exit == 0) != (len(want) == 0) || len(report.Failures) != len(want) {
			t.Fatalf("mask=%d exit=%d failures=%+v want=%v", mask, exit, report.Failures, want)
		}
		for _, failure := range report.Failures {
			if !want[failure.Code] {
				t.Fatalf("mask=%d unexpected %s", mask, failure.Code)
			}
		}
	}
}

func writeCheckGGUF(t *testing.T, dir, arch string, explicitDims bool) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	write := func(value any) {
		if err := binary.Write(f, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	str := func(value string) {
		write(uint64(len(value)))
		if _, err := f.WriteString(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString("GGUF"); err != nil {
		t.Fatal(err)
	}
	write(uint32(3))
	write(uint64(999)) // No tensor descriptors/data: check must stop at metadata.
	count := uint64(6)
	if explicitDims {
		count = 8
	}
	write(count)
	str("general.architecture")
	write(uint32(8))
	str(arch)
	for _, pair := range []struct {
		key   string
		value uint32
	}{
		{"block_count", 32}, {"attention.head_count_kv", 8}, {"embedding_length", 4096}, {"attention.head_count", 32}, {"context_length", 16384},
	} {
		str(arch + "." + pair.key)
		write(uint32(4))
		write(pair.value)
	}
	if explicitDims {
		str(arch + ".attention.key_length")
		write(uint32(4))
		write(uint32(64))
		str(arch + ".attention.value_length")
		write(uint32(4))
		write(uint32(128))
	}
}

func TestEngineCheckKVRAMGolden(t *testing.T) {
	for _, tc := range []struct {
		layout, cache string
		cells, bytes  uint64
	}{
		{"--kv-unified", "f16", 1792, 234881024},
		{"--no-kv-unified", "f16", 2304, 301989888},
		{"--kv-unified", "q8_0", 1792, 124780544},
		{"--no-kv-unified", "q8_0", 2304, 160432128},
	} {
		t.Run(tc.layout+"/"+tc.cache, func(t *testing.T) {
			dir := t.TempDir()
			writeCheckGGUF(t, dir, "qwen3", false)
			argv := changeCheckArg(checkFixtureArgv(), "--no-kv-unified", tc.layout)
			argv = changeCheckArg(argv, "--parallel", "--parallel", "3")
			argv = changeCheckArg(argv, "--ctx-size", "--ctx-size", "1600")
			argv = changeCheckArg(argv, "--cache-type-k", "--cache-type-k", tc.cache)
			argv = changeCheckArg(argv, "--cache-type-v", "--cache-type-v", tc.cache)
			report, code := runEngineCheck(t, dir, argv)
			if code != 0 || report.KVRAM.Bytes == nil || *report.KVRAM.Bytes != tc.bytes || report.KVRAM.Cells != tc.cells {
				t.Fatalf("exit=%d report=%+v want cells=%d bytes=%d", code, report, tc.cells, tc.bytes)
			}
		})
	}
}

func TestEngineCheckKVRAMDimensionsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	writeCheckGGUF(t, dir, "llama", true)
	argv := changeCheckArg(checkFixtureArgv(), "--ctx-size")
	argv = changeCheckArg(argv, "--cache-type-v", "--cache-type-v", "q8_0")
	report, code := runEngineCheck(t, dir, argv)
	if code != 0 || report.KVRAM.Bytes == nil || *report.KVRAM.Bytes != 1107296256 {
		t.Fatalf("mixed types, explicit K/V dimensions and model context default: exit=%d report=%+v", code, report)
	}
}

func TestEngineCheckShortAliasesAndPinnedInline(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"-m", "absent.gguf", "-c", "16384", "-np", "1", "-no-kvu", "-cram", "0", "--no-cache-idle-slots", "--chat-template", "chatml", "-ctk", "f16", "-ctv", "q8_0"}
	for _, layout := range []string{"-no-kvu", "-kvu"} {
		candidate := changeCheckArg(argv, "-no-kvu", layout)
		report, code := runEngineCheck(t, dir, candidate)
		if code != 0 || report.KVRAM.Status != "unknown" || report.KVRAM.ReasonCode != "model_absent" {
			t.Fatalf("exit=%d report=%+v", code, report)
		}
	}
}

func TestEngineCheckModelAbsenceIsNotReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		exit         int
	}{
		{"absent", "model_absent", 0}, {"malformed", "gguf_invalid", 1}, {"directory", "gguf_read_failed", 1}, {"hybrid", "gguf_architecture_unsupported", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			switch tc.name {
			case "malformed":
				if err := os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("GGUF"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, "model.gguf"), 0700); err != nil {
					t.Fatal(err)
				}
			case "hybrid":
				writeCheckGGUF(t, dir, "qwen35", false)
			}
			report, code := runEngineCheck(t, dir, checkFixtureArgv())
			if code != tc.exit || report.KVRAM.Status != "unknown" || report.KVRAM.ReasonCode != tc.reason {
				t.Fatalf("exit=%d report=%+v", code, report)
			}
		})
	}
}

func TestEngineCheckConfigRefusals(t *testing.T) {
	for _, tc := range []struct{ name, code, data string }{
		{"absent", "engine_config_absent", ""}, {"directory", "engine_config_read_failed", ""}, {"malformed", "engine_config_malformed", "["}, {"unknown_entry", "engine_entry_absent", "[agents.pi.profiles.other]\nfamily=\"llama.cpp\""},
		{"missing_kind", "engine_entry_malformed", "[agents.pi.profiles.fixture.runtime]\nargv=[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "entry.toml")
			if tc.name == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if tc.data != "" {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			exit := run([]string{"check", "--config", path, "--engine", "fixture", "--json"}, &stdout, &stderr)
			var report infra.EngineCheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if exit != 1 || len(report.Failures) != 1 || report.Failures[0].Code != tc.code {
				t.Fatalf("exit=%d report=%+v", exit, report)
			}
		})
	}
}

func TestEngineCheckTextAndUsage(t *testing.T) {
	path := writeCheckConfig(t, t.TempDir(), checkFixtureArgv())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check", "--config", path, "--engine", "fixture"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "KV RAM: unknown (model_absent)") {
		t.Fatalf("exit=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{{"check"}, {"check", "--bogus"}, {"check", "--config", path, "--engine", "fixture", "extra"}} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%v exit=%d", args, code)
		}
	}
}

func TestEngineCheckArgvSubset(t *testing.T) {
	for _, token := range []string{"--parallel=1", "--router", "--model-url", "--ctx-size=16384"} {
		argv := append(checkFixtureArgv(), token)
		report, exit := runEngineCheck(t, t.TempDir(), argv)
		if exit != 1 || len(report.Failures) == 0 || report.Failures[0].Code != "argv_unsupported" {
			t.Fatalf("outside-subset token %q admitted: exit=%d report=%+v", token, exit, report)
		}
	}
	argv := changeCheckArg(checkFixtureArgv(), "--cache-ram", "-cram", "00")
	report, exit := runEngineCheck(t, t.TempDir(), argv)
	if exit != 0 {
		t.Fatalf("numeric zero refused: %+v", report)
	}
}
