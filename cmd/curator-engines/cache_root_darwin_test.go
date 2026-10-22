package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

// The derived exact socket budget passes validation; each public operation
// refuses one extra byte before reading config or creating any state.
func TestCacheRootLength(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// Derive the budget from a real path, independently of the refusal guard.
	paths, err := infra.ResolveSharedRuntimePaths(root, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	limit := 103 - len(paths.RendezvousSocket) + len(paths.CanonicalCacheRoot)
	maximum := filepath.Join(paths.CanonicalCacheRoot, strings.Repeat("x", limit-len(paths.CanonicalCacheRoot)-1))
	base := maximum + "x"
	if err := os.Mkdir(maximum, 0700); err != nil {
		t.Fatal(err)
	}
	boundary, err := infra.ResolveSharedRuntimePaths(maximum, strings.Repeat("a", 64))
	if err != nil || len(boundary.RendezvousSocket) != 103 {
		t.Fatalf("exact boundary: %v %v", boundary, err)
	}
	if err := os.Remove(maximum); err != nil {
		t.Fatal(err)
	}
	if err := infra.ValidateEngineCacheRoot(maximum); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"check", "ensure", "status"} {
		t.Run(command, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := []string{command, "--cache-root", base, "--config", filepath.Join(root, "absent.toml"), "--engine", "model", "--json"}
			if code := run(args, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "broker_rendezvous_path_too_long") || !strings.Contains(stderr.String(), "limit is") {
				t.Fatalf("code=%d out=%s stderr=%s", code, &out, &stderr)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatalf("refusal created state: %v %v", files, err)
			}
		})
	}
}
