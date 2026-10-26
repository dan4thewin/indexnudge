# SQL recipes

These patterns come from day-to-day use of a code index. They are
examples, not mandatory contributor gates. Set `DB` to an existing
`.indexnudge-rust.db` or `.indexnudge-go.db`, then inspect its schema:

```sh
DB=.indexnudge-rust.db
sqlite3 "$DB" .schema
```

Use the refresh wrapper, or run the appropriate indexer's `--freshen`
(Go: `-freshen -dir PROJECT`) before querying. In-process SQLite clients
do not trigger the wrapper.

## Navigate before reading whole files

```sh
sqlite3 "$DB" "SELECT file, lines, language FROM files ORDER BY file;"
sqlite3 "$DB" "SELECT name, kind, start_line, end_line, signature
  FROM symbols WHERE file='src/foo.rs' ORDER BY start_line;"
sqlite3 "$DB" "SELECT line_num, content FROM lines
  WHERE file='src/foo.rs' AND line_num BETWEEN 120 AND 180 ORDER BY line_num;"
sqlite3 "$DB" "SELECT file, line_num, content FROM lines_fts
  WHERE lines_fts MATCH 'render_template';"
sqlite3 "$DB" "SELECT caller_file, caller_symbol, line_num
  FROM calls WHERE callee='render_template';"
sqlite3 "$DB" "SELECT module, line_num FROM imports WHERE file='src/foo.rs';"
```

Change the example paths and names to your project. Call rows describe
syntactic sites; possible targets are not a proof of runtime reachability:

```sh
sqlite3 "$DB" "SELECT callee, callee_qname, candidate_qname, def_file, def_line
  FROM callee_candidates WHERE caller_symbol='handle_request';"
```

## Read complexity on two axes

A broad, shallow dispatcher may have high cyclomatic complexity but low
cognitive complexity. Nested conditionals can have the opposite shape.
Show both rather than automatically refactoring the widest match:

```sh
sqlite3 "$DB" "SELECT name, file, start_line, cyclomatic, cognitive,
  cognitive - cyclomatic AS delta
  FROM symbols
  WHERE kind IN ('function','method')
    AND cyclomatic IS NOT NULL AND cognitive IS NOT NULL
  ORDER BY MAX(cyclomatic, cognitive) DESC LIMIT 20;"

sqlite3 "$DB" "SELECT AVG(cyclomatic) AS mean_cc, AVG(cognitive) AS mean_cog
  FROM symbols WHERE kind IN ('function','method')
    AND cyclomatic IS NOT NULL AND cognitive IS NOT NULL;"
```

Recompute means from the current project; historical hotspot rankings and
absolute scores drift. Scores are useful signals, not interchangeable
with every other calculator's output.

## Optional downstream gate: changed functions, not the whole backlog

One downstream workflow reports both metrics, exempts shallow dispatchers,
and gates only functions touched by a change. The query below illustrates
that policy with **example thresholds**, not indexnudge defaults:

```sql
WITH
changed(file, first_line, last_line) AS (
    VALUES ('src/foo.rs', 120, 180)
),
population AS (
    SELECT * FROM symbols
    WHERE kind IN ('function','method')
      AND cyclomatic IS NOT NULL AND cognitive IS NOT NULL
),
means AS (
    SELECT AVG(cyclomatic) AS cc, AVG(cognitive) AS cog FROM population
)
SELECT s.name, s.file, s.start_line, s.cyclomatic, s.cognitive
FROM population s CROSS JOIN means m
WHERE s.cognitive > 2
  AND s.cognitive - s.cyclomatic > -15
  AND (s.cyclomatic > 3 * m.cc OR s.cognitive > 3 * m.cog)
  AND EXISTS (
      SELECT 1 FROM changed c
      WHERE c.file = s.file
        AND c.first_line <= s.end_line AND c.last_line >= s.start_line
  )
ORDER BY MAX(s.cyclomatic, s.cognitive) DESC;
```

In a hook, replace `changed` with staged hunk ranges and use the same
source-directory scope for both the population and candidates. The
`cognitive <= 2` exemption matters independently of the wide-dispatcher
delta exemption: a small, flat dispatcher may not have a delta of -15.

An actual commit gate must also keep its indexed snapshot aligned with
the staged changes. Freshening the working tree alone is not enough when
the index contains unstaged edits. Treat query/refresh errors as errors,
not an empty passing report. No such hook is installed by this project.

## Find contexts rather than incidental mentions

```sh
sqlite3 "$DB" "SELECT file, line_num, symbol, value, context
  FROM string_literals
  WHERE value IN ('echo','printf') AND context IN ('compare','match_pat')
  ORDER BY file, line_num;"
sqlite3 "$DB" "SELECT occurrences, sites FROM block_dupes
  ORDER BY occurrences DESC LIMIT 10;"
```

Both views are approximations over indexed source lines. Check the
surrounding source before treating a match as semantic evidence.

## Rust: investigate possibly dead code

```sql
SELECT v.enum, v.variant
FROM (SELECT DISTINCT enum, variant FROM enum_variant_refs) v
WHERE NOT EXISTS (
    SELECT 1 FROM enum_variant_refs c
    WHERE c.enum = v.enum AND c.variant = v.variant
      AND c.context = 'construct'
);

SELECT s.name
FROM (SELECT DISTINCT name FROM struct_refs) s
WHERE NOT EXISTS (
    SELECT 1 FROM struct_refs c
    WHERE c.name = s.name AND c.context = 'construct'
);
```

Re-run after each removal: a construction site may itself belong to dead
code, so the next layer can appear only after cleanup. Imported bare
variants, aliases, generated code, macros, FFI allocation, and different
types sharing a short name can defeat these heuristics.

Visibility can focus a function report, but it is written visibility,
not effective public reachability through every enclosing module:

```sql
SELECT name, file, start_line
FROM symbols
WHERE kind = 'function' AND visibility IS NOT NULL
  AND name NOT IN (
      SELECT DISTINCT callee FROM calls WHERE kind IN ('function','method')
  )
ORDER BY file, start_line;
```

Functions used as values/callbacks, entry points, tests, exports, and
macro-generated callers can be missing from a syntactic call graph.
Rust's own lints remain useful; an index report is not a deletion oracle.
