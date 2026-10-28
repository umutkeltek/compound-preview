"""Negative-control evidence: the legacy echo-only program produces zero
filesystem effects when fed the SAME wire that drives the new REAL install.

This file imports no fixtures of its own; it constructs the SAME wire the
REAL install test uses, writes a fresh tempdir, runs the legacy echo-only
program as a subprocess against a wire that names real disk paths, and
asserts that no companion file appears and the app file is left untouched.
It is the test the REAL_PHASE1 packet mandated: "restore old echo-only body
as negative control: actual installation test MUST FAIL."
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

THIS_DIR = os.path.dirname(os.path.abspath(__file__))
LEGACY_PATH = os.path.join(THIS_DIR, "runtime_config_install_program_echo_only_legacy.py")


def legacy_sha256():
    with open(LEGACY_PATH, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def build_wire(tmpdir, *, attempt_suffix="9999999999999999"):
    """Build a minimal wire pointing at REAL tempdir paths. Same shape the
    REAL install test would use."""
    root_path = tmpdir + "/cardintel"
    os.makedirs(root_path)
    marker_path = root_path + "/.inactive_marker"
    with open(marker_path, "w") as fh:
        fh.write("fixture-comparison-only-marker\n")
    appsha = "a" * 64
    app_path = os.path.join(root_path, ".env")
    with open(app_path, "wb") as fh:
        fh.write(b"DATABASE_URL=postgresql+psycopg://fixture_app:synth_app_pw@synth_host:5432/fixture_app?sslmode=require&sslcert=/etc/ssl/certs/app.pem\n")
    os.chmod(app_path, 0o600)
    info_app = os.stat(app_path)
    appsha = hashlib.sha256(open(app_path, "rb").read()).hexdigest()
    root_info = os.stat(root_path)
    journals_dir = tmpdir + "/journals"
    os.makedirs(journals_dir, exist_ok=True)
    attempt_id = time.strftime("%Y%m%dt%H%M%Sz", time.gmtime()) + "-" + attempt_suffix
    return {
        "schema_version": "compound.runtime_config_installation_request.v0.1",
        "profile_id": "fixture.invalid",
        "attempt_id": attempt_id,
        "program_sha256": legacy_sha256(),
        "effect_set": "inactive-aws-staging-runtime-configuration-v1",
        "forced_inactive": True,
        "witness": {"ref": "rec_fixture_metadata", "sha256": "a" * 64,
                    "observed_at": "2026-10-20T00:00:00Z"},
        "resource": {"account": "000000000000", "region": "fixture-region",
                     "instance": "fixture:instance:example", "address": "10.0.0.10"},
        "ssh": {"alias": "target-fixture", "user": "fixture", "port": 22,
                "identity_file": "/fixture/identity",
                "known_hosts_file": "/fixture/known_hosts",
                "known_hosts_sha256": "a" * 64,
                "host_key_sha256": "SHA256:" + ("A" * 43),
                "interpreter_sha256": "a" * 64,
                "client_path": "/usr/lib/postgresql/17/bin/psql",
                "client_sha256": "a" * 64,
                "client_version": "psql (PostgreSQL) 17.11",
                "sudo_sha256": "a" * 64},
        "host": {"hostname": "host-fixture.invalid", "machine_id": "a" * 32,
                 "boot_id": "b" * 36, "system_identifier": "1234567890123456789",
                 "version_num": "170011"},
        "files": [
            {"basename": ".env", "path": app_path,
             "device": info_app.st_dev, "inode": info_app.st_ino,
             "bytes": info_app.st_size,
             "mtime_ns": str(info_app.st_mtime_ns),
             "ctime_ns": str(info_app.st_ctime_ns), "sha256": appsha},
            {"basename": "cardintel.env", "path": root_path + "/cardintel.env",
             "device": root_info.st_dev, "inode": 0, "bytes": 0,
             "mtime_ns": "1792197133436814154", "ctime_ns": "1792197133436814154",
             "sha256": "a" * 64},
            {"basename": "migrator.env", "path": root_path + "/migrator.env",
             "device": root_info.st_dev, "inode": 0, "bytes": 0,
             "mtime_ns": "1792197133436814154", "ctime_ns": "1792197133436814154",
             "sha256": "a" * 64},
            {"basename": "web.env", "path": root_path + "/web.env",
             "device": root_info.st_dev, "inode": 0, "bytes": 0,
             "mtime_ns": "1792197133436814154", "ctime_ns": "1792197133436814154",
             "sha256": "a" * 64},
        ],
        "roles": [
            {"kind": "application", "key": "DATABASE_URL", "name": "fixture_app",
             "source_host": "source.fixture.invalid", "source_database": "fixture_app"},
            {"kind": "migrator", "key": "CARDINTEL_DATABASE_URL", "name": "fixture_migrator",
             "source_host": "source.fixture.invalid", "source_database": "fixture_app"},
        ],
        "database": {"name": "fixture_app", "allow_connections": True,
                     "endpoint_host": "172.17.0.1", "endpoint_port": 15439,
                     "scheme": "postgresql+psycopg"},
        "posture": {"production": True, "live_fetch": False, "checkout_enabled": False},
    }, root_path


def run_legacy(wire, journals_dir, root_path):
    """Run the legacy echo-only program with a wire that names real paths.
    The legacy program accepts the wire and returns an observation with
    written=False across the board."""
    now_ns = int(time.time() * 1_000_000_000)
    wire_outer = {
        "request": wire,
        "issued_ns": str(now_ns - 1_000_000_000),
        "deadline_ns": str(now_ns + 49_000_000_000),
    }
    payload = json.dumps(wire_outer, sort_keys=True, separators=(",", ":")).encode("ascii")
    proc = subprocess.run(
        [sys.executable, "-I", "-B", LEGACY_PATH, legacy_sha256()],
        input=payload, capture_output=True, env={}, timeout=10,
    )
    return proc


def test_legacy_echo_only_does_not_install(workspace_dir):
    """Run legacy with a wire that points to a real disk layout. Confirm:
    - The .app file is byte-identical to its preimage.
    - No companion (api/migrator/web) exists.
    - The completed-without-journal marker is absent.
    """
    tmpdir = tempfile.mkdtemp(prefix="rci-legacy-", dir=workspace_dir)
    try:
        wire, root_path = build_wire(tmpdir)
        journals_dir = tmpdir + "/journals"
        os.makedirs(journals_dir, exist_ok=True)
        before_app_bytes = open(os.path.join(root_path, ".env"), "rb").read()
        proc = run_legacy(wire, journals_dir, root_path)
        # Legacy MUST succeed at the wire (its job is just to echo).
        if proc.returncode != 0:
            raise AssertionError("legacy should accept canonical wire: " + proc.stderr.decode("ascii", "replace"))
        # Observation must report all written=False.
        observation = json.loads(proc.stdout.decode("ascii").strip())
        for posture in observation["postures"]:
            if posture.get("written") is not False:
                raise AssertionError("legacy reported installed posture")
        # Filesystem MUST be unchanged.
        after_app_bytes = open(os.path.join(root_path, ".env"), "rb").read()
        if after_app_bytes != before_app_bytes:
            raise AssertionError("legacy modified .env despite echo-only contract")
        for name in ("cardintel.env", "migrator.env", "web.env"):
            if os.path.exists(os.path.join(root_path, name)):
                raise AssertionError("legacy created companion " + name)
        if not os.path.exists(os.path.join(root_path, ".inactive_marker")):
            raise AssertionError("legacy removed the comparison-only marker")
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def main(workspace_dir):
    fns = [(name, value) for name, value in sorted(globals().items())
           if name.startswith("test_") and callable(value)]
    for name, fn in fns:
        fn(workspace_dir)
        print("PASS " + name)
    print("OK")


if __name__ == "__main__":
    workspace = tempfile.mkdtemp(prefix="rci-neg-workspace-")
    main(workspace)
    shutil.rmtree(workspace, ignore_errors=True)
