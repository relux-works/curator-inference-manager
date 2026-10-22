//go:build darwin

package infra

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type engineCatalogGuardTestRef struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type engineCatalogGuardSite struct {
	Bounds []struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"bounds"`
	Key          string                      `json:"key"`
	File         string                      `json:"file"`
	Function     string                      `json:"function"`
	Constructor  string                      `json:"constructor"`
	Site         string                      `json:"site"`
	Line         int                         `json:"line"`
	Tests        []engineCatalogGuardTestRef `json:"tests"`
	MutationKeys []string                    `json:"mutation_keys"`
}

type engineCatalogGuardInventory struct {
	Files   []string                   `json:"files"`
	Sites   []engineCatalogGuardSite   `json:"sites"`
	Mutants []engineCatalogGuardMutant `json:"mutants"`
}

type engineCatalogGuardMutant struct {
	Key      string   `json:"key"`
	SiteKey  string   `json:"site_key"`
	File     string   `json:"file"`
	Function string   `json:"function"`
	Sites    []string `json:"sites"`
	Operand  string   `json:"operand"`
	Line     int      `json:"line"`
	Tests    []string `json:"tests"`
}

func TestEngineCatalogTypedRefusalASTGuard(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate guard source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	if !filepath.IsAbs(source) {
		root, _ = filepath.Abs("../..")
	}
	command := exec.Command("go", "run", filepath.Join(root, "scripts", "engine-catalog-mutants.go"), "-inventory")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("package-scoped AST inventory failed: %v\n%s", err, output)
	}
	var inventory engineCatalogGuardInventory
	if err := json.Unmarshal(output, &inventory); err != nil {
		t.Fatalf("decode package-scoped AST inventory: %v\n%s", err, output)
	}
	if len(inventory.Files) == 0 || len(inventory.Sites) == 0 || len(inventory.Mutants) == 0 {
		t.Fatalf("package walk yielded empty refusal inventory: files=%d sites=%d mutants=%d", len(inventory.Files), len(inventory.Sites), len(inventory.Mutants))
	}
	expectedFiles, err := guardProductionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !guardEqualStrings(inventory.Files, expectedFiles) {
		t.Fatalf("source walk mismatch: generator=%v test=%v", inventory.Files, expectedFiles)
	}

	sitesByKey := make(map[string]engineCatalogGuardSite, len(inventory.Sites))
	for _, site := range inventory.Sites {
		if _, exists := sitesByKey[site.Key]; exists {
			t.Errorf("duplicate refusal site key %q", site.Key)
		}
		sitesByKey[site.Key] = site
		if site.Site == "" || strings.Contains(site.Site, "<unresolved-site>") {
			t.Errorf("%s:%d %s has no statically identified refusal site", site.File, site.Line, site.Function)
		}
		if len(site.Tests) == 0 && len(site.Bounds) == 0 {
			t.Errorf("%s:%d %s.%s has no named behavioral test", site.File, site.Line, site.Constructor, site.Site)
			continue
		}
		for _, reference := range site.Tests {
			if !guardTestExists(root, reference) {
				t.Errorf("%s:%d %s.%s maps to missing test %s in %s", site.File, site.Line, site.Constructor, site.Site, reference.Name, reference.Dir)
			}
		}
	}
	actualBySite := make(map[string][]string, len(sitesByKey))
	seenMutationKeys := make(map[string]bool, len(inventory.Mutants))
	for _, mutant := range inventory.Mutants {
		if seenMutationKeys[mutant.Key] {
			t.Errorf("duplicate generated mutation key %q", mutant.Key)
		}
		seenMutationKeys[mutant.Key] = true
		if len(mutant.Tests) == 0 {
			t.Errorf("%s:%d %s narrowing mutant has no named behavioral tests", mutant.File, mutant.Line, mutant.Function)
		}
		if _, exists := sitesByKey[mutant.SiteKey]; !exists {
			t.Errorf("mutant %q refers to unenumerated site %q", mutant.Key, mutant.SiteKey)
		}
		actualBySite[mutant.SiteKey] = append(actualBySite[mutant.SiteKey], mutant.Key)
	}
	for key, site := range sitesByKey {
		expected := append([]string(nil), site.MutationKeys...)
		actual := append([]string(nil), actualBySite[key]...)
		sort.Strings(expected)
		sort.Strings(actual)
		if (len(expected) == 0 && len(site.Bounds) == 0) || !guardEqualStrings(expected, actual) {
			t.Errorf("%s:%d %s.%s member coverage mismatch: enumerated=%v generated=%v", site.File, site.Line, site.Constructor, site.Site, expected, actual)
		}
	}
	t.Logf("module-wide production source files from a filesystem walk across every build tag: %d; typed refusal/status sites enumerated with explicit certificates or declared review bounds: %d/%d sites; mutants=%d", len(inventory.Files), len(sitesByKey), len(sitesByKey), len(inventory.Mutants))
	for _, site := range inventory.Sites {
		for _, bound := range site.Bounds {
			t.Logf("%s bound: %s: %s", site.Key, bound.Kind, bound.Reason)
		}
	}
	t.Logf("accepted subset: P1-P4 propagation; unconditional: AC1 + removal mutant + consumer narrowing; see spec/refusal-evidence.md")
}

func guardProductionFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk production source %s: %w", path, walkErr)
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("lstat production source %s: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in production source walk: %s", filepath.ToSlash(relative))
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("production source is not a regular file: %s", filepath.ToSlash(relative))
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func TestEngineCatalogGuardIncludesWindowsTaggedRefusalsAndRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module guardfixture\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"root.go":               "package guardfixture\n",
		"newpkg/refusal.go":     "//go:build windows\n\npackage newpkg\nfunc Refuse() error { return typedRefusal() }\n",
		"newpkg/typed_error.go": "package newpkg\ntype refusal struct{}\nfunc typedRefusal() error { return refusal{} }\n",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := guardProductionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !guardEqualStrings(files, []string{"newpkg/refusal.go", "newpkg/typed_error.go", "root.go"}) {
		t.Fatalf("filesystem walk omitted a new or windows-tagged package file: %v", files)
	}
	if err := os.Symlink(filepath.Join(root, "newpkg"), filepath.Join(root, "linked_dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := guardProductionFiles(root); err == nil || !strings.Contains(err.Error(), "linked_dir") {
		t.Fatalf("symlinked directory was not refused by name: %v", err)
	}
}

func guardEqualStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func guardTestExists(root string, reference engineCatalogGuardTestRef) bool {
	parts := strings.Split(reference.Name, "/")
	functionName := parts[0]
	foundParent := false
	paths, err := filepath.Glob(filepath.Join(root, reference.Dir, "*_test.go"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return false
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Name.Name != functionName || !strings.HasPrefix(functionName, "Test") {
				continue
			}
			foundParent = true
			matched := true
			for _, subtest := range parts[1:] {
				if !guardTestSubtestExists(function.Body, subtest) {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
	}
	return foundParent && len(parts) == 1
}

func guardTestSubtestExists(node ast.Node, subtest string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		literal, ok := child.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && strings.ReplaceAll(value, " ", "_") == subtest {
			found = true
		}
		return !found
	})
	return found
}
