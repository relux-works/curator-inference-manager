package infra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pelletier/go-toml/v2"
)

type PiLaunchError struct {
	Code string
	Err  error
}

func (e *PiLaunchError) Error() string { return e.Err.Error() }
func (e *PiLaunchError) Unwrap() error { return e.Err }

func piError(code string, err error) error { return &PiLaunchError{Code: code, Err: err} }

var errEngineProfileNoKVBound = errors.New("referenced model-harness profile has no --max-kv-size bound")

func validatePiModelHarnessKVBound(contextWindow int, _ string, runtime PiRuntime) error {
	profileName, configPath, ok := modelHarnessRunInvocation(runtime.Executable, runtime.Argv)
	if !ok {
		return nil
	}
	if configPath == "" {
		if configured := os.Getenv("MODEL_HARNESS_CONFIG"); configured != "" {
			if !filepath.IsAbs(configured) {
				return errors.New("MODEL_HARNESS_CONFIG must be an absolute path")
			}
			configPath = filepath.Clean(configured)
		} else {
			configDir, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("resolve model-harness config directory: %w", err)
			}
			configPath = filepath.Join(configDir, "model-harness", "config.toml")
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read referenced model-harness config %s: %w", configPath, err)
	}
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse referenced model-harness config %s: %w", configPath, err)
	}
	profiles, present, err := projectConfigTable(document, "profiles", "profiles")
	if err != nil || !present {
		if err == nil {
			err = errors.New("profiles table is absent")
		}
		return fmt.Errorf("resolve referenced model-harness profile %q: %w", profileName, err)
	}
	raw, present := profiles[profileName]
	if !present {
		return fmt.Errorf("resolve referenced model-harness profile %q: unknown profile", profileName)
	}
	profile, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("model-harness profile %q is not a table", profileName)
	}
	mode, ok := profile["mode"].(string)
	if !ok || mode == "" {
		return fmt.Errorf("model-harness profile %q lacks mode", profileName)
	}
	if mode != "local" {
		return nil
	}
	argv, present := profile["argv"]
	if !present {
		return fmt.Errorf("model-harness profile %q has no argv", profileName)
	}
	argvValues, ok := argv.([]any)
	if !ok {
		return fmt.Errorf("model-harness profile %q argv is not an array", profileName)
	}
	arguments := make([]string, len(argvValues))
	for index, value := range argvValues {
		argument, ok := value.(string)
		if !ok {
			return fmt.Errorf("model-harness profile %q argv[%d] is not a string", profileName, index)
		}
		arguments[index] = argument
	}
	bound, present, err := maxKVSizeFromArgv(arguments)
	if err != nil {
		return fmt.Errorf("model-harness profile %q argv: %w", profileName, err)
	}
	if !present {
		return fmt.Errorf("model-harness profile %q (%s) has no --max-kv-size bound but context_window is %d: %w", profileName, configPath, contextWindow, errEngineProfileNoKVBound)
	}
	if bound < contextWindow {
		return fmt.Errorf("model-harness profile %q (%s) --max-kv-size %d is less than context_window %d", profileName, configPath, bound, contextWindow)
	}
	return nil
}

func modelHarnessRunInvocation(executable string, argv []string) (profile, configPath string, ok bool) {
	if filepath.Base(executable) != "model-harness" || len(argv) < 2 || argv[0] != "run" {
		return "", "", false
	}
	profile = argv[1]
	for i := 2; i+1 < len(argv); i++ {
		if argv[i] == "--config" {
			configPath = argv[i+1]
		}
	}
	return profile, configPath, true
}

func maxKVSizeFromArgv(argv []string) (value int, present bool, err error) {
	for i, token := range argv {
		if token != "--max-kv-size" {
			continue
		}
		if present {
			return 0, false, errors.New("--max-kv-size must appear at most once")
		}
		if i+1 >= len(argv) {
			return 0, false, errors.New("--max-kv-size must be followed by a value")
		}
		n, convErr := strconv.Atoi(argv[i+1])
		if convErr != nil || n <= 0 {
			return 0, false, fmt.Errorf("--max-kv-size value %q must be a positive integer", argv[i+1])
		}
		value, present = n, true
	}
	return value, present, nil
}
