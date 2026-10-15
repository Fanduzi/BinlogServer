// Package lintguard provides module-level functionality for lintguard.
// input: Go source directories and the set of FindingKind constant names whose kind the health evaluator manages
// output: Site values (kind managed-write) for every call named Report, Resolve, report or resolve, as a method (s.Report, logger.Report, r.report) or a plain function value, whose arguments name a managed Finding constant
// pos: ADR 0009 phase 2 P2-4 (design §5.6, #342): a managed kind has one writer, the evaluator; hot paths call SignalFailure/SignalSuccess
// note: if this file changes, update this header and module README.md.
package lintguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ManagedWrite is a Report/Resolve of a kind the evaluator manages.
const ManagedWrite Kind = "managed-write"

var writerNames = map[string]bool{"Report": true, "Resolve": true, "report": true, "resolve": true}

var kindConst = regexp.MustCompile(`^\s*(Finding\w+)\s+FindingKind\s*=\s*"([A-Z0-9_]+)"`)

// FindingConstants maps each `FindingX FindingKind = "KIND"` constant
// declared in dir's Go files to its kind string.
func FindingConstants(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, en := range entries {
		if !strings.HasSuffix(en.Name(), ".go") || strings.HasSuffix(en.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, en.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if m := kindConst.FindStringSubmatch(line); m != nil {
				out[m[1]] = m[2]
			}
		}
	}
	return out, nil
}

// ScanManagedWrites returns the managed-write sites in dir's non-test Go
// files; managed holds constant names (FindingDumpStalled), matched bare
// or behind a package selector (tasks.FindingDumpStalled).
func ScanManagedWrites(root, dir string, managed map[string]bool) ([]Site, error) {
	var out []Site
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				name = recvName(fn.Recv.List[0].Type) + "." + name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !writerNames[calleeName(call.Fun)] {
					return true
				}
				for _, arg := range call.Args {
					if namesManaged(arg, managed) {
						out = append(out, Site{rel, name, ManagedWrite, fset.Position(call.Pos()).Line})
						break
					}
				}
				return true
			})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, err
}

func calleeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func namesManaged(e ast.Expr, managed map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.Ident:
			if managed[t.Name] {
				found = true
			}
		case *ast.BasicLit:
			if t.Kind == token.STRING {
				if s, err := strconv.Unquote(t.Value); err == nil && managed["\x00"+s] {
					found = true
				}
			}
		}
		return !found
	})
	return found
}
