# Fail-closed refresh with SQLite-only locking

Status: implemented, with the locked no-op preflight described below.
Written 2026-11-01. Deployment and legacy-artifact recovery remain manual.

## Recommendation

Use two-phase schema publication on one physical SQLite connection, with
`locking_mode=EXCLUSIVE` retaining the database lock across both commits:

```text
acquire exclusive lock
  published, current schema -> scan for changes
    unchanged -> release lock without writing
    changed -> withdraw
    scan error -> commit withdrawal if possible, then fail
  published -> commit withdrawal -> unavailable
  unavailable -> commit refreshed schema/data -> published
close connection and release lock
```

This preserves the premise that **a known-stale index is worse than no usable
index**. It keeps ordinary SQLite clients inside the coordination mechanism,
without requiring application-level filesystem renames, `flock`, a daemon,
or cooperation with a separate lock-file convention.

The implementation is larger than hardening the existing `flock` variant.
Work-bearing refreshes pay for schema withdrawal and two durable commits;
no-op refreshes avoid those writes. It is the
better architectural fit for an index intended to be queried through ordinary
SQLite clients. Hardened `flock` remains a reasonable lower-effort containment
fix for a controlled, wrapper-only deployment.

Plain transactional refresh that rolls back to a publicly readable old index
is **not** an acceptable substitute.

## Contract and boundaries

The guarantees apply to supported SQLite clients on a filesystem with working
SQLite locking and durability primitives:

- After withdrawal commits, failed work before publication leaves the public
  index schema unavailable, not restored to its previous contents.
- The old data may survive under private names for recovery. It is not a
  published index and must never be restored without successfully refreshing
  or rebuilding it.
- Tables, FTS, views, file metadata, aggregates, and schema version are published
  together. Queries do not see a partly refreshed index.
- New readers wait while the writer holds its exclusive lock, subject to each
  reader's busy timeout. A timeout is an error, not permission to serve stale data.
- The canonical database file stays in place. "No usable index" means absent
  public index objects, not an absent file or successful queries returning
  misleading empty results.
- An open connection outside a transaction does not retain a snapshot.
  Already-active read transactions may finish before the writer acquires its
  initial exclusive lock.
- Successful publication represents the source observed during indexing, not
  an instantaneous or continuously updated snapshot of the entire filesystem.

### The pre-withdrawal limitation is real

**If the writer cannot acquire its lock or commit withdrawal, the old public
schema can remain available.** Examples include a long-held read transaction,
a read-only database, insufficient journal space, or an I/O error.

The wrapper must fail rather than issue its query, but cannot stop an unrelated
client reading that old schema after the failed writer releases its connection.
SQLite cannot persist invalidation when it cannot perform the required write.
Calling this "refresh did not start" must not obscure that observable limitation.

Filesystem withdrawal also has prerequisites, such as permission to rename in
the containing directory. However, a filesystem rename can sometimes withdraw
a read-only database that a schema-writing approach cannot invalidate. If
withdrawal under those conditions is a hard requirement, that is a reason to
prefer a filesystem-based publication mechanism instead.

## Protocol

### Connection setup and ownership

Keep rollback-journal `DELETE` mode for this design. WAL's concurrent-reader
advantage is not used while deliberately holding an exclusive database lock.
Initialize or validate journal mode during setup; do not silently change a
live WAL database underneath other clients.

Configure connection settings outside transactions. In particular, do normal
setup before enabling exclusive locking, then acquire `BEGIN EXCLUSIVE` before
reading the index catalog or state. Avoid first acquiring and retaining a
shared lock in exclusive locking mode and then trying to upgrade it.

```sql
-- One physical connection; approved rollback-journal configuration.
PRAGMA busy_timeout=5000;
PRAGMA synchronous=EXTRA;
PRAGMA main.locking_mode=EXCLUSIVE;

BEGIN EXCLUSIVE;
-- Validate state. For a published/current index, scan for changes.
-- If unchanged: ROLLBACK and close the physical connection; stop here.
-- Otherwise save the object manifest and withdraw public objects.
COMMIT;
-- The exclusive database lock remains held.

BEGIN EXCLUSIVE;
-- Restore names privately within this transaction.
-- Refresh/rebuild, recreate views, and update publication metadata.
COMMIT;
-- The exclusive database lock still remains held.

-- Application closes the physical connection to release the lock.
```

`BEGIN EXCLUSIVE` alone, in normal locking mode, does not span the commits.
Connection-level `locking_mode=EXCLUSIVE` is essential. There is no
inter-transaction ownership gap and no need for a persistent PID lock or a
second writer-arbitration mechanism.

Use `synchronous=EXTRA` as the conservative rollback-journal setting. This
tightens the earlier `FULL` proposal: durability matters for the withdrawal
commit itself, since losing that commit could make an old public schema
reappear after a power failure. SQLite documents additional DELETE-journal
durability with `EXTRA`. This is a real I/O cost, not a free safety improvement;
the exact sync operations depend on SQLite's journal handling.

### Persistent state and object manifest

Reserve a private namespace for protocol metadata and unpublished objects.
`_indexnudge_state` stores protocol version, language, and publication status.
`PRAGMA user_version` retains the old layout version until publication succeeds.
`_indexnudge_objects` stores names, kinds, and original view definitions;
private table names are derived as `_indexnudge_old_<original-name>`.

The control record remains accessible once the lock is released, even when the
index is unavailable. This lets an indexer resume and lets the wrapper select
the language for legacy database filenames without querying a withdrawn
`files` table.

The state flag is not what enforces unavailability. **Missing public object
names enforce it.** A `ready=false` flag alongside unchanged public tables
would not protect ordinary `SELECT` statements.

Catalog rules:

- Treat the database as index-owned, not as a general application database.
  Withdraw its application tables and views, excluding SQLite internals and
  the protocol's own private metadata.
- Reserve private prefixes and reject collisions. Quote catalog-derived SQL
  identifiers correctly.
- Rename the FTS5 virtual table as a unit. Let SQLite manage its shadow tables;
  do not enumerate and rename those shadow tables independently.
- Capture and drop public views before renaming tables. SQLite otherwise
  rewrites view references to the private names, leaving those views able to
  serve stale data after failure.
- Preserve ordinary custom view definitions for incremental refresh. Full
  rebuilds continue to treat custom additions as disposable, as the existing
  whole-file rebuild does.
- Neither the Rust nor Go schema defines triggers. Custom table-attached
  triggers and indexes survive supported table renames. Custom `INSTEAD OF`
  triggers on views do not: dropping a view drops its triggers, and the manifest
  stores only the view definition. Once withdrawal commits, these triggers are
  lost, even if the subsequent refresh fails; they are not automatically recreated.
- Do not manipulate `sqlite_schema` with `writable_schema`, or silently bypass
  malformed/unsupported schema objects.

Private data remains discoverable through catalog inspection and deliberate
private-name queries, just as an old `.bak` could be opened deliberately.
This is a publication boundary, not an access-control or secrecy boundary.

### Locked preflight

For a published index with the current schema/protocol, scan file names,
modification times, and sizes under the exclusive lock. Stop at the first
detected change. A complete, error-free scan with no changes rolls back the
read-only transaction and closes the connection without touching schema/data.

If the scan fails, attempt to commit withdrawal while still holding the lock,
then report the original error. Failure to commit withdrawal is reported too.
An abrupt process death cannot run this cleanup: the old public schema may
remain readable after lock release. This intentionally expands the
pre-withdrawal crash window in exchange for cheaper no-op refreshes.

Already-unavailable indexes, older schemas, initial builds, and explicitly
requested full rebuilds do not use this fast path.

### Phase A: withdraw and commit

After obtaining the exclusive lock:

1. Determine whether the database is published, already unavailable, or new.
   Validate language and supported schema/protocol versions under the lock.
2. For a published index, save the manifest and original view definitions,
   drop the public views, and rename tables into the private namespace.
3. Record the unavailable state and retained layout information in the same
   transaction, then commit.

On a detected change, do this before the full refresh traversal, source reads,
and parsing. The work phase scans again rather than relying on a partial
preflight result. A preflight error withdraws and exits without entering Phase B.

If an earlier attempt already committed withdrawal, use its retained state
rather than repeating withdrawal or treating it as a stale process lock.
A new, uninitialized database likewise has no published snapshot to withdraw.
Those paths need not add an otherwise empty withdrawal commit.

### Phase B: refresh and publish

Keep the same connection and exclusive lock:

1. Start the next transaction. For incremental refresh, rename private tables
   back to their public names and restore views inside this uncommitted transaction.
2. Scan source and update all affected tables, FTS, file metadata, and aggregates.
   For a rebuild, replace the private schema/data with the target public schema
   and contents within this transaction instead.
3. Publish the final schema version and state, and clear the pending manifest,
   in the same transaction.
4. Commit and close the physical connection before reporting success or
   handing control to the query client.

Other connections cannot observe the temporary restoration of public names.
They remain blocked by the connection's exclusive lock.

An error after Phase A commits but before publication must roll back Phase B
where possible and close the connection. SQLite then exposes the committed
unavailable state. A later indexer may resume using retained data, but must
actually finish a refresh or rebuild before publishing. An error reporting a
completed publication does not invalidate its freshly committed contents.

If the outcome of a commit or cleanup is uncertain, report that uncertainty.
Never claim that rollback happened merely because an API returned an error.

## Implementation and rollout

1. **Connection configuration is separate from schema creation.**
   [Rust orchestration](../rust/src/refresh.rs) and
   [Go orchestration](../go/refresh.go) own connection settings and publication.
   `create_schema` / `createSchema` retain the language-specific public DDL.
   Tables, views, indexes, and final version publication are transactional.

2. **Each language shares one publication/recovery path.**
   Incremental refresh, full rebuild, schema upgrade, and initialization use
   the same locking machinery. Rust schema version 13 and Go schema version 2
   introduce the private publication metadata. Interrupted migrations retain
   the old version and object manifest until a subsequent rebuild succeeds.

3. **Connection and transaction ownership are explicit.**
   Rust retains one `rusqlite::Connection` and uses exclusive transactions
   with rollback-on-drop behavior. Go reserves one `*sql.Conn`, calls
   `Conn.BeginTx` with the driver's `_txlock=exclusive` setting, and passes each
   `*sql.Tx` through a small transaction-compatible helper interface. No
   production transaction statements escape through a pooled `*sql.DB`.

   In Go, `Conn.Close` returns the connection to its pool; it is not a physical
   close in general. The indexer disables idle pooling on its dedicated handle,
   and closes both the reserved connection and handle on every exit.
   A failed `Tx.Commit` can also leave the Go transaction handle unusable for
   a subsequent rollback: do not rely solely
   on a deferred `Tx.Rollback`. Physical closure is the final cleanup for native
   transactions, including failed commits, and is required before reporting success.
   Work and cleanup errors are propagated together.

4. **Refresh errors propagate rather than publishing partial files.**
   Directory-walk, stat, read, row-iteration, and database-write errors abort.
   Intentional parse-error/syntax-only degradation remains. Stored lines,
   extraction, and metadata use captured source bytes; Go supplies those bytes
   to its package loader through an overlay. Metadata is checked around reads
   and again for indexed files before publication. This is not a global
   filesystem snapshot.

5. **The optional [SQLite wrapper](../bin/sqlite3) handles unavailable indexes.**
   Language dispatch, explicit output paths, physical-path handling, and query
   options remain supported. Metadata probes use no-create `mode=rw` connections
   so SQLite can recover hot journals after a killed writer; a read-only
   connection cannot do that recovery. Probes have busy handling and ignore
   user initialization scripts. Legacy filenames use persistent language
   metadata during recovery. Missing/uninitialized index paths are rejected;
   initial indexing stays explicit. Ordinary databases and `file:` URIs pass
   through (URIs do not trigger refresh). Refresh errors stop the query, and
   stdout remains query output.

6. **Roll out the whole protocol together; deployment remains manual.**
   Stop legacy rename-based writers before deploying both new indexers and the
   matching wrapper. Do not support mixed old/new writers. Handle old `.bak`
   and `.tmp` artifacts explicitly rather than restoring or deleting them
   automatically. Inspect for dangling artifact symlinks manually: Rust's
   current check misses them, whereas Go rejects them. README, query
   documentation, and regenerated agent snippets describe the new contract.
   After release, update the experiment repository's core pin rather than
   reviving duplicate sources.

The existing Rust/Go test targets cover rollback of data/schema, no-op writes,
retained locks, process death, recovery, and view/FTS correctness.
`make wrapper-test` exercises built CLIs and real SQLite queries with failures,
concurrent writers, hot-journal recovery, option passthrough, and metadata-probe
waiting. No coverage thresholds or complexity gates are introduced.

## Failure modes

| Case | Handling and remaining limitation |
|---|---|
| Cannot open the database, obtain the exclusive lock, or complete setup | Fail explicitly and do not run the wrapper's query. Withdrawal has not committed, so an old public schema may remain. This is not fully addressed by a SQLite-only scheme. |
| Preflight scan fails normally | Attempt withdrawal before releasing the lock, then report the scan error. A failed withdrawal is also reported and may leave the old public schema available. No scan error is classified as "unchanged." |
| Process dies during preflight | The old public schema can remain after lock release. This is the accepted cost of skipping publication writes on no-ops; reliable abrupt-death invalidation would require a prior durable withdrawal. |
| Phase A fails: read-only database, journal-space exhaustion, malformed schema, unsupported rename, or withdrawal commit failure | Roll back where possible and close. The same pre-withdrawal limitation applies: do not claim the index was invalidated. No unchecked fallback to another locking/journal mode. |
| Source traversal, stat, or read fails after withdrawal | Abort and leave the index unavailable. An incomplete traversal must not become a successful mass deletion. Retained private data can support a later retry. |
| Source syntax errors or unavailable type information | Preserve the documented lines-only/syntax-only degradation and diagnostics. Distinguish these accepted outcomes from actual I/O and storage failures. |
| SQL, constraint, FTS, or aggregation error midway through refresh | Roll back the entire second transaction. All data changes and public-name restoration disappear together; the old snapshot stays private. |
| User-added `INSTEAD OF` triggers on custom views | Not preserved by either indexer. Withdrawal drops the views and their triggers; recovery restores only the view definitions. This affects user additions, not the trigger-free shipped schemas. |
| Disk full or I/O failure during Phase B or rollback | Report failure, close, and let SQLite recovery operate on the journal. Do not assert database health if storage failure prevents reliable rollback. Persistent corruption requires explicit recovery/recreation. |
| Cancellation, process crash, or kill between the commits or during Phase B | After a committed withdrawal, process exit releases the SQLite lock and recovery leaves the public schema unavailable. There is no stale application lock to remove. |
| Commit outcome is uncertain, or the process dies after publication committed but before reporting success | The database may contain a complete new published index. Do not restore old data or invalidate a successful commit merely to match the caller's error. Retrying refresh is safe; exactly-once acknowledgement is not worth adding. |
| OS crash or power failure | Use durable withdrawal/publication commits with `synchronous=EXTRA`. Recovery should preserve a committed unavailable state or a completed new publication under SQLite's storage assumptions. Hardware that lies about syncs and broken filesystems remain outside the guarantee. |
| Full rebuild or schema upgrade fails | The second transaction rolls back DDL, data, and version changes together. Old private objects and their recovery manifest remain unavailable to public queries. Never commit an empty replacement schema first. |
| First-ever build fails | There is no old publication to preserve. An empty/uninitialized file may remain. Retry explicit indexing; do not unlink it automatically while another process may have opened it. |
| Output database would be indexed as source | Reject an output with the indexed language's source extension inside the source tree, before opening it. A stable-path index must not scan its own database as source. |
| Newer/unknown protocol, corrupt catalog, or incompatible database | Diagnose rather than interpret every schema-read error as "old schema, rebuild." Reject unsupported versions and recognizable language mismatches. Automatic salvage or downgrade is out of scope. |
| Legacy `.bak`/`.tmp` leftovers | Reject detected artifacts before opening the index. Rust's `Path::try_exists` follows symlinks and misses dangling ones; Go's `Lstat` rejects them too. Inspect and resolve leftovers manually with legacy writers stopped. They do not prove active ownership and are never automatically restored, overwritten, or removed. |
| User SQL fails after refresh succeeded | Return SQLite's error. The refresh remains committed; an invalid query is not evidence that the index is stale and must not trigger withdrawal. |

## Concurrency concerns

| Case | Handling and remaining limitation |
|---|---|
| Two indexers target the same database | The exclusive SQLite lock spans both transactions. The second writer waits, then inspects the current state after acquiring ownership. No lock-file identity, PID ownership, or inter-commit takeover protocol is needed. |
| Refresh competes with rebuild, upgrade, or initialization | All paths use the same ownership/publication machinery. Repeated explicit rebuild requests may duplicate work; coalescing them is deferred. |
| A new ordinary SQLite reader arrives during preflight, either phase, between commits, or after publication but before close | It waits on the retained exclusive lock, up to its busy timeout. After normal release it sees the confirmed unchanged snapshot, a complete publication, or an unavailable public schema. Abrupt death before withdrawal has the documented exception. A reader with no timeout can fail immediately with `SQLITE_BUSY`. |
| An active read transaction exists before the writer starts | The writer waits for it to finish. A sufficiently long read can exhaust the writer's acquisition timeout before withdrawal; the old schema can remain. The protocol does not kill readers or promise unlimited waiting. |
| An idle interactive SQLite connection remains open | It does not block the writer merely by existing. Its next read during refresh waits. This avoids the deployed shared-`flock` behavior of reserving read access for the entire client process lifetime. |
| A client caches prepared statements | Schema publication changes the schema cookie. Normal clients reprepare as needed; lower-level clients may need to handle `SQLITE_SCHEMA`. They must not assume prepared statements survive every refresh unchanged. |
| Multiple statements require a single generation | The caller uses an explicit read transaction. Separate autocommit statements may observe different completed generations. Do not automatically wrap arbitrary user SQL scripts. |
| Another writer starts after the wrapper's refresh returns but before its query begins | The query can wait through another refresh or time out. Refresh and arbitrary external CLI execution are not one atomic operation, and there is no fairness guarantee favoring the just-refreshed caller. |
| Files change while being scanned or parsed | Database atomicity does not freeze source files. Use consistent per-file bytes/metadata and reject detected races. Files added after enumeration may appear next time. Whole-tree snapshots and mandatory content hashing are not part of this change. |
| Content changes without detection by the mtime/size heuristic | Not solved by the publication protocol. Retain explicit full rebuild as recovery; hashing every file is a separate performance/semantics decision, not a prerequisite for this design. |
| Different databases or path aliases are used | Different databases do not share a global lock. Normalize database paths and rely on SQLite's locking, not runtime-directory or UID-based keys. Hard-linked aliases and live symlink retargeting remain unsupported. |
| Different projects/languages share one output, or external SQL mutates index-owned tables | SQLite serializes access but does not establish semantic ownership or validate arbitrary modifications. Check language where metadata permits; one project/language per database remains the contract. Do not add a general ownership registry. |
| Legacy binaries rename files, or another program replaces/unlinks the database | Unsupported during operation. SQLite cannot coordinate pathname replacement performed outside its locking protocol. Quiescent rollout is required. |
| Clients use `immutable=1`, disabled locking, or a filesystem with broken SQLite locking | Outside the supported live-index contract. Do not build distributed locking or defenses against arbitrary file overwrites into this local index. |
| Many refresh-before-query calls arrive together | Work on one database is serialized. Readers and writers can time out or experience long tail latency; SQLite does not provide application-level FIFO scheduling. No daemon, retry storm, or fairness scheduler is proposed. |

### Waiting policy

Retain the existing five-second busy timeout as the initial default, but describe
it honestly: **waiting is bounded, not guaranteed to cover every full rebuild**.
There are separate writer-acquisition and reader waits; changing a SQLite
client's timeout does not change the indexer's acquisition timeout.

A command's total duration can exceed five seconds because indexing and
multiple lock waits are separate operations. Do not add unbounded application
retry loops. Timeout configuration can be considered if representative use
requires it; it is not necessary to establish the publication protocol.

## Alternatives and relative implementation cost

Effort ratings are relative engineering scope, not elapsed-time commitments.
Latency statements below are structural estimates, not benchmark results.

| Approach | Fail-closed and reader coverage | Implementation effort | Ongoing complexity | End-user cost |
|---|---|---|---|---|
| Committed filesystem `.bak`/`.tmp` protocol without a real writer lock | Correct intent, insufficient coordination. Placeholder databases and shared work filenames can interfere with ownership and publication. | None if left unchanged, but not an acceptable endpoint. | Small code surface with implicit, timing-sensitive states. | Low happy-path overhead; unpredictable failures, lost work, and forced rebuilds. |
| Hardened OS/file locking plus filesystem withdrawal | Can preserve withdrawal-on-failure for a cooperating deployment. Every writer and relevant reader must use the same protocol; arbitrary SQLite clients do not honor that separate lock. | Low to medium; smaller than the SQLite-only refactor. Still requires indexer-owned locks, reliable identities, error handling, and recovery rules, not just copying a helper. | Two coordination layers: SQLite transactions and application locks/publication files. Platform/path handling and nonparticipating clients remain concerns. | Few lock and rename operations, generally lower fixed no-op overhead. Waiting is similar when all participants cooperate. Lifetime shared reader locks can make idle clients block refresh. |
| SQLite-only exclusive connection plus schema withdrawal | Enforces waiting and unavailable public names for ordinary lock-respecting SQLite clients. Has the explicit pre-withdrawal failure boundary above. | Medium to high initially. Go connection/transaction plumbing and safe catalog withdrawal/restoration are the largest changes. | One lock system, a durable publication state, and catalog recovery. No separate lock files or lock-owner cleanup. | No-op preflight is read-only. Changes/full rebuilds pay for two durable commits and schema churn; readers wait through scanning and refresh work. No whole-index data copy for incremental withdrawal. |
| Private immutable generations plus coordinated filesystem publication | Can separate working data from published snapshots, with a writer lock and a fail-closed public pointer. Existing handles still reference old generations. A read-only publication policy is needed to avoid arbitrary writers modifying supposedly immutable files. | High; a larger storage and lifecycle redesign. | Generation naming, copying/building, publication, recovery, and reclamation; read/write API implications. | Ordinary copying costs scale with database size, or require filesystem-specific copy-on-write support. Keeping the public pointer withdrawn preserves failure policy but still creates a period of unavailability/waiting. |

Ordinary in-place transactions plus a wrapper-only freshness check would be
cheaper than schema withdrawal, but leave the old public tables readable after
failure. That is a contract change, not an equivalent low-cost implementation.
A service could mediate all reads and writes, but would change the standalone
SQLite product and add deployment/operational complexity without a demonstrated
need.

## Cost of the recommended approach

### No-op versus work-bearing refresh

For an already published index with `T` top-level application tables and `V`
views, a refresh that needs withdrawal/publication adds roughly:

- `2T` table renames, plus FTS5's own shadow-object handling;
- `V` view drops and `V` view creations;
- small control/manifest writes and two write-transaction commits;
- schema reparsing and possible reprepare work in clients.

The current product schemas have 11 such tables and five views for Rust, and
12 tables and three views for Go, before any user additions or protocol metadata.
Ordinary renames change schema metadata, not every stored row; cost grows with
schema complexity rather than directly with indexed source volume.

A no-op avoids all of those writes. It still opens/configures a connection,
acquires the exclusive lock, reads index metadata, traverses source paths, and
checks file metadata. The wrapper also pays for a layout probe and,
for legacy filenames, a language probe. Readers remain excluded during the scan.

On a change, preflight stops early; the work phase starts a complete traversal.
This can repeat most of a scan when the first change is encountered late.
The optimization deliberately trades a larger pre-withdrawal crash window for
lower routine no-op overhead; caught scan errors still attempt withdrawal.

### Refresh and rebuild work

Directory traversal, parsing, Go package/type loading, extraction, and changed-row
updates remain. They execute while readers are excluded, not merely during a
short final commit window.

Full rebuilds gain one transaction around their schema and insertion work.
That can reduce existing per-statement transaction overhead; stronger durability
does not by itself prove every full rebuild will be slower. Actual effects need
measurement on representative projects.

Incremental publication does not require another full copy of the database.
Rollback journals still need space for modified pages, and a rebuild can need
substantial journal and growth space. An in-place rebuild also does not
automatically produce a compact new file: freed pages may remain reusable in
the database. Automatic `VACUUM` on every refresh is not proposed.

Manifest memory is proportional to schema definitions. Go retains source bytes
for changed files and passes them to the loader as an overlay, adding memory
and loader I/O proportional to those bytes. A full Go build captures all indexed
files for this purpose. Rust processes source buffers one file at a time.

### Latency and contention

Let:

- `W` be time spent acquiring the initial exclusive lock;
- `S` be the preflight scan, if applicable;
- `I` be withdrawal work and its durable commit;
- `F` be source discovery, parsing, and data work;
- `P` be publication work, its commit, and connection release.

For work-bearing refreshes, latency is approximately `W + S + I + F + P`.
The lock hold time is approximately `S + I + F + P`. A no-op pays only
`W + S` and connection release. A new reader can wait for the remaining hold time,
or longer if other writers acquire ownership first, up to its own waiting policy.

Parallel refresh-before-query calls do not parallelize work on the same index.
In the unfavorable case, tail latency approaches several serialized refresh
durations. Timeouts are therefore part of normal supported failure behavior,
not evidence that stale fallback should be enabled.

Safe initial reductions in overhead are batching DDL inside the two transactions,
using one connection, avoiding duplicate source reads, and avoiding unnecessary
aggregate recomputation. Naturally batching related SQL queries reduces repeated
refresh startup, but is optional usage guidance, not a tool-use restriction.

### Where implementation effort goes

| Area | Relative effort | Main source of complexity |
|---|---|---|
| Rust orchestration | Medium | Connection lifetime, two transaction scopes, splitting schema configuration from DDL, and reliable error propagation. Existing connection-based helpers are comparatively easy to reuse. |
| Go orchestration and helpers | Medium to high | Reserving one physical connection across commits, transaction-compatible extraction helpers, failed-commit cleanup, and avoiding pool reuse while exclusive locks remain held. |
| Catalog publication and recovery | Medium | Complete view withdrawal, FTS5 handling, durable manifests, namespace collisions, and interrupted schema upgrades. |
| Wrapper and deployment | Low to medium | Persistent language discovery, busy handling for probes, explicit unavailable/missing-index behavior, and retiring mixed legacy writers. |
| Focused failure/concurrency coverage | Medium | Deterministic synchronization, writer death, rollback of schema/data, retained lock lifetime, and ordinary SQLite reader behavior. |

Before choosing performance optimizations or new timeout controls, compare
no-op refresh, a small edit, and a full rebuild with single and concurrent query
clients on representative projects. Record elapsed time, lock waits, failures,
and disk growth. This calls for small measurements using existing tooling, not
a benchmark framework or mandatory performance gate.

## Deliberate non-goals

- No claim of perfect source freshness under continuous edits or invisible
  timestamp/size changes.
- No automatic publication of a last-good snapshot after failure.
- No transparent indefinite waiting, writer coalescing, fairness service, or daemon.
- No supported mixed operation with old filesystem-renaming indexers.
- No distributed locking, arbitrary-file-corruption defense, or automatic
  salvage of unknown/corrupt databases.
- No preservation guarantee for arbitrary user additions across a full rebuild.
- No new complexity/coverage gates or changes to cognitive/dead-code semantics.

## References

- [SQLite locking mode](https://www.sqlite.org/pragma.html#pragma_locking_mode):
  retained connection locks, and why switching to `NORMAL` alone does not release
  them until a subsequent database access.
- [SQLite transactions](https://www.sqlite.org/lang_transaction.html):
  exclusive transactions, commit failures, and rollback behavior.
- [SQLite ALTER TABLE](https://www.sqlite.org/lang_altertable.html):
  rename behavior, retained indexes/triggers, automatic view rewriting, and
  metadata rather than row copying.
- [SQLite synchronous settings](https://www.sqlite.org/pragma.html#pragma_synchronous):
  withdrawal-commit durability and the `FULL`/`EXTRA` distinction.
- [SQLite busy timeout](https://www.sqlite.org/pragma.html#pragma_busy_timeout)
  and [isolation](https://www.sqlite.org/isolation.html).
- [SQLite file-locking and rename hazards](https://www.sqlite.org/howtocorrupt.html).
- [modernc SQLite driver options](https://pkg.go.dev/modernc.org/sqlite@v1.50.0#Driver.Open):
  `_txlock=exclusive` and connection setup.
- [Go connection close](https://pkg.go.dev/database/sql#Conn.Close)
  and [transaction API](https://pkg.go.dev/database/sql#Tx).
