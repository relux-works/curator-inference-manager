//go:build darwin

package infra

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const provenanceCatalog = `schema_version = 1
[engines.selected]
mode = "local"
engine = "mlx-lm"
model = "org/model"
served_model_id = "served"
executable = "/missing/engine"
argv = ["--host", "{host}", "--port", "{port}"]
`

func provenanceOptions(t *testing.T) SharedRuntimeOperatorOptions {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "sp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	return SharedRuntimeOperatorOptions{ProjectDir: root, HomeDir: root, CacheRoot: cache, Profile: "selected"}
}

func provenanceReport(t *testing.T, options SharedRuntimeOperatorOptions) SharedRuntimeStatus {
	t.Helper()
	report, err := SharedRuntimeStatusReport(options)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSharedRuntimeStatusJSON(raw, options.Profile); err != nil {
		t.Fatal(err)
	}
	return report
}

// Status reports the canonical catalog parent and exact broker cache root,
// including when both are selected through symlinked directories.
func TestStatusProvenanceCanonicalPaths(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			options := provenanceOptions(t)
			catalog := filepath.Join(options.HomeDir, "engines.toml")
			if err := os.WriteFile(catalog, []byte(provenanceCatalog), 0600); err != nil {
				t.Fatal(err)
			}
			options.ConfigPath = catalog
			if linked {
				alias := filepath.Join(options.HomeDir, "alias")
				if err := os.Symlink(options.HomeDir, alias); err != nil {
					t.Fatal(err)
				}
				options.ConfigPath = filepath.Join(alias, "engines.toml")
				options.CacheRoot = filepath.Join(alias, "cache")
			}
			report := provenanceReport(t, options)
			canonical, err := filepath.EvalSymlinks(catalog)
			if err != nil {
				t.Fatal(err)
			}
			expected := StatusProvenance{ContractVersion: "status-provenance/v1", EnginesConfig: canonical,
				EnginesConfigSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(provenanceCatalog))), CacheRoot: report.Paths.CanonicalCacheRoot}
			if report.Provenance == nil || *report.Provenance != expected {
				t.Fatalf("provenance=%+v want=%+v", report.Provenance, expected)
			}
			cache, err := filepath.EvalSymlinks(options.CacheRoot)
			if err != nil || report.Provenance.CacheRoot != cache {
				t.Fatalf("cache=%q want=%q err=%v", report.Provenance.CacheRoot, cache, err)
			}
		})
	}
}

// Different catalog bytes/locations distinguish identical selected entries
// without changing catalog-independent engine, runtime or profile identity.
func TestStatusProvenanceDistinguishesCatalogs(t *testing.T) {
	options := provenanceOptions(t)
	var reports []SharedRuntimeStatus
	for i, data := range []string{provenanceCatalog, provenanceCatalog + "\n# unrelated comment\n"} {
		options.ConfigPath = filepath.Join(options.HomeDir, fmt.Sprintf("catalog-%d.toml", i))
		if err := os.WriteFile(options.ConfigPath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		reports = append(reports, provenanceReport(t, options))
	}
	a, b := reports[0], reports[1]
	if a.Provenance.EnginesConfig == b.Provenance.EnginesConfig || a.Provenance.EnginesConfigSHA256 == b.Provenance.EnginesConfigSHA256 {
		t.Fatal("catalog provenance was indistinguishable")
	}
	if a.EngineIdentity != b.EngineIdentity || a.RuntimeKey != b.RuntimeKey || a.ProfileDigest != b.ProfileDigest {
		t.Fatal("catalog provenance changed runtime identity")
	}
}

// Each status opens the catalog once and hashes the same bytes it decoded,
// even if the file is rewritten immediately after that read; the next call sees the rewrite.
func TestStatusProvenanceSameReadDigest(t *testing.T) {
	options := provenanceOptions(t)
	options.ConfigPath = filepath.Join(options.HomeDir, "engines.toml")
	original := []byte(provenanceCatalog)
	rewritten := []byte(strings.Replace(provenanceCatalog, `served_model_id = "served"`, `served_model_id = "changed"`, 1))
	if err := os.WriteFile(options.ConfigPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	oldOpen, oldRead := openEngineCatalogFile, readEngineCatalogFile
	t.Cleanup(func() { openEngineCatalogFile, readEngineCatalogFile = oldOpen, oldRead })
	opens, reads := 0, 0
	openEngineCatalogFile = func(path string) (*os.File, error) { opens++; return oldOpen(path) }
	readEngineCatalogFile = func(reader io.Reader) ([]byte, error) {
		reads++
		data, err := oldRead(reader)
		if err == nil {
			if err := os.WriteFile(options.ConfigPath, rewritten, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return data, err
	}
	first := provenanceReport(t, options)
	if opens != 1 || reads != 1 || first.Provenance.EnginesConfigSHA256 != fmt.Sprintf("%x", sha256.Sum256(original)) {
		t.Fatalf("first digest/read mismatch: %+v opens=%d reads=%d", first.Provenance, opens, reads)
	}
	second := provenanceReport(t, options)
	if opens != 2 || reads != 2 || second.Provenance.EnginesConfigSHA256 != fmt.Sprintf("%x", sha256.Sum256(rewritten)) || first.Provenance.EnginesConfigSHA256 == second.Provenance.EnginesConfigSHA256 {
		t.Fatalf("rewrite not observed: %+v opens=%d reads=%d", second.Provenance, opens, reads)
	}
	if first.RuntimeKey == second.RuntimeKey {
		t.Fatal("first call did not decode the original entry")
	}
}

// Legacy fallback omits provenance for both a missing catalog and a present
// catalog without the selected entry; an explicit absent selection returns no report.
func TestStatusProvenanceOmittedWithoutCatalogEntry(t *testing.T) {
	project, home, cache, _ := newSharedIntegrationProfile(t)
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: "profile"}
	catalog := filepath.Join(home, ".curator", "engines.toml")
	for _, present := range []bool{false, true} {
		if present {
			if err := os.MkdirAll(filepath.Dir(catalog), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(catalog, []byte(provenanceCatalog), 0600); err != nil {
				t.Fatal(err)
			}
		}
		report := provenanceReport(t, options)
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if report.Provenance != nil || strings.Contains(string(raw), `"provenance"`) {
			t.Fatalf("legacy status emitted provenance: %s", raw)
		}
	}
	options.ConfigPath = catalog
	report, err := SharedRuntimeStatusReport(options)
	if !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.selection") || report.Provenance != nil {
		t.Fatalf("absent selection: %+v %v", report, err)
	}
	raw, err := json.Marshal(report)
	if err != nil || strings.Contains(string(raw), `"provenance"`) {
		t.Fatalf("absent selection emitted provenance: %s %v", raw, err)
	}
}

// The same-read reader returns a digest only for a successfully decoded selected
// entry; absent selections and malformed bytes retain their existing state/refusal.
func TestCatalogDigestOmittedWithoutSelectedEntry(t *testing.T) {
	options := provenanceOptions(t)
	catalog := filepath.Join(options.HomeDir, "engines.toml")
	for _, tc := range []struct {
		name, data, selected string
		want                 EngineCatalogState
	}{
		{"valid", provenanceCatalog, "selected", EngineCatalogPresent},
		{"absent-entry", provenanceCatalog, "missing", EngineCatalogAbsent},
		{"malformed", "schema_version = [", "selected", EngineCatalogMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(catalog, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, state, digest, err := readEngineCatalogEntryWithDigest(catalog, tc.selected)
			if state != tc.want {
				t.Fatalf("state=%s error=%v", state, err)
			}
			if tc.want == EngineCatalogPresent {
				if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256([]byte(tc.data))) {
					t.Fatalf("digest=%q error=%v", digest, err)
				}
			} else if digest != "" || (tc.want == EngineCatalogAbsent && err != nil) || (tc.want == EngineCatalogMalformed && !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.decode")) {
				t.Fatalf("digest=%q error=%v", digest, err)
			}
		})
	}
}

// Retargeting the requested parent after the read cannot reattribute the
// decoded bytes to a different catalog directory.
func TestStatusProvenanceCatalogParentRetarget(t *testing.T) {
	options := provenanceOptions(t)
	originalDir, otherDir := filepath.Join(options.HomeDir, "original"), filepath.Join(options.HomeDir, "other")
	for _, dir := range []string{originalDir, otherDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "engines.toml"), []byte(provenanceCatalog), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(options.HomeDir, "alias")
	if err := os.Symlink(originalDir, alias); err != nil {
		t.Fatal(err)
	}
	options.ConfigPath = filepath.Join(alias, "engines.toml")
	oldRead := readEngineCatalogFile
	t.Cleanup(func() { readEngineCatalogFile = oldRead })
	readEngineCatalogFile = func(reader io.Reader) ([]byte, error) {
		data, err := oldRead(reader)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(otherDir, alias); err != nil {
			t.Fatal(err)
		}
		return data, nil
	}
	report := provenanceReport(t, options)
	canonical, err := filepath.EvalSymlinks(originalDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Provenance.EnginesConfig != filepath.Join(canonical, "engines.toml") {
		t.Fatalf("reattributed catalog: %+v", report.Provenance)
	}
}

// A parent that appears only after canonicalization failed cannot yield a
// partially attributed report, even when the subsequent catalog read succeeds.
func TestStatusProvenanceRefusesUnresolvedCatalogParent(t *testing.T) {
	options := provenanceOptions(t)
	options.ConfigPath = filepath.Join(options.HomeDir, "late", "engines.toml")
	oldLstat := lstatEngineCatalogFile
	t.Cleanup(func() { lstatEngineCatalogFile = oldLstat })
	lstatEngineCatalogFile = func(path string) (os.FileInfo, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(provenanceCatalog), 0600); err != nil {
			t.Fatal(err)
		}
		return oldLstat(path)
	}
	report, err := SharedRuntimeStatusReport(options)
	if !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.selection") || report.Provenance != nil {
		t.Fatalf("unresolved parent published provenance: %+v error=%v", report, err)
	}
}

// Canonical provenance does not rewrite the selected path used to expand a
// relative pinned model, preserving the existing argv and runtime identity inputs.
func TestStatusProvenancePreservesRelativeModelSelection(t *testing.T) {
	options := provenanceOptions(t)
	catalog := filepath.Join(options.HomeDir, "engines.toml")
	data := strings.Replace(provenanceCatalog, `engine = "mlx-lm"`, `engine = "llama-cpp"`, 1)
	data = strings.Replace(data, `model = "org/model"`, `model = "model.gguf"`, 1)
	data += "\n[engines.selected.weights]\nid = \"gguf-sha256:" + strings.Repeat("a", 64) + "\"\n"
	if err := os.WriteFile(catalog, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(options.HomeDir, "alias")
	if err := os.Symlink(options.HomeDir, alias); err != nil {
		t.Fatal(err)
	}
	options.ConfigPath = filepath.Join(alias, "engines.toml")
	resolved, err := resolveSharedProfileWithCatalog(options.ProjectDir, options.HomeDir, options.CacheRoot, options.Profile, options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.Source != options.ConfigPath {
		t.Fatalf("profile source changed: %q", resolved.Profile.Source)
	}
	argv := resolved.Profile.Runtime.Argv
	expectedModel := filepath.Join(alias, "model.gguf")
	models := 0
	for i, token := range argv {
		if token == "--model" {
			models++
			if i+1 >= len(argv) || argv[i+1] != expectedModel {
				t.Fatalf("relative model expansion changed: %v", argv)
			}
		}
	}
	if models != 1 {
		t.Fatalf("expected one expanded model: %v", argv)
	}
	report := provenanceReport(t, options)
	key, digest := SharedRuntimeKey(resolved.Profile)
	if report.RuntimeKey != key || report.ProfileDigest != digest {
		t.Fatal("status identity diverged from selected profile")
	}
	canonical, err := filepath.EvalSymlinks(catalog)
	if err != nil || report.Provenance.EnginesConfig != canonical {
		t.Fatalf("catalog=%+v error=%v", report.Provenance, err)
	}
}
