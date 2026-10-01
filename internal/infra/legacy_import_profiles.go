package infra

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ReadLegacyEngineProfiles transcribes the closed legacy profile schema into
// the existing catalog types. It never infers engine kind from executable names.
func ReadLegacyEngineProfiles(data []byte) (EngineCatalogDocument, map[string]string, error) {
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
	for _, value := range profiles {
		entry, ok := value.(map[string]any)
		if !ok {
			return result, reasons, legacyImportRefuse("profiles.entry", errors.New("entry table required"))
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
		if err := validateEngineCatalogEntry(name, entry); err != nil {
			return result, reasons, err
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
func legacyMergeCatalog(before, source EngineCatalogDocument, owned bool) (EngineCatalogDocument, bool, error) {
	changed := false
	if before.Engines == nil {
		before.Engines = map[string]EngineCatalogEntry{}
	}
	for name, entry := range source.Engines {
		old, present := before.Engines[name]
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
