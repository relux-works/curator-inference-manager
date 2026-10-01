//go:build darwin

package infra

import (
	"os"
	"strings"
	"testing"
)

func TestInferencePlaneDocumentationMatchesEngineCatalogV1(t *testing.T) {
	spec, err := os.ReadFile("../../spec/inference-plane.md")
	if err != nil {
		t.Fatal(err)
	}
	specText := string(spec)
	for _, required := range []string{
		"`schema_version = 1`",
		"TOML keys are case-sensitive",
		"`knobs.kv_context_size` and `knobs.prefill_chunk_size`",
		"`served_model_id` matching `[A-Za-z0-9][A-Za-z0-9._:/@+-]*`",
		"commas, whitespace, and `=` are refused",
		"an entry without it is non-runnable",
		"missing, empty, or untrimmed `engine` in any entry",
		"`pass` is refused only as a field token",
		"`auth` is refused as a field token",
		"every legacy `--model-id` option",
		"`mlx-lm` reports cached Hugging Face repository IDs",
		"catalog data never shortens or guesses a path-derived id",
		"this leaf adds no quarantine-clearing CLI command",
		"adopted by curator-engines v0.x; Decision 0020 pending in curator-spec",
		"closed P1–P4 propagation rule",
	} {
		if !strings.Contains(specText, required) {
			t.Errorf("spec/inference-plane.md is missing adopted catalog contract text %q", required)
		}
	}
	if strings.Contains(specText, "unquarantine") {
		t.Error("spec/inference-plane.md names a quarantine-clearing command that curator-engines does not expose")
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readmeText := string(readme)
	for _, required := range []string{
		"curator-spec/issues/114",
		"(cd contracttest && go test ./...)",
		"`engine_availability_reason`",
	} {
		if !strings.Contains(readmeText, required) {
			t.Errorf("README.md is missing catalog contract link or validation command %q", required)
		}
	}

	unresolved, err := os.ReadFile("../../UNRESOLVED_QUESTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unresolved), "Decision 0020") || !strings.Contains(string(unresolved), "curator-spec/issues/114") || strings.Contains(string(unresolved), "28odl0 stop-line") {
		t.Fatal("UNRESOLVED_QUESTIONS.md must retain only the real curator-spec adoption question")
	}
}
