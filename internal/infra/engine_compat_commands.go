package infra

import (
	"errors"
	"os"
	"path/filepath"
)

type EngineCompatibilityRender struct {
	Engine       string             `json:"engine"`
	Entry        EngineCatalogEntry `json:"entry"`
	Executable   string             `json:"executable"`
	Argv         []string           `json:"argv"`
	Availability string             `json:"engine_availability"`
	Reason       string             `json:"engine_availability_reason,omitempty"`
}

// RenderEngineCompatibility requires an explicit catalog engine. An alias can
// never select a provider/profile based on its own spelling or an absent value.
func RenderEngineCompatibility(home, name, alias string) (EngineCompatibilityRender, error) {
	result := EngineCompatibilityRender{Availability: "unknown"}
	if alias != "qwen-infra" && alias != "model-harness" {
		return result, legacyImportRefuse("alias.unsupported", errors.New("deprecated alias: migrate to curator-engines --engine NAME"))
	}
	if name == "" {
		return result, legacyImportRefuse("alias.engine_required", errors.New("explicit --engine is required"))
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return result, err
		}
	}
	entry, state, catalogErr := ReadEngineCatalogEntry(filepath.Join(home, ".curator", "engines.toml"), name)
	if catalogErr != nil {
		return result, catalogErr
	}
	if state != EngineCatalogPresent {
		return result, legacyImportRefuse("alias.engine_absent", errors.New("selected engine is absent; import or configure it explicitly"))
	}
	result.Engine = name
	result.Entry = entry
	result.Executable = entry.Executable
	if entry.Mode == "ssh" {
		result.Executable = entry.SSHExecutable
		if result.Executable == "" {
			result.Executable = "/usr/bin/ssh"
		}
	}
	result.Argv = engineCatalogRuntimeArgv(entry, catalogEndpointPort(name, entry), entry.ServedModelID)
	if entry.ServedModelID == "" {
		result.Reason = "served_model_id_missing"
	} else {
		result.Reason, _ = engineCatalogExecutableNotRunnable(entry)
	}
	return result, nil
}

// ValidateLegacyEngineCompatibilityConfig checks the old --config selection
// against the already selected owner catalog. It cannot supply a fallback.
func ValidateLegacyEngineCompatibilityConfig(path string, selected EngineCompatibilityRender) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return legacyImportRefuse("alias.config_read", err)
	}
	document, _, err := ReadLegacyEngineProfiles(data)
	if err != nil {
		return err
	}
	entry, present := document.Engines[selected.Engine]
	if !present {
		return legacyImportRefuse("alias.config_engine_absent", errors.New("selected profile absent in legacy config"))
	}
	if !legacySameEntry(entry, selected.Entry) {
		return legacyImportRefuse("alias.config_conflict", errors.New("legacy config differs from selected engine; migrate explicitly"))
	}
	return nil
}
