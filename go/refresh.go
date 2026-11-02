package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type database interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
	Prepare(string) (*sql.Stmt, error)
}

const privatePrefix = "_indexnudge_"
const oldPrefix = "_indexnudge_old_"
const controlSchema = `
CREATE TABLE _indexnudge_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    protocol INTEGER NOT NULL,
    language TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('published', 'unavailable'))
);
CREATE TABLE _indexnudge_objects (
    name TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('table', 'view')),
    definition TEXT
);
INSERT INTO _indexnudge_state VALUES (1, 1, 'go', 'unavailable');
`

type indexWriter struct {
	db   *sql.DB
	conn *sql.Conn
}

type publicationState struct {
	new       bool
	managed   bool
	available bool
	current   bool
}

type schemaObject struct {
	name       string
	kind       string
	definition sql.NullString
}

type refreshOutcome struct {
	rebuilt   bool
	reindexed int
	removed   int
}

func runIndex(dir, dbPath string, incremental bool) error {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolving source directory: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	path, err := databasePath(dbPath)
	if err != nil {
		return err
	}
	if filepath.Ext(path) == ".go" {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if filepath.IsLocal(rel) {
			return fmt.Errorf("a database inside the source tree must not use the .go extension")
		}
	}
	for _, suffix := range []string{".bak", ".tmp"} {
		_, err := os.Lstat(path + suffix)
		if err == nil {
			return fmt.Errorf("legacy artifact %s exists; stop old indexers and resolve it before indexing", path+suffix)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	writer, err := openWriter(path)
	if err != nil {
		return err
	}
	outcome, workErr := runLocked(writer.conn, dir, incremental)
	if err := errors.Join(workErr, writer.close()); err != nil {
		return err
	}
	if outcome.rebuilt {
		fmt.Fprintf(os.Stderr, "Done. Database: %s\n", path)
	} else if outcome.reindexed != 0 || outcome.removed != 0 {
		fmt.Fprintf(os.Stderr, "Freshened: %d reindexed, %d removed.\n", outcome.reindexed, outcome.removed)
	}
	return nil
}

func databasePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		return filepath.EvalSymlinks(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func openWriter(path string) (*indexWriter, error) {
	uri := &url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=exclusive"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Conn.Close must discard the physical connection, not pool its exclusive lock.
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	writer := &indexWriter{db, conn}
	if _, err = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); err == nil {
		var mode string
		err = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
		if err == nil && mode != "delete" {
			err = fmt.Errorf("unsupported journal mode %s; convert to DELETE with all clients stopped", mode)
		}
	}
	if err == nil {
		_, err = conn.ExecContext(context.Background(), "PRAGMA synchronous=EXTRA; PRAGMA locking_mode=EXCLUSIVE")
	}
	if err != nil {
		return nil, errors.Join(err, writer.close())
	}
	return writer, nil
}

func (w *indexWriter) close() error {
	return errors.Join(w.conn.Close(), w.db.Close())
}

func runLocked(conn *sql.Conn, dir string, incremental bool) (outcome refreshOutcome, err error) {
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return outcome, err
	}
	defer func() {
		if tx != nil {
			rollbackErr := tx.Rollback()
			if !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, rollbackErr)
			}
			// A failed Commit can leave a native transaction after ErrTxDone.
			// The caller always physically closes this owned connection.
		}
	}()
	state, err := inspectIndex(tx)
	if err != nil {
		return outcome, err
	}
	rebuild := !incremental || state.new || !state.managed || !state.current
	if state.new {
		if _, err = tx.Exec(controlSchema); err != nil {
			return outcome, err
		}
		if err = createSchema(tx); err != nil {
			return outcome, err
		}
		outcome, err = updateFiles(tx, dir, true)
		if err != nil {
			return outcome, err
		}
		if err = publishIndex(tx); err != nil {
			return outcome, err
		}
		return outcome, tx.Commit()
	}

	if state.available {
		changed, scanErr := true, error(nil)
		if !rebuild {
			changed, scanErr = hasChanges(tx, dir)
		}
		if scanErr == nil && !changed {
			return outcome, tx.Rollback()
		}
		withdrawalErr := withdrawIndex(tx, state.managed)
		if withdrawalErr == nil {
			withdrawalErr = tx.Commit()
		}
		if scanErr != nil {
			if withdrawalErr != nil {
				return outcome, errors.Join(fmt.Errorf("scan failed: %w", scanErr),
					fmt.Errorf("withdrawal failed: %w", withdrawalErr))
			}
			return outcome, fmt.Errorf("scan failed; index withdrawn: %w", scanErr)
		}
		if withdrawalErr != nil {
			return outcome, withdrawalErr
		}
	} else if err = tx.Rollback(); err != nil {
		return outcome, err
	}

	tx, err = conn.BeginTx(context.Background(), nil)
	if err != nil {
		return outcome, err
	}
	if rebuild {
		if err = discardPrivate(tx); err != nil {
			return outcome, err
		}
		err = createSchema(tx)
	} else {
		err = restoreIndex(tx)
	}
	if err != nil {
		return outcome, err
	}
	outcome, err = updateFiles(tx, dir, rebuild)
	if err != nil {
		return outcome, err
	}
	if err = publishIndex(tx); err != nil {
		return outcome, err
	}
	return outcome, tx.Commit()
}

func inspectIndex(db database) (publicationState, error) {
	var state publicationState
	var err error
	state.current, err = schemaIsCurrent(db)
	if err != nil {
		return state, err
	}
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='_indexnudge_state')").Scan(&state.managed); err != nil {
		return state, err
	}
	if state.managed {
		var protocol int
		var language, status string
		if err := db.QueryRow("SELECT protocol, language, status FROM _indexnudge_state WHERE id=1").
			Scan(&protocol, &language, &status); err != nil {
			return state, err
		}
		if protocol != 1 {
			return state, fmt.Errorf("unsupported publication protocol %d", protocol)
		}
		if language != "go" {
			return state, fmt.Errorf("expected a Go index, found %s", language)
		}
		if status != "published" && status != "unavailable" {
			return state, fmt.Errorf("invalid publication state %s", status)
		}
		state.available = status == "published"
		if state.available {
			var pending int
			if err := db.QueryRow("SELECT count(*) FROM _indexnudge_objects").Scan(&pending); err != nil {
				return state, err
			}
			if pending != 0 {
				return state, fmt.Errorf("published index has a pending object manifest")
			}
		}
		return state, nil
	}
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return state, err
	}
	defer rows.Close()
	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return state, err
		}
		if strings.HasPrefix(strings.ToLower(name), privatePrefix) {
			return state, fmt.Errorf("reserved object %s exists without publication metadata", name)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		return state, err
	}
	if err := rows.Close(); err != nil {
		return state, err
	}
	if len(names) == 0 {
		state.new = true
		return state, nil
	}
	if !names["files"] || !names["lines"] || !names["symbols"] {
		return state, fmt.Errorf("database is not a recognized indexnudge index")
	}
	var invalidLanguage bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM files WHERE language IS NULL OR language != 'go')").Scan(&invalidLanguage); err != nil {
		return state, err
	}
	if invalidLanguage {
		return state, fmt.Errorf("database contains non-Go files")
	}
	state.available = true
	return state, nil
}

func identifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func readObjects(db database, query string) ([]schemaObject, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.name, &object.kind, &object.definition); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func withdrawIndex(db database, managed bool) error {
	objects, err := readObjects(db, `
SELECT s.name, s.type, s.sql FROM sqlite_schema s
JOIN pragma_table_list t ON t.schema='main' AND t.name=s.name
WHERE s.type IN ('table','view') AND t.type != 'shadow'
  AND s.name NOT GLOB 'sqlite_*' ORDER BY s.name`)
	if err != nil {
		return err
	}
	public := make([]schemaObject, 0, len(objects))
	for _, object := range objects {
		if object.name == "_indexnudge_state" || object.name == "_indexnudge_objects" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(object.name), privatePrefix) {
			return fmt.Errorf("reserved object %s exists in published index", object.name)
		}
		public = append(public, object)
	}
	if !managed {
		if _, err := db.Exec(controlSchema); err != nil {
			return err
		}
	}
	var pending int
	if err := db.QueryRow("SELECT count(*) FROM _indexnudge_objects").Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return fmt.Errorf("published index has a pending object manifest")
	}
	for _, object := range public {
		if _, err := db.Exec("INSERT INTO _indexnudge_objects (name, kind, definition) VALUES (?, ?, ?)",
			object.name, object.kind, object.definition); err != nil {
			return err
		}
		if object.kind == "view" {
			if _, err := db.Exec("DROP VIEW " + identifier(object.name)); err != nil {
				return err
			}
		}
	}
	for _, object := range public {
		if object.kind == "table" {
			if _, err := db.Exec("ALTER TABLE " + identifier(object.name) + " RENAME TO " + identifier(oldPrefix+object.name)); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec("UPDATE _indexnudge_state SET status='unavailable' WHERE id=1")
	return err
}

func manifest(db database) ([]schemaObject, error) {
	objects, err := readObjects(db, "SELECT name, kind, definition FROM _indexnudge_objects ORDER BY kind, name")
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("unavailable index has no recovery manifest")
	}
	for _, object := range objects {
		if strings.HasPrefix(strings.ToLower(object.name), privatePrefix) ||
			(object.kind != "table" && object.kind != "view") {
			return nil, fmt.Errorf("invalid recovery object %s", object.name)
		}
	}
	return objects, nil
}

func restoreIndex(db database) error {
	objects, err := manifest(db)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if object.kind == "table" {
			_, err = db.Exec("ALTER TABLE " + identifier(oldPrefix+object.name) + " RENAME TO " + identifier(object.name))
		} else if object.definition.Valid {
			_, err = db.Exec(object.definition.String)
		} else {
			return fmt.Errorf("view definition missing for %s", object.name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func discardPrivate(db database) error {
	objects, err := manifest(db)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if object.kind == "table" {
			if _, err := db.Exec("DROP TABLE " + identifier(oldPrefix+object.name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func publishIndex(db database) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return err
	}
	_, err := db.Exec("DELETE FROM _indexnudge_objects; UPDATE _indexnudge_state SET status='published' WHERE id=1")
	return err
}

type fileStamp struct {
	mtime float64
	size  int64
}

type sourceFile struct {
	data  []byte
	stamp fileStamp
}

func statFile(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, fmt.Errorf("stat %s: %w", path, err)
	}
	mtime := float64(info.ModTime().Unix()) + float64(info.ModTime().Nanosecond())/1e9
	return fileStamp{mtime, info.Size()}, nil
}

func readSource(path string) (sourceFile, error) {
	stamp, err := statFile(path)
	if err != nil {
		return sourceFile{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sourceFile{}, fmt.Errorf("reading %s: %w", path, err)
	}
	after, err := statFile(path)
	if err != nil {
		return sourceFile{}, err
	}
	if stamp != after || stamp.size != int64(len(data)) {
		return sourceFile{}, fmt.Errorf("source changed while reading %s", path)
	}
	return sourceFile{data, stamp}, nil
}

func visitGoFiles(dir string, visit func(string) (bool, error)) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	stopped := false
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			stopped, err = visit(path)
			if err != nil {
				return err
			}
			if stopped {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return stopped, err
}

func collectGoFiles(dir string) ([]string, error) {
	var files []string
	_, err := visitGoFiles(dir, func(path string) (bool, error) {
		files = append(files, path)
		return false, nil
	})
	return files, err
}

func indexedFiles(db database) (map[string]fileStamp, error) {
	rows, err := db.Query("SELECT file, indexed_mtime, indexed_size FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make(map[string]fileStamp)
	for rows.Next() {
		var name string
		var stamp fileStamp
		if err := rows.Scan(&name, &stamp.mtime, &stamp.size); err != nil {
			return nil, err
		}
		files[name] = stamp
	}
	return files, rows.Err()
}

func hasChanges(db database, dir string) (bool, error) {
	remaining, err := indexedFiles(db)
	if err != nil {
		return false, err
	}
	changed, err := visitGoFiles(dir, func(path string) (bool, error) {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return false, err
		}
		stamp, err := statFile(path)
		if err != nil {
			return false, err
		}
		previous, exists := remaining[rel]
		delete(remaining, rel)
		return !exists || previous != stamp, nil
	})
	return changed || len(remaining) != 0, err
}

func updateFiles(db database, dir string, rebuild bool) (refreshOutcome, error) {
	outcome := refreshOutcome{rebuilt: rebuild}
	remaining := make(map[string]fileStamp)
	var err error
	if !rebuild {
		remaining, err = indexedFiles(db)
		if err != nil {
			return outcome, err
		}
	}
	files, err := collectGoFiles(dir)
	if err != nil {
		return outcome, err
	}
	if rebuild {
		fmt.Fprintf(os.Stderr, "Indexing %d files...\n", len(files))
	}
	type changedFile struct {
		path, rel string
		existed   bool
	}
	var changed []changedFile
	sources := make(map[string]sourceFile)
	for _, path := range files {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return outcome, err
		}
		stamp, err := statFile(path)
		if err != nil {
			return outcome, err
		}
		previous, exists := remaining[rel]
		delete(remaining, rel)
		if !exists || previous != stamp {
			source, err := readSource(path)
			if err != nil {
				return outcome, err
			}
			sources[path] = source
			changed = append(changed, changedFile{path, rel, exists})
		}
	}
	if len(changed) != 0 {
		fset, pkgFiles, info, err := loadPackages(dir, sources)
		if err != nil {
			return outcome, fmt.Errorf("loading packages: %w", err)
		}
		for _, change := range changed {
			if change.existed {
				if err := deleteFileRows(db, change.rel); err != nil {
					return outcome, err
				}
			}
			source := sources[change.path]
			if file, ok := pkgFiles[change.path]; ok {
				err = indexFile(db, fset, change.rel, source, file, info)
			} else {
				localFset := token.NewFileSet()
				file, parseErr := parser.ParseFile(localFset, change.path, source.data, parser.ParseComments)
				if parseErr != nil {
					fmt.Fprintf(os.Stderr, "parse error %s: %v (indexing lines only)\n", change.rel, parseErr)
					err = indexFileLinesOnly(db, change.rel, source)
				} else {
					err = indexFile(db, localFset, change.rel, source, file, nil)
				}
			}
			if err != nil {
				return outcome, err
			}
		}
	}
	for file := range remaining {
		if err := deleteFileRows(db, file); err != nil {
			return outcome, err
		}
	}
	if rebuild || len(changed) != 0 || len(remaining) != 0 {
		if _, err := db.Exec("DELETE FROM directories"); err != nil {
			return outcome, err
		}
		if err := finalize(db); err != nil {
			return outcome, err
		}
	}
	for path, source := range sources {
		stamp, err := statFile(path)
		if err != nil {
			return outcome, err
		}
		if stamp != source.stamp {
			return outcome, fmt.Errorf("source changed during refresh: %s", path)
		}
	}
	outcome.reindexed, outcome.removed = len(changed), len(remaining)
	return outcome, nil
}
