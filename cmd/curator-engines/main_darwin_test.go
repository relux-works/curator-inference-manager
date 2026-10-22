//go:build darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A valid short cache root lets status reach profile resolution for a missing engine.
func TestCuratorEnginesStatusDrivesProductionResolverWithEngineFlag(t *testing.T) {
	project := t.TempDir()
	home := filepath.Join(project, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })
	t.Setenv("HOME", home)
	cache, err := os.MkdirTemp("/tmp", "status-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--engine", "missing", "--cache-root", cache}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `"code":"unknown_pi_profile"`) {
		t.Fatalf("status --engine did not reach production profile resolution: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
