// Package replication provides module-level functionality for replication.
// input: a TCP listener that accepts and never answers (black-holed source)
// output: proof that every source connection is bounded and that nothing opens one outside connectSource
// pos: #338 QA P1 regression tests for ADR 0009 S12
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

// blackHole accepts TCP connections and never writes a byte.
func blackHole(t *testing.T) tasks.SourceConfig {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range held {
			c.Close()
		}
	})
	addr := ln.Addr().(*net.TCPAddr)
	return tasks.SourceConfig{Host: "127.0.0.1", Port: uint16(addr.Port), User: "u", Password: "p"}
}

func within(t *testing.T, limit time.Duration, name string, fn func()) {
	t.Helper()
	ch := make(chan struct{})
	go func() { fn(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s against a black-holed source", name, limit)
	}
}

// #338 QA P1: the idle tip probe opened an unbounded connection and froze
// the dump loop, so no stall was ever reported and no reconnect happened.
func TestSourceProbesAreBoundedOnBlackHole(t *testing.T) {
	src := blackHole(t)
	f := &mysqlStatusFetcher{}
	within(t, 15*time.Second, "FetchMasterStatus", func() { _, _ = f.FetchMasterStatus(context.Background(), src) })
	within(t, 15*time.Second, "FetchServerUUID", func() { _, _ = f.FetchServerUUID(context.Background(), src) })
	within(t, 15*time.Second, "ProbeSwitchGTID", func() {
		_, _, _, _ = f.ProbeSwitchGTID(context.Background(), src, "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5")
	})
}

// An idle dump whose tip probe hits a black-holed source still reports the
// stall and returns an error so the dump reconnects.
func TestIdleDumpOnBlackHoleStallsAndReconnects(t *testing.T) {
	src := blackHole(t)
	r := NewMySQLRunner(t.TempDir())
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	r.clock = clock.now
	rec := bindRecorder(r)
	st := &fakeStreamer{results: []streamResult{{event: xidAt(100)}, {err: context.DeadlineExceeded}}}
	if _, err := r.nextEvent(context.Background(), st, "bh", src, "mysql-bin.000003", 100, nil); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(dumpReadTimeout + time.Second)
	var err error
	within(t, 15*time.Second, "nextEvent", func() {
		_, err = r.nextEvent(context.Background(), st, "bh", src, "mysql-bin.000003", 100, nil)
	})
	if err == nil {
		t.Fatal("a dump silent past the read timeout must return an error so it reconnects")
	}
	if _, ok := rec.has("bh", tasks.FindingDumpStalled, ""); !ok {
		t.Fatalf("no DUMP_STALLED: %v", rec.open)
	}
}

// Every source connection goes through connectSource, which bounds dial,
// read and write. Any other call into go-mysql's client Connect* fails here.
func TestSourceConnectionsOnlyThroughConnectSource(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		aliases := map[string]bool{}
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != "github.com/go-mysql-org/go-mysql/client" {
				continue
			}
			name := "client"
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = true
		}
		if len(aliases) == 0 {
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || !aliases[id.Name] || !strings.HasPrefix(sel.Sel.Name, "Connect") {
					return true
				}
				if filepath.Base(path) == "source_conn.go" && fn.Name.Name == "connectSource" {
					return true
				}
				bad = append(bad, fset.Position(sel.Pos()).String()+" in "+fn.Name.Name)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("source connection opened outside connectSource (no bounded read/write timeout):\n%s", strings.Join(bad, "\n"))
	}
}
