// Package binlog provides module-level functionality for binlog.
// input: the Go sources of the segment walk (this package except the seam, the assurance chain, and the evaluator's disk call sites)
// output: a failure naming any os.Stat, os.Open, os.ReadDir, os.Lstat, os.ReadFile or filepath.Walk outside walk_ops.go
// pos: the structural guard so a nested walk cannot dodge WalkOps by calling os directly
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var walkGuardOS = map[string]bool{
	"Stat": true, "Open": true, "ReadDir": true, "Lstat": true, "ReadFile": true,
}

var walkGuardFilepath = map[string]bool{"Walk": true, "WalkDir": true}

// LoadOrCreateNodeID reads .node_id. That is not the segment walk.
var walkGuardSkipFunc = map[string]bool{"LoadOrCreateNodeID": true}

func TestWalkSeam_NoDirectOSOutsideSeam(t *testing.T) {
	root := moduleRoot(t)
	paths, err := walkGuardPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var hits []string
	for _, path := range paths {
		msg, err := walkGuardFile(fset, path)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, msg...)
	}
	if len(hits) != 0 {
		t.Fatalf("filesystem call outside the walk seam:\n%s", strings.Join(hits, "\n"))
	}
}

func TestWalkSeam_GuardFlagsDirectOS(t *testing.T) {
	flagged := "package p\nimport \"os\"\nfunc f() { _, _ = os.ReadDir(\"x\") }\n"
	if hits := walkGuardSource(t, "flagged.go", flagged); len(hits) != 1 || !strings.Contains(hits[0], "os.ReadDir") {
		t.Fatalf("os.ReadDir: %v", hits)
	}
	allowed := "package p\nimport \"os\"\nfunc f() { _, _ = os.OpenFile(\"x\", os.O_RDWR, 0) }\n"
	if hits := walkGuardSource(t, "allowed.go", allowed); len(hits) != 0 {
		t.Fatalf("OpenFile is a write: %v", hits)
	}
	skipped := "package p\nimport \"os\"\nfunc LoadOrCreateNodeID() { _, _ = os.ReadFile(\"x\") }\nfunc f() {}\n"
	if hits := walkGuardSource(t, "evaluator.go", skipped); len(hits) != 0 {
		t.Fatalf("node id read is not the walk: %v", hits)
	}
}

func walkGuardSource(t *testing.T, name, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return walkGuardAST(fset, name, f)
}

func walkGuardFile(fset *token.FileSet, path string) ([]string, error) {
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	return walkGuardAST(fset, path, f), nil
}

func walkGuardAST(fset *token.FileSet, path string, f *ast.File) []string {
	osNames, fpNames, osDot, fpDot := walkGuardImports(f)
	skip := filepath.Base(path) == "evaluator.go"
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && skip && walkGuardSkipFunc[fn.Name.Name] {
			return false
		}
		if call, ok := walkGuardCall(n, osNames, fpNames, osDot, fpDot); ok {
			pos := fset.Position(n.Pos())
			hits = append(hits, pos.String()+": "+call)
		}
		return true
	})
	return hits
}

func walkGuardImports(f *ast.File) (osNames, fpNames map[string]bool, osDot, fpDot bool) {
	osNames = map[string]bool{}
	fpNames = map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || imp.Name != nil && imp.Name.Name == "_" {
			continue
		}
		local, dot := "", false
		if imp.Name != nil {
			if imp.Name.Name == "." {
				dot = true
			} else {
				local = imp.Name.Name
			}
		}
		switch path {
		case "os":
			if dot {
				osDot = true
			} else {
				if local == "" {
					local = "os"
				}
				osNames[local] = true
			}
		case "path/filepath":
			if dot {
				fpDot = true
			} else {
				if local == "" {
					local = "filepath"
				}
				fpNames[local] = true
			}
		}
	}
	return osNames, fpNames, osDot, fpDot
}

func walkGuardCall(n ast.Node, osNames, fpNames map[string]bool, osDot, fpDot bool) (string, bool) {
	if sel, ok := n.(*ast.SelectorExpr); ok {
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if osNames[id.Name] && walkGuardOS[sel.Sel.Name] {
			return id.Name + "." + sel.Sel.Name, true
		}
		if fpNames[id.Name] && walkGuardFilepath[sel.Sel.Name] {
			return id.Name + "." + sel.Sel.Name, true
		}
		return "", false
	}
	call, ok := n.(*ast.CallExpr)
	if !ok || (!osDot && !fpDot) {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	if osDot && walkGuardOS[id.Name] {
		return "os." + id.Name, true
	}
	if fpDot && walkGuardFilepath[id.Name] {
		return "filepath." + id.Name, true
	}
	return "", false
}

func walkGuardPaths(root string) ([]string, error) {
	binlogDir := filepath.Join(root, "internal", "binlog")
	entries, err := os.ReadDir(binlogDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "walk_ops.go" {
			continue
		}
		out = append(out, filepath.Join(binlogDir, name))
	}
	for _, rel := range []string{
		"internal/assurance/chain.go",
		"internal/tasks/evaluator.go",
		"internal/tasks/disk_files.go",
		"internal/tasks/signals.go",
		"internal/tasks/chain_order.go",
		"internal/tasks/storage_health.go",
	} {
		out = append(out, filepath.Join(root, rel))
	}
	return out, nil
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
