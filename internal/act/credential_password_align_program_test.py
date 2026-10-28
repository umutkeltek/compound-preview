"""Synthetic protocol and actual Linux/PostgreSQL fixtures for the closed owner."""
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import types
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("alignment_fixture", ROOT / "credential_verify_program.py")
owner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(owner)
exec(compile((ROOT / "credential_password_align_program.py").read_bytes(), "credential_password_align_program.py", "exec"), owner.__dict__)


class AlignmentFormatTests(unittest.TestCase):
    def inactive(self, containers, listed=(), endpoint="http://fixture-api:9000"):
        class Processes:
            argv = None

            def spawn(self, argv, environment):
                self.argv = argv
                return self

            def complete(self, child, payload):
                return 0, "\n".join(listed).encode("ascii"), b""

        processes = Processes()
        request = {"catalog": {"docker_sha256": "d" * 64},
                   "profile": {"containers": containers, "web_api_base": endpoint}}
        with patch.object(owner, "program_hash", return_value="d" * 64):
            owner.alignment_runtime_inactive(request, processes)
        self.assertEqual(processes.argv, ["/usr/bin/docker", "container", "ls", "--all", "--format", "{{.Names}}"])

    def test_closed_topologies_accept_only_legacy_subsets_or_exact_seven(self):
        legacy = ["fixture-api", "fixture-worker", "fixture-provider-sync", "fixture-web", "fixture-cloudflared"]
        commercial = ["fixture-auth", "fixture-api", "fixture-worker", "fixture-billing", "fixture-billing-reconcile", "fixture-provider-sync", "fixture-web"]
        for names in (legacy, ["fixture-api"], ["fixture-api", "fixture-web"], commercial, list(reversed(commercial))):
            with self.subTest(names=names):
                self.inactive(names, listed=["unrelated-example"])
        for names in ([], ["fixture-web"], ["fixture-api", "fixture-api"],
                      ["fixture-api", "fixture-scheduler"], commercial[:6],
                      ["fixture-api", "fixture-auth", "fixture-web"],
                      commercial[:6] + ["fixture-api"], commercial[:6] + ["fixture-extra"],
                      commercial + ["fixture-cloudflared"], commercial[:6] + ["other-web"]):
            with self.subTest(refused=names), self.assertRaises(owner.Refusal):
                self.inactive(names)

    def test_commercial_guard_includes_all_eight_even_stopped_containers(self):
        commercial = ["fixture-auth", "fixture-api", "fixture-worker", "fixture-billing", "fixture-billing-reconcile", "fixture-provider-sync", "fixture-web"]
        for suffix in ("auth", "api", "worker", "billing", "billing-reconcile", "provider-sync", "web", "cloudflared"):
            with self.subTest(container=suffix), self.assertRaises(owner.Refusal):
                self.inactive(commercial, listed=["fixture-" + suffix])
        for suffix in ("api", "worker", "provider-sync", "web", "cloudflared"):
            with self.subTest(legacy=suffix), self.assertRaises(owner.Refusal):
                self.inactive(["fixture-api"], listed=["fixture-" + suffix])

    def test_closed_topologies_require_the_qualified_api_endpoint(self):
        for endpoint in ("", "http://fixture-web:9000", "http://other-api:9000", "http://fixture_api:9000", "http://fixture-api:9000/extra", "http://user@fixture-api:9000"):
            with self.subTest(endpoint=endpoint), self.assertRaises(owner.Refusal):
                self.inactive(["fixture-api"], endpoint=endpoint)

    def test_catalog_pin_commits_private_verifiers_and_revisions(self):
        catalog = {"database": {"oid": "123"}, "roles": [
            {"oid": 1, "xmin": "1", "ctid": "(1,1)", "verifier": "synthetic-old-a", "attributes": {}},
            {"oid": 2, "xmin": "1", "ctid": "(1,2)", "verifier": "synthetic-old-m", "attributes": {}}]}
        request = {"catalog": {"database_oid": 123, "roles": [
            {k: row[k] for k in ("oid", "xmin", "ctid")} for row in catalog["roles"]],
            "sha256": owner.canonical_digest(catalog)}}
        owner.alignment_catalog_pin(catalog, request)
        tampered = copy.deepcopy(catalog)
        tampered["roles"][0]["verifier"], tampered["roles"][1]["verifier"] = (
            tampered["roles"][1]["verifier"], tampered["roles"][0]["verifier"])
        self.assertEqual(owner.alignment_public_catalog(catalog), owner.alignment_public_catalog(tampered))
        with self.assertRaises(owner.Refusal):
            owner.alignment_catalog_pin(tampered, request)
        tampered = copy.deepcopy(catalog)
        tampered["roles"][0]["xmin"] = "2"
        with self.assertRaises(owner.Refusal):
            owner.alignment_catalog_pin(tampered, request)

    def test_mixed_or_revision_drift_never_classifies_as_postimage(self):
        before = {"roles": [{"oid": 1, "xmin": "1", "ctid": "(1,1)", "verifier": "old-a", "attributes": {}},
                            {"oid": 2, "xmin": "1", "ctid": "(1,2)", "verifier": "old-m", "attributes": {}}]}
        after = copy.deepcopy(before)
        for row in after["roles"]:
            row["verifier"] = "new"
            row["xmin"] = "2"
        self.assertEqual(owner.alignment_classify(before, before, after), "old_old")
        self.assertEqual(owner.alignment_classify(after, before, after), "new_new")
        mixed = copy.deepcopy(after)
        mixed["roles"][0] = before["roles"][0]
        with self.assertRaises(owner.Refusal):
            owner.alignment_classify(mixed, before, after)
        drift = copy.deepcopy(after)
        drift["roles"][0]["xmin"] = "3"
        with self.assertRaises(owner.Refusal):
            owner.alignment_classify(drift, before, after)

    def test_scram_format_rejects_plaintext_sql_and_invalid_digests(self):
        for value in ("plaintext", "SCRAM-SHA-256$4096:abc$bad:bad", "x';SELECT 1;--", None):
            with self.assertRaises(owner.Refusal):
                owner.alignment_scram(value)


class AlignmentDiagnosticTests(unittest.TestCase):
    def run_main(self, failure=None, cleanup=True, operation="inspect"):
        request = {"program_sha256": "d" * 64, "operation": operation,
                   "roles": [{"name": "fixture_app"}, {"name": "fixture_migrator"}],
                   "catalog": {"docker_sha256": "e" * 64}}
        issued = owner.time.time_ns()
        wire = {"request": request, "action_intent_id": "act_" + "a" * 12,
                "original_action_intent_id": "act_" + "a" * 12,
                "issued_ns": str(issued), "deadline_ns": str(issued + 50_000_000_000)}
        runtime, session = Mock(), Mock()
        runtime.host.return_value = {"fixture": "identity"}
        session.catalog.return_value = {"database": {"oid": 42}, "roles": [
            {"oid": 43, "xmin": "1", "ctid": "(0,1)"},
            {"oid": 44, "xmin": "1", "ctid": "(0,2)"}]}
        processes = Mock()
        processes.cleanup.return_value = cleanup
        stdout = types.SimpleNamespace(buffer=io.BytesIO())
        stderr = io.StringIO()
        with patch.object(owner.sys, "stdin", types.SimpleNamespace(buffer=io.BytesIO(json.dumps(wire).encode()))), \
             patch.object(owner.sys, "stdout", stdout), patch.object(owner.sys, "stderr", stderr), \
             patch.object(owner.sys, "argv", ["owner", request["program_sha256"]]), \
             patch.object(owner.signal, "signal"), patch.object(owner.signal, "setitimer"), \
             patch.object(owner, "Processes", return_value=processes), \
             patch.object(owner, "alignment_validate"), \
             patch.object(owner, "NativeRuntime", return_value=runtime), \
             patch.object(owner, "AlignmentSession", return_value=session) as constructor, \
             patch.object(owner, "alignment_file_metadata"), \
             patch.object(owner, "alignment_runtime_inactive"), \
             patch.object(owner, "alignment_closed_catalog") as closed, \
             patch.object(owner, "validate_catalog") as base, \
             patch.object(owner, "alignment_namespace") as namespace:
            if failure is not None:
                failure(runtime, session, constructor, closed, base, namespace)
            code = owner.alignment_main()
        self.assertEqual(processes.cleanup.call_count, 1)
        return code, stdout.buffer.getvalue().decode(), stderr.getvalue()

    def test_actual_inspection_path_has_only_finite_failed_stage_markers(self):
        cases = {
            "runtime_preconditions": lambda r, s, c, v, b, n: setattr(r.programs, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_negative_auth": lambda r, s, c, v, b, n: setattr(r.negative, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_session": lambda r, s, c, v, b, n: setattr(c, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_catalog_read": lambda r, s, c, v, b, n: setattr(s.catalog, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_catalog_shape": lambda r, s, c, v, b, n: setattr(v, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_base_catalog": lambda r, s, c, v, b, n: setattr(b, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "inspection_postchecks": lambda r, s, c, v, b, n: setattr(r.host, "side_effect", [{"fixture": "identity"}, RuntimeError("fixture-private-diagnostic")]),
            "cleanup": lambda r, s, c, v, b, n: setattr(s.close, "side_effect", RuntimeError("fixture-private-diagnostic")),
            "deadline": lambda r, s, c, v, b, n: setattr(r.programs, "side_effect", owner.StageRefusal("deadline")),
        }
        for stage, failure in cases.items():
            with self.subTest(stage=stage):
                self.assertEqual(self.run_main(failure), (1, "", "credential_password_align:refused:" + stage + "\n"))

    def test_scram_failure_is_distinct_from_closed_state_without_value_output(self):
        value = {"roles": [{"oid": 1, "xmin": "1", "ctid": "(0,1)",
                  "attributes": {"rolname": "fixture_app", "rolcanlogin": True},
                  "verifier": "fixture-private-invalid-verifier"}], "memberships": [],
                 "database": {}, "database_revision": {}, "settings": [], "hba": [],
                 "preload": "", "session_preload": "", "local_preload": "", "sessions": 0,
                 "application_connect": False, "public_connect": False, "maintenance_connect": [True, True]}
        value["roles"].append(copy.deepcopy(value["roles"][0]))
        progress = owner.AlignmentProgress()
        with self.assertRaises(owner.Refusal):
            owner.alignment_closed_catalog(value, {"roles": [{"name": "fixture_app"}]}, progress)
        self.assertEqual(progress.stage, "inspection_catalog_scram")

    def test_cleanup_overrides_success_and_does_not_acknowledge_mutation(self):
        self.assertEqual(self.run_main(cleanup=False), (1, "", "credential_password_align:refused:cleanup\n"))
        failure = lambda r, s, c, v, b, n: setattr(n, "side_effect", RuntimeError("fixture-private-diagnostic"))
        self.assertEqual(self.run_main(failure, operation="align"), (1, "", "credential_password_align:refused:password_operation\n"))
        self.assertEqual(self.run_main(failure, cleanup=False, operation="align"), (1, "", "credential_password_align:refused:cleanup\n"))

    def test_success_has_no_failure_stage_or_stderr(self):
        code, stdout, stderr = self.run_main()
        self.assertEqual((code, stderr), (0, ""))
        self.assertNotIn("failure_stage", json.loads(stdout))
        self.assertEqual(json.loads(stdout)["classification"], "metadata_only")


@unittest.skipUnless(os.environ.get("COMPOUND_ALIGNMENT_LINUX_PG17") == "1", "requires owned disposable Linux/root PostgreSQL17 fixture")
class LinuxPostgresAlignmentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        r = {"ssh": {"client_path": "/usr/lib/postgresql/17/bin/psql"}}
        result = subprocess.run(owner.client_argv(r, "postgres", True), env=owner.clean_env(),
            input=b"SELECT current_setting('hba_file');\n", capture_output=True, check=True)
        hba = Path(result.stdout.decode("ascii").strip())
        hba.write_text("local all all peer\nhost all all 127.0.0.1/32 scram-sha-256\nhost all all ::1/128 scram-sha-256\n")
        subprocess.run(owner.client_argv(r, "postgres", True), env=owner.clean_env(),
            input=b"SELECT pg_reload_conf();\n", capture_output=True, check=True)

    def query(self, sql):
        r = {"ssh": {"client_path": "/usr/lib/postgresql/17/bin/psql"}}
        result = subprocess.run(owner.client_argv(r, "postgres", True), env=owner.clean_env(),
                                input=("SET log_statement='none'; SET log_min_error_statement='panic'; SET client_min_messages='warning';\n" + sql).encode("ascii"), capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0)
        self.assertFalse(result.stderr)
        return result.stdout

    def setUp(self):
        self.assertEqual(os.geteuid(), 0)
        self.query("DROP DATABASE IF EXISTS repair_example; DROP ROLE IF EXISTS repair_app; DROP ROLE IF EXISTS repair_migrator;")
        self.query("CREATE ROLE repair_app LOGIN PASSWORD 'synthetic-old-app'; CREATE ROLE repair_migrator LOGIN PASSWORD 'synthetic-old-migrator';")
        self.query("CREATE DATABASE repair_example OWNER repair_migrator CONNECTION LIMIT 0;")
        self.query("REVOKE CONNECT ON DATABASE repair_example FROM PUBLIC; REVOKE CONNECT ON DATABASE repair_example FROM repair_app;")
        self.r = {"ssh": {"client_path": "/usr/lib/postgresql/17/bin/psql"}, "roles": [{"name": "repair_app"}, {"name": "repair_migrator"}],
                  "database": {"name": "repair_example", "allow_connections": True}}
        self.processes = owner.Processes(time.monotonic() + 30)
        self.root = Path("/root/compound-alignment-fixture")
        self.root.mkdir(mode=0o700, exist_ok=True)
        self.directory = Path(tempfile.mkdtemp(dir=self.root))
        self.r["file"] = {"path": str(self.directory / "original.env")}
        (self.directory / "original.env").write_text("SYNTHETIC=fixture\n")
        (self.directory / "original.env").chmod(0o600)

    def tearDown(self):
        self.assertTrue(self.processes.cleanup())

    def image(self):
        session = owner.AlignmentSession(self.r, self.processes)
        try:
            value = session.catalog(self.r)
            owner.alignment_closed_catalog(value, self.r)
            return value
        finally:
            session.close()

    def test_native_scram_duplex_transaction_and_two_actual_authentications(self):
        before = self.image()
        session = owner.AlignmentSession(self.r, self.processes)
        passwords = ["synthetic-canary-app", "synthetic-canary-migrator"]
        try:
            session.align(self.r, passwords)
            after = session.catalog(self.r)
            self.assertEqual(owner.alignment_public_catalog(before, False), owner.alignment_public_catalog(after, False))
            for index in range(2):
                owner.alignment_scram(after["roles"][index]["verifier"])
                self.assertNotIn(passwords[index], json.dumps(after))
            session.commit()
        finally:
            session.close()
        for index, role in enumerate(self.r["roles"]):
            result = subprocess.run(owner.client_argv(self.r, role["name"]), env=owner.clean_env(passwords[index]),
                                    input=b"SELECT CURRENT_USER;\n", capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(result.stdout, (role["name"] + "\n").encode())
            result = subprocess.run(owner.client_argv(self.r, role["name"]), env=owner.clean_env("synthetic-wrong"),
                                    input=b"SELECT 1;\n", capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 2)
            self.assertFalse(result.stdout)
            self.assertNotIn(passwords[index].encode(), result.stderr)
        self.assertEqual(owner.alignment_classify(self.image(), before, after), "new_new")

    def test_uncommitted_two_password_changes_roll_back_on_explicit_client_close(self):
        before = self.image()
        session = owner.AlignmentSession(self.r, self.processes)
        session.align(self.r, ["synthetic-never-committed-a", "synthetic-never-committed-m"])
        session.close()
        self.assertEqual(self.image(), before)

    def test_restore_is_one_transaction_and_preserves_all_nonpassword_catalogs(self):
        before = self.image()
        session = owner.AlignmentSession(self.r, self.processes)
        session.align(self.r, ["synthetic-new-a", "synthetic-new-m"])
        after = session.catalog(self.r)
        session.commit()
        session.close()
        self.assertEqual(owner.alignment_classify(self.image(), before, after), "new_new")
        session = owner.AlignmentSession(self.r, self.processes)
        session.restore(self.r, before)
        session.commit()
        session.close()
        restored = self.image()
        self.assertEqual(owner.alignment_public_catalog(restored, False), owner.alignment_public_catalog(before, False))
        self.assertEqual([x["verifier"] for x in restored["roles"]], [x["verifier"] for x in before["roles"]])

    def test_private_custody_is_exclusive_durable_and_rejects_hardlinks_symlinks(self):
        custody = owner.AlignmentCustody(self.r, "act_0123456789ab", True)
        try:
            image = self.image()
            custody.put("PREIMAGE.json", image)
            self.assertEqual(custody.get("PREIMAGE.json"), image)
            with self.assertRaises(FileExistsError):
                custody.put("PREIMAGE.json", image)
            namespace = self.directory / owner.ALIGN_NAMESPACE / "act_0123456789ab"
            with self.assertRaises(owner.Refusal):
                owner.AlignmentCustody(self.r, "act_1123456789ab", True)
            os.link(namespace / "PREIMAGE.json", namespace / "alias")
            with self.assertRaises(owner.Refusal):
                custody.get("PREIMAGE.json")
            os.unlink(namespace / "alias")
            os.symlink(namespace / "PREIMAGE.json", namespace / "POSTIMAGE.json")
            with self.assertRaises(OSError):
                custody.get("POSTIMAGE.json")
        finally:
            custody.close()


if __name__ == "__main__":
    unittest.main()
