package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mutationCommandFixture(t *testing.T, entry, tests string) string {
	t.Helper()
	root := t.TempDir()
	writeMutationFile(t, root, "go.mod", "module fixture\n\ngo 1.25.5\n")
	writeMutationFile(t, root, "gate.go", `package fixture
 type refusal struct{}
 func (*refusal) Error()string{return "refused"}
 func refuse()error{return &refusal{}}
 `+entry)
	writeMutationFile(t, root, "gate_test.go", "package fixture\nimport \"testing\"\n"+tests)
	return root
}

func writeMutationFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func mutationCommand(t *testing.T, root string, args ...string) ([]byte, int) {
	t.Helper()
	script, err := filepath.Abs("engine-catalog-mutants.go")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", append([]string{"run", script}, args...)...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if command.ProcessState == nil {
		t.Fatal(err)
	}
	code := command.ProcessState.ExitCode()
	t.Logf("go run %v exit=%d\n%s", args, code, output)
	return output, code
}

func writeMutationMappings(t *testing.T, root, admit string) {
	t.Helper()
	files, err := productionPackageScope(root)
	if err != nil {
		t.Fatal(err)
	}
	sites, _, err := typedPropagationSites(root, files)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := loadTests(root, files)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("fixture tests: %v %v", candidates, err)
	}
	ref := candidates[0].reference
	ref.BodySHA256 = candidates[0].bodyDigest
	var mappings []siteMapping
	for _, site := range sites {
		mapping := siteMapping{Site: site.id, Tests: []testReference{ref}}
		if !site.unconditional {
			mapping.Witnesses = []narrowingWitness{{Admit: admit}}
		}
		mappings = append(mappings, mapping)
	}
	data, err := json.Marshal(mappings)
	if err != nil {
		t.Fatal(err)
	}
	writeMutationFile(t, root, "scripts/engine-catalog-sites.json", string(data))
}

func TestRefusalEmulationRejectsChangedMappedTestBody(t *testing.T) {
	root := mutationCommandFixture(t, "", `func TestSharedRuntimePublicOperationsRefuseUnsupportedPlatforms(t *testing.T){if unsupportedSharedRuntimePlatform()==nil{t.Fatal("unsupported platform admitted")}}`)
	// Use the production routing filename to exercise main -> mutationTarget ->
	// runMutant against the caller-supplied Linux execution root.
	for _, platform := range []string{"posix", "darwin"} {
		tag := "!darwin && !windows"
		if platform == "darwin" {
			tag = "darwin"
		}
		writeMutationFile(t, root, "internal/infra/pi_shared_unsupported_"+platform+".go", "//go:build "+tag+"\n\npackage infra\nfunc unsupportedSharedRuntimePlatform()error{return &refusal{}}")
	}
	// Move fixture declarations and tests into the routed package.
	for _, name := range []string{"gate.go", "gate_test.go"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		writeMutationFile(t, root, "internal/infra/"+name, strings.Replace(string(data), "package fixture", "package infra", 1))
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeMutationMappings(t, root, "")
	data, code := mutationCommand(t, root, "-inventory")
	if code != 0 {
		t.Fatal(string(data))
	}
	var inventory inventoryReport
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	index := -1
	for i, m := range inventory.Mutants {
		if m.File == "internal/infra/pi_shared_unsupported_posix.go" {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("Linux mutation absent")
	}
	emu := t.TempDir()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, _ := filepath.Rel(root, path)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, ".go") {
			body = []byte(goBuildDirective.ReplaceAllString(string(body), "//go:build !darwin"))
			if strings.Contains(name, "_posix.go") {
				body = []byte(goBuildDirective.ReplaceAllString(string(body), "//go:build darwin"))
			}
			name = strings.ReplaceAll(name, "_darwin.go", "_darwin_emu.go")
		}
		writeMutationFile(t, emu, name, string(body))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	args := []string{"-out", ".temp/mutants", "-start", fmt.Sprint(index), "-count", "1", "-workers", "1", "-linux-emu-root", emu}
	data, code = mutationCommand(t, root, args...)
	if code != 0 {
		t.Fatalf("exact projection refused: %s", data)
	}
	testPath := filepath.Join(emu, "internal/infra/gate_test.go")
	original, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatal(err)
	}
	writeMutationFile(t, emu, "internal/infra/gate_test.go", strings.Replace(string(original), "{if unsupported", `{t.Fatal("unrelated pre-existing failure");if unsupported`, 1))
	baseline := exec.Command("go", "test", "./internal/infra", "-run", "^TestSharedRuntimePublicOperationsRefuseUnsupportedPlatforms$", "-count=1")
	baseline.Dir = emu
	output, err := baseline.CombinedOutput()
	t.Logf("forged baseline exit=%d\n%s", baseline.ProcessState.ExitCode(), output)
	if err == nil || !strings.Contains(string(output), "--- FAIL: TestSharedRuntimePublicOperationsRefuseUnsupportedPlatforms") {
		t.Fatal("reviewer pre-existing named failure not reproduced")
	}
	data, code = mutationCommand(t, root, args...)
	if code == 0 {
		t.Fatalf("changed mapped test counted as kill: %s", data)
	}
	log, err := os.ReadFile(filepath.Join(root, ".temp/mutants/generated", fmt.Sprintf("%03d", index), "go-test.log"))
	if err != nil || !strings.Contains(string(log), "test root identity mismatch") {
		t.Fatalf("wrong refusal: %v %s", err, log)
	}
	// Extra, missing and helper-body changes are changes to the execution set,
	// even when the mapped test's own name/body remains untouched.
	writeMutationFile(t, emu, "internal/infra/gate_test.go", string(original))
	refs := []testReference{{Dir: "internal/infra", Name: "TestSharedRuntimePublicOperationsRefuseUnsupportedPlatforms"}}
	writeMutationFile(t, emu, "internal/infra/extra_test.go", "package infra\nimport \"testing\"\nfunc TestUnrelated(t *testing.T){t.Fatal(\"unrelated\")}")
	if _, err := bindMutationTestRoot(root, emu, refs); err == nil {
		t.Fatal("additional test file accepted")
	}
}

func TestRefusalMutationRejectsAlreadyFailingBoundBaseline(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value<0 || label=="bad"{return errors.New("refused")}`)
	writeMutationFile(t, root, "gate_test.go", `package fixture
 import "testing"
 func TestRejectsMinusOne(t *testing.T){t.Fatal("pre-existing failure")}`)
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	row := runMutant(root, t.TempDir(), mutants[0], 0, "", "")
	if row.state != "invalid" || row.code != 1 || row.test != "" || !strings.Contains(row.bound, "baseline failed") {
		t.Fatalf("baseline failure counted as kill: %#v", row)
	}
}

func TestRefusalNarrowingPreservesOtherRejectedEngines(t *testing.T) {
	root := mutationCommandFixture(t, `type record struct{Engine,Mode string}
 func entry(value record)error{if value.Mode=="local"{if value.Engine!="llama-cpp"{return refuse()}};return nil}`, `func TestRejectsFuture(t *testing.T){if entry(record{Engine:"future",Mode:"local"})==nil{t.Fatal("future admitted")}}
 func TestRetainsOtherEngine(t *testing.T){if entry(record{Engine:"another-future",Mode:"local"})==nil{t.Fatal("other engine admitted")}}`)
	files, err := productionPackageScope(root)
	if err != nil {
		t.Fatal(err)
	}
	sites, _, err := typedPropagationSites(root, files)
	if err != nil {
		t.Fatal(err)
	}
	var selected refusalSite
	for _, site := range sites {
		if site.function == "entry" {
			selected = site
		}
	}
	selected.witnesses = []narrowingWitness{{Admit: `value.Engine == "future"`}}
	selected.tests = []testReference{{Dir: ".", Name: "TestRejectsFuture"}, {Dir: ".", Name: "TestRetainsOtherEngine"}}
	mutants, err := deriveMutants(root, []refusalSite{selected})
	if err != nil {
		t.Fatal(err)
	}
	rows := runMutants(root, t.TempDir(), mutants, 0, 1, "", "")
	logMutationEvidence(t, 1, rows)
	if rows[0].state != "killed" || rows[0].test != "TestRejectsFuture" {
		t.Fatalf("no singleton kill: %#v", rows)
	}
	overlay := filepath.Join(filepath.Dir(rows[0].log), "overlay.json")
	command := exec.Command("go", "test", "-overlay="+overlay, "-run", "^TestRetainsOtherEngine$", "-count=1", "./...")
	command.Dir = root
	data, err := command.CombinedOutput()
	t.Logf("retained sibling class exit=%d\n%s", command.ProcessState.ExitCode(), data)
	if err != nil {
		t.Fatal(err)
	}
}

func behavioralMutationFixture(t *testing.T, guards string) (string, []refusalSite) {
	t.Helper()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module witnessfixture\n\ngo 1.25.5\n")
	source := `package fixture
 import "errors"
 func entry(value int,label string)error {` + guards + `;return nil}`
	write("gate.go", source)
	write("gate_test.go", `package fixture
 import "testing"
 func TestRejectsMinusOne(t *testing.T){if entry(-1,"good")==nil{t.Fatal("minus one admitted")}}
 func TestRejectsMinusTwo(t *testing.T){if entry(-2,"good")==nil{t.Fatal("minus two admitted")}}
 func TestRejectsBadLabel(t *testing.T){if entry(0,"bad")==nil{t.Fatal("bad label admitted")}}
 `)
	baseline := exec.Command("go", "test", "-count=1", "./...")
	baseline.Dir = root
	output, err := baseline.CombinedOutput()
	t.Logf("baseline go test exit=%d\n%s", baseline.ProcessState.ExitCode(), output)
	if err != nil {
		t.Fatal(err)
	}
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, filepath.Join(root, "gate.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sites []refusalSite
	ast.Inspect(file, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		sites = append(sites, refusalSite{id: "gate.go:entry:" + string(rune('1'+len(sites))), file: "gate.go", function: "entry", constructor: "fixture refusal", guard: branch, guardExpr: branch.Cond, line: set.Position(branch.Pos()).Line, fset: set, witnesses: []narrowingWitness{{Admit: "value == -1", Member: 0}}, tests: []testReference{{Dir: ".", Name: "TestRejectsMinusOne"}, {Dir: ".", Name: "TestRejectsMinusTwo"}, {Dir: ".", Name: "TestRejectsBadLabel"}}})
		return true
	})
	return root, sites
}
func logMutationEvidence(t *testing.T, sites int, rows []result) {
	t.Helper()
	t.Log("\n" + renderTable(sites, len(rows), rows))
	for _, row := range rows {
		data, err := os.ReadFile(row.log)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s exit=%d\n%s", row.mutant.key, row.code, data)
		if row.jointLog != "" {
			data, err := os.ReadFile(row.jointLog)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("joint exit=%d\n%s", row.jointCode, data)
		}
	}
}
func TestRefusalNarrowingMutantsRetainGateAndAdmitOneMember(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value<0 || label=="bad" {return errors.New("refused")}`)
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutants) != 1 || !strings.Contains(mutants[0].replaceText, "value<0 || label==\"bad\"") || strings.Contains(mutants[0].replaceText, "&& false") {
		t.Fatalf("not a retained gate: %#v", mutants)
	}
	rows := runMutants(root, t.TempDir(), mutants, 0, 1, "", "")
	logMutationEvidence(t, len(sites), rows)
	if rows[0].state != "killed" || rows[0].test != "TestRejectsMinusOne" || rows[0].code != 1 {
		t.Fatalf("narrowing not killed by singleton witness: %#v", rows[0])
	}
	// Run the unaffected conjunct and second member separately against the same
	// overlay: killing the singleton alone cannot prove the retained class.
	overlay := filepath.Join(filepath.Dir(rows[0].log), "overlay.json")
	command := exec.Command("go", "test", "-count=1", "-overlay="+overlay, "-run", "^TestRejects(MinusTwo|BadLabel)$", "./...")
	command.Dir = root
	data, err := command.CombinedOutput()
	t.Logf("retained class go test exit=%d\n%s", command.ProcessState.ExitCode(), data)
	if err != nil {
		t.Fatal(err)
	}
}
func TestRefusalEquivalenceRequiresExecutedJointDownstreamMutation(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value<0 {return errors.New("first")};if value<0 || label=="bad" {return errors.New("downstream")}`)
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	rows := runMutants(root, t.TempDir(), mutants, 0, 1, "", "")
	logMutationEvidence(t, len(sites), rows)
	if rows[0].state != "equivalent" || rows[0].test != "TestRejectsMinusOne" || !strings.Contains(rows[0].bound, "joint") {
		t.Fatalf("joint equivalence not executed: %#v", rows[0])
	}
	if rows[1].state != "survived" || rows[1].test != "" || rows[1].bound == "" {
		t.Fatalf("unproven survivor misreported: %#v", rows[1])
	}
	for _, bad := range []result{{state: "killed", code: 1}, {state: "survived", code: 0}, {state: "invalid", code: 1, test: "TestRejectsMinusOne"}} {
		if provedJointEquivalence(result{state: "survived", code: 0, mutant: mutants[0]}, result{state: "survived", code: 0}, bad) {
			t.Fatal("unexecuted or unnamed proof admitted")
		}
	}
}
func TestRefusalNarrowingRejectsOperandDeletionAndUnboundPredicates(t *testing.T) {
	for _, predicate := range []string{"false", "true", "value < 0", "value == -1 || value == -2", "value == other", "(value<0) == true", `value == -1 && label == "good"`, "len(value) == 6", "value[0] == 'f'", "value[:] == nil", `value.Error() == "bad"`} {
		if _, err := singletonPredicate(predicate); err == nil {
			t.Fatalf("non-singleton predicate accepted: %s", predicate)
		}
	}
	if _, err := singletonPredicate(`value == -1`); err != nil {
		t.Fatal(err)
	}
	root, sites := behavioralMutationFixture(t, `if value<0 || label=="bad" {return errors.New("refused")}`)
	sites[0].witnesses = nil
	if _, err := deriveMutants(root, sites); err == nil {
		t.Fatal("missing narrowing witness treated as coverage")
	}
}

// These are the review's length/prefix reproducers through the real inventory
// entry point. The accepted control narrows the complete Engine string and
// retains a different rejected string in the behavioral suite.
func TestRefusalWitnessCannotCoarsenGuardedValue(t *testing.T) {
	root := mutationCommandFixture(t, `type record struct{Engine string}
 func validateEngineCatalogEntry(entry record)error{if entry.Engine!="llama-cpp"{return refuse()};return nil}`, `func TestRejectsFuture(t *testing.T){if validateEngineCatalogEntry(record{Engine:"future"})==nil{t.Fatal("future admitted")}}
 func TestRetainsFoobar(t *testing.T){if validateEngineCatalogEntry(record{Engine:"foobar"})==nil{t.Fatal("foobar admitted")}}`)
	// This fixture deliberately has two tests; construct exact certificates.
	files, err := productionPackageScope(root)
	if err != nil {
		t.Fatal(err)
	}
	sites, _, err := typedPropagationSites(root, files)
	if err != nil {
		t.Fatal(err)
	}
	tests, err := loadTests(root, files)
	if err != nil {
		t.Fatal(err)
	}
	var refs []testReference
	for _, test := range tests {
		ref := test.reference
		ref.BodySHA256 = test.bodyDigest
		refs = append(refs, ref)
	}
	write := func(admit string) {
		t.Helper()
		var mappings []siteMapping
		for _, site := range sites {
			m := siteMapping{Site: site.id, Tests: refs}
			if !site.unconditional {
				m.Witnesses = []narrowingWitness{{Admit: admit}}
			}
			mappings = append(mappings, m)
		}
		data, err := json.Marshal(mappings)
		if err != nil {
			t.Fatal(err)
		}
		writeMutationFile(t, root, "scripts/engine-catalog-sites.json", string(data))
	}
	write(`entry.Engine == "future"`)
	if data, code := mutationCommand(t, root, "-inventory"); code != 0 {
		t.Fatalf("singleton control refused: %s", data)
	}
	// Execute the singleton and its retention control, not just static checking.
	for _, site := range sites {
		if site.function != "validateEngineCatalogEntry" {
			continue
		}
		site.witnesses = []narrowingWitness{{Admit: `entry.Engine == "future"`}}
		site.tests = refs
		mutants, err := deriveMutants(root, []refusalSite{site})
		if err != nil {
			t.Fatal(err)
		}
		row := runMutant(root, t.TempDir(), mutants[0], 0, "", "")
		if row.state != "killed" || row.test != "TestRejectsFuture" {
			t.Fatalf("singleton control: %#v", row)
		}
		command := exec.Command("go", "test", "-overlay="+filepath.Join(filepath.Dir(row.log), "overlay.json"), "-run", "^TestRetainsFoobar$", "-count=1", "./...")
		command.Dir = root
		data, err := command.CombinedOutput()
		t.Logf("retention exit=%d\n%s", command.ProcessState.ExitCode(), data)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, admit := range []string{`len(entry.Engine) == 6`, `entry.Engine[0] == 'f'`, `entry.Engine == "future" && entry.Engine == "foobar"`, `entry.Engine[:1] == "f"`} {
		t.Run(admit, func(t *testing.T) {
			write(admit)
			data, code := mutationCommand(t, root, "-inventory")
			if code == 0 || !strings.Contains(string(data), "singleton equality") {
				t.Fatalf("projection certified: exit %d %s", code, data)
			}
		})
	}
}

func TestRefusalCompletenessRefusesUnnamedFailuresAndSurvivors(t *testing.T) {
	for _, row := range []result{
		{state: "invalid", code: 1},
		{state: "killed", code: 1},
		{state: "survived", code: 0},
		{state: "equivalent", code: 0, test: "TestRejectsMinusOne"},
	} {
		if mutationEvidenceComplete([]result{row}) {
			t.Fatalf("unproven mutation admitted: %#v", row)
		}
	}
	if !mutationEvidenceComplete([]result{{state: "killed", code: 1, test: "TestRejectsMinusOne"}}) {
		t.Fatal("named failing test not accepted")
	}
}

func TestRefusalUnconditionalUsesRemovalWithoutInventedInput(t *testing.T) {
	root, _ := behavioralMutationFixture(t, `return errors.New("unsupported")`)
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, filepath.Join(root, "gate.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fn := file.Decls[1].(*ast.FuncDecl)
	expression := fn.Body.List[0].(*ast.ReturnStmt).Results[0]
	site := refusalSite{unconditional: true, id: "gate.go:entry:1", file: "gate.go", function: "entry", fset: set, expression: expression, tests: []testReference{{Dir: ".", Name: "TestRejectsMinusOne"}}}
	mutants, err := deriveMutants(root, []refusalSite{site})
	if err != nil {
		t.Fatal(err)
	}
	if len(mutants) != 1 || !strings.Contains(mutants[0].replaceText, "return nil") || !strings.HasSuffix(mutants[0].key, ":removal") {
		t.Fatalf("unconditional masquerades as narrowing: %#v", mutants)
	}
	injected := site
	injected.witnesses = []narrowingWitness{{Admit: "value == -1", Member: 0}}
	if _, err := deriveMutants(root, []refusalSite{injected}); err == nil {
		t.Fatal("unconditional invented witness admitted")
	}
	rows := runMutants(root, t.TempDir(), mutants, 0, 1, "", "")
	logMutationEvidence(t, 1, rows)
	if !mutationEvidenceComplete(rows) || rows[0].test != "TestRejectsMinusOne" {
		t.Fatalf("removal not killed: %#v", rows)
	}
}

func TestRefusalSurvivorCannotBorrowIndividuallyKilledDownstreamProof(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value < -2 {return errors.New("primary")};if value<0 || label=="bad" {return errors.New("downstream")}`)
	sites[0].witnesses = []narrowingWitness{{Member: 0, Admit: "value == -3"}}
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	rows := runMutants(root, t.TempDir(), mutants, 0, 1, "", "")
	logMutationEvidence(t, len(sites), rows)
	if rows[0].state != "survived" || rows[1].state != "killed" {
		t.Fatalf("unexpected individual evidence: %#v", rows)
	}
	joint := mutants[0]
	joint.extra = []sourceMutation{{mutants[1].start, mutants[1].end, mutants[1].replaceText}}
	proof := runMutant(root, t.TempDir(), joint, 0, "", "")
	logMutationEvidence(t, len(sites), []result{proof})
	if proof.state != "killed" || proof.test == "" {
		t.Fatalf("joint did not fail named test: %#v", proof)
	}
	if provedJointEquivalence(rows[0], rows[1], proof) {
		t.Fatal("primary survivor borrowed a downstream-only failure")
	}
	if mutationEvidenceComplete(rows) {
		t.Fatal("unproven survivor attested as complete")
	}
}

func TestRefusalDefensiveBoundRequiresExecutedPinAndJointKill(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value<0 {return errors.New("first")};if value<0 || label=="bad" {return errors.New("downstream")}`)
	mutants, err := deriveMutants(root, sites)
	if err != nil {
		t.Fatal(err)
	}
	primary := runMutant(root, t.TempDir(), mutants[0], 0, "", "")
	pin := testReference{Dir: ".", Name: "TestRejectsMinusTwo"}
	b := siteBound{Kind: "defensive-unreachable", Reason: "fixture proof of structural rule, not a production dependency claim", PinningTest: &pin}
	proof, ok := proveDefensiveBound(root, t.TempDir(), primary, mutants[1], b, 0, "", "")
	if !ok || proof.state != "bound: defensive-unreachable" || proof.test != "TestRejectsMinusOne" || proof.jointCode != 1 || !mutationEvidenceComplete([]result{proof}) {
		t.Fatalf("missing executed bound: %#v", proof)
	}
	pin.Name = "TestAbsentPin"
	if _, ok := proveDefensiveBound(root, t.TempDir(), primary, mutants[1], b, 0, "", ""); ok {
		t.Fatal("absent pin admitted as dependency evidence")
	}
	b.PinningTest = nil
	if _, ok := proveDefensiveBound(root, t.TempDir(), primary, mutants[1], b, 0, "", ""); ok {
		t.Fatal("no pin admitted")
	}
	// A primary-only run stays a survivor; a downstream name is not a joint proof.
	if mutationEvidenceComplete([]result{primary}) {
		t.Fatal("unexecuted joint accepted")
	}
}

func TestRefusalDeclaredBoundsRequireSourceIdentityAndExactReason(t *testing.T) {
	root, sites := behavioralMutationFixture(t, `if value<0 || label=="bad" {return errors.New("no")}`)
	source, err := os.ReadFile(filepath.Join(root, sites[0].file))
	if err != nil {
		t.Fatal(err)
	}
	mapping := siteMapping{Site: sites[0].id, SourceSHA256: fmt.Sprintf("%x", sha256.Sum256(source)), Bounds: []siteBound{{Kind: "undecided", Member: -1, Reason: "no diagnostic witness; explicitly unproved"}}}
	if err := validateSiteBounds(root, sites[0], mapping); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"equivalent", "skip", ""} {
		bad := mapping
		bad.Bounds = []siteBound{{Kind: kind, Member: -1, Reason: "no proof"}}
		if validateSiteBounds(root, sites[0], bad) == nil {
			t.Fatal("unknown bound admitted", kind)
		}
	}
	bad := mapping
	bad.SourceSHA256 = ""
	if validateSiteBounds(root, sites[0], bad) == nil {
		t.Fatal("unbound source admitted")
	}
	bad = mapping
	bad.Bounds = []siteBound{{Kind: "undecided", Member: -1}}
	if validateSiteBounds(root, sites[0], bad) == nil {
		t.Fatal("silent gap admitted")
	}
	if err := os.WriteFile(filepath.Join(root, sites[0].file), append(source, []byte("\n// changed identity\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if validateSiteBounds(root, sites[0], mapping) == nil {
		t.Fatal("stale bound admitted")
	}
}

func TestReviewCommandJointProofCannotCloseUnprovedSurvivor(t *testing.T) {
	root := t.TempDir()
	script, err := filepath.Abs("engine-catalog-mutants.go")
	if err != nil {
		t.Fatal(err)
	}
	write := func(n, s string) {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.25.5\n")
	write("gate.go", `package fixture
 type refusal struct{}
 func (*refusal) Error()string{return "refused"}
 func refuse()error{return &refusal{}}
 func entry(value int)error{if value<0{return refuse()};if value<0{return refuse()};return nil}
 `)
	write("gate_test.go", `package fixture
 import "testing"
 func TestRejects(t *testing.T){if entry(-1)==nil{t.Fatal("minus one admitted")}}
 `)
	tests, err := loadTests(root, []string{"gate.go"})
	if err != nil || len(tests) != 1 {
		t.Fatalf("%v %v", tests, err)
	}
	ref := tests[0].reference
	ref.BodySHA256 = tests[0].bodyDigest
	mappings := []siteMapping{{Site: "gate.go:refuse:1", Tests: []testReference{ref}},
		{Site: "gate.go:entry:1", Tests: []testReference{ref}, Witnesses: []narrowingWitness{{Member: 0, Admit: "value == -1"}}},
		{Site: "gate.go:entry:2", Tests: []testReference{ref}, Witnesses: []narrowingWitness{{Member: 0, Admit: "value == -1"}}}}
	data, _ := json.Marshal(mappings)
	write("scripts/engine-catalog-sites.json", string(data))
	cmd := exec.Command("go", "run", script, "-workers", "1", "-out", ".temp/mutants")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	t.Logf("command exit=%d %s", cmd.ProcessState.ExitCode(), output)
	if err == nil {
		t.Fatal("unproved downstream survivor admitted")
	}
	table, err := os.ReadFile(filepath.Join(root, ".temp/mutants/TASK-260930-1hzscs_mutants_000-003.md"))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(table))
	if !strings.Contains(string(table), "equivalent") || !strings.Contains(string(table), "survived") {
		t.Fatal("missing evidence states")
	}
	logs, _ := filepath.Glob(filepath.Join(root, ".temp/mutants/joint/generated/*/go-test.log"))
	if len(logs) != 1 {
		t.Fatalf("joint logs %v", logs)
	}
	data, err = os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("joint log:\n%s", data)
	if !strings.Contains(string(data), "--- FAIL: TestRejects") {
		t.Fatal("no named joint failure")
	}
}
