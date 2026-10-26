// Package binlog provides module-level functionality for binlog.
// input: evaluator.go's imports, plus every non-test Go file in those packages and in internal/tasks
// output: a failure naming any banned filesystem call outside the seam allowlist
// pos: the structural guard so a nested walk cannot dodge WalkOps by calling os, filepath, io/fs, syscall or unix directly
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// walkGuardAllow is the only production files that may call the banned set.
// Each value is the one-line reason. The seam counts every call, including
// writes, so a directory read cannot hide in a file that also writes.
var walkGuardAllow = map[string]string{
	"internal/binlog/walk_ops.go": "counted seam for Stat, Lstat, Open, OpenFile, ReadDir, ReadFile, Glob and StampOf",
}

var walkGuardOS = map[string]bool{
	"Stat": true, "Lstat": true, "Open": true, "OpenFile": true,
	"ReadDir": true, "ReadFile": true, "DirFS": true,
}

var walkGuardFilepath = map[string]bool{"Walk": true, "WalkDir": true, "Glob": true}

var walkGuardFS = map[string]bool{
	"Stat": true, "ReadDir": true, "ReadFile": true, "WalkDir": true, "Glob": true, "DirFS": true,
}

var walkGuardSyscall = map[string]bool{
	"Stat": true, "Lstat": true, "Fstat": true, "Fstatat": true,
}

var walkGuardMethod = map[string]bool{"Readdirnames": true, "Readdir": true}

func TestWalkSeam_NoDirectOSOutsideSeam(t *testing.T) {
	root := moduleRoot(t)
	for path, reason := range walkGuardAllow {
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			t.Fatalf("%s: allowlist reason must be one non-empty line", path)
		}
	}
	pkgs, err := walkGuardPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"internal/tasks", "internal/binlog", "internal/assurance", "internal/health", "internal/healthstore"} {
		if !pkgs[want] {
			t.Fatalf("packages reached from the walk entry: %v, missing %s", pkgNames(pkgs), want)
		}
	}
	// replication, app and upload are not imported by the walk entry.
	for _, out := range []string{"internal/replication", "internal/app", "internal/upload"} {
		if pkgs[out] {
			t.Fatalf("walk derivation included %s, which evaluator.go does not import", out)
		}
	}
	t.Logf("walk packages: %s", strings.Join(pkgNames(pkgs), ", "))
	paths, err := walkGuardPaths(root, pkgs)
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
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"readdir", "package p\nimport \"os\"\nfunc f() { _, _ = os.ReadDir(\"x\") }\n", "os.ReadDir"},
		{"openfile", "package p\nimport \"os\"\nfunc f() { f, _ := os.OpenFile(\"x\", os.O_RDONLY, 0); _, _ = f.Readdirnames(0) }\n", "os.OpenFile"},
		{"readdirnames", "package p\nimport \"os\"\nfunc f() { f, _ := os.OpenFile(\"x\", os.O_RDONLY, 0); _, _ = f.Readdirnames(0) }\n", "Readdirnames"},
		{"glob", "package p\nimport \"path/filepath\"\nfunc f() { _, _ = filepath.Glob(\"x\") }\n", "filepath.Glob"},
		{"syscall", "package p\nimport \"syscall\"\nfunc f() { _ = syscall.Stat(\"x\", nil) }\n", "syscall.Stat"},
		{"fs", "package p\nimport \"io/fs\"\nfunc f() { _, _ = fs.ReadDir(nil, \"x\") }\n", "fs.ReadDir"},
		{"unix", "package p\nimport \"golang.org/x/sys/unix\"\nfunc f() { _ = unix.Fstatat(0, \"x\", nil, 0) }\n", "unix.Fstatat"},
	}
	for _, tc := range cases {
		hits := walkGuardSource(t, "flagged.go", tc.src)
		if !containsCall(hits, tc.want) {
			t.Errorf("%s: hits %v, want %s", tc.name, hits, tc.want)
		}
	}
	typeOnly := "package p\nimport \"syscall\"\nfunc f(st *syscall.Stat_t) { _ = st }\n"
	if hits := walkGuardSource(t, "stamp_linux.go", typeOnly); len(hits) != 0 {
		t.Fatalf("Stat_t is a type: %v", hits)
	}
}

func containsCall(hits []string, want string) bool {
	for _, hit := range hits {
		if strings.Contains(hit, want) {
			return true
		}
	}
	return false
}

func walkGuardSource(t *testing.T, name, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return walkGuardAST(fset, f)
}

func walkGuardFile(fset *token.FileSet, path string) ([]string, error) {
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	return walkGuardAST(fset, f), nil
}

type walkGuardPkgs struct {
	os, filepath, fs, syscall   map[string]bool
	osDot, fpDot, fsDot, sysDot bool
}

func walkGuardImports(f *ast.File) walkGuardPkgs {
	g := walkGuardPkgs{
		os: map[string]bool{}, filepath: map[string]bool{},
		fs: map[string]bool{}, syscall: map[string]bool{},
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || imp.Name != nil && imp.Name.Name == "_" {
			continue
		}
		var dst map[string]bool
		var dot *bool
		def := ""
		switch path {
		case "os":
			dst, dot, def = g.os, &g.osDot, "os"
		case "path/filepath":
			dst, dot, def = g.filepath, &g.fpDot, "filepath"
		case "io/fs":
			dst, dot, def = g.fs, &g.fsDot, "fs"
		case "syscall":
			dst, dot, def = g.syscall, &g.sysDot, "syscall"
		case "golang.org/x/sys/unix":
			dst, dot, def = g.syscall, &g.sysDot, "unix"
		default:
			continue
		}
		if imp.Name != nil && imp.Name.Name == "." {
			*dot = true
			continue
		}
		local := def
		if imp.Name != nil {
			local = imp.Name.Name
		}
		dst[local] = true
	}
	return g
}

func walkGuardAST(fset *token.FileSet, f *ast.File) []string {
	g := walkGuardImports(f)
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := walkGuardCall(n, g); ok {
			pos := fset.Position(n.Pos())
			hits = append(hits, pos.String()+": "+call)
		}
		return true
	})
	return hits
}

func walkGuardCall(n ast.Node, g walkGuardPkgs) (string, bool) {
	if sel, ok := n.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			name := sel.Sel.Name
			switch {
			case g.os[id.Name] && walkGuardOS[name]:
				return id.Name + "." + name, true
			case g.filepath[id.Name] && walkGuardFilepath[name]:
				return id.Name + "." + name, true
			case g.fs[id.Name] && walkGuardFS[name]:
				return id.Name + "." + name, true
			case g.syscall[id.Name] && walkGuardSyscall[name]:
				return id.Name + "." + name, true
			}
		}
		if walkGuardMethod[sel.Sel.Name] {
			return sel.Sel.Name, true
		}
		return "", false
	}
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	name := id.Name
	switch {
	case g.osDot && walkGuardOS[name]:
		return "os." + name, true
	case g.fpDot && walkGuardFilepath[name]:
		return "filepath." + name, true
	case g.fsDot && walkGuardFS[name]:
		return "fs." + name, true
	case g.sysDot && walkGuardSyscall[name]:
		return "syscall." + name, true
	}
	return "", false
}

// walkGuardPackages is the tasks package plus every internal package
// evaluator.go imports. That is the code a segment walk can reach.
func walkGuardPackages(root string) (map[string]bool, error) {
	path := filepath.Join(root, "internal", "tasks", "evaluator.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	pkgs := map[string]bool{"internal/tasks": true}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(p, "binlog_server/") {
			continue
		}
		rel := strings.TrimPrefix(p, "binlog_server/")
		if strings.HasPrefix(rel, "internal/") {
			pkgs[rel] = true
		}
	}
	return pkgs, nil
}

func walkGuardPaths(root string, pkgs map[string]bool) ([]string, error) {
	var out []string
	for _, rel := range pkgNames(pkgs) {
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			relFile := rel + "/" + name
			if _, ok := walkGuardAllow[relFile]; ok {
				continue
			}
			out = append(out, filepath.Join(root, relFile))
		}
	}
	return out, nil
}

func pkgNames(pkgs map[string]bool) []string {
	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
