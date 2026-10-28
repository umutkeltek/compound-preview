import importlib.util
import json
import io
import copy
import types
import time
from unittest.mock import patch
import os
from pathlib import Path
import sys
import tempfile
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("stage", Path(__file__).with_name("backup_credential_stage_program.py"))
stage = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage)

class StageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        uid, gid = os.geteuid(), os.getegid()
        # BSD filesystems (macOS) give a new entry its parent directory's
        # group, not the caller's egid, so a TMPDIR under /private/tmp
        # (root:wheel) makes every fixture file gid 0. Pin the fixture
        # tree's group so the fences compare one known owner on any TMPDIR.
        os.chown(self.root, uid, gid)
        os.chmod(self.root, 0o700)
        self.parent = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.real_owner = stage.owner_ok
        stage.owner_ok = lambda s: s.st_uid == uid and s.st_gid == gid
        self.real_publish = stage.publish_directory
        if sys.platform != "linux":
            def rename(parent, source, target):
                try:
                    os.stat(target, dir_fd=parent, follow_symlinks=False)
                except FileNotFoundError:
                    os.rename(source, target, src_dir_fd=parent, dst_dir_fd=parent)
                else:
                    raise stage.Refusal()
            stage.publish_directory = rename
        self.raw = [("synthetic-private-value-" + name).encode() for name in stage.NAMES]
        self.hashes = [stage.digest(raw) for raw in self.raw]
        self.request = {"profile_id": "fixture-stage", "attempt_id": "20261023t010203z-aaaaaaaaaaaaaaaa", "files": [{"stat": {"bytes": len(raw), "sha256": sha}} for raw, sha in zip(self.raw, self.hashes)]}
    def tearDown(self):
        stage.owner_ok = self.real_owner
        stage.publish_directory = self.real_publish
        os.close(self.parent)
        self.tmp.cleanup()
    def publish(self):
        return stage.publish(self.parent, self.request, self.raw, self.hashes, lambda: None)
    def test_exact_four_create_only_and_metadata_only(self):
        changed, written, posts = self.publish()
        self.assertTrue(changed)
        self.assertEqual(written, [True] * 4)
        final = self.root / "fixture-stage"
        self.assertEqual(set(os.listdir(final)), set(stage.NAMES))
        inodes = [os.lstat(final / name).st_ino for name in stage.NAMES]
        for name, raw in zip(stage.NAMES, self.raw):
            self.assertEqual((final / name).read_bytes(), raw)
            self.assertEqual(os.lstat(final / name).st_mode & 0o7777, 0o600)
        self.assertNotIn("synthetic-private-value", json.dumps(posts))
        self.assertFalse(self.publish()[0])
        self.assertEqual(inodes, [os.lstat(final / name).st_ino for name in stage.NAMES])
        self.assertTrue((self.root / (".bcs-" + self.request["attempt_id"] + ".json")).is_file())
    def seed_partial(self, prefix):
        scratch = self.root / (".bcs-" + self.request["attempt_id"])
        scratch.mkdir(mode=0o700)
        marker = stage.canonical({"schema": "compound.backup_credential_stage_journal.v0.1", "request_sha256": stage.digest(stage.canonical(self.request))})
        journal = self.root / (scratch.name + ".json")
        journal.write_bytes(marker)
        journal.chmod(0o600)
        file = scratch / stage.NAMES[0]
        file.write_bytes(prefix)
        file.chmod(0o600)
        return file
    def test_interrupted_prefix_can_complete_under_same_request(self):
        self.seed_partial(self.raw[0][:7])
        self.assertTrue(self.publish()[0])
        self.assertEqual((self.root / "fixture-stage" / stage.NAMES[0]).read_bytes(), self.raw[0])
    def test_wrong_partial_is_preserved_and_refused(self):
        file = self.seed_partial(b"wrong")
        with self.assertRaises(stage.Refusal):
            self.publish()
        self.assertEqual(file.read_bytes(), b"wrong")
        self.assertFalse((self.root / "fixture-stage").exists())
    def test_symlink_and_foreign_destination_refused(self):
        final = self.root / "fixture-stage"
        final.mkdir(mode=0o700)
        file = final / stage.NAMES[0]
        file.write_bytes(b"existing-value")
        file.chmod(0o600)
        with self.assertRaises((stage.Refusal, FileNotFoundError)):
            self.publish()
        self.assertEqual(file.read_bytes(), b"existing-value")
        final.rename(self.root / "foreign")
        os.symlink(self.root / "foreign", final)
        with self.assertRaises(OSError):
            self.publish()
    def main_wire(self):
        request = copy.deepcopy(self.request)
        program = stage.digest(Path(stage.__file__).read_bytes())
        executable = b"synthetic-system-executable"
        request.update({"schema_version": "compound.backup_credential_stage_request.v0.1", "effect_set": "four-existing-files-root-private-create-only", "program_sha256": program, "root": {"path": "/root/private-recovery/fixture-stage"}, "host": {"hostname": "host.fixture.invalid", "machine_id": "a" * 32, "boot_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, "ssh": {"interpreter_sha256": stage.digest(executable), "sudo_sha256": stage.digest(executable)}})
        for name, file in zip(stage.NAMES, request["files"]):
            file["basename"] = name
            file["path"] = request["root"]["path"] + "/" + name
        wire = {"request": request, "credentials": [__import__("base64").b64encode(raw).decode() for raw in self.raw], "credential_sha256": self.hashes, "operation": "stage", "deadline_ns": str(time.time_ns() + 30 * 10**9)}
        return wire, program, executable
    def run_main(self, wire, program, executable):
        real_open = os.open
        output = io.BytesIO()
        def root_open(path, *args, **kwargs):
            return real_open(self.root if path == "/root" else path, *args, **kwargs)
        with patch.object(stage.os, "geteuid", return_value=0), patch.object(stage.os, "open", side_effect=root_open), patch.object(stage, "identity", return_value=wire["request"]["host"]), patch("builtins.open", side_effect=lambda *args, **kwargs: io.BytesIO(executable)), patch.object(stage.sys, "argv", ["program", program]), patch.object(stage.sys, "stdin", types.SimpleNamespace(buffer=io.BytesIO(json.dumps(wire).encode()))), patch.object(stage.sys, "stdout", types.SimpleNamespace(buffer=output)):
            stage.main()
        return output.getvalue()
    def test_real_main_parser_and_storage_path(self):
        wire, program, executable = self.main_wire()
        raw = self.run_main(wire, program, executable)
        result = json.loads(raw)
        self.assertEqual(result["state"], "staged_backup_credentials")
        self.assertEqual(result["request_sha256"], stage.digest(stage.canonical(wire["request"])))
        self.assertNotIn(b"synthetic-private-value", raw)
        final = self.root / "private-recovery" / "fixture-stage"
        self.assertEqual(set(os.listdir(final)), set(stage.NAMES))
        for name, body in zip(stage.NAMES, self.raw):
            self.assertEqual((final / name).read_bytes(), body)
    def test_real_main_parser_rejects_changed_body_before_destination(self):
        wire, program, executable = self.main_wire()
        wire["credential_sha256"][0] = "0" * 64
        with self.assertRaises(stage.Refusal):
            self.run_main(wire, program, executable)
        self.assertFalse((self.root / "private-recovery").exists())
    def test_remote_fifo_readers_refuse_without_writer(self):
        code = """
import importlib.util, pathlib, os, tempfile
spec=importlib.util.spec_from_file_location("stage",pathlib.Path("backup_credential_stage_program.py"))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
with tempfile.TemporaryDirectory() as tmp:
 p=pathlib.Path(tmp);os.chmod(p,0o700);os.mkfifo(p/"fifo",0o600)
 fd=os.open(p,os.O_RDONLY|os.O_DIRECTORY)
 try:
  for call in (lambda:m.secure_file(fd,"fifo",{"bytes":0,"sha256":""}),lambda:m.journal(fd,"fifo",b"synthetic",False)):
   try:call()
   except (m.Refusal,OSError):pass
   else:raise AssertionError("FIFO accepted")
 finally:os.close(fd)
"""
        subprocess.run([sys.executable, "-I", "-B", "-c", code], check=True, timeout=5)
    def test_deadline_stops_before_publication(self):
        with self.assertRaises(stage.Refusal):
            stage.publish(self.parent, self.request, self.raw, self.hashes, lambda: (_ for _ in ()).throw(stage.Refusal()))
        self.assertFalse((self.root / "fixture-stage").exists())

if __name__ == "__main__":
    unittest.main()
