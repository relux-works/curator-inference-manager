package contracttest_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-agents-management/pkg/runtimecatalog"
)

func TestGeneratedManagerCatalogsAreDifferentiallyReadableBySkillAgentsManagementV0530(t *testing.T) {
	contractDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(contractDir, ".."))
	outputDir := t.TempDir()
	command := exec.Command("go", "test", "-count=1", "./internal/infra", "-run", "^TestWriteEngineCatalogContractFixturesForCoReader$")
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "CURATOR_ENGINE_CATALOG_CONTRACT_OUT="+outputDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("manager writer fixture generation failed: %v\n%s", err, output)
	}
	for _, testCase := range []struct {
		file string
		name string
		kind string
	}{
		{file: "local-engines.toml", name: "contract-local", kind: "llama-cpp"},
		{file: "local-unbounded-engines.toml", name: "contract-local-unbounded", kind: "mlx-lm"},
		{file: "ssh-engines.toml", name: "contract-ssh", kind: "mlx-lm"},
	} {
		data, err := os.ReadFile(filepath.Join(outputDir, testCase.file))
		if err != nil {
			t.Fatalf("read manager-produced %s catalog: %v", testCase.file, err)
		}
		catalog, err := runtimecatalog.ParseEngineCatalog(data)
		if err != nil {
			t.Fatalf("skill-agents-management v0.5.30 rejected manager-produced %s catalog: %v\n%s", testCase.file, err, data)
		}
		if len(catalog.Entries) != 1 || catalog.Entries[0].Name != testCase.name || catalog.Entries[0].Kind != testCase.kind {
			t.Fatalf("co-reader parsed unexpected %s engine entries: %#v", testCase.file, catalog.Entries)
		}
	}

	var manifest struct {
		Seed       int64 `json:"seed"`
		Iterations int   `json:"iterations"`
		Cases      []struct {
			Name            string `json:"name"`
			File            string `json:"file"`
			Mode            string `json:"mode"`
			Class           string `json:"class"`
			ManagerAccepted bool   `json:"manager_accepted"`
		} `json:"cases"`
	}
	manifestData, err := os.ReadFile(filepath.Join(outputDir, "generated-catalogs.json"))
	if err != nil {
		t.Fatalf("read generated manager catalog manifest: %v", err)
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode generated manager catalog manifest: %v", err)
	}
	if manifest.Seed != 0x5eedc0de || manifest.Iterations != 128 || len(manifest.Cases) < manifest.Iterations*2 {
		t.Fatalf("generated corpus bounds changed unexpectedly: seed=%#x iterations=%d cases=%d", manifest.Seed, manifest.Iterations, len(manifest.Cases))
	}
	accepted, rejected := 0, 0
	for _, generated := range manifest.Cases {
		data, err := os.ReadFile(filepath.Join(outputDir, generated.File))
		if err != nil {
			t.Fatalf("read generated %s catalog %q: %v", generated.Mode, generated.Name, err)
		}
		catalog, parseErr := runtimecatalog.ParseEngineCatalog(data)
		coReaderAccepted := parseErr == nil
		if coReaderAccepted != generated.ManagerAccepted {
			t.Fatalf("seed=%#x class=%s manager/co-reader acceptance differed for %s entry %q: manager=%t co-reader=%t err=%v\n%s", manifest.Seed, generated.Class, generated.Mode, generated.Name, generated.ManagerAccepted, coReaderAccepted, parseErr, data)
		}
		if strings.HasPrefix(generated.Class, "sibling-") && coReaderAccepted {
			t.Fatalf("real runtimecatalog reader accepted generated sibling mutation %s: %s", generated.Class, data)
		}
		if generated.ManagerAccepted {
			accepted++
			entry, ok := catalog.Entry(generated.Name)
			if !ok || entry.Kind == "" {
				t.Fatalf("co-reader omitted accepted generated engine entry %q: %#v", generated.Name, catalog.Entries)
			}
		} else {
			rejected++
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("generated corpus did not exercise both acceptance outcomes: accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestRealEngineCatalogReaderRejectsLegacyTokenCounterStringKey(t *testing.T) {
	body := []byte("schema_version = 1\n[engines.safe-engine]\nengine = \"mlx-lm\"\n[engines.safe-engine.knobs]\nprefill_chunk_tokens = \"unbounded\"\n")
	if _, err := runtimecatalog.ParseEngineCatalog(body); err == nil {
		t.Fatal("skill-agents-management v0.5.30 accepted a non-integer string under a *_tokens counter key")
	}
}
