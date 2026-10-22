//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func prepareWeightsEngine(t *testing.T, entry infra.EngineCatalogEntry) (string, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "runtime.pid")
	t.Setenv("CATALOG_FAKE_PID_FILE", pidFile)
	cache := prepareCatalogCLIEntry(t, "fixture", entry)
	return cache, pidFile
}
func assertWeightsRefusal(t *testing.T, args []string, code, site, pidFile string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exit := runWithContext(ctx, args, &out, &stderr)
	var refusal struct {
		Code string `json:"code"`
		Site string `json:"site"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || exit != 1 || refusal.Code != code || (site != "" && refusal.Site != site) || out.Len() != 0 {
		t.Fatalf("refusal: exit=%d stdout=%s stderr=%s decode=%v", exit, &out, &stderr, err)
	}
	if pidFile != "" {
		if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
			t.Fatalf("refused request started runtime: %v", err)
		}
	}
	return stderr.String()
}

// Every malformed/unsupported table refuses at its distinct catalog site before
// the fake executable runs, including an otherwise valid SSH or MLX entry.
func TestWeightsCatalogRefusalsDoNotStartProcess(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	for _, tc := range []struct{ name, table, code, site string }{
		{"uppercase", `id = "gguf-sha256:` + strings.Repeat("A", 64) + `"`, "engine_catalog_malformed", "entry.weights.id"},
		{"whitespace", `id = "gguf-sha256:` + strings.Repeat("a", 64) + ` "`, "engine_catalog_malformed", "entry.weights.id"},
		{"leading_whitespace", `id = " gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_malformed", "entry.weights.id"},
		{"short", `id = "gguf-sha256:` + strings.Repeat("a", 63) + `"`, "engine_catalog_malformed", "entry.weights.id"},
		{"non_string", `id = 42`, "engine_catalog_malformed", "entry.weights.id"},
		{"empty", `id = ""`, "engine_catalog_malformed", "entry.weights.id"},
		{"missing", "", "engine_catalog_malformed", "entry.weights.id"},
		{"other_prefix", `id = "sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.id.unsupported"},
		{"cache_opt_out", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"` + "\ncache=false", "engine_catalog_malformed", "catalog.weights.keys"},
		{"unknown", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"` + "\nversion=1", "engine_catalog_malformed", "catalog.weights.keys"},
		{"not_table", `"gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_malformed", "catalog.weights"},
		{"mlx", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.scope"},
		{"ssh", `id = "gguf-sha256:` + strings.Repeat("a", 64) + `"`, "engine_catalog_unsupported", "entry.weights.scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, entry, _ := weightsFixture(t)
			entry.Weights = nil
			entry.Executable = fake
			if tc.name == "mlx" {
				entry.Engine = "mlx-lm"
			}
			if tc.name == "ssh" {
				entry = infra.EngineCatalogEntry{Engine: "llama-cpp", Mode: "ssh", SSHExecutable: fake, SSHTarget: "fixture", RemoteExecutable: "/fixture/runtime", RemoteProfile: "fixture", RemoteHost: "127.0.0.1", RemotePort: 18012, ServedModelID: "fixture-model"}
			}
			_, pidFile := prepareWeightsEngine(t, entry)
			config := filepath.Join(os.Getenv("HOME"), ".curator", "engines.toml")
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "not_table" {
				data = append(data, []byte("\nweights = "+tc.table+"\n")...)
			} else {
				data = append(data, []byte("\n[engines.fixture.weights]\n"+tc.table+"\n")...)
			}
			if err := os.WriteFile(config, data, 0600); err != nil {
				t.Fatal(err)
			}
			assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--json"}, tc.code, tc.site, pidFile)
		})
	}
}

// Cold ensure compares recorded/actual and all three pin values; neither a
// disagreement nor a pin on an unpinned entry may start the runtime.
func TestWeightsEnsureRefusalsDoNotStartProcess(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	for _, name := range []string{"actual_mismatch", "pin_mismatch", "unpinned"} {
		t.Run(name, func(t *testing.T) {
			dir, entry, actual := weightsFixture(t)
			entry.Model = filepath.Join(dir, entry.Model)
			entry.Executable = fake
			pin := "gguf-sha256:" + strings.Repeat("b", 64)
			if name == "actual_mismatch" {
				entry.Weights.ID = "gguf-sha256:" + strings.Repeat("a", 64)
				pin = ""
			}
			recorded := entry.Weights.ID
			if name == "unpinned" {
				entry.Weights = nil
			}
			_, pidFile := prepareWeightsEngine(t, entry)
			args := []string{"ensure", "--engine", "fixture", "--json"}
			if pin != "" {
				args = append(args, "--expected-weights-id", pin)
			}
			code := "engine_weights_mismatch"
			if name == "unpinned" {
				code = "engine_weights_unpinned"
			}
			refusal := assertWeightsRefusal(t, args, code, "", pidFile)
			if name != "unpinned" {
				for _, id := range []string{actual, recorded, pin} {
					if !strings.Contains(refusal, id) {
						t.Fatalf("missing identity %q: %s", id, refusal)
					}
				}
			}
		})
	}
}

// Matching recorded/actual/operator ids start the fake runtime and status binds
// verification to that instance. Sharing survives file removal without rehash;
// a disagreeing new pin cannot acquire a lease or replace that instance.
func TestWeightsVerifiedLifecycleAndSharedPin(t *testing.T) {
	dir, entry, id := weightsFixture(t)
	entry.Model = filepath.Join(dir, entry.Model)
	entry.Executable = buildCatalogFakeRuntime(t)
	cache, pidFile := prepareWeightsEngine(t, entry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, stderr := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- runWithContext(ctx, []string{"ensure", "--engine", "fixture", "--expected-weights-id", id, "--json"}, out, stderr)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		stopCatalogTestBrokers(cache)
	}()
	lease := waitForCatalogLeaseRecord(t, out, stderr, done, cancel, cache)
	status, _ := runStatus(t, []string{"status", "--engine", "fixture", "--json"})
	if status.Weights.Verification != "verified" || status.Weights.ID != id || status.Weights.ObservedID != id || status.Weights.ObservedAt == nil || status.Weights.ObservedAt.IsZero() {
		t.Fatalf("status=%+v", status.Weights)
	}
	// Verified broker weights must also appear in the readings envelope.
	foundArtifact := false
	for _, row := range status.Readings.Facts {
		if string(row.Fact) == "weight-artifact" {
			foundArtifact = row.Outcome == "observed-value" && row.Source == "broker-verified-weights"
		}
	}
	if !foundArtifact {
		t.Fatal("verified weights missing from readings")
	}
	pidBefore, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entry.Model); err != nil {
		t.Fatal(err)
	}
	options := infra.SharedRuntimeOperatorOptions{ProjectDir: currentProjectDir(), Profile: "fixture", ExpectedWeightsID: id}
	shared, err := infra.AcquireSharedRuntimeLease(context.Background(), options, "", os.Environ())
	if err != nil {
		t.Fatalf("shared rehashed file: %v", err)
	}
	shared.Close()
	wrong := "gguf-sha256:" + strings.Repeat("c", 64)
	refusal := assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--expected-weights-id", wrong, "--json"}, "engine_weights_mismatch", "", "")
	if !strings.Contains(refusal, id) || !strings.Contains(refusal, wrong) {
		t.Fatal(refusal)
	}
	pidAfter, err := os.ReadFile(pidFile)
	if err != nil || !bytes.Equal(pidBefore, pidAfter) {
		t.Fatalf("instance replaced: %s -> %s %v", pidBefore, pidAfter, err)
	}
	status, _ = runStatus(t, []string{"status", "--engine", "fixture", "--json"})
	if status.Weights.Verification != "verified" || len(status.Leases) != 1 || status.Leases[0].LeaseID != lease.LeaseID {
		t.Fatalf("refusal changed leases or verification: %+v", status)
	}
}

// Replacing the model after hashing but before readiness stops the process that
// was started and returns the file-changed refusal without granting any lease.
func TestWeightsReplacementBeforeReadyStopsProcess(t *testing.T) {
	dir, entry, _ := weightsFixture(t)
	entry.Model = filepath.Join(dir, entry.Model)
	entry.Executable = buildCatalogFakeRuntime(t)
	cache, pidFile := prepareWeightsEngine(t, entry)
	defer stopCatalogTestBrokers(cache)
	t.Setenv("CATALOG_FAKE_REPLACE_MODEL", "1")
	assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--json"}, "engine_weights_file_changed", "", "")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake never started: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replaced-file runtime %d is still running", pid)
}

// Legacy catalog status explicitly reports unpinned, preserving broker absence.
func TestWeightsLegacyStatusUnpinned(t *testing.T) {
	status := runCatalogEngineStatus(t, "/bin/echo", "fixture-model")
	if status.Weights.ID != "" || status.Weights.Verification != "unpinned" || status.Broker.State != "absent" {
		t.Fatalf("legacy status=%+v", status)
	}
}

// The launch gate refuses the same invalid/split metadata as compute and check,
// even with a matching digest, before the fake runtime can write its marker.
func TestWeightsGGUFEnsureRefusalsDoNotStartProcess(t *testing.T) {
	fake := buildCatalogFakeRuntime(t)
	dir, _, _ := weightsFixture(t)
	valid, err := os.ReadFile(filepath.Join(dir, "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range weightsGGUFRefusals(t, valid) {
		t.Run(tc.name, func(t *testing.T) {
			dir, entry, _ := weightsFixture(t)
			entry.Model = filepath.Join(dir, tc.model)
			entry.Executable = fake
			entry.Weights.ID = fmt.Sprintf("gguf-sha256:%x", sha256.Sum256(tc.data))
			if err := os.WriteFile(entry.Model, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			cache, marker := prepareWeightsEngine(t, entry)
			defer stopCatalogTestBrokers(cache)
			assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--json"}, tc.code, "", marker)
		})
	}
}

// The CLI opt-out hashes actual bytes even when a same-owner private cache entry
// has an exact file key and a forged digest matching both pins; no process starts.
func TestWeightsEnsureNoCacheRejectsForgedEntry(t *testing.T) {
	dir, entry, actual := weightsFixture(t)
	entry.Model = filepath.Join(dir, entry.Model)
	forged := "gguf-sha256:" + strings.Repeat("a", 64)
	if actual == forged {
		t.Fatal("fixture must mismatch forged pin")
	}
	entry.Weights.ID = forged
	cache, marker := prepareWeightsEngine(t, entry)
	path, err := filepath.EvalSymlinks(entry.Model)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	cached := map[string]any{
		"Key": map[string]any{"Path": path, "Device": uint64(stat.Dev), "Inode": stat.Ino, "Size": info.Size(), "MTimeNS": stat.Mtimespec.Nano(), "CTimeNS": stat.Ctimespec.Nano()},
		"ID":  forged,
	}
	raw, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(cache, "weights-verification")
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(path))))
	if err := os.WriteFile(cacheFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	refusal := assertWeightsRefusal(t, []string{"ensure", "--engine", "fixture", "--expected-weights-id", forged, "--no-weights-cache", "--json"}, "engine_weights_mismatch", "", marker)
	if !strings.Contains(refusal, actual) {
		t.Fatalf("refusal lacks actual byte hash: %s", refusal)
	}
	after, err := os.ReadFile(cacheFile)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("opt-out changed cache: %v", err)
	}
}
