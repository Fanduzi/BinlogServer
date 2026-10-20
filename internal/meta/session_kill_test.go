// Package meta provides module-level functionality for meta.
// input: a session-kill bound and the side-kill sources
// output: proof that a mismatched wait_timeout is degraded, a missing procedure stays off, and the client sends no kill statement
// pos: ADR 0011 PR-2 P2-8 and P2-9 without a server
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSessionTimeoutMismatchIsDegraded(t *testing.T) {
	st := &sessionState{expectSec: 5}
	st.noteTimeout(28800)
	store := &MySQLTaskStore{session: st}
	ok, detail := store.SessionDegraded()
	if !ok || !strings.Contains(detail, "28800") {
		t.Fatalf("degraded %v %q", ok, detail)
	}
	st.noteTimeout(5)
	ok, detail = store.SessionDegraded()
	if ok || detail != "" {
		t.Fatalf("match stayed degraded %v %q", ok, detail)
	}
}

func TestSideKillStaysOffWhenProcedureMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{mode: "auto", bound: time.Second, flavor: "mysql", armed: "off"}
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM information_schema.routines").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	store.RefreshSideKill(context.Background(), "n1")
	if store.SideKill() != "off" {
		t.Fatalf("side_kill %s, want off", store.SideKill())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSideKillOffDoesNotQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{mode: "off", bound: time.Second, flavor: "mysql"}
	store.RefreshSideKill(context.Background(), "n1")
	if store.SideKill() != "off" {
		t.Fatalf("side_kill %s", store.SideKill())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSideKillClientDoesNotSendKill(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	killWord := regexp.MustCompile(`(?i)\bKILL\s`)
	for _, name := range []string{"side_kill.go", "session_kill.go"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if killWord.MatchString(b.String()) {
			t.Fatalf("%s contains a kill statement", name)
		}
	}
}
