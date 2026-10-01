package main

import (
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures resolve actual objects through go/types, including shadowed names.
func propagationFixture(t *testing.T, body string) (typedUnit, *ast.FuncDecl) {
	t.Helper()
	source := `package fixture
 import("errors";"fmt";"reflect")
 var _=errors.Join
 var _=fmt.Errorf
 var _=reflect.DeepEqual
 type refusal struct{}
 func (*refusal) Error() string {return "refused"}
 func refuse() error {return &refusal{}}
 ` + body
	u := typedUnit{set: token.NewFileSet(), info: &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}, constructors: map[types.Object]bool{}, errorTypes: map[*types.Named]bool{}}
	file, err := parser.ParseFile(u.set, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	config := types.Config{Importer: importer.Default()}
	pkg, err := config.Check("fixture", u.set, []*ast.File{file}, u.info)
	if err != nil {
		t.Fatal(err)
	}
	u.pkg = pkg
	u.indexBodies([]*ast.File{file})
	u.errorTypes[pkg.Scope().Lookup("refusal").Type().(*types.Named)] = true
	u.constructors[pkg.Scope().Lookup("refuse")] = true
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "entry" {
			return u, fn
		}
	}
	t.Fatal("missing entry")
	return u, nil
}
func fixturePropagations(u typedUnit, fn *ast.FuncDecl) []bool {
	var parents []ast.Node
	var accepted []bool
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if node == nil {
			parents = parents[:len(parents)-1]
			return false
		}
		if expr, ok := node.(ast.Expr); ok && u.refusalProducer(expr) && !u.refusalObservation(expr, parents) {
			nested := false
			if len(parents) > 0 {
				if parent, ok := parents[len(parents)-1].(ast.Expr); ok {
					nested = u.refusalProducer(parent)
				}
			}
			if !nested {
				accepted = append(accepted, u.propagates(fn, expr, parents))
			}
		}
		parents = append(parents, node)
		return true
	})
	return accepted
}
func TestRefusalInventoryPropagationP1P2P3AndNearMisses(t *testing.T) {
	cases := []struct {
		name, body string
		allowed    bool
	}{
		{"P1 typed value", `func entry(r *refusal) error{return r}`, true},

		{"P1 return", `func entry() error {return refuse()}`, true},
		{"P1 if init", `func entry() error {if err:=refuse();err!=nil{return err};return nil}`, true},
		{"P1 returned nonerror storage", `type box struct{Err error};func entry() box{return box{refuse()}}`, false},
		{"P1 returned literal", `func entry() error {return &refusal{}}`, true},
		{"P1 closure instead of returning", `func entry() error {if err:=refuse();err!=nil{_=func()error{return err}};return nil}`, false},
		{"P1 local then return", `func entry() error {err:=refuse();return err}`, false},
		{"P2 error channel", `func entry(ch chan error) {ch<-refuse()}`, true},
		{"P2 concrete channel", `func entry(ch chan *refusal) {ch<- &refusal{}}`, true},
		{"P2 any channel", `func entry(ch chan any) {ch<-refuse()}`, false},
		{"P3 nested own result", `func entry() {_=func()(other error){other=refuse();return}}`, true},
		{"P3 own result", `func entry() (err error) {err=refuse();return}`, true},
		{"P3 deferred result", `func entry() (err error) {defer func(){err=refuse()}();return}`, true},
		{"P3 other function result", `func entry() (err error) {_=func() (other error) {err=refuse();return};return}`, false},
		{"P3 field", `func entry() {s:=struct{Err error}{};s.Err=refuse()}`, false},
		{"argument wrapper", `func wrapper(err error) error {return err};func entry() error {return wrapper(refuse())}`, true},
		{"composite stored", `func entry() {s:=struct{Err error}{refuse()};_=s}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, fn := propagationFixture(t, tc.body)
			got := fixturePropagations(u, fn)
			if len(got) != 1 || got[0] != tc.allowed {
				t.Fatalf("propagations=%v want one allowed=%t", got, tc.allowed)
			}
		})
	}
}
func TestRefusalInventoryResolvesConstructorObjectNotShadowedName(t *testing.T) {
	u, fn := propagationFixture(t, `func entry() {refuse:=func() error{return nil};local:=refuse();_=local}`)
	if got := fixturePropagations(u, fn); len(got) != 0 {
		t.Fatalf("name overlap invented refusals: %v", got)
	}
}
func TestRefusalMappingUsesASTIdentityAndRejectsDeletedNegativeTest(t *testing.T) {
	ref := testReference{Dir: "infra", Name: "TestNegative", BodySHA256: "fixture-body"}
	maps := []siteMapping{{Site: "infra.go:entry:1", Tests: []testReference{ref}}}
	candidates := []parsedTest{{reference: ref, bodyDigest: "fixture-body"}}
	if _, err := mappedSiteTests("infra.go:entry:1", maps, candidates); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id string
		tests    []parsedTest
	}{
		{"different ordinal", "infra.go:entry:2", candidates},
		{"different file", "other.go:entry:1", candidates},
		{"different function", "infra.go:other:1", candidates},
		{"deleted test", "infra.go:entry:1", []parsedTest{{reference: testReference{Dir: "infra", Name: "TestNegativeNameOverlap"}}}},
		{"deleted negative body", "infra.go:entry:1", []parsedTest{{reference: ref, bodyDigest: "changed-body"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mappedSiteTests(tc.id, maps, tc.tests); err == nil {
				t.Fatal("identity or missing test admitted")
			}
		})
	}
}
func TestRefusalInventoryWrapperArgumentReportsFileLine(t *testing.T) {
	u, fn := propagationFixture(t, `func wrapper(err error) error {return fmt.Errorf("%w",err)};func entry() error {return wrapper(refuse())}`)
	got := fixturePropagations(u, fn)
	if len(got) != 1 || got[0] {
		t.Fatal("ordinary error wrapper argument was accepted")
	}
	if !strings.Contains(propagationSubset, "P1") {
		t.Fatal("guard omitted accepted subset")
	}
}

func TestRefusalCommandInventoryMapsIdentityAndRejectsDeletedNegativeTest(t *testing.T) {
	root := t.TempDir()
	script, err := filepath.Abs("engine-catalog-mutants.go")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module commandfixture\n\ngo 1.25.5\n")
	write("gate.go", `package fixture
 import("errors";"fmt")
 var _=errors.Join
 var _=fmt.Errorf
 type refusal struct{value int}
 func (*refusal) Error()string{return "refused"}
 func refuse(value int)error{return &refusal{value}}
 func entry(value int)error{if value<0{return refuse(value)};return nil}
 `)
	testSource := `package fixture
 import "testing"
 func TestRejects(t *testing.T){if entry(-1)==nil{t.Fatal("admitted")}}
 `
	write("gate_test.go", testSource)
	testAST, err := parser.ParseFile(token.NewFileSet(), "gate_test.go", testSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest := testBodyDigest(testAST.Decls[1].(*ast.FuncDecl).Body)
	maps := []siteMapping{
		{Site: "gate.go:refuse:1", Tests: []testReference{{Dir: ".", Name: "TestRejects", BodySHA256: digest}}},
		{Site: "gate.go:entry:1", Tests: []testReference{{Dir: ".", Name: "TestRejects", BodySHA256: digest}}, Witnesses: []narrowingWitness{{Admit: "value == -1"}}},
	}
	data, err := json.Marshal(maps)
	if err != nil {
		t.Fatal(err)
	}
	write("scripts/engine-catalog-sites.json", string(data))
	run := func() ([]byte, error) {
		command := exec.Command("go", "run", script, "-inventory")
		command.Dir = root
		data, err := command.CombinedOutput()
		t.Logf("go run -inventory exit=%d\n%s", command.ProcessState.ExitCode(), data)
		return data, err
	}
	data, err = run()
	if err != nil {
		t.Fatal(err)
	}
	var report inventoryReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Sites) != 2 || len(report.Mutants) != 2 || report.AcceptedSubset != propagationSubset {
		t.Fatalf("unexpected inventory: %#v", report)
	}
	command := exec.Command("go", "run", script, "-out", ".temp/mutants", "-workers", "1")
	command.Dir = root
	evidence, runErr := command.CombinedOutput()
	t.Logf("go run -out .temp/mutants -workers 1 exit=%d\n%s", command.ProcessState.ExitCode(), evidence)
	if runErr != nil {
		t.Fatalf("command mutation execution failed: %s err=%v", evidence, runErr)
	}
	table, readErr := os.ReadFile(filepath.Join(root, ".temp/mutants/TASK-260930-1hzscs_mutants_000-002.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Log(string(table))
	if strings.Count(string(table), "fails: TestRejects") != 2 || !strings.Contains(string(evidence), "unresolved=0") {
		t.Fatalf("command did not execute both named killing tests: %s", table)
	}

	write("gate_test.go", strings.ReplaceAll(testSource, "TestRejects(", "TestRejectsLookalike("))
	data, err = run()
	if err == nil || !strings.Contains(string(data), "mapped negative test missing") {
		t.Fatalf("deleted negative test admitted: %s err=%v", data, err)
	}
	write("gate_test.go", `package fixture
 import "testing"
 func TestRejects(t *testing.T){}
 `)
	data, err = run()
	if err == nil || !strings.Contains(string(data), "body changed") {
		t.Fatalf("deleted negative body admitted: %s err=%v", data, err)
	}
	write("gate_test.go", testSource)

	write("gate.go", `package fixture
 type refusal struct{value int}
 func (*refusal) Error()string{return "refused"}
 func refuse(value int)error{return &refusal{value}}
 func wrapper(cause error)error{return cause}
 func entry(value int)error{if value<0{return wrapper(refuse(value))};return nil}
 `)
	data, err = run()
	if err != nil {
		t.Fatalf("P4 literal wrapper did not pass actual command: %s %v", data, err)
	}
	write("gate.go", `package fixture
 import("errors";"fmt")
 var _=errors.Join
 var _=fmt.Errorf
 type refusal struct{value int}
 func (*refusal) Error()string{return "refused"}
 func refuse(value int)error{return &refusal{value}}
 func wrapper(cause error)error{return fmt.Errorf("%w",cause)}
 func entry(value int)error{return wrapper(refuse(value))}
 `)
	data, err = run()
	if err == nil || !strings.Contains(string(data), "gate.go:9: typed refusal does not propagate under P1-P4") {
		t.Fatalf("ordinary wrapper argument not refused with file:line: %s err=%v", data, err)
	}
	write("gate.go", `package fixture
 import("errors";"fmt")
 var _=errors.Join
 var _=fmt.Errorf
 type refusal struct{value int}
 func (*refusal) Error()string{return "refused"}
 func refuse(value int)error{return &refusal{value}}
 func entry(value int)error{return refuse(value)}
 var stored=refuse(-1)
 `)
	data, err = run()
	if err == nil || !strings.Contains(string(data), "gate.go:9: typed refusal declaration does not propagate under P1-P4") {
		t.Fatalf("global storage not refused with file:line: %s err=%v", data, err)
	}
}

func TestRefusalInventoryP4WrapperAndNearMisses(t *testing.T) {
	cases := []struct {
		name, body string
		allowed    bool
	}{
		{"cause", `func wrapper(cause error)error{return cause};func entry()error{return wrapper(refuse())}`, true},
		{"join", `func wrapper(cause error)error{return errors.Join(cause,fmt.Errorf("cleanup"))};func entry()error{return wrapper(refuse())}`, true},
		{"fmt", `func wrapper(cause error)error{return fmt.Errorf("%w",cause)};func entry()error{return wrapper(refuse())}`, false},
		{"different return", `func wrapper(cause error)error{if cause==nil{return errors.New("other")};return cause};func entry()error{return wrapper(refuse())}`, false},
		{"dropped", `func wrapper(cause error)error{return cause};func entry(){wrapper(refuse())}`, false},
		{"stored", `func wrapper(cause error)error{return cause};func entry(){local:=wrapper(refuse());_=local}`, false},
		{"interface", `type I interface{Wrap(error)error};func entry(i I)error{return i.Wrap(refuse())}`, false},
		{"function value", `func wrapper(cause error)error{return cause};func entry()error{w:=wrapper;return w(refuse())}`, false},
		{"nested consumer", `func wrapper(cause error)error{return cause};func entry()error{return wrapper((wrapper(refuse())))}`, false},
		{"transitive", `func wrapper(cause error)error{return cause};func second(cause error)error{return wrapper(cause)};func entry()error{return second(refuse())}`, false},
		{"send", `func wrapper(cause error)error{return cause};func entry(ch chan error){ch<-wrapper(refuse())}`, true},
		{"named result", `func wrapper(cause error)error{return cause};func entry()(err error){err=wrapper(refuse());return}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, fn := propagationFixture(t, tc.body)
			got := fixturePropagations(u, fn)
			if len(got) != 1 || got[0] != tc.allowed {
				t.Fatalf("propagations=%v want %t", got, tc.allowed)
			}
		})
	}
}

func TestRefusalInventoryTypedValueCannotBeStoredThenReturned(t *testing.T) {
	u, fn := propagationFixture(t, `func entry(r *refusal)error{local:=r;return local}`)
	got := fixturePropagations(u, fn)
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("typed-value storage escaped inventory: %v", got)
	}
}

func TestRefusalWitnessMustBindActualRejectedMember(t *testing.T) {
	u, fn := propagationFixture(t, `const fixed="unsupported";func entry(value,other int)error{if value<0{return refuse()};return nil}`)
	branch := fn.Body.List[0].(*ast.IfStmt)
	site := refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: branch.Cond}
	if err := typedWitnessMember(site, narrowingWitness{Admit: "value == -1"}); err != nil {
		t.Fatal(err)
	}
	for _, admit := range []string{`fixed == "unsupported"`, `other == -1`, `unknown == -1`} {
		if err := typedWitnessMember(site, narrowingWitness{Admit: admit}); err == nil {
			t.Fatalf("fictitious member accepted: %s", admit)
		}
	}
	u, fn = propagationFixture(t, `func entry(value struct{Number int})error{if value.Number<0{return refuse()};return nil}`)
	branch = fn.Body.List[0].(*ast.IfStmt)
	site = refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: branch.Cond}
	if err := typedWitnessMember(site, narrowingWitness{Admit: "value.Number == -1"}); err != nil {
		t.Fatal(err)
	}
	u, fn = propagationFixture(t, `func entry()error{return &refusal{}}`)
	site = refusalSite{id: "fixture.go:entry:1", typeContext: &u, expression: fn.Body.List[0].(*ast.ReturnStmt).Results[0]}
	if err := typedWitnessMember(site, narrowingWitness{Admit: `true == true`}); err == nil {
		t.Fatal("inputless singleton invented a narrowing domain")
	}
}

func TestRefusalWitnessFullSelectorPathIdentities(t *testing.T) {
	for _, source := range []string{
		`func entry(value struct{Engine,Mode string})error{if value.Engine!="llama-cpp"{return refuse()};return nil}`,
		`type leaf struct{Engine,Mode string};func entry(value struct{Left,Right leaf})error{if value.Left.Engine!="llama-cpp"{return refuse()};return nil}`,
	} {
		u, fn := propagationFixture(t, source)
		branch := fn.Body.List[0].(*ast.IfStmt)
		site := refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: branch.Cond}
		path := "value.Engine"
		bad := []string{`value.Mode == "local"`}
		if strings.Contains(source, "value.Left.Engine") {
			path = "value.Left.Engine"
			bad = []string{`value.Right.Engine == "future"`, `value.Left.Mode == "local"`}
		}
		if err := typedWitnessMember(site, narrowingWitness{Admit: path + ` == "future"`}); err != nil {
			t.Fatal(err)
		}
		for _, admit := range bad {
			if err := typedWitnessMember(site, narrowingWitness{Admit: admit}); err == nil {
				t.Fatalf("sibling-field witness accepted: %s", admit)
			}
		}
	}
}

func TestRefusalWitnessCannotBindSiblingField(t *testing.T) {
	root := mutationCommandFixture(t, `type record struct{Engine,Mode string}
 func entry(value record)error{if value.Mode=="local"{if value.Engine!="llama-cpp"{return refuse()}};return nil}`, `func TestRejects(t *testing.T){for _,engine:=range []string{"future","another-future"}{if entry(record{Engine:engine,Mode:"local"})==nil{t.Fatal("engine admitted")}}}`)
	writeMutationMappings(t, root, "value.Engine == \"future\"")
	data, code := mutationCommand(t, root, "-inventory")
	if code != 0 {
		t.Fatalf("valid witness refused: %s", data)
	}
	writeMutationMappings(t, root, "value.Mode == \"local\"")
	data, code = mutationCommand(t, root, "-inventory")
	if code == 0 || !strings.Contains(string(data), "sibling field") {
		t.Fatalf("public inventory admitted sibling witness: exit %d %s", code, data)
	}
}

func TestRefusalSingletonEqualityRequiresTypedConstantAndExactSyntax(t *testing.T) {
	u, fn := propagationFixture(t, `type count int;func entry(value count, text string)error{if value<0{return refuse()};return nil}`)
	site := refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: fn.Body.List[0].(*ast.IfStmt).Cond}
	if err := typedWitnessMember(site, narrowingWitness{Admit: "value == -1"}); err != nil {
		t.Fatal(err)
	}
	for _, admit := range []string{`value == 1.5`, `value == "bad"`, `value == count(-1)`, `value == -1 && text == "bad"`, `text == "bad"`} {
		if err := typedWitnessMember(site, narrowingWitness{Admit: admit}); err == nil {
			t.Fatalf("not a typed singleton: %s", admit)
		}
	}
	// Observations never borrow their operand's identity, even if the original
	// guarding domain was length. These need explicit bounds under brief 08.
	u, fn = propagationFixture(t, `func entry(value string)error{if len(value)>5{return refuse()};return nil}`)
	site = refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: fn.Body.List[0].(*ast.IfStmt).Cond}
	for _, admit := range []string{`len(value) == 6`, `value == "future"`} {
		if err := typedWitnessMember(site, narrowingWitness{Admit: admit}); err == nil {
			t.Fatalf("length domain borrowed: %s", admit)
		}
	}
}

func TestRefusalSingletonDeepEqualityRequiresFullySpecifiedLiteral(t *testing.T) {
	for _, tc := range []struct {
		source, allowed string
		refused         []string
	}{
		{`func entry(value []string)error{if value==nil{return refuse()};return nil}`, `reflect.DeepEqual(value, []string{})`, []string{`value == nil`, `reflect.DeepEqual(value, []string{other})`, `reflect.DeepEqual(value, []string{1:"bad"})`, `reflect.DeepEqual(value, []int{})`}},
		{`type record struct{Values []string; Label string};func entry(value record)error{if value.Label=="" || value.Values==nil{return refuse()};return nil}`, `reflect.DeepEqual(value.Values, []string{})`, []string{`reflect.DeepEqual(value, record{Label:"bad"})`}},
	} {
		u, fn := propagationFixture(t, tc.source)
		site := refusalSite{id: "fixture.go:entry:1", typeContext: &u, guardExpr: fn.Body.List[0].(*ast.IfStmt).Cond}
		if strings.Contains(tc.source, "type record") {
			site.guardExpr = rejectedGroups(site.guardExpr)[1]
		}
		if err := typedWitnessMember(site, narrowingWitness{Admit: tc.allowed}); err != nil {
			t.Fatal(err)
		}
		for _, admit := range tc.refused {
			if err := typedWitnessMember(site, narrowingWitness{Admit: admit}); err == nil {
				t.Fatalf("partial/mismatched deep singleton: %s", admit)
			}
		}
	}
	// A complete struct literal pins all fields; omission is refused even when
	// omitted fields would default to zero. The guard's direct value is boolean.
	u, _ := propagationFixture(t, `type record struct{Values []string; Label string};func entry(value record)error{return nil}`)
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`record{Values: []string{}, Label: "bad"}`, true},
		{`record{Label: "bad"}`, false},
		{`record{Values: []string{1:"bad"}, Label: "bad"}`, false},
	} {
		expr, err := parser.ParseExpr(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
		if err := types.CheckExpr(u.set, u.pkg, 0, expr, info); err != nil {
			t.Fatal(err)
		}
		if got := fullySpecifiedLiteral(expr, info); got != tc.want {
			t.Fatalf("%s: full=%t want %t", tc.expr, got, tc.want)
		}
	}
}

func TestRefusalConditionalDispatchCannotBorrowUnconditionalRemoval(t *testing.T) {
	for _, source := range []string{
		`func entry(value string)error{switch value {case "good":return nil;default:return refuse()}}`,
		`func entry(value int)error{if value>0 {return nil};return refuse()}`,
	} {
		_, fn := propagationFixture(t, source)
		var parents []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				parents = parents[:len(parents)-1]
				return false
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuse" && soleUnconditionalReturn(fn, parents) {
					t.Fatal("conditional dispatch admitted as unconditional")
				}
			}
			parents = append(parents, n)
			return true
		})
	}
}

func TestRefusalInventoryIncludesStandaloneIgnoreCommands(t *testing.T) {
	root := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.25.5\n")
	write("root.go", "package fixture\n")
	command := "//go:build ignore\n\npackage main\ntype refusal struct{}\nfunc (refusal) Error() string{return \"refused\"}\nfunc entry(value int) error{if value<0{return refusal{}};return nil}\nfunc main(){}\n"
	write("tool.go", command)
	sites, _, err := typedPropagationSites(root, []string{"root.go", "tool.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].id != "tool.go:entry:1" {
		t.Fatalf("standalone refusal omitted: %#v", sites)
	}
	write("tool.go", strings.Replace(command, "return refusal{}", "local:=refusal{};return local", 1))
	if _, _, err := typedPropagationSites(root, []string{"root.go", "tool.go"}); err == nil || !strings.Contains(err.Error(), "tool.go:") {
		t.Fatalf("standalone storage admitted: %v", err)
	}
	write("tool.go", strings.Replace(command, "//go:build ignore", "//go:build unconfigured", 1))
	if _, _, err := typedPropagationSites(root, []string{"root.go", "tool.go"}); err == nil || !strings.Contains(err.Error(), "no type-resolved platform context") {
		t.Fatalf("unsupported context skipped: %v", err)
	}
}
