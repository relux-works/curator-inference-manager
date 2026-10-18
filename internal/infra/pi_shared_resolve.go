//go:build !windows

package infra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const projectConfigFileName = "project-config.toml"

var readLegacyPiIdentityConfigFile = os.ReadFile

func resolveSharedProfile(projectDir, homeDir, cacheRoot, profileName string) (sharedResolvedProfile, error) {
	return resolveSharedProfileWithCatalog(projectDir, homeDir, cacheRoot, profileName, "")
}

func resolveSharedProfileWithCatalog(projectDir, homeDir, cacheRoot, profileName, catalogFile string) (sharedResolvedProfile, error) {
	if catalogFile != "" && !filepath.IsAbs(catalogFile) {
		return sharedResolvedProfile{}, engineCatalogReadFailed("catalog.selection", errors.New("config path must be absolute"))
	}
	if cacheRoot != "" && !filepath.IsAbs(cacheRoot) {
		return sharedResolvedProfile{}, sharedRuntimeError("shared_runtime_state_path_invalid", errors.New("cache root must be absolute"))
	}
	explicitCatalog := catalogFile != ""
	project, err := CanonicalProjectDir(projectDir)
	if err != nil {
		return sharedResolvedProfile{}, err
	}
	if homeDir == "" {
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return sharedResolvedProfile{}, err
		}
	}
	if err := RefuseActiveLegacyEngineState(cacheRoot); err != nil {
		return sharedResolvedProfile{}, err
	}
	var profiles map[string]PiProfile
	if profileName == "" {
		var selectedProfile string
		profiles, selectedProfile, err = readPiProfiles(project, homeDir)
		if err != nil {
			return sharedResolvedProfile{}, err
		}
		profileName = selectedProfile
	}
	if profileName == "" {
		return sharedResolvedProfile{}, sharedRuntimeError("shared_runtime_not_configured", errors.New("no managed Pi profile is selected"))
	}
	if !explicitCatalog {
		catalogFile, err = engineCatalogPath(homeDir)
		if err != nil {
			return sharedResolvedProfile{}, err
		}
	}
	entry, state, catalogErr := ReadEngineCatalogEntry(catalogFile, profileName)
	if catalogErr != nil {
		return sharedResolvedProfile{}, catalogErr
	}
	if state == EngineCatalogPresent {
		legacy, present, profileErr := readPiIdentity(project, homeDir, profileName)
		if profileErr != nil {
			return sharedResolvedProfile{}, profileErr
		}
		if present && !catalogPiIdentityAndArgvAgree(profileName, entry, legacy) {
			return sharedResolvedProfile{}, engineCatalogConflict("resolution.identity_argv", errors.New("catalog and legacy Pi profile disagree on model identity or executable/argv"))
		}
		resolved, err := resolveCatalogProfile(project, homeDir, cacheRoot, profileName, entry, catalogFile)
		if explicitCatalog {
			resolved.ConfigPath = catalogFile
		}
		return resolved, err
	}
	if explicitCatalog {
		return sharedResolvedProfile{}, engineCatalogReadFailed("catalog.selection", errors.New("selected catalog file or engine entry is absent"))
	}
	if profiles == nil {
		profiles, _, err = readPiProfiles(project, homeDir)
		if err != nil {
			return sharedResolvedProfile{}, err
		}
	}
	profile, ok := profiles[profileName]
	if !ok {
		return sharedResolvedProfile{}, sharedRuntimeError("unknown_pi_profile", fmt.Errorf("unknown Pi profile %q", profileName))
	}
	if profile.Runtime.Sharing == nil || profile.Runtime.Sharing.Mode != "shared" {
		return sharedResolvedProfile{}, sharedRuntimeError("shared_runtime_not_configured", fmt.Errorf("pi profile %q is not configured for shared runtime mode", profileName))
	}
	runtimeKey, profileDigest := SharedRuntimeKey(profile)
	paths, err := ResolveSharedRuntimePaths(cacheRoot, runtimeKey)
	if err != nil {
		return sharedResolvedProfile{}, err
	}
	return sharedResolvedProfile{
		Project: project, HomeDir: homeDir, ProfileName: profileName,
		EngineIdentity: SharedRuntimeEngineIdentity{Name: profileName, Key: runtimeKey},
		Profile:        profile, Sharing: *profile.Runtime.Sharing,
		RuntimeKey: runtimeKey, ProfileDigest: profileDigest, Paths: paths,
	}, nil
}

func readPiProfiles(project, homeDir string) (map[string]PiProfile, string, error) {
	globalConfig := filepath.Join(homeDir, ".agents", ".configs", projectConfigFileName)
	profiles := map[string]PiProfile{}
	selectedProfile := ""
	for _, ancestor := range ancestorDirsRootFirst(project) {
		path := filepath.Join(ancestor, ".agents", ".configs", projectConfigFileName)
		if samePath(path, globalConfig) {
			continue
		}
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, "", sharedRuntimeError("invalid_project_configuration", fmt.Errorf("read project config: %w", readErr))
		}
		var document map[string]any
		if parseErr := toml.Unmarshal(data, &document); parseErr != nil {
			return nil, "", sharedRuntimeError("invalid_project_configuration", fmt.Errorf("parse TOML: %w", parseErr))
		}
		agents, present, tableErr := projectConfigTable(document, "agents", "agents")
		if tableErr != nil {
			return nil, "", sharedRuntimeError("invalid_project_configuration", projectConfigFieldError(path, "agents", tableErr))
		}
		if !present {
			continue
		}
		primary, _, configured, parseErr := parsePiConfig(agents, path)
		if parseErr != nil {
			return nil, "", sharedRuntimeError("invalid_project_configuration", parseErr)
		}
		if primary.Profile != nil {
			selectedProfile = *primary.Profile
		}
		for name, profile := range configured {
			profile.Source = path
			profiles[name] = profile
		}
	}
	return profiles, selectedProfile, nil
}

type legacyPiIdentity struct {
	Model      string
	Executable string
	Argv       []string
}

func readPiIdentity(project, homeDir, name string) (legacyPiIdentity, bool, error) {
	globalConfig := filepath.Join(homeDir, ".agents", ".configs", projectConfigFileName)
	var selected legacyPiIdentity
	present := false
	for _, ancestor := range ancestorDirsRootFirst(project) {
		path := filepath.Join(ancestor, ".agents", ".configs", projectConfigFileName)
		if samePath(path, globalConfig) {
			continue
		}
		data, err := readLegacyPiIdentityConfigFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("read project config for compatibility check: %w", err))
		}
		var document map[string]any
		if err := toml.Unmarshal(data, &document); err != nil {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("parse project config for compatibility check: %w", err))
		}
		agents, agentsPresent, err := projectConfigTable(document, "agents", "agents")
		if err != nil || !agentsPresent {
			if err != nil {
				return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", err)
			}
			continue
		}
		pi, piPresent, err := projectConfigTable(agents, "pi", "agents.pi")
		if err != nil || !piPresent {
			if err != nil {
				return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", err)
			}
			continue
		}
		profiles, profilesPresent, err := projectConfigTable(pi, "profiles", "agents.pi.profiles")
		if err != nil || !profilesPresent {
			if err != nil {
				return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", err)
			}
			continue
		}
		raw, exists := profiles[name]
		if !exists {
			continue
		}
		profile, ok := raw.(map[string]any)
		if !ok {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s must be a table", name))
		}
		model, ok := profile["model"].(string)
		if !ok {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s.model must be a string", name))
		}
		if model == "" {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s.model must not be empty", name))
		}
		runtimeTable, runtimePresent, err := projectConfigTable(profile, "runtime", "agents.pi.profiles."+name+".runtime")
		if err != nil {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", err)
		}
		if !runtimePresent {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", errors.New("runtime table is absent"))
		}
		executable, ok := runtimeTable["executable"].(string)
		if !ok {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s.runtime.executable must be a string", name))
		}
		rawArgv, ok := runtimeTable["argv"].([]any)
		if !ok {
			return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s.runtime.argv must be an array", name))
		}
		argv := make([]string, len(rawArgv))
		for index, rawToken := range rawArgv {
			token, ok := rawToken.(string)
			if !ok {
				return legacyPiIdentity{}, false, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("agents.pi.profiles.%s.runtime.argv[%d] must be a string", name, index))
			}
			argv[index] = token
		}
		selected = legacyPiIdentity{Model: model, Executable: executable, Argv: argv}
		present = true
	}
	return selected, present, nil
}

func catalogPiIdentityAndArgvAgree(name string, entry EngineCatalogEntry, legacy legacyPiIdentity) bool {
	port := catalogEndpointPort(name, entry)
	model := entry.Model
	if model == "" {
		model, _ = engineCatalogServedModelID(entry)
	}
	argv := engineCatalogIdentityArgv(entry, port)
	executable := entry.Executable
	if entry.Mode == "ssh" {
		executable = entry.SSHExecutable
		if executable == "" {
			executable = "/usr/bin/ssh"
		}
	}
	return model == legacy.Model && executable == legacy.Executable && equalStrings(argv, legacy.Argv)
}

func CanonicalProjectDir(projectDir string) (string, error) {
	if strings.TrimSpace(projectDir) == "" {
		return "", errors.New("project directory is required")
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("resolve project directory %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("project path is not a directory")
		}
		return "", fmt.Errorf("stat project directory %s: %w", resolved, err)
	}
	return filepath.Clean(resolved), nil
}

func ancestorDirsRootFirst(startDir string) []string {
	dir := filepath.Clean(startDir)
	var cwdFirst []string
	for {
		cwdFirst = append(cwdFirst, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	rootFirst := make([]string, 0, len(cwdFirst))
	for i := len(cwdFirst) - 1; i >= 0; i-- {
		rootFirst = append(rootFirst, cwdFirst[i])
	}
	return rootFirst
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func scrubSharedRuntimeEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, item := range environ {
		name := item
		if index := indexByte(item, '='); index >= 0 {
			name = item[:index]
		}
		if len(name) >= 3 && name[:3] == "PI_" {
			continue
		}
		if name == "HF_ENDPOINT" || name == "MODEL_ENDPOINT" || name == "GGML_BACKEND_PATH" || name == "LLAMA_API_KEY" {
			continue
		}
		if hasAnyPrefixFold(name, "DYLD_", "LD_", "NODE_", "BUN_", "LLAMA_ARG_") {
			continue
		}
		result = append(result, item)
	}
	return result
}

func indexByte(value string, target byte) int {
	for i := range value {
		if value[i] == target {
			return i
		}
	}
	return -1
}

func hasAnyPrefixFold(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if len(value) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			left := value[i]
			if left >= 'a' && left <= 'z' {
				left -= 'a' - 'A'
			}
			if left != prefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
