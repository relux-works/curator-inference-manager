package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func runEngineCompatibility(parent context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		_ = writeLine(stderr, "compat requires alias and command; explicitly select --engine NAME")
		return 2
	}
	alias, command := args[0], args[1]
	if command == "--print-config" {
		command = "print-config"
	}
	if command == "--doctor" {
		command = "doctor"
	}
	flags := flag.NewFlagSet("curator-engines compat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("engine", "", "explicit engine name")
	home := flags.String("home", "", "destination home")
	cache := flags.String("cache-root", "", "manager cache root")
	config := flags.String("config", "", "legacy config to verify against the explicitly selected catalog entry")
	flags.Bool("json", false, "JSON output")
	// Legacy model-harness uses a positional profile. It is an explicit engine,
	// never a default; the profile body comes from the manager-owned catalog.
	rest := args[2:]
	if alias == "model-harness" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		*engine = rest[0]
		rest = rest[1:]
	}
	if err := flags.Parse(rest); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	rendered, err := infra.RenderEngineCompatibility(*home, *engine, alias)
	if err != nil {
		_ = writeLine(stderr, "migrate to curator-engines with an explicitly selected --engine NAME; import legacy profiles with import-legacy preview")
		return reportError(stderr, err)
	}
	if *config != "" {
		if err := infra.ValidateLegacyEngineCompatibilityConfig(*config, rendered); err != nil {
			return reportError(stderr, err)
		}
	}
	options := infra.SharedRuntimeOperatorOptions{HomeDir: *home, CacheRoot: *cache, ProjectDir: currentProjectDir(), Profile: *engine}
	switch command {
	case "print-config", "render":
		return writeJSON(stdout, rendered, stderr)
	case "diagnostics", "doctor", "status":
		status, err := infra.SharedRuntimeStatusReport(options)
		if err != nil {
			_ = writeJSON(stdout, map[string]string{"engine_availability": "unknown", "reason": "status_read_failed"}, stderr)
			return reportError(stderr, err)
		}
		return writeJSON(stdout, status, stderr)
	case "run", "ensure":
		return runEnsure(parent, options, true, stdout, stderr)
	case "stress":
		return runEngineStress(parent, options, rendered, stdout, stderr)
	case "stop":
		result, err := infra.StopSharedRuntime(options, false, 10*time.Second)
		if err != nil {
			return reportError(stderr, err)
		}
		return writeJSON(stdout, result, stderr)
	case "quarantine", "unquarantine":
		result, err := infra.SetSharedRuntimeManualQuarantine(options, command == "quarantine")
		if err != nil {
			return reportError(stderr, err)
		}
		return writeJSON(stdout, result, stderr)
	default:
		_ = writeLine(stderr, "unsupported compatibility command; migrate to curator-engines")
		return 65
	}
}

func runEngineStress(parent context.Context, options infra.SharedRuntimeOperatorOptions, rendered infra.EngineCompatibilityRender, stdout, stderr io.Writer) int {
	policy := rendered.Entry.Stress
	if policy == nil {
		_ = writeLine(stderr, "stress requires the selected engine's imported stress policy")
		return 65
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(policy.StartupTimeoutSeconds+policy.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	lease, err := infra.AcquireSharedRuntimeLease(ctx, options, "", os.Environ())
	if err != nil {
		_ = writeJSON(stdout, map[string]string{"engine_availability": "unknown"}, stderr)
		return reportError(stderr, err)
	}
	defer lease.Close()
	// The legacy prompt budget is an input request. Actual token usage comes
	// from the server response; it is never inferred from word count.
	payload, err := json.Marshal(map[string]any{"model": rendered.Entry.ServedModelID, "messages": []map[string]string{{"role": "user", "content": strings.Repeat("test ", policy.PromptTokens)}}, "max_tokens": policy.MaxOutputTokens})
	if err != nil {
		return reportError(stderr, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(lease.Record().Endpoint, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return reportError(stderr, err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: time.Duration(policy.RequestTimeoutSeconds) * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return reportError(stderr, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return reportError(stderr, err)
	}
	if response.StatusCode != http.StatusOK {
		return reportError(stderr, fmt.Errorf("stress response HTTP %d", response.StatusCode))
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return reportError(stderr, err)
	}
	return writeJSON(stdout, result, stderr)
}
