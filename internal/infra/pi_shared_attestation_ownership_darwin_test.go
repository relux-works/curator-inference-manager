//go:build darwin

package infra

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reviewFailTransport struct{}

func (reviewFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("review transient client network failure")
}
func TestReviewStatusAttestationFailureMustNotStopExistingLease(t *testing.T) {
	project, home, cache, _ := newSharedIntegrationProfile(t)
	fake := buildSharedFakeRuntime(t, filepath.Dir(project))
	writePiProjectConfig(t, project, sharedPiProfileTOML("profile", fake, 18011))
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: "profile"}
	lease, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-review-status", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	defer func() { _, _ = StopSharedRuntime(options, true, 3*time.Second) }()
	before, err := SharedRuntimeStatusReport(options)
	if err != nil || before.Runtime == nil {
		t.Fatalf("baseline: %#v %v", before, err)
	}
	broken := options
	broken.HTTPClient = &http.Client{Transport: reviewFailTransport{}}
	_, err = SharedRuntimeStatusReport(broken)
	if err == nil {
		t.Fatal("client network error was hidden")
	}
	t.Logf("status returned expected attestation refusal: %v", err)
	time.Sleep(300 * time.Millisecond)
	after, err := SharedRuntimeStatusReport(options)
	if err != nil || after.Runtime == nil || after.Runtime.PID != before.Runtime.PID || after.Broker.State != "serving" {
		t.Fatalf("read-only status failure stopped a pre-existing leased runtime: before=%s pid=%d after=%s runtime=%v error=%v", before.Broker.State, before.Runtime.PID, after.Broker.State, after.Runtime, err)
	}
}
func TestReviewMissingServedIDDoesNotMaskPathError(t *testing.T) {
	entry := EngineCatalogEntry{Mode: "local", Engine: "mlx-lm", Executable: "/bin/echo", Argv: []string{"--host", "{host}", "--port", "{port}"}}
	_, err := resolveCatalogProfile(t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "missing"), "candidate", entry, "engines.toml")
	if err == nil {
		t.Fatal("missing-id classification masked filesystem path failure")
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("path failure lost: %v", err)
	}
}

func TestReviewCatalogStatusAttestationFailureMustNotStopExistingLease(t *testing.T) {
	project, home, cache, _ := newSharedIntegrationProfile(t)
	fake := buildSharedFakeRuntime(t, filepath.Dir(project))
	writePiProjectConfig(t, project, sharedPiProfileTOML("profile", fake, 18011))
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"catalog-review": {Mode: "local", Engine: "mlx-lm", Model: "Model", ServedModelID: "Model", Executable: fake, Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}}})
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: "catalog-review"}
	lease, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-review-status", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	defer func() { _, _ = StopSharedRuntime(options, true, 3*time.Second) }()
	before, err := SharedRuntimeStatusReport(options)
	if err != nil || before.Runtime == nil {
		t.Fatalf("baseline: %#v %v", before, err)
	}
	broken := options
	broken.HTTPClient = &http.Client{Transport: reviewFailTransport{}}
	_, err = SharedRuntimeStatusReport(broken)
	if err == nil {
		t.Fatal("client network error was hidden")
	}
	t.Logf("status returned expected attestation refusal: %v", err)
	time.Sleep(300 * time.Millisecond)
	after, err := SharedRuntimeStatusReport(options)
	if err != nil || after.Runtime == nil || after.Runtime.PID != before.Runtime.PID || after.Broker.State != "serving" {
		t.Fatalf("read-only status failure stopped a pre-existing leased runtime: before=%s pid=%d after=%s runtime=%v error=%v", before.Broker.State, before.Runtime.PID, after.Broker.State, after.Runtime, err)
	}
}

func TestReviewRejectedSecondAcquireMustNotStopExistingLease(t *testing.T) {
	project, home, cache, _ := newSharedIntegrationProfile(t)
	fake := buildSharedFakeRuntime(t, filepath.Dir(project))
	writePiProjectConfig(t, project, sharedPiProfileTOML("profile", fake, 18011))
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: "profile"}
	lease, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-review-status", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	defer func() { _, _ = StopSharedRuntime(options, true, 3*time.Second) }()
	before, err := SharedRuntimeStatusReport(options)
	if err != nil || before.Runtime == nil {
		t.Fatalf("baseline: %#v %v", before, err)
	}
	broken := options
	broken.HTTPClient = &http.Client{Transport: reviewFailTransport{}}
	_, err = AcquireSharedRuntimeLease(context.Background(), broken, "RUN-review-second-client", os.Environ())
	if err == nil {
		t.Fatal("client network error was hidden")
	}
	t.Logf("status returned expected attestation refusal: %v", err)
	time.Sleep(300 * time.Millisecond)
	after, err := SharedRuntimeStatusReport(options)
	if err != nil || after.Runtime == nil || after.Runtime.PID != before.Runtime.PID || after.Broker.State != "serving" {
		t.Fatalf("read-only status failure stopped a pre-existing leased runtime: before=%s pid=%d after=%s runtime=%v error=%v", before.Broker.State, before.Runtime.PID, after.Broker.State, after.Runtime, err)
	}
}
