package main

import (
	"errors"
	"flag"
	"io"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func runWeights(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "compute" {
		_ = writeLine(stderr, "weights requires compute --config FILE --engine NAME [--json]")
		return 2
	}
	flags := flag.NewFlagSet("curator-engines weights compute", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "", "explicit engine catalog")
	engine := flags.String("engine", "", "engine entry name")
	jsonOutput := flags.Bool("json", false, "write JSON output")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *config == "" || *engine == "" {
		_ = writeLine(stderr, "weights compute requires --config FILE and --engine NAME")
		return 2
	}
	observed, err := infra.ComputeEngineWeights(*config, *engine)
	if err != nil {
		return reportError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, observed, stderr)
	}
	if err := writeLine(stdout, observed.ID); err != nil {
		return reportError(stderr, err)
	}
	return 0
}
