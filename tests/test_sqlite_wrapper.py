"""Real-CLI regression tests; run with make wrapper-test."""

import concurrent.futures
import contextlib
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parent.parent
WRAPPER = ROOT / "bin" / "sqlite3"


class SQLiteWrapperTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="indexnudge-wrapper-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.env = dict(os.environ)
        self.env["PATH"] = os.pathsep.join(
            [str(ROOT / "bin-rust"), str(ROOT / "bin-go"), self.env["PATH"]]
        )

    def command(self, args, *, success=True, env=None):
        result = subprocess.run(
            [str(arg) for arg in args],
            env=env or self.env,
            capture_output=True,
            text=True,
            timeout=30,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertEqual(result.stdout, "", "failure emitted query output")
        return result

    def index_command(self, language, directory, database):
        executable = ROOT / f"bin-{language}" / "indexnudge"
        if language == "rust":
            return [executable, directory, "--output", database]
        return [executable, "-dir", directory, "-output", database]

    def write_source(self, language, directory, name):
        if language == "rust":
            (directory / "lib.rs").write_text(f"fn {name}() {{}}\n")
        else:
            (directory / "lib.go").write_text(
                f"package fixture\nfunc {name}() {{}}\n"
            )

    def project(self, language, *, legacy=False, filename=None):
        directory = self.root / language
        directory.mkdir()
        self.write_source(language, directory, "old_name")
        if language == "go":
            (directory / "go.mod").write_text("module fixture\n\ngo 1.25\n")
        database = directory / (filename or (
            ".indexnudge.db" if legacy else f".indexnudge-{language}.db"
        ))
        result = self.command(self.index_command(language, directory, database))
        self.assertEqual(result.stdout, "")
        return directory, database

    def query_command(self, database, sql):
        return [WRAPPER, "-batch", "-init", "/dev/null", database, sql]

    def execute(self, database, sql):
        with contextlib.closing(sqlite3.connect(database)) as connection:
            result = connection.execute(sql).fetchall()
            connection.commit()
            return result

    def test_refresh_and_output_options(self):
        for language in ("rust", "go"):
            with self.subTest(language=language):
                directory, database = self.project(language)
                self.write_source(language, directory, "new_name")
                args = [
                    WRAPPER, "-init", "/dev/null", "-lookaside", "0", "0",
                    "-header", "-csv", database,
                    "SELECT name FROM symbols ORDER BY name;",
                ]
                self.assertEqual(self.command(args).stdout, "name\nnew_name\n")
                self.assertFalse(Path(str(database) + ".bak").exists())
                self.assertFalse(Path(str(database) + ".tmp").exists())

    def test_output_cannot_be_indexed_as_source(self):
        for language, extension in (("rust", "rs"), ("go", "go")):
            with self.subTest(language=language):
                directory, _ = self.project(language)
                database = directory / f"index.{extension}"
                result = self.command(
                    self.index_command(language, directory, database), success=False
                )
                self.assertIn("inside the source tree", result.stderr)
                self.assertFalse(database.exists())

    def test_failed_legacy_named_index_recovers(self):
        for language in ("rust", "go"):
            with self.subTest(language=language):
                directory, database = self.project(language, legacy=True)
                self.execute(database, """CREATE TRIGGER reject_insert
                    BEFORE INSERT ON files BEGIN
                    SELECT RAISE(ABORT, 'injected insert failure'); END""")
                self.write_source(language, directory, "new_name")
                args = self.query_command(database, "SELECT name FROM symbols;")
                failed = self.command(args, success=False)
                self.assertIn("injected insert failure", failed.stderr)
                with self.assertRaisesRegex(sqlite3.OperationalError, "no such table"):
                    self.execute(database, "SELECT name FROM symbols")
                self.assertEqual(
                    self.execute(database, "SELECT language, status FROM _indexnudge_state"),
                    [(language, "unavailable")],
                )
                self.execute(database, "DROP TRIGGER reject_insert")
                self.assertEqual(self.command(args).stdout, "new_name\n")

    def test_probe_waits_across_commits(self):
        for language in ("rust", "go"):
            with self.subTest(language=language):
                directory, database = self.project(language, legacy=True)
                self.write_source(language, directory, "new_name")
                with contextlib.closing(sqlite3.connect(database, isolation_level=None)) as blocker:
                    blocker.execute("PRAGMA locking_mode=EXCLUSIVE")
                    blocker.execute("BEGIN EXCLUSIVE")
                    process = subprocess.Popen(
                        [str(arg) for arg in self.query_command(
                            database, "SELECT name FROM symbols;"
                        )],
                        env=self.env, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE, text=True,
                    )
                    try:
                        for statement in ("COMMIT", "BEGIN EXCLUSIVE", "COMMIT"):
                            blocker.execute(statement)
                            time.sleep(0.05)
                            self.assertIsNone(process.poll(), "query bypassed retained lock")
                        blocker.close()
                        stdout, stderr = process.communicate(timeout=15)
                        self.assertEqual(process.returncode, 0, stderr)
                        self.assertEqual(stdout, "new_name\n")
                    finally:
                        if process.poll() is None:
                            process.kill()
                        process.communicate()

    def test_parallel_wrappers_and_direct_rebuild(self):
        for language in ("rust", "go"):
            with self.subTest(language=language):
                directory, database = self.project(language)
                self.write_source(language, directory, "new_name")
                query = self.query_command(database, "SELECT name FROM symbols;")
                with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                    queries = [pool.submit(self.command, query) for _ in range(3)]
                    rebuild = pool.submit(
                        self.command, self.index_command(language, directory, database)
                    )
                    for result in queries:
                        self.assertEqual(result.result().stdout, "new_name\n")
                    self.assertEqual(rebuild.result().stdout, "")

    def test_missing_empty_and_non_index_arguments(self):
        database = self.root / ".indexnudge-rust.db"
        args = self.query_command(database, "SELECT 1;")
        self.command(args, success=False)
        self.assertFalse(database.exists(), "missing index was created by a query")
        database.touch()
        self.command(args, success=False)
        self.assertEqual(database.stat().st_size, 0)
        args = [
            WRAPPER, "-init", "/dev/null",
            "-cmd", ".print .indexnudge-option.db",
            ":memory:", ".print .indexnudge-query.db",
        ]
        self.assertEqual(
            self.command(args).stdout,
            ".indexnudge-option.db\n.indexnudge-query.db\n",
        )

    def test_hot_journal_recovery_before_probe(self):
        for language in ("rust", "go"):
            with self.subTest(language=language):
                directory, database = self.project(language, legacy=True)
                self.write_source(language, directory, "new_name")
                child = subprocess.Popen(
                    [sys.executable, "-c", """
import sqlite3, sys
db = sqlite3.connect(sys.argv[1], isolation_level=None)
db.execute("PRAGMA cache_size=1")
db.execute("BEGIN EXCLUSIVE")
db.execute("UPDATE symbols SET name='uncommitted'")
db.execute("UPDATE files SET indexed_size=0")
print("LOCKED", flush=True)
sys.stdin.read(1)
""", str(database)],
                    stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE, text=True,
                )
                try:
                    self.assertEqual(child.stdout.readline().strip(), "LOCKED")
                    child.kill()
                    child.communicate(timeout=10)
                    args = self.query_command(database, "SELECT name FROM symbols;")
                    self.assertEqual(self.command(args).stdout, "new_name\n")
                finally:
                    if child.poll() is None:
                        child.kill()
                    child.communicate()

    def test_database_symlink_and_probe_ignores_sqliterc(self):
        directory, database = self.project("rust", filename=".indexnudge #?%.db")
        alias = self.root / ".indexnudge-alias.db"
        alias.symlink_to(database)
        home = self.root / "home"
        home.mkdir()
        (home / ".sqliterc").write_text(".headers on\n.mode json\n")
        env = dict(self.env, HOME=str(home))
        self.write_source("rust", directory, "new_name")
        args = self.query_command(alias, "SELECT name FROM symbols;")
        self.assertEqual(self.command(args, env=env).stdout, "new_name\n")
        self.assertTrue(alias.is_symlink(), "refresh replaced the database symlink")
        args = self.query_command(database.as_uri(), "SELECT name FROM symbols;")
        self.assertEqual(self.command(args).stdout, "new_name\n")


if __name__ == "__main__":
    unittest.main()
