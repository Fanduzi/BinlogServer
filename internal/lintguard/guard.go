// Package lintguard finds ignored errors in the backup data path (ADR 0009 phase 0).
// input: Go source directories
// output: Site values for `_ = call(...)` statements and `if err != nil { log... }` blocks whose body only logs
// pos: the scanner behind ignored_errors_test.go; runs inside `go test ./...`, so CI enforces it without a workflow change
// note: if this file changes, update this header and module README.md.
package lintguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind names what was ignored.
type Kind string

const (
	// BlankCall is `_ = f(...)` (or `_, _ = f(...)`): the result, usually an error, is thrown away.
	BlankCall Kind = "blank-call"
	// LogOnly is `if err != nil { log.Printf(...) }`: the error is logged and nothing else happens.
	LogOnly Kind = "log-only"
)

// Site is one ignored error. Key is stable across line moves: file, enclosing func and kind.
type Site struct {
	File string
	Func string
	Kind Kind
	Line int
}

// Key is "file:func:kind".
func (s Site) Key() string { return s.File + ":" + s.Func + ":" + string(s.Kind) }

// Scan walks dir (non-recursive into testdata) and returns sites in non-test Go files, with paths relative to root.
func Scan(root, dir string) ([]Site, error) {
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
		out = append(out, scanFile(fset, filepath.ToSlash(rel), f)...)
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

func scanFile(fset *token.FileSet, rel string, f *ast.File) []Site {
	var out []Site
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
			switch st := n.(type) {
			case *ast.AssignStmt:
				if allBlank(st.Lhs) && len(st.Rhs) == 1 {
					if _, call := st.Rhs[0].(*ast.CallExpr); call {
						out = append(out, Site{rel, name, BlankCall, fset.Position(st.Pos()).Line})
					}
				}
			case *ast.IfStmt:
				if errCheck(st.Cond) && st.Else == nil && logOnly(st.Body) {
					out = append(out, Site{rel, name, LogOnly, fset.Position(st.Pos()).Line})
				}
			}
			return true
		})
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

func allBlank(lhs []ast.Expr) bool {
	for _, e := range lhs {
		id, ok := e.(*ast.Ident)
		if !ok || id.Name != "_" {
			return false
		}
	}
	return len(lhs) > 0
}

// errCheck matches `err != nil` and `xErr != nil` (also inside &&/||).
func errCheck(e ast.Expr) bool {
	b, ok := e.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	if b.Op == token.LAND || b.Op == token.LOR {
		return errCheck(b.X) || errCheck(b.Y)
	}
	if b.Op != token.NEQ {
		return false
	}
	id, ok := b.X.(*ast.Ident)
	nilID, okNil := b.Y.(*ast.Ident)
	return ok && okNil && nilID.Name == "nil" && (id.Name == "err" || strings.HasSuffix(id.Name, "Err"))
}

// logOnly reports a non-empty block made only of log.* calls.
func logOnly(b *ast.BlockStmt) bool {
	if len(b.List) == 0 {
		return false
	}
	for _, s := range b.List {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return false
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || (pkg.Name != "log" && pkg.Name != "slog") {
			return false
		}
	}
	return true
}
