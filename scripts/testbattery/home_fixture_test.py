"""Filesystem and direct-test falsifiers for the optional fixture accelerator."""
import importlib.util
import contextlib
import os
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("fixture", Path(__file__).with_name("home_fixture.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)

class FixtureSafety(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.stage = self.root / "cache" / "stage"
        self.pin = fixture.PinnedStage(self.stage, create=True)
        fixture.PIN = self.pin
        fixture.ARGS = SimpleNamespace(fds=())
        fixture.write_leaf(self.stage / "owner", b"test-owner")
        fixture.make_directory(self.stage / "scratch")
        self.outside = self.root / "outside"
        self.outside.mkdir()
        (self.outside / "sentinel").write_bytes(b"preserve outside")

    def tearDown(self):
        fixture.REMOVE_HOOK = lambda *args: None
        self.pin.close()
        fixture.PIN = None
        self.temporary.cleanup()

    def assertOutside(self):
        self.assertEqual((self.outside / "sentinel").read_bytes(), b"preserve outside")

    def test_cache_root_permissions_are_required(self):
        self.pin.close()
        self.stage.parent.chmod(0o755)
        try:
            with self.assertRaises(RuntimeError):
                fixture.PinnedStage(self.stage)
            absent = self.stage.parent / "not-created"
            with self.assertRaises(RuntimeError):
                fixture.PinnedStage(absent, create=True)
            self.assertFalse(absent.exists())
        finally:
            self.stage.parent.chmod(0o700)

    def test_scratch_swap_never_traverses_outside(self):
        scratch = self.stage / "scratch"
        fixture.write_leaf(scratch / "owned", b"owned")
        def swap(*_):
            scratch.rename(self.stage / "scratch-old")
            scratch.symlink_to(self.outside, target_is_directory=True)
        fixture.REMOVE_HOOK = swap
        with self.assertRaises((OSError, RuntimeError)):
            fixture.clear_directory(scratch)
        self.assertOutside()

    def test_ancestor_swap_refuses_before_outside_mutation(self):
        replacement = self.outside / "stage" / "scratch"
        replacement.mkdir(parents=True)
        (replacement / "sentinel").write_bytes(b"preserve replacement")
        def swap(*_):
            self.stage.parent.rename(self.root / "original-cache")
            self.stage.parent.symlink_to(self.outside, target_is_directory=True)
        fixture.REMOVE_HOOK = swap
        with self.assertRaises(RuntimeError):
            fixture.clear_directory(self.stage / "scratch")
        self.assertEqual((replacement / "sentinel").read_bytes(), b"preserve replacement")
        self.assertOutside()

    def test_child_replacement_is_preserved(self):
        child = self.stage / "scratch" / "child"
        fixture.make_directory(child)
        fixture.write_leaf(child / "owned", b"owned")
        def swap(parent, name, fd):
            if name == "child":
                child.rename(child.with_name("old"))
                child.mkdir()
                (child / "replacement").write_bytes(b"preserve")
        fixture.REMOVE_HOOK = swap
        with self.assertRaises(RuntimeError):
            fixture.remove_path(child)
        self.assertEqual((child / "replacement").read_bytes(), b"preserve")

    def test_poisoned_persistent_leaves_never_overwrite_outside(self):
        for name in ("probe.go", "policy.sb", "outside-sentinel"):
            for kind in ("symlink", "hardlink"):
                with self.subTest(name=name, kind=kind):
                    target = self.stage / name
                    if kind == "symlink":
                        target.symlink_to(self.outside / "sentinel")
                    else:
                        os.link(self.outside / "sentinel", target)
                    with self.assertRaises((OSError, RuntimeError)):
                        fixture.write_leaf(target, b"unsafe overwrite")
                    self.assertOutside()
                    target.unlink()

    def test_identical_write_preserves_inode_and_timestamps(self):
        path = self.stage / "probe.go"
        fixture.write_leaf(path, b"same", 0o600)
        before = path.stat()
        fixture.write_leaf(path, b"same", 0o600)
        after = path.stat()
        self.assertEqual((before.st_ino, before.st_mtime_ns, before.st_ctime_ns),
                         (after.st_ino, after.st_mtime_ns, after.st_ctime_ns))

    def test_budget_exhaustion_preserves_owner_and_allows_exact_retry(self):
        child = self.stage / "scratch" / "child"
        fixture.make_directory(child)
        fixture.write_leaf(child / "first", b"first")
        fixture.write_leaf(child / "second", b"second")
        with self.assertRaises(fixture.CacheUnavailable):
            fixture.remove_path(child, fixture.RemovalBudget(operations=1))
        self.assertEqual(fixture.read_leaf(self.stage / "owner"), b"test-owner")
        fixture.remove_path(child)
        self.assertFalse(child.exists())

    def test_retirement_refuses_wrong_owner_and_retains_only_exact_marker(self):
        fixture.ARGS.identity = "wrong"
        fixture.write_leaf(self.stage / "home.test", b"binary")
        with self.assertRaises(RuntimeError):
            fixture.retire(self.stage)
        self.assertEqual(fixture.read_leaf(self.stage / "home.test"), b"binary")
        fixture.ARGS.identity = "test-owner"
        fixture.retire(self.stage)
        self.assertEqual(set(os.listdir(self.stage)), {"owner", "scratch"})
        self.assertEqual(fixture.read_leaf(self.stage / "owner"), b"test-owner")
        self.assertOutside()

    def test_completed_execution_drift_never_certifies_or_replays(self):
        execute, check = fixture.execute_tests, fixture.require_inputs
        calls = []
        try:
            fixture.execute_tests = lambda stage: calls.append("execute") or 0
            def drift(*args):
                calls.append("check")
                raise fixture.CacheUnavailable("observable ancestor metadata changed")
            fixture.require_inputs = drift
            self.assertEqual(fixture.execute_sealed_tests(self.stage, {}), 76)
            self.assertEqual(calls, ["execute", "check"])
            for status in (1, 75, 76):
                calls.clear()
                fixture.execute_tests = lambda stage, status=status: status
                self.assertEqual(fixture.execute_sealed_tests(self.stage, {}), 1)
                self.assertEqual(calls, [])
            fixture.execute_tests = lambda stage: 0
            def corrupted(*args):
                raise RuntimeError("fixture source integrity mismatch")
            fixture.require_inputs = corrupted
            with self.assertRaises(RuntimeError):
                fixture.execute_sealed_tests(self.stage, {})
        finally:
            fixture.execute_tests, fixture.require_inputs = execute, check


    @contextlib.contextmanager
    def host_inputs(self):
        for name in ("home.test", "probe", "policy.sb"):
            fixture.write_leaf(self.stage / name, b"synthetic fixture input")
        fixture.make_directory(self.stage / "source")
        policy = self.stage / "synthetic-system.sb"
        fixture.write_leaf(policy, b"synthetic system policy")
        values = {
            "kern.osversion": "synthetic-build",
            "kern.boottime": "{ sec = 1000, usec = 100 }",
            "kern.bootsessionuuid": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
            "hw.machine": "arm64",
            "hw.logicalcpu": "8",
        }

        def sysctl(args, **kwargs):
            self.assertEqual(args[:2], ["/usr/sbin/sysctl", "-n"])
            value = values[args[2]]
            if isinstance(value, Exception):
                raise value
            return value.encode()

        # These tests assert host identity only. The real ancestor scan lists the
        # shared TMPDIR, which parallel Go tests churn, so an entry could vanish
        # between listing and stat (local battery flake). Production stages live
        # in a quiet shared cache outside TMPDIR instead.
        with patch.object(fixture, "run", side_effect=sysctl), patch.object(fixture, "SYSTEM_POLICY", policy), \
                patch.object(fixture, "ancestor_digest", return_value="synthetic-ancestors"):
            yield values

    def test_host_identity_survives_boot_timestamp_clock_adjustment(self):
        with self.host_inputs() as values:
            before = fixture.inputs(self.stage)
            values["kern.boottime"] = "{ sec = 1000, usec = 64417 }"
            self.assertEqual(fixture.inputs(self.stage)["host_sha256"], before["host_sha256"])

    def test_host_identity_detects_reboot_even_with_same_boot_timestamp(self):
        with self.host_inputs() as values:
            before = fixture.inputs(self.stage)
            values["kern.bootsessionuuid"] = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
            after = fixture.inputs(self.stage)
            self.assertNotEqual(after["host_sha256"], before["host_sha256"])

    def test_host_identity_keeps_other_host_inputs_sealed(self):
        with self.host_inputs() as values:
            before = fixture.inputs(self.stage)["host_sha256"]
            values["kern.osversion"] = "replacement-build"
            self.assertNotEqual(fixture.inputs(self.stage)["host_sha256"], before)

    def test_host_identity_refuses_unavailable_or_invalid_boot_uuid(self):
        with self.host_inputs() as values:
            for value in (RuntimeError("sysctl unavailable"), "", "not-a-uuid"):
                with self.subTest(value=str(value)):
                    values["kern.bootsessionuuid"] = value
                    with self.assertRaises(fixture.CacheUnavailable):
                        fixture.inputs(self.stage)


    def test_early_exit_fails_and_unexpected_skip_is_ineligible(self):
        source = self.stage / "source" / "internal" / "home"
        fixture.make_directory(source, parents=True)
        fixture.write_leaf(self.stage / "source" / "go.mod", b"module fixture\n\ngo 1.23\n")
        original = fixture.sandbox_args
        fixture.sandbox_args = lambda stage, binary, args: [str(binary)] + args
        try:
            for body, expected in [('os.Exit(0)', 'failure'), ('os.Exit(76)', 'failure'), ('t.Skip("outside dependency denied")', 'skip')]:
                fixture.write_leaf(source / "home_test.go",
                    ('package home\nimport("os";"testing")\nfunc TestCoverage(t *testing.T){_ = os.Args;' + body + '}\n').encode())
                subprocess.run(["go", "test", "-c", "-o", str(self.stage / "home.test"), "./internal/home"],
                               cwd=self.stage / "source", check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                if expected == "failure":
                    self.assertEqual(fixture.execute_sealed_tests(self.stage, {}), 1)
                else:
                    with self.assertRaises(fixture.CacheUnavailable):
                        fixture.execute_tests(self.stage)
        finally:
            fixture.sandbox_args = original

if __name__ == "__main__":
    unittest.main()
