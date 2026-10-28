"""Protocol checks; optional real PostgreSQL catalog check in an owned container."""
import ast
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import types
import unittest
from unittest.mock import patch


source_path = Path(__file__).with_name("credential_verify_program.py")
tree = ast.parse(source_path.read_text())
# Drop executable entrypoint rather than depending on __name__ convention.
tree.body = [node for node in tree.body if not isinstance(node, ast.If)]
program = types.ModuleType("fixture_credential_protocol")
exec(compile(tree, "<closed-credential-protocol-fixture>", "exec"), program.__dict__)


def request():
    h = "a" * 64
    return {
        "schema_version": "compound.credential_verification_request.v0.1",
        "profile_id": "fixture.invalid", "attempt_id": "20261017t130000z-aaaaaaaaaaaaaaaa",
        "program_sha256": h, "effect_set": "existing-local-postgres-maintenance-auth-v1",
        "witness": {"ref": "rec_fixture_metadata", "sha256": h, "observed_at": "2026-10-17T12:59:00Z"},
        "resource": {"account": "000000000000", "region": "fixture-region", "instance": "fixture:instance:example", "address": "10.0.0.10"},
        "ssh": {"alias": "target.fixture.invalid", "user": "fixture", "port": 22,
                "identity_file": "/fixture/identity", "known_hosts_file": "/fixture/known_hosts",
                "known_hosts_sha256": h, "host_key_sha256": "SHA256:" + "A" * 43,
                "interpreter_sha256": h, "client_path": "/usr/lib/postgresql/17/bin/psql",
                "client_sha256": h, "client_version": "psql (PostgreSQL) 17.11", "sudo_sha256": h},
        "host": {"hostname": "host.fixture.invalid", "machine_id": "a" * 32,
                 "boot_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "system_identifier": "1234567890123456789", "version_num": "170011"},
        "file": {"path": "/srv/fixture/.env", "device": 1, "inode": 2, "bytes": 300,
                 "mtime_ns": "1792197133436814154", "ctime_ns": "1792197133436814154", "sha256": h},
        "roles": [{"kind": "application", "key": "APP_URL", "name": "fixture_app", "source_host": "source.fixture.invalid", "source_database": "fixture_db"},
                  {"kind": "migrator", "key": "MIGRATE_URL", "name": "fixture_migrator", "source_host": "source.fixture.invalid", "source_database": "fixture_db"}],
        "database": {"name": "fixture_db", "allow_connections": True},
    }


PASSWORDS = ["fixture-secret-only-A", "fixture-secret-only-B"]


def dotenv(r, passwords=PASSWORDS):
    return ("\n".join(role["key"] + "=postgresql://" + role["name"] + ":" + passwords[index]
                      + "@source.fixture.invalid:5432/fixture_db?sslmode=verify-full"
                      for index, role in enumerate(r["roles"])) + "\n").encode()


def catalog(r, pids):
    return {"system_identifier": r["host"]["system_identifier"], "version_num": r["host"]["version_num"],
            "port": 5432, "privileged": True,
            "roles": [{"name": role["name"], "login": True, "scram_present": True, "superuser": False,
                       "createdb": False, "createrole": False, "replication": False, "bypassrls": False,
                       "inherit": True, "connection_limit": -1, "role_config": None, "valid_until": None,
                       "membership_rows": 0} for role in r["roles"]],
            "database": {"name": r["database"]["name"], "owner": r["roles"][1]["name"], "connection_limit": 0,
                         "allow_connections": True, "application_connect": False, "public_connect": False},
            "sessions": [{"pid": pid, "user": r["roles"][index]["name"], "database": "postgres", "address": "127.0.0.1"}
                         for index, pid in enumerate(pids)]}


class MockRuntime:
    def __init__(self, r):
        self.r = r
        self.raw = dotenv(r)
        self.calls = []
        self.cat_mutate = None
        self.post_raw = self.raw
        self.file_count = 0

    def programs(self, r):
        self.calls.append(("programs",))

    def recheck(self, r):
        self.calls.append(("programs_after",))

    def host(self, r):
        self.calls.append(("host",))
        return copy.deepcopy(r["host"])

    def file(self, r):
        self.file_count += 1
        return self.raw if self.file_count == 1 else self.post_raw

    def catalog(self, r, pids):
        self.calls.append(("catalog", list(pids)))
        value = catalog(r, pids)
        if self.cat_mutate:
            self.cat_mutate(value, bool(pids))
        return value

    def positive(self, r, role, password):
        self.calls.append(("positive", role["name"], password))
        return {"user": role["name"], "session_user": role["name"], "database": "postgres",
                "address": "127.0.0.1", "port": 5432, "version_num": r["host"]["version_num"],
                "pid": 8001 if role["kind"] == "application" else 8002}

    def negative(self, r, role, password):
        self.calls.append(("negative", role["name"], password))


class PureProtocolTests(unittest.TestCase):
    def setUp(self):
        self.no_native = patch.object(program.subprocess, "Popen", side_effect=AssertionError("native_boundary_forbidden"))
        self.no_native.start()
        self.addCleanup(self.no_native.stop)

    def run_mock(self, r=None, runtime=None, generator=None):
        r = r or request()
        runtime = runtime or MockRuntime(r)
        if generator is None:
            values = iter(["fixture-wrong-A", "fixture-wrong-B"])
            generator = lambda _: next(values)
        return program.run_protocol(r, runtime, generator)

    def run_main(self, runtime, cleanup=True, signal_cleanup_failure=False):
        r = runtime.r
        issued = program.time.time_ns()
        wire = {"request": r, "issued_ns": str(issued), "deadline_ns": str(issued + 50_000_000_000)}
        stdout, stderr = io.StringIO(), io.StringIO()
        cleanups = []
        def cleanup_processes():
            cleanups.append(True)
            return cleanup() if callable(cleanup) else cleanup
        processes = types.SimpleNamespace(cleanup=cleanup_processes)
        def timer(_which, seconds):
            if seconds == 0 and signal_cleanup_failure:
                raise RuntimeError("fixture-secret-in-signal-cleanup")
        with patch.object(program.sys, "stdin", types.SimpleNamespace(buffer=io.BytesIO(json.dumps(wire).encode()))), \
             patch.object(program.sys, "stdout", stdout), patch.object(program.sys, "stderr", stderr), \
             patch.object(program.sys, "argv", ["fixture", r["program_sha256"]]), \
             patch.object(program.signal, "signal"), patch.object(program.signal, "setitimer", side_effect=timer), \
             patch.object(program, "Processes", return_value=processes), \
             patch.object(program, "NativeRuntime", return_value=runtime):
            code = program.main()
        self.assertEqual(cleanups, [True])
        return code, stdout.getvalue(), stderr.getvalue()

    def test_main_emits_only_bounded_source_format_stage(self):
        runtime = MockRuntime(request())
        runtime.raw = b"fixture-secret-must-never-be-rendered"
        code, stdout, stderr = self.run_main(runtime)
        self.assertEqual(code, 1)
        self.assertEqual(stdout, "")
        self.assertEqual(stderr, "credential_verify:refused:source_format\n")

    def test_all_failure_stages_are_content_free_and_cleanup_always_runs(self):
        cases = {
            "executable": ("programs", 1), "host_identity": ("host", 1),
            "protected_file": ("file", 1), "pre_catalog": ("catalog", 1),
            "app_existing": ("positive", 1), "app_wrong": ("negative", 1),
            "migrator_existing": ("positive", 2), "migrator_wrong": ("negative", 2),
            "post_catalog": ("catalog", 2), "postchecks": ("file", 2),
        }
        for stage, (method, nth) in cases.items():
            with self.subTest(stage=stage):
                runtime = MockRuntime(request())
                original = getattr(runtime, method)
                calls = []
                def injected(*args):
                    calls.append(True)
                    if len(calls) == nth:
                        raise RuntimeError("fixture-secret-never-render-this")
                    return original(*args)
                with patch.object(runtime, method, side_effect=injected):
                    code, stdout, stderr = self.run_main(runtime)
                self.assertEqual((code, stdout, stderr), (1, "", "credential_verify:refused:" + stage + "\n"))
        runtime = MockRuntime(request())
        runtime.r["effect_set"] = "invalid"
        self.assertEqual(self.run_main(runtime), (1, "", "credential_verify:refused:request_validation\n"))
        runtime = MockRuntime(request())
        with patch.object(runtime, "programs", side_effect=program.StageRefusal("deadline")):
            self.assertEqual(self.run_main(runtime), (1, "", "credential_verify:refused:deadline\n"))

    def test_cleanup_failure_overrides_success_and_other_stage(self):
        for raw, cleanup in ((None, False), (b"fixture-secret", False), (None, lambda: (_ for _ in ()).throw(RuntimeError("fixture-secret")))):
            with self.subTest(raw=raw, cleanup=cleanup):
                runtime = MockRuntime(request())
                if raw is not None:
                    runtime.raw = raw
                self.assertEqual(self.run_main(runtime, cleanup), (1, "", "credential_verify:refused:cleanup\n"))
        self.assertEqual(self.run_main(MockRuntime(request()), signal_cleanup_failure=True),
                         (1, "", "credential_verify:refused:cleanup\n"))

    def test_process_deadline_has_only_finite_stage(self):
        with self.assertRaises(program.StageRefusal) as caught:
            program.Processes(program.time.monotonic() - 1).remaining()
        self.assertEqual(caught.exception.stage, "deadline")

    def test_main_success_has_no_stage_or_diagnostic(self):
        code, stdout, stderr = self.run_main(MockRuntime(request()))
        self.assertEqual((code, stderr), (0, ""))
        observation = json.loads(stdout)
        self.assertNotIn("failure_stage", observation)
        for secret in PASSWORDS:
            self.assertNotIn(secret, stdout)

    def test_source_uri_grammar_remains_closed(self):
        r = request()
        raw = dotenv(r)
        changes = [
            (b"postgresql://", b"postgresql+asyncpg://"), (b":5432/", b"/"),
            (b":5432/", b":5433/"), (b"source.fixture.invalid", b"other.fixture.invalid"),
            (r["roles"][0]["name"].encode() + b":", b"other_role:"),
            (b"/fixture_db?", b"/other_db?"),
        ]
        changes += [(b"sslmode=verify-full", (key + "=synthetic").encode()) for key in
                    ("host", "hostaddr", "port", "dbname", "user", "password", "service", "passfile", "options")]
        changes.append((b"sslmode=verify-full", b"sslmode=verify-full&sslmode=require"))
        for before, after in changes:
            with self.subTest(after=after), self.assertRaises(program.Refusal):
                program.parse_passwords(raw.replace(before, after, 1), r["roles"])
        self.assertEqual(program.parse_passwords(raw.replace(b"postgresql://", b"postgres://"), r["roles"]), PASSWORDS)


    def test_client_version_accepts_only_bounded_bare_or_debian_metadata(self):
        valid = ['psql (PostgreSQL) 17.11', 'psql (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)', 'psql (PostgreSQL) 18.2 (Debian 18.2-2.pgdg13+1~fixture1)', 'psql (PostgreSQL) 17.11 (Debian 17.11-1111111111111111111111111111111111111111111111111111111111111111)']
        invalid = ['psql (PostgreSQL) 17.11 (Ubuntu 17.11-1)', 'psql (PostgreSQL) 17.11 (Debian 18.2-1.pgdg12+2)', 'psql (PostgreSQL) 17.11 (Debian 17.11-)', 'psql (PostgreSQL) 17.11 (Debian 17.11-1xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx)', 'psql (PostgreSQL) 17.11 (Debian 17.11-1);command', 'psql (PostgreSQL) 17.11 (Debian 17.11-1"x)', 'psql (PostgreSQL) 17.11 (Debian 17.11-1\\x)', 'psql (PostgreSQL) 17.11\n', 'psql (PostgreSQL) 17.11\r\n', 'psql (PostgreSQL) 17.11 (Debian 17.11-1\x00x)', 'psql (PostgreSQL) 17.11\nextra output', 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx']
        for value in valid:
            r = request()
            r["ssh"]["client_version"] = value
            with self.subTest(valid=value):
                program.validate_request(r)
        for value in invalid:
            r = request()
            r["ssh"]["client_version"] = value
            with self.subTest(invalid=value), self.assertRaises(program.Refusal):
                program.validate_request(r)

    def test_client_version_keeps_exact_unstripped_stdout_comparison(self):
        r = request()
        r["ssh"]["client_version"] = "psql (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)"
        expected = (r["ssh"]["client_version"] + "\n").encode("ascii")
        for output, accepted in ((expected, True), (b"psql (PostgreSQL) 17.11\n", False),
                                 (expected.rstrip(b"\n"), False), (expected + b"extra\n", False),
                                 (expected.replace(b"\n", b"\r\n"), False)):
            processes = types.SimpleNamespace(spawn=lambda *args: object(),
                                              complete=lambda *args: (0, output, b""))
            runtime = program.NativeRuntime(processes)
            with self.subTest(output=output), patch.object(runtime, "recheck"):
                if accepted:
                    runtime.programs(r)
                else:
                    with self.assertRaises(program.Refusal):
                        runtime.programs(r)

    def test_success_is_four_role_probes_and_two_privileged_reads_without_secret_export(self):
        r = request()
        runtime = MockRuntime(r)
        value = self.run_mock(r, runtime)
        probes = [call for call in runtime.calls if call[0] in ("positive", "negative")]
        self.assertEqual([call[0] for call in probes], ["positive", "negative", "positive", "negative"])
        self.assertEqual([call[2] for call in probes[::2]], PASSWORDS)
        self.assertEqual(len([call for call in runtime.calls if call[0] == "catalog"]), 2)
        for call in probes[1::2]:
            self.assertNotIn(call[2], PASSWORDS)
        encoded = json.dumps(value)
        for sentinel in PASSWORDS + ["fixture-wrong-A", "fixture-wrong-B", "postgresql://", "source.fixture.invalid"]:
            self.assertNotIn(sentinel, encoded)
        for field in ("configuration_accepted", "application_access_accepted", "source_capture_accepted", "migration_accepted"):
            self.assertIs(value[field], False)
        self.assertEqual(value["request_sha256"], program.canonical_digest(r))

    def test_exact_two_key_roster_rejects_extra_duplicate_missing_and_changed_name(self):
        r = request()
        for raw in (dotenv(r) + b"EXTRA=ignored\n", dotenv(r) + dotenv(r).splitlines()[0] + b"\n",
                    dotenv(r).splitlines()[0], dotenv(r).replace(b"APP_URL=", b"OTHER_URL=")):
            with self.subTest(raw=raw), self.assertRaises(program.Refusal):
                program.parse_passwords(raw, r["roles"])

    def test_source_endpoint_and_query_overrides_refuse_before_authentication(self):
        r = request()
        for old, new in ((b"source.fixture.invalid", b"wrong.fixture.invalid"), (b":5432/", b":5433/"),
                         (b"sslmode=verify-full", b"host=127.0.0.1"), (b"sslmode=verify-full", b"options=-csearch_path=evil"),
                         (b"sslmode=verify-full", b"passfile=/fixture/pass"), (b"fixture_app:", b"other_app:")):
            runtime = MockRuntime(r)
            runtime.raw = runtime.raw.replace(old, new)
            with self.subTest(new=new), self.assertRaises(program.Refusal):
                self.run_mock(r, runtime)
            self.assertFalse(any(call[0] in ("positive", "negative") for call in runtime.calls))

    def test_existing_passwords_must_differ_and_each_negative_differs_from_both(self):
        r = request()
        runtime = MockRuntime(r)
        runtime.raw = dotenv(r, [PASSWORDS[0], PASSWORDS[0]])
        with self.assertRaises(program.Refusal):
            self.run_mock(r, runtime)
        self.assertFalse(any(call[0] in ("positive", "negative") for call in runtime.calls))
        values = iter(PASSWORDS + ["new-wrong-A"] + PASSWORDS + ["new-wrong-A", "new-wrong-B"])
        self.assertEqual(program.negative_passwords(PASSWORDS, lambda _: next(values)), ["new-wrong-A", "new-wrong-B"])
        with self.assertRaises(program.Refusal):
            program.negative_passwords(PASSWORDS, lambda _: PASSWORDS[1])

    def test_privileged_identity_is_never_requested_from_unprivileged_role(self):
        self.assertNotIn("pg_control_system", program.IDENTITY_SQL)
        self.assertNotIn("pg_authid", program.IDENTITY_SQL)
        self.assertIn("pg_backend_pid", program.IDENTITY_SQL)
        sql = program.catalog_sql(request(), [8001, 8002])
        self.assertIn("pg_catalog.pg_control_system()", sql)
        self.assertIn("pg_catalog.pg_stat_activity", sql)
        self.assertIn(":'app_role'", sql)
        self.assertNotIn("SET ", sql)
        self.assertNotIn("ALTER ", sql)
        self.assertNotIn("GRANT ", sql)

    def test_privileged_catalog_pre_and_post_refuse_identity_role_membership_and_fence_drift(self):
        def mutations(value):
            return [lambda: value.update(system_identifier="9999999999999999999"),
                    lambda: value["roles"][0].update(superuser=True),
                    lambda: value["roles"][1].update(membership_rows=1),
                    lambda: value["roles"][0].update(role_config=[]),
                    lambda: value["roles"][1].update(scram_present=False),
                    lambda: value["database"].update(connection_limit=-1),
                    lambda: value["database"].update(application_connect=True),
                    lambda: value["database"].update(public_connect=True)]
        for post in (False, True):
            for index in range(8):
                runtime = MockRuntime(request())
                runtime.cat_mutate = lambda value, is_post: mutations(value)[index]() if post == is_post else None
                with self.subTest(post=post, index=index), self.assertRaises(program.Refusal):
                    self.run_mock(runtime=runtime)
                if not post:
                    self.assertFalse(any(call[0] in ("positive", "negative") for call in runtime.calls))

    def test_post_catalog_correlates_live_positive_sessions_to_pinned_cluster(self):
        for field, value in (("pid", 9999), ("user", "wrong_role"), ("database", "fixture_db"), ("address", "10.0.0.9")):
            runtime = MockRuntime(request())
            runtime.cat_mutate = lambda cat, post: cat["sessions"][0].update({field: value}) if post else None
            with self.subTest(field=field), self.assertRaises(program.Refusal):
                self.run_mock(runtime=runtime)

    def test_file_and_host_drift_never_produce_observation(self):
        runtime = MockRuntime(request())
        runtime.post_raw += b"\n"
        with self.assertRaises(program.Refusal):
            self.run_mock(runtime=runtime)
        runtime = MockRuntime(request())
        runtime.host = lambda r: dict(r["host"], boot_id="bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
        with self.assertRaises(program.Refusal):
            self.run_mock(runtime=runtime)
        runtime = MockRuntime(request())
        runtime.recheck = lambda r: program.require(False)
        with self.assertRaises(program.Refusal):
            self.run_mock(runtime=runtime)

    def test_url_controls_refuse_instead_of_urlsplit_silently_normalizing(self):
        r = request()
        for control in (b"\t", b"\x01", b" "):
            with self.subTest(control=control), self.assertRaises(program.Refusal):
                program.parse_passwords(dotenv(r).replace(b"source.fixture", control + b"source.fixture"), r["roles"])

    def test_stat_identity_preserves_nanoseconds_and_refuses_rounding_links_or_custody(self):
        r = request()
        info = types.SimpleNamespace(st_dev=1, st_ino=2, st_size=300, st_uid=0, st_gid=0, st_mode=0o100600,
                                     st_nlink=1, st_mtime_ns=1792197133436814154, st_ctime_ns=1792197133436814154)
        self.assertTrue(program.stat_matches(info, r["file"]))
        for field, value in (("st_mtime_ns", 1792197133436814155), ("st_ctime_ns", 1792197133436814155),
                             ("st_nlink", 2), ("st_uid", 10), ("st_gid", 10), ("st_mode", 0o100644)):
            altered = copy.copy(info)
            setattr(altered, field, value)
            self.assertFalse(program.stat_matches(altered, r["file"]))
        rounded = copy.deepcopy(r)
        rounded["file"]["mtime_ns"] = 1792197133436814154
        with self.assertRaises(program.Refusal):
            program.validate_request(rounded)

    def test_custody_open_uses_nofollow_for_all_ancestors_and_file(self):
        r = request()
        good = types.SimpleNamespace(st_uid=0, st_gid=0, st_mode=0o100600, st_nlink=1)
        opened = []
        def fake_open(path, flags, **kwargs):
            opened.append((path, flags))
            return len(opened)
        with patch.object(program.os, "open", side_effect=fake_open), patch.object(program.os, "fstat", return_value=good), patch.object(program.os, "close"):
            program.trusted_open(r["file"]["path"], secret=True)
        for path, flags in opened[1:]:
            self.assertTrue(flags & program.os.O_NOFOLLOW)
        self.assertTrue(opened[-1][1] & program.os.O_CLOEXEC)
        bad = types.SimpleNamespace(st_uid=10, st_gid=0, st_mode=0o40755)
        with patch.object(program.os, "open", side_effect=fake_open), patch.object(program.os, "fstat", return_value=bad), patch.object(program.os, "close"), self.assertRaises(program.Refusal):
            program.trusted_open(r["file"]["path"], secret=True)

    def test_negative_auth_accepts_only_exact_libpq_single_or_fallback_rejections(self):
        r = request()

        class FakeProcesses:
            result = None

            def spawn(self, argv, env):
                self.assert_password_not_in_argv = env["PGPASSWORD"] not in argv
                return object()

            def complete(self, child, payload):
                assert payload == b"SELECT 1;\n"
                return self.result

        processes = FakeProcesses()
        runtime = program.NativeRuntime(processes)
        for role in r["roles"]:
            expected = (
                'psql: error: connection to server at "127.0.0.1", port 5432 failed: '
                'FATAL:  password authentication failed for user "' + role["name"] + '"\n'
            ).encode("ascii")
            second = expected[len(b"psql: error: "):]
            pair = expected + second
            for diagnostic in (expected, pair):
                with self.subTest(role=role["name"], accepted=diagnostic):
                    processes.result = (2, b"", diagnostic)
                    runtime.negative(r, role, "fixture-wrong-password")
                    self.assertTrue(processes.assert_password_not_in_argv)
            rejected = (
                (0, b"", pair),
                (1, b"", pair),
                (2, b"unexpected\n", pair),
                (2, b"", expected + expected),
                (2, b"", pair + second),
                (2, b"", pair + b"other error\n"),
                (2, b"", expected + b"connection refused\n"),
                (2, b"", pair.replace(role["name"].encode("ascii"), b"other_role", 1)),
                (2, b"", pair.rstrip(b"\n")),
            )
            for result in rejected:
                with self.subTest(role=role["name"], rejected=result):
                    processes.result = result
                    with self.assertRaises(program.Refusal):
                        runtime.negative(r, role, "fixture-wrong-password")

    def test_explicit_password_failure_is_required_network_or_generic_errors_refuse(self):
        r = request()
        role = r["roles"][0]
        expected = ('psql: error: connection to server at "127.0.0.1", port 5432 failed: FATAL:  password authentication failed for user "' + role["name"] + '"\n').encode()
        class FakeProcesses:
            result = (2, b"", expected)
            def spawn(self, argv, env):
                self.argv, self.env = argv, env
                return object()
            def complete(self, child, sql):
                return self.result
        processes = FakeProcesses()
        runtime = program.NativeRuntime(processes)
        runtime.negative(r, role, "fixture-wrong-A")
        self.assertEqual(processes.env["PGPASSWORD"], "fixture-wrong-A")
        self.assertNotIn("fixture-wrong-A", " ".join(processes.argv))
        self.assertIn("--host=127.0.0.1", processes.argv)
        for result in ((2, b"", b"network connection refused\n"), (2, b"", b"timeout\n"),
                       (1, b"", expected), (2, b"unexpected", expected), (2, b"", expected + b"other error\n")):
            processes.result = result
            with self.subTest(result=result), self.assertRaises(program.Refusal):
                runtime.negative(r, role, "fixture-wrong-A")

    def test_environment_has_no_ambient_auth_sources_or_pgoptions(self):
        env = program.clean_env("fixture-only")
        self.assertEqual(set(env), {"PATH", "HOME", "LC_ALL", "LANG", "PGCONNECT_TIMEOUT", "PGAPPNAME", "PGPASSWORD"})
        argv = program.client_argv(request(), "fixture_app")
        self.assertIn("-X", argv)
        self.assertIn("-w", argv)
        self.assertIn("--dbname=postgres", argv)
        self.assertNotIn("-c", argv)

    def test_deadline_bound_skew_refusal_and_cleanup_kill_reap(self):
        issued = 1792242000000000000
        deadline = issued + 50_000_000_000
        self.assertEqual(program.remote_budget(str(issued), str(deadline), issued, 10.0), 53.0)
        self.assertEqual(program.remote_budget(str(issued), str(deadline), issued + 15_000_000_000, 10.0), 41.0)
        for now, end in ((issued - 2_000_000_001, deadline), (issued + 15_000_000_001, deadline), (issued, deadline + 1)):
            with self.assertRaises(program.Refusal):
                program.remote_budget(str(issued), str(end), now, 10.0)
        class FakeChild:
            pid = 4242
            stdin = stdout = stderr = None
            waited = False
            def poll(self):
                return None
            def wait(self, timeout):
                self.waited = True
        child = FakeChild()
        processes = program.Processes(10.0)
        processes.children = [child]
        with patch.object(program.os, "killpg") as kill:
            self.assertTrue(processes.cleanup())
            kill.assert_called_once_with(4242, program.signal.SIGKILL)
        self.assertTrue(child.waited)

    def test_closed_json_rejects_duplicate_keys_and_nonstandard_numbers(self):
        for raw in ('{"a":1,"a":2}', '{"a":NaN}', '{"a":Infinity}'):
            with self.assertRaises(program.Refusal):
                program.decode_json(raw)


@unittest.skipUnless(os.environ.get("COMPOUND_CREDENTIAL_VERIFY_TEST_CONTAINER") or os.environ.get("COMPOUND_CREDENTIAL_VERIFY_TEST_PSQL"),
                     "set an owned PostgreSQL fixture container or local psql/socket for real catalog coverage")
class PostgreSQLCatalogTests(unittest.TestCase):
    def query(self, sql):
        local = os.environ.get("COMPOUND_CREDENTIAL_VERIFY_TEST_PSQL")
        if local:
            argv = [local, "-h", os.environ["COMPOUND_CREDENTIAL_VERIFY_TEST_SOCKET"],
                    "-p", os.environ["COMPOUND_CREDENTIAL_VERIFY_TEST_PORT"],
                    "-U", os.environ["COMPOUND_CREDENTIAL_VERIFY_TEST_ROLE"],
                    "-d", os.environ["COMPOUND_CREDENTIAL_VERIFY_TEST_DATABASE"],
                    "-X", "-w", "-A", "-t", "-v", "ON_ERROR_STOP=1"]
            env = {"PATH": "/usr/bin:/bin", "HOME": "/nonexistent", "LC_ALL": "C", "LANG": "C",
                   "PGPASSFILE": "/nonexistent", "PGSERVICEFILE": "/nonexistent"}
        else:
            argv = ["docker", "exec", "-i", os.environ["COMPOUND_CREDENTIAL_VERIFY_TEST_CONTAINER"],
                    "/bin/sh", "-c", 'exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -X -A -t -v ON_ERROR_STOP=1']
            env = None
        result = subprocess.run(
            argv, env=env, input=sql, capture_output=True, text=True, timeout=15, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_real_catalog_query_reads_role_settings_from_pg_roles(self):
        identity = self.query("SELECT json_build_object('role',CURRENT_USER,'database',current_database());\n")
        r = request()
        r["roles"][0]["name"] = identity["role"]
        r["roles"][1]["name"] = "pg_monitor"
        r["database"]["name"] = identity["database"]
        settings = self.query("SELECT json_object_agg(rolname,rolconfig) FROM pg_roles "
                              "WHERE rolname IN (CURRENT_USER,'pg_monitor');\n")
        for pids in ([], [1, 2]):
            with self.subTest(pids=pids):
                actual = self.query(program.catalog_sql(r, pids))
                self.assertEqual(actual["database"]["name"], identity["database"])
                self.assertEqual([row["name"] for row in actual["roles"]],
                                 [identity["role"], "pg_monitor"])
                for role in actual["roles"]:
                    self.assertEqual(role["role_config"], settings[role["name"]])
                    self.assertIs(type(role["scram_present"]), bool)
                    self.assertNotIn("rolpassword", role)
                if not pids:
                    self.assertEqual(actual["sessions"], [])


if __name__ == "__main__":
    unittest.main(verbosity=1)
