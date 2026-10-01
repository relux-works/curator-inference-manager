package infra

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ReadLegacyEngineProfiles transcribes the closed legacy profile schema into
// the existing catalog types. It never infers engine kind from executable names.
func ReadLegacyEngineProfiles(data []byte) (EngineCatalogDocument, map[string]string, error) {
	return readLegacyEngineProfiles(data, nil)
}

// Presence is collected before defaults, normalization, and legacy key translation.
func readLegacyEngineProfiles(data []byte, presence map[string]map[string]bool) (EngineCatalogDocument, map[string]string, error) {
	result := EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{}}
	reasons := map[string]string{}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return result, reasons, legacyImportRefuse("profiles.decode", err)
	}
	if len(raw) != 1 {
		return result, reasons, legacyImportRefuse("profiles.document", errors.New("only profiles is supported"))
	}
	profiles, ok := raw["profiles"].(map[string]any)
	if !ok {
		return result, reasons, legacyImportRefuse("profiles.table", errors.New("profiles table required"))
	}
	for name, value := range profiles {
		entry, ok := value.(map[string]any)
		if !ok {
			return result, reasons, legacyImportRefuse("profiles.entry", errors.New("entry table required"))
		}
		if presence != nil {
			presence[name] = map[string]bool{}
			legacyCollectPresence(entry, "", presence[name])
		}
		// served_model_id and the renamed knob keys are catalog-only, never old fields.
		if _, present := entry["served_model_id"]; present {
			return result, reasons, legacyImportRefuse("profiles.catalog_only", errors.New("catalog-only field"))
		}
		if _, present := entry["engine"]; !present {
			entry["engine"] = "mlx-lm"
		}
		if value, present := entry["knobs"]; present {
			knobs, ok := value.(map[string]any)
			if !ok {
				return result, reasons, legacyImportRefuse("profiles.knobs", errors.New("knobs table required"))
			}
			for _, pair := range [][2]string{{"kv_context_tokens", "kv_context_size"}, {"prefill_chunk_tokens", "prefill_chunk_size"}} {
				if _, present := knobs[pair[1]]; present {
					return result, reasons, legacyImportRefuse("profiles.knob_catalog_only", errors.New("catalog-only knob"))
				}
				if value, present := knobs[pair[0]]; present {
					text, ok := value.(string)
					if !ok {
						return result, reasons, legacyImportRefuse("profiles.knob_type", errors.New("legacy knob must be string"))
					}
					if text != "unbounded" && !engineCatalogPositiveInteger(text) {
						return result, reasons, legacyImportRefuse("profiles.knob_value", errors.New("invalid legacy knob"))
					}
					knobs[pair[1]] = engineCatalogKnobSizeWireValue(text)
					delete(knobs, pair[0])
				}
			}
		}
	}
	raw = map[string]any{"schema_version": int64(1), "engines": profiles}
	encoded, err := toml.Marshal(raw)
	if err != nil {
		return result, reasons, legacyImportRefuse("profiles.encode", err)
	}
	if err := validateEngineCatalogExactKeys(encoded); err != nil {
		return result, reasons, err
	}
	if err := toml.NewDecoder(bytes.NewReader(encoded)).DisallowUnknownFields().Decode(&result); err != nil {
		return result, reasons, legacyImportRefuse("profiles.types", err)
	}
	for name, entry := range result.Engines {
		if err := legacyRefuseCredentialArgv(entry.Argv); err != nil {
			return result, reasons, err
		}
		entry, err = normalizeEngineCatalogEntry(entry)
		if err != nil {
			return result, reasons, err
		}
		entry.ServedModelID, reasons[name] = legacyServedModelID(entry)
		if presence == nil {
			if err := validateEngineCatalogEntry(name, entry); err != nil {
				return result, reasons, err
			}
		}
		if code, _ := engineCatalogExecutableNotRunnable(entry); code != "" {
			reasons[name] = code
		}
		result.Engines[name] = entry
	}
	return result, reasons, nil
}

var legacyCredentialFlag = regexp.MustCompile(`(?i)(^|[-_])(api[-_]?key|token|secret|password|auth(entication|orization)?|bearer|credential)([-_]|$)`)
var legacySecretValue = regexp.MustCompile(`(?i)(^(sk-|ghp_|xox[baprs]-)|^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$)`)

func legacyRefuseCredentialArgv(argv []string) error {
	for _, token := range argv {
		flag, value, hasValue := strings.Cut(token, "=")
		if strings.HasPrefix(flag, "-") && legacyCredentialFlag.MatchString(flag) {
			// Neither the flag suffix after '=' nor a following value leaves here.
			return legacyImportRefuse("profiles.credential_flag", fmt.Errorf("credential flag %s", strings.ToLower(legacyCredentialFlag.FindString(flag))))
		}
		if legacySecretValue.MatchString(token) || hasValue && legacySecretValue.MatchString(value) {
			return legacyImportRefuse("profiles.credential_value", errors.New("secret-shaped argv value"))
		}
	}
	return nil
}

// First legacy identity option wins. mlx-lm advertises the HF repository name,
// never a basename guessed from a local model path. mlx-swift and llama-cpp
// retain their server basename convention unless an explicit identity exists.
func legacyServedModelID(entry EngineCatalogEntry) (string, string) {
	if entry.Mode == "ssh" {
		return "", "remote_served_model_id_unknown"
	}
	flags := []string{"--alias", "-a"}
	if entry.Engine == "mlx-swift" {
		flags = []string{"--model-id"}
	}
	if entry.Engine == "mlx-lm" {
		flags = nil
	}
	candidate := legacyFirstOption(entry.Argv, flags...)
	if entry.Engine == "llama-cpp" {
		candidate = strings.SplitN(candidate, ",", 2)[0]
	}
	if candidate == "" {
		candidate = legacyFirstOption(entry.Argv, "--model", "-m", "-hf", "--hf-repo")
		if candidate == "" {
			candidate = entry.Model
		}
		if entry.Engine == "mlx-lm" && filepath.IsAbs(candidate) {
			return "", "mlx_lm_local_path_identity_unknown"
		}
		if entry.Engine != "mlx-lm" {
			candidate = filepath.Base(candidate)
		}
	}
	if !engineCatalogServedModelIDPattern.MatchString(candidate) {
		return "", "served_model_id_missing"
	}
	return candidate, ""
}
func legacyFirstOption(argv []string, names ...string) string {
	for index, token := range argv {
		for _, name := range names {
			if token == name && index+1 < len(argv) {
				return argv[index+1]
			}
			if strings.HasPrefix(token, name+"=") {
				return strings.TrimPrefix(token, name+"=")
			}
		}
	}
	return ""
}

func legacyCatalogDocument(data []byte) (EngineCatalogDocument, error) {
	var document EngineCatalogDocument
	if err := validateEngineCatalogExactKeys(data); err != nil {
		return document, err
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&document); err != nil {
		return document, legacyImportRefuse("destination.decode", err)
	}
	if document.SchemaVersion != 1 {
		return document, legacyImportRefuse("destination.version", errors.New("schema_version 1 required"))
	}
	for name, entry := range document.Engines {
		normalized, err := normalizeEngineCatalogEntry(entry)
		if err != nil {
			return document, err
		}
		if err := validateEngineCatalogEntry(name, normalized); err != nil {
			return document, err
		}
		document.Engines[name] = normalized
	}
	return document, nil
}
func legacySameEntry(a, b EngineCatalogEntry) bool {
	left, errA := marshalEngineCatalogEntry(a)
	right, errB := marshalEngineCatalogEntry(b)
	return errA == nil && errB == nil && bytes.Equal(left, right)
}
func legacyMergeCatalog(before, source EngineCatalogDocument, owned bool, presence map[string]map[string]bool, fieldPlan map[string]map[string]string) (EngineCatalogDocument, bool, error) {
	changed := false
	if before.Engines == nil {
		before.Engines = map[string]EngineCatalogEntry{}
	}
	for name, entry := range source.Engines {
		old, present := before.Engines[name]
		fields := map[string]string{}
		if present {
			entry = legacyOverlayEntry(old, entry, presence[name], fields)
		} else {
			legacyOverlayFields(reflect.ValueOf(&entry).Elem(), reflect.ValueOf(entry), presence[name], "", fields, false)
		}
		if !present {
			if !presence[name]["engine"] {
				fields["engine"] = "default"
			}
			fields["served_model_id"] = "derive"
		}
		fieldPlan[name] = fields
		if err := validateEngineCatalogEntry(name, entry); err != nil {
			return before, false, err
		}
		if present && legacySameEntry(old, entry) {
			continue
		}
		if present && !owned {
			return before, false, legacyImportRefuse("destination.conflict", fmt.Errorf("engine %s differs", name))
		}
		before.Engines[name] = entry
		changed = true
	}
	return before, changed, nil
}

// The closed catalog schema supplies the typed values; this map retains only
// source key presence, including explicit false, zero, and empty arrays.
func legacyCollectPresence(table map[string]any, prefix string, fields map[string]bool) {
	for key, value := range table {
		path := prefix + key
		switch path {
		case "knobs.kv_context_tokens":
			path = "knobs.kv_context_size"
		case "knobs.prefill_chunk_tokens":
			path = "knobs.prefill_chunk_size"
		}
		fields[path] = true
		if nested, ok := value.(map[string]any); ok {
			legacyCollectPresence(nested, path+".", fields)
		}
	}
}

func legacyOverlayEntry(old, incoming EngineCatalogEntry, presence map[string]bool, plan map[string]string) EngineCatalogEntry {
	result := old
	legacyOverlayFields(reflect.ValueOf(&result).Elem(), reflect.ValueOf(incoming), presence, "", plan, true)
	// Served identity is derived from the effective identity fields, never from
	// a source default that might have been absent in the destination overlay.
	if presence["mode"] || presence["engine"] || presence["model"] || presence["argv"] {
		result.ServedModelID, _ = legacyServedModelID(result)
		plan["served_model_id"] = "derive"
	}
	return result
}

// Walk schema tags rather than zero values. Nested pointers are copied before
// overlay so planning never mutates the original destination entry.
func legacyOverlayFields(destination, incoming reflect.Value, presence map[string]bool, prefix string, plan map[string]string, existing bool) {
	typ := destination.Type()
	for index := 0; index < typ.NumField(); index++ {
		key := strings.Split(typ.Field(index).Tag.Get("toml"), ",")[0]
		if key == "-" {
			continue
		}
		path := prefix + key
		dst, src := destination.Field(index), incoming.Field(index)
		if dst.Kind() == reflect.Pointer {
			// A table is a container, not a field value. An empty table cannot
			// introduce a policy or erase any of its absent members.
			hasMember := false
			for member := range presence {
				if strings.HasPrefix(member, path+".") {
					hasMember = true
					break
				}
			}
			if dst.IsNil() && !hasMember {
				plan[path] = "absent"
				continue
			}
			copy := reflect.New(dst.Type().Elem())
			if !dst.IsNil() {
				copy.Elem().Set(dst.Elem())
			}
			if src.IsNil() {
				src = reflect.New(dst.Type().Elem())
			}
			legacyOverlayFields(copy.Elem(), src.Elem(), presence, path+".", plan, existing && !dst.IsNil())
			dst.Set(copy)
			continue
		}
		if !presence[path] {
			if existing {
				plan[path] = "preserve"
			} else {
				plan[path] = "absent"
			}
			continue
		}
		// Normalized knob strings are the catalog's in-memory representation.
		switch path {
		case "knobs.kv_context_size":
			dst = destination.FieldByName("KVContextTokens")
			src = incoming.FieldByName("KVContextTokens")
		case "knobs.prefill_chunk_size":
			dst = destination.FieldByName("PrefillChunkTokens")
			src = incoming.FieldByName("PrefillChunkTokens")
		}
		plan[path] = "set"
		if existing && reflect.DeepEqual(dst.Interface(), src.Interface()) {
			plan[path] = "unchanged"
		}
		dst.Set(src)
	}
}
