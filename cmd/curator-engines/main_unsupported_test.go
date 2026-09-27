//go:build !darwin

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCuratorEnginesCommandsRefuseUnsupportedPlatforms(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{name: "status", args: []string{"status", "--json"}},
		{name: "ensure", args: []string{"ensure", "--json"}},
		{name: "release", args: []string{"release", "--lease-token", "test-token", "--json"}},
		{name: "stop", args: []string{"stop", "--json"}},
		{name: "runtime_broker", args: []string{"runtime", "broker", "--runtime-key", "test-key", "--profile-project", "/tmp", "--profile", "test", "--home-dir", "/tmp", "--cache-root", "/tmp"}},
		{name: "runtime_launcher", args: []string{"runtime", "runtime-launch", "--runtime-key", "test-key", "--profile-project", "/tmp", "--profile", "test", "--home-dir", "/tmp", "--cache-root", "/tmp"}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runWithContext(context.Background(), command.args, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code=%d, want unsupported-operation exit 1; stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("unsupported operation wrote success output: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "shared_runtime_platform_unsupported") {
				t.Fatalf("refusal=%q, want shared_runtime_platform_unsupported", stderr.String())
			}
		})
	}
}

func TestCuratorEnginesStatusEngineFlagPreservesUnsupportedPlatformRefusal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--engine", "missing"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("status --engine exit=%d, want unsupported-platform refusal; stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("status --engine wrote success output on an unsupported platform: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "shared_runtime_platform_unsupported") {
		t.Fatalf("status --engine refusal=%q, want shared_runtime_platform_unsupported", stderr.String())
	}
}
