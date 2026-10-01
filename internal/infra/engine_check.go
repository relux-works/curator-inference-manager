package infra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"

	"github.com/pelletier/go-toml/v2"
)

// EngineCheckEntry is the read-only check boundary; it does not resolve or launch a runtime.
type EngineCheckEntry struct {
	Name      string
	Kind      string
	Argv      []string
	Directory string
}

type EngineCheckFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type EngineKVRAM struct {
	Status     string  `json:"status"`
	Bytes      *uint64 `json:"bytes,omitempty"`
	ReasonCode string  `json:"reason_code,omitempty"`
	Layout     string  `json:"layout,omitempty"`
	Cells      uint64  `json:"cells,omitempty"`
}

type EngineCheckReport struct {
	Engine   string               `json:"engine"`
	Failures []EngineCheckFailure `json:"failures"`
	KVRAM    EngineKVRAM          `json:"kv_ram"`
}

// CheckEngineFile reads ONLY the supplied file: schema v1 catalogs or legacy Pi
// entries. Catalog validation and argv transcription use the existing owner.
func CheckEngineFile(path, name string) EngineCheckReport {
	report := EngineCheckReport{Engine: name, Failures: []EngineCheckFailure{}, KVRAM: EngineKVRAM{Status: "unknown", ReasonCode: "entry_unavailable"}}
	fail := func(code string) EngineCheckReport {
		report.Failures = append(report.Failures, EngineCheckFailure{Code: code, Message: "cannot read selected engine entry"})
		return report
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fail("engine_config_absent")
	}
	if err != nil {
		return fail("engine_config_read_failed")
	}
	var config struct {
		SchemaVersion *int           `toml:"schema_version"`
		Engines       map[string]any `toml:"engines"`
		Agents        struct {
			Pi struct {
				Profiles map[string]struct {
					Family  string
					Runtime struct{ Argv []string }
				}
			}
		}
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return fail("engine_config_malformed")
	}
	if config.SchemaVersion != nil || config.Engines != nil {
		entry, state, err := ReadEngineCatalogEntry(path, name)
		if err != nil {
			if state == EngineCatalogReadFailed {
				return fail("engine_config_read_failed")
			}
			return fail("engine_config_malformed")
		}
		if state != EngineCatalogPresent {
			return fail("engine_entry_absent")
		}
		kind := entry.Engine
		if entry.Mode == "ssh" {
			kind = "ssh"
		}
		// Endpoint placeholders do not affect KV sizing. Transcribe configured
		// model/knobs without launching or rewriting legacy identity options.
		return CheckEngineEntry(EngineCheckEntry{Name: name, Kind: kind, Argv: engineCatalogIdentityArgv(entry, 1), Directory: filepath.Dir(path)})
	}
	profile, ok := config.Agents.Pi.Profiles[name]
	if !ok {
		return fail("engine_entry_absent")
	}
	if profile.Family == "" {
		return fail("engine_entry_malformed")
	}
	return CheckEngineEntry(EngineCheckEntry{Name: name, Kind: profile.Family, Argv: profile.Runtime.Argv, Directory: filepath.Dir(path)})
}

type engineArgSpec struct {
	name    string
	value   bool
	reading inferenceengine.Fact
	mlxOnly bool
}

var engineCheckFlags = map[string]engineArgSpec{
	"--parallel": {name: "parallel", value: true}, "-np": {name: "parallel", value: true},
	"--kv-unified": {name: "unified", value: false}, "-kvu": {name: "unified", value: false},
	"--no-kv-unified": {name: "separate", value: false}, "-no-kvu": {name: "separate", value: false},
	"--cache-ram": {name: "cache_ram", value: true}, "-cram": {name: "cache_ram", value: true},
	"--no-cache-idle-slots": {name: "no_idle", value: false}, "--cache-idle-slots": {name: "idle", value: false},
	"--chat-template-file": {name: "template_file", value: true}, "--chat-template": {name: "template", value: true},
	"--cache-type-k": {name: "type_k", value: true}, "-ctk": {name: "type_k", value: true},
	"--cache-type-v": {name: "type_v", value: true}, "-ctv": {name: "type_v", value: true},
	"--sleep-idle-seconds": {name: "sleep", value: true},
	"--model":              {name: "model", value: true}, "-m": {name: "model", value: true},
	"--ctx-size": {name: "ctx", value: true, reading: inferenceengine.FactContextArgv}, "-c": {name: "ctx", value: true, reading: inferenceengine.FactContextArgv},
	"--models-dir": {name: "router", value: true}, "--models-preset": {name: "router", value: true}, "--models-max": {name: "router", value: true},
	"--models-autoload": {name: "router", value: false}, "--no-models-autoload": {name: "router", value: false},
	"--in-prefix": {name: "template_override", value: true}, "--in-suffix": {name: "template_override", value: true},
	"--alias": {name: "other", value: true}, "--host": {name: "other", value: true}, "--port": {name: "other", value: true},
	"--flash-attn": {name: "other", value: true}, "-fa": {name: "other", value: true}, "--gpu-layers": {name: "other", value: true}, "-ngl": {name: "other", value: true},
	"--batch-size": {name: "other", value: true}, "-b": {name: "other", value: true}, "--ubatch-size": {name: "other", value: true, reading: inferenceengine.FactPrefillArgv}, "-ub": {name: "other", value: true, reading: inferenceengine.FactPrefillArgv},
	"--threads": {name: "other", value: true}, "-t": {name: "other", value: true}, "--jinja": {name: "other", value: false},
	"--no-webui": {name: "other", value: false}, "--no-ui": {name: "other", value: false},
	// Capacity spellings share this table with status. MLX options do not
	// expand the documented llama-cpp check subset.
	"--max-kv-size":       {value: true, reading: inferenceengine.FactContextArgv, mlxOnly: true},
	"--prefill-step-size": {value: true, reading: inferenceengine.FactPrefillArgv, mlxOnly: true},
}

func parseEngineCheckArgs(argv []string) (map[string][]string, []EngineCheckFailure) {
	values := map[string][]string{}
	var failures []EngineCheckFailure
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		spec, ok := engineCheckFlags[token]
		if !ok || spec.mlxOnly {
			failures = append(failures, EngineCheckFailure{Code: "argv_unsupported", Message: "argument outside the documented check subset"})
			continue
		}
		value := "present"
		if spec.value {
			value = ""
			if i+1 >= len(argv) {
				failures = append(failures, EngineCheckFailure{Code: "argv_missing_value", Message: "argument requires a following value"})
			} else {
				// llama-server consumes the next token verbatim, even if it
				// looks like an option. Data must never satisfy a K0 knob.
				i++
				value = argv[i]
			}
		}
		values[spec.name] = append(values[spec.name], value)
	}
	return values, failures
}

func singleEngineArg(values map[string][]string, name string) string {
	if len(values[name]) != 1 {
		return ""
	}
	return values[name][0]
}

func positiveEngineArg(values map[string][]string, name string) uint64 {
	value, err := strconv.ParseUint(singleEngineArg(values, name), 10, 32)
	if err != nil {
		return 0
	}
	return value
}

// CheckEngineEntry reports all K0 refusals, independently of model availability.
func CheckEngineEntry(entry EngineCheckEntry) EngineCheckReport {
	report := EngineCheckReport{Engine: entry.Name, Failures: []EngineCheckFailure{}, KVRAM: EngineKVRAM{Status: "unknown", ReasonCode: "engine_kind_unsupported"}}
	if entry.Kind != "llama-cpp" && entry.Kind != "llama.cpp" {
		return report
	}
	values, failures := parseEngineCheckArgs(entry.Argv)
	report.Failures = append(report.Failures, failures...)
	refuse := func(code, message string) {
		report.Failures = append(report.Failures, EngineCheckFailure{Code: code, Message: message})
	}
	parallel := positiveEngineArg(values, "parallel")
	if parallel == 0 {
		refuse("k0_parallel", "require one explicit positive --parallel")
	}
	unified := len(values["unified"]) == 1 && len(values["separate"]) == 0
	separate := len(values["separate"]) == 1 && len(values["unified"]) == 0
	if !unified && !separate {
		refuse("k0_layout", "require exactly one explicit KV layout")
	}
	if !zeroEngineArg(values, "cache_ram") {
		refuse("k0_cache_ram", "require --cache-ram 0")
	}
	if len(values["no_idle"]) != 1 || len(values["idle"]) != 0 {
		refuse("k0_idle_slots", "require --no-cache-idle-slots without its inverse")
	}
	pinned := (singleEngineArg(values, "template_file") != "" && len(values["template"]) == 0) || (singleEngineArg(values, "template") != "" && len(values["template_file"]) == 0)
	if !pinned || len(values["template_override"]) != 0 {
		refuse("k0_template", "require one pinned chat template without prefix/suffix overrides")
	}
	if !validEngineCacheType(singleEngineArg(values, "type_k")) {
		refuse("k0_cache_type_k", "require one explicit valid K cache type")
	}
	if !validEngineCacheType(singleEngineArg(values, "type_v")) {
		refuse("k0_cache_type_v", "require one explicit valid V cache type")
	}
	if len(values["sleep"]) != 0 {
		refuse("k0_sleep_idle", "sleep-idle is forbidden, including zero")
	}
	if len(values["router"]) != 0 || singleEngineArg(values, "model") == "" {
		refuse("k0_router", "require a single local model and no router mode")
	}
	report.KVRAM = engineKVRAM(entry, values, parallel, unified, separate)
	if report.KVRAM.ReasonCode == "gguf_read_failed" || report.KVRAM.ReasonCode == "gguf_invalid" {
		refuse(report.KVRAM.ReasonCode, "model metadata could not be read")
	}
	return report
}

func engineKVRAM(entry EngineCheckEntry, values map[string][]string, parallel uint64, unified, separate bool) EngineKVRAM {
	unknown := func(code string) EngineKVRAM { return EngineKVRAM{Status: "unknown", ReasonCode: code} }
	model := singleEngineArg(values, "model")
	if model == "" {
		return unknown("model_unspecified")
	}
	if !filepath.IsAbs(model) {
		model = filepath.Join(entry.Directory, model)
	}
	metadata, code := readEngineGGUF(model)
	if code != "" {
		return unknown(code)
	}
	ctx := positiveEngineArg(values, "ctx")
	if len(values["ctx"]) == 0 || singleEngineArg(values, "ctx") == "0" {
		ctx = metadata.number(metadata.arch + ".context_length")
	}
	if ctx == 0 || parallel == 0 || (!unified && !separate) {
		return unknown("kv_arguments_invalid")
	}
	// Bound the untrusted training context BEFORE padding. In particular,
	// UINT64_MAX + 255 must not wrap into zero cells and a later zero divisor.
	if ctx > 1<<40 {
		return unknown("kv_size_overflow")
	}
	// llama.cpp c21284cdf: pad the total, then divide and pad each separate stream.
	cells := (ctx + 255) / 256 * 256
	layout := "unified"
	if separate {
		layout = "non-unified"
		perStream := cells / parallel
		if perStream == 0 {
			return unknown("kv_arguments_invalid")
		}
		perStream = (perStream + 255) / 256 * 256
		if perStream > ^uint64(0)/parallel {
			return unknown("kv_size_overflow")
		}
		cells = perStream * parallel
	}
	layers := metadata.number(metadata.arch + ".block_count")
	heads := metadata.number(metadata.arch + ".attention.head_count_kv")
	keyDim := metadata.number(metadata.arch + ".attention.key_length")
	valueDim := metadata.number(metadata.arch + ".attention.value_length")
	if keyDim == 0 || valueDim == 0 {
		embed, allHeads := metadata.number(metadata.arch+".embedding_length"), metadata.number(metadata.arch+".attention.head_count")
		if embed == 0 || allHeads == 0 || embed%allHeads != 0 {
			return unknown("gguf_dimensions_unknown")
		}
		if keyDim == 0 {
			keyDim = embed / allHeads
		}
		if valueDim == 0 {
			valueDim = embed / allHeads
		}
	}
	if layers == 0 || heads == 0 {
		return unknown("gguf_dimensions_unknown")
	}
	// Bound arithmetic before multiplication, and reject unsupported block geometry.
	if layers > 10000 || heads > 10000 || keyDim > 10000 || valueDim > 10000 || cells > 1<<40 {
		return unknown("kv_size_overflow")
	}
	k, okK := engineCacheRow(singleEngineArg(values, "type_k"), heads*keyDim)
	v, okV := engineCacheRow(singleEngineArg(values, "type_v"), heads*valueDim)
	if !okK || !okV {
		return unknown("cache_type_unsupported")
	}
	rowBytes := k + v
	if rowBytes > ^uint64(0)/layers || rowBytes*layers > ^uint64(0)/cells {
		return unknown("kv_size_overflow")
	}
	bytes := rowBytes * layers * cells
	return EngineKVRAM{Status: "known", Bytes: &bytes, Layout: layout, Cells: cells}
}

func engineCacheRow(cacheType string, elements uint64) (uint64, bool) {
	switch cacheType {
	case "f16":
		return elements * 2, true
	case "q8_0":
		return elements / 32 * 34, elements%32 == 0
	default:
		return 0, false
	}
}

func validEngineCacheType(value string) bool {
	switch value {
	case "f32", "f16", "bf16", "q8_0", "q4_0", "q4_1", "q5_0", "q5_1", "iq4_nl":
		return true
	default:
		return false
	}
}

func zeroEngineArg(values map[string][]string, name string) bool {
	value, err := strconv.ParseInt(singleEngineArg(values, name), 10, 64)
	return err == nil && value == 0
}

// Keep error strings free of model paths; check output may be attached as evidence.
func ggufError(format string, args ...any) error { return fmt.Errorf(format, args...) }
