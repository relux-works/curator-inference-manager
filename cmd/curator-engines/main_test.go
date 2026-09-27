package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

const (
	realEngineExecutableEnv = "CURATOR_ENGINES_REAL_TEST_EXECUTABLE"
	realEngineModelEnv      = "CURATOR_ENGINES_REAL_TEST_MODEL"
	realEngineHelperEnv     = "CURATOR_ENGINES_REAL_TEST_CRASH_HELPER"
	realEngineLeasePathEnv  = "CURATOR_ENGINES_REAL_TEST_LEASE_PATH"
	realEngineProjectEnv    = "CURATOR_ENGINES_REAL_TEST_PROJECT"
	realEngineNameEnv       = "CURATOR_ENGINES_REAL_TEST_NAME"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "runtime" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestCuratorEnginesHelpUsesAdoptedLifecycleCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit=%d stderr=%q", code, stderr.String())
	}
	for _, command := range []string{"status", "ensure", "release", "stop", "--engine NAME", "--json"} {
		if !strings.Contains(stdout.String(), command) {
			t.Fatalf("public help omits %q: %s", command, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "runtime broker") {
		t.Fatalf("public help exposes the hidden broker command: %s", stdout.String())
	}
}

func TestCuratorEnginesReleaseRejectsMissingLeaseToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"release", "--engine", "fixture"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "release requires --lease-token") {
		t.Fatalf("release admitted a request without its bearer token: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCuratorEnginesStopTextOutputOmitsEmptyEngineName(t *testing.T) {
	got := formatStopTextOutput("", "stopped")
	if got != "Shared runtime is stopped." {
		t.Fatalf("stop output=%q, want a shared-runtime status without an empty engine name", got)
	}
	if strings.Contains(got, "Engine  ") {
		t.Fatalf("stop output contains an empty engine name: %q", got)
	}
}

func TestCuratorEnginesRealEngineLifecycle(t *testing.T) {
	executable := os.Getenv(realEngineExecutableEnv)
	model := os.Getenv(realEngineModelEnv)
	if executable == "" || model == "" {
		t.Skipf("set %s and %s to run the real-engine acceptance test", realEngineExecutableEnv, realEngineModelEnv)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatalf("real engine executable: %v", err)
	}
	if _, err := os.Stat(model); err != nil {
		t.Fatalf("real engine model: %v", err)
	}
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	portProbe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portProbe.Addr().(*net.TCPAddr).Port
	if err := portProbe.Close(); err != nil {
		t.Fatal(err)
	}
	engineName := "real-qwen"
	writeRealEngineProjectConfig(t, project, engineName, executable, model, port)
	t.Setenv("HOME", home)
	t.Chdir(project)

	status, _ := runStatus(t, []string{"status", "--engine", engineName, "--json"})
	if status.Broker.State != "absent" || len(status.Leases) != 0 {
		t.Fatalf("cold status found engine state: broker=%q leases=%d", status.Broker.State, len(status.Leases))
	}

	ensureContext, cancelEnsure := context.WithCancel(context.Background())
	ensureOutput := &lockedBuffer{}
	ensureError := &lockedBuffer{}
	ensureDone := make(chan int, 1)
	go func() {
		ensureDone <- runWithContext(ensureContext, []string{"ensure", "--engine", engineName, "--json"}, ensureOutput, ensureError)
	}()
	defer func() {
		cancelEnsure()
		select {
		case <-ensureDone:
		case <-time.After(5 * time.Second):
		}
	}()
	lease := waitForLeaseRecord(t, ensureOutput)
	if lease.EngineIdentity.Name != engineName || lease.LeaseID == "" || lease.LeaseToken == "" || lease.LeaseID == lease.LeaseToken || lease.LeaseLifetime != "client_process" {
		t.Fatalf("real ensure returned incomplete lease identity or lifetime: engine=%q id_present=%t token_present=%t lifetime=%q", lease.EngineIdentity.Name, lease.LeaseID != "", lease.LeaseToken != "", lease.LeaseLifetime)
	}
	status, rawStatus := runStatus(t, []string{"status", "--engine", engineName, "--json"})
	if status.Broker.State != "serving" || status.Runtime == nil || len(status.Leases) != 1 || status.EngineIdentity.Name != engineName {
		t.Fatalf("real status did not report one serving engine lease: broker=%q runtime=%t leases=%d identity=%q", status.Broker.State, status.Runtime != nil, len(status.Leases), status.EngineIdentity.Name)
	}
	if strings.Contains(rawStatus, "lease_token") || status.Leases[0].LeaseToken != "" {
		t.Fatal("real status disclosed a bearer lease token")
	}
	var refused struct {
		Code string `json:"code"`
	}
	stopOutput, stopError := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"stop", "--engine", engineName, "--json"}, stopOutput, stopError); code != 1 || json.Unmarshal(stopError.Bytes(), &refused) != nil || refused.Code != "shared_runtime_leases_active" {
		t.Fatalf("real stop did not refuse the busy engine: exit=%d stdout=%q stderr=%q", code, stopOutput.String(), stopError.String())
	}

	releaseOutput, releaseError := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"release", "--engine", engineName, "--lease-token", lease.LeaseToken, "--json"}, releaseOutput, releaseError); code != 0 {
		t.Fatalf("real token release failed: exit=%d stdout=%q stderr=%q", code, releaseOutput.String(), releaseError.String())
	}
	if code := waitForExit(t, ensureDone); code != 0 {
		t.Fatalf("ensure owner did not exit cleanly after real token release: exit=%d stderr=%q", code, ensureError.String())
	}
	waitForEngineAbsent(t, engineName)

	cancelContext, cancel := context.WithCancel(context.Background())
	cancelOutput := &lockedBuffer{}
	cancelError := &lockedBuffer{}
	cancelDone := make(chan int, 1)
	go func() {
		cancelDone <- runWithContext(cancelContext, []string{"ensure", "--engine", engineName, "--json"}, cancelOutput, cancelError)
	}()
	_ = waitForLeaseRecord(t, cancelOutput)
	cancel()
	if code := waitForExit(t, cancelDone); code != 0 {
		t.Fatalf("cancelled ensure exit=%d stderr=%q", code, cancelError.String())
	}
	waitForEngineAbsent(t, engineName)

	crashLeasePath := filepath.Join(t.TempDir(), "crash-lease.json")
	crash := exec.Command(os.Args[0], "-test.run=^TestCuratorEnginesRealEngineCrashHelper$", "-test.count=1")
	crash.Dir = project
	crash.Env = append(os.Environ(),
		realEngineHelperEnv+"=1", realEngineLeasePathEnv+"="+crashLeasePath,
		realEngineProjectEnv+"="+project, realEngineNameEnv+"="+engineName,
	)
	var crashStdout, crashStderr bytes.Buffer
	crash.Stdout, crash.Stderr = &crashStdout, &crashStderr
	if err := crash.Start(); err != nil {
		t.Fatal(err)
	}
	_ = waitForLeaseFile(t, crashLeasePath)
	if err := crash.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := crash.Wait(); err == nil {
		t.Fatal("crash helper exited successfully after SIGKILL")
	}
	waitForEngineAbsent(t, engineName)
}

func TestCuratorEnginesRealEngineCrashHelper(t *testing.T) {
	if os.Getenv(realEngineHelperEnv) != "1" {
		t.Skip("real-engine crash subprocess helper")
	}
	leaseFile, err := os.Create(os.Getenv(realEngineLeasePathEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leaseFile.Close() })
	code := run([]string{"ensure", "--engine", os.Getenv(realEngineNameEnv), "--json"}, leaseFile, os.Stderr)
	if code != 0 {
		t.Fatalf("ensure helper exit=%d", code)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.String()
}

func runStatus(t *testing.T, args []string) (infra.SharedRuntimeStatus, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("%s exit=%d stdout=%q stderr=%q", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	var status infra.SharedRuntimeStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode real status JSON: %v", err)
	}
	return status, stdout.String()
}

func waitForLeaseRecord(t *testing.T, output *lockedBuffer) infra.SharedLeaseRecord {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		var record infra.SharedLeaseRecord
		if json.Unmarshal([]byte(output.String()), &record) == nil && record.LeaseToken != "" {
			return record
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ensure did not write a lease record before deadline; stderr=%s", output.String())
	return infra.SharedLeaseRecord{}
}

func waitForLeaseFile(t *testing.T, path string) infra.SharedLeaseRecord {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		var record infra.SharedLeaseRecord
		if err == nil && json.Unmarshal(data, &record) == nil && record.LeaseToken != "" {
			return record
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("crash helper did not write a lease record before deadline")
	return infra.SharedLeaseRecord{}
}

func waitForExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(15 * time.Second):
		t.Fatal("ensure command did not exit after release or cancellation")
		return -1
	}
}

func waitForEngineAbsent(t *testing.T, engineName string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"status", "--engine", engineName, "--json"}, &stdout, &stderr)
		var status infra.SharedRuntimeStatus
		if code == 0 && json.Unmarshal(stdout.Bytes(), &status) == nil && status.Broker.State == "absent" && len(status.Leases) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("engine broker retained runtime or lease state after owner cleanup")
}

func writeRealEngineProjectConfig(t *testing.T, project, name, executable, model string, port int) {
	t.Helper()
	argv := []string{"-m", model, "--alias", name, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-c", "4096", "--no-webui"}
	quotedArgv := make([]string, len(argv))
	for index, argument := range argv {
		quotedArgv[index] = strconv.Quote(argument)
	}
	profile := strconv.Quote(name)
	config := fmt.Sprintf(`[agents.pi.primary_session]
profile = %s
pi_compatibility = "github-release:earendil-works/pi@v0.84.2:darwin-arm64#sha256-c996e888b7f7dce44bcf24f69176ac646c44139d3916bd49a6b28e5a8c5e3a65"

[agents.pi.profiles.%s]
provider = "local-provider"
publisher = "local-cache"
family = "llama.cpp"
model = %s
base_url = "http://127.0.0.1:%d/v1"
api = "openai-completions"
reasoning = false
input = ["text"]
context_window = 4096
max_tokens = 256
thinking = "off"
requested_capabilities = ["text", "tools"]

[agents.pi.profiles.%s.lifecycle_log_retention]
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

[agents.pi.profiles.%s.compat]
supports_developer_role = false
supports_reasoning_effort = false
supports_usage_in_streaming = true
supports_finish_reason = true
max_tokens_field = "max_tokens"

[agents.pi.profiles.%s.runtime]
executable = %s
argv = [%s]
readiness_path = "/models"
startup_timeout_seconds = 120
shutdown_timeout_seconds = 10

[agents.pi.profiles.%s.runtime.sharing]
mode = "shared"
linger_seconds = 0
max_leases = 2
max_segment_bytes = 1048576
max_segments = 7
heartbeat_interval_seconds = 1
lease_stale_seconds = 5
restart_limit = 3
restart_initial_backoff_seconds = 1
restart_max_backoff_seconds = 4
stable_run_seconds = 2
quarantine_seconds = 30
broker_start_timeout_seconds = 180
resource_pressure_mode = "disabled"
`, profile, profile, profile, port, profile, profile, profile, strconv.Quote(executable), strings.Join(quotedArgv, ", "), profile)
	configPath := filepath.Join(project, ".agents", ".configs", "project-config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}
