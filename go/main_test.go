package main

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const sample = `package example

import (
	"fmt"
	"sync"
)

type Point struct {
	X float64
	Y float64
}

func (p *Point) Distance(other *Point) float64 {
	dx := p.X - other.X
	dy := p.Y - other.Y
	return dx*dx + dy*dy
}

func Add(a, b int) int {
	return a + b
}

func main() {
	p := &Point{X: 1.0, Y: 2.0}
	q := &Point{X: 4.0, Y: 6.0}
	d := p.Distance(q)
	fmt.Println("distance:", d)
	sum := Add(3, 4)
	fmt.Println("sum:", sum)
}
`

const sampleWithChannels = `package example

import "fmt"

func producer(ch chan<- int, done chan struct{}) {
	for i := 0; i < 10; i++ {
		ch <- i
	}
	close(ch)
	close(done)
}

func consumer(ch <-chan int) {
	for v := range ch {
		fmt.Println(v)
	}
}

func dispatcher(in <-chan int, out chan<- int) {
	select {
	case v := <-in:
		out <- v
	}
}

func main() {
	ch := make(chan int)
	done := make(chan struct{})
	go producer(ch, done)
	go consumer(ch)
	<-done
}
`

// sampleNoChannels has range over slice/map — should produce zero channel rows
const sampleNoChannels = `package example

func loops() {
	nums := []int{1, 2, 3}
	for _, n := range nums {
		_ = n
	}
	m := map[string]int{"a": 1}
	for k, v := range m {
		_ = k
		_ = v
	}
}
`

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := createSchema(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func indexSample(t *testing.T, db *sql.DB, src, filename string) {
	t.Helper()
	dir := t.TempDir()
	// loadPackages needs a Go module
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	fset, pkgFiles, info, err := loadPackages(dir)
	if err != nil {
		t.Fatalf("loadPackages: %v", err)
	}
	f, ok := pkgFiles[path]
	if !ok {
		// Try matching by filename suffix — abs paths may differ
		for k, v := range pkgFiles {
			if filepath.Base(k) == filename {
				f = v
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("file %s not found in loaded packages (have %v)", filename, pkgFileKeys(pkgFiles))
		}
	}
	if err := indexFile(db, fset, filename, path, f, info); err != nil {
		t.Fatal(err)
	}
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		result = append(result, s)
	}
	return result
}

func queryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

func pkgFileKeys(m map[string]*ast.File) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestSymbolsExtracted(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	names := queryStrings(t, db, "SELECT name FROM symbols ORDER BY start_line")
	if !contains(names, "Point") {
		t.Error("missing struct Point")
	}
	if !contains(names, "Distance") {
		t.Error("missing method Distance")
	}
	if !contains(names, "Add") {
		t.Error("missing function Add")
	}
	if !contains(names, "main") {
		t.Error("missing function main")
	}

	// Check kinds
	kinds := queryStrings(t, db, "SELECT kind FROM symbols WHERE name = 'Point'")
	if !contains(kinds, "struct") {
		t.Errorf("Point should be struct, got %v", kinds)
	}
	kinds = queryStrings(t, db, "SELECT kind FROM symbols WHERE name = 'Distance'")
	if !contains(kinds, "method") {
		t.Errorf("Distance should be method, got %v", kinds)
	}
	kinds = queryStrings(t, db, "SELECT kind FROM symbols WHERE name = 'Add'")
	if !contains(kinds, "function") {
		t.Errorf("Add should be function, got %v", kinds)
	}
}

func TestQualifiedNames(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	qnames := queryStrings(t, db, "SELECT qualified_name FROM symbols WHERE name = 'Distance' AND qualified_name IS NOT NULL")
	if len(qnames) == 0 {
		t.Fatal("Distance should have a qualified name")
	}
	if qnames[0] != "*testmod.Point.Distance" {
		t.Errorf("expected *testmod.Point.Distance, got %s", qnames[0])
	}
}

func TestImportsExtracted(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	modules := queryStrings(t, db, "SELECT module FROM imports WHERE file = 'example.go' ORDER BY module")
	if !contains(modules, "fmt") {
		t.Error("missing import fmt")
	}
	if !contains(modules, "sync") {
		t.Error("missing import sync")
	}

	// Verify line numbers are actual source lines, not token.Pos offsets
	lines := queryStrings(t, db, "SELECT CAST(line_num AS TEXT) FROM imports WHERE file = 'example.go' AND module = 'fmt'")
	if len(lines) == 0 {
		t.Fatal("no line number for fmt import")
	}
	lineNum, _ := strconv.Atoi(lines[0])
	if lineNum < 1 || lineNum > 10 {
		t.Errorf("fmt import line should be in first 10 lines, got %d (token.Pos leak?)", lineNum)
	}
}

func TestCallsExtracted(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	callees := queryStrings(t, db, "SELECT DISTINCT callee FROM calls ORDER BY callee")
	if !contains(callees, "Add") {
		t.Error("missing call to Add")
	}
	if !contains(callees, "p.Distance") || !contains(callees, "fmt.Println") {
		// method calls show as receiver.method
		t.Errorf("expected method calls, got %v", callees)
	}
}


func TestCalleeQnamePopulated(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	// callee_qname should be non-empty for at least some calls
	total := queryInt(t, db, "SELECT count(*) FROM calls WHERE caller_file = 'example.go'")
	if total < 1 {
		t.Fatal("no calls found")
	}
	withQname := queryInt(t, db, "SELECT count(*) FROM calls WHERE callee_qname != '' AND callee_qname IS NOT NULL")
	if withQname < 1 {
		t.Errorf("no calls have callee_qname populated, total calls = %d", total)
	}

	// callee_qname for fmt.Println should contain "fmt"
	qnames := queryStrings(t, db, "SELECT callee_qname FROM calls WHERE callee = 'fmt.Println'")
	if len(qnames) == 0 {
		t.Error("no qname for fmt.Println")
	} else if !strings.Contains(qnames[0], "fmt") {
		t.Errorf("fmt.Println qname = %q, expected to contain 'fmt'", qnames[0])
	}
}

func TestCalleeQnameJoinableToSymbols(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	// Any call whose callee matches a local symbol name should have
	// callee_qname that's joinable (i.e., the qualified_name exists in symbols)
	joinCount := queryInt(t, db, `
		SELECT count(*) FROM calls c
		JOIN symbols s ON c.callee_qname = s.qualified_name
		WHERE c.caller_file = 'example.go'`)
	if joinCount < 1 {
		t.Errorf("no calls join to symbols via callee_qname, expected at least 1")
	}
}

func TestDefsExtracted(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	vars := queryStrings(t, db, "SELECT var FROM defs WHERE file = 'example.go' ORDER BY var")
	if !contains(vars, "p") {
		t.Error("missing def p")
	}
	if !contains(vars, "q") {
		t.Error("missing def q")
	}
	if !contains(vars, "d") {
		t.Error("missing def d")
	}
	if !contains(vars, "dx") {
		t.Error("missing def dx")
	}
	if !contains(vars, "sum") {
		t.Error("missing def sum")
	}
}

func TestLinesHaveSymbolAttribution(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	// A line inside main should be attributed to main
	syms := queryStrings(t, db, "SELECT symbol FROM lines WHERE file = 'example.go' AND content LIKE '%Point{X: 1.0%'")
	if len(syms) == 0 || syms[0] != "main" {
		t.Errorf("expected main attribution, got %v", syms)
	}

	// A line inside Distance should be attributed to Point.Distance
	syms = queryStrings(t, db, "SELECT symbol FROM lines WHERE file = 'example.go' AND content LIKE '%dx*dx%'")
	if len(syms) == 0 || syms[0] != "Point.Distance" {
		t.Errorf("expected Point.Distance attribution, got %v", syms)
	}
}

func TestFTSSearchWorks(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")
	if err := finalize(db); err != nil {
		t.Fatal(err)
	}

	count := queryInt(t, db, `SELECT count(*) FROM lines_fts WHERE content MATCH '"Distance"'`)
	if count < 2 {
		t.Errorf("expected at least 2 FTS hits for Distance, got %d", count)
	}
}

func TestDirectoriesPopulated(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")
	if err := populateDirectories(db); err != nil {
		t.Fatal(err)
	}

	count := queryInt(t, db, "SELECT file_count FROM directories WHERE path = '.'")
	if count != 1 {
		t.Errorf("expected 1 file in root directory, got %d", count)
	}
}

func TestChannelsMake(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sampleWithChannels, "chans.go")

	ops := queryStrings(t, db, "SELECT operation FROM channels WHERE file = 'chans.go' ORDER BY line_num")
	if !contains(ops, "make") {
		t.Error("missing make operation")
	}
}

func TestChannelsSendRecvClose(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sampleWithChannels, "chans.go")

	ops := queryStrings(t, db, "SELECT DISTINCT operation FROM channels ORDER BY operation")
	for _, expected := range []string{"send", "recv", "close", "make", "select"} {
		if !contains(ops, expected) {
			t.Errorf("missing channel operation: %s (got %v)", expected, ops)
		}
	}
}

func TestChannelsSendHasVar(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sampleWithChannels, "chans.go")

	vars := queryStrings(t, db, "SELECT var FROM channels WHERE operation = 'send'")
	if len(vars) == 0 {
		t.Fatal("expected send operations with var names")
	}
	if !contains(vars, "ch") {
		t.Errorf("expected send to channel 'ch', got %v", vars)
	}
}

func TestChannelsRangeOnlyForChannelTypes(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sampleNoChannels, "nochans.go")

	count := queryInt(t, db, "SELECT count(*) FROM channels WHERE file = 'nochans.go'")
	if count != 0 {
		t.Errorf("expected 0 channel operations for slice/map range, got %d", count)
	}
}

func TestChannelsRangeOverChannel(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sampleWithChannels, "chans.go")

	ranges := queryStrings(t, db, "SELECT var FROM channels WHERE operation = 'range'")
	if !contains(ranges, "ch") {
		t.Errorf("expected range over channel 'ch', got %v", ranges)
	}
}

func TestCollectGoFilesSkipsVendor(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg"), 0755)
	os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0755)
	os.WriteFile(filepath.Join(dir, "pkg", "main.go"), []byte("package main\n"), 0644)
	os.WriteFile(filepath.Join(dir, "vendor", "lib", "dep.go"), []byte("package lib\n"), 0644)

	files := collectGoFiles(dir)
	for _, f := range files {
		if filepath.Base(f) == "dep.go" {
			t.Error("should skip vendor/")
		}
	}
	found := false
	for _, f := range files {
		if filepath.Base(f) == "main.go" {
			found = true
		}
	}
	if !found {
		t.Error("should find pkg/main.go")
	}
}

func TestFreshenDetectsStaleFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".indexnudge.db")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package main\n\nfunc Hello() {}\n"), 0644)

	if err := fullIndex(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ := sql.Open("sqlite", dbPath)
	count := queryInt(t, db, "SELECT count(*) FROM symbols WHERE name = 'Hello'")
	if count != 1 {
		t.Fatalf("expected 1 Hello symbol, got %d", count)
	}
	db.Close()

	time.Sleep(50 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package main\n\nfunc Hello() {}\nfunc Goodbye() {}\n"), 0644)

	if err := doFreshen(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ = sql.Open("sqlite", dbPath)
	defer db.Close()
	count = queryInt(t, db, "SELECT count(*) FROM symbols WHERE name = 'Goodbye'")
	if count != 1 {
		t.Errorf("freshen should have picked up Goodbye, got %d", count)
	}
}

func TestFreshenRemovesDeletedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".indexnudge.db")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "gone.go"), []byte("package main\n\nfunc Ephemeral() {}\n"), 0644)

	if err := fullIndex(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ := sql.Open("sqlite", dbPath)
	count := queryInt(t, db, "SELECT count(*) FROM files")
	if count != 1 {
		t.Fatalf("expected 1 file, got %d", count)
	}
	db.Close()

	os.Remove(filepath.Join(dir, "gone.go"))

	if err := doFreshen(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ = sql.Open("sqlite", dbPath)
	defer db.Close()
	count = queryInt(t, db, "SELECT count(*) FROM files")
	if count != 0 {
		t.Errorf("freshen should have removed deleted file, got %d", count)
	}
	count = queryInt(t, db, "SELECT count(*) FROM symbols")
	if count != 0 {
		t.Errorf("symbols from deleted file should be gone, got %d", count)
	}
}


func TestFreshenVerifiesAllTables(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".indexnudge.db")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package main\n\nimport \"fmt\"\n\nvar X = 1\n\nfunc Hello() {\n\tfmt.Println(X)\n}\n"), 0644)

	if err := fullIndex(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	// files table
	fc := queryInt(t, db, "SELECT count(*) FROM files")
	if fc != 1 {
		t.Errorf("files count = %d, want 1", fc)
	}

	// lines table
	lc := queryInt(t, db, "SELECT count(*) FROM lines WHERE file = 'lib.go'")
	if lc != 9 {
		t.Errorf("lines count = %d, want 9", lc)
	}

	// lines_fts table
	ftsc := queryInt(t, db, "SELECT count(*) FROM lines_fts WHERE file = 'lib.go'")
	if ftsc != 9 {
		t.Errorf("lines_fts count = %d, want 9", ftsc)
	}

	// symbols table
	sc := queryInt(t, db, "SELECT count(*) FROM symbols WHERE file = 'lib.go' AND name = 'Hello'")
	if sc != 1 {
		t.Errorf("symbols Hello count = %d, want 1", sc)
	}

	// calls table
	cc := queryInt(t, db, "SELECT count(*) FROM calls WHERE caller_file = 'lib.go'")
	if cc < 1 {
		t.Errorf("calls count = %d, want >= 1", cc)
	}

	// imports table
	ic := queryInt(t, db, "SELECT count(*) FROM imports WHERE file = 'lib.go' AND module = 'fmt'")
	if ic != 1 {
		t.Errorf("imports fmt count = %d, want 1", ic)
	}

	// defs table
	dc := queryInt(t, db, "SELECT count(*) FROM defs WHERE file = 'lib.go' AND var = 'X'")
	if dc != 1 {
		t.Errorf("defs X count = %d, want 1", dc)
	}

	// directories table
	dirc := queryInt(t, db, "SELECT count(*) FROM directories")
	if dirc < 1 {
		t.Errorf("directories count = %d, want >= 1", dirc)
	}

	// FTS search works
	fts := queryInt(t, db, "SELECT count(*) FROM lines_fts WHERE content MATCH '\"Hello\"'")
	if fts < 1 {
		t.Errorf("FTS match for Hello = %d, want >= 1", fts)
	}
}

func TestFreshenCleansAllTablesOnDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".indexnudge.db")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package main\n\nimport \"fmt\"\n\nvar X = 1\n\nfunc Hello() {\n\tfmt.Println(X)\n}\n"), 0644)

	if err := fullIndex(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(dir, "lib.go"))
	time.Sleep(50 * time.Millisecond)

	if err := doFreshen(dir, dbPath); err != nil {
		t.Fatal(err)
	}

	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	for _, q := range []struct{ table, col string }{
		{"files", "file"},
		{"lines", "file"},
		{"lines_fts", "file"},
		{"symbols", "file"},
		{"calls", "caller_file"},
		{"imports", "file"},
		{"defs", "file"},
	} {
		c := queryInt(t, db, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = 'lib.go'", q.table, q.col))
		if c != 0 {
			t.Errorf("%s still has %d rows for lib.go after delete", q.table, c)
		}
	}
}

func TestEmptyFileIndexed(t *testing.T) {
	db := setupDB(t)

	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty.go")
	os.WriteFile(emptyFile, []byte("package main\n"), 0644)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, emptyFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexFile(db, fset, "empty.go", emptyFile, f, nil); err != nil {
		t.Fatal(err)
	}

	lc := queryInt(t, db, "SELECT lines FROM files WHERE file = 'empty.go'")
	if lc != 1 {
		t.Errorf("lines = %d, want 1 for single-line file", lc)
	}

	ftsc := queryInt(t, db, "SELECT count(*) FROM lines_fts WHERE file = 'empty.go'")
	if ftsc != 1 {
		t.Errorf("lines_fts count = %d, want 1", ftsc)
	}
}

func TestLineCountNoTrailingPhantom(t *testing.T) {
	db := setupDB(t)
	// sample ends with "\n" — should NOT produce a phantom empty line
	indexSample(t, db, sample, "example.go")

	// Count actual non-empty source lines (what the file really has)
	srcLines := strings.Split(strings.TrimSuffix(sample, "\n"), "\n")

	dbLineCount := queryInt(t, db, "SELECT lines FROM files WHERE file = 'example.go'")
	if int(dbLineCount) != len(srcLines) {
		t.Errorf("files.lines = %d, want %d (trailing newline phantom?)", dbLineCount, len(srcLines))
	}

	maxLine := queryInt(t, db, "SELECT MAX(line_num) FROM lines WHERE file = 'example.go'")
	if int(maxLine) != len(srcLines) {
		t.Errorf("max line_num = %d, want %d", maxLine, len(srcLines))
	}

	// The last line in the DB should not be empty
	lastContent := queryStrings(t, db, "SELECT content FROM lines WHERE file = 'example.go' AND line_num = (SELECT MAX(line_num) FROM lines WHERE file = 'example.go')")
	if len(lastContent) > 0 && lastContent[0] == "" {
		t.Error("last line in DB is empty — phantom trailing line from strings.Split")
	}
}


func TestParseErrorIndexesLinesOnly(t *testing.T) {
	db := setupDB(t)

	// Write invalid Go to a temp file
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.go")
	badSrc := "package main\n\nfunc broken( {\n\treturn\n}\n"
	os.WriteFile(badFile, []byte(badSrc), 0644)

	// Try to parse — should fail
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, badFile, nil, parser.ParseComments)
	if parseErr == nil {
		t.Fatal("expected parse error for invalid Go")
	}

	// Index lines only
	if err := indexFileLinesOnly(db, "bad.go", badFile); err != nil {
		t.Fatal(err)
	}

	// Lines should be present
	lineCount := queryInt(t, db, "SELECT COUNT(*) FROM lines WHERE file = 'bad.go'")
	if lineCount != 5 {
		t.Errorf("lines count = %d, want 5", lineCount)
	}

	// parse_error should be 1
	parseError := queryInt(t, db, "SELECT parse_error FROM files WHERE file = 'bad.go'")
	if parseError != 1 {
		t.Errorf("parse_error = %d, want 1", parseError)
	}

	// No symbols should exist
	symCount := queryInt(t, db, "SELECT COUNT(*) FROM symbols WHERE file = 'bad.go'")
	if symCount != 0 {
		t.Errorf("symbols count = %d, want 0 for parse error file", symCount)
	}

	// No calls should exist
	callCount := queryInt(t, db, "SELECT COUNT(*) FROM calls WHERE caller_file = 'bad.go'")
	if callCount != 0 {
		t.Errorf("calls count = %d, want 0 for parse error file", callCount)
	}
}

func TestParseErrorClearedOnFix(t *testing.T) {
	db := setupDB(t)

	dir := t.TempDir()
	badFile := filepath.Join(dir, "fixme.go")

	// First: index broken file
	badSrc := "package main\n\nfunc broken( {\n}\n"
	os.WriteFile(badFile, []byte(badSrc), 0644)
	indexFileLinesOnly(db, "fixme.go", badFile)

	pe := queryInt(t, db, "SELECT parse_error FROM files WHERE file = 'fixme.go'")
	if pe != 1 {
		t.Fatalf("parse_error should be 1, got %d", pe)
	}

	// Fix the file
	goodSrc := "package main\n\nfunc fixed() {\n}\n"
	os.WriteFile(badFile, []byte(goodSrc), 0644)

	// Delete old rows and re-index
	deleteFileRows(db, "fixme.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, badFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	indexFile(db, fset, "fixme.go", badFile, f, nil)

	pe2 := queryInt(t, db, "SELECT parse_error FROM files WHERE file = 'fixme.go'")
	if pe2 != 0 {
		t.Errorf("parse_error should be 0 after fix, got %d", pe2)
	}

	// Symbols should now exist
	symCount := queryInt(t, db, "SELECT COUNT(*) FROM symbols WHERE file = 'fixme.go'")
	if symCount == 0 {
		t.Error("expected symbols after fixing parse error")
	}
}
func TestFuncSignature(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")

	sigs := queryStrings(t, db, "SELECT signature FROM symbols WHERE name = 'Distance' AND signature IS NOT NULL")
	if len(sigs) == 0 {
		t.Fatal("Distance should have a signature")
	}
	sig := sigs[0]
	if sig == "" {
		t.Error("signature should not be empty")
	}
	// Should contain receiver, name, params, return type
	for _, want := range []string{"Point", "Distance", "Point", "float64"} {
		found := false
		for i := range sig {
			if i+len(want) <= len(sig) && sig[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("signature %q should contain %q", sig, want)
		}
	}
}

const sampleCC = `package example

func Trivial(x int) int {
return x + 1
}

func Linear(x int) int {
y := x + 1
z := y * 2
return z
}

func OneBranch(x int) int {
if x > 0 {
return 1
}
return -1
}

func SwitchThree(x int) string {
switch x {
case 0:
return "zero"
case 1:
return "one"
default:
return "other"
}
}

func ShortCircuit(a, b, c bool) bool {
return a && (b || c)
}

func LoopAndRange(xs []int) int {
total := 0
for _, x := range xs {
if x > 0 {
total += x
}
}
return total
}

func OuterWithClosure(x int) int {
inner := func(y int) int {
if y > 0 {
return y
}
return -y
}
return inner(x)
}
`

func TestCyclomaticComputed(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleCC, "cc.go")

got := map[string]int64{}
rows, err := db.Query("SELECT name, COALESCE(cyclomatic, -1) FROM symbols WHERE file = 'cc.go' AND kind IN ('function','method')")
if err != nil {
t.Fatal(err)
}
defer rows.Close()
for rows.Next() {
var name string
var cc int64
if err := rows.Scan(&name, &cc); err != nil {
t.Fatal(err)
}
got[name] = cc
}

want := map[string]int64{
"Trivial":          1, // no branches
"Linear":           1, // no branches
"OneBranch":        2, // one if
"SwitchThree":      4, // 1 + 3 case clauses
"ShortCircuit":     3, // && and || each add 1
"LoopAndRange":     3, // range + if
"OuterWithClosure": 3, // outer +1 + FuncLit base +1 + inner `if` +1 — matches Mozilla cyclomatic.sum (recurses into closure)
}
for name, w := range want {
if g, ok := got[name]; !ok {
t.Errorf("%s: missing from symbols", name)
} else if g != w {
t.Errorf("%s: cyclomatic = %d, want %d", name, g, w)
}
}
}

func TestCyclomaticNullForNonFunctions(t *testing.T) {
db := setupDB(t)
indexSample(t, db, "package p\n\ntype Foo struct { X int }\n\ntype Bar int\n", "types.go")

rows, err := db.Query("SELECT name, cyclomatic FROM symbols WHERE file = 'types.go' AND kind NOT IN ('function','method')")
if err != nil {
t.Fatal(err)
}
defer rows.Close()
any := false
for rows.Next() {
any = true
var name string
var cc sql.NullInt64
if err := rows.Scan(&name, &cc); err != nil {
t.Fatal(err)
}
if cc.Valid {
t.Errorf("%s (non-function) should have NULL cyclomatic, got %d", name, cc.Int64)
}
}
if !any {
t.Fatal("expected at least one struct/type row")
}
}

const sampleSL = `package example

import (
"fmt"
)

func dispatch(name string) int {
if name == "echo" {
return 1
}
if name != "true" {
return 2
}
switch name {
case "false", "noop":
return 0
case "fail":
return -1
default:
return len(name)
}
}

func talk() {
fmt.Println("hello", "world")
fmt.Sprintf("debug: %s", "info")
}

// Comment mentioning "echo" should be filtered.
func commentFilter() {}
`

func TestStringLiteralsViewDispatchBuckets(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleSL, "sl.go")

rows, err := db.Query(`
SELECT value, context, COUNT(*)
FROM string_literals
WHERE file = 'sl.go'
GROUP BY value, context
ORDER BY value, context`)
if err != nil {
t.Fatal(err)
}
defer rows.Close()

got := map[string]int64{}
for rows.Next() {
var v, c string
var n int64
if err := rows.Scan(&v, &c, &n); err != nil {
t.Fatal(err)
}
got[v+"|"+c] = n
}

checks := []struct{ key string; want int64; reason string }{
{"echo|compare",     1, "if name == \"echo\""},
{"true|compare",     1, "if name != \"true\""},
{"false|match_pat",  1, `case "false", "noop"`},
{"noop|match_pat",   1, "multi-string case clause"},
{"fail|match_pat",   1, `case "fail"`},
{"hello|fmt",        1, "fmt.Println first arg"},
{"debug: %s|fmt",    1, "fmt.Sprintf format string"},
}
for _, c := range checks {
if n, ok := got[c.key]; !ok {
t.Errorf("missing %s (%s)", c.key, c.reason)
} else if n != c.want {
t.Errorf("%s: got count %d, want %d (%s)", c.key, n, c.want, c.reason)
}
}
}

func TestStringLiteralsViewFiltersComments(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleSL, "sl.go")

// The // comment line mentions "echo" but should not appear as a dispatch hit.
hits := queryInt(t, db,
`SELECT COUNT(*) FROM string_literals
 WHERE file = 'sl.go' AND value = 'echo' AND context = 'compare'`)
if hits != 1 {
t.Errorf("expected 1 dispatch 'echo' (the if-eq), got %d (likely comment leaked through)", hits)
}
}

// Note: Go's case clauses with comma-separated values like
//   case "false", "noop":
// emit ONE source line containing both literals. The recursive CTE
// peels each string off and tags both rows match_pat — verified above.
// Sanity-check the pos ordering:
func TestStringLiteralsViewMultiStringOrdering(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleSL, "sl.go")

rows, err := db.Query(`
SELECT value, pos FROM string_literals
WHERE file = 'sl.go' AND context = 'match_pat' AND value IN ('false','noop')
ORDER BY line_num, pos`)
if err != nil {
t.Fatal(err)
}
defer rows.Close()
var order []string
for rows.Next() {
var v string
var p int
if err := rows.Scan(&v, &p); err != nil {
t.Fatal(err)
}
order = append(order, fmt.Sprintf("%s@%d", v, p))
}
if len(order) != 2 || order[0] != "false@1" || order[1] != "noop@2" {
t.Errorf("multi-string ordering wrong: %v", order)
}
}

const sampleDupe = `package example

func first() int {
ctx := makeContext()
builder := ctx.Builder()
result := builder.Compile()
if result == nil {
return -1
}
return *result
}

func second() int {
ctx := makeContext()
builder := ctx.Builder()
result := builder.Compile()
if result == nil {
return -1
}
return *result + 1
}

func thirdUnique() int {
raw := loadRawBytes()
parsed := parseOrDefault(raw)
normalized := canonicalize(parsed)
hashed := hashCanon(normalized)
return len(hashed)
}
`

func TestBlockDupesFinds4LineDuplicates(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleDupe, "dupe.go")

max := queryInt(t, db, "SELECT COALESCE(MAX(occurrences), 0) FROM block_dupes")
if max < 2 {
t.Fatalf("expected at least one 4-line block duplicated 2+ times, got max=%d", max)
}

count := queryInt(t, db,
`SELECT COALESCE(MAX(occurrences), 0) FROM block_dupes
 WHERE shingle LIKE '%makecontext%' AND shingle LIKE '%builder.compile%'`)
if count != 2 {
t.Errorf("expected the makeContext+Compile shingle to occur exactly 2 times, got %d", count)
}
}

func TestBlockDupesSkipsUnique(t *testing.T) {
db := setupDB(t)
indexSample(t, db, sampleDupe, "dupe.go")

unique := queryInt(t, db, "SELECT COUNT(*) FROM block_dupes WHERE shingle LIKE '%canonicalize%'")
if unique != 0 {
t.Errorf("unique block should not appear in block_dupes, got %d hits", unique)
}
}

// ─── Schema version ───────────────────────────────────────────────────────

func TestSchemaVersionPragma(t *testing.T) {
	db := setupDB(t)
	got := queryInt(t, db, "PRAGMA user_version")
	if got != int64(SchemaVersion) {
		t.Errorf("PRAGMA user_version = %d, want %d", got, SchemaVersion)
	}
}

func TestSchemaIsCurrentDetectsCurrent(t *testing.T) {
	db := setupDB(t)
	if !schemaIsCurrent(db) {
		t.Error("schemaIsCurrent should be true on fresh DB")
	}
}

func TestSchemaIsCurrentDetectsOutdated(t *testing.T) {
	db := setupDB(t)
	if _, err := db.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	if schemaIsCurrent(db) {
		t.Error("schemaIsCurrent should be false when user_version < SchemaVersion")
	}
}

// ─── indexed_size column + size-based freshen detection ──────────────────

func TestIndexedSizeStored(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, sample, "example.go")
	got := queryInt(t, db, "SELECT indexed_size FROM files WHERE file = 'example.go'")
	wantMin := int64(len(sample) - 10)
	if got < wantMin {
		t.Errorf("indexed_size = %d, want >= %d (source byte length is %d)", got, wantMin, len(sample))
	}
}

// ─── Cognitive complexity ────────────────────────────────────────────────

// lookupCognitive returns name→cognitive from the symbols table.
func lookupCognitive(t *testing.T, db *sql.DB, file string) map[string]int64 {
	t.Helper()
	m := map[string]int64{}
	rows, err := db.Query("SELECT name, COALESCE(cognitive, -1) FROM symbols WHERE file = ? AND kind IN ('function','method')", file)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var c int64
		if err := rows.Scan(&name, &c); err != nil {
			t.Fatal(err)
		}
		m[name] = c
	}
	return m
}

func TestCognitive20ArmSwitchIsOne(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Dispatch(x int) int {
	switch x {
	case 0:
		return 0
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	case 4:
		return 4
	case 5:
		return 5
	case 6:
		return 6
	case 7:
		return 7
	case 8:
		return 8
	case 9:
		return 9
	case 10:
		return 10
	case 11:
		return 11
	case 12:
		return 12
	case 13:
		return 13
	case 14:
		return 14
	case 15:
		return 15
	case 16:
		return 16
	case 17:
		return 17
	case 18:
		return 18
	case 19:
		return 19
	default:
		return -1
	}
}
`
	indexSample(t, db, src, "dispatch.go")
	got := lookupCognitive(t, db, "dispatch.go")["Dispatch"]
	if got != 1 {
		t.Errorf("20-arm switch with single-return arms: cognitive = %d, want 1 (dispatcher discount)", got)
	}
}

func TestCognitive4LevelNestedIfIs10(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Nested(a, b, c, d bool) int {
	if a {
		if b {
			if c {
				if d {
					return 1
				}
			}
		}
	}
	return 0
}
`
	indexSample(t, db, src, "nested.go")
	got := lookupCognitive(t, db, "nested.go")["Nested"]
	if got != 10 {
		t.Errorf("4-level nested if: cognitive = %d, want 10 (1+2+3+4)", got)
	}
}

func TestCognitiveElseIfLadderNoNestingBonus(t *testing.T) {
	db := setupDB(t)
	src := `package p

import "fmt"

func Ladder(x int) {
	if x == 0 {
		fmt.Println("0")
	} else if x == 1 {
		fmt.Println("1")
	} else if x == 2 {
		fmt.Println("2")
	} else if x == 3 {
		fmt.Println("3")
	} else if x == 4 {
		fmt.Println("4")
	} else if x == 5 {
		fmt.Println("5")
	}
}
`
	indexSample(t, db, src, "ladder.go")
	got := lookupCognitive(t, db, "ladder.go")["Ladder"]
	if got != 6 {
		t.Errorf("6-case else-if ladder: cognitive = %d, want 6 (1 if + 5 else-if, no nesting bonus)", got)
	}
}

func TestCognitiveIfInClosureGetsLambdaBonus(t *testing.T) {
	db := setupDB(t)
	src := `package p

func WithClosure(x int) int {
	f := func(y int) int {
		if y > 0 {
			return y
		}
		return -y
	}
	return f(x)
}
`
	indexSample(t, db, src, "closure.go")
	// Inside the closure body: if has nesting=0 + lambda=1 → +2.
	// The `else` branch is implicit (early return); no else clause emitted.
	// Outer fn body: no decisions.
	// Total = 2.
	got := lookupCognitive(t, db, "closure.go")["WithClosure"]
	if got != 2 {
		t.Errorf("if inside closure (no else): cognitive = %d, want 2 (1 base + 1 lambda)", got)
	}
}

func TestCognitiveSwitchWithNestedSwitch(t *testing.T) {
	db := setupDB(t)
	src := `package p

func NestedSwitch(x, y int) int {
	switch x {
	case 0:
		switch y {
		case 0:
			return 0
		default:
			return 1
		}
	default:
		return 2
	}
}
`
	indexSample(t, db, src, "nestedsw.go")
	// Outer switch: +1 (nesting=0→1)
	// Inner switch (in case body): +2 (1 base + 1 nesting)
	// Total = 3.
	got := lookupCognitive(t, db, "nestedsw.go")["NestedSwitch"]
	if got != 3 {
		t.Errorf("nested switch: cognitive = %d, want 3 (outer +1, inner +2)", got)
	}
}

func TestCognitiveEarlyReturnsAreZero(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Bail(x int) int {
	if x < 0 {
		return -1
	}
	return x
}
`
	indexSample(t, db, src, "bail.go")
	// Just the `if` contributes +1. early return adds 0.
	got := lookupCognitive(t, db, "bail.go")["Bail"]
	if got != 1 {
		t.Errorf("early return: cognitive = %d, want 1 (only the if contributes)", got)
	}
}

func TestCognitiveLabeledBreakContinue(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Looped() {
Outer:
	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			if i == j {
				continue Outer
			}
			if i+j > 15 {
				break Outer
			}
		}
	}
}
`
	indexSample(t, db, src, "looped.go")
	// Outer for: +1 (nesting=1)
	// Inner for: +2 (nesting=2)
	// First if: +3 (nesting=3 after first if entry? no — nesting is bumped INSIDE if)
	// Actually: outer for body has nesting=1. Inner for: +2, nesting=2.
	//   Inner for body has nesting=2.
	//   First if: +3 (2+1), nesting=3.
	//     Labeled continue Outer: +1.
	//   nesting=2.
	//   Second if: +3 (2+1), nesting=3.
	//     Labeled break Outer: +1.
	// Total: 1+2+3+1+3+1 = 11.
	got := lookupCognitive(t, db, "looped.go")["Looped"]
	if got != 11 {
		t.Errorf("labeled break/continue: cognitive = %d, want 11", got)
	}
}

func TestCognitiveMozillaCalibration(t *testing.T) {
	// Verbatim Mozilla rust_1_level_nesting translated to Go.
	// Calibration anchor: per-function value should equal Mozilla's
	// cognitive.sum for the equivalent Rust source (sum = 11).
	db := setupDB(t)
	src := `package p

func F() {
	if true {                              // +1
		if true {                          // +2 (nesting = 1)
			_ = 0
		} else if 1 == 1 {                 // +1
			if true {                      // +3 (nesting = 2)
				_ = 0
			}
		} else {                           // +1
			if true {                      // +3 (nesting = 2)
				_ = 0
			}
		}
	}
}
`
	indexSample(t, db, src, "moz.go")
	got := lookupCognitive(t, db, "moz.go")["F"]
	if got != 11 {
		t.Errorf("Mozilla 1-level-nesting calibration: cognitive = %d, want 11", got)
	}
}

func TestCognitiveNullForNonFunctions(t *testing.T) {
	db := setupDB(t)
	indexSample(t, db, "package p\n\ntype Foo struct { X int }\n\ntype Bar int\n", "types2.go")

	rows, err := db.Query("SELECT name, cognitive FROM symbols WHERE file = 'types2.go' AND kind NOT IN ('function','method')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		any = true
		var name string
		var c sql.NullInt64
		if err := rows.Scan(&name, &c); err != nil {
			t.Fatal(err)
		}
		if c.Valid {
			t.Errorf("%s (non-function) should have NULL cognitive, got %d", name, c.Int64)
		}
	}
	if !any {
		t.Fatal("expected at least one struct/type row")
	}
}

// ─── Blocks table ────────────────────────────────────────────────────────

func TestBlocksExtractedKinds(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Spans(x int) int {
	if x > 0 {
		return 1
	} else {
		return -1
	}
}

func Loops(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	for i := 0; i < 10; i++ {
		total += i
	}
	return total
}

func Switched(x int) string {
	switch x {
	case 0:
		return "zero"
	default:
		return "other"
	}
}
`
	indexSample(t, db, src, "blocks.go")

	kinds := queryStrings(t, db, "SELECT DISTINCT kind FROM blocks WHERE file = 'blocks.go' ORDER BY kind")
	wantAtLeast := []string{"else", "fn", "for", "if", "switch"}
	for _, want := range wantAtLeast {
		if !contains(kinds, want) {
			t.Errorf("blocks kinds missing %q (have %v)", want, kinds)
		}
	}
}

func TestBlocksParentSymbolPopulated(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Outer(x int) int {
	if x > 0 {
		return 1
	}
	return -1
}
`
	indexSample(t, db, src, "blocks_parent.go")
	parents := queryStrings(t, db,
		"SELECT DISTINCT parent_symbol FROM blocks WHERE file = 'blocks_parent.go' AND parent_symbol IS NOT NULL")
	if !contains(parents, "Outer") {
		t.Errorf("expected blocks.parent_symbol = 'Outer', got %v", parents)
	}
}

// ─── Refs table ──────────────────────────────────────────────────────────

func TestRefsExtractedKinds(t *testing.T) {
	db := setupDB(t)
	src := `package p

import "fmt"

type Widget struct {
	Name string
}

func make() *Widget { return &Widget{Name: "x"} }

func Use(w *Widget) {
	x := w.Name
	fmt.Println(x)
	_ = make()
}
`
	indexSample(t, db, src, "refs.go")
	kinds := queryStrings(t, db, "SELECT DISTINCT kind FROM refs WHERE file = 'refs.go' ORDER BY kind")
	// "call" from plain Ident call (make()); "field" from selector .Sel (Println, Name);
	// "def" from x := ..., from `make` and `Use` FuncDecl.Name, from Widget params; "type" from
	// type positions; "use" elsewhere.
	for _, want := range []string{"call", "def", "field", "type", "use"} {
		if !contains(kinds, want) {
			t.Errorf("refs kinds missing %q (have %v)", want, kinds)
		}
	}
}

func TestRefsCapturesNamedReferences(t *testing.T) {
	db := setupDB(t)
	src := `package p

import "fmt"

func F() {
	x := 42
	fmt.Println(x)
}
`
	indexSample(t, db, src, "refs2.go")
	got := queryInt(t, db, "SELECT COUNT(*) FROM refs WHERE file = 'refs2.go' AND name = 'x'")
	if got < 2 {
		t.Errorf("expected at least 2 refs to 'x' (def + use), got %d", got)
	}
	gotPrintln := queryInt(t, db, "SELECT COUNT(*) FROM refs WHERE file = 'refs2.go' AND name = 'Println' AND kind = 'field'")
	if gotPrintln == 0 {
		t.Error("expected fmt.Println selector recorded with kind='field'")
	}
}

// ─── match_arms table ────────────────────────────────────────────────────

func TestMatchArmsExtracted(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Pick(x int) string {
	switch x {
	case 0:
		return "zero"
	case 1, 2, 3:
		return "small"
	default:
		return "other"
	}
}
`
	indexSample(t, db, src, "arms.go")
	patterns := queryStrings(t, db,
		"SELECT pattern FROM match_arms WHERE file = 'arms.go' ORDER BY start_line")
	if len(patterns) != 3 {
		t.Fatalf("expected 3 match_arms rows, got %d: %v", len(patterns), patterns)
	}
	want := []string{"0", "1, 2, 3", "default"}
	for i, w := range want {
		if patterns[i] != w {
			t.Errorf("match_arms[%d].pattern = %q, want %q", i, patterns[i], w)
		}
	}
}

func TestMatchArmsHasEarlyReturn(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Mixed(x int) int {
	switch x {
	case 0:
		return 0
	case 1:
		_ = x + 1
		return 1
	case 2:
		_ = x + 2
	default:
		_ = func() { return }()
		_ = x
	}
	return -1
}
`
	indexSample(t, db, src, "arms_er.go")

	rows, err := db.Query(
		"SELECT pattern, has_early_return FROM match_arms WHERE file = 'arms_er.go' ORDER BY start_line")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var pat string
		var er int
		if err := rows.Scan(&pat, &er); err != nil {
			t.Fatal(err)
		}
		got[pat] = er
	}

	want := map[string]int{
		"0":       1, // bare return
		"1":       1, // return after stmt
		"2":       0, // no return
		"default": 0, // return is inside a closure — must NOT count
	}
	for pat, w := range want {
		if g, ok := got[pat]; !ok {
			t.Errorf("missing arm %q", pat)
		} else if g != w {
			t.Errorf("arm %q: has_early_return = %d, want %d", pat, g, w)
		}
	}
}

func TestMatchArmsParentFnPopulated(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Outer(x int) int {
	switch x {
	case 0:
		return 0
	default:
		return -1
	}
}
`
	indexSample(t, db, src, "arms_parent.go")
	parents := queryStrings(t, db,
		"SELECT DISTINCT parent_fn FROM match_arms WHERE file = 'arms_parent.go'")
	if !contains(parents, "Outer") {
		t.Errorf("expected match_arms.parent_fn = 'Outer', got %v", parents)
	}
}

func TestMatchArmsSelectStmt(t *testing.T) {
	db := setupDB(t)
	src := `package p

func Multiplex(in, out chan int, done chan struct{}) {
	for {
		select {
		case v := <-in:
			out <- v
		case <-done:
			return
		}
	}
}
`
	indexSample(t, db, src, "arms_select.go")
	got := queryInt(t, db, "SELECT COUNT(*) FROM match_arms WHERE file = 'arms_select.go'")
	if got != 2 {
		t.Errorf("expected 2 match_arms rows from 2-arm select, got %d", got)
	}
}

