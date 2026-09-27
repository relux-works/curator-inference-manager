//go:build !windows

package infra

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var piStateKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PiStatePaths retains the identity inputs carried by the v7 pi_shared hello.
// The extracted broker does not create or own the Pi client's state tree.
type PiStatePaths struct {
	CanonicalCacheRoot string `json:"canonical_cache_root"`
	ProjectStateKey    string `json:"project_state_key"`
	ProfileStateKey    string `json:"profile_state_key"`
	RunStateKey        string `json:"run_state_key,omitempty"`
	ProfileRoot        string `json:"profile_root"`
	LifecycleLogsRoot  string `json:"lifecycle_logs_root"`
	Root               string `json:"root"`
	AgentDir           string `json:"agent_dir"`
	SessionsDir        string `json:"sessions_dir"`
	LogsDir            string `json:"logs_dir"`
	ModelsJSON         string `json:"models_json"`
	SettingsJSON       string `json:"settings_json"`
	Lock               string `json:"lock"`
}

func exactStateKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

var piStateKey = exactStateKey

func ResolvePiClientStatePaths(cacheRoot, canonicalProject, profileName, runID string) (PiStatePaths, error) {
	if cacheRoot == "" {
		var err error
		cacheRoot, err = os.UserCacheDir()
		if err != nil {
			return PiStatePaths{}, err
		}
	}
	abs, err := filepath.Abs(cacheRoot)
	if err != nil {
		return PiStatePaths{}, err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return PiStatePaths{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("cache root is not a directory")
		}
		return PiStatePaths{}, err
	}
	projectKey, profileKey := piStateKey(canonicalProject), piStateKey(profileName)
	if !piStateKeyPattern.MatchString(projectKey) || !piStateKeyPattern.MatchString(profileKey) {
		return PiStatePaths{}, errors.New("state key is not lowercase SHA-256 hex")
	}
	components := []string{"agents-infra", "pi", projectKey, profileKey}
	runKey := ""
	lockName := "session.lock"
	if runID != "" {
		runKey = exactStateKey("agents-infra.pi.run.v1\x00" + runID)
		components = append(components, "runs", runKey)
		lockName = "client.lock"
	}
	root := filepath.Join(append([]string{canonical}, components...)...)
	profileRoot := filepath.Join(canonical, "agents-infra", "pi", projectKey, profileKey)
	return PiStatePaths{
		CanonicalCacheRoot: canonical,
		ProjectStateKey:    projectKey,
		ProfileStateKey:    profileKey,
		RunStateKey:        runKey,
		ProfileRoot:        profileRoot,
		LifecycleLogsRoot:  filepath.Join(profileRoot, "lifecycle-logs"),
		Root:               root,
		AgentDir:           filepath.Join(root, "agent"),
		SessionsDir:        filepath.Join(root, "sessions"),
		LogsDir:            filepath.Join(root, "logs"),
		ModelsJSON:         filepath.Join(root, "agent", "models.json"),
		SettingsJSON:       filepath.Join(root, "agent", "settings.json"),
		Lock:               filepath.Join(root, lockName),
	}, nil
}

func ResolvePiStatePaths(cacheRoot, canonicalProject, profileName string) (PiStatePaths, error) {
	return ResolvePiClientStatePaths(cacheRoot, canonicalProject, profileName, "")
}
