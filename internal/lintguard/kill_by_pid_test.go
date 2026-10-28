package lintguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Scripts, workflows and the Makefile run on shared hosts, where another run
// uses the same command line: they kill only the PIDs they started ($! or a
// pidfile). Finding or killing a process by name is banned outright, in every
// tracked file under scripts/ and .github/ and in the Makefile, whatever its
// extension or language and comments included: nothing needs these tools.
// Markdown is prose, so a README or template may say not to use them.
var killByNameRE = regexp.MustCompile(`\b(?:pkill|killall|pgrep|pidof)\b`)

// killBanPaths are the tracked files the ban covers.
var killBanPaths = []string{"scripts", ".github", "Makefile"}

// killByName lists file:line for every banned token in root's tracked files.
func killByName(t *testing.T, root string) (hits []string, scanned []string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root, "ls-files", "-z", "--"}, killBanPaths...)...).Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p == "" || strings.HasSuffix(p, ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		scanned = append(scanned, p)
		for _, loc := range killByNameRE.FindAllIndex(raw, -1) {
			hits = append(hits, p+":"+strconv.Itoa(1+strings.Count(string(raw[:loc[0]]), "\n")))
		}
	}
	return hits, scanned
}

func TestScriptsKillOnlyTheirOwnPIDs(t *testing.T) {
	hits, scanned := killByName(t, filepath.Join("..", ".."))
	for _, h := range hits {
		t.Errorf("%s finds or kills a process by name; kill the PIDs this script started ($! or a pidfile) instead", h)
	}
	for _, want := range []string{"Makefile", ".github/workflows/e2e.yml", "scripts/e2e/upgrade-legacy-failback.sh"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("%s is not scanned (scanned %d files)", want, len(scanned))
		}
	}
}

// Every covered place and form is caught; files outside it are not scanned.
func TestKillByNameCoversEveryFile(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		".github/workflows/ci.yml":         "steps:\n  - run: pkill -f server\n",
		".github/actions/x/action.sh":      "kill $(pidof binlog-server)\n",
		"Makefile":                         "stop:\n\tkillall binlog-server\n",
		"scripts/a.bash":                   "echo \"#\"; pkill -f x\n",
		"scripts/noext":                    "curl http://x/#a && pkill x\n",
		"scripts/e2e/p.py":                 "subprocess.run([\"pkill\", \"-f\", \"x\"])\n",
		"scripts/b.sh":                     "# a comment still counts: pgrep\nkill \"$PID\"\n",
		"scripts/c.sh":                     "pgrep -f x | xargs kill\n",
		"scripts/ok.sh":                    "kill \"$SERVER_PID\"; pkilled=1; my_pgrep_x=2\n",
		"docs/outside.md":                  "pkill is fine in docs\n",
		"scripts/README.md":                "Never use pkill here.\n",
		".github/PULL_REQUEST_TEMPLATE.md": "- [ ] no pgrep or killall\n",
		"scripts/notes.md.sh":              "pidof x\n",
		"scripts/e2e/run-cmd":              "pkill -f x\n",
	}
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "untracked.sh"), []byte("pkill x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _ := killByName(t, dir)
	slices.Sort(hits)
	want := []string{
		".github/actions/x/action.sh:1", ".github/workflows/ci.yml:2", "Makefile:2",
		"scripts/a.bash:1", "scripts/b.sh:1", "scripts/c.sh:1", "scripts/e2e/p.py:1", "scripts/e2e/run-cmd:1",
		"scripts/noext:1", "scripts/notes.md.sh:1",
	}
	if !slices.Equal(hits, want) {
		t.Fatalf("hits %q\nwant %q", hits, want)
	}
}

// The failback e2e restarts the server several times; cleanup kills every
// server PID it recorded, so each start must record its PID.
func TestFailbackRecordsEveryServerPID(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "e2e", "upgrade-legacy-failback.sh"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	starts := 0
	for i, l := range lines {
		if strings.TrimSpace(l) != "SERVER_PID=$!" {
			continue
		}
		starts++
		if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != `echo "$SERVER_PID" >>"$SERVER_PIDFILE"` {
			t.Errorf("line %d: SERVER_PID=$! is not recorded in $SERVER_PIDFILE", i+1)
		}
	}
	if starts == 0 {
		t.Error("no SERVER_PID=$! start found")
	}
	if !strings.Contains(string(raw), `done <"$SERVER_PIDFILE"`) {
		t.Error("cleanup does not kill the PIDs in $SERVER_PIDFILE")
	}
}
