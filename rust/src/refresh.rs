use super::{
    FileStamp, SCHEMA_VERSION, collect_rs_files, create_schema, delete_file_rows, file_stamp,
    finalize, index_file, visit_rs_files,
};
use anyhow::{Context, Result, bail, ensure};
use rusqlite::{Connection, TransactionBehavior};
use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tree_sitter::Parser;

const PRIVATE_PREFIX: &str = "_indexnudge_";
const OLD_PREFIX: &str = "_indexnudge_old_";
const CONTROL_SCHEMA: &str = "
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
    INSERT INTO _indexnudge_state VALUES (1, 1, 'rust', 'unavailable');
";

struct State {
    new: bool,
    managed: bool,
    available: bool,
    version: i64,
}

struct Object {
    name: String,
    kind: String,
    definition: Option<String>,
}

#[derive(Default)]
struct Outcome {
    rebuilt: bool,
    reindexed: usize,
    removed: usize,
}

pub(super) fn run(dir: &Path, db_path: &Path, incremental: bool) -> Result<()> {
    let path = database_path(db_path)?;
    ensure!(
        !(path.starts_with(dir) && path.extension().is_some_and(|ext| ext == "rs")),
        "a database inside the source tree must not use the .rs extension"
    );
    for artifact in [
        PathBuf::from(format!("{}.bak", path.display())),
        PathBuf::from(format!("{}.tmp", path.display())),
        path.with_extension("db.bak"),
        path.with_extension("db.tmp"),
    ] {
        ensure!(
            !artifact.try_exists()?,
            "legacy artifact {} exists; stop old indexers and resolve it before indexing",
            artifact.display()
        );
    }
    let mut conn = open_writer(&path)?;
    let result = run_locked(&mut conn, dir, incremental);
    let rollback = if conn.is_autocommit() {
        Ok(())
    } else {
        conn.execute_batch("ROLLBACK")
            .context("rolling back refresh")
    };
    let close = conn.close().map_err(|(_, error)| anyhow::Error::new(error));
    let cleanup = match (rollback, close) {
        (Err(error), Err(close)) => Err(error.context(format!("closing database: {close:#}"))),
        (Err(error), _) | (_, Err(error)) => Err(error),
        _ => Ok(()),
    };
    let outcome = match (result, cleanup) {
        (Err(error), Err(cleanup)) => {
            return Err(error.context(format!("cleanup failed: {cleanup:#}")));
        }
        (Err(error), _) | (_, Err(error)) => return Err(error),
        (Ok(outcome), Ok(())) => outcome,
    };
    if outcome.rebuilt {
        eprintln!("Done. Database: {}", path.display());
    } else if outcome.reindexed != 0 || outcome.removed != 0 {
        eprintln!(
            "Freshened: {} reindexed, {} removed.",
            outcome.reindexed, outcome.removed
        );
    }
    Ok(())
}

fn database_path(path: &Path) -> Result<PathBuf> {
    match fs::symlink_metadata(path) {
        Ok(_) => path.canonicalize().context("resolving database path"),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            let parent = path
                .parent()
                .filter(|p| !p.as_os_str().is_empty())
                .unwrap_or(Path::new("."));
            let name = path.file_name().context("database path has no filename")?;
            Ok(parent.canonicalize()?.join(name))
        }
        Err(error) => Err(error.into()),
    }
}

fn open_writer(path: &Path) -> Result<Connection> {
    let conn = Connection::open(path)?;
    conn.busy_timeout(Duration::from_secs(5))?;
    let mode: String = conn.query_row("PRAGMA journal_mode", [], |row| row.get(0))?;
    ensure!(
        mode == "delete",
        "unsupported journal mode {mode}; convert to DELETE with all clients stopped"
    );
    conn.execute_batch("PRAGMA synchronous=EXTRA; PRAGMA locking_mode=EXCLUSIVE;")?;
    Ok(conn)
}

fn run_locked(conn: &mut Connection, dir: &Path, incremental: bool) -> Result<Outcome> {
    let tx = conn.transaction_with_behavior(TransactionBehavior::Exclusive)?;
    let state = inspect(&tx)?;
    let rebuild = !incremental || state.new || !state.managed || state.version != SCHEMA_VERSION;

    if state.new {
        tx.execute_batch(CONTROL_SCHEMA)?;
        create_schema(&tx)?;
        let outcome = update_files(&tx, dir, true)?;
        publish(&tx)?;
        tx.commit()?;
        return Ok(outcome);
    }

    if state.available {
        let scan = if rebuild {
            Ok(true)
        } else {
            has_changes(&tx, dir)
        };
        if matches!(scan, Ok(false)) {
            tx.rollback()?;
            return Ok(Outcome::default());
        }
        // Even a failed preflight withdraws before releasing the exclusive lock.
        let withdrawal =
            withdraw(&tx, state.managed).and_then(|()| tx.commit().map_err(Into::into));
        if let Err(error) = scan {
            return match withdrawal {
                Ok(()) => Err(error.context("scan failed; index withdrawn")),
                Err(withdrawal) => {
                    Err(error.context(format!("scan failed and withdrawal failed: {withdrawal:#}")))
                }
            };
        }
        withdrawal?;
    } else {
        tx.rollback()?;
    }

    let tx = conn.transaction_with_behavior(TransactionBehavior::Exclusive)?;
    if rebuild {
        discard_private(&tx)?;
        create_schema(&tx)?;
    } else {
        restore(&tx)?;
    }
    let outcome = update_files(&tx, dir, rebuild)?;
    publish(&tx)?;
    tx.commit()?;
    Ok(outcome)
}

fn inspect(conn: &Connection) -> Result<State> {
    let version = conn.query_row("PRAGMA user_version", [], |row| row.get(0))?;
    ensure!(
        (0..=SCHEMA_VERSION).contains(&version),
        "unsupported schema version {version} (maximum {SCHEMA_VERSION})"
    );
    let managed: bool = conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='_indexnudge_state')",
        [],
        |row| row.get(0),
    )?;
    if managed {
        let (protocol, language, status): (i64, String, String) = conn.query_row(
            "SELECT protocol, language, status FROM _indexnudge_state WHERE id=1",
            [],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
        )?;
        ensure!(protocol == 1, "unsupported publication protocol {protocol}");
        ensure!(
            language == "rust",
            "expected a Rust index, found {language}"
        );
        ensure!(
            status == "published" || status == "unavailable",
            "invalid publication state {status}"
        );
        if status == "published" {
            let pending: i64 =
                conn.query_row("SELECT count(*) FROM _indexnudge_objects", [], |row| {
                    row.get(0)
                })?;
            ensure!(
                pending == 0,
                "published index has a pending object manifest"
            );
        }
        return Ok(State {
            new: false,
            managed,
            available: status == "published",
            version,
        });
    }
    let mut stmt = conn.prepare("SELECT name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")?;
    let names: Vec<String> = stmt
        .query_map([], |row| row.get(0))?
        .collect::<rusqlite::Result<_>>()?;
    ensure!(
        !names
            .iter()
            .any(|name| name.to_ascii_lowercase().starts_with(PRIVATE_PREFIX)),
        "database contains reserved _indexnudge_ objects without publication metadata"
    );
    if names.is_empty() {
        return Ok(State {
            new: true,
            managed,
            available: false,
            version,
        });
    }
    ensure!(
        ["files", "lines", "symbols"]
            .iter()
            .all(|name| names.iter().any(|n| n == name)),
        "database is not a recognized indexnudge index"
    );
    let invalid_language: bool = conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM files WHERE language IS NULL OR language != 'rust')",
        [],
        |row| row.get(0),
    )?;
    ensure!(!invalid_language, "database contains non-Rust files");
    Ok(State {
        new: false,
        managed,
        available: true,
        version,
    })
}

fn identifier(name: &str) -> String {
    format!("\"{}\"", name.replace('"', "\"\""))
}

fn withdraw(conn: &Connection, managed: bool) -> Result<()> {
    let mut stmt = conn.prepare(
        "SELECT s.name, s.type, s.sql FROM sqlite_schema s
         JOIN pragma_table_list t ON t.schema='main' AND t.name=s.name
         WHERE s.type IN ('table','view') AND t.type != 'shadow'
           AND s.name NOT GLOB 'sqlite_*' ORDER BY s.name",
    )?;
    let objects = stmt
        .query_map([], |row| {
            Ok(Object {
                name: row.get(0)?,
                kind: row.get(1)?,
                definition: row.get(2)?,
            })
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    drop(stmt);
    let mut public = Vec::new();
    for object in objects {
        if object.name == "_indexnudge_state" || object.name == "_indexnudge_objects" {
            continue;
        }
        ensure!(
            !object.name.to_ascii_lowercase().starts_with(PRIVATE_PREFIX),
            "reserved object {} exists in published index",
            object.name
        );
        public.push(object);
    }
    if !managed {
        conn.execute_batch(CONTROL_SCHEMA)?;
    }
    let pending: i64 = conn.query_row("SELECT count(*) FROM _indexnudge_objects", [], |row| {
        row.get(0)
    })?;
    ensure!(
        pending == 0,
        "published index has a pending object manifest"
    );
    for object in &public {
        conn.execute(
            "INSERT INTO _indexnudge_objects (name, kind, definition) VALUES (?1, ?2, ?3)",
            rusqlite::params![object.name, object.kind, object.definition],
        )?;
        if object.kind == "view" {
            conn.execute_batch(&format!("DROP VIEW {}", identifier(&object.name)))?;
        }
    }
    for object in &public {
        if object.kind == "table" {
            conn.execute_batch(&format!(
                "ALTER TABLE {} RENAME TO {}",
                identifier(&object.name),
                identifier(&format!("{OLD_PREFIX}{}", object.name))
            ))?;
        }
    }
    conn.execute(
        "UPDATE _indexnudge_state SET status='unavailable' WHERE id=1",
        [],
    )?;
    Ok(())
}

fn manifest(conn: &Connection) -> Result<Vec<Object>> {
    let mut stmt =
        conn.prepare("SELECT name, kind, definition FROM _indexnudge_objects ORDER BY kind, name")?;
    let objects = stmt
        .query_map([], |row| {
            Ok(Object {
                name: row.get(0)?,
                kind: row.get(1)?,
                definition: row.get(2)?,
            })
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    ensure!(
        !objects.is_empty(),
        "unavailable index has no recovery manifest"
    );
    for object in &objects {
        ensure!(
            !object.name.to_ascii_lowercase().starts_with(PRIVATE_PREFIX)
                && (object.kind == "table" || object.kind == "view"),
            "invalid recovery object {}",
            object.name
        );
    }
    Ok(objects)
}

fn restore(conn: &Connection) -> Result<()> {
    for object in manifest(conn)? {
        match object.kind.as_str() {
            "table" => conn.execute_batch(&format!(
                "ALTER TABLE {} RENAME TO {}",
                identifier(&format!("{OLD_PREFIX}{}", object.name)),
                identifier(&object.name)
            ))?,
            "view" => conn.execute_batch(
                object
                    .definition
                    .as_deref()
                    .context("view definition missing")?,
            )?,
            _ => bail!("invalid recovery object {}", object.name),
        }
    }
    Ok(())
}

fn discard_private(conn: &Connection) -> Result<()> {
    for object in manifest(conn)? {
        if object.kind == "table" {
            conn.execute_batch(&format!(
                "DROP TABLE {}",
                identifier(&format!("{OLD_PREFIX}{}", object.name))
            ))?;
        }
    }
    Ok(())
}

fn publish(conn: &Connection) -> Result<()> {
    conn.pragma_update(None, "user_version", SCHEMA_VERSION)?;
    conn.execute_batch(
        "DELETE FROM _indexnudge_objects;
        UPDATE _indexnudge_state SET status='published' WHERE id=1;",
    )?;
    Ok(())
}

fn indexed_files(conn: &Connection) -> Result<HashMap<String, FileStamp>> {
    let mut stmt = conn.prepare("SELECT file, indexed_mtime, indexed_size FROM files")?;
    Ok(stmt
        .query_map([], |row| {
            Ok((
                row.get(0)?,
                FileStamp {
                    mtime: row.get(1)?,
                    size: row.get(2)?,
                },
            ))
        })?
        .collect::<rusqlite::Result<_>>()?)
}

fn has_changes(conn: &Connection, dir: &Path) -> Result<bool> {
    let mut remaining = indexed_files(conn)?;
    let changed = visit_rs_files(dir, |path| {
        let rel = path.strip_prefix(dir)?.to_string_lossy();
        Ok(remaining.remove(rel.as_ref()) != Some(file_stamp(path)?))
    })?;
    Ok(changed || !remaining.is_empty())
}

fn update_files(conn: &Connection, dir: &Path, rebuild: bool) -> Result<Outcome> {
    let mut remaining = if rebuild {
        HashMap::new()
    } else {
        indexed_files(conn)?
    };
    let files = collect_rs_files(dir)?;
    if rebuild {
        eprintln!("Indexing {} files...", files.len());
    }
    let mut parser = Parser::new();
    parser.set_language(&tree_sitter_rust::LANGUAGE.into())?;
    let mut indexed = Vec::new();
    for path in &files {
        let rel = path.strip_prefix(dir)?.to_string_lossy();
        let previous = remaining.remove(rel.as_ref());
        if previous != Some(file_stamp(path)?) {
            if previous.is_some() {
                delete_file_rows(conn, &rel)?;
            }
            indexed.push((path, index_file(conn, &mut parser, &rel, path)?));
        }
    }
    for file in remaining.keys() {
        delete_file_rows(conn, file)?;
    }
    if rebuild || !indexed.is_empty() || !remaining.is_empty() {
        conn.execute("DELETE FROM directories", [])?;
        finalize(conn)?;
    }
    for (path, stamp) in &indexed {
        ensure!(
            *stamp == file_stamp(path)?,
            "source changed during refresh: {}",
            path.display()
        );
    }
    Ok(Outcome {
        rebuilt: rebuild,
        reindexed: indexed.len(),
        removed: remaining.len(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("lib.rs"), "fn old_name() {}\n").unwrap();
        let path = dir.path().join(".indexnudge-rust.db");
        run(dir.path(), &path, false).unwrap();
        (dir, path)
    }

    fn count(conn: &Connection, query: &str) -> rusqlite::Result<i64> {
        conn.query_row(query, [], |row| row.get(0))
    }

    fn unavailable(conn: &Connection) {
        for query in [
            "SELECT count(*) FROM files",
            "SELECT count(*) FROM symbols",
            "SELECT count(*) FROM lines_fts",
            "SELECT count(*) FROM callee_candidates",
        ] {
            assert!(
                count(conn, query).is_err(),
                "{query} must not serve old data"
            );
        }
        let status: String = conn
            .query_row("SELECT status FROM _indexnudge_state", [], |row| row.get(0))
            .unwrap();
        assert_eq!(status, "unavailable");
        assert_eq!(
            count(
                conn,
                "SELECT count(*) FROM _indexnudge_old_symbols WHERE name='old_name'"
            )
            .unwrap(),
            1
        );
    }

    #[test]
    fn noop_does_not_write_and_backdated_changes_refresh() {
        let (dir, path) = fixture();
        let reader = Connection::open(&path).unwrap();
        assert_eq!(count(&reader, "SELECT count(*) FROM files").unwrap(), 1);
        let before = fs::read(&path).unwrap();
        run(dir.path(), &path, true).unwrap();
        assert_eq!(
            before,
            fs::read(&path).unwrap(),
            "no-op changed database pages"
        );

        let source = dir.path().join("lib.rs");
        let older = fs::metadata(&source).unwrap().modified().unwrap() - Duration::from_secs(60);
        fs::write(&source, "fn new_name() {}\n").unwrap();
        fs::File::options()
            .write(true)
            .open(&source)
            .unwrap()
            .set_modified(older)
            .unwrap();
        run(dir.path(), &path, true).unwrap();
        assert_eq!(
            count(
                &reader,
                "SELECT count(*) FROM symbols WHERE name='new_name'"
            )
            .unwrap(),
            1
        );
    }

    #[test]
    fn failed_work_withdraws_views_and_fts_then_recovers() {
        let (dir, path) = fixture();
        let reader = Connection::open(&path).unwrap();
        reader
            .execute_batch(
                r#"
            CREATE TABLE "custom ""table" (value TEXT);
            INSERT INTO "custom ""table" VALUES ('keep');
            CREATE VIEW "custom ""view" AS SELECT name FROM symbols;
            CREATE VIEW nested AS SELECT * FROM "custom ""view";
            CREATE TRIGGER reject_insert BEFORE INSERT ON files BEGIN
                SELECT RAISE(ABORT, 'injected insert failure');
            END;
        "#,
            )
            .unwrap();
        fs::write(
            dir.path().join("lib.rs"),
            "fn new_name() {}\nfn extra() {}\n",
        )
        .unwrap();
        let error = run(dir.path(), &path, true).unwrap_err();
        assert!(format!("{error:#}").contains("injected insert failure"));
        unavailable(&reader);
        assert!(count(&reader, "SELECT count(*) FROM nested").is_err());
        assert!(count(&reader, r#"SELECT count(*) FROM "custom ""table""#).is_err());
        assert_eq!(
            count(
                &reader,
                "SELECT count(*) FROM _indexnudge_old_lines_fts WHERE content MATCH 'old_name'"
            )
            .unwrap(),
            1
        );

        reader.execute_batch("DROP TRIGGER reject_insert").unwrap();
        run(dir.path(), &path, true).unwrap();
        assert_eq!(
            count(&reader, "SELECT count(*) FROM nested WHERE name='new_name'").unwrap(),
            1
        );
        assert_eq!(
            count(
                &reader,
                "SELECT count(*) FROM lines_fts WHERE content MATCH 'new_name'"
            )
            .unwrap(),
            1
        );
        assert_eq!(
            count(
                &reader,
                r#"SELECT count(*) FROM "custom ""table" WHERE value='keep'"#
            )
            .unwrap(),
            1
        );
        assert_eq!(
            count(&reader, "SELECT count(*) FROM _indexnudge_objects").unwrap(),
            0
        );
        let integrity: String = reader
            .query_row("PRAGMA integrity_check", [], |row| row.get(0))
            .unwrap();
        assert_eq!(integrity, "ok");
    }

    #[test]
    fn scan_and_rebuild_errors_leave_recoverable_withdrawal() {
        for mode in ["scan", "rebuild", "upgrade"] {
            let (dir, path) = fixture();
            let reader = Connection::open(&path).unwrap();
            if mode == "upgrade" {
                reader.execute_batch("DROP TABLE _indexnudge_objects; DROP TABLE _indexnudge_state; PRAGMA user_version=12").unwrap();
            }
            assert!(run(&dir.path().join("missing"), &path, mode != "rebuild").is_err());
            unavailable(&reader);
            if mode == "upgrade" {
                assert_eq!(count(&reader, "PRAGMA user_version").unwrap(), 12);
            }
            run(dir.path(), &path, true).unwrap();
            assert_eq!(count(&reader, "SELECT count(*) FROM files").unwrap(), 1);
            assert_eq!(
                count(&reader, "PRAGMA user_version").unwrap(),
                SCHEMA_VERSION
            );
        }
    }

    #[test]
    fn exclusive_lock_spans_both_commits() {
        let (dir, path) = fixture();
        let reader = Connection::open(&path).unwrap();
        reader.busy_timeout(Duration::ZERO).unwrap();
        reader.execute_batch("BEGIN; SELECT * FROM files;").unwrap();
        let mut writer = open_writer(&path).unwrap();
        writer.busy_timeout(Duration::ZERO).unwrap();
        assert!(
            writer
                .transaction_with_behavior(TransactionBehavior::Exclusive)
                .is_err()
        );
        writer.close().unwrap();
        reader.execute_batch("COMMIT").unwrap();

        let mut writer = open_writer(&path).unwrap();
        let mut contender = open_writer(&path).unwrap();
        contender.busy_timeout(Duration::ZERO).unwrap();
        let busy = || {
            let error = count(&reader, "SELECT count(*) FROM files").unwrap_err();
            assert_eq!(
                error.sqlite_error_code(),
                Some(rusqlite::ErrorCode::DatabaseBusy)
            );
        };
        let tx = writer
            .transaction_with_behavior(TransactionBehavior::Exclusive)
            .unwrap();
        busy();
        withdraw(&tx, true).unwrap();
        tx.commit().unwrap();
        busy();
        assert!(
            contender
                .transaction_with_behavior(TransactionBehavior::Exclusive)
                .is_err()
        );
        contender.close().unwrap();

        fs::write(dir.path().join("lib.rs"), "fn new_name() {}\n").unwrap();
        let tx = writer
            .transaction_with_behavior(TransactionBehavior::Exclusive)
            .unwrap();
        restore(&tx).unwrap();
        update_files(&tx, dir.path(), false).unwrap();
        publish(&tx).unwrap();
        tx.commit().unwrap();
        busy();
        writer.close().unwrap();
        assert_eq!(
            count(
                &reader,
                "SELECT count(*) FROM symbols WHERE name='new_name'"
            )
            .unwrap(),
            1
        );
    }

    #[test]
    fn rejects_newer_schema_and_wal_without_overwriting() {
        for setup in ["PRAGMA user_version=999", "PRAGMA journal_mode=WAL"] {
            let (dir, path) = fixture();
            let conn = Connection::open(&path).unwrap();
            conn.execute_batch(setup).unwrap();
            conn.close().unwrap();
            assert!(run(dir.path(), &path, true).is_err());
            let reader = Connection::open(&path).unwrap();
            assert_eq!(
                count(
                    &reader,
                    "SELECT count(*) FROM symbols WHERE name='old_name'"
                )
                .unwrap(),
                1
            );
        }
    }

    #[test]
    fn crash_child() {
        use std::io::{Read, Write};
        let Some(path) = std::env::var_os("INDEXNUDGE_CRASH_TEST_DB") else {
            return;
        };
        let mut conn = open_writer(Path::new(&path)).unwrap();
        let tx = conn
            .transaction_with_behavior(TransactionBehavior::Exclusive)
            .unwrap();
        withdraw(&tx, true).unwrap();
        tx.commit().unwrap();
        let tx = conn
            .transaction_with_behavior(TransactionBehavior::Exclusive)
            .unwrap();
        restore(&tx).unwrap();
        tx.execute("UPDATE symbols SET name='uncommitted'", [])
            .unwrap();
        println!("WITHDRAWN");
        std::io::stdout().flush().unwrap();
        std::io::stdin().read_exact(&mut [0]).unwrap();
        panic!("crash test child should have been killed");
    }

    #[test]
    fn killed_writer_leaves_no_public_snapshot() {
        use std::io::{BufRead, BufReader};
        use std::process::{Command, Stdio};
        let (dir, path) = fixture();
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "refresh::tests::crash_child", "--nocapture"])
            .env("INDEXNUDGE_CRASH_TEST_DB", &path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut output = BufReader::new(child.stdout.take().unwrap());
        loop {
            let mut line = String::new();
            assert_ne!(
                output.read_line(&mut line).unwrap(),
                0,
                "child exited before withdrawal"
            );
            if line.trim() == "WITHDRAWN" {
                break;
            }
        }
        child.kill().unwrap();
        assert!(!child.wait().unwrap().success());
        let reader = Connection::open(&path).unwrap();
        unavailable(&reader);
        fs::write(dir.path().join("lib.rs"), "fn new_name() {}\n").unwrap();
        run(dir.path(), &path, true).unwrap();
        assert_eq!(
            count(
                &reader,
                "SELECT count(*) FROM symbols WHERE name='new_name'"
            )
            .unwrap(),
            1
        );
    }
}
