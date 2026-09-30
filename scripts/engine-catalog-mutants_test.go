package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProductionPackageScopeIncludesWindowsTaggedRefusalsAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module scopefixture\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"root.go":                "package scopefixture\n",
		"newpkg/refusal.go":      "//go:build windows\n\npackage newpkg\ntype typedRefusal struct{}\nfunc (typedRefusal) Error() string { return \"refused\" }\nfunc Refuse() error { return typedRefusal{} }\n",
		"newpkg/refusal_test.go": "package newpkg\n",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := productionPackageScope(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"newpkg/refusal.go", "root.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filesystem walk omitted build-tagged production files: got=%v want=%v", got, want)
	}
	sites, err := collectSites(root, got)
	if err != nil {
		t.Fatal(err)
	}
	foundTypedRefusal := false
	for _, site := range sites {
		if site.file == "newpkg/refusal.go" {
			foundTypedRefusal = true
		}
	}
	if !foundTypedRefusal {
		t.Fatal("windows-only typed refusal was not inventoried")
	}
	if err := os.Symlink(filepath.Join(root, "newpkg", "refusal.go"), filepath.Join(root, "newpkg", "linked.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := productionPackageScope(root); err == nil || !strings.Contains(err.Error(), "linked.go") {
		t.Fatalf("module-wide scope did not refuse symlinked source by name: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "newpkg", "linked.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "newpkg"), filepath.Join(root, "linked_dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := productionPackageScope(root); err == nil || !strings.Contains(err.Error(), "linked_dir") {
		t.Fatalf("module-wide scope did not refuse symlinked directory by name: %v", err)
	}
}

func TestTestsNamingSiteSelectsOnlyMatchingRefusalSubtests(t *testing.T) {
	source := `package infra
func TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness(t *testing.T) {
	tests := []struct { name, code string }{
		{name: "peer uid root narrowing", code: "broker_peer_untrusted"},
		{name: "broker executable same inode wrong device", code: "broker_executable_identity_mismatch"},
		{name: "broker executable same device wrong inode", code: "broker_executable_identity_mismatch"},
	}
	for _, testCase := range tests { t.Run(testCase.name, func(t *testing.T) { require(testCase.code) }) }
}`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness" {
			body = function.Body
		}
	}
	if body == nil {
		t.Fatal("test fixture function was not parsed")
	}
	candidate := parsedTest{reference: testReference{Name: "TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness", Dir: "internal/infra"}, body: body}
	got := testsNamingSite([]parsedTest{candidate}, "connectAndAttestSharedRuntime", "broker_executable_identity_mismatch")
	want := []testReference{
		{Name: "TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness/broker_executable_same_device_wrong_inode", Dir: "internal/infra"},
		{Name: "TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness/broker_executable_same_inode_wrong_device", Dir: "internal/infra"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected tests=%#v want=%#v", got, want)
	}
}

func TestMatchingSubtestNamesPreservesNestedParentsAndDirectSiteWitnesses(t *testing.T) {
	source := `package infra
func TestReadPiTerminalEvidenceSummaryRefusesActiveAndUnclassifiableState(t *testing.T) {
	t.Run("record and filesystem members", func(t *testing.T) {
		cases := []struct { name, site string }{
			{name: "schema version", site: "entry.record"},
			{name: "directory device identity", site: "entry.identity"},
		}
		for _, testCase := range cases { t.Run(testCase.name, func(t *testing.T) { requireSite(testCase.site) }) }
	})
	t.Run("active identity fstat error", func(t *testing.T) { requireSite("entry.identity") })
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	function := file.Decls[0].(*ast.FuncDecl)
	gotRecord := matchingSubtestNames(function.Body, "entry.record")
	wantRecord := []string{"record_and_filesystem_members/schema_version"}
	if !reflect.DeepEqual(gotRecord, wantRecord) {
		t.Fatalf("nested record witnesses=%#v want=%#v", gotRecord, wantRecord)
	}
	gotIdentity := matchingSubtestNames(function.Body, "entry.identity")
	wantIdentity := []string{
		"record_and_filesystem_members/directory_device_identity",
		"active_identity_fstat_error",
	}
	if !reflect.DeepEqual(gotIdentity, wantIdentity) {
		t.Fatalf("nested identity witnesses=%#v want=%#v", gotIdentity, wantIdentity)
	}
}

func TestTestsNamingSiteFindsTypedErrorWitness(t *testing.T) {
	source := `package infra
func TestConnectAndAttestSharedRuntimeClassifiesAbsentAndRefusedSocketsForAcquisition(t *testing.T) {
	var refusal *sharedConnectError
	_ = refusal
}`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	function := file.Decls[0].(*ast.FuncDecl)
	candidate := parsedTest{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body}
	got := testsNamingSite([]parsedTest{candidate}, "connectAndAttestSharedRuntime", "type:sharedConnectError")
	want := []testReference{{Name: function.Name.Name, Dir: "internal/infra"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected tests=%#v want=%#v", got, want)
	}
}

func TestTestPatternsKeepSubtestAlternativesScopedToTheirParent(t *testing.T) {
	got := testPatterns([]string{
		"TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness/broker_executable_same_inode_wrong_device",
		"TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness/broker_executable_same_device_wrong_inode",
		"TestReadSharedWireMessageRejectsMalformedAndTrailingFrames",
	})
	want := []string{
		"^TestConnectAndAttestSharedRuntimeRejectsEveryGateDeleteAndNarrowWitness$/^(broker_executable_same_device_wrong_inode|broker_executable_same_inode_wrong_device)$",
		"^TestReadSharedWireMessageRejectsMalformedAndTrailingFrames$",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns=%#v want=%#v", got, want)
	}
}

func TestEquivalentNarrowingUsesNamedDownstreamFilesystemWitnesses(t *testing.T) {
	for _, mutant := range []mutant{
		{
			file: "internal/infra/pi_shared_protocol.go", function: "ResolveSharedRuntimePaths",
			memberText: "err != nil", line: 253,
			tests: []testReference{{Name: "TestResolveSharedRuntimePathsRefusesCacheRootFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "ResolveSharedRuntimePaths",
			memberText: "err != nil", line: 257,
			tests: []testReference{{Name: "TestResolveSharedRuntimePathsRefusesCacheRootFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "ResolveSharedRuntimePaths",
			memberText: "relErr != nil", line: 285,
			tests: []testReference{{Name: "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "ResolveSharedRuntimePaths",
			memberText: `strings.HasPrefix(rel, ".."+string(filepath.Separator))`, line: 285,
			tests: []testReference{{Name: "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "ResolveSharedRuntimePaths",
			memberText: "filepath.IsAbs(rel)", line: 285,
			tests: []testReference{{Name: "TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "CreateSharedRuntimeTree",
			memberText: "err != nil", line: 299,
			tests: []testReference{{Name: "TestCreateSharedRuntimeTreeRefusesEveryUnsafePath", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "readSharedBrokerRecord",
			memberText: "err != nil", line: 411,
			tests: []testReference{{Name: "TestReadSharedBrokerRecordRefusesReadAndDecodeFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "writeSharedJSONAtomic",
			memberText: "err != nil", line: 435,
			tests: []testReference{{Name: "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "writeSharedJSONAtomic",
			memberText: "err != nil", line: 445,
			tests: []testReference{{Name: "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_protocol.go", function: "writeSharedJSONAtomic",
			memberText: "err != nil", line: 440,
			tests: []testReference{{Name: "TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_log_rotation_darwin.go", function: "Prune",
			memberText: "statErr != nil", line: 66,
			tests: []testReference{{Name: "TestPruneSharedFilesystemLogSinkRejectsEveryUnsafeArchive/fifo_type", Dir: "internal/infra"}},
		},
		{
			file: "internal/infra/pi_shared_operator_darwin.go", function: "readSharedLeaseMirrors",
			memberText: "err != nil", line: 382,
			tests: []testReference{{Name: "TestReadSharedLeaseMirrorsRefusesDirectoryFileAndJSONFailures", Dir: "internal/infra"}},
		},
	} {
		if bound, witness, ok := equivalentNarrowing(mutant); !ok || bound == "" || witness == "" {
			t.Fatalf("equivalent narrowing not documented for %#v: bound=%q witness=%q ok=%t", mutant, bound, witness, ok)
		}
	}
}

func TestProductionCallersDerivesHelperCallGraphFromAST(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "infra.go")
	source := `package infra
func RunSharedRuntimeLauncher() error { return sharedRuntimeMismatch("runtime_authorization_mismatch", "schema") }
func sharedRuntimeMismatch(code, field string) error { return nil }
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	callers, err := productionCallers(root, []string{"infra.go"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"RunSharedRuntimeLauncher"}
	if !reflect.DeepEqual(callers["sharedRuntimeMismatch"], want) {
		t.Fatalf("derived callers=%#v want=%#v", callers["sharedRuntimeMismatch"], want)
	}
}

func TestTestsForRefusalSiteUsesASTDerivedCallerWitness(t *testing.T) {
	source := `package infra
func TestRunSharedRuntimeLauncherRefusesEveryMismatch(t *testing.T) { requireCode("runtime_authorization_mismatch") }
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	function := file.Decls[0].(*ast.FuncDecl)
	candidates := []parsedTest{{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body}}
	site := refusalSite{function: "sharedRuntimeMismatch", site: "%s differs"}
	got := testsForRefusalSite(candidates, map[string][]string{"sharedRuntimeMismatch": {"RunSharedRuntimeLauncher"}}, site)
	want := []testReference{{Name: function.Name.Name, Dir: "internal/infra"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("derived test witnesses=%#v want=%#v", got, want)
	}
}

func TestTestsForRefusalSitePrefersDirectASTTestCalls(t *testing.T) {
	source := `package infra
func TestEngineCatalogReaderRejectsMalformedEntryName(t *testing.T) {
	for _, testCase := range []struct { name, site string }{{name: "bad name", site: "entry.name"}} {
		t.Run(testCase.name, func(t *testing.T) { ReadEngineCatalogEntry("", testCase.name); requireSite(testCase.site) })
	}
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	function := file.Decls[0].(*ast.FuncDecl)
	candidates := []parsedTest{{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body}}
	site := refusalSite{function: "ReadEngineCatalogEntry", site: "entry.name"}
	got := testsForRefusalSite(candidates, nil, site)
	want := []testReference{{Name: "TestEngineCatalogReaderRejectsMalformedEntryName/bad_name", Dir: "internal/infra"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct AST test witnesses=%#v want=%#v", got, want)
	}
}

func TestTestsForRefusalSiteFindsNegativeSubtestThroughProductionCaller(t *testing.T) {
	source := `package infra
func TestCatalogLegacyFixtureAcceptance(t *testing.T) {
	if err := validateEngineCatalogEntry("accepted", EngineCatalogEntry{}); err != nil { t.Fatal(err) }
}
func TestEngineCatalogValidationRefusesMalformedEntryMembers(t *testing.T) {
	for _, testCase := range []struct { name, site string }{{name: "unknown engine kind", site: "entry.engine"}} {
		t.Run(testCase.name, func(t *testing.T) { ReadEngineCatalogEntry("", "invalid"); requireSite(testCase.site) })
	}
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []parsedTest
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body})
	}
	site := refusalSite{function: "validateEngineCatalogEntry", site: "entry.engine"}
	callers := map[string][]string{"validateEngineCatalogEntry": {"ReadEngineCatalogEntry"}}
	got := testsForRefusalSite(candidates, callers, site)
	want := []testReference{{Name: "TestEngineCatalogValidationRefusesMalformedEntryMembers/unknown_engine_kind", Dir: "internal/infra"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("site-scoped caller test witnesses=%#v want=%#v", got, want)
	}
}

func TestTestsForTypedLiteralRefusalIncludesDirectFunctionTests(t *testing.T) {
	source := `package infra
func TestSharedRuntimeBeginAttemptRefusesActiveAutomaticQuarantine(t *testing.T) {
	_ = sharedRuntimeBeginAttempt(ledger, now)
}
func TestSharedRuntimeManualQuarantinePersistsAndRefusesAttempt(t *testing.T) {
	_ = sharedRuntimeBeginAttempt(ledger, now)
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []parsedTest
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body})
	}
	site := refusalSite{function: "sharedRuntimeBeginAttempt", constructor: "SharedRuntimeError", site: "shared runtime is manually quarantined"}
	got := testsForRefusalSite(candidates, nil, site)
	want := []testReference{
		{Name: "TestSharedRuntimeBeginAttemptRefusesActiveAutomaticQuarantine", Dir: "internal/infra"},
		{Name: "TestSharedRuntimeManualQuarantinePersistsAndRefusesAttempt", Dir: "internal/infra"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed literal test witnesses=%#v want=%#v", got, want)
	}
}

func TestTestsForTypedLiteralRefusalIncludesNamedProductionPathWitness(t *testing.T) {
	source := `package infra
func TestSharedBrokerAcquireLeaseRefusesNonceAndTokenReadFailures(t *testing.T) {
	_ = fixture.acquireLease()
}
func TestSharedBrokerAcquireLeaseRefusesPolicyPressureSupersededGenerationAndLeaseLimit(t *testing.T) {
	response := sendProductionAcquire(t, fixture)
	_ = response
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "guard_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []parsedTest
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body})
	}
	site := refusalSite{function: "acquireLease", constructor: "SharedRuntimeError", site: "effective shared runtime lease limit reached"}
	got := testsForRefusalSite(candidates, nil, site)
	want := []testReference{
		{Name: "TestSharedBrokerAcquireLeaseRefusesNonceAndTokenReadFailures", Dir: "internal/infra"},
		{Name: "TestSharedBrokerAcquireLeaseRefusesPolicyPressureSupersededGenerationAndLeaseLimit", Dir: "internal/infra"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed literal test witnesses=%#v want=%#v", got, want)
	}
}

func TestAvailabilityAttestationNarrowingMapsToCLIRegression(t *testing.T) {
	root := t.TempDir()
	productionPath := filepath.Join(root, "internal", "infra", "status.go")
	if err := os.MkdirAll(filepath.Dir(productionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	production := `package infra
func reportStatus(resolved resolvedState, report *statusReport) {
	if resolved.CatalogEntry {
		report.EngineAvailability = "available"
	}
}`
	if err := os.WriteFile(productionPath, []byte(production), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, productionPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sites []refusalSite
	collectCatalogAvailabilityAttestationGates("internal/infra/status.go", fset, file, &sites)
	if len(sites) != 1 || sites[0].site != "available" || sites[0].guardExpr == nil {
		t.Fatalf("availability attestation sites=%#v want one guarded available assignment", sites)
	}

	testSource := `package main
func TestCuratorEnginesCatalogIdentifierLifecycleThroughRun(t *testing.T) {
	var status SharedRuntimeStatus
	if code := run([]string{"status", "--engine", "qwen-local", "--json"}, &stdout, &stderr); code != 0 { t.Fatal(code) }
	if status.EngineAvailability != "available" { t.Fatal(status.EngineAvailability) }
}`
	testFile, err := parser.ParseFile(token.NewFileSet(), "catalog_cli_test.go", testSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	candidate := parsedTest{reference: testReference{Name: "TestCuratorEnginesCatalogIdentifierLifecycleThroughRun", Dir: "cmd/curator-engines"}, body: testFile.Decls[0].(*ast.FuncDecl).Body}
	callers := map[string][]string{"reportStatus": {"run"}}
	sites[0].tests = testsForAvailabilityAttestationGate([]parsedTest{candidate}, callers, sites[0])
	wantTests := []testReference{{Name: candidate.reference.Name, Dir: candidate.reference.Dir}}
	if !reflect.DeepEqual(sites[0].tests, wantTests) {
		t.Fatalf("availability gate test witnesses=%#v want=%#v", sites[0].tests, wantTests)
	}

	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutants) != 1 || mutants[0].memberText != "resolved.CatalogEntry" || !strings.Contains(mutants[0].replaceText, "&& false") {
		t.Fatalf("availability narrowing mutants=%#v want one CatalogEntry-to-false mutant", mutants)
	}
}

func TestCatalogResolutionNarrowingGatesMapToResolverRegressions(t *testing.T) {
	root := t.TempDir()
	productionPath := filepath.Join(root, "internal", "infra", "pi_shared_resolve.go")
	if err := os.MkdirAll(filepath.Dir(productionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	production := `package infra
func resolveSharedProfile() (any, error) {
	entry, state, catalogErr := ReadEngineCatalogEntry()
	_ = entry
	if catalogErr != nil { return nil, catalogErr }
	if state == EngineCatalogPresent { return resolveCatalogProfile() }
	return resolveLegacyProfile(), nil
}`
	if err := os.WriteFile(productionPath, []byte(production), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, productionPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sites []refusalSite
	collectCatalogResolutionDecisionGates("internal/infra/pi_shared_resolve.go", fset, file, &sites)
	if len(sites) != 2 {
		t.Fatalf("catalog resolution gates=%#v want read-failure and catalog-first gates", sites)
	}

	testSource := `package infra
func TestResolveSharedProfileDoesNotFallBackAfterCatalogReadFailure(t *testing.T) {
	_, err := resolveSharedProfile()
	var catalogError *EngineCatalogError
	if !errors.As(err, &catalogError) || catalogError.State != EngineCatalogReadFailed { t.Fatal(err) }
}
func TestResolveSharedProfileUsesCatalogForSelectedPrimaryProfile(t *testing.T) {
	resolved, _ := resolveSharedProfile()
	if !resolved.CatalogEntry { t.Fatal("legacy profile selected") }
}`
	testFile, err := parser.ParseFile(token.NewFileSet(), "resolve_catalog_test.go", testSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []parsedTest
	for _, declaration := range testFile.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Body != nil && strings.HasPrefix(function.Name.Name, "Test") {
			candidates = append(candidates, parsedTest{reference: testReference{Name: function.Name.Name, Dir: "internal/infra"}, body: function.Body})
		}
	}
	for index := range sites {
		sites[index].tests = testsForRefusalSite(candidates, nil, sites[index])
	}
	wantByConstructor := map[string]string{
		"catalog-first resolution gate":         "TestResolveSharedProfileUsesCatalogForSelectedPrimaryProfile",
		"catalog read-failure propagation gate": "TestResolveSharedProfileDoesNotFallBackAfterCatalogReadFailure",
	}
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutants) != 2 {
		t.Fatalf("catalog decision mutants=%#v want one narrowing mutant per decision gate", mutants)
	}
	for _, mutant := range mutants {
		want := wantByConstructor[mutant.constructor]
		if want == "" || len(mutant.tests) != 1 || mutant.tests[0].Name != want || !strings.Contains(mutant.replaceText, "&& false") {
			t.Fatalf("catalog decision mutant=%#v want mapped narrowing witness %q", mutant, want)
		}
	}
}
