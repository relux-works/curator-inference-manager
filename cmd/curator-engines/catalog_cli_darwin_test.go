//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func TestCuratorEnginesStatusReportsAvailabilityUnknownWithoutChangingKnownBrokerAbsence(t *testing.T) {
	status := runCatalogEngineStatus(t, "/missing/mlx_lm.server", "/models/qwen-not-installed")
	if status.EngineIdentity.Name != "qwen-local" || status.EngineAvailability != "unknown" || status.Broker.State != "absent" {
		t.Fatalf("unavailable engine inference changed known broker absence or lost unknown availability: %#v", status)
	}
}

func TestCuratorEnginesMissingServedIDReportsUnknownAndEnsureRefusesBeforeBrokerStart(t *testing.T) {
	cache := prepareCatalogCLIEntry(t, "missing-id", infra.EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "org/qwen-model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	})
	status, _ := runStatus(t, []string{"status", "--engine", "missing-id", "--json"})
	if status.EngineAvailability != "unknown" || status.EngineAvailabilityReason != "served_model_id_missing" || status.Broker.State != "absent" {
		t.Fatalf("missing served id was not represented as a non-runnable unknown engine: %#v", status)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"ensure", "--engine", "missing-id", "--json"}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), `"code":"engine_not_runnable"`) || !strings.Contains(stderr.String(), "served_model_id_missing") {
		t.Fatalf("ensure did not refuse the entry with its named reason: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cache, "curator-engines", "shared-runtimes-v1")); !os.IsNotExist(err) {
		t.Fatalf("non-runnable ensure created broker state: %v", err)
	}
}

func TestCuratorEnginesShebangExecutableReportsNonRunnableAndIsNotLaunched(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "engine-script")
	marker := filepath.Join(root, "script-ran")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cache := prepareCatalogCLIEntry(t, "script-engine", infra.EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "org/qwen-model", ServedModelID: "qwen-served",
		Executable: script, Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	})
	status, _ := runStatus(t, []string{"status", "--engine", "script-engine", "--json"})
	if status.EngineAvailability != "unknown" || status.EngineAvailabilityReason != "script_executable_unsupported" || status.Broker.State != "absent" {
		t.Fatalf("shebang executable was not reported as non-runnable unknown: %#v", status)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"ensure", "--engine", "script-engine", "--json"}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), `"code":"engine_not_runnable"`) || !strings.Contains(stderr.String(), "script_executable_unsupported") {
		t.Fatalf("ensure did not refuse a shebang executable with its named reason: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shebang executable was launched by curator-engines: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "curator-engines", "shared-runtimes-v1")); !os.IsNotExist(err) {
		t.Fatalf("non-runnable ensure created broker state: %v", err)
	}
}

func prepareCatalogCLIEntry(t *testing.T, name string, entry infra.EngineCatalogEntry) string {
	t.Helper()
	project, err := os.MkdirTemp("/tmp", "p-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	home, err := os.MkdirTemp("/tmp", "ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	cache := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := infra.MarshalEngineCatalog(infra.EngineCatalogDocument{
		SchemaVersion: infra.EngineCatalogSchemaVersion,
		Engines:       map[string]infra.EngineCatalogEntry{name: entry},
	})
	if err != nil {
		t.Fatalf("encode catalog test entry: %v", err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)
	return cache
}

func TestCuratorEnginesStatusRefusesCommaServedModelIDAtValidation(t *testing.T) {
	project, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	home, err := os.MkdirTemp("/tmp", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := "schema_version = 1\n" +
		"[engines.qwen-local]\nmode = \"local\"\nengine = \"llama-cpp\"\n" +
		"served_model_id = \"qwen-a,qwen-b\"\nexecutable = \"/bin/echo\"\n" +
		"argv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n"
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--engine", "qwen-local", "--json"}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), `"code":"engine_catalog_malformed"`) ||
		!strings.Contains(stderr.String(), `"site":"entry.served_model_id"`) {
		t.Fatalf("production status did not refuse comma-bearing served_model_id before launch: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCuratorEnginesReadinessRequiresDeclaredServedModelIDMembership(t *testing.T) {
	fakeRuntime := buildCatalogFakeRuntime(t)
	t.Setenv("CATALOG_FAKE_MODEL_IDS", "unrelated-model")
	modelsRequest := filepath.Join(t.TempDir(), "models-requested")
	t.Setenv("CATALOG_FAKE_MODELS_REQUEST_FILE", modelsRequest)
	cache := prepareCatalogCLIEntry(t, "membership-check", infra.EngineCatalogEntry{
		Mode: "local", Engine: "llama-cpp", ServedModelID: "requested-served-model",
		Executable: fakeRuntime,
		Argv:       []string{"serve", "--host", "{host}", "--port", "{port}"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := runWithContext(ctx, []string{"ensure", "--engine", "membership-check", "--json"}, &stdout, &stderr)
	if code != 77 || !strings.Contains(stderr.String(), "shared_runtime_quarantined") {
		t.Fatalf("ensure did not refuse readiness without declared model membership: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), `"lease_token"`) {
		t.Fatalf("ensure granted a lease without declared model membership: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(modelsRequest); err != nil {
		t.Fatalf("fake engine did not receive the production readiness model-list request: %v", err)
	}
	if diagnostic := catalogBrokerDiagnostic(cache); diagnostic != "" && !strings.Contains(diagnostic, "runtime_model_unavailable") {
		t.Fatalf("broker quarantine had an unrelated cause: %s", diagnostic)
	}
}

func TestCuratorEnginesStatusKeepsKnownAbsentStateForModelIdentifier(t *testing.T) {
	status := runCatalogEngineStatus(t, "/bin/echo", "Qwen3-local")
	if status.EngineIdentity.Name != "qwen-local" || status.EngineAvailability != "unknown" || status.Broker.State != "absent" {
		t.Fatalf("known-cold catalog engine did not retain absent broker state: %#v", status)
	}
}

func TestCuratorEnginesSSHStatusKeepsKnownAbsentState(t *testing.T) {
	project, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	home, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := "schema_version = 1\n" +
		"[engines.remote-qwen]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nssh_executable = \"/usr/bin/ssh\"\n" +
		"served_model_id = \"remote-served-model-id\"\n" +
		"ssh_target = \"model-host\"\nremote_executable = \"/opt/model-harness\"\n" +
		"remote_profile = \"qwen-mlx\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n"
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--engine", "remote-qwen", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("SSH catalog status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var status infra.SharedRuntimeStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode SSH status: %v; stdout=%q", err, stdout.String())
	}
	if status.Broker.State != "absent" || status.EngineAvailability != "unknown" {
		t.Fatalf("SSH mode did not retain broker absence and unknown remote availability: %#v", status)
	}
}

func TestCuratorEnginesCatalogIdentifierLifecycleThroughRun(t *testing.T) {
	const engineName = "qwen-local"
	const modelID = "Qwen3.8-27B-mlx-8bit"
	project, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	home, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	executable := buildCatalogFakeRuntime(t)
	catalog := "schema_version = 1\n" +
		"[engines." + engineName + "]\nmode = \"local\"\nengine = \"mlx-lm\"\n" +
		"model = " + strconv.Quote(modelID) + "\nserved_model_id = " + strconv.Quote(modelID) + "\nexecutable = " + strconv.Quote(executable) + "\n" +
		"argv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n"
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)

	status, _ := runStatus(t, []string{"status", "--engine", engineName, "--json"})
	if status.Broker.State != "absent" || status.EngineIdentity.Name != engineName || status.EngineAvailability != "unknown" {
		t.Fatalf("catalog identifier did not report known-cold state: broker=%#v identity=%#v", status.Broker, status.EngineIdentity)
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
	lease := waitForCatalogLeaseRecord(t, ensureOutput, ensureError, ensureDone, cancelEnsure,
		filepath.Join(home, "Library", "Caches"))
	if lease.EngineIdentity.Name != engineName || lease.LeaseToken == "" {
		t.Fatalf("catalog ensure did not return a live lease: %#v", lease)
	}
	var servingStatusOutput, servingStatusError bytes.Buffer
	if code := run([]string{"status", "--engine", engineName, "--json"}, &servingStatusOutput, &servingStatusError); code != 0 {
		t.Fatalf("serving catalog status exit=%d stdout=%q stderr=%q", code, servingStatusOutput.String(), servingStatusError.String())
	}
	if err := json.Unmarshal(servingStatusOutput.Bytes(), &status); err != nil {
		t.Fatalf("decode serving catalog status: %v; stdout=%q", err, servingStatusOutput.String())
	}
	if status.Broker.State != "serving" || status.EngineAvailability != "available" || status.Runtime == nil || len(status.Leases) != 1 {
		t.Fatalf("catalog status did not observe the real serving process: %#v", status)
	}

	var releaseOut, releaseErr bytes.Buffer
	if code := run([]string{"release", "--engine", engineName, "--lease-token", lease.LeaseToken, "--json"}, &releaseOut, &releaseErr); code != 0 {
		t.Fatalf("catalog release exit=%d stdout=%q stderr=%q", code, releaseOut.String(), releaseErr.String())
	}
	if code := waitForExit(t, ensureDone); code != 0 {
		t.Fatalf("catalog ensure owner exit=%d stderr=%q", code, ensureError.String())
	}
	waitForEngineAbsent(t, engineName)
	var stopOut, stopErr bytes.Buffer
	if code := run([]string{"stop", "--engine", engineName, "--force", "--json"}, &stopOut, &stopErr); code != 0 {
		t.Fatalf("catalog stop exit=%d stdout=%q stderr=%q", code, stopOut.String(), stopErr.String())
	}
	if strings.Contains(stopOut.String(), modelID) || strings.Contains(stopErr.String(), modelID) {
		t.Fatal("catalog lifecycle output unexpectedly expanded or leaked the model identifier")
	}
}

func waitForCatalogLeaseRecord(t *testing.T, output, stderr *lockedBuffer, done chan int, cancel context.CancelFunc, cacheRoot string) infra.SharedLeaseRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var record infra.SharedLeaseRecord
		if json.Unmarshal([]byte(output.String()), &record) == nil && record.LeaseToken != "" {
			return record
		}
		select {
		case code := <-done:
			done <- code
			t.Fatalf("ensure exited before a lease record: code=%d stdout=%q stderr=%q", code, output.String(), stderr.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		done <- code
		diagnostic := catalogBrokerDiagnostic(cacheRoot)
		stopCatalogTestBrokers(cacheRoot)
		t.Fatalf("ensure did not acquire a lease within 10s: code=%d stdout=%q stderr=%q broker_files=%s", code, output.String(), stderr.String(), diagnostic)
	case <-time.After(5 * time.Second):
		diagnostic := catalogBrokerDiagnostic(cacheRoot)
		stopCatalogTestBrokers(cacheRoot)
		t.Fatalf("ensure did not acquire a lease within 10s or stop after cancellation: stdout=%q stderr=%q broker_files=%s", output.String(), stderr.String(), diagnostic)
	}
	return infra.SharedLeaseRecord{}
}

func catalogBrokerDiagnostic(cacheRoot string) string {
	paths, _ := filepath.Glob(filepath.Join(cacheRoot, "curator-engines", "shared-runtimes-v1", "*", "broker-state.json"))
	var result strings.Builder
	for _, path := range paths {
		for _, candidate := range []string{path, strings.TrimSuffix(path, "broker-state.json") + "broker.log", strings.TrimSuffix(path, "broker-state.json") + "runtime.log"} {
			data, err := os.ReadFile(candidate)
			if err == nil {
				result.WriteString(candidate)
				result.WriteString(": ")
				result.Write(data)
				result.WriteByte('\n')
			}
		}
	}
	return result.String()
}

func stopCatalogTestBrokers(cacheRoot string) {
	paths, _ := filepath.Glob(filepath.Join(cacheRoot, "curator-engines", "shared-runtimes-v1", "*", "broker-state.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record infra.SharedBrokerRecord
		if json.Unmarshal(data, &record) == nil && record.Broker.PID > 0 && record.Broker.PGID == record.Broker.PID {
			_ = syscall.Kill(-record.Broker.PGID, syscall.SIGTERM)
		}
	}
}

func TestCuratorEnginesRealLegacyFixturesReachReadinessUsingServedModelID(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate real legacy fixtures")
	}
	fixturePath := filepath.Join(filepath.Dir(source), "..", "..", "internal", "infra", "testdata", "legacy-model-harness-fixtures.toml")
	if !filepath.IsAbs(source) {
		fixturePath = filepath.Join("..", "..", "internal", "infra", "testdata", "legacy-model-harness-fixtures.toml")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read real legacy fixture: %v", err)
	}
	var fixture struct {
		Profiles map[string]infra.EngineCatalogEntry `toml:"profiles"`
	}
	if err := toml.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode real legacy fixture: %v", err)
	}
	fakeRuntime := buildCatalogFakeRuntime(t)
	cases := []struct {
		profile string
		engine  string
	}{
		{profile: "qwen-benchmark-python", engine: "mlx-lm"},
		{profile: "qwen-default-engine", engine: "mlx-lm"},
		{profile: "mlx-swift-knobs", engine: "mlx-swift"},
		{profile: "llama-cpp-knobs", engine: "llama-cpp"},
	}
	for _, testCase := range cases {
		t.Run(testCase.profile, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			if _, err := exec.LookPath("agents-infra"); err == nil {
				t.Fatal("catalog lifecycle unexpectedly found agents-infra on PATH")
			}
			entry, ok := fixture.Profiles[testCase.profile]
			if !ok {
				t.Fatalf("legacy fixture lacks %q", testCase.profile)
			}
			entry.Engine = testCase.engine
			entry.Executable = fakeRuntime
			servedModel := legacyFixtureServedModelID(entry)
			if servedModel == "" {
				t.Fatal("real fixture does not name a served model in model, --model, or --alias")
			}
			entry.ServedModelID = servedModel
			runCatalogFixtureLifecycle(t, testCase.profile, entry)
		})
	}
}

func TestCuratorEnginesSSHReadinessRequiresAndChecksExplicitServedID(t *testing.T) {
	entry := infra.EngineCatalogEntry{
		Mode: "ssh", Engine: "mlx-lm", SSHExecutable: buildCatalogFakeRuntime(t), SSHTarget: "remote-test-host",
		RemoteExecutable: "/opt/model-harness", RemoteProfile: "remote-profile-name",
		RemoteHost: "127.0.0.1", RemotePort: 18012, ServedModelID: "remote-served-model-id",
	}
	t.Setenv("CATALOG_FAKE_SERVED_MODEL_ID", "remote-served-model-id")
	t.Setenv("CATALOG_FAKE_ADDITIONAL_MODEL_IDS", "other-remote-model,another-model")
	runCatalogFixtureLifecycle(t, "remote-qwen", entry)
}

func TestCuratorEnginesReadinessPinsResolvedAliasForShortLegacyLlamaFlags(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate real legacy model-harness fixture")
	}
	fixturePath := filepath.Join(filepath.Dir(source), "..", "..", "internal", "infra", "testdata", "legacy-model-harness-fixtures.toml")
	if !filepath.IsAbs(source) {
		fixturePath = filepath.Join("..", "..", "internal", "infra", "testdata", "legacy-model-harness-fixtures.toml")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profiles map[string]infra.EngineCatalogEntry `toml:"profiles"`
	}
	if err := toml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	entry := fixture.Profiles["qwen-benchmark-llamacpp"]
	if entry.Executable == "" {
		t.Fatal("real llama-server fixture was not decoded")
	}
	entry.Engine = "llama-cpp"
	modelID := legacyFixtureServedModelID(entry)
	if modelID == "" {
		t.Fatal("real llama-server fixture did not contain a model id")
	}
	modelArg := ""
	for index, argument := range entry.Argv {
		if argument == "--model" || argument == "-m" {
			if index+1 < len(entry.Argv) {
				modelArg = entry.Argv[index+1]
			}
			break
		}
		if strings.HasPrefix(argument, "--model=") || strings.HasPrefix(argument, "-m=") {
			modelArg = strings.SplitN(argument, "=", 2)[1]
			break
		}
	}
	if modelArg == "" {
		t.Fatal("real llama-server fixture did not contain its model argv value")
	}
	entry.Executable = buildCatalogFakeRuntime(t)
	entry.ServedModelID = "resolved-served-alias"
	for index, argument := range entry.Argv {
		if argument == "--model" {
			entry.Argv[index] = "-m"
		}
	}
	entry.Argv = append(entry.Argv[:len(entry.Argv)-2], append([]string{"-a", "stale-legacy-alias"}, entry.Argv[len(entry.Argv)-2:]...)...)
	argvLog := filepath.Join(t.TempDir(), "runtime-argv.txt")
	t.Setenv("CATALOG_FAKE_ARGV_FILE", argvLog)
	t.Setenv("CATALOG_FAKE_ADDITIONAL_MODEL_IDS", modelID+",stale-legacy-alias,alternate-model")
	runCatalogFixtureLifecycle(t, "llama-short-flags", entry)
	launchedArgv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read launched fake-runtime argv: %v", err)
	}
	actual := strings.Split(strings.TrimSpace(string(launchedArgv)), "\n")
	if !containsStringPair(actual, "-m", modelArg) || !containsStringPair(actual, "--alias", entry.ServedModelID) || containsStringPair(actual, "-a", "stale-legacy-alias") {
		t.Fatalf("catalog launcher did not replace legacy -a while preserving -m: %q", actual)
	}
}

func TestGeneratedLegacyLlamaArgvReachesReadinessWithDeclaredServedID(t *testing.T) {
	const seed int64 = 0x2609299a
	const iterations = 32
	rng := rand.New(rand.NewSource(seed))
	fakeRuntime := buildCatalogFakeRuntime(t)
	t.Setenv("CATALOG_FAKE_SERVED_MODEL_ID", "")
	t.Setenv("CATALOG_FAKE_ADDITIONAL_MODEL_IDS", "another-served-id,unrelated-model")
	for iteration := 0; iteration < iterations; iteration++ {
		iteration := iteration
		t.Run(fmt.Sprintf("seed-%x/case-%02d", seed, iteration), func(t *testing.T) {
			servedID := fmt.Sprintf("served-model-%03d", iteration)
			model := ""
			if iteration%3 == 0 {
				model = fmt.Sprintf("/opt/fixture/models/model-%03d.gguf", iteration)
			}
			entry := infra.EngineCatalogEntry{
				Mode: "local", Engine: "llama-cpp", ServedModelID: servedID,
				Model: model, Executable: fakeRuntime,
				Argv: generatedLegacyReadinessArgv(rng, iteration, model == ""),
			}
			argvLog := filepath.Join(t.TempDir(), "runtime-argv.txt")
			t.Setenv("CATALOG_FAKE_ARGV_FILE", argvLog)
			engineName := fmt.Sprintf("readiness-%03d", iteration)
			runCatalogFixtureLifecycle(t, engineName, entry)

			launchedBytes, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("read launched fake-runtime argv: %v", err)
			}
			launched := strings.Split(strings.TrimSuffix(string(launchedBytes), "\n"), "\n")
			port := 0
			for index := 0; index+1 < len(launched); index++ {
				if launched[index] == "--port" {
					port, _ = strconv.Atoi(launched[index+1])
					break
				}
			}
			if port == 0 {
				t.Fatalf("seed=%#x iteration=%d launch omitted endpoint port: %q", seed, iteration, launched)
			}
			want := expectedLegacyReadinessArgv(entry, port)
			if !reflect.DeepEqual(launched, want) {
				t.Fatalf("seed=%#x iteration=%d launcher changed non-alias legacy argv:\n got: %q\nwant: %q", seed, iteration, launched, want)
			}
		})
	}
}

func TestGeneratedLegacyArgvForEveryCatalogEngineKindReachesReadiness(t *testing.T) {
	const seed int64 = 0x26093006
	const iterations = 48
	fakeRuntime := buildCatalogFakeRuntime(t)
	t.Setenv("CATALOG_FAKE_ADDITIONAL_MODEL_IDS", "another-served-id,unrelated-model")
	engines := []string{"llama-cpp", "mlx-lm", "mlx-swift"}
	for iteration := 0; iteration < iterations; iteration++ {
		iteration := iteration
		engine := engines[iteration%len(engines)]
		t.Run(fmt.Sprintf("seed-%x/%s/case-%02d", seed, engine, iteration), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed + int64(iteration)))
			servedID := fmt.Sprintf("declared-served-%03d", iteration)
			t.Setenv("CATALOG_FAKE_SERVED_MODEL_ID", "")
			model := ""
			if iteration%2 == 0 {
				model = fmt.Sprintf("/opt/fixture/models/model-%03d.gguf", iteration)
			}
			argv := generatedLegacyReadinessArgv(rng, iteration, model == "")
			if engine == "mlx-swift" {
				if iteration%2 == 0 {
					argv = insertBeforeCatalogEndpoint(argv, "--model-id", "legacy-id,second", "--model-id=stale-id")
				} else {
					argv = insertBeforeCatalogEndpoint(argv, "--model-id=legacy-id", "--model-id", "other-id")
				}
			}
			if engine == "mlx-lm" {
				t.Setenv("CATALOG_FAKE_SERVED_MODEL_ID", servedID)
			}
			entry := infra.EngineCatalogEntry{
				Mode: "local", Engine: engine, ServedModelID: servedID,
				Model: model, Executable: fakeRuntime, Argv: argv,
			}
			t.Logf("generated input engine=%s served_model_id=%s model=%q argv=%q", entry.Engine, entry.ServedModelID, entry.Model, entry.Argv)
			argvLog := filepath.Join(t.TempDir(), "runtime-argv.txt")
			t.Setenv("CATALOG_FAKE_ARGV_FILE", argvLog)
			engineName := fmt.Sprintf("generated-%s-%03d", strings.ReplaceAll(engine, "-", ""), iteration)
			runCatalogFixtureLifecycle(t, engineName, entry)

			launchedBytes, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("read launched fake-runtime argv: %v", err)
			}
			launched := strings.Split(strings.TrimSuffix(string(launchedBytes), "\n"), "\n")
			port := 0
			for index := 0; index+1 < len(launched); index++ {
				if launched[index] == "--port" {
					port, _ = strconv.Atoi(launched[index+1])
					break
				}
			}
			if port == 0 {
				t.Fatalf("seed=%#x engine=%s iteration=%d launch omitted endpoint port: %q", seed, engine, iteration, launched)
			}
			want := expectedLegacyReadinessArgv(entry, port)
			if !reflect.DeepEqual(launched, want) {
				t.Fatalf("seed=%#x engine=%s iteration=%d launcher changed non-identity legacy argv:\n got: %q\nwant: %q", seed, engine, iteration, launched, want)
			}
		})
	}
}

func insertBeforeCatalogEndpoint(argv []string, tokens ...string) []string {
	if len(argv) < 4 {
		return append([]string(nil), tokens...)
	}
	result := append([]string(nil), argv[:len(argv)-4]...)
	result = append(result, tokens...)
	return append(result, argv[len(argv)-4:]...)
}

func generatedLegacyReadinessArgv(rng *rand.Rand, iteration int, noModelField bool) []string {
	modelValues := []string{
		"/models/gguf-model,variant.gguf", "org/hf-model", "hf://org/repo", "legacy-model-id",
	}
	model := modelValues[rng.Intn(len(modelValues))]
	argv := []string{"serve"}
	switch iteration % 8 {
	case 0:
		argv = append(argv, "-m", model, "-m", "second-model")
	case 1:
		argv = append(argv, "--model="+model, "-m=second-model")
	case 2:
		argv = append(argv, "-hf", "org/quantized-model")
	case 3:
		argv = append(argv, "-m="+model)
	case 4:
		if noModelField {
			argv = append(argv, "--model", model)
		} else {
			argv = append(argv, "--model="+model)
		}
	case 5:
		argv = append(argv, "-m", model, "--model="+modelValues[(iteration+1)%len(modelValues)])
	case 6:
		argv = append(argv, "-hf", "org/repo", "--model="+model)
	default:
		argv = append(argv, "-m", model, "-m=another,model", "--no-webui")
	}
	if iteration%2 == 0 {
		argv = append(argv, "--alias", "legacy-first,legacy-second", "-a=stale,alias")
	} else {
		argv = append(argv, "-a", "legacy-first,legacy-second", "--alias=stale,alias")
	}
	return append(argv, "--host", "{host}", "--port", "{port}")
}

func expectedLegacyReadinessArgv(entry infra.EngineCatalogEntry, port int) []string {
	argv := make([]string, 0, len(entry.Argv)+5)
	for index := 0; index < len(entry.Argv); index++ {
		token := entry.Argv[index]
		if entry.Engine == "llama-cpp" && (token == "-a" || token == "--alias") {
			if index+1 < len(entry.Argv) {
				index++
			}
			continue
		}
		if entry.Engine == "llama-cpp" && (strings.HasPrefix(token, "-a=") || strings.HasPrefix(token, "--alias=")) {
			continue
		}
		if entry.Engine == "mlx-swift" && token == "--model-id" {
			if index+1 < len(entry.Argv) {
				index++
			}
			continue
		}
		if entry.Engine == "mlx-swift" && strings.HasPrefix(token, "--model-id=") {
			continue
		}
		switch token {
		case "{host}":
			token = "127.0.0.1"
		case "{port}":
			token = strconv.Itoa(port)
		}
		argv = append(argv, token)
	}
	if entry.Model != "" {
		argv = append(argv, "--model", entry.Model)
	}
	switch entry.Engine {
	case "llama-cpp":
		return append(argv, "--alias", entry.ServedModelID)
	case "mlx-swift":
		return append(argv, "--model-id", entry.ServedModelID)
	default:
		return argv
	}
}

func containsStringPair(values []string, flag, value string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == flag && values[index+1] == value {
			return true
		}
	}
	return false
}

func runCatalogFixtureLifecycle(t *testing.T, engineName string, entry infra.EngineCatalogEntry) string {
	t.Helper()
	project, err := os.MkdirTemp("/tmp", "p-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	home, err := os.MkdirTemp("/tmp", "ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)
	var status infra.SharedRuntimeStatus
	selectedFreeEndpoint := false
	for attempt := 0; attempt < 64; attempt++ {
		candidateName := engineName
		if attempt > 0 {
			candidateName = fmt.Sprintf("%s-port-%02d", engineName, attempt)
		}
		catalog, err := infra.MarshalEngineCatalog(infra.EngineCatalogDocument{
			SchemaVersion: infra.EngineCatalogSchemaVersion,
			Engines:       map[string]infra.EngineCatalogEntry{candidateName: entry},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalogPath, catalog, 0o600); err != nil {
			t.Fatal(err)
		}
		status, _ = runStatus(t, []string{"status", "--engine", candidateName, "--json"})
		if status.Broker.State != "absent" || status.EngineAvailability != "unknown" {
			t.Fatalf("cold catalog engine did not retain absent broker and unknown availability: %#v", status)
		}
		endpoint, err := url.Parse(status.Endpoint)
		if err != nil || endpoint.Host == "" {
			t.Fatalf("cold catalog status returned an invalid endpoint %q: %v", status.Endpoint, err)
		}
		listener, err := net.Listen("tcp", endpoint.Host)
		if err != nil {
			continue
		}
		_ = listener.Close()
		engineName = candidateName
		selectedFreeEndpoint = true
		break
	}
	if !selectedFreeEndpoint {
		t.Fatalf("could not select an unused deterministic catalog endpoint for %q", engineName)
	}

	ensureContext, cancelEnsure := context.WithCancel(context.Background())
	ensureOutput, ensureError := &lockedBuffer{}, &lockedBuffer{}
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
	lease := waitForCatalogLeaseRecord(t, ensureOutput, ensureError, ensureDone, cancelEnsure,
		filepath.Join(home, "Library", "Caches"))
	if lease.LeaseToken == "" || lease.EngineIdentity.Name != engineName {
		t.Fatalf("real fixture did not reach a live engine lease: %#v", lease)
	}
	serving, _ := runStatus(t, []string{"status", "--engine", engineName, "--json"})
	if serving.Broker.State != "serving" || serving.EngineAvailability != "available" || serving.Runtime == nil {
		t.Fatalf("fixture model readiness did not yield an available serving runtime: %#v", serving)
	}
	servedModel := entry.ServedModelID
	if servedModel != "" {
		response, err := http.Get(serving.Runtime.Endpoint + "/models")
		if err != nil {
			t.Fatalf("read reported served model id: %v", err)
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&payload)
		_ = response.Body.Close()
		found := false
		for _, model := range payload.Data {
			if model.ID == servedModel {
				found = true
			}
		}
		if decodeErr != nil || !found {
			t.Fatalf("fake real-fixture server served model=%#v want=%q decodeErr=%v", payload.Data, servedModel, decodeErr)
		}
	}
	var releaseOut, releaseErr bytes.Buffer
	if code := run([]string{"release", "--engine", engineName, "--lease-token", lease.LeaseToken, "--json"}, &releaseOut, &releaseErr); code != 0 {
		t.Fatalf("release exit=%d stdout=%q stderr=%q", code, releaseOut.String(), releaseErr.String())
	}
	if code := waitForExit(t, ensureDone); code != 0 {
		t.Fatalf("ensure owner exit=%d stderr=%q", code, ensureError.String())
	}
	waitForEngineAbsent(t, engineName)
	var stopOut, stopErr bytes.Buffer
	if code := run([]string{"stop", "--engine", engineName, "--force", "--json"}, &stopOut, &stopErr); code != 0 {
		t.Fatalf("stop exit=%d stdout=%q stderr=%q", code, stopOut.String(), stopErr.String())
	}
	return engineName
}

func legacyFixtureServedModelID(entry infra.EngineCatalogEntry) string {
	for index, argument := range entry.Argv {
		if argument == "--alias" || argument == "-a" {
			if index+1 < len(entry.Argv) {
				return strings.SplitN(entry.Argv[index+1], ",", 2)[0]
			}
		}
		if strings.HasPrefix(argument, "--alias=") || strings.HasPrefix(argument, "-a=") {
			return strings.SplitN(strings.SplitN(argument, "=", 2)[1], ",", 2)[0]
		}
	}
	model := entry.Model
	if model == "" {
		for index, argument := range entry.Argv {
			if argument == "--model" || argument == "-m" {
				if index+1 < len(entry.Argv) {
					model = entry.Argv[index+1]
					break
				}
			}
			if strings.HasPrefix(argument, "--model=") || strings.HasPrefix(argument, "-m=") {
				model = strings.SplitN(argument, "=", 2)[1]
				break
			}
		}
	}
	if model == "" {
		return ""
	}
	return path.Base(model)
}

func runCatalogEngineStatus(t *testing.T, executable, model string) infra.SharedRuntimeStatus {
	t.Helper()
	project := t.TempDir()
	home, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, ".curator", "engines.toml")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := "schema_version = 1\n" +
		"[engines.qwen-local]\nmode = \"local\"\nengine = \"mlx-lm\"\n" +
		"model = " + strconv.Quote(model) + "\nserved_model_id = " + strconv.Quote(filepath.Base(model)) + "\nexecutable = " + strconv.Quote(executable) + "\n" +
		"argv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n"
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(project)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--engine", "qwen-local", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var status infra.SharedRuntimeStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v; stdout=%q", err, stdout.String())
	}
	return status
}
