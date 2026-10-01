// Command engine-catalog-mutants derives typed refusal gates from all
// production Go files found by a build-tag-independent filesystem walk, generates narrowing
// mutants, and runs the behavioral tests that name each refusal site.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type refusalSite struct {
	bounds               []siteBound
	unconditional        bool
	typeContext          *typedUnit
	fset                 *token.FileSet
	witnesses            []narrowingWitness
	id                   string
	file                 string
	function             string
	constructor          string
	site                 string
	line                 int
	guard                *ast.IfStmt
	guardExpr            ast.Expr
	elseBranch           bool
	call                 *ast.CallExpr
	expression           ast.Expr
	tests                []testReference
	expectedMutationKeys []string
}

type testReference struct {
	BodySHA256 string `json:"body_sha256"`
	Name       string `json:"name"`
	Dir        string `json:"dir"`
}

type parsedTest struct {
	bodyDigest string
	reference  testReference
	body       *ast.BlockStmt
}

type sourceMutation struct {
	start, end  int
	replacement string
}

type mutant struct {
	bounds      []siteBound
	extra       []sourceMutation
	key         string
	siteID      string
	file        string
	function    string
	constructor string
	sites       []string
	tests       []testReference
	line        int
	guardText   string
	memberText  string
	replaceText string
	start       int
	end         int
}

type overlay struct {
	Replace map[string]string `json:"Replace"`
}

type inventorySite struct {
	Bounds       []siteBound     `json:"bounds,omitempty"`
	Key          string          `json:"key"`
	File         string          `json:"file"`
	Function     string          `json:"function"`
	Constructor  string          `json:"constructor"`
	Site         string          `json:"site"`
	Line         int             `json:"line"`
	Tests        []testReference `json:"tests"`
	MutationKeys []string        `json:"mutation_keys"`
}

type inventoryReport struct {
	AcceptedSubset string            `json:"accepted_subset"`
	Files          []string          `json:"files"`
	Sites          []inventorySite   `json:"sites"`
	Mutants        []inventoryMutant `json:"mutants"`
}

type inventoryMutant struct {
	Key      string   `json:"key"`
	SiteKey  string   `json:"site_key"`
	File     string   `json:"file"`
	Function string   `json:"function"`
	Sites    []string `json:"sites"`
	Operand  string   `json:"operand"`
	Line     int      `json:"line"`
	Tests    []string `json:"tests"`
}

func main() {
	out := flag.String("out", "", "write the Markdown table and per-mutant logs beneath this path")
	start := flag.Int("start", 0, "first generated mutant index, zero based")
	count := flag.Int("count", 0, "maximum mutants to run; zero means all remaining")
	workers := flag.Int("workers", 4, "parallel behavioral mutant processes")
	linuxEmulationRoot := flag.String("linux-emu-root", "", "materialized Linux file-set emulation root for non-Darwin mutants")
	windowsEmulationRoot := flag.String("windows-emu-root", "", "materialized Windows file-set emulation root for non-Darwin mutants")
	scopeFiles := flag.String("files", "", "comma-separated production files selecting execution rows after the complete module census")
	taskID := flag.String("task", "TASK-260930-1hzscs", "safe outcome filename prefix")
	inventoryOnly := flag.Bool("inventory", false, "print the package-derived refusal inventory as JSON")
	flag.Parse()
	root, err := os.Getwd()
	fatalIf(err)
	rootPath, err := filepath.Abs(root)
	fatalIf(err)

	files, err := productionPackageScope(rootPath)
	fatalIf(err)
	sites, err := collectSites(rootPath, files)
	fatalIf(err)
	mutants, err := deriveMutants(rootPath, sites)
	fatalIf(err)
	mutants, err = selectMutationFiles(files, mutants, *scopeFiles)
	fatalIf(err)
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(*taskID) {
		fatalIf(fmt.Errorf("task must be a safe filename prefix"))
	}
	if *inventoryOnly {
		report := inventoryReport{Files: files, AcceptedSubset: propagationSubset}
		for _, site := range sites {
			report.Sites = append(report.Sites, inventorySite{Key: site.id, File: site.file, Function: site.function, Constructor: site.constructor, Site: site.site, Line: site.line, Tests: site.tests, MutationKeys: site.expectedMutationKeys, Bounds: site.bounds})
		}
		for _, mutant := range mutants {
			tests := make([]string, len(mutant.tests))
			for index, reference := range mutant.tests {
				tests[index] = reference.Dir + ":" + reference.Name
			}
			report.Mutants = append(report.Mutants, inventoryMutant{Key: mutant.key, SiteKey: mutant.siteID, File: mutant.file, Function: mutant.function, Sites: mutant.sites, Operand: mutant.memberText, Line: mutant.line, Tests: tests})
		}
		fatalIf(json.NewEncoder(os.Stdout).Encode(report))
		return
	}
	if containsValue(files, "internal/infra/pi_shared_unsupported_posix.go") {
		*linuxEmulationRoot = validateEmulationRoot(*linuxEmulationRoot, "Linux", "internal/infra/pi_shared_unsupported_posix.go")
	}
	if containsValue(files, "internal/infra/pi_shared_api_unsupported.go") {
		*linuxEmulationRoot = validateEmulationRoot(*linuxEmulationRoot, "Linux", "internal/infra/pi_shared_api_unsupported.go")
	}
	if containsValue(files, "internal/infra/pi_shared_unsupported_windows.go") {
		*windowsEmulationRoot = validateEmulationRoot(*windowsEmulationRoot, "Windows", "internal/infra/pi_shared_unsupported_windows.go")
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required and must be under .temp/")
		os.Exit(2)
	}
	outPath, err := filepath.Abs(*out)
	fatalIf(err)
	if !within(filepath.Join(rootPath, ".temp"), outPath) {
		fatalIf(fmt.Errorf("mutation outputs must be under the repository .temp/ directory"))
	}
	if err := os.MkdirAll(outPath, 0o700); err != nil {
		fatalIf(err)
	}

	if *start < 0 || *start > len(mutants) {
		fatalIf(fmt.Errorf("start index %d is outside 0..%d", *start, len(mutants)))
	}
	if *workers < 1 {
		fatalIf(fmt.Errorf("workers must be positive"))
	}
	end := len(mutants)
	if *count > 0 && *start+*count < end {
		end = *start + *count
	}
	rows := runMutants(rootPath, outPath, mutants[*start:end], *start, *workers, *linuxEmulationRoot, *windowsEmulationRoot)
	manifest := filepath.Join(outPath, fmt.Sprintf("%s_mutants_%03d-%03d.md", *taskID, *start, end))
	if err := os.WriteFile(manifest, []byte(renderTable(len(sites), len(mutants), rows)), 0o600); err != nil {
		fatalIf(err)
	}
	records := []map[string]any{}
	for _, row := range rows {
		records = append(records, map[string]any{"index": row.index, "key": row.mutant.key, "site": row.mutant.siteID, "member": row.mutant.memberText, "state": row.state, "test": row.test, "exit": row.code, "bound": row.bound, "log": row.log, "joint_log": row.jointLog, "joint_exit": row.jointCode, "target": row.target})
	}
	encoded, err := json.MarshalIndent(records, "", "  ")
	fatalIf(err)
	fatalIf(os.WriteFile(manifest+".json", encoded, 0600))
	failures := 0
	for _, row := range rows {
		if row.state != "killed" && row.state != "equivalent" && !strings.HasPrefix(row.state, "bound: ") {
			failures++
		}
	}
	fmt.Printf("package production files=%d; refusal/gate sites=%d; generated mutants=%d; run=%d; unresolved=%d; table=%s\n", len(files), len(sites), len(mutants), len(rows), failures, manifest)
	if !mutationEvidenceComplete(rows) {
		os.Exit(1)
	}
}

type result struct {
	index     int
	mutant    mutant
	target    string
	state     string
	test      string
	code      int
	log       string
	jointLog  string
	jointCode int
	bound     string
}

func validateEmulationRoot(path, platform, source string) string {
	if path == "" {
		fatalIf(fmt.Errorf("%s emulation root is required because %s is in the guarded package scope; materialize it with scripts/test-nondarwin-emu.sh", platform, source))
	}
	absolute, err := filepath.Abs(path)
	fatalIf(err)
	info, err := os.Stat(absolute)
	if err != nil {
		fatalIf(fmt.Errorf("%s emulation root is unavailable: %w", platform, err))
	}
	if !info.IsDir() {
		fatalIf(fmt.Errorf("%s emulation root is not a directory: %s", platform, absolute))
	}
	return absolute
}

func mutationTarget(root, source, linuxEmulationRoot, windowsEmulationRoot string) (testRoot, overlaySource, target string) {
	switch source {
	case "internal/infra/pi_shared_unsupported_posix.go":
		testRoot = linuxEmulationRoot
		target = "linux-emulated"
	case "internal/infra/pi_shared_api_unsupported.go":
		testRoot = linuxEmulationRoot
		target = "linux-emulated"
	case "internal/infra/pi_shared_unsupported_windows.go":
		testRoot = windowsEmulationRoot
		target = "windows-emulated"
	default:
		return root, filepath.Join(root, source), "darwin"
	}
	filename := filepath.Base(source)
	if strings.HasSuffix(filename, "_windows.go") {
		filename = strings.TrimSuffix(filename, ".go") + "_emu.go"
	}
	overlaySource = filepath.Join(testRoot, filepath.Dir(source), filename)
	if _, err := os.Stat(overlaySource); err != nil {
		fatalIf(fmt.Errorf("%s emulation source is unavailable: %w", target, err))
	}
	return testRoot, overlaySource, target
}

var goBuildDirective = regexp.MustCompile(`(?m)^//go:build[^\r\n]*`)

func darwinTaggedSource(source []byte) []byte {
	if !goBuildDirective.Match(source) {
		fatalIf(fmt.Errorf("emulated platform source has no //go:build directive"))
	}
	return goBuildDirective.ReplaceAll(source, []byte("//go:build darwin"))
}

func productionPackageScope(root string) ([]string, error) {
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
			return fmt.Errorf("lstat production source %s: %w", filepath.ToSlash(relative), err)
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
	if len(files) == 0 {
		return nil, fmt.Errorf("module packages contain no production Go files")
	}
	sort.Strings(files)
	return files, nil
}

func collectSites(root string, files []string) ([]refusalSite, error) {
	sites, sets, err := typedPropagationSites(root, files)
	if err != nil {
		return nil, err
	}
	tests, err := loadTests(root, files)
	if err != nil {
		return nil, err
	}
	mappings, err := readSiteMappings(root)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for i := range sites {
		known[sites[i].id] = true
		var mapping *siteMapping
		for j := range mappings {
			if mappings[j].Site == sites[i].id {
				if mapping != nil {
					return nil, fmt.Errorf("%s: duplicate certificate", sites[i].id)
				}
				mapping = &mappings[j]
			}
		}
		if mapping == nil {
			return nil, fmt.Errorf("%s: missing certificate or explicit bound", sites[i].id)
		}
		if err = validateSiteBounds(root, sites[i], *mapping); err != nil {
			return nil, err
		}
		sites[i].bounds = mapping.Bounds
		sites[i].witnesses = mapping.Witnesses
		if len(mapping.Tests) > 0 {
			sites[i].tests, err = mappedSiteTests(sites[i].id, mappings, tests)
			if err != nil {
				return nil, err
			}
		} else if !hasUndecidedBound(mapping.Bounds, -1) {
			return nil, fmt.Errorf("%s: missing named negative test", sites[i].id)
		}
		sites[i].expectedMutationKeys, err = expectedMutationKeys(sets[sites[i].file], sites[i])
		if err != nil {
			return nil, err
		}
	}
	for _, mapping := range mappings {
		if !known[mapping.Site] {
			return nil, fmt.Errorf("%s: stale certificate outside AST census", mapping.Site)
		}
	}

	return sites, nil
}

func productionCallers(root string, files []string) (map[string][]string, error) {
	callers := make(map[string][]string)
	for _, sourceName := range files {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, sourceName), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := ""
				switch target := call.Fun.(type) {
				case *ast.Ident:
					callee = target.Name
				case *ast.SelectorExpr:
					callee = target.Sel.Name
				}
				if callee != "" {
					callers[callee] = appendUnique(callers[callee], function.Name.Name)
				}
				return true
			})
		}
	}
	for _, functions := range callers {
		sort.Strings(functions)
	}
	return callers, nil
}

// Legacy selection helpers remain exercised by the original regression tests.
// The production inventory never invokes them: collectSites uses explicit
// file/function/ordinal certificates through mappedSiteTests.
func testsForRefusalSite(candidates []parsedTest, callers map[string][]string, site refusalSite) []testReference {
	if site.constructor == "availability attestation gate" {
		return testsForAvailabilityAttestationGate(candidates, callers, site)
	}
	if site.constructor == "catalog-first resolution gate" {
		return testsForCatalogFirstResolutionGate(candidates, site)
	}
	if site.constructor == "catalog read-failure propagation gate" {
		return testsForCatalogReadFailureGate(candidates, site)
	}
	functionName := strings.ToLower(site.function)
	if site.call == nil && site.constructor != "" {
		// Preserve direct helper tests, and also include named production-path
		// tests for typed literal refusals. A typed literal may be returned by
		// a lower-level function whose caller is exercised through a test helper,
		// so requiring the test body to call the owner directly would miss the
		// behavioral witness.
		witnesses := testsCallingFunction(candidates, site.function, "")
		witnesses = append(witnesses, testsNamingSite(candidates, site.function, site.site)...)
		if len(witnesses) != 0 {
			return uniqueSortedTestReferences(witnesses)
		}
	}
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate.reference.Name), functionName) {
			return testsNamingSite(candidates, site.function, site.site)
		}
	}
	direct := testsCallingFunction(candidates, site.function, site.site)
	if len(direct) != 0 {
		return direct
	}
	var mapped []testReference
	for _, caller := range callers[site.function] {
		callerTests := testsCallingFunction(candidates, caller, site.site)
		if len(callerTests) == 0 {
			callerTests = testsNamingSite(candidates, caller, site.site)
		}
		mapped = append(mapped, callerTests...)
	}
	if len(mapped) != 0 {
		return uniqueSortedTestReferences(mapped)
	}
	return testsNamingSite(candidates, site.function, site.site)
}

func testsForCatalogFirstResolutionGate(candidates []parsedTest, site refusalSite) []testReference {
	var mapped []testReference
	for _, candidate := range candidates {
		if testCallsFunction(candidate.body, site.function) && containsIdentifier(candidate.body, "CatalogEntry") {
			mapped = appendUniqueTest(mapped, candidate.reference)
		}
	}
	sortTestReferences(mapped)
	return mapped
}

func testsForCatalogReadFailureGate(candidates []parsedTest, site refusalSite) []testReference {
	var mapped []testReference
	for _, candidate := range candidates {
		if testCallsFunction(candidate.body, site.function) &&
			containsIdentifier(candidate.body, "EngineCatalogError") &&
			containsIdentifier(candidate.body, "EngineCatalogReadFailed") {
			mapped = appendUniqueTest(mapped, candidate.reference)
		}
	}
	sortTestReferences(mapped)
	return mapped
}

func testsForAvailabilityAttestationGate(candidates []parsedTest, callers map[string][]string, site refusalSite) []testReference {
	// Walk outward through production callers so an end-to-end CLI test can
	// witness a status field assigned by an infrastructure helper. The test
	// must both assert the availability field/value and call a reachable
	// production entry point; matching a test name or a copied string alone is
	// not behavioral evidence.
	reachable := map[string]bool{site.function: true}
	queue := []string{site.function}
	for len(queue) > 0 {
		callee := queue[0]
		queue = queue[1:]
		for _, caller := range callers[callee] {
			if !reachable[caller] {
				reachable[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	var mapped []testReference
	for _, candidate := range candidates {
		if !containsIdentifier(candidate.body, "EngineAvailability") || !containsString(candidate.body, site.site) {
			continue
		}
		for productionEntry := range reachable {
			if testCallsFunction(candidate.body, productionEntry) {
				mapped = appendUniqueTest(mapped, candidate.reference)
				break
			}
		}
	}
	sortTestReferences(mapped)
	return mapped
}

func testCallsFunction(body *ast.BlockStmt, functionName string) bool {
	called := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := ""
		switch target := call.Fun.(type) {
		case *ast.Ident:
			callee = target.Name
		case *ast.SelectorExpr:
			callee = target.Sel.Name
		}
		called = called || callee == functionName
		return !called
	})
	return called
}

func testsCallingFunction(candidates []parsedTest, functionName, site string) []testReference {
	var mapped []testReference
	for _, candidate := range candidates {
		callsFunction := false
		ast.Inspect(candidate.body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := ""
			switch target := call.Fun.(type) {
			case *ast.Ident:
				callee = target.Name
			case *ast.SelectorExpr:
				callee = target.Sel.Name
			}
			callsFunction = callsFunction || callee == functionName
			return true
		})
		if !callsFunction {
			continue
		}
		if site == "" {
			mapped = appendUniqueTest(mapped, candidate.reference)
			continue
		}
		subtests := matchingSubtestNames(candidate.body, site)
		if len(subtests) == 0 {
			if containsString(candidate.body, site) {
				mapped = appendUniqueTest(mapped, candidate.reference)
			}
			continue
		}
		for _, subtest := range subtests {
			child := candidate.reference
			child.Name += "/" + testNameSegment(subtest)
			mapped = appendUniqueTest(mapped, child)
		}
	}
	sortTestReferences(mapped)
	return mapped
}

func uniqueSortedTestReferences(references []testReference) []testReference {
	unique := make([]testReference, 0, len(references))
	for _, reference := range references {
		unique = appendUniqueTest(unique, reference)
	}
	sortTestReferences(unique)
	return unique
}

func receiverName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		if name, ok := value.X.(*ast.Ident); ok {
			return name.Name
		}
	}
	return ""
}

func nearestGuardForNode(parents []ast.Node, target ast.Node) (*ast.IfStmt, ast.Expr, bool) {
	for index := len(parents) - 1; index >= 0; index-- {
		branch, ok := parents[index].(*ast.IfStmt)
		if !ok {
			continue
		}
		if branch.Body != nil && target.Pos() >= branch.Body.Pos() && target.End() <= branch.Body.End() {
			return branch, branch.Cond, false
		}
		if branch.Else != nil && target.Pos() >= branch.Else.Pos() && target.End() <= branch.Else.End() {
			return branch, branch.Cond, true
		}
	}
	return nil, nil, false
}

func collectUnknownStatusGates(path string, fset *token.FileSet, file *ast.File, sites *[]refusalSite) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			branch, ok := node.(*ast.IfStmt)
			if !ok || !containsString(branch.Body, "catalog-engine-unavailable") {
				return true
			}
			position := fset.Position(branch.If)
			id := fmt.Sprintf("%s:%d:unknown-status:catalog-engine-unavailable", path, position.Offset)
			*sites = append(*sites, refusalSite{id: id, file: path, function: function.Name.Name, constructor: "unknown-status gate", site: "catalog-engine-unavailable", line: position.Line, guard: branch, guardExpr: branch.Cond})
			return true
		})
	}
}

func collectCatalogResolutionDecisionGates(path string, fset *token.FileSet, file *ast.File, sites *[]refusalSite) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		catalogReadErrors := catalogReadErrorIdentifiers(function.Body)
		var parents []ast.Node
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if node == nil {
				if len(parents) > 0 {
					parents = parents[:len(parents)-1]
				}
				return false
			}
			branch, ok := node.(*ast.IfStmt)
			if ok {
				if isEngineCatalogPresentCondition(branch.Cond) && containsCall(branch.Body, "resolveCatalogProfile") {
					position := fset.Position(branch.If)
					id := fmt.Sprintf("%s:%d:catalog-first:entry-present", path, position.Offset)
					*sites = append(*sites, refusalSite{
						id: id, file: path, function: function.Name.Name,
						constructor: "catalog-first resolution gate", site: "catalog_entry_present",
						line: position.Line, guard: branch, guardExpr: branch.Cond,
					})
				}
				if errorName, ok := notNilIdentifier(branch.Cond); ok && catalogReadErrors[errorName] && returnsIdentifier(branch.Body, errorName) {
					position := fset.Position(branch.If)
					id := fmt.Sprintf("%s:%d:catalog-read-failure:propagate", path, position.Offset)
					*sites = append(*sites, refusalSite{
						id: id, file: path, function: function.Name.Name,
						constructor: "catalog read-failure propagation gate", site: "catalog_read_failed",
						line: position.Line, guard: branch, guardExpr: branch.Cond,
					})
				}
			}
			parents = append(parents, node)
			return true
		})
	}
}

func catalogReadErrorIdentifiers(body *ast.BlockStmt) map[string]bool {
	identifiers := make(map[string]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for rhsIndex, right := range assignment.Rhs {
			if !containsCall(right, "ReadEngineCatalogEntry") {
				continue
			}
			resultIndex := rhsIndex + 2
			if resultIndex >= len(assignment.Lhs) {
				continue
			}
			if identifier, ok := assignment.Lhs[resultIndex].(*ast.Ident); ok {
				identifiers[identifier.Name] = true
			}
		}
		return true
	})
	return identifiers
}

func isEngineCatalogPresentCondition(expression ast.Expr) bool {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.EQL {
		return false
	}
	return (identifierNamed(binary.X, "state") && identifierNamed(binary.Y, "EngineCatalogPresent")) ||
		(identifierNamed(binary.Y, "state") && identifierNamed(binary.X, "EngineCatalogPresent"))
}

func identifierNamed(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func notNilIdentifier(expression ast.Expr) (string, bool) {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.NEQ {
		return "", false
	}
	if identifier, ok := binary.X.(*ast.Ident); ok && identifierNamed(binary.Y, "nil") {
		return identifier.Name, true
	}
	if identifier, ok := binary.Y.(*ast.Ident); ok && identifierNamed(binary.X, "nil") {
		return identifier.Name, true
	}
	return "", false
}

func returnsIdentifier(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		returned, ok := child.(*ast.ReturnStmt)
		if !ok || found {
			return !found
		}
		for _, result := range returned.Results {
			if identifierContains(result, name) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func identifierContains(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		identifier, ok := child.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func containsCall(node ast.Node, functionName string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		call, ok := child.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := ""
		switch target := call.Fun.(type) {
		case *ast.Ident:
			callee = target.Name
		case *ast.SelectorExpr:
			callee = target.Sel.Name
		}
		found = found || callee == functionName
		return !found
	})
	return found
}

func collectCatalogAvailabilityAttestationGates(path string, fset *token.FileSet, file *ast.File, sites *[]refusalSite) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		var parents []ast.Node
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if node == nil {
				if len(parents) > 0 {
					parents = parents[:len(parents)-1]
				}
				return false
			}
			assignment, ok := node.(*ast.AssignStmt)
			if ok {
				for index, left := range assignment.Lhs {
					if index >= len(assignment.Rhs) {
						continue
					}
					selector, ok := left.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "EngineAvailability" || !stringLiteralEquals(assignment.Rhs[index], "available") {
						continue
					}
					guard, guardExpr, elseBranch := nearestGuardForNode(parents, assignment)
					if guard == nil || elseBranch {
						continue
					}
					position := fset.Position(assignment.Pos())
					id := fmt.Sprintf("%s:%d:availability-attestation:available", path, position.Offset)
					*sites = append(*sites, refusalSite{
						id: id, file: path, function: function.Name.Name,
						constructor: "availability attestation gate", site: "available",
						line: position.Line, guard: guard, guardExpr: guardExpr,
					})
				}
			}
			parents = append(parents, node)
			return true
		})
	}
}

func stringLiteralEquals(expression ast.Expr, wanted string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == wanted
}

func containsString(node ast.Node, wanted string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		literal, ok := child.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == wanted {
			found = true
		}
		return !found
	})
	return found
}

func loadTests(root string, changedFiles []string) ([]parsedTest, error) {
	var testDirectories []string
	for _, changed := range changedFiles {
		directory := filepath.ToSlash(filepath.Dir(changed))
		testDirectories = appendUnique(testDirectories, directory)
	}
	var candidates []parsedTest
	for _, directory := range testDirectories {
		paths, err := filepath.Glob(filepath.Join(root, directory, "*_test.go"))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if parseErr != nil {
				return nil, parseErr
			}
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") || function.Body == nil {
					continue
				}
				candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: directory}, body: function.Body, bodyDigest: testBodyDigest(function.Body)})
			}
		}
	}
	return candidates, nil
}

func testsNamingSite(candidates []parsedTest, functionName, site string) []testReference {
	fullFunctionName := strings.ToLower(functionName)
	functionWords := meaningfulWords(functionName)
	var siteSpecific []testReference
	for _, candidate := range candidates {
		// Exact owning-function names take precedence here: shared words such
		// as "attest" must not pull in another production path's tests.
		if !strings.Contains(strings.ToLower(candidate.reference.Name), fullFunctionName) {
			continue
		}
		if strings.HasPrefix(site, "type:") {
			typeName := strings.TrimPrefix(site, "type:")
			if containsIdentifier(candidate.body, typeName) {
				siteSpecific = appendUniqueTest(siteSpecific, candidate.reference)
			}
			continue
		}
		matchingNames := matchingSubtestNames(candidate.body, site)
		for _, name := range matchingNames {
			child := candidate.reference
			child.Name += "/" + testNameSegment(name)
			siteSpecific = appendUniqueTest(siteSpecific, child)
		}
		if len(matchingNames) == 0 && containsString(candidate.body, site) {
			siteSpecific = appendUniqueTest(siteSpecific, candidate.reference)
		}
	}
	if len(siteSpecific) > 0 {
		sortTestReferences(siteSpecific)
		return siteSpecific
	}
	var namedFunction []testReference
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate.reference.Name), fullFunctionName) {
			namedFunction = appendUniqueTest(namedFunction, candidate.reference)
		}
	}
	if len(namedFunction) > 0 {
		sortTestReferences(namedFunction)
		return namedFunction
	}
	var exact []testReference
	for _, candidate := range candidates {
		if containsString(candidate.body, site) {
			exact = appendUniqueTest(exact, candidate.reference)
		}
	}
	if len(exact) > 0 {
		// Literal site names are useful only when the test also names the
		// production function that owns the refusal. A different subsystem may
		// mention the same error code while exercising a helper or a mock.
		var narrowed []testReference
		for _, candidate := range exact {
			if testNameMatches(candidate.Name, functionWords) {
				narrowed = appendUniqueTest(narrowed, candidate)
			}
		}
		if len(narrowed) > 0 {
			return narrowed
		}
		// Exact error-code text alone can point at unrelated tests that only
		// happen to mention the same code. Keep looking for named tests of the
		// owning production function instead of treating that coincidence as
		// behavioral coverage.
		exact = nil
	}
	// A subtable may name a member (for example "engine") and construct the
	// full site key at runtime. Bind that test only when its name also shares
	// a meaningful word with the production function owning the refusal.
	parts := strings.Split(site, ".")
	leaf := strings.ToLower(parts[len(parts)-1])
	for _, candidate := range candidates {
		if testNameMatches(candidate.reference.Name, functionWords) && (strings.Contains(strings.ToLower(candidate.reference.Name), leaf) || containsString(candidate.body, leaf)) {
			exact = appendUniqueTest(exact, candidate.reference)
		}
	}
	if len(exact) == 0 {
		for _, candidate := range candidates {
			if testNameMatches(candidate.reference.Name, functionWords) {
				exact = appendUniqueTest(exact, candidate.reference)
			}
		}
	}
	sortTestReferences(exact)
	return exact
}

func matchingSubtestNames(node ast.Node, site string) []string {
	var names []string
	var walk func(ast.Node, []string)
	walk = func(current ast.Node, parents []string) {
		ast.Inspect(current, func(child ast.Node) bool {
			if call, ok := child.(*ast.CallExpr); ok && isTestRunCall(call) && len(call.Args) >= 2 {
				callback, ok := call.Args[1].(*ast.FuncLit)
				if !ok || callback.Body == nil {
					return false
				}
				name, staticName := stringLiteral(call.Args[0])
				if staticName {
					path := append(append([]string(nil), parents...), testNameSegment(name))
					if containsTestBodyString(callback.Body, site) {
						names = appendUnique(names, strings.Join(path, "/"))
					}
					walk(callback.Body, path)
				} else {
					walk(callback.Body, parents)
				}
				return false
			}
			literal, ok := child.(*ast.CompositeLit)
			if !ok {
				return true
			}
			name := ""
			matchesSite := false
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok {
					continue
				}
				text, ok := stringLiteral(field.Value)
				if !ok {
					continue
				}
				if key.Name == "name" {
					name = text
				} else if text == site {
					matchesSite = true
				}
			}
			if name != "" && matchesSite {
				path := append(append([]string(nil), parents...), testNameSegment(name))
				names = appendUnique(names, strings.Join(path, "/"))
			}
			return true
		})
	}
	walk(node, nil)
	return names
}

func containsTestBodyString(node ast.Node, wanted string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		if call, ok := child.(*ast.CallExpr); ok && isTestRunCall(call) {
			return false
		}
		if _, ok := child.(*ast.CompositeLit); ok {
			return false
		}
		literal, ok := child.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == wanted {
			found = true
		}
		return !found
	})
	return found
}

func isTestRunCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Run"
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func containsIdentifier(node ast.Node, wanted string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		identifier, ok := child.(*ast.Ident)
		if ok && identifier.Name == wanted {
			found = true
		}
		return !found
	})
	return found
}

func testNameSegment(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

func sortTestReferences(references []testReference) {
	sort.Slice(references, func(i, j int) bool {
		if references[i].Dir != references[j].Dir {
			return references[i].Dir < references[j].Dir
		}
		return references[i].Name < references[j].Name
	})
}

func meaningfulWords(name string) []string {
	var words []string
	// Retain the full receiver-free function name as well as useful words so
	// tests named TestResolveSharedProfile... and TestReadPiProfiles... remain
	// discoverable even when every individual word is intentionally generic.
	words = appendUnique(words, strings.ToLower(name))
	start := 0
	for index := 1; index < len(name); index++ {
		if name[index] >= 'A' && name[index] <= 'Z' {
			word := strings.ToLower(name[start:index])
			if len(word) >= 4 && word != "read" && word != "resolve" && word != "shared" && word != "runtime" && word != "entry" && word != "catalog" && word != "profile" && word != "with" && word != "from" {
				words = appendUnique(words, word)
			}
			start = index
		}
	}
	word := strings.ToLower(name[start:])
	if len(word) >= 4 && word != "read" && word != "resolve" && word != "shared" && word != "runtime" && word != "entry" && word != "catalog" && word != "profile" && word != "with" && word != "from" {
		words = appendUnique(words, word)
	}
	return words
}

func testNameMatches(name string, words []string) bool {
	lower := strings.ToLower(name)
	for _, word := range words {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func appendUniqueTest(values []testReference, additions ...testReference) []testReference {
	for _, value := range additions {
		found := false
		for _, current := range values {
			if current == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}

func deriveMutants(root string, sites []refusalSite) ([]mutant, error) {
	var mutants []mutant
	sets := map[string]*token.FileSet{}
	sources := map[string][]byte{}
	for _, site := range sites {
		if len(site.witnesses) == 0 && hasUndecidedBound(site.bounds, -1) {
			continue
		}
		fset, source, err := sourceFor(root, site.file, sets, sources)
		if err != nil {
			return nil, err
		}
		if site.fset != nil {
			fset = site.fset
		}
		keys, err := expectedMutationKeys(fset, site)
		if err != nil {
			return nil, err
		}
		if site.unconditional && len(site.witnesses) == 0 {
			if site.expression == nil {
				return nil, fmt.Errorf("%s: unconditional site has no refusal expression", site.id)
			}
			target := site.expression
			start, end := fset.Position(target.Pos()).Offset, fset.Position(target.End()).Offset
			mutants = append(mutants, mutant{bounds: site.bounds, key: keys[0], siteID: site.id, file: site.file, function: site.function, constructor: site.constructor, sites: []string{site.site}, tests: site.tests, line: site.line, guardText: string(source[start:end]), memberText: "unconditional: AC1 + removal mutant + consumer narrowing", replaceText: "func() error { _ = (" + string(source[start:end]) + "); return nil }()", start: start, end: end})
			continue
		}
		for index, witness := range site.witnesses {
			exception, err := singletonPredicate(witness.Admit)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", site.id, err)
			}
			if err := typedWitnessMember(site, witness); err != nil {
				return nil, err
			}
			target := site.guardExpr
			if target == nil {
				target = site.expression
			}
			start, end := fset.Position(target.Pos()).Offset, fset.Position(target.End()).Offset
			original := string(source[start:end])
			member := original
			if site.guardExpr != nil {
				groups := rejectedGroups(site.guardExpr)
				if witness.Member < 0 || witness.Member >= len(groups) {
					return nil, fmt.Errorf("%s: witness member outside condition class", site.id)
				}
				member = expressionText(fset, groups[witness.Member])
				if site.elseBranch {
					member = "!(" + member + ")"
				}
				// An OR witness must satisfy exactly the selected rejected group.
				prefix := []string{}
				for other, group := range groups {
					if other != witness.Member {
						prefix = append(prefix, "!("+expressionText(fset, group)+")")
					}
				}
				predicate, _ := parser.ParseExpr(exception)
				needsSelected := false
				ast.Inspect(predicate, func(node ast.Node) bool {
					switch e := node.(type) {
					case *ast.CallExpr:
						if method, ok := e.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "Error" {
							needsSelected = true
						}
					case *ast.IndexExpr:
						needsSelected = true
					}
					return true
				})
				if needsSelected {
					prefix = append(prefix, "("+member+")")
				}
				prefix = append(prefix, "("+exception+")")
				exception = strings.Join(prefix, " && ")
			} else if witness.Member != 0 {
				return nil, fmt.Errorf("%s: unconditional refusal has one class", site.id)
			}
			replacement := "(" + original + ") && !(" + exception + ")"
			if site.elseBranch {
				replacement = "(" + original + ") || (" + exception + ")"
			}
			if site.guardExpr == nil {
				replacement = "func() error { if !(" + exception + ") {return (" + original + ")}; return nil }()"
			}
			mutants = append(mutants, mutant{bounds: site.bounds, key: keys[index], siteID: site.id, file: site.file, function: site.function, constructor: site.constructor, sites: []string{site.site}, tests: site.tests, line: site.line, guardText: original, memberText: member + "; admit exactly " + exception, replaceText: replacement, start: start, end: end})
		}
	}
	sort.Slice(mutants, func(i, j int) bool { return mutants[i].key < mutants[j].key })
	if len(mutants) == 0 {
		return nil, fmt.Errorf("no certified narrowing mutants")
	}
	return mutants, nil
}
func expressionText(set *token.FileSet, expr ast.Expr) string {
	var out strings.Builder
	fatalIf(format.Node(&out, set, expr))
	return out.String()
}
func rejectedGroups(expr ast.Expr) []ast.Expr {
	expr = unparen(expr)
	if binary, ok := expr.(*ast.BinaryExpr); ok && binary.Op == token.LOR {
		return append(rejectedGroups(binary.X), rejectedGroups(binary.Y)...)
	}
	return []ast.Expr{expr}
}

// The frozen witness grammar admits one equality, never an observation or a
// conjunction. Types and exact guarded-expression identity are checked below.
func singletonPredicate(source string) (string, error) {
	expr, err := parser.ParseExpr(source)
	if err != nil {
		return "", err
	}
	if _, _, ok := singletonShape(expr); !ok {
		return "", fmt.Errorf("narrowing witness must be singleton equality: identical guarded expression == constant literal, or deep equality with a fully specified composite literal")
	}
	return expressionText(token.NewFileSet(), expr), nil
}

func singletonShape(expr ast.Expr) (operand, literal ast.Expr, ok bool) {
	switch value := unparen(expr).(type) {
	case *ast.BinaryExpr:
		if value.Op == token.EQL && singletonOperand(value.X) && constantLiteral(value.Y) {
			return value.X, value.Y, true
		}
	case *ast.CallExpr:
		// The callee is additionally resolved to standard reflect.DeepEqual by types.
		if callee, yes := value.Fun.(*ast.SelectorExpr); yes && callee.Sel.Name == "DeepEqual" && len(value.Args) == 2 && singletonOperand(value.Args[0]) {
			if _, composite := unparen(value.Args[1]).(*ast.CompositeLit); composite {
				return value.Args[0], value.Args[1], true
			}
		}
	}
	return nil, nil, false
}

func constantLiteral(expr ast.Expr) bool {
	switch value := unparen(expr).(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return value.Name == "true" || value.Name == "false"
	case *ast.UnaryExpr:
		_, literal := value.X.(*ast.BasicLit)
		return literal && (value.Op == token.SUB || value.Op == token.ADD)
	}
	return false
}

func singletonOperand(expr ast.Expr) bool {
	switch value := unparen(expr).(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return singletonOperand(value.X)
	}
	return false // len, indexing, slicing, methods and other observations refuse.
}
func expectedMutationKeys(fset *token.FileSet, site refusalSite) ([]string, error) {
	if site.unconditional && len(site.witnesses) > 0 {
		return nil, fmt.Errorf("%s: unconditional refusal cannot invent narrowing inputs", site.id)
	}
	if len(site.witnesses) == 0 && hasUndecidedBound(site.bounds, -1) {
		return nil, nil
	}
	if len(site.witnesses) == 0 && site.unconditional && site.expression != nil {
		return []string{site.id + ":removal"}, nil
	}
	if len(site.witnesses) == 0 {
		return nil, fmt.Errorf("%s:%d %s: no explicit singleton witness for typed refusal site", site.file, site.line, site.id)
	}
	var keys []string
	for i := range site.witnesses {
		keys = append(keys, fmt.Sprintf("%s:witness:%d", site.id, i+1))
	}
	return keys, nil
}

func sourceFor(root, path string, sets map[string]*token.FileSet, sources map[string][]byte) (*token.FileSet, []byte, error) {
	if sets[path] != nil {
		return sets[path], sources[path], nil
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, filepath.Join(root, path), nil, 0); err != nil {
		return nil, nil, err
	}
	source, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return nil, nil, err
	}
	sets[path] = fset
	sources[path] = source
	return fset, source, nil
}

func runMutants(root, out string, mutants []mutant, start, workers int, linuxEmulationRoot, windowsEmulationRoot string) []result {
	if workers > len(mutants) {
		workers = len(mutants)
	}
	rows := make([]result, len(mutants))
	jobs := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				rows[index] = runMutant(root, out, mutants[index], start+index, linuxEmulationRoot, windowsEmulationRoot)
			}
		}()
	}
	for index := range mutants {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	// Only two independently surviving guards can establish a joint bound.
	// A downstream guard that already fails alone cannot be an equivalence proof.
	for i := range rows {
		if rows[i].state == "survived" {
			for _, b := range rows[i].mutant.bounds {
				if b.Kind != "defensive-unreachable" {
					continue
				}
				for j := range rows {
					if rows[j].mutant.key != b.DownstreamKey {
						continue
					}
					proof, ok := proveDefensiveBound(root, out, rows[i], rows[j].mutant, b, start+i, linuxEmulationRoot, windowsEmulationRoot)
					if ok {
						rows[i] = proof
					}
					break
				}
			}
		}
		if rows[i].state != "survived" {
			continue
		}
		for j := range rows {
			if rows[j].state != "survived" || !downstreamOf(rows[i].mutant, rows[j].mutant) {
				continue
			}
			joint := rows[i].mutant
			joint.extra = []sourceMutation{{rows[j].mutant.start, rows[j].mutant.end, rows[j].mutant.replaceText}}
			proof := runMutant(root, filepath.Join(out, "joint"), joint, start+i, linuxEmulationRoot, windowsEmulationRoot)
			if provedJointEquivalence(rows[i], rows[j], proof) {
				rows[i].jointLog = proof.log
				rows[i].jointCode = proof.code
				rows[i].state = "equivalent"
				rows[i].test = proof.test
				rows[i].bound = fmt.Sprintf("both individual mutations survive; joint with %s fails %s (exit %d; log %s). Bound: mapped witnesses only, not universal semantic equivalence", rows[j].mutant.key, proof.test, proof.code, proof.log)
				break
			}
		}
	}
	for i := range rows {
		if rows[i].state == "survived" {
			for _, b := range rows[i].mutant.bounds {
				if b.Kind == "undecided" && (b.Member == -1 || strings.HasSuffix(rows[i].mutant.key, fmt.Sprintf(":witness:%d", b.Member+1))) {
					rows[i].state = "bound: undecided"
					rows[i].bound = b.Reason
					break
				}
			}
		}
	}
	return rows
}

func runMutant(root, out string, current mutant, id int, linuxEmulationRoot, windowsEmulationRoot string) result {
	absoluteSource := filepath.Join(root, current.file)
	source, err := os.ReadFile(absoluteSource)
	fatalIf(err)
	edits := append([]sourceMutation{{current.start, current.end, current.replaceText}}, current.extra...)
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	mutatedSource := append([]byte(nil), source...)
	boundary := len(source)
	for _, edit := range edits {
		if edit.start < 0 || edit.end < edit.start || edit.end > boundary {
			fatalIf(fmt.Errorf("overlapping or invalid mutation spans"))
		}
		mutatedSource = append(append(append([]byte(nil), mutatedSource[:edit.start]...), []byte(edit.replacement)...), mutatedSource[edit.end:]...)
		boundary = edit.start
	}
	if _, err := parser.ParseFile(token.NewFileSet(), absoluteSource, mutatedSource, 0); err != nil {
		fatalIf(fmt.Errorf("generated mutant did not parse (%s:%d): %w", current.file, current.line, err))
	}
	mutantDir := filepath.Join(out, "generated", fmt.Sprintf("%03d", id))
	fatalIf(os.MkdirAll(mutantDir, 0o700))
	testRoot, overlaySource, target := mutationTarget(root, current.file, linuxEmulationRoot, windowsEmulationRoot)
	if target != "darwin" {
		mutatedSource = darwinTaggedSource(mutatedSource)
		if _, err := parser.ParseFile(token.NewFileSet(), overlaySource, mutatedSource, 0); err != nil {
			fatalIf(fmt.Errorf("generated %s emulation mutant did not parse (%s:%d): %w", target, current.file, current.line, err))
		}
	}
	mutantPath := filepath.Join(mutantDir, filepath.Base(overlaySource))
	fatalIf(os.WriteFile(mutantPath, mutatedSource, 0o600))
	overlayPath := filepath.Join(mutantDir, "overlay.json")
	overlayBytes, err := json.Marshal(overlay{Replace: map[string]string{overlaySource: mutantPath}})
	fatalIf(err)
	fatalIf(os.WriteFile(overlayPath, overlayBytes, 0o600))
	patterns := testsByDirectory(current.tests)
	row := result{index: id, mutant: current, target: target, state: "unmapped", code: -1, bound: "no named behavioral test contains the enumerated refusal site", log: filepath.Join(mutantDir, "go-test.log")}
	if len(patterns) == 0 {
		return row
	}
	identity, err := bindMutationTestRoot(root, testRoot, current.tests)
	if err != nil {
		row.state, row.bound = "invalid", err.Error()
		fatalIf(os.WriteFile(row.log, []byte(row.bound+"\n"), 0600))
		return row
	}
	baseline := selectedBaseline(testRoot, identity, patterns)
	fatalIf(os.WriteFile(filepath.Join(mutantDir, "baseline.log"), baseline.output, 0600))
	if baseline.code != 0 {
		row.state, row.code, row.bound = "invalid", baseline.code, "selected baseline failed before mutation; no kill evidence"
		fatalIf(os.WriteFile(row.log, baseline.output, 0600))
		return row
	}
	directories := make([]string, 0, len(patterns))
	for directory := range patterns {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	var combined []byte
	var failed error
	for _, directory := range directories {
		for _, pattern := range testPatterns(patterns[directory]) {
			packagePath := "./" + directory
			command := exec.Command("go", "test", "-timeout=45s", "-overlay="+overlayPath, packagePath, "-run", pattern, "-count=1")
			command.Dir = testRoot
			combined = append(combined, []byte("target "+target+"\n")...)
			output, runErr := command.CombinedOutput()
			combined = append(combined, []byte("package "+packagePath+" run "+pattern+"\n")...)
			combined = append(combined, output...)
			if runErr != nil {
				failed = runErr
				break
			}
		}
		if failed != nil {
			break
		}
	}
	fatalIf(os.WriteFile(row.log, combined, 0o600))
	row.test = failingTest(combined)
	if failed == nil {
		row.code = 0
		if bound, witness, equivalent := equivalentNarrowing(current); equivalent {
			row.state = "equivalent"
			row.bound = bound
			row.test = witness
		} else {
			row.state = "survived"
			row.bound = "mapped behavioral suite did not expose this narrowing mutation"
		}
	} else if exit, ok := failed.(*exec.ExitError); ok {
		row.code = exit.ExitCode()
		if row.test != "" && containsTest(current.tests, row.test) {
			row.state = "killed"
			row.bound = ""
		} else {
			row.state = "invalid"
			row.bound = "go test failed without a named behavioral test failure; inspect the attached log"
		}
	} else {
		row.state = "invalid"
		row.bound = failed.Error()
		row.code = -1
	}
	return row
}

// The emulator may change build directives and platform filename suffixes,
// never declarations. Bind the whole package Go-file set (tests, helpers and
// sources), rather than trusting a test's name in an arbitrary directory.
func mutationRootIdentity(root string, references []testReference) (map[string]string, error) {
	identity := map[string]string{}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if os.IsNotExist(err) && name == "go.sum" {
			continue
		}
		if err != nil {
			return nil, err
		}
		identity[name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	directories := testsByDirectory(references)
	files, err := productionPackageScope(root)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		directories[filepath.ToSlash(filepath.Dir(file))] = nil
	}
	for directory := range directories {
		paths, err := filepath.Glob(filepath.Join(root, directory, "*.go"))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			stat, err := os.Lstat(path)
			if err != nil || !stat.Mode().IsRegular() {
				return nil, fmt.Errorf("test root non-regular or unreadable Go file: %s (%v)", path, err)
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return nil, err
			}
			var body strings.Builder
			if err := format.Node(&body, token.NewFileSet(), file); err != nil {
				return nil, err
			}
			name := filepath.Base(path)
			for _, platform := range []string{"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows"} {
				name = strings.ReplaceAll(name, "_"+platform+"_emu", "_"+platform)
			}
			key := filepath.ToSlash(filepath.Join(directory, name))
			if _, exists := identity[key]; exists {
				return nil, fmt.Errorf("test root duplicate Go identity: %s", key)
			}
			identity[key] = fmt.Sprintf("%x", sha256.Sum256([]byte(body.String())))
		}
	}
	return identity, nil
}

func bindMutationTestRoot(primary, execution string, references []testReference) (string, error) {
	want, err := mutationRootIdentity(primary, references)
	if err != nil {
		return "", err
	}
	got, err := mutationRootIdentity(execution, references)
	if err != nil {
		return "", err
	}
	if len(want) != len(got) {
		return "", fmt.Errorf("mutation test root identity mismatch: Go file set changed")
	}
	for file, digest := range want {
		if got[file] != digest {
			return "", fmt.Errorf("mutation test root identity mismatch: %s", file)
		}
	}
	data, err := json.Marshal(got)
	return fmt.Sprintf("%x", sha256.Sum256(data)), err
}

type baselineEvidence struct {
	code   int
	output []byte
}
type baselineRun struct {
	ready    chan struct{}
	evidence baselineEvidence
}

var mutationBaselines sync.Map

// Reuse only an exact content identity, execution root and selected test set.
func selectedBaseline(root, identity string, patterns map[string][]string) baselineEvidence {
	encoded, err := json.Marshal(patterns)
	fatalIf(err)
	key := root + ":" + identity + ":" + string(encoded)
	run := &baselineRun{ready: make(chan struct{})}
	prior, loaded := mutationBaselines.LoadOrStore(key, run)
	if loaded {
		run = prior.(*baselineRun)
		<-run.ready
		return run.evidence
	}
	defer close(run.ready)
	var directories []string
	for directory := range patterns {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		for _, pattern := range testPatterns(patterns[directory]) {
			command := exec.Command("go", "test", "-timeout=45s", "./"+directory, "-run", pattern, "-count=1")
			command.Dir = root
			output, err := command.CombinedOutput()
			code := 0
			if err != nil {
				code = -1
				if command.ProcessState != nil {
					code = command.ProcessState.ExitCode()
				}
			}
			run.evidence.output = append(run.evidence.output, []byte(fmt.Sprintf("baseline package ./%s run %s exit %d\n", directory, pattern, code))...)
			run.evidence.output = append(run.evidence.output, output...)
			if code != 0 {
				run.evidence.code = code
				return run.evidence
			}
		}
	}
	return run.evidence
}

// Legacy callers cannot attest equivalence without executed joint evidence.
func equivalentNarrowing(current mutant) (bound, witness string, equivalent bool) {
	return "no executed joint primary-plus-downstream mutation", "", false
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsTest(values []testReference, target string) bool {
	for _, value := range values {
		if value.Name == target || strings.HasPrefix(value.Name, target+"/") {
			return true
		}
	}
	return false
}

func testsByDirectory(references []testReference) map[string][]string {
	byDirectory := make(map[string][]string)
	for _, reference := range references {
		byDirectory[reference.Dir] = appendUnique(byDirectory[reference.Dir], reference.Name)
	}
	return byDirectory
}

func testPatterns(names []string) []string {
	wholeTests := map[string]bool{}
	children := map[string][]string{}
	for _, name := range names {
		parts := strings.Split(name, "/")
		if len(parts) == 1 {
			wholeTests[parts[0]] = true
			continue
		}
		prefix := strings.Join(parts[:len(parts)-1], "/")
		children[prefix] = appendUnique(children[prefix], parts[len(parts)-1])
	}
	patterns := make([]string, 0, len(wholeTests)+len(children))
	for name := range wholeTests {
		patterns = append(patterns, "^"+regexp.QuoteMeta(name)+"$")
	}
	for prefix, leaves := range children {
		sort.Strings(leaves)
		parts := strings.Split(prefix, "/")
		quoted := make([]string, 0, len(parts)+1)
		for _, part := range parts {
			quoted = append(quoted, "^"+regexp.QuoteMeta(part)+"$")
		}
		last := make([]string, 0, len(leaves))
		for _, leaf := range leaves {
			last = append(last, regexp.QuoteMeta(leaf))
		}
		quoted = append(quoted, "^("+strings.Join(last, "|")+")$")
		patterns = append(patterns, strings.Join(quoted, "/"))
	}
	sort.Strings(patterns)
	return patterns
}

var failedTest = regexp.MustCompile(`(?m)^--- FAIL: (Test[[:alnum:]_]+(/[[:alnum:]_]+)*)`)

func failingTest(output []byte) string {
	if strings.Contains(string(output), "panic: test timed out") {
		return ""
	}
	match := failedTest.FindSubmatch(output)
	if len(match) >= 2 {
		return string(match[1])
	}
	return ""
}

func renderTable(siteCount, mutantCount int, rows []result) string {
	var builder strings.Builder
	builder.WriteString("# TASK-260930-1hzscs mutation table\n\n")
	fmt.Fprintf(&builder, "Typed refusal and status-gate sites mapped across the package-derived AST guard scope: %d. AST-derived narrowing mutants: %d.\n\n", siteCount, mutantCount)
	builder.WriteString("| Mutant | Narrowing applied to | Named failing test / equivalence witness | Target | Exit | Result or bound | Log |\n| --- | --- | --- | --- | ---: | --- | --- |\n")
	for _, row := range rows {
		what := "replace typed refusal result with nil"
		if row.mutant.memberText != "" {
			what = fmt.Sprintf("certified `%s` at line %d", markdownCell(row.mutant.memberText), row.mutant.line)
		}
		testNames := make([]string, len(row.mutant.tests))
		for index, reference := range row.mutant.tests {
			testNames[index] = reference.Dir + ":" + reference.Name
		}
		testEvidence := "mapped: " + strings.Join(testNames, ", ")
		if row.test != "" {
			switch row.state {
			case "killed":
				testEvidence = "fails: " + row.test
			case "equivalent":
				testEvidence = "witness: " + row.test
			}
		}
		fmt.Fprintf(&builder, "| M%03d `%s` | %s in `%s` (`%s`; sites %s) | %s | %s | %d | %s | `%s` |\n",
			row.index, row.mutant.key, what, markdownCell(row.mutant.guardText), row.mutant.constructor, markdownCell(strings.Join(row.mutant.sites, ", ")), markdownCell(testEvidence), row.target, row.code, markdownCell(row.state+boundSuffix(row.bound)), row.log)
	}
	return builder.String()
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.ReplaceAll(value, "|", "\\|")
}

func boundSuffix(bound string) string {
	if bound == "" {
		return ""
	}
	return ": " + bound
}

func appendUnique(values []string, additions ...string) []string {
	for _, value := range additions {
		if value == "" {
			continue
		}
		found := false
		for _, current := range values {
			if current == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

const propagationSubset = "P1: direct error return or returning if-init; P2: send to error-implementing channel element; P3: enclosing function named error result (including its direct deferred closure); P4: module wrapper returns the incoming error parameter or standard errors.Join including it on every return, and its result propagates under P1-P3 (no transitive wrappers, interface/function-value calls or fmt.Errorf). Other producer uses and non-regular .go entries refuse. Reflection and generics-instantiated constructors are out of contract. Unconditional: AC1 + removal mutant + consumer narrowing. R4 frozen: singleton equality witness (identical guarded variable/selector == typed constant literal; non-comparable types require standard reflect.DeepEqual with a fully specified composite literal). Calls/len/index/slice/method/range/inequality/conjunction witnesses refuse. Bounds: defensive-unreachable requires a green dependency pin plus a named joint-mutant kill; undecided bounds name exact evidence gaps for review (not killed or equivalent)."

type listedPackage struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
	Standard                bool
}

type typedUnit struct {
	pkg          *types.Package
	set          *token.FileSet
	files        map[string]*ast.File
	info         *types.Info
	constructors map[types.Object]bool
	errorTypes   map[*types.Named]bool
	bodies       map[types.Object]*ast.FuncDecl
}

// Platform contexts resolve objects, not names. Every walked source must occur
// in a checked context; an unresolvable context is a failure, never absence.
func typedUnits(root string, files []string) ([]typedUnit, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root = canonical
	selected := map[string]bool{}
	for _, file := range files {
		selected[file] = true
	}
	covered := map[string]bool{}
	var units []typedUnit
	for _, platform := range []string{"darwin", "linux", "windows"} {
		command := exec.Command("go", "list", "-deps", "-export", "-json", "./...")
		command.Dir = root
		command.Env = append(os.Environ(), "GOOS="+platform, "CGO_ENABLED=0", "PWD="+root)
		data, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("type context %s: %w", platform, err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		var packages []listedPackage
		exports := map[string]string{}
		for {
			var pkg listedPackage
			if err := decoder.Decode(&pkg); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			packages = append(packages, pkg)
			if pkg.Export != "" {
				exports[pkg.ImportPath] = pkg.Export
			}
		}
		// Explicit ignore-tag commands have their own package context. Never
		// omit them from the source census or mix their declarations together.
		for _, file := range files {
			if !standaloneIgnoreCommand(root, file) {
				continue
			}
			command := exec.Command("go", "list", "-deps", "-export", "-json", file)
			command.Dir = root
			command.Env = append(os.Environ(), "GOOS="+platform, "CGO_ENABLED=0", "PWD="+root)
			data, err := command.Output()
			if err != nil {
				return nil, fmt.Errorf("standalone type context %s %s: %w", platform, file, err)
			}
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			for {
				var pkg listedPackage
				if err := decoder.Decode(&pkg); err == io.EOF {
					break
				} else if err != nil {
					return nil, err
				}
				if pkg.Export != "" {
					exports[pkg.ImportPath] = pkg.Export
				}
				if pkg.ImportPath == "command-line-arguments" {
					packages = append(packages, pkg)
				}
			}
		}
		for _, pkg := range packages {
			canonicalDir, err := filepath.EvalSymlinks(pkg.Dir)
			if err != nil {
				return nil, fmt.Errorf("resolve package directory: %w", err)
			}
			pkg.Dir = canonicalDir
			if !within(root, pkg.Dir) {
				continue
			}
			unit := typedUnit{set: token.NewFileSet(), files: map[string]*ast.File{}, info: &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}, constructors: map[types.Object]bool{}, errorTypes: map[*types.Named]bool{}}
			var syntax []*ast.File
			fresh := false
			for _, name := range pkg.GoFiles {
				path := filepath.Join(pkg.Dir, name)
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return nil, err
				}
				relative = filepath.ToSlash(relative)
				if !selected[relative] {
					continue
				}
				parsed, err := parser.ParseFile(unit.set, path, nil, 0)
				if err != nil {
					return nil, err
				}
				unit.files[relative] = parsed
				syntax = append(syntax, parsed)
				fresh = fresh || !covered[relative]
			}
			if len(syntax) == 0 || !fresh {
				continue
			}
			imp := importer.ForCompiler(unit.set, "gc", func(path string) (io.ReadCloser, error) {
				if exports[path] == "" {
					return nil, fmt.Errorf("missing export for %s", path)
				}
				return os.Open(exports[path])
			})
			config := types.Config{Importer: imp}
			checked, err := config.Check(pkg.ImportPath, unit.set, syntax, unit.info)
			if err != nil {
				return nil, fmt.Errorf("typed inventory %s: %w", platform, err)
			}
			unit.pkg = checked
			for _, name := range checked.Scope().Names() {
				obj, ok := checked.Scope().Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				named, ok := types.Unalias(obj.Type()).(*types.Named)
				if ok && (implementsError(named) || implementsError(types.NewPointer(named))) {
					unit.errorTypes[named] = true
				}
			}
			// A constructor is a resolved function with a direct returned refusal
			// literal, or a direct return of another constructor. No data-flow tracing.
			changed := true
			for changed {
				changed = false
				for _, file := range syntax {
					for _, decl := range file.Decls {
						fn, ok := decl.(*ast.FuncDecl)
						if !ok || fn.Body == nil || len(fn.Body.List) != 1 {
							continue
						}
						if _, direct := fn.Body.List[0].(*ast.ReturnStmt); !direct {
							continue
						}

						obj := unit.info.Defs[fn.Name]
						signature, ok := obj.Type().(*types.Signature)
						if !ok || signature.Results().Len() != 1 || !implementsError(signature.Results().At(0).Type()) {
							continue
						}
						if unit.constructors[obj] {
							continue
						}
						ast.Inspect(fn.Body, func(node ast.Node) bool {
							if _, ok := node.(*ast.FuncLit); ok {
								return false
							}
							ret, ok := node.(*ast.ReturnStmt)
							if !ok {
								return true
							}
							for _, expr := range ret.Results {
								if unit.refusalProducer(expr) {
									unit.constructors[obj] = true
									changed = true
								}
							}
							return true
						})
					}
				}
			}
			unit.indexBodies(syntax)
			units = append(units, unit)
			for file := range unit.files {
				covered[file] = true
			}
		}
	}
	for _, file := range files {
		if !covered[file] {
			return nil, fmt.Errorf("%s: no type-resolved platform context (accepted GOOS: darwin/linux/windows)", file)
		}
	}
	return units, nil
}

func implementsError(t types.Type) bool {
	if t == nil {
		return false
	}
	return types.Implements(t, types.Universe.Lookup("error").Type().Underlying().(*types.Interface))
}
func (u typedUnit) object(expr ast.Expr) types.Object {
	switch x := expr.(type) {
	case *ast.Ident:
		return u.info.Uses[x]
	case *ast.SelectorExpr:
		return u.info.Uses[x.Sel]
	}
	return nil
}
func unparen(expr ast.Expr) ast.Expr {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = p.X
	}
}
func (u typedUnit) refusalProducer(expr ast.Expr) bool {
	expr = unparen(expr)
	switch x := expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		if _, variable := u.object(expr).(*types.Var); variable {
			return u.refusalType(u.info.TypeOf(expr))
		}
		return false
	case *ast.CallExpr:
		return u.constructors[u.object(x.Fun)] || u.refusalType(u.info.TypeOf(x))
	case *ast.UnaryExpr:
		if x.Op != token.AND {
			return false
		}
		_, ok := x.X.(*ast.CompositeLit)
		return ok && u.refusalType(u.info.TypeOf(x))
	case *ast.CompositeLit:
		return u.refusalType(u.info.TypeOf(x))
	}
	return false
}
func (u typedUnit) refusalType(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, ok := t.(*types.Named)
	return ok && u.errorTypes[n]
}

func typedPropagationSites(root string, files []string) ([]refusalSite, map[string]*token.FileSet, error) {
	units, err := typedUnits(root, files)
	if err != nil {
		return nil, nil, err
	}
	sets := map[string]*token.FileSet{}
	seen := map[string]bool{}
	var sites []refusalSite
	for _, u := range units {
		for file, syntax := range u.files {
			if seen[file] {
				continue
			}
			seen[file] = true
			sets[file] = u.set
			for _, decl := range syntax.Decls {
				if _, ok := decl.(*ast.GenDecl); !ok {
					continue
				}
				var invalid []string
				ast.Inspect(decl, func(node ast.Node) bool {
					if expr, ok := node.(ast.Expr); ok && u.refusalProducer(expr) {
						invalid = append(invalid, fmt.Sprintf("%s:%d: typed refusal declaration does not propagate under P1-P4", file, u.set.Position(expr.Pos()).Line))
						return false
					}
					return true
				})
				if len(invalid) > 0 {
					return nil, nil, fmt.Errorf("%s", strings.Join(invalid, "\n"))
				}
			}
			for _, decl := range syntax.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var parents []ast.Node
				ordinal := 0
				var failures []string
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if node == nil {
						parents = parents[:len(parents)-1]
						return false
					}
					expr, ok := node.(ast.Expr)
					producer := ok && u.refusalProducer(expr) && !u.refusalObservation(expr, parents)
					// &T{} is one producer, not two sites.
					if producer && len(parents) > 0 {
						if parent, ok := parents[len(parents)-1].(ast.Expr); ok && u.refusalProducer(parent) {
							producer = false
						}
					}
					if producer {
						position := u.set.Position(expr.Pos())
						if !u.propagates(fn, expr, parents) {
							failures = append(failures, fmt.Sprintf("%s:%d: typed refusal does not propagate under P1-P4", file, position.Line))
							return false
						}
						ordinal++
						guard, condition, otherwise := nearestGuardForNode(parents, expr)
						name := types.TypeString(u.info.TypeOf(expr), nil)
						var call *ast.CallExpr
						if x, ok := expr.(*ast.CallExpr); ok {
							call = x
							if object := u.object(x.Fun); object != nil {
								name = object.Name()
							}
						}
						label := producerLabel(expr)
						sites = append(sites, refusalSite{unconditional: soleUnconditionalReturn(fn, parents), typeContext: &u, fset: u.set, id: fmt.Sprintf("%s:%s:%d", file, functionIdentity(fn), ordinal), file: file, function: functionIdentity(fn), constructor: name, site: label, line: position.Line, guard: guard, guardExpr: condition, elseBranch: otherwise, call: call, expression: expr})
					}
					parents = append(parents, node)
					return true
				})
				if len(failures) > 0 {
					return nil, nil, fmt.Errorf("%s", strings.Join(failures, "\n"))
				}
			}
			before := len(sites)
			collectUnknownStatusGates(file, u.set, syntax, &sites)
			collectCatalogAvailabilityAttestationGates(file, u.set, syntax, &sites)
			collectCatalogResolutionDecisionGates(file, u.set, syntax, &sites)
			for i := before; i < len(sites); i++ {
				sites[i].fset = u.set
				sites[i].typeContext = &u
				sites[i].id = fmt.Sprintf("%s:%s:gate:%d", file, sites[i].function, i-before+1)
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].id < sites[j].id })
	return sites, sets, nil
}
func functionIdentity(fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		return receiverName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return fn.Name.Name
}
func producerLabel(expr ast.Expr) string {
	var label string
	ast.Inspect(expr, func(n ast.Node) bool {
		if label != "" {
			return false
		}
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			label, _ = strconv.Unquote(lit.Value)
		}
		return true
	})
	if label == "" {
		var b strings.Builder
		_ = format.Node(&b, token.NewFileSet(), expr)
		label = b.String()
	}
	return label
}
func (u typedUnit) propagates(fn *ast.FuncDecl, expr ast.Expr, parents []ast.Node) bool {
	return u.propagatesUnder(fn, expr, parents, true)
}
func (u typedUnit) propagatesUnder(fn *ast.FuncDecl, expr ast.Expr, parents []ast.Node, wrappers bool) bool {
	// Only parentheses and an address-of literal may sit between the producer
	// and its consumer. Composite storage and ordinary arguments are rejected.
	index := len(parents) - 1
	for index >= 0 {
		switch parents[index].(type) {
		case *ast.ParenExpr, *ast.KeyValueExpr, *ast.CompositeLit, *ast.UnaryExpr:
			index--
			continue
		}
		break
	}
	if index < 0 {
		return false
	}
	switch consumer := parents[index].(type) {
	case *ast.ReturnStmt:
		for _, v := range consumer.Results {
			if v.Pos() <= expr.Pos() && v.End() >= expr.End() {
				return implementsError(u.info.TypeOf(v))
			}
		}
	case *ast.SendStmt:
		channel, ok := u.info.TypeOf(consumer.Chan).Underlying().(*types.Chan)
		return ok && unparen(consumer.Value) == expr && implementsError(channel.Elem())
	case *ast.AssignStmt:
		if len(consumer.Lhs) != 1 || len(consumer.Rhs) != 1 || unparen(consumer.Rhs[0]) != expr {
			return false
		}
		target, ok := consumer.Lhs[0].(*ast.Ident)
		if !ok {
			return false
		}
		object := u.info.Uses[target]
		if object == nil {
			object = u.info.Defs[target]
		}
		if index > 0 {
			if branch, ok := parents[index-1].(*ast.IfStmt); ok && branch.Init == consumer {
				count := 0
				onlyReturn := true
				ast.Inspect(branch.Body, func(node ast.Node) bool {
					if _, closure := node.(*ast.FuncLit); closure {
						return false
					}
					ret, ok := node.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for _, value := range ret.Results {
						if id, ok := unparen(value).(*ast.Ident); ok && u.info.Uses[id] == object {
							count++
						}
					}
					return true
				})
				// No references outside the single return and the condition.
				ast.Inspect(branch.Body, func(node ast.Node) bool {
					if id, ok := node.(*ast.Ident); ok && u.info.Uses[id] == object {
						found := false
						ast.Inspect(branch.Body, func(n ast.Node) bool {
							if ret, ok := n.(*ast.ReturnStmt); ok {
								for _, v := range ret.Results {
									found = found || v == id
								}
							}
							return true
						})
						onlyReturn = onlyReturn && found
					}
					return true
				})
				return count == 1 && onlyReturn && implementsError(object.Type())
			}
		}
		results := fn.Type.Results
		// The nearest ordinary closure owns its own named results. A direct
		// deferred closure may instead assign its enclosing function's result.
		for i := index - 1; i >= 0; i-- {
			literal, ok := parents[i].(*ast.FuncLit)
			if !ok {
				continue
			}
			own := namedErrorResult(u, literal.Type.Results, object)
			if own {
				return true
			}
			deferred := false
			if i >= 2 {
				if call, ok := parents[i-1].(*ast.CallExpr); ok {
					if statement, ok := parents[i-2].(*ast.DeferStmt); ok && statement.Call == call {
						deferred = true
					}
				}
			}
			if !deferred {
				return false
			}
		}
		if !namedErrorResult(u, results, object) {
			return false
		}
		return true
	case *ast.CallExpr:
		if !wrappers {
			return false
		}
		// Constructors may nest refusal producers in their own arguments.
		if u.constructors[u.object(consumer.Fun)] {
			return true
		}
		for arg, value := range consumer.Args {
			if unparen(value) == expr && u.propagationWrapper(consumer, arg) {
				// Recursion here classifies the consumer only; it never traces values.
				// P4 may consume only a P1-P3 result, not another wrapper argument.
				return u.propagatesUnder(fn, consumer, parents[:index], false)
			}
		}
		return false
	}
	return false
}

type narrowingWitness struct {
	Admit  string `json:"admit"`
	Member int    `json:"member"`
}

type siteBound struct {
	Kind          string         `json:"kind"`
	Reason        string         `json:"reason"`
	Member        int            `json:"member"`
	PinningTest   *testReference `json:"pinning_test,omitempty"`
	DownstreamKey string         `json:"downstream_key,omitempty"`
}
type siteMapping struct {
	Bounds       []siteBound        `json:"bounds,omitempty"`
	SourceSHA256 string             `json:"source_sha256,omitempty"`
	Witnesses    []narrowingWitness `json:"witnesses"`
	Site         string             `json:"site"`
	Tests        []testReference    `json:"tests"`
}

func readSiteMappings(root string) ([]siteMapping, error) {
	data, err := os.ReadFile(filepath.Join(root, "scripts", "engine-catalog-sites.json"))
	if err != nil {
		return nil, err
	}
	var mappings []siteMapping
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mappings); err != nil {
		return nil, err
	}
	return mappings, nil
}
func mappedSiteTests(id string, mappings []siteMapping, candidates []parsedTest) ([]testReference, error) {
	var mapped []testReference
	matches := 0
	for _, mapping := range mappings {
		if mapping.Site == id {
			matches++
			mapped = mapping.Tests
		}
	}
	if matches != 1 || len(mapped) == 0 {
		return nil, fmt.Errorf("%s: want exactly one explicit AST identity mapping with named negative tests", id)
	}
	for _, ref := range mapped {
		found := false
		// Certificates bind to test declarations. Subtest execution remains selected
		// by the harness, but absence of the parent declaration always refuses.
		for _, candidate := range candidates {
			found = found || (candidate.reference.Dir == ref.Dir && candidate.reference.Name == ref.Name && ref.BodySHA256 != "" && candidate.bodyDigest == ref.BodySHA256)
		}
		if !found {
			return nil, fmt.Errorf("%s: mapped negative test missing or body changed: %s:%s", id, ref.Dir, ref.Name)
		}
	}
	return mapped, nil
}

func downstreamOf(primary, downstream mutant) bool {
	return primary.file == downstream.file && primary.function == downstream.function && primary.end <= downstream.start && primary.siteID != downstream.siteID
}
func provedJointEquivalence(primary, downstream, joint result) bool {
	return primary.state == "survived" && primary.code == 0 && downstream.state == "survived" && downstream.code == 0 && joint.state == "killed" && joint.code != 0 && joint.test != "" && containsTest(primary.mutant.tests, joint.test)
}

func mutationEvidenceComplete(rows []result) bool {
	if len(rows) == 0 {
		return false
	}
	for _, row := range rows {
		if row.state == "bound: undecided" && row.code == 0 && row.bound != "" {
			continue
		}
		if row.state == "bound: defensive-unreachable" && row.code == 0 && row.test != "" && row.jointLog != "" && row.jointCode != 0 && row.bound != "" {
			continue
		}
		if row.test == "" {
			return false
		}
		if row.state == "killed" && row.code != 0 {
			continue
		}
		if row.state == "equivalent" && row.code == 0 && row.jointLog != "" && row.jointCode != 0 {
			continue
		}
		return false
	}
	return true
}

func testBodyDigest(body *ast.BlockStmt) string {
	var text strings.Builder
	fatalIf(format.Node(&text, token.NewFileSet(), body))
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text.String())))
}

func namedErrorResult(u typedUnit, results *ast.FieldList, object types.Object) bool {
	if results == nil || object == nil || !implementsError(object.Type()) {
		return false
	}
	for _, field := range results.List {
		for _, name := range field.Names {
			if u.info.Defs[name] == object {
				return true
			}
		}
	}
	return false
}

// Bodies and parameter references are resolved objects, never name overlaps.
func (u *typedUnit) indexBodies(files []*ast.File) {
	u.bodies = map[types.Object]*ast.FuncDecl{}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				u.bodies[u.info.Defs[fn.Name]] = fn
			}
		}
	}
}
func (u typedUnit) propagationWrapper(call *ast.CallExpr, argument int) bool {
	object, ok := u.object(call.Fun).(*types.Func)
	if !ok {
		return false
	}
	fn := u.bodies[object]
	if fn == nil {
		return false
	} // interface calls and imported/external bodies refuse
	signature := object.Type().(*types.Signature)
	if argument >= signature.Params().Len() {
		return false
	}
	parameter := signature.Params().At(argument)
	if !implementsError(parameter.Type()) {
		return false
	}
	count, valid := 0, true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if _, closure := node.(*ast.FuncLit); closure {
			return false
		}
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for i := 0; i < signature.Results().Len(); i++ {
			if !implementsError(signature.Results().At(i).Type()) {
				continue
			}
			count++
			if i >= len(ret.Results) {
				valid = false
				continue
			}
			value := unparen(ret.Results[i])
			if u.object(value) == parameter {
				continue
			}
			joined, ok := value.(*ast.CallExpr)
			if !ok {
				valid = false
				continue
			}
			join, ok := u.object(joined.Fun).(*types.Func)
			if !ok || join.Pkg() == nil || join.Pkg().Path() != "errors" || join.Name() != "Join" {
				valid = false
				continue
			}
			found := false
			for _, arg := range joined.Args {
				found = found || u.object(unparen(arg)) == parameter
			}
			valid = valid && found
		}
		return true
	})
	return count > 0 && valid
}

// Observation is a closed syntax exception, not value-flow tracing: selecting
// an error's fields/methods, comparing to nil, and the errors.As target address
// inspect a previously received refusal; they do not create/store a refusal.
func (u typedUnit) refusalObservation(expr ast.Expr, parents []ast.Node) bool {
	if len(parents) == 0 {
		return false
	}
	parent := parents[len(parents)-1]
	if selector, ok := parent.(*ast.SelectorExpr); ok && selector.X == expr {
		return true
	}
	if binary, ok := parent.(*ast.BinaryExpr); ok && (binary.Op == token.EQL || binary.Op == token.NEQ) {
		other := binary.X
		if other == expr {
			other = binary.Y
		}
		if id, ok := other.(*ast.Ident); ok && u.info.Uses[id] == types.Universe.Lookup("nil") {
			return true
		}
	}
	if address, ok := parent.(*ast.UnaryExpr); ok && address.Op == token.AND && len(parents) >= 2 {
		if call, ok := parents[len(parents)-2].(*ast.CallExpr); ok && len(call.Args) == 2 && call.Args[1] == address {
			fn, ok := u.object(call.Fun).(*types.Func)
			return ok && fn.Pkg() != nil && fn.Pkg().Path() == "errors" && fn.Name() == "As"
		}
	}
	return false
}

// A singleton binds the identical guarded expression (syntax AND object path),
// not an observation of it. Calls/indexes are opaque when selecting operands;
// their arguments cannot lend their identity to a coarser predicate.
func typedWitnessMember(site refusalSite, witness narrowingWitness) error {
	expr, err := parser.ParseExpr(witness.Admit)
	if err != nil {
		return err
	}
	operand, literal, shape := singletonShape(expr)
	if !shape {
		return fmt.Errorf("%s: witness must be singleton equality", site.id)
	}
	if site.typeContext == nil {
		return nil
	} // legacy parsed-condition fixtures
	u := site.typeContext
	target := site.guardExpr
	if target == nil {
		target = site.expression
	}
	selected := target
	if site.guardExpr != nil {
		groups := rejectedGroups(target)
		if witness.Member < 0 || witness.Member >= len(groups) {
			return fmt.Errorf("%s: witness member outside condition class", site.id)
		}
		selected = groups[witness.Member]
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
	if err := types.CheckExpr(u.set, u.pkg, target.Pos(), expr, info); err != nil {
		return fmt.Errorf("%s: witness does not resolve in guarding scope: %w", site.id, err)
	}
	matched := false
	ast.Inspect(selected, func(node ast.Node) bool {
		expression, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		if singletonOperand(expression) {
			input, path := witnessAccessPath(expression, u.info), witnessAccessPath(operand, info)
			matched = matched || (len(input) > 0 && sameAccessPath(input, path) && expressionText(token.NewFileSet(), unparen(expression)) == expressionText(token.NewFileSet(), unparen(operand)))
			return false // selecting a field cannot also certify its base or sibling field.
		}
		switch expression.(type) {
		case *ast.CallExpr, *ast.IndexExpr, *ast.IndexListExpr, *ast.SliceExpr:
			return false
		}
		return true
	})
	if !matched {
		return fmt.Errorf("%s: singleton equality must bind the identical guarded expression; no constant, sibling field, unrelated domain or observation", site.id)
	}
	typ := info.TypeOf(operand)
	if _, ok := unparen(expr).(*ast.BinaryExpr); ok {
		value := info.Types[unparen(literal)]
		basic, isBasic := typ.Underlying().(*types.Basic)
		if !isBasic || basic.Info()&types.IsUntyped != 0 || value.Value == nil || !types.AssignableTo(value.Type, typ) {
			return fmt.Errorf("%s: singleton equality needs a go/types constant literal of the guarded type", site.id)
		}
	} else {
		call := unparen(expr).(*ast.CallExpr)
		callee, ok := resolvedWitnessObject(call.Fun, info).(*types.Func)
		if !ok || callee.Pkg() == nil || callee.Pkg().Path() != "reflect" || callee.Name() != "DeepEqual" || types.Comparable(typ) || !types.Identical(typ, info.TypeOf(literal)) || !fullySpecifiedLiteral(literal, info) {
			return fmt.Errorf("%s: singleton equality needs standard reflect.DeepEqual on a non-comparable guarded type and a fully specified composite literal", site.id)
		}
	}
	return nil
}

// Only direct variable/field access is in the frozen grammar. Syntax is checked
// separately: no call, index, slice or method is erased from identity.
func witnessAccessPath(expr ast.Expr, info *types.Info) []types.Object {
	switch value := unparen(expr).(type) {
	case *ast.Ident:
		if object, ok := info.Uses[value].(*types.Var); ok && !object.IsField() {
			return []types.Object{object}
		}
	case *ast.SelectorExpr:
		object, ok := info.Uses[value.Sel].(*types.Var)
		if !ok {
			return nil
		}
		if !object.IsField() {
			return []types.Object{object}
		}
		if base := witnessAccessPath(value.X, info); len(base) > 0 {
			return append(base, object)
		}
	}
	return nil
}
func sameAccessPath(left, right []types.Object) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// All aggregate members are literal values; structs and arrays must specify
// every field/slot. Keyed slices/arrays, pointers and implicit interface values
// are outside this closed literal subset. No partial aggregate is a singleton.
func fullySpecifiedLiteral(expr ast.Expr, info *types.Info) bool {
	expr = unparen(expr)
	literal, ok := expr.(*ast.CompositeLit)
	if !ok {
		return constantLiteral(expr) && info.Types[expr].Value != nil
	}
	switch typ := info.TypeOf(expr).Underlying().(type) {
	case *types.Array:
		if int64(len(literal.Elts)) != typ.Len() {
			return false
		}
	case *types.Slice:
	case *types.Map:
		for _, element := range literal.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok || !fullySpecifiedLiteral(pair.Key, info) || !fullySpecifiedLiteral(pair.Value, info) {
				return false
			}
		}
		return true
	case *types.Struct:
		if len(literal.Elts) != typ.NumFields() {
			return false
		}
		fields := map[string]bool{}
		for i, element := range literal.Elts {
			if pair, keyed := element.(*ast.KeyValueExpr); keyed {
				name, ok := pair.Key.(*ast.Ident)
				if !ok || fields[name.Name] || !fullySpecifiedLiteral(pair.Value, info) {
					return false
				}
				fields[name.Name] = true
			} else if !fullySpecifiedLiteral(element, info) {
				return false
			} else {
				fields[typ.Field(i).Name()] = true
			}
		}
		for i := 0; i < typ.NumFields(); i++ {
			if !fields[typ.Field(i).Name()] {
				return false
			}
		}
		return true
	default:
		return false
	}
	for _, element := range literal.Elts {
		if !fullySpecifiedLiteral(element, info) {
			return false
		}
	}
	return true
}

// A single direct return has no guarding condition. Dispatch arms and returns
// following other statements retain their ordinary witness requirement; they
// cannot borrow this unconditional certificate merely because no IfStmt is a
// parent of the producer expression.
func soleUnconditionalReturn(fn *ast.FuncDecl, parents []ast.Node) bool {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	for _, parent := range parents {
		if parent == ret {
			return true
		}
		if _, ok := parent.(*ast.FuncLit); ok {
			return false
		}
	}
	return false
}

func hasUndecidedBound(bounds []siteBound, member int) bool {
	for _, b := range bounds {
		if b.Kind == "undecided" && b.Member == member && b.Reason != "" {
			return true
		}
	}
	return false
}
func validateSiteBounds(root string, site refusalSite, mapping siteMapping) error {
	data, err := os.ReadFile(filepath.Join(root, site.file))
	if err != nil {
		return err
	}
	if mapping.SourceSHA256 != "" && mapping.SourceSHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		return fmt.Errorf("%s: certificate source identity changed", site.id)
	}
	for _, b := range mapping.Bounds {
		if mapping.SourceSHA256 == "" || b.Reason == "" || b.Member < -1 {
			return fmt.Errorf("%s: bound requires source identity, reason and member", site.id)
		}
		switch b.Kind {
		case "undecided":
		case "defensive-unreachable":
			if b.PinningTest == nil || b.PinningTest.Name == "" || b.DownstreamKey == "" {
				return fmt.Errorf("%s: defensive bound lacks pin or downstream", site.id)
			}
			candidates, err := loadTests(root, []string{site.file})
			if err != nil {
				return err
			}
			if _, err = mappedSiteTests(site.id, []siteMapping{{Site: site.id, Tests: []testReference{*b.PinningTest}}}, candidates); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unknown bound kind %q", site.id, b.Kind)
		}
	}
	return nil
}
func proveDefensiveBound(root, out string, primary result, downstream mutant, b siteBound, id int, linux, windows string) (result, bool) {
	if primary.state != "survived" || primary.code != 0 || b.PinningTest == nil || !downstreamOf(primary.mutant, downstream) {
		return primary, false
	}
	cmd := exec.Command("go", "test", "./"+b.PinningTest.Dir, "-run", "^"+regexp.QuoteMeta(b.PinningTest.Name)+"$", "-count=1", "-json")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	pinLog := filepath.Join(out, fmt.Sprintf("pin-%03d.log", id))
	fatalIf(os.WriteFile(pinLog, output, 0600))
	if err != nil || !namedTestPassed(output, b.PinningTest.Name) {
		return primary, false
	}
	joint := primary.mutant
	joint.extra = []sourceMutation{{downstream.start, downstream.end, downstream.replaceText}}
	proof := runMutant(root, filepath.Join(out, "defensive-joint"), joint, id, linux, windows)
	if proof.state != "killed" || proof.test == "" || !containsTest(primary.mutant.tests, proof.test) {
		return primary, false
	}
	primary.state = "bound: defensive-unreachable"
	primary.test = proof.test
	primary.jointCode = proof.code
	primary.jointLog = proof.log
	primary.bound = fmt.Sprintf("%s; pin %s exit 0 (%s); joint with %s killed by %s exit %d (%s)", b.Reason, b.PinningTest.Name, pinLog, downstream.key, proof.test, proof.code, proof.log)
	return primary, true
}

func namedTestPassed(output []byte, name string) bool {
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var event struct{ Action, Test string }
		err := decoder.Decode(&event)
		if err == io.EOF {
			return false
		}
		if err != nil {
			return false
		}
		if event.Test == name && event.Action == "pass" {
			return true
		}
	}
}

// The accepted standalone subset is exactly //go:build ignore on package main.
// Other uncovered build contexts continue to refuse rather than being skipped.
func standaloneIgnoreCommand(root, file string) bool {
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, file), nil, parser.ParseComments)
	if err != nil || parsed.Name.Name != "main" {
		return false
	}
	for _, group := range parsed.Comments {
		if group.Pos() > parsed.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			tag, ok := expr.(*constraint.TagExpr)
			return err == nil && ok && tag.Tag == "ignore"
		}
	}
	return false
}

func resolvedWitnessObject(expr ast.Expr, info *types.Info) types.Object {
	switch value := unparen(expr).(type) {
	case *ast.Ident:
		return info.Uses[value]
	case *ast.SelectorExpr:
		return info.Uses[value.Sel]
	}
	return nil
}

// Selection never narrows the certificate census; unknown paths fail closed.
func selectMutationFiles(files []string, mutants []mutant, scope string) ([]mutant, error) {
	if scope == "" {
		return mutants, nil
	}
	selected := map[string]bool{}
	for _, path := range strings.Split(scope, ",") {
		if !containsValue(files, path) || selected[path] {
			return nil, fmt.Errorf("unknown or duplicate production file %q", path)
		}
		selected[path] = true
	}
	var filtered []mutant
	for _, candidate := range mutants {
		if selected[candidate.file] {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("selected files contain no certified mutants")
	}
	return filtered, nil
}
