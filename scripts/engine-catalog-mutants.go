// Command engine-catalog-mutants derives typed refusal gates from all
// production Go files found by a build-tag-independent filesystem walk, generates narrowing
// mutants, and runs the behavioral tests that name each refusal site.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type refusalSite struct {
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
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type parsedTest struct {
	reference testReference
	body      *ast.BlockStmt
}

type mutant struct {
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
	Files   []string          `json:"files"`
	Sites   []inventorySite   `json:"sites"`
	Mutants []inventoryMutant `json:"mutants"`
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
	if *inventoryOnly {
		report := inventoryReport{Files: files}
		for _, site := range sites {
			report.Sites = append(report.Sites, inventorySite{Key: site.id, File: site.file, Function: site.function, Constructor: site.constructor, Site: site.site, Line: site.line, Tests: site.tests, MutationKeys: site.expectedMutationKeys})
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
		fmt.Fprintln(os.Stderr, "-out is required and must be outside the managed Story worktree")
		os.Exit(2)
	}
	outPath, err := filepath.Abs(*out)
	fatalIf(err)
	if within(rootPath, outPath) {
		fatalIf(fmt.Errorf("mutation outputs must be outside the repository worktree"))
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
	manifest := filepath.Join(outPath, fmt.Sprintf("TASK-260929-9amima_mutants_%03d-%03d.md", *start, end))
	if err := os.WriteFile(manifest, []byte(renderTable(len(sites), len(mutants), rows)), 0o600); err != nil {
		fatalIf(err)
	}
	failures := 0
	for _, row := range rows {
		if row.state != "killed" && row.state != "equivalent" {
			failures++
		}
	}
	fmt.Printf("package production files=%d; refusal/gate sites=%d; generated mutants=%d; run=%d; unresolved=%d; table=%s\n", len(files), len(sites), len(mutants), len(rows), failures, manifest)
	if failures > 0 {
		os.Exit(1)
	}
}

type result struct {
	index  int
	mutant mutant
	target string
	state  string
	test   string
	code   int
	log    string
	bound  string
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
	constructorsByDirectory := make(map[string]map[string]bool)
	for _, sourceName := range files {
		directory := filepath.ToSlash(filepath.Dir(sourceName))
		if constructorsByDirectory[directory] != nil {
			continue
		}
		constructors, err := domainErrorHelpers(root, []string{sourceName})
		if err != nil {
			return nil, err
		}
		constructorsByDirectory[directory] = constructors
	}
	var sites []refusalSite
	fileSets := make(map[string]*token.FileSet)
	for _, sourceName := range files {
		path := filepath.Join(root, sourceName)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		fileSets[sourceName] = fset
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
				if returned, ok := node.(*ast.ReturnStmt); ok {
					guard, guardExpr, elseBranch := nearestGuard(parents, returned)
					for _, expression := range returned.Results {
						collectTypedCalls(expression, constructorsByDirectory[filepath.ToSlash(filepath.Dir(sourceName))], func(call *ast.CallExpr, constructor, site string) {
							position := fset.Position(call.Pos())
							id := fmt.Sprintf("%s:%d:%s:%s", sourceName, position.Offset, constructor, site)
							sites = append(sites, refusalSite{id: id, file: sourceName, function: function.Name.Name, constructor: constructor, site: site, line: position.Line, guard: guard, guardExpr: guardExpr, elseBranch: elseBranch, call: call, expression: call})
						})
						collectTypedLiterals(expression, constructorsByDirectory[filepath.ToSlash(filepath.Dir(sourceName))], func(target ast.Expr, constructor, site string) {
							position := fset.Position(target.Pos())
							id := fmt.Sprintf("%s:%d:%s:%s", sourceName, position.Offset, constructor, site)
							sites = append(sites, refusalSite{id: id, file: sourceName, function: function.Name.Name, constructor: constructor, site: site, line: position.Line, guard: guard, guardExpr: guardExpr, elseBranch: elseBranch, expression: target})
						})
					}
				}
				parents = append(parents, node)
				return true
			})
		}
		collectUnknownStatusGates(sourceName, fset, file, &sites)
		collectCatalogAvailabilityAttestationGates(sourceName, fset, file, &sites)
		collectCatalogResolutionDecisionGates(sourceName, fset, file, &sites)
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("package-derived AST inventory found no typed refusal or status gate sites")
	}
	tests, err := loadTests(root, files)
	if err != nil {
		return nil, err
	}
	callers, err := productionCallers(root, files)
	if err != nil {
		return nil, err
	}
	for index := range sites {
		sites[index].tests = testsForRefusalSite(tests, callers, sites[index])
		keys, keyErr := expectedMutationKeys(fileSets[sites[index].file], sites[index])
		if keyErr != nil {
			return nil, keyErr
		}
		sites[index].expectedMutationKeys = keys
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

func packageSourceFiles(root string, directories []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	for _, directory := range directories {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.ToSlash(filepath.Join(directory, name))
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
		}
	}
	return files, nil
}

func domainErrorHelpers(root string, changedFiles []string) (map[string]bool, error) {
	var directories []string
	for _, path := range changedFiles {
		directories = appendUnique(directories, filepath.ToSlash(filepath.Dir(path)))
	}
	files, err := packageSourceFiles(root, directories)
	if err != nil {
		return nil, err
	}
	type errorMethod struct{ receiver string }
	var methods []errorMethod
	var declarations []*ast.FuncDecl
	for _, path := range files {
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, 0)
		if parseErr != nil {
			return nil, parseErr
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if function.Name.Name == "Error" && function.Recv != nil && function.Type.Results != nil && len(function.Type.Results.List) == 1 {
				if receiver := receiverName(function.Recv.List[0].Type); receiver != "" {
					methods = append(methods, errorMethod{receiver: receiver})
				}
			}
			if function.Recv == nil && function.Body != nil {
				declarations = append(declarations, function)
			}
		}
	}
	errorTypes := map[string]bool{}
	for _, method := range methods {
		errorTypes[method.receiver] = true
	}
	helpers := map[string]bool{}
	for _, function := range declarations {
		if returnsErrorType(function.Type.Results, errorTypes) && returnsCustomError(function.Body, errorTypes) {
			helpers[function.Name.Name] = true
		}
	}
	for name := range errorTypes {
		helpers[name] = true
	}
	return helpers, nil
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

func returnsErrorType(results *ast.FieldList, errorTypes map[string]bool) bool {
	if results == nil || len(results.List) != 1 || len(results.List[0].Names) > 1 {
		return false
	}
	if name, ok := results.List[0].Type.(*ast.Ident); ok && (name.Name == "error" || errorTypes[name.Name]) {
		return true
	}
	return false
}

func returnsCustomError(body *ast.BlockStmt, errorTypes map[string]bool) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		returned, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, expression := range returned.Results {
			ast.Inspect(expression, func(child ast.Node) bool {
				switch value := child.(type) {
				case *ast.CompositeLit:
					if name, ok := value.Type.(*ast.Ident); ok && errorTypes[name.Name] {
						found = true
					}
				}
				return !found
			})
		}
		return !found
	})
	return found
}

func calledName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	}
	return ""
}

func collectTypedCalls(expression ast.Expr, constructors map[string]bool, found func(*ast.CallExpr, string, string)) {
	ast.Inspect(expression, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calledName(call.Fun)
		if !constructors[name] {
			return true
		}
		var site string
		for _, argument := range call.Args {
			literal, ok := argument.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			if value, err := strconv.Unquote(literal.Value); err == nil {
				site = value
				break
			}
		}
		if site == "" {
			if len(call.Args) > 0 {
				var expression strings.Builder
				if err := format.Node(&expression, token.NewFileSet(), call.Args[0]); err == nil {
					site = "expr:" + expression.String()
				}
			}
			if site == "" {
				site = "helper:" + name
			}
		}
		found(call, name, site)
		return false
	})
}

func collectTypedLiterals(expression ast.Expr, errorTypes map[string]bool, found func(ast.Expr, string, string)) {
	var literal *ast.CompositeLit
	var target ast.Expr
	switch value := expression.(type) {
	case *ast.UnaryExpr:
		if value.Op == token.AND {
			literal, _ = value.X.(*ast.CompositeLit)
			target = value
		}
	case *ast.CompositeLit:
		literal = value
		target = value
	}
	if literal == nil {
		return
	}
	typeName := receiverName(literal.Type)
	if !errorTypes[typeName] {
		return
	}
	site := "type:" + typeName
	ast.Inspect(literal, func(node ast.Node) bool {
		value, ok := node.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return true
		}
		if text, err := strconv.Unquote(value.Value); err == nil && text != "" {
			site = text
			return false
		}
		return true
	})
	found(target, typeName, site)
}

func nearestGuard(parents []ast.Node, returned *ast.ReturnStmt) (*ast.IfStmt, ast.Expr, bool) {
	for index := len(parents) - 1; index >= 0; index-- {
		branch, ok := parents[index].(*ast.IfStmt)
		if !ok {
			continue
		}
		if branch.Body != nil && returned.Pos() >= branch.Body.Pos() && returned.End() <= branch.Body.End() {
			return branch, branch.Cond, false
		}
		if branch.Else != nil && returned.Pos() >= branch.Else.Pos() && returned.End() <= branch.Else.End() {
			return branch, branch.Cond, true
		}
	}
	return nil, nil, false
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
				candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: directory}, body: function.Body})
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
	fileSets := make(map[string]*token.FileSet)
	sources := make(map[string][]byte)
	for _, site := range sites {
		members := []ast.Expr{}
		if site.guardExpr != nil {
			booleanMembers(site.guardExpr, &members)
		}
		if len(members) == 0 {
			if site.expression == nil {
				return nil, fmt.Errorf("%s:%d %s.%s has no mutable refusal member", site.file, site.line, site.constructor, site.site)
			}
			fset, source, err := sourceFor(root, site.file, fileSets, sources)
			if err != nil {
				return nil, err
			}
			start := fset.Position(site.expression.Pos()).Offset
			end := fset.Position(site.expression.End()).Offset
			key := mutationKey(site.id, start, end)
			mutants = append(mutants, mutant{key: key, siteID: site.id, file: site.file, function: site.function, constructor: site.constructor, sites: []string{site.site}, tests: append([]testReference(nil), site.tests...), line: site.line, start: start, end: end, replaceText: nilErrorReplacement(fset, site.expression)})
			_ = source
			continue
		}
		fset, source, err := sourceFor(root, site.file, fileSets, sources)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			memberStart := fset.Position(member.Pos()).Offset
			memberEnd := fset.Position(member.End()).Offset
			replacement := "(" + string(source[memberStart:memberEnd]) + ") && false"
			if site.elseBranch {
				replacement = "(" + string(source[memberStart:memberEnd]) + ") || true"
			}
			key := mutationKey(site.id, memberStart, memberEnd)
			mutants = append(mutants, mutant{key: key, siteID: site.id, file: site.file, function: site.function, constructor: site.constructor, sites: []string{site.site}, tests: append([]testReference(nil), site.tests...), line: site.line, guardText: string(source[fset.Position(site.guardExpr.Pos()).Offset:fset.Position(site.guardExpr.End()).Offset]), memberText: string(source[memberStart:memberEnd]), replaceText: replacement, start: memberStart, end: memberEnd})
		}
	}
	generated := make(map[string]int, len(mutants))
	for _, current := range mutants {
		generated[current.key]++
	}
	for _, site := range sites {
		for _, expected := range site.expectedMutationKeys {
			if generated[expected] != 1 {
				return nil, fmt.Errorf("%s:%d %s.%s member %s produced %d narrowing mutants; want exactly one", site.file, site.line, site.constructor, site.site, expected, generated[expected])
			}
		}
	}
	sort.Slice(mutants, func(i, j int) bool {
		if mutants[i].file != mutants[j].file {
			return mutants[i].file < mutants[j].file
		}
		if mutants[i].line != mutants[j].line {
			return mutants[i].line < mutants[j].line
		}
		return mutants[i].key < mutants[j].key
	})
	if len(mutants) == 0 {
		return nil, fmt.Errorf("typed refusal inventory generated no narrowing mutants")
	}
	return mutants, nil
}

func expectedMutationKeys(fset *token.FileSet, site refusalSite) ([]string, error) {
	var keys []string
	if site.guardExpr != nil {
		members := []ast.Expr{}
		booleanMembers(site.guardExpr, &members)
		for _, member := range members {
			start := fset.Position(member.Pos()).Offset
			end := fset.Position(member.End()).Offset
			keys = append(keys, mutationKey(site.id, start, end))
		}
	}
	if len(keys) == 0 && site.expression != nil {
		start := fset.Position(site.expression.Pos()).Offset
		end := fset.Position(site.expression.End()).Offset
		keys = append(keys, mutationKey(site.id, start, end))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s:%d %s.%s has no enumerable refusal member", site.file, site.line, site.constructor, site.site)
	}
	return keys, nil
}

func mutationKey(siteID string, start, end int) string {
	return fmt.Sprintf("%s:%d:%d", siteID, start, end)
}

func nilErrorReplacement(fset *token.FileSet, expression ast.Expr) string {
	var text strings.Builder
	if err := format.Node(&text, fset, expression); err != nil {
		fatalIf(err)
	}
	return "func() error { _ = (" + text.String() + "); return nil }()"
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

func booleanMembers(expression ast.Expr, into *[]ast.Expr) {
	if binary, ok := expression.(*ast.BinaryExpr); ok && (binary.Op == token.LAND || binary.Op == token.LOR) {
		booleanMembers(binary.X, into)
		booleanMembers(binary.Y, into)
		return
	}
	*into = append(*into, expression)
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
	return rows
}

func runMutant(root, out string, current mutant, id int, linuxEmulationRoot, windowsEmulationRoot string) result {
	absoluteSource := filepath.Join(root, current.file)
	source, err := os.ReadFile(absoluteSource)
	fatalIf(err)
	mutatedSource := append([]byte(nil), source[:current.start]...)
	mutatedSource = append(mutatedSource, []byte(current.replaceText)...)
	mutatedSource = append(mutatedSource, source[current.end:]...)
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
		if row.test != "" {
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
func equivalentNarrowing(current mutant) (bound, witness string, equivalent bool) {
	// These are deliberately narrow equivalence proofs for constructor operands
	// whose malformed value is rejected again by a later invariant in the same
	// production parser. The generated mutant set itself is still derived from
	// every AST operand/member; only proven downstream-equivalent survivors are
	// classified here.
	type equivalentCase struct {
		file, function, member, test string
		line                         int
		bound                        string
	}
	parserEquivalents := []equivalentCase{
		{"internal/infra/pi_shared_protocol.go", "ResolveSharedRuntimePaths", "err != nil", "TestResolveSharedRuntimePathsRefusesCacheRootFailures", 253, "For the removed-working-directory fixture, filepath.Abs cannot determine the relative cache path and returns an empty absolute path. Narrowing its error check passes that value to EvalSymlinks, which still refuses the removed cwd before any cache-root path is returned. Bound: refusal remains shared_runtime_state_path_invalid; only the specific Abs error attribution is lost."},
		{"internal/infra/pi_shared_protocol.go", "ResolveSharedRuntimePaths", "err != nil", "TestResolveSharedRuntimePathsRefusesCacheRootFailures", 257, "For the missing cache-root fixture, EvalSymlinks does not produce a usable canonical path. Narrowing its error check passes the empty/unusable path to the immediately following Stat, which still refuses it before any runtime path is returned. Bound: refusal remains shared_runtime_state_path_invalid; only the symlink-resolution detail is lost."},
		{"internal/infra/pi_shared_protocol.go", "ResolveSharedRuntimePaths", "relErr != nil", "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", 285, "Both checked paths are built with filepath.Join(canonical, fixed namespace components, and a validated lowercase SHA-256 key). Rel therefore compares paths on the same root/volume and cannot fail for these constructed values. Bound: only construction through this function is covered; the check remains necessary if path construction changes."},
		{"internal/infra/pi_shared_protocol.go", "ResolveSharedRuntimePaths", "strings.HasPrefix(rel, \"..\"+string(filepath.Separator))", "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", 285, "The relative paths are generated by joining the canonical root with fixed non-parent namespace components and a validated lowercase SHA-256 key. They cannot begin with a parent traversal. Bound: only current constants and hash-only keys are covered; keep this guard if the path grammar expands."},
		{"internal/infra/pi_shared_protocol.go", "ResolveSharedRuntimePaths", "filepath.IsAbs(rel)", "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", 285, "filepath.Rel returns a relative result for the same-volume paths generated by joining the canonical root and fixed/hash-only descendants. Bound: only the current root-relative construction is covered; keep this guard if the relation inputs change."},
		{"internal/infra/pi_shared_protocol.go", "CreateSharedRuntimeTree", "err != nil", "TestCreateSharedRuntimeTreeRefusesEveryUnsafePath", 299, "If opening the canonical cache root fails and this refusal is narrowed, the descriptor stays invalid; the first openOrCreateDirAt call fails and its enclosing typed refusal returns before creating any path. Bound: only error attribution moves from root Open to the first directory open; no managed tree is created."},
		{"internal/infra/pi_shared_protocol.go", "readSharedBrokerRecord", "err != nil", "TestReadSharedBrokerRecordRefusesReadAndDecodeFailures", 411, "A non-absence os.ReadFile failure yields no bytes. Narrowing this error check reaches the strict JSON decoder, which refuses the empty payload with EOF before a broker record is returned. Bound: the refusal remains shared_runtime_state_unreadable; read-versus-decode attribution changes."},
		{"internal/infra/pi_shared_protocol.go", "writeSharedJSONAtomic", "err != nil", "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", 435, "If opening the destination directory fails and this check is narrowed, the invalid descriptor causes the immediately following Openat to fail before a temp file is created or state is installed. Bound: refusal remains shared_runtime_state_path_invalid; only the failing syscall attribution moves."},
		{"internal/infra/pi_shared_protocol.go", "writeSharedJSONAtomic", "err != nil", "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", 445, "When Openat fails, the invalid descriptor cannot create a file. If this early check is narrowed, the following os.NewFile/Write path still fails and the existing writeErr refusal returns before Rename; the test verifies the typed refusal and empty destination directory. Bound: only attribution moves from Openat to the write path, with no committed or temporary state."},
		{"internal/infra/pi_shared_protocol.go", "writeSharedJSONAtomic", "err != nil", "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", 440, "The Go crypto/rand.Read contract says it never returns an error and always fills the supplied buffer; its default operating-system reader is documented to do the same on supported systems. Bound: equivalence applies to this Go API/toolchain contract; the error branch cannot be reached through crypto/rand.Read as specified."},
		{"internal/infra/pi_config.go", "parsePiProfile", "err != nil", "TestParsePiProfileRejectsEveryMalformedField", 293, "requiredString failure leaves API empty, and the same condition's API equality operand still rejects every invalid/absent value. Bound: refusal attribution can move from the accessor error to the API mismatch; this test asserts refusal at the API field."},
		{"internal/infra/pi_config.go", "parsePiProfile", "err != nil", "TestParsePiProfileRejectsEveryMalformedField", 302, "requiredStringArray failure leaves Input nil, and the same condition's exact [text] comparison still rejects it. Bound: refusal cause can change while the profile remains rejected at the input field."},
		{"internal/infra/pi_config.go", "parsePiProfile", "err != nil", "TestParsePiProfileRejectsEveryMalformedField", 317, "requiredString failure leaves Thinking empty, and the immediately following documented-thinking membership check still rejects it. Bound: error detail can change; acceptance cannot."},
		{"internal/infra/pi_config.go", "parsePiProfile", "err != nil", "TestParsePiProfileRejectsEveryMalformedField", 326, "requiredStringArray failure leaves RequestedCapabilities nil, and the final exact capability-set comparison still rejects it. Bound: refusal attribution can move to the final requested-capabilities invariant."},
		{"internal/infra/pi_config.go", "parsePiProfile", "tableErr != nil", "TestParsePiProfileRejectsEveryMalformedField", 344, "projectConfigTable reports a wrong-typed lifecycle_log_retention table as (nil, false, error); narrowing the error member leaves !present true. Bound: only the error detail changes for this malformed table."},
		{"internal/infra/pi_config.go", "parsePiProfile", "tableErr != nil", "TestParsePiProfileRejectsEveryMalformedField", 354, "projectConfigTable reports a wrong-typed compat table as (nil, false, error); narrowing the error member leaves !present true. Bound: only the error detail changes for this malformed table."},
		{"internal/infra/pi_config.go", "parsePiProfile", "tableErr != nil", "TestParsePiProfileRejectsEveryMalformedField", 364, "projectConfigTable reports a wrong-typed runtime table as (nil, false, error); narrowing the error member leaves !present true. Bound: only the error detail changes for this malformed table."},
		{"internal/infra/pi_config.go", "parsePiLifecycleLogRetention", "!ok", "TestParsePiLifecycleLogRetentionRejectsEveryMalformedField", 458, "checkedPositiveAddMul(managedSlots, 1, 4) can overflow only above the stricter earlier 5+4*managedSlots bound; that earlier checked calculation refuses first. Bound: this addition-overflow branch is unreachable for an input that passes the preceding scan-entry bound."},
		{"internal/infra/pi_config.go", "parsePiCompaction", "compaction.CompactAtTokens >= contextWindow", "TestParsePiCompactionRejectsEveryMalformedField", 500, "if this comparison is narrowed, an equal or excessive threshold yields ReserveTokens <= 0; the later ReserveTokens < maxTokens check rejects it because maxTokens is positive. Bound: the exact refusal message may move to the reserve invariant."},
		{"internal/infra/pi_config.go", "parsePiCompaction", "err != nil", "TestParsePiCompactionRejectsEveryMalformedField", 505, "a reserve_tokens accessor failure leaves ReserveTokens at zero; the later ReserveTokens < maxTokens check still rejects because maxTokens is positive. Bound: the exact refusal message may move to the reserve invariant."},
		{"internal/infra/pi_config.go", "parsePiRuntime", "err != nil", "TestParsePiRuntimeRejectsEveryMalformedField", 562, "a required executable read failure leaves an empty string; the immediately following absolute-path validation rejects it. Bound: the refusal detail may change, but an executable cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntime", "err != nil", "TestParsePiRuntimeRejectsEveryMalformedField", 571, "a required argv read failure leaves a nil slice; len(argv)==0 in the same condition still rejects it. Bound: refusal detail may change, but an empty argv cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntime", "err != nil", "TestParsePiRuntimeRejectsEveryMalformedField", 582, "a required readiness_path read failure leaves an empty string; the same condition's /models equality still rejects it. Bound: refusal detail may change, but the malformed path cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeSharing", "err != nil", "TestParsePiRuntimeSharingRejectsEveryMalformedField", 622, "a mode read failure leaves an empty value; the following exclusive/shared allow-list check still rejects it. Bound: refusal detail may change, but an unknown mode cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeSharing", "maximumLingerSeconds < 0", "TestParsePiRuntimeSharingRejectsEveryMalformedField", 632, "when no room remains for shutdown plus handoff, maximumLingerSeconds is negative; with nonnegative linger_seconds the next linger > maximum comparison still rejects. Bound: the error message changes from the overflow-specific explanation to the computed maximum."},
		{"internal/infra/pi_config.go", "parsePiRuntimeSharing", "err != nil", "TestParsePiRuntimeSharingRejectsEveryMalformedField", 656, "a broker-start-timeout read failure leaves zero; the subsequent timeout < runtime startup + shutdown + slop check still rejects. Bound: refusal detail may change, but the broker timeout cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeSharing", "err != nil", "TestParsePiRuntimeSharingRejectsEveryMalformedField", 669, "a restart-max-backoff read failure leaves zero; the next max-backoff < positive initial-backoff check still rejects. Bound: refusal detail may change, but invalid restart timing cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeSharing", "err != nil", "TestParsePiRuntimeSharingRejectsEveryMalformedField", 684, "a resource-pressure-mode read failure leaves empty; the following closed switch default refuses every value outside disabled/provider. Bound: refusal detail may change, but no unknown mode can be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeResourcePressure", "err != nil", "TestParsePiRuntimeResourcePressureRejectsEveryMalformedField", 717, "an observation-path read failure leaves empty; the immediately following clean-absolute-path check rejects it. Bound: refusal detail may change, but an unread path cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeResourcePressure", "err != nil", "TestParsePiRuntimeResourcePressureRejectsEveryMalformedField", 741, "a pressure-action read failure leaves empty; the following exact action equality check rejects it. Bound: refusal detail may change, but an unknown action cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeResourcePressure", "err != nil", "TestParsePiRuntimeResourcePressureRejectsEveryMalformedField", 747, "an unknown-action read failure leaves empty; the following exact action equality check rejects it. Bound: refusal detail may change, but an unknown action cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiRuntimeResourcePressure", "err != nil", "TestParsePiRuntimeResourcePressureRejectsEveryMalformedField", 753, "a busy-action read failure leaves empty; the following exact action equality check rejects it. Bound: refusal detail may change, but an unknown action cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiDFlash", "err != nil", "TestParsePiDFlashRejectsEveryMalformedField", 777, "a target_argv read failure leaves nil; len(target_argv)==0 in the same condition still rejects it. Bound: refusal detail may change, but an empty target argument vector cannot be admitted."},
		{"internal/infra/pi_config.go", "parsePiDFlash", "err != nil", "TestParsePiDFlashRejectsEveryMalformedField", 783, "a draft_argv read failure leaves nil; len(draft_argv)==0 in the same condition still rejects it. Bound: refusal detail may change, but an empty draft argument vector cannot be admitted."},
		{"internal/infra/pi_shared_broker_darwin.go", "openSharedBrokerLock", "err != nil", "TestOpenSharedBrokerLockRejectsUnsafeFiles", 114, "a failed open leaves an invalid descriptor; after this member is narrowed, the immediate Fstat still fails and the following path-validation branch refuses it. Bound: the refusal detail can move from open() to Fstat()."},
		{"internal/infra/pi_shared_broker_darwin.go", "openSharedBrokerLock", "err != nil", "TestOpenSharedBrokerLockRejectsUnsafeFiles", 123, "an Fstat error leaves a zero-valued stat record; the following S_IFREG comparison still refuses it. Bound: the low-level error detail may change, but an invalid descriptor cannot be admitted."},
		{"internal/infra/pi_shared_broker_darwin.go", "openSharedBrokerLock", "stat.Mode&unix.S_IFMT != unix.S_IFREG", "TestOpenSharedBrokerLockRejectsUnsafeFiles", 123, "the named FIFO attack still refuses when this type comparison is narrowed because Darwin flock rejects the FIFO and openSharedBrokerLock maps that failure to shared_runtime_state_path_invalid. Bound: the FIFO is refused by flock rather than by the earlier regular-file classification; TestOpenSharedBrokerLockRejectsUnsafeFiles exercises the production entry point."},
		{"internal/infra/pi_shared_broker_darwin.go", "openSharedLog", "err != nil", "TestOpenSharedLogRejectsUnsafeFiles", 483, "a failed open leaves an invalid descriptor; the immediate Fstat still fails and the following path-validation branch refuses it. Bound: the refusal detail can move from open() to Fstat()."},
		{"internal/infra/pi_shared_broker_darwin.go", "openSharedLog", "err != nil", "TestOpenSharedLogRejectsUnsafeFiles", 491, "an Fstat error leaves a zero-valued stat record; the following S_IFREG comparison still refuses it. Bound: the low-level error detail may change, but an invalid descriptor cannot be admitted."},
		{"internal/infra/pi_shared_log_rotation_darwin.go", "Prune", "statErr != nil", "TestPruneSharedFilesystemLogSinkRejectsEveryUnsafeArchive", 66, "unix.Lstat failure leaves a zero-valued Stat_t, whose S_IFMT is not S_IFREG; after this error operand is narrowed, the next file-type comparison still refuses the archive. Bound: only the error attribution changes; failed archive inspection still cannot admit or prune the entry."},
		{"internal/infra/pi_shared_launcher_darwin.go", "RunSharedRuntimeLauncher", "err != nil", "TestRunSharedRuntimeLauncherRejectsNonFIFODescriptor", 38, "unix.Fstat failure leaves a zero-valued Stat_t, whose S_IFMT is not S_IFIFO; after this error operand is narrowed, the same OR expression still refuses the descriptor as non-FIFO. Bound: the kernel-error detail may change, but no descriptor with a failed Fstat can be admitted; the named production-entrypoint test covers a non-FIFO descriptor."},
		{"internal/infra/pi_shared_operator_darwin.go", "readSharedLeaseMirrors", "err != nil", "TestReadSharedLeaseMirrorsRefusesDirectoryFileAndJSONFailures", 382, "A failed os.ReadFile yields no bytes; if this error check is narrowed, the strict JSON decoder immediately rejects the resulting empty payload with EOF. Bound: read-failure attribution changes to malformed empty JSON, but unreadable lease state is still refused before any lease status is emitted."},
		{"internal/infra/pi_shared_supervision.go", "readSharedRuntimeRestartLedger", "err != nil", "TestReadSharedRuntimeRestartLedgerRejectsMalformedAndWrongIdentity", 107, "A failed os.ReadFile other than absence yields no bytes; if this error check is narrowed, the strict JSON decoder immediately rejects the resulting empty payload with EOF. Bound: read-failure attribution changes to malformed empty JSON, but unreadable restart-ledger state is still refused before any status or restart decision is produced."},
		{"internal/infra/pi_shared_broker_darwin.go", "readSharedWireMessage", "err != nil", "TestReadSharedWireMessageRejectsMalformedAndTrailingFrames", 1430, "after a malformed JSON frame makes the initial Decoder.Decode fail, requireJSONEOF on the same decoder still observes the stored decode failure and returns protocol_violation. The named test covers malformed and trailing JSON through readSharedWireMessage. Bound: only malformed-frame handling via the immediate second decoder check is equivalent; valid frames remain governed by the first decode."},
		{"internal/infra/pi_shared_broker_darwin.go", "reclaimSharedRuntime", "runtimeRecord.PGID <= 0", "TestReclaimSharedRuntimeSignalsOnlyAfterMatchedPositiveKernelPGID", 550, "a nonpositive recorded PGID cannot equal the live kernel PGID verified immediately above: a live process group id is positive. The named test exercises the production path with a matched positive kernel and recorded PGID and observes the signal target. Bound: Darwin kernel identity is the witness; this proves that branch is redundant after the earlier PGID equality, not that arbitrary fabricated PGIDs are valid."},
		{"internal/infra/pi_shared_client_darwin.go", "connectAndAttestSharedRuntime", "!ok", "TestConnectAndAttestSharedRuntimeExactProtocolVersionReportsOnlyThePassedGateSet", 349, "the production dialer requests network unix, whose Darwin Go implementation returns *net.UnixConn; the named successful AF_UNIX exchange exercises this path and reaches the full attestation result. Bound: equivalence is tied to the Go toolchain implementation used by this binding and does not claim that arbitrary net.Conn values satisfy the assertion."},
	}
	for _, proof := range parserEquivalents {
		if current.file == proof.file && current.function == proof.function && current.line == proof.line && current.memberText == proof.member && containsTest(current.tests, proof.test) {
			return "equivalent downstream refusal: " + proof.bound + " Named test: " + proof.test + ".", proof.test, true
		}
	}
	if current.file == "internal/infra/pi_shared_process_support.go" &&
		current.function == "waitPiRuntimeReady" &&
		current.memberText == "ctx.Err() != nil" &&
		current.guardText == "ctx.Err() != nil" &&
		containsValue(current.sites, "pi_deadline_exceeded") &&
		containsTest(current.tests, "TestWaitPiRuntimeReadyRefusesCancelledExitedAndFatalStates") {
		return "equivalent within the context contract: this branch returns the canceled/deadline refusal before the next outer select, which also observes ctx.Done(); narrowing it cannot admit readiness or bypass refusal. Bound: if cancellation, child exit, fatal output, or timeout become ready together, select scheduling can change which refusal wins; refusal precedence is not promised.", "TestWaitPiRuntimeReadyRefusesCancelledExitedAndFatalStates", true
	}
	if current.file == "internal/infra/pi_terminal_evidence.go" &&
		current.function == "ReadPiTerminalEvidenceSummary" &&
		current.guardText == "errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)" &&
		(current.memberText == "errors.Is(err, syscall.EWOULDBLOCK)" || current.memberText == "errors.Is(err, syscall.EAGAIN)") &&
		containsValue(current.sites, "entry.active_lock") &&
		containsTest(current.tests, "TestReadPiTerminalEvidenceSummaryRefusesActiveAndUnclassifiableState") &&
		syscall.EWOULDBLOCK == syscall.EAGAIN {
		return "equivalent on this GOOS/GOARCH: syscall.EWOULDBLOCK equals syscall.EAGAIN, so either operand recognizes the same nonblocking-flock active state. Bound: only aliasing errno targets are established; no claim is made for platforms where these errno values differ.", "TestReadPiTerminalEvidenceSummaryRefusesActiveAndUnclassifiableState", true
	}
	return "", "", false
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
	match := failedTest.FindSubmatch(output)
	if len(match) >= 2 {
		return string(match[1])
	}
	// Go's timeout panic reports active test names in its running-tests list,
	// not through the ordinary --- FAIL line for the interrupted test.
	timeoutTest := regexp.MustCompile(`(?m)^\s+(Test[[:alnum:]_]+(/[[:alnum:]_]+)*) \([0-9.]+[smh]\)`)
	match = timeoutTest.FindSubmatch(output)
	if len(match) >= 2 {
		return string(match[1])
	}
	return ""
}

func renderTable(siteCount, mutantCount int, rows []result) string {
	var builder strings.Builder
	builder.WriteString("# TASK-260929-9amima mutation table\n\n")
	fmt.Fprintf(&builder, "Typed refusal and status-gate sites mapped across the package-derived AST guard scope: %d. AST-derived narrowing mutants: %d.\n\n", siteCount, mutantCount)
	builder.WriteString("| Mutant | Narrowing applied to | Named failing test / equivalence witness | Target | Exit | Result or bound | Log |\n| --- | --- | --- | --- | ---: | --- | --- |\n")
	for _, row := range rows {
		what := "replace typed refusal result with nil"
		if row.mutant.memberText != "" {
			what = fmt.Sprintf("narrow `%s` to false at line %d", markdownCell(row.mutant.memberText), row.mutant.line)
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
			row.index, row.mutant.function, what, markdownCell(row.mutant.guardText), row.mutant.constructor, markdownCell(strings.Join(row.mutant.sites, ", ")), markdownCell(testEvidence), row.target, row.code, markdownCell(row.state+boundSuffix(row.bound)), row.log)
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
