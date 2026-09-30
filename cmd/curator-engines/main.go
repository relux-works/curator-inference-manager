package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/relux-works/curator-inference-manager/internal/infra"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		writeUsage(stdout)
		return 0
	}
	if args[0] == "runtime" {
		return runInternal(args[1:], stdout, stderr)
	}
	command := args[0]
	if command != "status" && command != "ensure" && command != "release" && command != "stop" {
		if err := writeFormatted(stderr, "unknown curator-engines command %q\n", command); err != nil {
			return 1
		}
		writeUsage(stderr)
		return 2
	}

	flags := flag.NewFlagSet("curator-engines "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("engine", "", "engine entry name")
	jsonOutput := flags.Bool("json", false, "write JSON output")
	var leaseToken *string
	var force *bool
	var timeoutSeconds *int
	if command == "release" {
		leaseToken = flags.String("lease-token", "", "lease token returned by ensure")
	}
	if command == "stop" {
		force = flags.Bool("force", false, "force stop despite active leases")
		timeoutSeconds = flags.Int("timeout", 10, "stop timeout in seconds")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		if err := writeFormatted(stderr, "unexpected arguments: %s\n", strings.Join(flags.Args(), " ")); err != nil {
			return 1
		}
		return 2
	}
	options := infra.SharedRuntimeOperatorOptions{ProjectDir: currentProjectDir(), Profile: *engine}

	switch command {
	case "status":
		status, err := infra.SharedRuntimeStatusReport(options)
		if err != nil {
			return reportError(stderr, err)
		}
		if *jsonOutput {
			return writeJSON(stdout, status, stderr)
		}
		if err := writeFormatted(stdout, "Engine: %s\nState: %s\nEndpoint: %s\nLeases: %d\n", status.EngineIdentity.Name, status.Broker.State, status.Endpoint, len(status.Leases)); err != nil {
			return reportError(stderr, err)
		}
		return 0
	case "ensure":
		return runEnsure(parent, options, *jsonOutput, stdout, stderr)
	case "release":
		if leaseToken == nil || *leaseToken == "" {
			_ = writeLine(stderr, "release requires --lease-token")
			return 2
		}
		if err := infra.ReleaseSharedRuntimeLease(options, *leaseToken); err != nil {
			return reportError(stderr, err)
		}
		if *jsonOutput {
			return writeJSON(stdout, map[string]string{"state": "released"}, stderr)
		}
		if err := writeLine(stdout, "Released lease."); err != nil {
			return reportError(stderr, err)
		}
		return 0
	case "stop":
		if *timeoutSeconds <= 0 {
			_ = writeLine(stderr, "--timeout must be a positive number of seconds")
			return 2
		}
		result, err := infra.StopSharedRuntime(options, *force, time.Duration(*timeoutSeconds)*time.Second)
		if err != nil {
			return reportError(stderr, err)
		}
		if *jsonOutput {
			return writeJSON(stdout, result, stderr)
		}
		if err := writeLine(stdout, formatStopTextOutput(*engine, result.State)); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	return 2
}

func formatStopTextOutput(engine, state string) string {
	if engine == "" {
		return fmt.Sprintf("Shared runtime is %s.", state)
	}
	return fmt.Sprintf("Engine %s is %s.", engine, state)
}

func currentProjectDir() string {
	directory, err := os.Getwd()
	if err != nil {
		return "."
	}
	return directory
}

func runEnsure(parent context.Context, options infra.SharedRuntimeOperatorOptions, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	lease, err := infra.AcquireSharedRuntimeLease(ctx, options, "", os.Environ())
	if err != nil {
		return reportError(stderr, err)
	}
	defer lease.Close()
	if jsonOutput {
		if code := writeJSON(stdout, lease.Record(), stderr); code != 0 {
			return code
		}
	} else {
		record := lease.Record()
		if err := writeFormatted(stdout, "Engine: %s\nEndpoint: %s\nLease token: %s\nLease lifetime: %s\n", record.EngineIdentity.Name, record.Endpoint, record.LeaseToken, record.LeaseLifetime); err != nil {
			return reportError(stderr, err)
		}
	}
	select {
	case <-ctx.Done():
		return 0
	case monitorErr := <-lease.Monitor():
		if monitorErr != nil {
			return reportError(stderr, monitorErr)
		}
		return 0
	}
}

func runInternal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "broker" && args[0] != "runtime-launch") {
		_ = writeLine(stderr, "unknown internal runtime command")
		return 2
	}
	command := args[0]
	flags := flag.NewFlagSet("curator-engines runtime "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeKey := flags.String("runtime-key", "", "")
	project := flags.String("profile-project", "", "")
	profile := flags.String("profile", "", "")
	home := flags.String("home-dir", "", "")
	cache := flags.String("cache-root", "", "")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *runtimeKey == "" || *project == "" || *profile == "" || *home == "" || *cache == "" {
		_ = writeLine(stderr, "internal runtime command requires --runtime-key, --profile-project, --profile, --home-dir and --cache-root")
		return 2
	}
	options := infra.SharedRuntimeBrokerOptions{
		RuntimeKey: *runtimeKey, ProfileProject: *project, ProfileName: *profile,
		HomeDir: *home, CacheRoot: *cache, Environ: os.Environ(),
	}
	var err error
	if command == "broker" {
		err = infra.RunSharedRuntimeBroker(options)
	} else {
		err = infra.RunSharedRuntimeLauncher(infra.SharedRuntimeLauncherOptions{
			RuntimeKey: options.RuntimeKey, ProfileProject: options.ProfileProject,
			ProfileName: options.ProfileName, HomeDir: options.HomeDir,
			CacheRoot: options.CacheRoot, Environ: options.Environ,
		})
	}
	if err != nil {
		return reportError(stderr, err)
	}
	return 0
}

func reportError(stderr io.Writer, err error) int {
	_ = writeLine(stderr, err.Error())
	if code, ok := infra.SharedRuntimeExitCode(err); ok {
		return code
	}
	if code, ok := infra.EngineCatalogExitCode(err); ok {
		return code
	}
	return 1
}

func writeJSON(writer io.Writer, value any, stderr io.Writer) int {
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		_ = writeLine(stderr, err.Error())
		return 1
	}
	return 0
}

func writeUsage(writer io.Writer) {
	_ = writeLine(writer, "Usage: curator-engines <status|ensure|release|stop> [--engine NAME] [--json]")
	_ = writeLine(writer, "  release also requires --lease-token TOKEN")
	_ = writeLine(writer, "  stop accepts --force and --timeout SECONDS")
}

func writeLine(writer io.Writer, value string) error {
	_, err := fmt.Fprintln(writer, value)
	return err
}

func writeFormatted(writer io.Writer, format string, values ...any) error {
	_, err := fmt.Fprintf(writer, format, values...)
	return err
}
