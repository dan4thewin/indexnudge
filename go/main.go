// indexnudge — index a Go codebase into SQLite for agentic querying
// AI-generated code
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	dir := flag.String("dir", ".", "directory to index")
	output := flag.String("output", "", "output database path (default: <dir>/.indexnudge.db)")
	freshen := flag.Bool("freshen", false, "incremental refresh: only re-index files newer than the DB")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}

	dbPath := *output
	if dbPath == "" {
		dbPath = filepath.Join(absDir, ".indexnudge-go.db")
	}

	if *freshen {
		if err := doFreshen(absDir, dbPath); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := fullIndex(absDir, dbPath); err != nil {
			log.Fatal(err)
		}
	}
}

func fullIndex(dir, dbPath string) error {
	bakPath := dbPath + ".bak"
	tmpPath := dbPath + ".tmp"

	// Move existing DB to .bak for safety
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, bakPath); err != nil {
			return err
		}
	}
	// Remove any stale .tmp
	os.Remove(tmpPath)

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return err
	}

	indexErr := func() error {
		defer db.Close()

		if err := createSchema(db); err != nil {
			return err
		}

		fset, pkgFiles, info, err := loadPackages(dir)
		if err != nil {
			return fmt.Errorf("loading packages: %w", err)
		}

		// Use collectGoFiles as the canonical file list so fullIndex
		// and doFreshen see the same set of files.
		diskFiles := collectGoFiles(dir)
		fmt.Fprintf(os.Stderr, "Indexing %d files...\n", len(diskFiles))

		for _, absPath := range diskFiles {
			rel, _ := filepath.Rel(dir, absPath)
			if f, ok := pkgFiles[absPath]; ok {
				if err := indexFile(db, fset, rel, absPath, f, info); err != nil {
					fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
				}
			} else {
				localFset := token.NewFileSet()
				src, _ := os.ReadFile(absPath)
				parsed, parseErr := parser.ParseFile(localFset, absPath, src, parser.ParseComments)
				if parseErr != nil {
					fmt.Fprintf(os.Stderr, "parse error %s: %v (indexing lines only)\n", rel, parseErr)
					if err := indexFileLinesOnly(db, rel, absPath); err != nil {
						fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
					}
				} else {
					if err := indexFile(db, localFset, rel, absPath, parsed, nil); err != nil {
						fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
					}
				}
			}
		}

		return finalize(db)
	}()

	if indexErr != nil {
		fmt.Fprintf(os.Stderr, "Full index failed: %v. Old database preserved as .bak if it existed.\n", indexErr)
		return indexErr
	}

	if err := os.Rename(tmpPath, dbPath); err != nil {
		return err
	}
	os.Remove(bakPath)
	fmt.Fprintf(os.Stderr, "Done. Database: %s\n", dbPath)
	return nil
}

func doFreshen(dir, dbPath string) error {
	bakPath := dbPath + ".bak"

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		if _, err := os.Stat(bakPath); err == nil {
			// Another freshen is in progress — exit, let shim fall back
			fmt.Fprintln(os.Stderr, "Another freshen in progress, skipping.")
			return nil
		}
		fmt.Fprintln(os.Stderr, "No existing database, doing full index.")
		return fullIndex(dir, dbPath)
	}

	// Rename canonical → .bak
	if err := os.Rename(dbPath, bakPath); err != nil {
		return err
	}

	// Check schema version before attempting incremental freshen.
	{
		probe, err := sql.Open("sqlite", bakPath)
		if err != nil {
			return err
		}
		current := schemaIsCurrent(probe)
		probe.Close()
		if !current {
			if err := os.Rename(bakPath, dbPath); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Schema outdated, doing full reindex.")
			return fullIndex(dir, dbPath)
		}
	}

	freshenErr := func() error {
		db, err := sql.Open("sqlite", bakPath)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.Exec("PRAGMA journal_mode=DELETE; PRAGMA synchronous=OFF; PRAGMA busy_timeout=5000"); err != nil {
			return err
		}
		if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
			return err
		}

		diskFiles := collectGoFiles(dir)
		diskSet := make(map[string]bool)
		for _, f := range diskFiles {
			rel, _ := filepath.Rel(dir, f)
			diskSet[rel] = true
		}

		rows, err := db.Query("SELECT file, indexed_mtime, indexed_size FROM files")
		if err != nil {
			return err
		}
		type dbFile struct {
			file  string
			mtime float64
			size  int64
		}
		var dbFiles []dbFile
		for rows.Next() {
			var f dbFile
			if err := rows.Scan(&f.file, &f.mtime, &f.size); err != nil {
				return err
			}
			dbFiles = append(dbFiles, f)
		}
		rows.Close()

		dbMtimes := make(map[string]float64)
		dbSizes := make(map[string]int64)
		for _, f := range dbFiles {
			dbMtimes[f.file] = f.mtime
			dbSizes[f.file] = f.size
		}

		// Check if anything needs updating
		needsWork := false
		for _, f := range dbFiles {
			if !diskSet[f.file] {
				needsWork = true
				break
			}
		}
		if !needsWork {
			for _, absPath := range diskFiles {
				rel, _ := filepath.Rel(dir, absPath)
				diskMtime := fileMtime(absPath)
				diskSize := fileSize(absPath)
				dbMtime, exists := dbMtimes[rel]
				if !exists || diskMtime > dbMtime || diskSize != dbSizes[rel] {
					needsWork = true
					break
				}
			}
		}

		if !needsWork {
			if _, err := db.Exec("COMMIT"); err != nil {
				return err
			}
			return nil
		}

		// Load type info for accurate channel detection
		fset, pkgFiles, info, err := loadPackages(dir)
		if err != nil {
			return fmt.Errorf("loading packages: %w", err)
		}

		var reindexed, removed int

		// Remove deleted files
		for _, f := range dbFiles {
			if !diskSet[f.file] {
				if err := deleteFileRows(db, f.file); err != nil {
					return err
				}
				removed++
			}
		}

		// Index new or stale files
		for _, absPath := range diskFiles {
			rel, _ := filepath.Rel(dir, absPath)
			diskMtime := fileMtime(absPath)
			diskSize := fileSize(absPath)
			dbMtime, exists := dbMtimes[rel]
			if !exists || diskMtime > dbMtime || diskSize != dbSizes[rel] {
				if exists {
					if err := deleteFileRows(db, rel); err != nil {
						return err
					}
				}
				if astFile, ok := pkgFiles[absPath]; ok {
					if err := indexFile(db, fset, rel, absPath, astFile, info); err != nil {
						fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
						continue
					}
				} else {
					localFset := token.NewFileSet()
					src, _ := os.ReadFile(absPath)
					parsed, parseErr := parser.ParseFile(localFset, absPath, src, parser.ParseComments)
					if parseErr != nil {
						fmt.Fprintf(os.Stderr, "parse error %s: %v (indexing lines only)\n", rel, parseErr)
						if err := indexFileLinesOnly(db, rel, absPath); err != nil {
							fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
							continue
						}
					} else {
						if err := indexFile(db, localFset, rel, absPath, parsed, nil); err != nil {
							fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
							continue
						}
					}
				}
				reindexed++
			}
		}

		if reindexed > 0 || removed > 0 {
			if _, err := db.Exec("DELETE FROM directories"); err != nil {
				return err
			}
			if err := finalize(db); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Freshened: %d reindexed, %d removed.\n", reindexed, removed)
		}

		if _, err := db.Exec("COMMIT"); err != nil {
			return err
		}
		return nil
	}()

	if freshenErr != nil {
		// db.Close() via defer triggers ROLLBACK. .bak is untouched (pre-freshen state).
		// Canonical absent — shims fall back to filesystem.
		fmt.Fprintf(os.Stderr, "Freshen failed: %v. Database preserved as .bak.\n", freshenErr)
		return freshenErr
	}

	// Success — rename .bak back to canonical
	return os.Rename(bakPath, dbPath)
}

// loadPackages uses go/packages to get type information for the whole module.
// Returns the shared FileSet, a map of abs path → parsed AST, and type info.
func loadPackages(dir string) (*token.FileSet, map[string]*ast.File, *types.Info, error) {
	cfg := &packages.Config{
		Mode: packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedName,
		Dir: dir,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, nil, nil, err
	}

	// Merge all type info into one
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	result := make(map[string]*ast.File)
	var fset *token.FileSet

	for _, pkg := range pkgs {
		if fset == nil {
			fset = pkg.Fset
		}
		for k, v := range pkg.TypesInfo.Types {
			info.Types[k] = v
		}
		for k, v := range pkg.TypesInfo.Defs {
			info.Defs[k] = v
		}
		for k, v := range pkg.TypesInfo.Uses {
			info.Uses[k] = v
		}
		for k, v := range pkg.TypesInfo.Selections {
			info.Selections[k] = v
		}
		for i, f := range pkg.Syntax {
			if i < len(pkg.GoFiles) {
				result[pkg.GoFiles[i]] = f
			}
		}
	}
	if fset == nil {
		fset = token.NewFileSet()
	}
	return fset, result, info, nil
}

func collectGoFiles(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == "vendor" || base == ".git" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func fileMtime(path string) float64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(info.ModTime().UnixMilli()) / 1000.0
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// SchemaVersion tracks the on-disk schema layout. Bump whenever tables,
// columns, indexes, or view definitions change so that --freshen can
// detect outdated DBs and fall back to a full reindex.
const SchemaVersion = 1

// schemaIsCurrent returns true if the DB schema matches the indexer.
// freshen consults this and falls back to fullIndex if outdated.
func schemaIsCurrent(db *sql.DB) bool {
	var version int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return false
	}
	return version >= SchemaVersion
}

func createSchema(db *sql.DB) error {
	schema := `
PRAGMA journal_mode=DELETE;
PRAGMA synchronous=OFF;
PRAGMA user_version=1;

CREATE TABLE lines (
    file     TEXT,
    line_num INTEGER,
    content  TEXT,
    symbol   TEXT,
    PRIMARY KEY (file, line_num)
);
CREATE VIRTUAL TABLE lines_fts USING fts5(file, line_num, content, symbol);

CREATE TABLE symbols (
    id             INTEGER PRIMARY KEY,
    name           TEXT,
    qualified_name TEXT,
    kind           TEXT,
    file           TEXT,
    start_line     INTEGER,
    end_line       INTEGER,
    parent         TEXT,
    signature      TEXT,
    cyclomatic     INTEGER,
    cognitive      INTEGER
);

CREATE TABLE calls (
    caller_file   TEXT,
    caller_symbol TEXT,
    callee        TEXT,
    callee_qname  TEXT,
    kind          TEXT,
    line_num      INTEGER
);
CREATE TABLE defs (
    symbol TEXT,
    file   TEXT,
    line_num INTEGER,
    var    TEXT,
    kind   TEXT
);

CREATE TABLE imports (
    file   TEXT,
    module TEXT,
    line_num INTEGER
);

CREATE INDEX idx_symbols_name ON symbols(name);
CREATE INDEX idx_symbols_qualified ON symbols(qualified_name);
CREATE INDEX idx_symbols_file ON symbols(file);
CREATE INDEX idx_symbols_cyclomatic ON symbols(cyclomatic);
CREATE INDEX idx_symbols_cognitive ON symbols(cognitive);
CREATE INDEX idx_calls_callee ON calls(callee);
CREATE INDEX idx_calls_caller ON calls(caller_symbol);
CREATE INDEX idx_defs_var ON defs(var);
CREATE INDEX idx_imports_module ON imports(module);

CREATE TABLE files (
    file           TEXT PRIMARY KEY,
    lines          INTEGER,
    language       TEXT,
    indexed_mtime  REAL,
    indexed_size   INTEGER DEFAULT 0,
    parse_error    INTEGER DEFAULT 0
);

CREATE TABLE directories (
    path        TEXT PRIMARY KEY,
    file_count  INTEGER,
    total_lines INTEGER
);

CREATE TABLE channels (
    id        INTEGER PRIMARY KEY,
    file      TEXT,
    symbol    TEXT,
    line_num  INTEGER,
    operation TEXT,
    var       TEXT
);
CREATE INDEX idx_channels_var ON channels(var);
CREATE INDEX idx_channels_file ON channels(file);

CREATE TABLE blocks (
    file          TEXT,
    open_line     INTEGER,
    close_line    INTEGER,
    kind          TEXT,
    parent_symbol TEXT
);
CREATE INDEX idx_blocks_file ON blocks(file);
CREATE INDEX idx_blocks_open ON blocks(file, open_line);

CREATE TABLE match_arms (
    file        TEXT,
    parent_fn   TEXT,
    pattern     TEXT,
    start_line  INTEGER,
    end_line    INTEGER,
    body_start  INTEGER,
    body_end    INTEGER,
    has_early_return INTEGER DEFAULT 0
);
CREATE INDEX idx_match_arms_file ON match_arms(file);

CREATE TABLE refs (
    file     TEXT,
    line_num INTEGER,
    name     TEXT,
    kind     TEXT
);
CREATE INDEX idx_refs_file_line ON refs(file, line_num);
CREATE INDEX idx_refs_name ON refs(name);

CREATE VIEW callee_candidates AS
SELECT
    c.caller_file,
    c.caller_symbol,
    c.callee,
    c.callee_qname,
    c.line_num       AS call_line,
    c.kind           AS call_kind,
    s.qualified_name AS candidate_qname,
    s.file           AS def_file,
    s.start_line     AS def_line,
    s.parent         AS def_parent,
    s.signature
FROM calls c
LEFT JOIN symbols s
    ON s.name = c.callee
   AND s.kind IN ('function','method')
   AND (c.callee_qname IS NULL OR s.qualified_name = c.callee_qname);

CREATE VIEW string_literals AS
WITH RECURSIVE peeled(file, line_num, symbol, content, value, rest, pos) AS (
    SELECT file, line_num, symbol, content,
           substr(content,
                  instr(content, '"') + 1,
                  instr(substr(content, instr(content, '"') + 1), '"') - 1),
           substr(substr(content, instr(content, '"') + 1),
                  instr(substr(content, instr(content, '"') + 1), '"') + 1),
           1
    FROM lines
    WHERE content LIKE '%"%"%'
      AND trim(content) NOT LIKE '//%'
    UNION ALL
    SELECT file, line_num, symbol, content,
           substr(rest, instr(rest, '"') + 1,
                  instr(substr(rest, instr(rest, '"') + 1), '"') - 1),
           substr(substr(rest, instr(rest, '"') + 1),
                  instr(substr(rest, instr(rest, '"') + 1), '"') + 1),
           pos + 1
    FROM peeled
    WHERE rest LIKE '%"%"%' AND pos < 8
)
SELECT file, line_num, symbol, value, pos,
       CASE
           WHEN content LIKE '%==%"' || value || '"%'
             OR content LIKE '%!=%"' || value || '"%' THEN 'compare'
           WHEN content LIKE '%case %"' || value || '"%' THEN 'match_pat'
           WHEN content LIKE '%fmt.Print%'
             OR content LIKE '%fmt.Sprint%'
             OR content LIKE '%fmt.Errorf%'
             OR content LIKE '%fmt.Fprintf%'
             OR content LIKE '%log.Print%'
             OR content LIKE '%log.Fatal%'            THEN 'fmt'
           ELSE 'arg'
       END AS context
FROM peeled
WHERE value != '';

CREATE VIEW block_dupes AS
WITH ln AS (
    SELECT file, line_num, trim(lower(content)) AS norm,
           ROW_NUMBER() OVER (PARTITION BY file ORDER BY line_num) AS seq
    FROM lines
    WHERE trim(content) != ''
),
sh AS (
    SELECT a.file, a.line_num AS start_line, d.line_num AS end_line,
           a.norm || char(10) || b.norm || char(10) || c.norm || char(10) || d.norm AS shingle
    FROM ln a
    JOIN ln b ON b.file = a.file AND b.seq = a.seq + 1
    JOIN ln c ON c.file = a.file AND c.seq = a.seq + 2
    JOIN ln d ON d.file = a.file AND d.seq = a.seq + 3
)
SELECT shingle,
       COUNT(*) AS occurrences,
       GROUP_CONCAT(file || ':' || start_line, ' | ') AS sites
FROM sh
GROUP BY shingle
HAVING COUNT(*) >= 2;
`
	_, err := db.Exec(schema)
	return err
}

func indexFile(db *sql.DB, fset *token.FileSet, rel, absPath string, f *ast.File, info *types.Info) error {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	content := strings.TrimSuffix(string(src), "\n")
	lines := strings.Split(content, "\n")
	if err := indexLines(db, fset, rel, lines, f); err != nil {
		return err
	}
	if err := extractSymbols(db, fset, rel, f, info); err != nil {
		return err
	}
	if err := extractCalls(db, fset, rel, f, info); err != nil {
		return err
	}
	if err := extractImports(db, fset, rel, f); err != nil {
		return err
	}
	if err := extractDefs(db, fset, rel, f); err != nil {
		return err
	}
	if err := extractChannels(db, fset, rel, f, info); err != nil {
		return err
	}
	if err := extractBlocks(db, fset, rel, f); err != nil {
		return err
	}
	if err := extractRefs(db, fset, rel, f); err != nil {
		return err
	}
	if err := extractMatchArms(db, fset, rel, src, f); err != nil {
		return err
	}

	mtime := fileMtime(absPath)
	size := fileSize(absPath)
	if _, err := db.Exec("INSERT INTO files (file, lines, language, indexed_mtime, indexed_size) VALUES (?, ?, 'go', ?, ?)",
		rel, len(lines), mtime, size); err != nil {
		return err
	}

	return nil
}

func indexFileLinesOnly(db *sql.DB, rel, absPath string) error {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	content := strings.TrimSuffix(string(src), "\n")
	lines := strings.Split(content, "\n")

	stmt, err := db.Prepare("INSERT INTO lines (file, line_num, content, symbol) VALUES (?, ?, ?, '')")
	if err != nil {
		return err
	}
	defer stmt.Close()

	stmtFts, err := db.Prepare("INSERT INTO lines_fts (file, line_num, content, symbol) VALUES (?, ?, ?, '')")
	if err != nil {
		return err
	}
	defer stmtFts.Close()

	for i, line := range lines {
		if _, err := stmt.Exec(rel, i+1, line); err != nil {
			return err
		}
		if _, err := stmtFts.Exec(rel, i+1, line); err != nil {
			return err
		}
	}

	mtime := fileMtime(absPath)
	size := fileSize(absPath)
	if _, err := db.Exec("INSERT INTO files (file, lines, language, indexed_mtime, indexed_size, parse_error) VALUES (?, ?, 'go', ?, ?, 1)",
		rel, len(lines), mtime, size); err != nil {
		return err
	}

	return nil
}
func indexLines(db *sql.DB, fset *token.FileSet, rel string, lines []string, f *ast.File) error {
	// Build line→symbol map
	symMap := make(map[int]string)
	ast.Inspect(f, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.FuncDecl:
			name := funcDeclName(decl)
			start := fset.Position(decl.Pos()).Line
			end := fset.Position(decl.End()).Line
			for i := start; i <= end; i++ {
				symMap[i] = name
			}
		}
		return true
	})

	stmt, err := db.Prepare("INSERT INTO lines (file, line_num, content, symbol) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	stmtFts, err := db.Prepare("INSERT INTO lines_fts (file, line_num, content, symbol) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmtFts.Close()

	for i, line := range lines {
		lineNum := i + 1
		sym, ok := symMap[lineNum]
		if !ok {
			sym = ""
		}
		if _, err := stmt.Exec(rel, lineNum, line, sym); err != nil {
			return err
		}
		if _, err := stmtFts.Exec(rel, lineNum, line, sym); err != nil {
			return err
		}
	}
	return nil
}

func extractSymbols(db *sql.DB, fset *token.FileSet, rel string, f *ast.File, info *types.Info) error {
	stmt, err := db.Prepare(
		"INSERT INTO symbols (name, qualified_name, kind, file, start_line, end_line, parent, signature, cyclomatic, cognitive) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			kind := "function"
			var parent, qname, sig *string

			if d.Recv != nil {
				kind = "method"
				recvType := recvTypeName(d.Recv)
				if recvType != "" {
					p := recvType
					parent = &p
				}
				// Use types.Info for qualified name if available
				if info != nil {
					if obj, ok := info.Defs[d.Name]; ok && obj != nil {
						if fn, ok := obj.(*types.Func); ok {
							q := fn.Type().(*types.Signature).Recv().Type().String() + "." + name
							qname = &q
						}
					}
				}
				if qname == nil && parent != nil {
					q := *parent + "." + name
					qname = &q
				}
			} else {
				if info != nil {
					if obj, ok := info.Defs[d.Name]; ok && obj != nil {
						if pkg := obj.Pkg(); pkg != nil {
							q := pkg.Path() + "." + name
							qname = &q
						}
					}
				}
			}

			s := funcSignature(d)
			if s != "" {
				sig = &s
			}
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			cyclo := cyclomaticComplexity(d.Body)
			cog := cognitiveComplexity(d.Body)
			if _, err := stmt.Exec(name, qname, kind, rel, start, end, parent, sig, cyclo, cog); err != nil {
				return err
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					kind := "type"
					switch s.Type.(type) {
					case *ast.StructType:
						kind = "struct"
					case *ast.InterfaceType:
						kind = "interface"
					}
					start := fset.Position(s.Pos()).Line
					end := fset.Position(s.End()).Line
					if _, err := stmt.Exec(s.Name.Name, nil, kind, rel, start, end, nil, nil, nil, nil); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// cyclomaticComplexity computes McCabe complexity over a function body
// using Mozilla rust-code-analysis-compatible semantics (per-function
// cyclomatic.sum, including nested function-literal CCs as if merged into
// the parent space). Rules:
//
//   +1 base per function/method
//   +1 per IfStmt, ForStmt, RangeStmt, CaseClause (switch), CommClause (select)
//   +1 per && / ||
//   +1 per FuncLit (closure) — and recurse INTO the FuncLit body so its
//     own decisions accumulate, matching how Mozilla merges child-space sums.
//
// Not counted: TypeSwitchStmt itself (each case clause already counted),
// matches!-style macros (Go has none), or-pattern equivalents (case lists
// like `case 1, 2, 3:` count as ONE CaseClause, not three).
func cyclomaticComplexity(body *ast.BlockStmt) *int {
	if body == nil {
		return nil
	}
	count := 1
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			count++       // closure contributes its base +1 to enclosing fn
			return true   // recurse — closure decisions also count (Mozilla .sum semantics)
		case *ast.IfStmt:
			count++
		case *ast.CaseClause:
			count++
		case *ast.CommClause:
			count++
		case *ast.ForStmt:
			count++
		case *ast.RangeStmt:
			count++
		case *ast.TypeSwitchStmt:
			_ = x // case clauses contribute via CaseClause; switch itself adds nothing
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				count++
			}
		}
		return true
	})
	return &count
}

// cogState carries the running cognitive complexity counters during a walk.
// `nesting` is the local nesting depth that increments on each structural
// nesting construct (if/for/range/switch/select). `lambda` increments on
// entry to a function literal (closure). `depth` mirrors Mozilla's
// nested-function-depth bonus; Go has no nested fn declarations so this
// stays 0 in practice, but the field is kept for cross-language parity
// with the Rust impl. `prevBool` tracks the previous boolean operator
// ("&&", "||", "NOT", or "") for Sonar's BoolSequence rule.
type cogState struct {
	nesting  int
	depth    int
	lambda   int
	prevBool string
	count    int
}

// cognitiveComplexity computes Sonar Cognitive Complexity (Campbell 2018)
// over a function body, calibrated to match the Rust indexer's
// count_cognitive_complexity (which in turn matches Mozilla
// rust-code-analysis's `cognitive.sum`). Cross-language node mapping:
//
//   *ast.IfStmt              +1 + nesting; else-if and else add +1 flat
//   *ast.ForStmt             +1 + nesting
//   *ast.RangeStmt           +1 + nesting
//   *ast.SwitchStmt          +1 + nesting (the switch itself; arms add 0)
//   *ast.TypeSwitchStmt      +1 + nesting
//   *ast.SelectStmt          +1 + nesting
//   *ast.CaseClause          0
//   *ast.CommClause          0
//   *ast.BranchStmt (labeled break/continue/goto)  +1
//   *ast.BinaryExpr (&&/||)  BoolSequence: +1 first, +1 on operator switch
//   *ast.UnaryExpr  (!)      sets BoolSequence marker to NOT
//   *ast.FuncLit             lambda++ inside; closure decisions add to enclosing fn
//
// Not counted (matching Sonar/Mozilla): early `return`, `defer`, `go`,
// `?`-style error propagation (Go has none), case-list alternatives
// (`case 1, 2, 3:` is one case, not three).
func cognitiveComplexity(body *ast.BlockStmt) *int {
	if body == nil {
		return nil
	}
	state := &cogState{}
	walkCognitive(body, state)
	return &state.count
}

// walkCognitive processes a single AST node. It mutates state.count
// directly. For nesting constructs, it bumps state.nesting before
// descending and restores afterwards. For non-special nodes it
// recurses into immediate children via walkCognitiveChildren so the
// walk continues without changing state.
func walkCognitive(node ast.Node, state *cogState) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *ast.IfStmt:
		state.count += state.nesting + state.lambda + state.depth + 1
		savedPrev := state.prevBool
		state.prevBool = ""
		state.nesting++

		if n.Init != nil {
			walkCognitive(n.Init, state)
		}
		if n.Cond != nil {
			walkCognitive(n.Cond, state)
		}
		walkCognitive(n.Body, state)

		// Walk the else/else-if chain while nesting is still bumped, so
		// any decisions inside the chained branches see the same nesting
		// as the if-body branch. Each else (whether plain or "else if")
		// adds +1 flat — no nesting bonus, no nesting bump.
		elseNode := n.Else
		for elseNode != nil {
			state.count++
			switch e := elseNode.(type) {
			case *ast.IfStmt:
				// "else if": walk this IfStmt's parts WITHOUT triggering the
				// IfStmt case (which would re-add +1+nesting).
				if e.Init != nil {
					walkCognitive(e.Init, state)
				}
				if e.Cond != nil {
					walkCognitive(e.Cond, state)
				}
				walkCognitive(e.Body, state)
				elseNode = e.Else
			default:
				walkCognitive(elseNode, state)
				elseNode = nil
			}
		}

		state.nesting--
		state.prevBool = savedPrev

	case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		state.count += state.nesting + state.lambda + state.depth + 1
		savedPrev := state.prevBool
		state.prevBool = ""
		state.nesting++
		walkCognitiveChildren(n, state)
		state.nesting--
		state.prevBool = savedPrev

	case *ast.BranchStmt:
		// break / continue / goto / fallthrough — only +1 if labeled.
		if n.Label != nil {
			state.count++
		}

	case *ast.BinaryExpr:
		if n.Op == token.LAND || n.Op == token.LOR {
			marker := "&&"
			if n.Op == token.LOR {
				marker = "||"
			}
			if state.prevBool == "" || state.prevBool != marker {
				state.count++
			}
			state.prevBool = marker
		}
		walkCognitiveChildren(n, state)

	case *ast.UnaryExpr:
		if n.Op == token.NOT {
			state.prevBool = "NOT"
		}
		walkCognitiveChildren(n, state)

	case *ast.FuncLit:
		savedLambda := state.lambda
		savedPrev := state.prevBool
		state.lambda++
		state.prevBool = ""
		walkCognitiveChildren(n, state)
		state.lambda = savedLambda
		state.prevBool = savedPrev

	default:
		walkCognitiveChildren(node, state)
	}
}

// walkCognitiveChildren visits each IMMEDIATE child of node by delegating
// to walkCognitive. It uses ast.Walk with a visitor that recognises the
// root via pointer identity and recurses one level only.
func walkCognitiveChildren(node ast.Node, state *cogState) {
	ast.Walk(&cogChildWalker{state: state, root: node}, node)
}

type cogChildWalker struct {
	state *cogState
	root  ast.Node
}

func (w *cogChildWalker) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	if n == w.root {
		return w
	}
	walkCognitive(n, w.state)
	return nil
}

func extractCalls(db *sql.DB, fset *token.FileSet, rel string, f *ast.File, info *types.Info) error {
	stmt, err := db.Prepare(
		"INSERT INTO calls (caller_file, caller_symbol, callee, callee_qname, kind, line_num) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		callerSym := funcDeclName(fn)

		var execErr error
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if execErr != nil {
				return false
			}
			switch expr := n.(type) {
			case *ast.CallExpr:
				line := fset.Position(expr.Pos()).Line
				switch fun := expr.Fun.(type) {
			case *ast.Ident:
					qname := ""
					if info != nil {
						if obj, ok := info.Uses[fun]; ok && obj != nil {
							if pkg := obj.Pkg(); pkg != nil {
								qname = pkg.Path() + "." + obj.Name()
							} else {
								qname = obj.Name()
							}
						}
					}
					_, execErr = stmt.Exec(rel, callerSym, fun.Name, qname, "function", line)
			case *ast.SelectorExpr:
					callee := selectorString(fun)
					qname := ""
					if info != nil {
						if sel, ok := info.Selections[fun]; ok {
							recv := sel.Recv().String()
							qname = recv + "." + fun.Sel.Name
						} else if obj, ok := info.Uses[fun.Sel]; ok && obj != nil {
							if pkg := obj.Pkg(); pkg != nil {
								qname = pkg.Path() + "." + obj.Name()
							}
						}
					}
					_, execErr = stmt.Exec(rel, callerSym, callee, qname, "method", line)
				}
			}
			return true
		})
		if execErr != nil {
			return execErr
		}
	}
	return nil
}

func extractImports(db *sql.DB, fset *token.FileSet, rel string, f *ast.File) error {
	stmt, err := db.Prepare("INSERT INTO imports (file, module, line_num) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if _, err := stmt.Exec(rel, path, fset.Position(imp.Pos()).Line); err != nil {
			return err
		}
	}
	return nil
}

func extractDefs(db *sql.DB, fset *token.FileSet, rel string, f *ast.File) error {
	stmt, err := db.Prepare(
		"INSERT INTO defs (symbol, file, line_num, var, kind) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		callerSym := funcDeclName(fn)

		var execErr error
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if execErr != nil {
				return false
			}
			switch node := n.(type) {
			case *ast.AssignStmt:
				kind := "assign"
				if node.Tok.String() == ":=" {
					kind = "short_var"
				}
				for _, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						line := fset.Position(ident.Pos()).Line
						_, execErr = stmt.Exec(callerSym, rel, line, ident.Name, kind)
					}
				}
			case *ast.RangeStmt:
				if node.Key != nil {
					if ident, ok := node.Key.(*ast.Ident); ok {
						line := fset.Position(ident.Pos()).Line
						_, execErr = stmt.Exec(callerSym, rel, line, ident.Name, "range_key")
					}
				}
				if node.Value != nil {
					if ident, ok := node.Value.(*ast.Ident); ok {
						line := fset.Position(ident.Pos()).Line
						_, execErr = stmt.Exec(callerSym, rel, line, ident.Name, "range_value")
					}
				}
			}
			return true
		})
		if execErr != nil {
			return execErr
		}
	}

	// Package-level vars and consts
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			kind := "var"
			if gd.Tok.String() == "const" {
				kind = "const"
			}
			for _, name := range vs.Names {
				line := fset.Position(name.Pos()).Line
				if _, err := stmt.Exec(nil, rel, line, name.Name, kind); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func extractChannels(db *sql.DB, fset *token.FileSet, rel string, f *ast.File, info *types.Info) error {
	stmt, err := db.Prepare(
		"INSERT INTO channels (file, symbol, line_num, operation, var) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		sym := funcDeclName(fn)

		var execErr error
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if execErr != nil {
				return false
			}
			switch node := n.(type) {
			case *ast.SendStmt:
				line := fset.Position(node.Pos()).Line
				varName := exprName(node.Chan)
				_, execErr = stmt.Exec(rel, sym, line, "send", varName)

			case *ast.UnaryExpr:
				if node.Op == token.ARROW {
					line := fset.Position(node.Pos()).Line
					varName := exprName(node.X)
					_, execErr = stmt.Exec(rel, sym, line, "recv", varName)
				}

			case *ast.CallExpr:
				if ident, ok := node.Fun.(*ast.Ident); ok {
					switch ident.Name {
					case "make":
						if len(node.Args) > 0 {
							if _, ok := node.Args[0].(*ast.ChanType); ok {
								line := fset.Position(node.Pos()).Line
								_, execErr = stmt.Exec(rel, sym, line, "make", nil)
							}
						}
					case "close":
						if len(node.Args) == 1 {
							line := fset.Position(node.Pos()).Line
							varName := exprName(node.Args[0])
							_, execErr = stmt.Exec(rel, sym, line, "close", varName)
						}
					}
				}

			case *ast.RangeStmt:
				if node.X != nil && info != nil {
					if tv, ok := info.Types[node.X]; ok {
						if _, isChan := tv.Type.Underlying().(*types.Chan); isChan {
							line := fset.Position(node.Pos()).Line
							varName := exprName(node.X)
							_, execErr = stmt.Exec(rel, sym, line, "range", varName)
						}
					}
				}

			case *ast.SelectStmt:
				line := fset.Position(node.Pos()).Line
				_, execErr = stmt.Exec(rel, sym, line, "select", nil)
			}
			return true
		})
		if execErr != nil {
			return execErr
		}
	}
	return nil
}

// extractBlocks walks the file emitting one row per multi-line *ast.BlockStmt.
// Kind is derived from the block's syntactic parent (fn body, if-body,
// else-body, for-body, switch-body, select-body, closure-body, or bare block).
// parent_symbol is the qualified name of the enclosing top-level FuncDecl.
func extractBlocks(db *sql.DB, fset *token.FileSet, rel string, f *ast.File) error {
	stmt, err := db.Prepare(
		"INSERT INTO blocks (file, open_line, close_line, kind, parent_symbol) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	v := &blockExtractor{stmt: stmt, fset: fset, rel: rel}
	ast.Walk(v, f)
	return v.err
}

type blockExtractor struct {
	stmt    *sql.Stmt
	fset    *token.FileSet
	rel     string
	parents []ast.Node
	fnStack []string
	err     error
}

func (e *blockExtractor) Visit(n ast.Node) ast.Visitor {
	if e.err != nil {
		return nil
	}
	if n == nil {
		// post-order: pop stack
		if len(e.parents) > 0 {
			top := e.parents[len(e.parents)-1]
			e.parents = e.parents[:len(e.parents)-1]
			if _, isFn := top.(*ast.FuncDecl); isFn && len(e.fnStack) > 0 {
				e.fnStack = e.fnStack[:len(e.fnStack)-1]
			}
		}
		return nil
	}

	if block, ok := n.(*ast.BlockStmt); ok {
		openLine := e.fset.Position(block.Lbrace).Line
		closeLine := e.fset.Position(block.Rbrace).Line
		if closeLine > openLine {
			kind := "block"
			if len(e.parents) > 0 {
				kind = blockKindFromParent(e.parents[len(e.parents)-1], block)
			}
			var parentSym any
			if len(e.fnStack) > 0 {
				parentSym = e.fnStack[len(e.fnStack)-1]
			}
			if _, err := e.stmt.Exec(e.rel, openLine, closeLine, kind, parentSym); err != nil {
				e.err = err
				return nil
			}
		}
	}

	if fd, ok := n.(*ast.FuncDecl); ok {
		e.fnStack = append(e.fnStack, funcDeclName(fd))
	}
	e.parents = append(e.parents, n)
	return e
}

func blockKindFromParent(parent ast.Node, block *ast.BlockStmt) string {
	switch p := parent.(type) {
	case *ast.FuncDecl:
		return "fn"
	case *ast.FuncLit:
		return "closure"
	case *ast.IfStmt:
		if p.Else == block {
			return "else"
		}
		return "if"
	case *ast.ForStmt:
		return "for"
	case *ast.RangeStmt:
		return "for"
	case *ast.SwitchStmt:
		return "switch"
	case *ast.TypeSwitchStmt:
		return "switch"
	case *ast.SelectStmt:
		return "select"
	}
	return "block"
}

// extractRefs records every identifier reference with a kind classification
// (use/def/type/call/field) derived from the syntactic parent. Mirrors the
// Rust indexer's refs table for cross-language query parity.
func extractRefs(db *sql.DB, fset *token.FileSet, rel string, f *ast.File) error {
	stmt, err := db.Prepare(
		"INSERT INTO refs (file, line_num, name, kind) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	v := &refExtractor{stmt: stmt, fset: fset, rel: rel}
	ast.Walk(v, f)
	return v.err
}

type refExtractor struct {
	stmt    *sql.Stmt
	fset    *token.FileSet
	rel     string
	parents []ast.Node
	err     error
}

func (e *refExtractor) Visit(n ast.Node) ast.Visitor {
	if e.err != nil {
		return nil
	}
	if n == nil {
		if len(e.parents) > 0 {
			e.parents = e.parents[:len(e.parents)-1]
		}
		return nil
	}

	if id, ok := n.(*ast.Ident); ok {
		if id.Name != "" && id.Name != "_" {
			kind := "use"
			if len(e.parents) > 0 {
				kind = refKindFromParent(id, e.parents[len(e.parents)-1])
			}
			line := e.fset.Position(id.Pos()).Line
			if _, err := e.stmt.Exec(e.rel, line, id.Name, kind); err != nil {
				e.err = err
				return nil
			}
		}
	}

	e.parents = append(e.parents, n)
	return e
}

func refKindFromParent(id *ast.Ident, parent ast.Node) string {
	switch p := parent.(type) {
	case *ast.CallExpr:
		if p.Fun == id {
			return "call"
		}
		return "use"
	case *ast.SelectorExpr:
		if p.Sel == id {
			return "field"
		}
		return "use"
	case *ast.AssignStmt:
		if p.Tok == token.DEFINE {
			for _, lhs := range p.Lhs {
				if lhs == id {
					return "def"
				}
			}
		}
		return "use"
	case *ast.ValueSpec:
		for _, name := range p.Names {
			if name == id {
				return "def"
			}
		}
		return "type"
	case *ast.Field:
		for _, name := range p.Names {
			if name == id {
				return "def"
			}
		}
		return "type"
	case *ast.FuncDecl:
		if p.Name == id {
			return "def"
		}
		return "use"
	case *ast.TypeSpec:
		if p.Name == id {
			return "def"
		}
		return "type"
	case *ast.LabeledStmt:
		if p.Label == id {
			return "def"
		}
		return "use"
	case *ast.ImportSpec, *ast.BranchStmt:
		return "use"
	case *ast.StarExpr, *ast.ArrayType, *ast.MapType, *ast.ChanType,
		*ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return "type"
	case *ast.CompositeLit:
		if p.Type == id {
			return "type"
		}
		return "use"
	}
	return "use"
}

// extractMatchArms walks switch/type-switch/select statements emitting one
// row per case/comm clause. Mirrors the Rust indexer's match_arms table
// (chosen over a Go-idiomatic name like "case_clauses" for cross-language
// query parity — a downstream tool can `SELECT * FROM match_arms` against
// either DB without language-specific branching).
//
// has_early_return is true when the case body contains a top-level
// *ast.ReturnStmt (not inside a closure), matching the Rust semantics.
func extractMatchArms(db *sql.DB, fset *token.FileSet, rel string, src []byte, f *ast.File) error {
	stmt, err := db.Prepare(
		"INSERT INTO match_arms (file, parent_fn, pattern, start_line, end_line, body_start, body_end, has_early_return) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	v := &matchArmExtractor{stmt: stmt, fset: fset, rel: rel, src: src}
	ast.Walk(v, f)
	return v.err
}

type matchArmExtractor struct {
	stmt    *sql.Stmt
	fset    *token.FileSet
	rel     string
	src     []byte
	fnStack []string
	err     error
	popKind []bool // true at indexes where we pushed an fn scope
}

func (e *matchArmExtractor) Visit(n ast.Node) ast.Visitor {
	if e.err != nil {
		return nil
	}
	if n == nil {
		if len(e.popKind) > 0 {
			if e.popKind[len(e.popKind)-1] {
				e.fnStack = e.fnStack[:len(e.fnStack)-1]
			}
			e.popKind = e.popKind[:len(e.popKind)-1]
		}
		return nil
	}

	pushed := false
	if fd, ok := n.(*ast.FuncDecl); ok {
		e.fnStack = append(e.fnStack, funcDeclName(fd))
		pushed = true
	}
	e.popKind = append(e.popKind, pushed)

	switch c := n.(type) {
	case *ast.CaseClause:
		e.emit(c.List, c.Body, c.Case, c.Colon, c.End())
	case *ast.CommClause:
		var listExprs []ast.Node
		if c.Comm != nil {
			listExprs = []ast.Node{c.Comm}
		}
		e.emitNodes(listExprs, c.Body, c.Case, c.Colon, c.End())
	}
	return e
}

// emit records a CaseClause row. patternExprs is the case expression list
// (nil for `default:`). bodyStmts is the body slice. casePos is the `case`
// keyword token; colonPos is the `:`; endPos is the clause end.
func (e *matchArmExtractor) emit(patternExprs []ast.Expr, bodyStmts []ast.Stmt, casePos, colonPos, endPos token.Pos) {
	nodes := make([]ast.Node, len(patternExprs))
	for i, x := range patternExprs {
		nodes[i] = x
	}
	e.emitNodes(nodes, bodyStmts, casePos, colonPos, endPos)
}

func (e *matchArmExtractor) emitNodes(patternNodes []ast.Node, bodyStmts []ast.Stmt, casePos, colonPos, endPos token.Pos) {
	startLine := e.fset.Position(casePos).Line
	endLine := e.fset.Position(endPos).Line
	if endLine < startLine {
		endLine = startLine
	}

	pattern := "default"
	if len(patternNodes) > 0 {
		// Render via raw source bytes between the first pattern node's
		// start and the colon. This handles arbitrary expressions cleanly.
		patStart := e.fset.Position(patternNodes[0].Pos()).Offset
		patEnd := e.fset.Position(colonPos).Offset
		if patStart >= 0 && patEnd > patStart && patEnd <= len(e.src) {
			pattern = strings.TrimSpace(string(e.src[patStart:patEnd]))
		}
	}

	bodyStart := e.fset.Position(colonPos).Line + 1
	bodyEnd := endLine
	if len(bodyStmts) > 0 {
		bodyStart = e.fset.Position(bodyStmts[0].Pos()).Line
		bodyEnd = e.fset.Position(bodyStmts[len(bodyStmts)-1].End()).Line
	}

	hasEarlyReturn := 0
	for _, s := range bodyStmts {
		if bodyContainsReturnOutsideClosure(s) {
			hasEarlyReturn = 1
			break
		}
	}

	var parentFn any
	if len(e.fnStack) > 0 {
		parentFn = e.fnStack[len(e.fnStack)-1]
	}

	if _, err := e.stmt.Exec(e.rel, parentFn, pattern, startLine, endLine, bodyStart, bodyEnd, hasEarlyReturn); err != nil {
		e.err = err
	}
}

// bodyContainsReturnOutsideClosure returns true if `n` contains an
// *ast.ReturnStmt that isn't nested inside a *ast.FuncLit.
func bodyContainsReturnOutsideClosure(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		switch node.(type) {
		case *ast.FuncLit:
			return false // don't recurse into closures
		case *ast.ReturnStmt:
			found = true
			return false
		}
		return true
	})
	return found
}

// Helpers

func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		recvType := recvTypeName(fn.Recv)
		if recvType != "" {
			return recvType + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

func recvTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	// Strip pointer
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if ident, ok := t.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func funcSignature(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fn.Recv != nil {
		recvType := recvTypeName(fn.Recv)
		if recvType != "" {
			b.WriteString("(" + recvType + ") ")
		}
	}
	b.WriteString(fn.Name.Name + "(")
	if fn.Type.Params != nil {
		var params []string
		for _, p := range fn.Type.Params.List {
			typeName := exprName(p.Type)
			for _, name := range p.Names {
				params = append(params, name.Name+" "+typeName)
			}
			if len(p.Names) == 0 {
				params = append(params, typeName)
			}
		}
		b.WriteString(strings.Join(params, ", "))
	}
	b.WriteString(")")
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		var results []string
		for _, r := range fn.Type.Results.List {
			results = append(results, exprName(r.Type))
		}
		if len(results) == 1 {
			b.WriteString(" " + results[0])
		} else {
			b.WriteString(" (" + strings.Join(results, ", ") + ")")
		}
	}
	return b.String()
}

func selectorString(sel *ast.SelectorExpr) string {
	if ident, ok := sel.X.(*ast.Ident); ok {
		return ident.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}

func exprName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return selectorString(e)
	case *ast.StarExpr:
		return "*" + exprName(e.X)
	case *ast.ArrayType:
		return "[]" + exprName(e.Elt)
	case *ast.MapType:
		return "map[" + exprName(e.Key) + "]" + exprName(e.Value)
	case *ast.ChanType:
		return "chan " + exprName(e.Value)
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		if e.Elt != nil {
			return "..." + exprName(e.Elt)
		}
		return "..."
	}
	return ""
}

func deleteFileRows(db *sql.DB, file string) error {
	for _, table := range []string{"lines", "lines_fts", "symbols", "calls", "imports", "defs", "files", "channels", "blocks", "match_arms", "refs"} {
		col := "file"
		if table == "calls" {
			col = "caller_file"
		}
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s = ?", table, col), file); err != nil {
			return err
		}
	}
	return nil
}

func finalize(db *sql.DB) error {
	return populateDirectories(db)
}

func populateDirectories(db *sql.DB) error {
	rows, err := db.Query("SELECT file, lines FROM files")
	if err != nil {
		return err
	}
	defer rows.Close()

	dirs := make(map[string][2]int64) // [file_count, total_lines]
	for rows.Next() {
		var file string
		var lines int64
		if err := rows.Scan(&file, &lines); err != nil {
			return err
		}
		dir := filepath.Dir(file)
		for {
			if dir == "" {
				dir = "."
			}
			entry := dirs[dir]
			entry[0]++
			entry[1] += lines
			dirs[dir] = entry
			if dir == "." {
				break
			}
			dir = filepath.Dir(dir)
		}
	}

	stmt, err := db.Prepare("INSERT INTO directories (path, file_count, total_lines) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for path, counts := range dirs {
		if _, err := stmt.Exec(path, counts[0], counts[1]); err != nil {
			return err
		}
	}
	return nil
}
