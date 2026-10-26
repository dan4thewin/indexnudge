# Contributing to indexnudge

This repository contains the Rust/Go indexers, the optional auto-refreshing
`sqlite3` wrapper, and public query documentation. Experimental nudges and
text-tool wrappers belong in the separate experiment repository.

- Preserve the CLI and database contracts unless a requested change
  intentionally changes them. Rust and Go schemas have documented differences.
- Each indexer's source contains its schema. Bump its schema version when
  stored data or query semantics change so `--freshen` rebuilds old indexes.
- Keep `README.md`, `docs/queries.md`, and the generated agent snippets
  consistent with actual behavior. Edit `AGENTS.md.gpp`, then run
  `make snippets` rather than editing generated snippets directly.
- Use the existing tests: `make rust-test`, `make go-test`, or `make test`.
  When checking the wrapper, exercise the built CLI and a real SQLite
  query; stdout should remain usable as query output.
- Do not add mandatory complexity/coverage gates or tool-use restrictions
  just because the documentation demonstrates how downstream users can
  build such policies. The default product is the index, not enforcement.
- Keep changes focused. Ask before consequential, unsettled public API or
  architectural choices; ordinary reversible implementation choices do
  not need repeated confirmation.
