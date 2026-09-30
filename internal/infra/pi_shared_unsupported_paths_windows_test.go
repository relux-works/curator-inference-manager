//go:build windows

package infra

import (
	"errors"
	"os"
	"testing"
)

func TestResolveSharedRuntimePathsRefusesUnsupportedPlatform(t *testing.T) {
	_, err := ResolveSharedRuntimePaths("/tmp", "profile")
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("ResolveSharedRuntimePaths returned %v, want shared_runtime_platform_unsupported", err)
	}
}

func TestResolveCatalogProfileKeepsRunnableUnsupportedPlatformRefusal(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", ServedModelID: "declared-model",
		Executable: executable,
	}
	_, err = resolveCatalogProfile(t.TempDir(), t.TempDir(), t.TempDir(), "candidate", entry, "engines.toml")
	var shared *SharedRuntimeError
	if !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
		t.Fatalf("runnable catalog entry bypassed platform refusal: %v", err)
	}
}
