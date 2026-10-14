// Package tasks provides module-level functionality for tasks.
// input: the non-test Go files of package tasks
// output: assertions that the storage-health verdict and the replay cut are read from internal/assurance: tasks does not call Ledger.Damage or define excludeDamagedSegments
// pos: structural guard for ADR 0009 phase 1 (P1-3)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestHealthAndCutLiveInAssurance(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncDecl:
				if v.Recv == nil && v.Name.Name == "excludeDamagedSegments" {
					t.Errorf("%s defines excludeDamagedSegments; the replay cut is assurance.KeepBefore", name)
				}
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Damage" {
					t.Errorf("%s calls .Damage at %s; the verdict is assurance.Chain.Health", name, fset.Position(v.Pos()))
				}
			}
			return true
		})
	}
}
