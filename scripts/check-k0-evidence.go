//go:build ignore

// This task-specific harness derives refusal sites from Go AST and executes
// behavioral CLI tests against narrowing mutations, restoring candidate bytes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type site struct {
	Code      string `json:"code"`
	Line      int    `json:"line"`
	Condition string `json:"condition"`
}
type mutant struct {
	Code, Test, Bound string
	Exit              int
	Killed            bool
}

func main() {
	output := flag.String("out", ".temp/k0-evidence", "evidence output directory")
	flag.Parse()
	if err := execute(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(output string) error {
	path := "internal/infra/engine_check.go"
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.WriteFile(path, original, 0644); err != nil {
			panic(err)
		}
	}()
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, original, 0)
	if err != nil {
		return err
	}
	tests := map[string]struct{ name, exception, bound string }{
		"k0_parallel":     {"parallel_missing", `len(values["parallel"]) != 0`, "admits only absent parallel; invalid and duplicated parallel still refused"},
		"k0_layout":       {"layout_missing", `len(values["unified"]) + len(values["separate"]) != 0`, "admits only absent layout; contradictions still refused"},
		"k0_cache_ram":    {"cache_ram_missing", `len(values["cache_ram"]) != 0`, "admits only absent cache-ram; enabled cache still refused"},
		"k0_idle_slots":   {"idle_slots_missing", `len(values["no_idle"]) + len(values["idle"]) != 0`, "admits only absent idle policy; explicit inverse still refused"},
		"k0_template":     {"template_missing", `len(values["template"]) + len(values["template_file"]) + len(values["template_override"]) != 0`, "admits only absent template; empty, conflicting and overridden templates still refused"},
		"k0_cache_type_k": {"type_k_missing", `len(values["type_k"]) != 0`, "admits only absent K type; invalid type still refused"},
		"k0_cache_type_v": {"type_v_missing", `len(values["type_v"]) != 0`, "admits only absent V type; invalid type still refused"},
		"k0_sleep_idle":   {"sleep_zero", `singleEngineArg(values,"sleep") != "0"`, "admits only sleep-idle zero; nonzero and duplicate flags still refused"},
		"k0_router":       {"router_directory", `singleEngineArg(values,"router") != "router"`, "admits one router-directory fixture; missing model and other router selectors still refused"},
	}
	var sites []site
	var mutations []struct {
		code       string
		start, end int
	}
	// Walk each refusal's actual enclosing IfStmt, never a hand-written site list.
	ast.Inspect(file, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		for _, body := range statement.Body.List {
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || (fn.Name != "refuse" && fn.Name != "fail") || len(call.Args) == 0 {
					return true
				}
				arg := call.Args[0]
				code := "dynamic:" + string(original[fset.Position(arg.Pos()).Offset:fset.Position(arg.End()).Offset])
				if literal, ok := arg.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					code, err = strconv.Unquote(literal.Value)
					if err != nil {
						return true
					}
				}
				start, end := fset.Position(statement.Cond.Pos()).Offset, fset.Position(statement.Cond.End()).Offset
				sites = append(sites, site{code, fset.Position(call.Pos()).Line, string(original[start:end])})
				if _, exists := tests[code]; exists {
					mutations = append(mutations, struct {
						code       string
						start, end int
					}{code, start, end})
				}
				return true
			})
		}
		return true
	})
	// Include typed argv failures constructed directly, deriving their enclosing
	// grammar predicate from the AST rather than maintaining a refusal list.
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		kind, ok := literal.Type.(*ast.Ident)
		if !ok || kind.Name != "EngineCheckFailure" {
			return true
		}
		for _, element := range literal.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := pair.Key.(*ast.Ident)
			value, isString := pair.Value.(*ast.BasicLit)
			if !ok || key.Name != "Code" || !isString || value.Kind != token.STRING {
				continue
			}
			code, err := strconv.Unquote(value.Value)
			if err != nil {
				continue
			}
			var enclosing *ast.IfStmt
			ast.Inspect(file, func(n ast.Node) bool {
				if statement, ok := n.(*ast.IfStmt); ok && statement.Pos() <= literal.Pos() && statement.End() >= literal.End() {
					if enclosing == nil || statement.Pos() > enclosing.Pos() {
						enclosing = statement
					}
				}
				return true
			})
			condition := "unconditional"
			if enclosing != nil {
				condition = string(original[fset.Position(enclosing.Cond.Pos()).Offset:fset.Position(enclosing.Cond.End()).Offset])
			}
			sites = append(sites, site{code, fset.Position(literal.Pos()).Line, condition})
		}
		return true
	})
	if err := saveJSON(filepath.Join(output, "refusal-sites.json"), sites); err != nil {
		return err
	}
	if len(mutations) != len(tests) {
		return fmt.Errorf("AST yields %d K0 gates; expect %d", len(mutations), len(tests))
	}
	var results []mutant
	allKilled := true
	for index, m := range mutations {
		test := tests[m.code]
		condition := string(original[m.start:m.end])
		changed := append([]byte{}, original[:m.start]...)
		changed = append(changed, []byte("("+condition+") && ("+test.exception+")")...)
		changed = append(changed, original[m.end:]...)
		if err := os.WriteFile(path, changed, 0644); err != nil {
			return err
		}
		logPath := filepath.Join(output, fmt.Sprintf("mutant-%02d-%s.log", index+1, m.code))
		log, err := os.Create(logPath)
		if err != nil {
			return err
		}
		cmd := exec.Command("go", "test", "./cmd/curator-engines", "-run", "^TestEngineCheckRequiredKnobs$/^"+test.name+"$", "-count=1", "-v")
		cmd.Stdout = log
		cmd.Stderr = log
		err = cmd.Run()
		if closeErr := log.Close(); closeErr != nil {
			return closeErr
		}
		exit := 0
		if err != nil {
			if status, ok := err.(*exec.ExitError); ok {
				exit = status.ExitCode()
			} else {
				return err
			}
		}
		// A compilation failure or a non-test failure is a survivor, not evidence.
		data, err := os.ReadFile(logPath)
		if err != nil {
			return err
		}
		killed := exit == 1 && contains(data, []byte("--- FAIL: TestEngineCheckRequiredKnobs/"+test.name))
		results = append(results, mutant{m.code, "TestEngineCheckRequiredKnobs/" + test.name, test.bound, exit, killed})
		if !killed {
			allKilled = false
		}
		if err := os.WriteFile(path, original, 0644); err != nil {
			return err
		}
	}
	for index, m := range []struct{ code, before, after, test, bound string }{
		{"catalog-schema-fallback", "if config.SchemaVersion != nil || config.Engines != nil {", "if (config.SchemaVersion != nil || config.Engines != nil) && (config.SchemaVersion == nil || *config.SchemaVersion != 2) {", "TestEngineCheckCatalogV1/schema_invalid", "only schema_version 2 bypasses strict catalog validation and can use the legacy decoy"},
		{"catalog-ssh-exemption", `if entry.Mode == "ssh" {`, `if entry.Mode == "ssh" && entry.Engine != "llama-cpp" {`, "TestEngineCheckCatalogV1/ssh", "only SSH entries declaring llama-cpp are wrongly subjected to local K0; other SSH kinds stay exempt"},
		{"argv-option-value-confusion", "} else {\n\t\t\t\t// llama-server", "} else if argv[i+1] != \"--no-cache-idle-slots\" {\n\t\t\t\t// llama-server", "TestEngineCheckOptionValueIsNotIdlePolicy", "only the idle-policy token is reinterpreted as a flag instead of consumed as data"},
		{"argv-missing-passthrough-value", `failures = append(failures, EngineCheckFailure{Code: "argv_missing_value", Message: "argument requires a following value"})`, `if token != "--alias" { failures = append(failures, EngineCheckFailure{Code: "argv_missing_value", Message: "argument requires a following value"}) }`, "TestEngineCheckMissingPassthroughValues/--alias", "only trailing alias is admitted; the other 12 pass-through spellings still refuse"},
		{"kv-context-padding-overflow", "if ctx > 1<<40 {", "if ctx > 1<<40 && ctx != ^uint64(0) {", "TestEngineCheckKVContextPaddingOverflow/uint64_max", "only UINT64_MAX bypasses the context bound; all other oversized contexts still refuse computation"},
	} {
		if strings.Count(string(original), m.before) != 1 {
			return fmt.Errorf("mutation %s does not identify exactly one site", m.code)
		}
		changed := strings.Replace(string(original), m.before, m.after, 1)
		if err := os.WriteFile(path, []byte(changed), 0644); err != nil {
			return err
		}
		logPath := filepath.Join(output, fmt.Sprintf("mutant-%02d-%s.log", len(mutations)+index+1, m.code))
		log, err := os.Create(logPath)
		if err != nil {
			return err
		}
		parts := strings.Split(m.test, "/")
		for i := range parts {
			parts[i] = "^" + parts[i] + "$"
		}
		cmd := exec.Command("go", "test", "./cmd/curator-engines", "-run", strings.Join(parts, "/"), "-count=1", "-v")
		cmd.Stdout, cmd.Stderr = log, log
		err = cmd.Run()
		if closeErr := log.Close(); closeErr != nil {
			return closeErr
		}
		exit := 0
		if err != nil {
			if status, ok := err.(*exec.ExitError); ok {
				exit = status.ExitCode()
			} else {
				return err
			}
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			return err
		}
		killed := exit == 1 && contains(data, []byte("--- FAIL: "+m.test))
		results = append(results, mutant{m.code, m.test, m.bound, exit, killed})
		allKilled = allKilled && killed
		if err := os.WriteFile(path, original, 0644); err != nil {
			return err
		}
	}
	if err := saveJSON(filepath.Join(output, "mutants.json"), results); err != nil {
		return err
	}
	if !allKilled {
		return fmt.Errorf("surviving mutants: see mutants.json")
	}
	fmt.Printf("AST: %d refusal sites (including typed argv constructions and dynamic codes); %d of %d narrowing mutants killed by named behavioral tests\n", len(sites), len(results), len(tests)+5)
	return nil
}

func contains(data, needle []byte) bool {
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func saveJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
