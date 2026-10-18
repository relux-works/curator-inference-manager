//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func operatorPathFixture(t *testing.T, executable string) (string, string) {
	t.Helper()
	prepareCatalogCLIEntry(t, "selected", infra.EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "selected-model", ServedModelID: "selected-model",
		Executable: executable, Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	})
	config := filepath.Join(t.TempDir(), "selected.toml")
	// Move the catalog away from HOME so accidental default lookup cannot pass.
	if err := os.Rename(filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml"), config); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(os.Getenv("HOME"), "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	return config, cache
}

// All four commands refuse relative operator paths with specific typed codes,
// before creating state; the valid cold status is the positive control.
func TestOperatorPathsRejectRelative(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("selected.toml", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("cache", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status", "ensure", "release", "stop"} {
		for _, invalid := range []struct{ flag, value, code string }{
			{"--config", "selected.toml", "engine_catalog_read_failed"},
			{"--cache-root", "cache", "shared_runtime_state_path_invalid"},
		} {
			t.Run(command+invalid.flag, func(t *testing.T) {
				args := []string{command, "--engine", "selected", "--config", config, "--cache-root", cache, invalid.flag, invalid.value, "--json"}
				if command == "release" {
					args = append(args, "--lease-token", "invalid-token")
				}
				var out, stderr bytes.Buffer
				if code := run(args, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), `"code":`+strconv.Quote(invalid.code)) || out.Len() != 0 {
					t.Fatalf("exit=%d stdout=%q stderr=%q want=%s", code, out.String(), stderr.String(), invalid.code)
				}
			})
		}
	}
	status, _ := runStatus(t, []string{"status", "--engine", "selected", "--config", config, "--cache-root", cache, "--json"})
	canonical, err := filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatal(err)
	}
	if status.EngineIdentity.Name != "selected" || status.Broker.State != "absent" || status.Paths.CanonicalCacheRoot != canonical {
		t.Fatalf("selected paths not used: %#v", status)
	}
	for _, root := range []string{cache, "cache"} {
		if _, err := os.Stat(filepath.Join(root, "curator-engines")); !os.IsNotExist(err) {
			t.Fatalf("cold/refused requests wrote state: %v", err)
		}
	}
}

// Relocating the same unpinned entry changes neither runtime key nor engine identity.
func TestOperatorPathsPreserveIdentityAndDefaults(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	selected, _ := runStatus(t, []string{"status", "--engine", "selected", "--config", config, "--cache-root", cache, "--json"})
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	defaults, _ := runStatus(t, []string{"status", "--engine", "selected", "--json"})
	if selected.RuntimeKey != defaults.RuntimeKey || selected.EngineIdentity != defaults.EngineIdentity || selected.ProfileDigest != defaults.ProfileDigest {
		t.Fatal("catalog location changed identity")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(os.Getenv("HOME"), "Library", "Caches"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Paths.CanonicalCacheRoot != canonical {
		t.Fatalf("default cache changed: %#v", defaults.Paths)
	}
}

// Explicit selection retains strict catalog permission/schema checks, and never
// falls back to the default catalog when the selected file or entry is absent.
func TestOperatorPathsStrictCatalog(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	runStatus(t, []string{"status", "--engine", "selected", "--config", config, "--cache-root", cache, "--json"})
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		prepare    func()
	}{
		{"permissions", "engine_catalog_malformed", func() {
			if err := os.Chmod(config, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown-key", "engine_catalog_malformed", func() {
			if err := os.Chmod(config, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, append([]byte("unknown = true\n"), data...), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-entry", "engine_catalog_read_failed", func() {
			if err := os.WriteFile(config, bytes.ReplaceAll(data, []byte("engines.selected"), []byte("engines.other")), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-file", "engine_catalog_read_failed", func() {
			if err := os.Remove(config); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.prepare()
			var out, stderr bytes.Buffer
			code := run([]string{"status", "--engine", "selected", "--config", config, "--cache-root", cache, "--json"}, &out, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), `"code":`+strconv.Quote(tc.code)) || out.Len() != 0 {
				t.Fatalf("exit=%d out=%q err=%q", code, out.String(), stderr.String())
			}
		})
	}
}

// Oversized cache selections refuse before any state write and name both the
// actual socket length and the unchanged 103-byte limit.
func TestOperatorPathsRejectLongRendezvous(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	long := filepath.Join(cache, strings.Repeat("x", 110))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run([]string{"status", "--engine", "selected", "--config", config, "--cache-root", long, "--json"}, &out, &stderr)
	canonical, err := filepath.EvalSymlinks(long)
	if err != nil {
		t.Fatal(err)
	}
	length := len(filepath.Join(canonical, "curator-engines", "r1", strings.Repeat("0", 32), "b.sock"))
	if code != 1 || !strings.Contains(stderr.String(), `"code":"broker_rendezvous_path_too_long"`) || !strings.Contains(stderr.String(), strconv.Itoa(length)+" bytes; limit is 103 bytes") || out.Len() != 0 {
		t.Fatalf("exit=%d out=%q err=%q", code, out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(long, "curator-engines")); !os.IsNotExist(err) {
		t.Fatalf("refusal created state: %v", err)
	}
}

// An absent selected broker requires force; forced stop is an exact absent
// no-op. The valid catalog/cache are the same for both requests.
func TestOperatorPathsStopAbsent(t *testing.T) {
	config, cache := operatorPathFixture(t, "/bin/echo")
	args := []string{"stop", "--engine", "selected", "--config", config, "--cache-root", cache, "--json"}
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	var refusal infra.SharedRuntimeError
	if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || code != 1 || stdout.Len() != 0 || refusal.Code != "shared_runtime_broker_unreachable_use_force" {
		t.Fatalf("absent stop: exit=%d stdout=%q stderr=%q decode=%v", code, stdout.String(), stderr.String(), err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run(append(args, "--force"), &stdout, &stderr)
	var result infra.SharedRuntimeStopResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != 0 || stderr.Len() != 0 || result != (infra.SharedRuntimeStopResult{State: "absent"}) {
		t.Fatalf("forced absent stop: exit=%d result=%#v stderr=%q decode=%v", code, result, stderr.String(), err)
	}
}

// Relative CLI invocations use the explicit catalog/cache for ensure, status,
// release and a typed active-lease stop refusal. After zero-linger shutdown,
// forced stop is an absent no-op, not a stop of an already-draining broker.
func TestOperatorPathsRelativeInvocationLifecycle(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	binary := filepath.Join(t.TempDir(), "curator-engines")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	config, cache := operatorPathFixture(t, fake)
	common := []string{"--engine", "selected", "--config", config, "--cache-root", cache, "--json"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "./curator-engines", append([]string{"ensure"}, common...)...)
	child.Dir = filepath.Dir(binary)
	out, stderr := &lockedBuffer{}, &lockedBuffer{}
	child.Stdout, child.Stderr = out, stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		if err := child.Wait(); err != nil {
			done <- 1
		} else {
			done <- 0
		}
	}()
	defer func() { cancel(); <-done }()
	// Cleanup only this fixture's recorded broker if a lifecycle assertion fails.
	defer stopCatalogTestBrokers(cache)
	lease := waitForCatalogLeaseRecord(t, out, stderr, done, cancel, cache)
	// Every operator must use the same executable inode as the broker.
	invokeCLI := func(command string, extra ...string) ([]byte, []byte, error) {
		args := append(append([]string{command}, common...), extra...)
		operation := exec.CommandContext(ctx, "./curator-engines", args...)
		operation.Dir = child.Dir
		var stdout, stderr bytes.Buffer
		operation.Stdout, operation.Stderr = &stdout, &stderr
		err := operation.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
	runCLI := func(command string, extra ...string) []byte {
		t.Helper()
		output, stderr, err := invokeCLI(command, extra...)
		if err != nil {
			t.Fatalf("%s: %v: stdout=%s stderr=%s", command, err, output, stderr)
		}
		return output
	}
	var status infra.SharedRuntimeStatus
	if err := json.Unmarshal(runCLI("status"), &status); err != nil {
		t.Fatal(err)
	}
	if status.RuntimeKey != lease.RuntimeKey || status.EngineIdentity != lease.EngineIdentity || status.Broker.State != "serving" || status.Runtime == nil || len(status.Leases) != 1 {
		t.Fatalf("client/broker disagreed: %#v lease=%#v", status, lease)
	}
	// A live stop must reach this broker and preserve its active lease.
	output, refusalJSON, stopErr := invokeCLI("stop")
	var refusal struct {
		Code    string `json:"code"`
		Details struct {
			LeaseCount int                       `json:"lease_count"`
			Leases     []infra.SharedLeaseStatus `json:"leases"`
		} `json:"details"`
	}
	var stopExit *exec.ExitError
	if err := json.Unmarshal(refusalJSON, &refusal); err != nil || !errors.As(stopErr, &stopExit) || stopExit.ExitCode() != 1 || len(output) != 0 || refusal.Code != "shared_runtime_leases_active" || refusal.Details.LeaseCount != 1 || len(refusal.Details.Leases) != 1 || refusal.Details.Leases[0].LeaseID != lease.LeaseID {
		t.Fatalf("active stop: exit=%v stdout=%q stderr=%q decode=%v", stopErr, output, refusalJSON, err)
	}
	var result map[string]string
	released := runCLI("release", "--lease-token", lease.LeaseToken)
	if err := json.Unmarshal(released, &result); err != nil || result["state"] != "released" {
		t.Fatalf("release result=%q err=%v", released, err)
	}
	select {
	case code := <-done:
		done <- code
		if code != 0 {
			t.Fatalf("released ensure exit=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released ensure did not exit")
	}
	// Catalog entries use zero linger. Wait for completed state/socket cleanup;
	// a hello during shutdown can legitimately fail with shared_runtime_unavailable.
	for _, path := range []string{status.Paths.BrokerState, status.Paths.RendezvousSocket} {
		for {
			_, err := os.Lstat(path)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("released broker did not clean up %s: %v", path, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	var stopped infra.SharedRuntimeStopResult
	if err := json.Unmarshal(runCLI("stop", "--force"), &stopped); err != nil || stopped.State != "absent" || stopped.BrokerPID != 0 || stopped.RuntimePID != 0 || stopped.BrokerTerminated || stopped.RuntimeTerminated {
		t.Fatalf("stop result=%#v err=%v", stopped, err)
	}
}
