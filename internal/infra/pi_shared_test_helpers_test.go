//go:build !windows

package infra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

const PiCompatibilityV0842DarwinARM64 = "github-release:earendil-works/pi@v0.84.2:darwin-arm64#sha256-c996e888b7f7dce44bcf24f69176ac646c44139d3916bd49a6b28e5a8c5e3a65"

func validPiProfileTOML(name, runtime string, port int, dflash bool) string {
	extraCaps := `requested_capabilities = ["text", "tools"]`
	dflashBlock := ""
	argv := fmt.Sprintf(`["serve", "--model", "Model", "--host", "127.0.0.1", "--port", "%d"]`, port)
	if dflash {
		extraCaps = `requested_capabilities = ["dflash", "text", "tools"]`
		argv = fmt.Sprintf(`["serve", "--model", "Model", "--draft", "Draft", "--host", "127.0.0.1", "--port", "%d"]`, port)
		dflashBlock = fmt.Sprintf(`
[agents.pi.profiles.%q.runtime.dflash]
target_model = "Model"
draft_model = "Draft"
target_argv = ["--model", "Model"]
draft_argv = ["--draft", "Draft"]
`, name)
	}
	return fmt.Sprintf(`
[agents.pi.primary_session]
profile = %q
pi_compatibility = %q

[agents.pi.profiles.%q]
provider = "local-provider"
publisher = "fixture-publisher"
family = "fixture-family"
model = "Model"
base_url = "http://127.0.0.1:%d/v1"
api = "openai-completions"
reasoning = false
input = ["text"]
context_window = 8192
max_tokens = 1024
thinking = "off"
%s

[agents.pi.profiles.%q.lifecycle_log_retention]
max_count = 8
max_bytes = 1048576
max_envelope_bytes = 2097152
max_age_seconds = 4838400
create_timeout_seconds = 5
append_timeout_seconds = 5
close_timeout_seconds = 5
status_timeout_seconds = 5
maintenance_timeout_seconds = 5
max_scan_entries = 512
max_scan_control_bytes = 262144
max_mutations_per_operation = 8

[agents.pi.profiles.%q.compat]
supports_developer_role = false
supports_reasoning_effort = false
supports_usage_in_streaming = true
supports_finish_reason = true
max_tokens_field = "max_tokens"

[agents.pi.profiles.%q.runtime]
executable = %q
argv = %s
readiness_path = "/models"
startup_timeout_seconds = 5
shutdown_timeout_seconds = 2
%s`, name, PiCompatibilityV0842DarwinARM64, name, port, extraCaps, name, name, name, runtime, argv, dflashBlock)
}

func sharedPiProfileTOML(name, runtime string, port int) string {
	return validPiProfileTOML(name, runtime, port, false) + fmt.Sprintf(`
[agents.pi.profiles.%q.runtime.sharing]
mode = "shared"
linger_seconds = 0
max_leases = 8
max_segment_bytes = 1048576
max_segments = 7
heartbeat_interval_seconds = 15
lease_stale_seconds = 60
restart_limit = 3
restart_initial_backoff_seconds = 1
restart_max_backoff_seconds = 4
stable_run_seconds = 10
quarantine_seconds = 30
broker_start_timeout_seconds = 40
resource_pressure_mode = "disabled"
`, name)
}

func validPiProfileWithArgv(t *testing.T, name, runtime string, port int, argv []string, startupTimeout int) string {
	t.Helper()
	base := validPiProfileTOML(name, runtime, port, false)
	defaultArgv := fmt.Sprintf(`["serve", "--model", "Model", "--host", "127.0.0.1", "--port", "%d"]`, port)
	effectiveArgv := append([]string(nil), argv...)
	contains := func(want string) bool {
		for _, token := range effectiveArgv {
			if token == want {
				return true
			}
		}
		return false
	}
	if !contains("--host") {
		effectiveArgv = append(effectiveArgv, "--host", "127.0.0.1")
	}
	if !contains("--port") {
		effectiveArgv = append(effectiveArgv, "--port", strconv.Itoa(port))
	}
	encoded := make([]string, len(effectiveArgv))
	for index, token := range effectiveArgv {
		encoded[index] = strconv.Quote(token)
	}
	base = strings.Replace(base, defaultArgv, "["+strings.Join(encoded, ", ")+"]", 1)
	base = strings.Replace(base, "startup_timeout_seconds = 5", fmt.Sprintf("startup_timeout_seconds = %d", startupTimeout), 1)
	return base
}

func writePiProjectConfig(t *testing.T, project, body string) {
	t.Helper()
	path := filepath.Join(project, ".agents", ".configs", projectConfigFileName)
	mustMkdir(t, filepath.Dir(path))
	mustWrite(t, path, body)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustParsedPiProfile(t *testing.T, dflash bool) PiProfile {
	t.Helper()
	var document map[string]any
	if err := toml.Unmarshal([]byte(validPiProfileTOML("profile", "/bin/echo", 18011, dflash)), &document); err != nil {
		t.Fatal(err)
	}
	agents, present, err := projectConfigTable(document, "agents", "agents")
	if err != nil || !present {
		t.Fatalf("fixture lacks agents table: present=%t err=%v", present, err)
	}
	_, _, profiles, err := parsePiConfig(agents, "config")
	if err != nil {
		t.Fatal(err)
	}
	return profiles["profile"]
}

func piErrorCode(err error) string {
	var launch *PiLaunchError
	if errors.As(err, &launch) {
		return launch.Code
	}
	return ""
}

func CreatePiStateTree(state PiStatePaths) error {
	for _, directory := range []string{state.Root, state.AgentDir, state.SessionsDir, state.LogsDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}

type PiProfileLock struct{ file *os.File }

func AcquirePiProfileLock(state PiStatePaths) (*PiProfileLock, error) {
	file, err := os.OpenFile(state.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &PiProfileLock{file: file}, nil
}

func (lock *PiProfileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	return lock.file.Close()
}
