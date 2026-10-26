// Package binlog provides module-level functionality for binlog.
// input: go list -deps ./internal/tasks, plus the walk entry package, excluding replication, app and upload
// output: a failure naming any banned filesystem call outside the seam file or an off-path function allowlist
// pos: the structural guard so a nested walk cannot dodge WalkOps by calling os, filepath, io/fs, ioutil, syscall or unix from a package the tasks module reaches
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// walkGuardFileAllow skips a whole file. The seam is the only entry.
// Uncounted work inside that file is a known gap, not a scanned escape.
var walkGuardFileAllow = map[string]string{
	"internal/binlog/walk_ops.go": "counted seam; uncounted work inside this file is a known gap",
}

// walkGuardFuncAllow skips one function that is not on the evaluator walk.
// A new function in the same file is still scanned.
var walkGuardFuncAllow = map[string]map[string]string{
	"internal/taskrecords/taskrecords.go": {
		"LoadKey":          "reads the operator key file at startup, not the segment walk",
		"GenerateKeyFile":  "writes a new key file, not the segment walk",
		"Exists":           "stats the task-record file .tasks.enc, not a segment",
		"Load":             "reads the task-record file, not a segment",
		"RemoveStaleTemps": "globs task-record temp files, not the segment directory",
		"writeAtomic":      "writes the task-record file and fsyncs its directory",
	},
	"internal/tasks/resume.go": {
		"resolvedAbs": "resolves a catalog path spelling for SEGMENT_NOT_ON_WORKER, not the segment walk",
	},
	"internal/tasks/segment_download.go": {
		"symlinkEscapes": "checks a download target stays inside the task directory, not the segment walk",
	},
}

// walkGuardExclude drops packages that go list may reach but that are not
// the evaluator walk. Each value is the one-line reason.
var walkGuardExclude = map[string]string{
	"binlog_server/internal/replication": "replication writes the segments; it is not the evaluator walk",
	"binlog_server/internal/app":         "process startup and HTTP shutdown, not the segment walk",
	"binlog_server/internal/upload":      "object upload, not the segment walk",
}

var walkGuardOS = map[string]bool{
	"Stat": true, "Lstat": true, "Open": true, "OpenFile": true, "OpenRoot": true,
	"ReadDir": true, "ReadFile": true, "DirFS": true,
}

var walkGuardFilepath = map[string]bool{"Walk": true, "WalkDir": true, "Glob": true, "EvalSymlinks": true}

var walkGuardIOUtil = map[string]bool{"ReadDir": true, "ReadFile": true}

var walkGuardFS = map[string]bool{
	"Stat": true, "ReadDir": true, "ReadFile": true, "WalkDir": true, "Glob": true, "DirFS": true,
}

var walkGuardSyscall = map[string]bool{
	"Stat": true, "Lstat": true, "Fstat": true, "Fstatat": true,
}

// ReadDir, Readdir and Readdirnames are methods on *os.File and *os.Root.
// OpenRoot is the *os.Root method. Package calls such as binlog.ReadDir are
// not methods: the receiver is the import name.
var walkGuardMethod = map[string]bool{
	"ReadDir": true, "Readdir": true, "Readdirnames": true, "OpenRoot": true,
}

func TestWalkSeam_NoDirectOSOutsideSeam(t *testing.T) {
	root := moduleRoot(t)
	oneLine := func(where, reason string) {
		t.Helper()
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			t.Fatalf("%s: reason must be one non-empty line", where)
		}
	}
	for path, reason := range walkGuardFileAllow {
		oneLine(path, reason)
	}
	for path, fns := range walkGuardFuncAllow {
		for fn, reason := range fns {
			oneLine(path+" "+fn, reason)
		}
	}
	for path, reason := range walkGuardExclude {
		oneLine(path, reason)
	}
	pkgs, err := walkGuardPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"internal/tasks", "internal/binlog", "internal/assurance",
		"internal/health", "internal/healthstore", "internal/ownership", "internal/taskrecords",
	} {
		if !pkgs[want] {
			t.Fatalf("packages reached from the tasks deps: %v, missing %s", pkgNames(pkgs), want)
		}
	}
	for imp := range walkGuardExclude {
		rel := strings.TrimPrefix(imp, "binlog_server/")
		if pkgs[rel] {
			t.Fatalf("excluded package %s was scanned (%s)", rel, walkGuardExclude[imp])
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
		msg, err := walkGuardFile(fset, root, path)
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
		{"ioutil", "package p\nimport \"io/ioutil\"\nfunc f() { _, _ = ioutil.ReadDir(\"x\") }\n", "ioutil.ReadDir"},
		{"ioutilread", "package p\nimport \"io/ioutil\"\nfunc f() { _, _ = ioutil.ReadFile(\"x\") }\n", "ioutil.ReadFile"},
		{"openroot", "package p\nimport \"os\"\nfunc f() { _, _ = os.OpenRoot(\"x\") }\n", "os.OpenRoot"},
		{"evalsymlinks", "package p\nimport \"path/filepath\"\nfunc f() { _, _ = filepath.EvalSymlinks(\"x\") }\n", "filepath.EvalSymlinks"},
		{"filereaddir", "package p\nimport \"os\"\nfunc f() { fh, _ := os.Open(\"x\"); _, _ = fh.ReadDir(0) }\n", "ReadDir"},
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
	seam := "package p\nimport b \"binlog_server/internal/binlog\"\nfunc f() { _, _ = b.ReadDir(\"x\") }\n"
	if hits := walkGuardSource(t, "flagged.go", seam); len(hits) != 0 {
		t.Fatalf("seam ReadDir is the counted call: %v", hits)
	}
}

func TestWalkSeam_FuncAllowIsOneFunction(t *testing.T) {
	src := "package taskrecords\nimport \"os\"\nfunc LoadKey() { _, _ = os.Stat(\"k\") }\nfunc Extra() { _, _ = os.ReadDir(\"x\") }\n"
	hits := walkGuardSource(t, "internal/taskrecords/taskrecords.go", src)
	if len(hits) != 1 || !containsCall(hits, "os.ReadDir") {
		t.Fatalf("new function should be flagged, LoadKey should not: %v", hits)
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
	return walkGuardAST(fset, name, f)
}

func walkGuardFile(fset *token.FileSet, root, path string) ([]string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	return walkGuardAST(fset, filepath.ToSlash(rel), f), nil
}

type walkGuardPkgs struct {
	os, filepath, fs, ioutil, syscall  map[string]bool
	names                              map[string]bool
	osDot, fpDot, fsDot, ioDot, sysDot bool
}

func walkGuardImports(f *ast.File) walkGuardPkgs {
	g := walkGuardPkgs{
		os: map[string]bool{}, filepath: map[string]bool{},
		fs: map[string]bool{}, ioutil: map[string]bool{},
		syscall: map[string]bool{}, names: map[string]bool{},
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
		case "io/ioutil":
			dst, dot, def = g.ioutil, &g.ioDot, "ioutil"
		case "syscall":
			dst, dot, def = g.syscall, &g.sysDot, "syscall"
		case "golang.org/x/sys/unix":
			dst, dot, def = g.syscall, &g.sysDot, "unix"
		case "binlog_server/internal/binlog":
			def = "binlog"
		default:
			continue
		}
		if imp.Name != nil && imp.Name.Name == "." {
			if dot != nil {
				*dot = true
			}
			continue
		}
		local := def
		if imp.Name != nil {
			local = imp.Name.Name
		}
		g.names[local] = true
		if dst != nil {
			dst[local] = true
		}
	}
	return g
}

func walkGuardAST(fset *token.FileSet, rel string, f *ast.File) []string {
	g := walkGuardImports(f)
	skip := walkGuardFuncAllow[rel]
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Name != nil && skip[fn.Name.Name] != "" {
			return false
		}
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
			case g.ioutil[id.Name] && walkGuardIOUtil[name]:
				return id.Name + "." + name, true
			}
			if g.names[id.Name] {
				return "", false
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
	case g.ioDot && walkGuardIOUtil[name]:
		return "ioutil." + name, true
	}
	return "", false
}

// walkGuardPackages is go list -deps ./internal/tasks, kept to this module's
// internal packages, plus the walk entry package itself. replication, app
// and upload are dropped even if a future import reaches them.
func walkGuardPackages(root string) (map[string]bool, error) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}", "./internal/tasks")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go list -deps ./internal/tasks: %w\n%s", err, ee.Stderr)
		}
		return nil, err
	}
	pkgs := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		imp, dir, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !strings.HasPrefix(imp, "binlog_server/internal/") {
			continue
		}
		if _, excluded := walkGuardExclude[imp]; excluded {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return nil, err
		}
		pkgs[filepath.ToSlash(rel)] = true
	}
	pkgs["internal/tasks"] = true
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
			if _, ok := walkGuardFileAllow[relFile]; ok {
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
