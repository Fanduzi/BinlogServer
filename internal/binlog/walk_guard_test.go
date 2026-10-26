// Package binlog provides module-level functionality for binlog.
// input: go list -deps ./internal/tasks, plus the walk entry package, with an explicit closed exclusion list
// output: a failure naming any os, io/fs, io/ioutil, syscall, or golang.org/x/sys symbol that is not on the non-I/O allowlist, and any filepath.Walk, WalkDir, Glob, or EvalSymlinks, outside the seam file or an off-path function
// pos: the structural guard so a nested walk cannot dodge WalkOps by naming a filesystem symbol from a package the tasks module reaches
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// walkGuardFileAllow skips a whole file. The seam is the only entry.
// Uncounted work inside that file is a known gap, not a scanned escape.
var walkGuardFileAllow = map[string]string{
	"internal/binlog/walk_ops.go": "counted seam; uncounted work inside this file is a known gap",
}

// walkFuncID is pkg path + receiver type + function name. A method and a
// function that share a name are different keys, as are two methods whose
// receivers differ.
type walkFuncID struct {
	pkg  string
	recv string
	name string
}

// walkGuardFuncAllow skips one function that is not on the evaluator walk.
// A new function, or the same name on another receiver, is still scanned.
var walkGuardFuncAllow = map[walkFuncID]string{
	{pkg: "binlog_server/internal/taskrecords", name: "LoadKey"}:                            "reads the operator key file at startup, not the segment walk",
	{pkg: "binlog_server/internal/taskrecords", name: "GenerateKeyFile"}:                    "writes a new key file, not the segment walk",
	{pkg: "binlog_server/internal/taskrecords", recv: "*Store", name: "Exists"}:             "stats the task-record file .tasks.enc, not a segment",
	{pkg: "binlog_server/internal/taskrecords", recv: "*Store", name: "Load"}:               "reads the task-record file, not a segment",
	{pkg: "binlog_server/internal/taskrecords", recv: "*Store", name: "RemoveStaleTemps"}:   "globs task-record temp files, not the segment directory",
	{pkg: "binlog_server/internal/taskrecords", name: "writeAtomic"}:                        "writes the task-record file and fsyncs its directory",
	{pkg: "binlog_server/internal/tasks", name: "resolvedAbs"}:                              "resolves a catalog path spelling for SEGMENT_NOT_ON_WORKER, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "symlinkEscapes"}:                           "checks a download target stays inside the task directory, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "writeAtomic"}:                              "writes the verifier progress file, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "LoadOrCreateNodeID"}:                       "writes the node id file, not the segment walk",
	{pkg: "binlog_server/internal/tasks", recv: "*fileFindingStore", name: "save"}:          "writes .findings.json, not the segment walk",
	{pkg: "binlog_server/internal/healthstore", recv: "*FileStore", name: "save"}:           "writes .health.json, not the segment walk",
	{pkg: "binlog_server/internal/binlog", name: "compactSealSums"}:                         "rewrites the seal checksum ledger, not the segment walk",
	{pkg: "binlog_server/internal/binlog", name: "RecordSealSum"}:                           "appends a seal-ledger row through the seam; Sync and Close are on that file",
	{pkg: "binlog_server/internal/binlog", name: "LoadSealSums"}:                            "reads the seal checksum ledger, not a segment directory",
	{pkg: "binlog_server/internal/binlog", name: "DurableCursor"}:                           "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "EventSpan"}:                               "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "PreambleOnly"}:                            "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "HeaderEventsOnly"}:                        "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "LastRotateTarget"}:                        "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "sealedSegmentCorrupt"}:                    "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "segmentContentsAt"}:                       "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "ScanOpenTail"}:                            "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "ReadSealFacts"}:                           "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "positionRegresses"}:                       "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/binlog", name: "strayRotate"}:                             "opens one segment through the seam and reads that file; the open is counted",
	{pkg: "binlog_server/internal/assurance", name: "bytePrefix"}:                           "opens two segments through the seam and compares their bytes; the opens are counted",
	{pkg: "binlog_server/internal/tasks", recv: "*Scheduler", name: "openLocalTaskSegment"}: "opens one named segment for download, not the evaluator directory walk",
	{pkg: "binlog_server/internal/tasks", name: "materializeReplayTar"}:                     "writes the replay tar, not the segment walk",
	{pkg: "binlog_server/internal/tasks", recv: "*removeOnClose", name: "Close"}:            "closes and deletes the replay tar, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "sealedSuccessorOf"}:                        "stats an already-open download to compare mtime, not the directory walk",
	{pkg: "binlog_server/internal/tasks", recv: "*Rehasher", name: "hashFile"}:              "closes the segment the seam opened for the rehasher",
	{pkg: "binlog_server/internal/tasks", name: "setIdleIOPriority"}:                        "sets the rehasher thread I/O priority, not a directory read",
	{pkg: "binlog_server/internal/tasks", name: "dropPageCache"}:                            "drops the page cache after a rehash read, not a directory read",
	{pkg: "binlog_server/internal/binlog", name: "readerRemaining"}:                         "stats and seeks an already-open segment to bound the read; the open is counted at the seam",
	{pkg: "binlog_server/internal/tasks", name: "writeLegacyJournal"}:                       "writes the legacy repair journal, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "writeChainFile"}:                           "writes the source-chain file, not the segment walk",
	{pkg: "binlog_server/internal/tasks", name: "syncFile"}:                                 "fsyncs a file the seam opened during legacy repair",
	{pkg: "binlog_server/internal/tasks", name: "syncDir"}:                                  "fsyncs a directory the seam opened during legacy repair",
	{pkg: "binlog_server/internal/tasks", name: "fetchLegacySegment"}:                       "writes one legacy segment into the task directory, not the evaluator walk",
	{pkg: "binlog_server/internal/tasks", name: "applyLegacyRepair"}:                        "renames repaired segment files, not the directory walk",
	{pkg: "binlog_server/internal/tasks", name: "defaultVerifyFileOps"}:                     "write, sync, close and remove hooks for the verifier progress file, not the segment walk",
}

// walkGuardScan is the closed set of internal packages this guard scans.
// A tasks dependency that is not here and not excluded fails the guard.
var walkGuardScan = map[string]bool{
	"binlog_server/internal/assurance":   true,
	"binlog_server/internal/binlog":      true,
	"binlog_server/internal/health":      true,
	"binlog_server/internal/healthstore": true,
	"binlog_server/internal/ownership":   true,
	"binlog_server/internal/taskrecords": true,
	"binlog_server/internal/tasks":       true,
}

// walkGuardExclude is the closed set of internal packages that are not the
// evaluator walk. Each value is the one-line reason. A dependency that is
// in neither this map nor walkGuardScan fails the guard.
var walkGuardExclude = map[string]string{
	"binlog_server/internal/replication": "replication writes the segments; it is not the evaluator walk",
	"binlog_server/internal/app":         "process startup and HTTP shutdown, not the segment walk",
	"binlog_server/internal/upload":      "object upload, not the segment walk",
}

// walkGuardSymbolAllow is the positive set of non-I/O symbols. A method's
// key is "Recv.Name" or "*Recv.Name". Anything else from os, io/fs,
// io/ioutil, syscall, or golang.org/x/sys fails. path/filepath is restricted
// only for Walk, WalkDir, Glob, and EvalSymlinks, and those four are not here.
var walkGuardSymbolAllow = map[string]map[string]bool{
	"os": {
		"Getenv": true, "Getpid": true, "Exit": true, "Hostname": true,
		"IsNotExist": true, "IsExist": true, "IsPermission": true, "IsTimeout": true,
		"ErrNotExist": true, "ErrExist": true, "ErrPermission": true, "ErrClosed": true,
		"File": true, "FileInfo": true, "FileMode": true, "DirEntry": true,
		"PathError": true, "LinkError": true,
		"ModeSymlink": true, "ModeDir": true, "ModePerm": true, "ModeType": true,
		"O_RDONLY": true, "O_WRONLY": true, "O_RDWR": true,
		"O_CREATE": true, "O_EXCL": true, "O_APPEND": true, "O_TRUNC": true,
		"PathSeparator": true,
		"*File.Name":    true, "*File.Fd": true,
	},
	"io/fs": {
		"ErrNotExist": true, "FileMode": true, "FileInfo": true, "DirEntry": true, "PathError": true,
		"FileInfo.Name": true, "FileInfo.Size": true, "FileInfo.Mode": true,
		"FileInfo.ModTime": true, "FileInfo.IsDir": true, "FileInfo.Sys": true,
		"FileMode.IsRegular": true, "FileMode.Perm": true, "FileMode.IsDir": true, "FileMode.Type": true,
		"DirEntry.Name": true, "DirEntry.IsDir": true, "DirEntry.Type": true,
		"Path": true, "Op": true, "Err": true,
	},
	"syscall": {
		"Stat_t": true, "Timespec": true, "Errno": true,
		"Ctim": true, "Atim": true, "Mtim": true, "Sec": true, "Nsec": true,
	},
	"golang.org/x/sys/unix": {
		"SYS_IOPRIO_SET": true, "FADV_DONTNEED": true,
	},
}

// walkGuardFileIO is the name set the allowlist test refuses. It is not the
// guard. The guard rejects every symbol that is absent from walkGuardSymbolAllow.
var walkGuardFileIO = map[string]bool{
	"Open": true, "OpenFile": true, "OpenRoot": true, "OpenInRoot": true, "Openat": true,
	"Create": true, "CreateTemp": true, "NewFile": true,
	"ReadDir": true, "ReadFile": true, "WriteFile": true, "Readlink": true, "ReadLink": true,
	"Remove": true, "RemoveAll": true, "Mkdir": true, "MkdirAll": true, "MkdirTemp": true,
	"Rename": true, "Stat": true, "Lstat": true, "Truncate": true, "Chmod": true, "Chown": true,
	"Chtimes": true, "Link": true, "Symlink": true, "DirFS": true,
	"Walk": true, "WalkDir": true, "Glob": true, "EvalSymlinks": true,
	"Getdents": true, "ReadDirent": true, "Getdirentries": true,
	"Fstat": true, "Fstatat": true, "Statfs": true, "Fstatfs": true,
	"Chdir": true, "Fchdir": true, "Getwd": true,
	"Read": true, "ReadAt": true, "Write": true, "WriteAt": true, "WriteString": true,
	"WriteTo": true, "ReadFrom": true, "Close": true, "Sync": true, "Seek": true,
	"Readdir": true, "Readdirnames": true, "Fadvise": true, "Syscall": true,
	"Fsync": true, "Fdatasync": true, "Unlink": true, "Unlinkat": true, "Rmdir": true,
}

var walkGuardFilepathIO = map[string]bool{
	"Walk": true, "WalkDir": true, "Glob": true, "EvalSymlinks": true,
}

const walkGuardRoute = "route it through the seam or justify adding it to the allowlist"

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
	for id, reason := range walkGuardFuncAllow {
		oneLine(id.pkg+" "+id.recv+" "+id.name, reason)
	}
	for path, reason := range walkGuardExclude {
		oneLine(path, reason)
	}
	if got := walkGuardExclude["binlog_server/internal/upload"]; got != "object upload, not the segment walk" {
		t.Fatalf("upload exclusion reason: %q", got)
	}
	deps, err := walkGuardDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	var scanned []string
	for _, dep := range deps {
		scanned = append(scanned, dep.rel)
	}
	sort.Strings(scanned)
	t.Logf("walk packages: %s", strings.Join(scanned, ", "))
	for _, want := range []string{
		"internal/tasks", "internal/binlog", "internal/assurance",
		"internal/health", "internal/healthstore", "internal/ownership", "internal/taskrecords",
	} {
		if !containsName(scanned, want) {
			t.Fatalf("packages reached from the tasks deps: %v, missing %s", scanned, want)
		}
	}
	fset := token.NewFileSet()
	var hits []string
	for _, dep := range deps {
		msg, err := walkGuardPackage(fset, dep)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, msg...)
	}
	if len(hits) != 0 {
		t.Fatalf("filesystem symbol outside the walk seam:\n%s", strings.Join(hits, "\n"))
	}
}

func TestWalkSeam_UnclassifiedPackageFails(t *testing.T) {
	err := classifyDep("binlog_server/internal/notlisted")
	if err == nil || !strings.Contains(err.Error(), "neither guarded nor excluded") {
		t.Fatalf("unclassified package: %v", err)
	}
}

func TestWalkSeam_UploadStaysExcluded(t *testing.T) {
	reason, ok := walkGuardExclude["binlog_server/internal/upload"]
	if !ok {
		t.Fatal("upload is not on the exclusion list")
	}
	if reason != "object upload, not the segment walk" {
		t.Fatalf("upload reason: %q", reason)
	}
	if walkGuardScan["binlog_server/internal/upload"] {
		t.Fatal("upload is both scanned and excluded")
	}
}

func TestWalkSeam_AllowlistHasNoFileIO(t *testing.T) {
	for pkg, syms := range walkGuardSymbolAllow {
		for key := range syms {
			name := key
			if i := strings.LastIndex(key, "."); i >= 0 {
				name = key[i+1:]
			}
			if walkGuardFileIO[name] {
				t.Errorf("%s.%s does file or directory I/O", pkg, key)
			}
		}
	}
	for name := range walkGuardFilepathIO {
		if walkGuardSymbolAllow["path/filepath"][name] {
			t.Errorf("path/filepath.%s is on the allowlist", name)
		}
	}
}

func TestWalkSeam_AllowlistRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "syscall-readdirent",
			src: `package p
import "syscall"
func f() {
	fd, err := syscall.Open("d", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil { return }
	var buf [8]byte
	_, _ = syscall.ReadDirent(fd, buf[:])
}
`,
			want: "syscall.Open",
		},
		{
			name: "unix-getdents",
			src: `package p
import "golang.org/x/sys/unix"
func f() {
	var buf [8]byte
	_, _ = unix.Getdents(0, buf[:])
}
`,
			want: "golang.org/x/sys/unix.Getdents",
		},
		{
			name: "openinroot",
			src:  "package p\nimport \"os\"\nfunc f() { _, _ = os.OpenInRoot(\"d\", \"n\") }\n",
			want: "os.OpenInRoot",
		},
		{
			name: "dirfs",
			src: `package p
import (
	"io/fs"
	"os"
)
func f() { _, _ = fs.ReadDir(os.DirFS("d"), ".") }
`,
			want: "os.DirFS",
		},
		{
			name: "chdir",
			src:  "package p\nimport \"os\"\nfunc f() error { return os.Chdir(\"x\") }\n",
			want: "os.Chdir",
		},
		{
			name: "dot-import",
			src:  "package p\nimport . \"os\"\nfunc f() { _, _ = ReadDir(\"x\") }\n",
			want: "os.ReadDir",
		},
		{
			name: "alias",
			src:  "package p\nimport s \"os\"\nfunc f() { var rd = s.ReadDir; _, _ = rd(\"x\") }\n",
			want: "os.ReadDir",
		},
		{
			name: "file-readdir",
			src:  "package p\nimport \"os\"\nfunc f() { fh, _ := os.Open(\"x\"); _, _ = fh.ReadDir(0) }\n",
			want: "os.*File.ReadDir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "chdir" && walkGuardSymbolAllow["os"]["Chdir"] {
				t.Fatal("os.Chdir is on the allowlist; the hypothetical function no longer proves allowlist semantics")
			}
			hits := walkGuardSnippet(t, "p", tc.src, nil)
			if !containsCall(hits, tc.want) {
				t.Fatalf("hits %v, want %s", hits, tc.want)
			}
			for _, hit := range hits {
				if !strings.Contains(hit, walkGuardRoute) {
					t.Fatalf("hit %q does not say to %s", hit, walkGuardRoute)
				}
			}
			if tc.name == "syscall-readdirent" && !containsCall(hits, "syscall.ReadDirent") {
				t.Fatalf("hits %v, want syscall.ReadDirent", hits)
			}
			if tc.name == "dirfs" && !containsCall(hits, "io/fs.ReadDir") {
				t.Fatalf("hits %v, want io/fs.ReadDir", hits)
			}
		})
	}
	typeOnly := "package p\nimport \"syscall\"\nfunc f(st *syscall.Stat_t) { _ = st.Ctim.Sec }\n"
	if hits := walkGuardSnippet(t, "p", typeOnly, nil); len(hits) != 0 {
		t.Fatalf("Stat_t fields are not I/O: %v", hits)
	}
	seam := "package p\nimport b \"binlog_server/internal/binlog\"\nfunc f() { _, _ = b.ReadDir(\"x\") }\n"
	if hits := walkGuardSnippet(t, "p", seam, nil); len(hits) != 0 {
		t.Fatalf("seam ReadDir is the counted call: %v", hits)
	}
}

func TestWalkSeam_SameNameMethodIsNotAllowlisted(t *testing.T) {
	src := `package taskrecords
import "os"
type Store struct{}
type relister struct{}
func (s *Store) Load(dir string) { _, _ = os.ReadDir(dir) }
func (relister) Load(dir string) { _, _ = os.ReadDir(dir) }
`
	allow := map[walkFuncID]string{
		{pkg: "binlog_server/internal/taskrecords", recv: "*Store", name: "Load"}: "the task-record load",
	}
	hits := walkGuardSnippet(t, "binlog_server/internal/taskrecords", src, allow)
	if len(hits) != 1 || !containsCall(hits, "os.ReadDir") {
		t.Fatalf("relister.Load should be the only hit: %v", hits)
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

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func walkGuardSnippet(t *testing.T, pkgPath, src string, allow map[walkFuncID]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	return walkGuardUses(fset, pkg, []*ast.File{f}, info, allow)
}

type depPkg struct {
	path string
	dir  string
	rel  string
}

func classifyDep(imp string) error {
	if reason, ok := walkGuardExclude[imp]; ok {
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			return fmt.Errorf("%s: exclusion reason must be one non-empty line", imp)
		}
		return nil
	}
	if walkGuardScan[imp] {
		return nil
	}
	return fmt.Errorf("%s is in the tasks dependency set and is neither guarded nor excluded", imp)
}

func walkGuardDeps(root string) ([]depPkg, error) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}", "./internal/tasks")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go list -deps ./internal/tasks: %w\n%s", err, ee.Stderr)
		}
		return nil, err
	}
	var deps []depPkg
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		imp, dir, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !strings.HasPrefix(imp, "binlog_server/internal/") {
			continue
		}
		if err := classifyDep(imp); err != nil {
			return nil, err
		}
		if _, excluded := walkGuardExclude[imp]; excluded {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			continue
		}
		seen[rel] = true
		deps = append(deps, depPkg{path: imp, dir: dir, rel: rel})
	}
	if !seen["internal/tasks"] {
		return nil, fmt.Errorf("internal/tasks is not in the dependency set")
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].rel < deps[j].rel })
	return deps, nil
}

func walkGuardPackage(fset *token.FileSet, dep depPkg) ([]string, error) {
	entries, err := os.ReadDir(dep.dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := build.Default.MatchFile(dep.dir, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dep.dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(dep.path, fset, files, info)
	if err != nil {
		return nil, err
	}
	return walkGuardUses(fset, pkg, files, info, walkGuardFuncAllow), nil
}

func walkGuardUses(fset *token.FileSet, pkg *types.Package, files []*ast.File, info *types.Info, allow map[walkFuncID]string) []string {
	type span struct {
		from, to token.Pos
	}
	var skip []span
	for _, f := range files {
		rel := fileRel(fset.Position(f.Pos()).Filename)
		if _, ok := walkGuardFileAllow[rel]; ok {
			skip = append(skip, span{f.Pos(), f.End()})
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name == nil {
				continue
			}
			id := walkFuncID{pkg: pkg.Path(), name: fn.Name.Name}
			if obj := info.Defs[fn.Name]; obj != nil {
				if sig, ok := obj.Type().(*types.Signature); ok && sig.Recv() != nil {
					id.recv = recvKey(sig.Recv().Type())
				}
			}
			if _, ok := allow[id]; ok {
				skip = append(skip, span{fn.Body.Pos(), fn.Body.End()})
			}
		}
	}
	skipped := func(p token.Pos) bool {
		for _, s := range skip {
			if p >= s.from && p < s.to {
				return true
			}
		}
		return false
	}
	var hits []string
	for id, obj := range info.Uses {
		if skipped(id.Pos()) {
			continue
		}
		if _, ok := obj.(*types.PkgName); ok || obj == nil || obj.Pkg() == nil {
			continue
		}
		pkgPath := obj.Pkg().Path()
		key := symbolKey(obj)
		if !symbolRestricted(pkgPath, key) {
			continue
		}
		if walkGuardSymbolAllow[pkgPath][key] {
			continue
		}
		pos := fset.Position(id.Pos())
		hits = append(hits, fmt.Sprintf("%s:%d:%d: %s.%s: %s", filepath.ToSlash(pos.Filename), pos.Line, pos.Column, pkgPath, key, walkGuardRoute))
	}
	sort.Strings(hits)
	return hits
}

func symbolRestricted(pkgPath, key string) bool {
	switch pkgPath {
	case "os", "io/fs", "io/ioutil", "syscall":
		return true
	case "path/filepath":
		name := key
		if i := strings.LastIndex(key, "."); i >= 0 {
			name = key[i+1:]
		}
		return walkGuardFilepathIO[name]
	default:
		return strings.HasPrefix(pkgPath, "golang.org/x/sys/")
	}
}

func symbolKey(obj types.Object) string {
	key := obj.Name()
	fn, ok := obj.(*types.Func)
	if !ok {
		return key
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return key
	}
	return recvKey(sig.Recv().Type()) + "." + fn.Name()
}

func recvKey(t types.Type) string {
	star := ""
	if p, ok := t.(*types.Pointer); ok {
		star = "*"
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return star + n.Obj().Name()
	}
	return star + t.String()
}

func fileRel(filename string) string {
	slash := filepath.ToSlash(filename)
	if i := strings.Index(slash, "internal/"); i >= 0 {
		return slash[i:]
	}
	return slash
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
