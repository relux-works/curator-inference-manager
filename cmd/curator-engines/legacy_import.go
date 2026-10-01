//go:build !windows

package main

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func runLegacyImport(args []string, stdout, stderr io.Writer) int {
	action := "preview"
	if len(args) > 0 {
		action = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("curator-engines import-legacy "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "isolated destination home (default current home)")
	config := flags.String("config", "", "legacy model-harness TOML")
	evidence := flags.String("evidence-root", "", "legacy lifecycle-log entries")
	cache := flags.String("cache-root", "", "manager cache root")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	if *home == "" {
		var err error
		*home, err = os.UserHomeDir()
		if err != nil {
			return reportError(stderr, err)
		}
	}
	result, err := infra.ImportLegacyEngineState(infra.LegacyImportOptions{HomeDir: *home, ConfigPath: *config, EvidenceRoot: *evidence, CacheRoot: *cache, ProjectDir: currentProjectDir()}, action)
	if err != nil {
		return reportError(stderr, err)
	}
	return writeJSON(stdout, result, stderr)
}
func runLegacyEvidence(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	result, err := infra.ReadLegacyPiLifecycleEvidence(args[0])
	if err != nil {
		return reportError(stderr, err)
	}
	return writeJSON(stdout, result, stderr)
}
func legacyImportExitCode(err error) (int, bool) {
	var refusal *infra.LegacyImportError
	if errors.As(err, &refusal) {
		return 65, true
	}
	var evidence *infra.PiTerminalEvidenceError
	if errors.As(err, &evidence) {
		return 65, true
	}
	return 0, false
}
