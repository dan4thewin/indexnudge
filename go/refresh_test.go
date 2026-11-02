package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func refreshFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeRefreshSource(t, dir, "package fixture\nfunc OldName() {}\n")
	path := filepath.Join(dir, ".indexnudge #?.db")
	if err := fullIndex(dir, path); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func writeRefreshSource(t *testing.T, dir, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
}

func refreshReader(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func assertUnavailable(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"files", "symbols", "lines_fts", "callee_candidates"} {
		var n int
		err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n)
		if err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Fatalf("%s should be absent, got %v", table, err)
		}
	}
	status := queryStrings(t, db, "SELECT status FROM _indexnudge_state")
	if len(status) != 1 || status[0] != "unavailable" {
		t.Fatalf("unexpected publication state %v", status)
	}
	if queryInt(t, db, "SELECT count(*) FROM _indexnudge_old_symbols WHERE name='OldName'") != 1 {
		t.Fatal("old private snapshot was not retained")
	}
}

func TestPublicationNoOpAndBackdatedChange(t *testing.T) {
	dir, path := refreshFixture(t)
	reader := refreshReader(t, path)
	queryInt(t, reader, "SELECT count(*) FROM files")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doFreshen(dir, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("no-op changed database pages")
	}
	source := filepath.Join(dir, "lib.go")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	writeRefreshSource(t, dir, "package fixture\nfunc NewName() {}\n")
	older := info.ModTime().Add(-time.Minute)
	if err := os.Chtimes(source, older, older); err != nil {
		t.Fatal(err)
	}
	if err := doFreshen(dir, path); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, reader, "SELECT count(*) FROM symbols WHERE name='NewName'") != 1 {
		t.Fatal("backdated edit with unchanged size was missed")
	}
}

func TestPublicationFailureAndRecovery(t *testing.T) {
	for _, trigger := range []string{
		"BEFORE INSERT ON files",
		"BEFORE INSERT ON defs WHEN NEW.var='a'",
	} {
		t.Run(trigger, func(t *testing.T) {
			dir, path := refreshFixture(t)
			reader := refreshReader(t, path)
			_, err := reader.Exec(`
CREATE TABLE "custom ""table" (value TEXT);
INSERT INTO "custom ""table" VALUES ('keep');
CREATE VIEW "custom ""view" AS SELECT name FROM symbols;
CREATE VIEW nested AS SELECT * FROM "custom ""view";
CREATE TRIGGER reject_insert ` + trigger + ` BEGIN
    SELECT RAISE(ABORT, 'injected insert failure');
END;`)
			if err != nil {
				t.Fatal(err)
			}
			writeRefreshSource(t, dir, "package fixture\nfunc NewName() { a,b := 1,2; _,_ = a,b }\n")
			err = doFreshen(dir, path)
			if err == nil || !strings.Contains(err.Error(), "injected insert failure") {
				t.Fatalf("refresh must propagate the write error: %v", err)
			}
			assertUnavailable(t, reader)
			var n int
			if err := reader.QueryRow("SELECT count(*) FROM nested").Scan(&n); err == nil {
				t.Fatal("custom view still exposes old data")
			}
			if queryInt(t, reader, "SELECT count(*) FROM _indexnudge_old_lines_fts WHERE content MATCH 'OldName'") != 1 {
				t.Fatal("private FTS snapshot was not rolled back")
			}
			if _, err := reader.Exec("DROP TRIGGER reject_insert"); err != nil {
				t.Fatal(err)
			}
			if err := doFreshen(dir, path); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				"SELECT count(*) FROM nested WHERE name='NewName'",
				"SELECT count(*) FROM lines_fts WHERE content MATCH 'NewName'",
				`SELECT count(*) FROM "custom ""table" WHERE value='keep'`,
			} {
				if queryInt(t, reader, query) != 1 {
					t.Fatalf("restored data missing: %s", query)
				}
			}
			if queryInt(t, reader, "SELECT count(*) FROM _indexnudge_objects") != 0 {
				t.Fatal("publication left a pending manifest")
			}
			if got := queryStrings(t, reader, "PRAGMA integrity_check"); len(got) != 1 || got[0] != "ok" {
				t.Fatalf("integrity check: %v", got)
			}
		})
	}
}

func TestPublicationScanAndRebuildFailure(t *testing.T) {
	for _, mode := range []string{"scan", "rebuild", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
			dir, path := refreshFixture(t)
			reader := refreshReader(t, path)
			if mode == "upgrade" {
				if _, err := reader.Exec("DROP TABLE _indexnudge_objects; DROP TABLE _indexnudge_state; PRAGMA user_version=1"); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := openWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			_, workErr := runLocked(writer.conn, filepath.Join(dir, "missing"), mode != "rebuild")
			if err := writer.close(); err != nil {
				t.Fatal(err)
			}
			if workErr == nil {
				t.Fatal("scan of missing directory succeeded")
			}
			assertUnavailable(t, reader)
			if mode == "upgrade" && queryInt(t, reader, "PRAGMA user_version") != 1 {
				t.Fatal("failed migration published its new version")
			}
			if err := doFreshen(dir, path); err != nil {
				t.Fatal(err)
			}
			if queryInt(t, reader, "SELECT count(*) FROM files") != 1 ||
				queryInt(t, reader, "PRAGMA user_version") != SchemaVersion {
				t.Fatal("retry did not finish publication")
			}
		})
	}
}

func TestPublicationLockLifetime(t *testing.T) {
	dir, path := refreshFixture(t)
	reader := refreshReader(t, path)
	readTx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := readTx.QueryRow("SELECT count(*) FROM files").Scan(&n); err != nil {
		t.Fatal(err)
	}
	writer, err := openWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	ctx := context.Background()
	if _, err := writer.conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	if tx, err := writer.conn.BeginTx(ctx, nil); err == nil {
		tx.Rollback()
		t.Fatal("writer bypassed active reader")
	}
	if err := readTx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertBusy := func() {
		t.Helper()
		err := reader.QueryRow("SELECT count(*) FROM files").Scan(&n)
		if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
			t.Fatalf("reader bypassed exclusive lock: %v", err)
		}
	}
	tx, err := writer.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertBusy()
	if err := withdrawIndex(tx, true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertBusy()
	tx, err = writer.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndex(tx); err != nil {
		t.Fatal(err)
	}
	writeRefreshSource(t, dir, "package fixture\nfunc NewName() {}\n")
	if _, err := updateFiles(tx, dir, false); err != nil {
		t.Fatal(err)
	}
	if err := publishIndex(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertBusy()
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, reader, "SELECT count(*) FROM symbols WHERE name='NewName'") != 1 {
		t.Fatal("physical connection close did not release new publication")
	}
}

func TestFailedCommitClosesNativeTransaction(t *testing.T) {
	_, path := refreshFixture(t)
	writer, err := openWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	ctx := context.Background()
	if _, err := writer.conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := withdrawIndex(tx, true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = writer.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndex(tx); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY);
CREATE TABLE child (id INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED);
INSERT INTO child VALUES (1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("expected deferred constraint failure")
	}
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("unexpected transaction state: %v", err)
	}
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(t, refreshReader(t, path))
}

func TestCapturedSourceMatchesASTAndStoredLines(t *testing.T) {
	dir, _ := refreshFixture(t)
	path := filepath.Join(dir, "lib.go")
	source := sampleSource(t, path)
	writeRefreshSource(t, dir, "package fixture\nfunc NewName() {}\n")
	fset, files, info, err := loadPackages(dir, map[string]sourceFile{path: source})
	if err != nil {
		t.Fatal(err)
	}
	file, ok := files[path]
	if !ok {
		t.Fatal("captured file missing from loaded package")
	}
	db := setupDB(t)
	if err := indexFile(db, fset, "lib.go", source, file, info); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, db, "SELECT count(*) FROM symbols WHERE name='OldName'") != 1 ||
		queryInt(t, db, "SELECT count(*) FROM lines WHERE content LIKE '%OldName%'") != 1 {
		t.Fatal("AST and stored source must come from the same captured bytes")
	}
	if changed, err := hasChanges(db, dir); err != nil || !changed {
		t.Fatalf("later edit must remain detectable: %v, %v", changed, err)
	}
}

func TestPublicationCrashChild(t *testing.T) {
	path := os.Getenv("INDEXNUDGE_GO_CRASH_TEST_DB")
	if path == "" {
		return
	}
	writer, err := openWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := writer.conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := withdrawIndex(tx, true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = writer.conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndex(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE symbols SET name='uncommitted'"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("WITHDRAWN")
	io.ReadFull(os.Stdin, make([]byte, 1))
	t.Fatal("crash test child should have been killed")
}

func TestKilledWriterLeavesNoPublicSnapshot(t *testing.T) {
	dir, path := refreshFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestPublicationCrashChild$")
	child.Env = append(os.Environ(), "INDEXNUDGE_GO_CRASH_TEST_DB="+path)
	child.Stderr = os.Stderr
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "WITHDRAWN" {
		t.Fatalf("child exited before withdrawal: %q, %v", line, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("expected killed process")
	}
	reader := refreshReader(t, path)
	assertUnavailable(t, reader)
	writeRefreshSource(t, dir, "package fixture\nfunc NewName() {}\n")
	if err := doFreshen(dir, path); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, reader, "SELECT count(*) FROM symbols WHERE name='NewName'") != 1 {
		t.Fatal("retry did not publish new data")
	}
}
