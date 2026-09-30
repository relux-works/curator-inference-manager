package main

import (
	"errors"
	"flag"
	"io"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func runCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("curator-engines check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "", "explicit engine entry configuration file")
	engine := flags.String("engine", "", "engine entry name")
	jsonOutput := flags.Bool("json", false, "write JSON output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *config == "" || *engine == "" {
		_ = writeLine(stderr, "check requires --config FILE and --engine NAME, with no positional arguments")
		return 2
	}
	report := infra.CheckEngineFile(*config, *engine)
	if *jsonOutput {
		if code := writeJSON(stdout, report, stderr); code != 0 {
			return code
		}
	} else {
		if err := writeFormatted(stdout, "Engine: %s\nKV RAM: %s", report.Engine, report.KVRAM.Status); err != nil {
			return reportError(stderr, err)
		}
		if report.KVRAM.Bytes != nil {
			if err := writeFormatted(stdout, " (%d bytes)", *report.KVRAM.Bytes); err != nil {
				return reportError(stderr, err)
			}
		} else {
			if err := writeFormatted(stdout, " (%s)", report.KVRAM.ReasonCode); err != nil {
				return reportError(stderr, err)
			}
		}
		if err := writeLine(stdout, ""); err != nil {
			return reportError(stderr, err)
		}
		for _, failure := range report.Failures {
			if err := writeFormatted(stdout, "%s: %s\n", failure.Code, failure.Message); err != nil {
				return reportError(stderr, err)
			}
		}
	}
	if len(report.Failures) != 0 {
		return 1
	}
	return 0
}
