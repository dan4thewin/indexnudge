# indexnudge

**A local code index you can query with SQL.** Index Rust or Go source into
SQLite, then use ordinary `SELECT` statements to explore symbols, calls,
source lines, complexity, and references.

The index works on its own. An optional `sqlite3` wrapper refreshes changed
files before a query. No daemon, hosted service, patched coding agent, or
replacement for `grep`, `sed`, or `cat` is required.

## Build

Requires Bash, a SQLite CLI with FTS5, and the toolchain for the language
you want to index:

- **Rust:** a toolchain supporting edition 2024, Cargo, and a C compiler
  for tree-sitter and the bundled SQLite library.
- **Go:** Go 1.25 or newer.

```sh
make rust                 # bin-rust/indexnudge
make go                   # bin-go/indexnudge
make                      # both
```

The two executables have the same name but live in separate directories.
Rust uses tree-sitter-rust; Go uses `go/ast` and `go/types`. Both run
natively on the development machine.

## Index and query

```sh
# Rust
bin-rust/indexnudge /path/to/rust-project
sqlite3 /path/to/rust-project/.indexnudge-rust.db \
  "SELECT name, kind, file, start_line FROM symbols ORDER BY file, start_line;"

# Go
bin-go/indexnudge -dir /path/to/go-project
sqlite3 /path/to/go-project/.indexnudge-go.db \
  "SELECT caller_symbol, callee, line_num FROM calls LIMIT 20;"
```

Go package/type information is loaded in the indexed project's module
environment. Without usable package information, the indexer can fall back
to syntax-only indexing.

Each index is a snapshot. Refresh explicitly when using a normal SQLite
client:

```sh
bin-rust/indexnudge --freshen /path/to/rust-project
bin-go/indexnudge -freshen -dir /path/to/go-project
```

Freshening handles added, changed, and deleted files, checking modification
time and size. A no-op is silent. A schema version change triggers a full
rebuild; the database is disposable, not a migration store.

## Optional: refresh before every SQLite query

Put the matching directory on PATH:

```sh
export PATH="/path/to/indexnudge/bin-rust:$PATH"  # or bin-go
sqlite3 /path/to/project/.indexnudge-rust.db ".schema"
```

These directories contain **only the indexer and `sqlite3` wrapper**. Your
normal text tools are unchanged. Keep the checkout layout intact: the
wrapper symlinks point at `bin/sqlite3`.

For an existing `.indexnudge-rust.db` or `.indexnudge-go.db`, the wrapper
runs the corresponding indexer before forwarding arguments to the real
SQLite CLI. Ordinary SQLite databases pass through. Refresh errors are
reported rather than querying a known-stale snapshot. Initial indexing is
explicit; the wrapper does not discover or create new indexes.

Python's `sqlite3`, Rust's `rusqlite`, GUI clients, and other libraries do
not run this wrapper. Freshen explicitly before opening the database from
those clients.

## What's in the database?

| Tables / views | Use |
|---|---|
| `files`, `directories` | Project inventory and line counts |
| `symbols` | Definitions, signatures, source spans, cyclomatic and cognitive complexity |
| `lines`, `lines_fts` | Source lines with enclosing-symbol attribution and FTS5 search |
| `calls`, `callee_candidates` | Call sites and possible local targets |
| `defs`, `refs`, `imports` | Bindings, identifier references, and imports |
| `blocks`, `match_arms` | Block and branch spans; Go switch/select clauses use `match_arms` |
| `string_literals`, `block_dupes` | String contexts and repeated four-line runs |
| Rust: `enum_variant_refs`, `struct_refs` | Candidates for never-constructed variants and structs |
| Go: `channels` | Channel creation, send, receive, close, range, and select |

The schemas are similar, not identical: Rust also exposes
`symbols.visibility`. Use `sqlite3 DATABASE .schema` for the actual schema;
the schema definitions in each indexer are authoritative.

See [SQL recipes](docs/queries.md) for navigation, two-metric complexity
reports, and an **optional downstream gate example**. The recipes inform
your workflow; indexnudge does not install or enforce project policy.

## Use with a coding agent

Append one of the self-contained instruction blocks to a project's
`AGENTS.md`:

```sh
cat AGENTS.md.rust.snippet >> /path/to/project/AGENTS.md
# Or AGENTS.md.go.snippet / AGENTS.md.snippet for a mixed project.
```

The templates teach queries and explain freshness and limitations. They
do not require tool interception or prescribe complexity thresholds.
Edit `AGENTS.md.gpp` and run `make snippets` to regenerate them; only
template development requires `gpp`.

## Limits

This is a source index, not a compiler's complete call graph. Rust paths
are recorded as written, method targets can be ambiguous, and macro
contents are inspected heuristically rather than expanded. Some views
are line-based approximations. Never-constructed/dead-code results are
**candidates to investigate, not proof that deletion is safe**.

Complexity scores follow the implemented counting conventions and need
not exactly match other tools. Read cyclomatic and cognitive scores
together: a wide dispatcher and deeply nested logic are different shapes.

## Development

`make test` runs the existing Rust and Go tests and checks the wrapper's
shell syntax. No quality hooks are installed automatically.

Experimental text-tool wrappers, agent nudges, benchmark transcripts,
and the project's development history are maintained separately in
[`indexnudge-exp`](https://github.com/dan4thewin/indexnudge-exp); they are
not part of this distribution.

## License

Apache License 2.0. See [LICENSE](LICENSE).
