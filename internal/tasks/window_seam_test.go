// Package tasks provides module-level functionality for tasks.
// input: the non-test Go files of package tasks
// output: an assertion that the recoverable-window derivation is no longer defined here
// pos: structural guard for ADR 0009 phase 1 (P1-2): the window is derived once, in internal/assurance
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

func TestWindowDerivationLivesInAssurance(t *testing.T) {
	moved := map[string]bool{
		"assessRecovery": true, "assessRecoveryFrom": true, "groupChain": true, "segmentBreaks": true,
		"missingIndexBreak": true, "mergeGTID": true, "gtidHole": true, "chainStoredSet": true,
		"switchHole": true, "intraSegmentHole": true, "eventSeqHole": true, "interiorHoleBreak": true,
	}
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
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && moved[fn.Name.Name] {
				t.Errorf("%s still defines %s; the window is derived in internal/assurance", name, fn.Name.Name)
			}
		}
	}
}
