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

type sharedResolvedProfile struct {
	Project        string
	HomeDir        string
	ProfileName    string
	EngineIdentity SharedRuntimeEngineIdentity
	Profile        PiProfile
	Sharing        PiRuntimeSharing
	RuntimeKey     string
	ProfileDigest  string
	Paths          SharedRuntimePaths
}

func resolveSharedProfile(projectDir, homeDir, cacheRoot, profileName string) (sharedResolvedProfile, error) {
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
			return sharedResolvedProfile{}, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("read project config: %w", readErr))
		}
		var document map[string]any
		if parseErr := toml.Unmarshal(data, &document); parseErr != nil {
			return sharedResolvedProfile{}, sharedRuntimeError("invalid_project_configuration", fmt.Errorf("parse TOML: %w", parseErr))
		}
		agents, present, tableErr := projectConfigTable(document, "agents", "agents")
		if tableErr != nil {
			return sharedResolvedProfile{}, sharedRuntimeError("invalid_project_configuration", projectConfigFieldError(path, "agents", tableErr))
		}
		if !present {
			continue
		}
		primary, _, configured, parseErr := parsePiConfig(agents, path)
		if parseErr != nil {
			return sharedResolvedProfile{}, sharedRuntimeError("invalid_project_configuration", parseErr)
		}
		if primary.Profile != nil {
			selectedProfile = *primary.Profile
		}
		for name, profile := range configured {
			profile.Source = path
			profiles[name] = profile
		}
	}
	if profileName == "" {
		profileName = selectedProfile
	}
	if profileName == "" {
		return sharedResolvedProfile{}, sharedRuntimeError("shared_runtime_not_configured", errors.New("no managed Pi profile is selected"))
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
		Project:        project,
		HomeDir:        homeDir,
		ProfileName:    profileName,
		EngineIdentity: SharedRuntimeEngineIdentity{Name: profileName, Key: runtimeKey},
		Profile:        profile,
		Sharing:        *profile.Runtime.Sharing,
		RuntimeKey:     runtimeKey,
		ProfileDigest:  profileDigest,
		Paths:          paths,
	}, nil
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
