//go:build darwin

package infra

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Discover the attestation/acquisition call graph and its wire stop sites from
// every production file. There is no list of permitted cleanup call sites.
// The accepted subset is static named calls (including package function aliases)
// and sharedWireMessage literals; dynamic dispatch is covered by the API class
// property, not claimed as a general interprocedural proof by this AST guard.
func TestAttestationCleanupOwnershipASTGuard(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	sites, err := guardAttestationCleanupOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	if sites == 0 {
		t.Fatal("attestation graph contains no guarded cleanup stop site")
	}
	t.Logf("%d of %d discovered attestation cleanup stop sites require typed launch ownership; force-stop sites=0; static calls and package aliases, wire literals only", sites, sites)
}

func guardAttestationCleanupOwnership(root string) (int, error) {
	files, err := guardProductionFiles(root)
	if err != nil {
		return 0, err
	}
	fset := token.NewFileSet()
	functions := map[string][]*ast.FuncDecl{}
	aliases := map[string]string{}
	var roots []string
	for _, file := range files {
		tree, err := parser.ParseFile(fset, filepath.Join(root, file), nil, 0)
		if err != nil {
			return 0, err
		}
		for _, decl := range tree.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok {
				functions[function.Name.Name] = append(functions[function.Name.Name], function)
				name := strings.ToLower(function.Name.Name)
				if strings.Contains(name, "attest") || strings.Contains(name, "acquiresharedruntime") {
					roots = append(roots, function.Name.Name)
				}
			}
			if general, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range general.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok && len(value.Names) == 1 && len(value.Values) == 1 {
						if name, ok := value.Values[0].(*ast.Ident); ok {
							aliases[value.Names[0].Name] = name.Name
						}
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	sites := 0
	for len(roots) > 0 {
		name := roots[0]
		roots = roots[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		if target := aliases[name]; target != "" {
			roots = append(roots, target)
		}
		for _, function := range functions[name] {
			var failure error
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					switch called := call.Fun.(type) {
					case *ast.Ident:
						roots = append(roots, called.Name)
					case *ast.SelectorExpr:
						roots = append(roots, called.Sel.Name)
					}
				}
				literal, ok := node.(*ast.CompositeLit)
				if !ok || ownershipASTText(literal.Type) != "sharedWireMessage" {
					return true
				}
				fields := map[string]ast.Expr{}
				for _, element := range literal.Elts {
					if field, ok := element.(*ast.KeyValueExpr); ok {
						fields[ownershipASTText(field.Key)] = field.Value
					}
				}
				if ownershipASTText(fields["Type"]) != `"stop"` {
					return true
				}
				sites++
				position := fset.Position(literal.Pos())
				if function.Recv == nil || len(function.Recv.List) != 1 || ownershipASTText(function.Recv.List[0].Type) != "sharedRuntimeLaunchOwnership" || len(function.Recv.List[0].Names) != 1 {
					failure = fmt.Errorf("%s: cleanup stop has no typed launch-ownership receiver", position)
					return false
				}
				receiver := function.Recv.List[0].Names[0].Name
				if len(function.Body.List) == 0 {
					return false
				}
				guard, ok := function.Body.List[0].(*ast.IfStmt)
				if !ok || guard.Init != nil || guard.Else != nil || ownershipASTText(guard.Cond) != "!"+receiver+".matches(broker)" || len(guard.Body.List) != 1 {
					failure = fmt.Errorf("%s: cleanup stop lacks a dominating launch-identity rejection", position)
					return false
				}
				if _, ok := guard.Body.List[0].(*ast.ReturnStmt); !ok {
					failure = fmt.Errorf("%s: failed ownership does not return before cleanup", position)
					return false
				}
				if force := fields["Force"]; force != nil && ownershipASTText(force) != "false" {
					failure = fmt.Errorf("%s: attestation cleanup can force-stop a concurrently leased runtime", position)
					return false
				}
				return true
			})
			if failure != nil {
				return sites, failure
			}
		}
	}
	return sites, nil
}

func ownershipASTText(node ast.Node) string {
	if node == nil {
		return ""
	}
	var output bytes.Buffer
	_ = format.Node(&output, token.NewFileSet(), node)
	return output.String()
}

func TestAttestationCleanupOwnershipASTGuardDiscoversNewCallSites(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		t.Run(strconv.FormatBool(tagged), func(t *testing.T) {
			root := t.TempDir()
			body := `package infra
func connectAndAttestSharedRuntime() { addedCleanup() }
func addedCleanup() { send(sharedWireMessage{Type: "stop", Force: true}) }
`
			if tagged {
				body = "//go:build windows\n\n" + body
			}
			if err := os.WriteFile(filepath.Join(root, "added.go"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			sites, err := guardAttestationCleanupOwnership(root)
			if err == nil || sites != 1 || !strings.Contains(err.Error(), "added.go") {
				t.Fatalf("new stop site not refused: sites=%d error=%v", sites, err)
			}
		})
	}
}
