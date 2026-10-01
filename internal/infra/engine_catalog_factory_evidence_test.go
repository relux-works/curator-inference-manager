package infra

import (
	"errors"
	"testing"
)

func TestEngineCatalogErrorFactoriesNeverReturnSuccess(t *testing.T) {
	cause := errors.New("fixture rejection")
	for _, tc := range []struct {
		name    string
		factory func(string, error) error
		state   EngineCatalogState
	}{
		{"malformed", engineCatalogMalformed, EngineCatalogMalformed},
		{"read failed", engineCatalogReadFailed, EngineCatalogReadFailed},
		{"conflict", engineCatalogConflict, EngineCatalogConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.factory("fixture.site", cause)
			var typed *EngineCatalogError
			if !errors.As(err, &typed) || typed.State != tc.state || typed.Site != "fixture.site" || !errors.Is(err, cause) {
				t.Fatalf("factory erased or changed rejection: %v", err)
			}
			if code, ok := EngineCatalogExitCode(err); !ok || code != 1 {
				t.Fatalf("refusal became CLI success: %d %v", code, ok)
			}
		})
	}
	var typed *SharedRuntimeError
	err := engineCatalogNotRunnableError(sharedResolvedProfile{NotRunnableCode: "fixture_code", NotRunnableReason: "fixture reason"})
	if !errors.As(err, &typed) || typed.Code != "engine_not_runnable" {
		t.Fatalf("not-runnable refusal erased: %v", err)
	}
}

func TestReviewNarrowingMustKeepOtherRejectedEngines(t *testing.T) {
	for _, engine := range []string{"future", "another-future"} {
		t.Run(engine, func(t *testing.T) {
			entry := EngineCatalogEntry{Mode: "local", Engine: engine, Executable: "/bin/echo", Argv: []string{"{host}", "{port}"}}
			if err := validateEngineCatalogEntry("qwen", entry); err == nil {
				t.Fatalf("entire rejected engine class admitted: %q", engine)
			}
		})
	}
}
