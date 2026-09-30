package infra

import (
	"errors"
	"testing"
)

// Production call site: RunSharedRuntimeLauncher -> ValidatePiExecutionEnvironment.
// The validator is attacked directly for malformed and runtime-affecting names;
// the launcher entry test separately proves it runs before execve.
func TestValidatePiExecutionEnvironmentRejectsEveryMalformedEntryAndRuntimeAffectingName(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		code    string
	}{
		{name: "missing equals", environ: []string{"BROKEN"}, code: "pi_execution_environment_malformed"},
		{name: "empty name", environ: []string{"=value"}, code: "pi_execution_environment_malformed"},
		{name: "duplicate name", environ: []string{"SAFE=one", "SAFE=two"}, code: "pi_execution_environment_malformed"},
		{name: "HF_ENDPOINT", environ: []string{"HF_ENDPOINT=https://invalid.example/token"}, code: "pi_execution_environment_invalid"},
		{name: "MODEL_ENDPOINT", environ: []string{"MODEL_ENDPOINT=https://invalid.example/token"}, code: "pi_execution_environment_invalid"},
		{name: "GGML_BACKEND_PATH", environ: []string{"GGML_BACKEND_PATH=/tmp/backend"}, code: "pi_execution_environment_invalid"},
		{name: "LLAMA_API_KEY", environ: []string{"LLAMA_API_KEY=redacted"}, code: "pi_execution_environment_invalid"},
		{name: "DYLD prefix", environ: []string{"DYLD_TEST=value"}, code: "pi_execution_environment_invalid"},
		{name: "LD prefix", environ: []string{"LD_TEST=value"}, code: "pi_execution_environment_invalid"},
		{name: "NODE prefix", environ: []string{"NODE_TEST=value"}, code: "pi_execution_environment_invalid"},
		{name: "BUN prefix", environ: []string{"BUN_TEST=value"}, code: "pi_execution_environment_invalid"},
		{name: "LLAMA_ARG prefix", environ: []string{"LLAMA_ARG_TEST=value"}, code: "pi_execution_environment_invalid"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidatePiExecutionEnvironment(testCase.environ)
			var launchError *PiLaunchError
			if !errors.As(err, &launchError) || launchError.Code != testCase.code {
				got := ""
				if launchError != nil {
					got = launchError.Code
				}
				t.Fatalf("environment=%q error code=%q want=%q err=%v", testCase.environ, got, testCase.code, err)
			}
		})
	}
}
