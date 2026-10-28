import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("retire", Path(__file__).with_name("source_marker_retirement_program.py"))
retire = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retire)

class MarkerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        uid, gid = os.geteuid(), os.getegid()
        # BSD filesystems (macOS) give a new entry its parent directory's
        # group, not the caller's egid, so a TMPDIR under /private/tmp
        # (root:wheel) makes every fixture file gid 0. Pin the fixture
        # tree's group so the fences compare one known owner on any TMPDIR.
        os.chown(self.root, uid, gid)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.real_owner = retire.owner_ok
        retire.owner_ok = lambda s: s.st_uid == uid and s.st_gid == gid
        self.real_rename = retire.rename_no_replace
        if sys.platform != "linux":
            def rename(srcfd, src, dstfd, dst):
                try:
                    os.stat(dst, dir_fd=dstfd, follow_symlinks=False)
                except FileNotFoundError:
                    os.rename(src, dst, src_dir_fd=srcfd, dst_dir_fd=dstfd)
                else:
                    raise retire.Refusal()
            retire.rename_no_replace = rename
        names = (".env", "service.env", "migration.env", "frontend.env")
        files = []
        for name in names:
            path = self.root / name
            path.write_bytes(("synthetic-env-" + name).encode())
            path.chmod(0o600)
            files.append({"basename": name, "path": "/srv/fixture/" + name, "stat": retire.metadata(path.lstat())})
        self.marker = self.root / ".env.SOURCE_COMPARISON_ONLY"
        self.marker.write_bytes(b"synthetic-source-marker")
        self.marker.chmod(0o600)
        self.inactive = self.root / ".runtime-staging-inactive"
        self.inactive.write_bytes(b"synthetic-inactive")
        self.inactive.chmod(0o600)
        self.lock = self.root / "maintenance.lock"
        self.lock.write_bytes(b"")
        self.lock.chmod(0o644)
        program = retire.digest(Path(retire.__file__).read_bytes())
        self.program = program
        self.request = {"schema_version": "compound.source_marker_retirement_request.v0.1", "effect_set": "metadata-only-source-marker-rename-after-native-acceptance", "program_sha256": program, "attempt_id": "20261023t010203z-bbbbbbbbbbbbbbbb", "root": {"path": "/srv/fixture"}, "files": files, "recovery_basename": "fixture-source-marker-recovery", "container_prefix": "fixture", "marker": {"basename": self.marker.name, "path": "/srv/fixture/" + self.marker.name, "stat": retire.metadata(self.marker.lstat())}, "inactive": {"basename": self.inactive.name, "path": "/srv/fixture/" + self.inactive.name, "stat": retire.metadata(self.inactive.lstat())}, "maintenance_lock": {"basename": "fixture-runtime-maintenance.lock", "path": "/run/lock/fixture-runtime-maintenance.lock", "stat": retire.metadata(self.lock.lstat())}, "host": {"hostname": "host.fixture.invalid", "machine_id": "a" * 32, "boot_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "system_identifier": "1234567890123456789", "version_num": "170011"}, "database": {"name": "fixture_db", "oid": "12345", "allow_connections": False}}
        self.db = {"system_identifier": self.request["host"]["system_identifier"], "version_num": "170011", "oid": "12345", "allow_connections": False, "connection_limit": 0, "clients": 0}
        self.containers = ""
        self.probes = []
    def tearDown(self):
        retire.owner_ok = self.real_owner
        retire.rename_no_replace = self.real_rename
        os.close(self.fd)
        self.tmp.cleanup()
    def test_legacy_request_pin_is_reconcile_only(self):
        legacy_sha = "b517afa4728bf99bc9a1e347b186641ea921f2cbb2ae7df9adb81ba854c51487"
        with self.assertRaises(retire.Refusal):
            self.run_main(mutate=lambda request: request.update(program_sha256=legacy_sha))
        with self.assertRaises(retire.Refusal):
            self.run_main("reconcile", mutate=lambda request: request.update(program_sha256="f" * 64))
        self.assertTrue(self.marker.exists())
        original = copy.deepcopy(self.request)
        original["program_sha256"] = legacy_sha
        original["executables"] = {"client_path": "/usr/lib/postgresql/17/bin/psql"}
        result = self.run_main("reconcile", mutate=lambda request: request.update(program_sha256=legacy_sha))
        self.assertEqual(result["request_sha256"], retire.digest(retire.canonical(original)))
        self.assertEqual(result["program_sha256"], self.program)
        self.assertTrue(result["source_marker_retired"])

    def run_main(self, operation="retire", mutate=None, clock=None, budget_ns=30 * 10**9, on_probe=None):
        request = copy.deepcopy(self.request)
        if mutate:
            mutate(request)
        clock_read = (lambda: clock[0]) if clock is not None else time.time_ns
        wire = {"request": request, "operation": operation, "deadline_ns": str(clock_read() + budget_ns)}
        output = io.BytesIO()
        real_open, real_lstat = os.open, os.lstat
        def opened(path, *args, **kwargs):
            if path == request["maintenance_lock"]["path"]:
                path = self.lock
            return real_open(path, *args, **kwargs)
        def lstat(path, *args, **kwargs):
            if path == request["maintenance_lock"]["path"]:
                path = self.lock
            return real_lstat(path, *args, **kwargs)
        def probe(argv, **kwargs):
            self.probes.append(argv)
            if on_probe is not None:
                on_probe(argv, kwargs, int(wire["deadline_ns"]))
            self.assertEqual(kwargs["env"]["HOME"], "/nonexistent")
            if argv[0] == "/usr/bin/docker":
                return types.SimpleNamespace(returncode=0, stdout=self.containers)
            self.assertEqual(argv[0], "/usr/sbin/runuser")
            self.assertIn("default_transaction_read_only=on", kwargs["env"]["PGOPTIONS"])
            return types.SimpleNamespace(returncode=0, stdout=json.dumps(self.db))
        request["executables"] = {"client_path": "/usr/lib/postgresql/17/bin/psql"}
        host = {key: request["host"][key] for key in ("hostname", "machine_id", "boot_id")}
        def no_environment_open(path, *args, **kwargs):
            raise AssertionError("program attempted a body open: " + str(path))
        with patch.object(retire.time, "time_ns", side_effect=clock_read), patch.object(retire.os, "geteuid", return_value=0), patch.object(retire.os, "open", side_effect=opened), patch.object(retire.os, "lstat", side_effect=lstat), patch.object(retire, "open_tree", side_effect=lambda path: os.dup(self.fd)), patch.object(retire, "identity", return_value=host), patch.object(retire, "executable_pins", return_value=None), patch.object(retire.subprocess, "run", side_effect=probe), patch("builtins.open", side_effect=no_environment_open), patch.object(retire.sys, "argv", ["program", self.program]), patch.object(retire.sys, "stdin", types.SimpleNamespace(buffer=io.BytesIO(json.dumps(wire).encode()))), patch.object(retire.sys, "stdout", types.SimpleNamespace(buffer=output)):
            retire.main()
        return json.loads(output.getvalue())
    def test_real_main_preserves_inode_env_and_inactivity(self):
        inode = self.marker.lstat().st_ino
        before = [file["stat"] for file in self.request["files"]]
        result = self.run_main()
        self.assertTrue(result["source_marker_retired"])
        self.assertFalse(result["credential_bodies_opened"])
        self.assertEqual(result["runtime_before"], before)
        self.assertEqual(result["runtime_after"], before)
        self.assertFalse(self.marker.exists())
        self.assertTrue(self.inactive.exists())
        copy = self.root / self.request["recovery_basename"] / (self.marker.name + ".preimage")
        self.assertEqual(copy.lstat().st_ino, inode)
        self.assertGreaterEqual(len(self.probes), 4)
        self.assertNotIn("synthetic-env-", json.dumps(result))
        again = self.run_main("reconcile")
        self.assertFalse(again["changed"])
        self.assertEqual(copy.lstat().st_ino, inode)
    def test_active_container_or_database_blocks_rename(self):
        self.containers = "fixture-api\n"
        with self.assertRaises(retire.Refusal):
            self.run_main()
        self.assertTrue(self.marker.exists())
        self.containers = ""
        self.db["clients"] = 1
        with self.assertRaises(retire.Refusal):
            self.run_main()
        self.assertTrue(self.marker.exists())
    def test_runtime_ctime_or_inactive_drift_blocks(self):
        file = self.root / "service.env"
        file.chmod(0o644)
        with self.assertRaises(retire.Refusal):
            self.run_main()
        self.assertTrue(self.marker.exists())
    def test_foreign_preimage_is_never_overwritten(self):
        recovery = self.root / self.request["recovery_basename"]
        recovery.mkdir(mode=0o700)
        copy = recovery / (self.marker.name + ".preimage")
        copy.write_bytes(b"foreign")
        copy.chmod(0o600)
        with self.assertRaises(retire.Refusal):
            self.run_main()
        self.assertEqual(copy.read_bytes(), b"foreign")
        self.assertTrue(self.marker.exists())
    def test_parser_rejects_path_escape_before_mutation(self):
        with self.assertRaises(retire.Refusal):
            self.run_main(mutate=lambda r: r["marker"].update(basename="../../foreign"))
        self.assertTrue(self.marker.exists())
    def test_delayed_successful_fence_never_renames_past_deadline(self):
        inode = self.marker.lstat().st_ino
        recovery = self.root / self.request["recovery_basename"]
        for late_fence in (1, 2):
            with self.subTest(late_fence=late_fence):
                clock = [10 * 10**9]
                completed = [0]
                def delayed(argv, kwargs, deadline):
                    if argv[0] == "/usr/sbin/runuser":
                        completed[0] += 1
                        if completed[0] == late_fence:
                            clock[0] = deadline + 1
                with self.assertRaises(retire.Refusal):
                    self.run_main(clock=clock, on_probe=delayed)
                self.assertTrue(self.marker.exists())
                self.assertEqual(self.marker.lstat().st_ino, inode)
                self.assertFalse((recovery / (self.marker.name + ".preimage")).exists())
                if late_fence == 1:
                    self.assertFalse(recovery.exists())

    def test_child_timeouts_use_each_remaining_deadline(self):
        clock = [10 * 10**9]
        timeouts = []
        def spending_probe(argv, kwargs, deadline):
            remaining = (deadline - clock[0]) / 10**9
            self.assertGreater(kwargs["timeout"], 0)
            self.assertLessEqual(kwargs["timeout"], remaining)
            self.assertLessEqual(kwargs["timeout"], 10 if argv[0] == "/usr/bin/docker" else 15)
            timeouts.append(kwargs["timeout"])
            if len(timeouts) == 1:
                clock[0] += 1_500_000_000
        self.run_main(clock=clock, budget_ns=2 * 10**9, on_probe=spending_probe)
        self.assertEqual(timeouts[:2], [2.0, 0.5])

    def test_exhausted_wire_budget_never_starts_a_child(self):
        clock = [10 * 10**9]
        with self.assertRaises(retire.Refusal):
            self.run_main(clock=clock, budget_ns=0)
        self.assertEqual(self.probes, [])
        self.assertTrue(self.marker.exists())
        self.assertFalse((self.root / self.request["recovery_basename"]).exists())

    def test_exhausted_after_docker_never_starts_psql(self):
        clock = [10 * 10**9]
        def spend_budget(argv, kwargs, deadline):
            clock[0] = deadline
        with self.assertRaises(retire.Refusal):
            self.run_main(clock=clock, budget_ns=2 * 10**9, on_probe=spend_budget)
        self.assertEqual([argv[0] for argv in self.probes], ["/usr/bin/docker"])
        self.assertTrue(self.marker.exists())
        self.assertFalse((self.root / self.request["recovery_basename"]).exists())

    def test_fifo_journal_refusal_is_bounded(self):
        code = """
import importlib.util, pathlib, os, tempfile
s=importlib.util.spec_from_file_location("m",pathlib.Path("source_marker_retirement_program.py"));m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
with tempfile.TemporaryDirectory() as tmp:
 os.mkfifo(pathlib.Path(tmp)/"PREPARED.json",0o600);fd=os.open(tmp,os.O_RDONLY|os.O_DIRECTORY)
 try:
  try:m.journal(fd,{"synthetic":"metadata"})
  except (m.Refusal,OSError):pass
  else:raise AssertionError("FIFO accepted")
 finally:os.close(fd)
"""
        subprocess.run([sys.executable, "-I", "-B", "-c", code], check=True, timeout=5)


class ExecutablePinTests(unittest.TestCase):
    def test_full_large_executable_and_suffix_are_pinned(self):
        legacy_path = Path(__file__).with_name("testdata") / "source_marker_retirement_legacy_v0_1.py"
        legacy_spec = importlib.util.spec_from_file_location("legacy_retire", legacy_path)
        legacy = importlib.util.module_from_spec(legacy_spec)
        legacy_spec.loader.exec_module(legacy)
        self.assertEqual(legacy.digest(legacy_path.read_bytes()), "b517afa4728bf99bc9a1e347b186641ea921f2cbb2ae7df9adb81ba854c51487")
        real_open = open
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "large-executable"
            executable.write_bytes(b"a" * (32 * 1024 * 1024) + b"pinned suffix")
            sha = retire.digest(executable.read_bytes())
            request = {"ssh": {"interpreter_sha256": sha, "sudo_sha256": sha},
                       "executables": {"docker_sha256": sha, "runuser_sha256": sha,
                                       "client_path": "/usr/lib/postgresql/17/bin/psql", "client_sha256": sha}}
            paths = {"/usr/bin/python3", "/usr/bin/sudo", "/usr/bin/docker",
                     "/usr/sbin/runuser", request["executables"]["client_path"]}
            def opened(path, mode):
                self.assertIn(path, paths)
                self.assertEqual(mode, "rb")
                return real_open(executable, mode)
            with patch("builtins.open", side_effect=opened):
                with self.assertRaises(legacy.Refusal):
                    legacy.executable_pins(request)
                retire.executable_pins(request)
            with real_open(executable, "r+b") as file:
                file.seek(32 * 1024 * 1024 + 1)
                file.write(b"X")
            with patch("builtins.open", side_effect=opened), self.assertRaises(retire.Refusal):
                retire.executable_pins(request)

    def test_executable_size_ceiling_refuses_instead_of_hashing_a_prefix(self):
        real_open = open
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "oversize-executable"
            executable.write_bytes(b"x" * 65)
            sha = retire.digest(executable.read_bytes())
            request = {"ssh": {"interpreter_sha256": sha, "sudo_sha256": sha},
                       "executables": {"docker_sha256": sha, "runuser_sha256": sha,
                                       "client_path": "/usr/lib/postgresql/17/bin/psql", "client_sha256": sha}}
            with patch.object(retire, "MAX_EXECUTABLE_BYTES", 64), patch("builtins.open", side_effect=lambda path, mode: real_open(executable, mode)), self.assertRaises(retire.Refusal):
                retire.executable_pins(request)


if __name__ == "__main__":
    unittest.main()
