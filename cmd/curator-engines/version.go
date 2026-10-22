package main

import (
	"errors"
	"flag"
	"io"
	"runtime/debug"
)

var readBuildInfo = debug.ReadBuildInfo

type buildVersion struct {
	ModuleVersion string `json:"module_version"`
	VCSRevision   string `json:"vcs_revision"`
	VCSTime       string `json:"vcs_time"`
	VCSModified   string `json:"vcs_modified"`
	GoVersion     string `json:"go_version"`
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("curator-engines version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write JSON output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	version := buildVersion{"unknown", "unknown", "unknown", "unknown", "unknown"}
	known := func(value string) string {
		if value == "" {
			return "unknown"
		}
		return value
	}
	if info, ok := readBuildInfo(); ok && info != nil {
		version.ModuleVersion, version.GoVersion = known(info.Main.Version), known(info.GoVersion)
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				version.VCSRevision = known(setting.Value)
			case "vcs.time":
				version.VCSTime = known(setting.Value)
			case "vcs.modified":
				version.VCSModified = known(setting.Value)
			}
		}
	}
	if *jsonOutput {
		return writeJSON(stdout, version, stderr)
	}
	if err := writeFormatted(stdout, "Module version: %s\nVCS revision: %s\nVCS time: %s\nVCS modified: %s\nGo version: %s\n", version.ModuleVersion, version.VCSRevision, version.VCSTime, version.VCSModified, version.GoVersion); err != nil {
		return reportError(stderr, err)
	}
	return 0
}
