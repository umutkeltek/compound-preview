"""Closed, ephemeral runtime configuration installer (legacy echo-only).

This file is the BYTE-IDENTICAL preserved snapshot of the pre-Phase-1 program
that never wrote any remote file in production. The current owner was found to
echo metadata only; Phase-1 replaces the live entry point at
`runtime_config_install_program.py` with the REAL filesystem transaction
engine. This legacy snapshot exists solely so that the negative-control
evidence (`runtime_config_install_program_echo_only_negative_test.py`) can
prove the OLD behaviour produced zero files when the same wire is fed to it.

The Go owner binds `runtime_config_install_program.py` only. This file is not
embedded; it is never a production entry point.
"""
import hashlib
import json
import os
import re
import signal
import sys
import time


class Refusal(Exception):
    pass


def require(condition):
    if not condition:
        raise Refusal()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def decode_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda _: require(False))


def exact_keys(value, keys):
    require(type(value) is dict and set(value) == set(keys.split()))


def canonical_digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":"),
                                     ensure_ascii=True).encode("ascii")).hexdigest()


def validate_request(r):
    exact_keys(r, "schema_version profile_id attempt_id program_sha256 effect_set witness resource ssh host files roles database posture forced_inactive")
    for key, keys in {
        "witness": "ref sha256 observed_at",
        "resource": "account region instance address",
        "ssh": "alias user port identity_file known_hosts_file known_hosts_sha256 host_key_sha256 interpreter_sha256 client_path client_sha256 client_version sudo_sha256",
        "host": "hostname machine_id boot_id system_identifier version_num",
        "database": "name allow_connections endpoint_host endpoint_port scheme",
        "posture": "production live_fetch checkout_enabled",
    }.items():
        exact_keys(r[key], keys)
    require(r["schema_version"] == "compound.runtime_config_installation_request.v0.1")
    require(r["effect_set"] == "inactive-aws-staging-runtime-configuration-v1")
    require(re.fullmatch(r"[0-9]{8}t[0-9]{6}z-[0-9a-f]{16}", r["attempt_id"]) is not None)
    require(re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,127}", r["profile_id"]) is not None)
    require(r["forced_inactive"] is True)
    require(r["posture"]["production"] is True)
    require(r["posture"]["live_fetch"] is False)
    require(r["posture"]["checkout_enabled"] is False)
    require(type(r["files"]) is list and len(r["files"]) == 4)
    basenames = []
    for index, file in enumerate(r["files"]):
        exact_keys(file, "basename path device inode bytes mtime_ns ctime_ns sha256")
        require(re.fullmatch(r"[\.a-zA-Z0-9_-]{1,128}", file["basename"]) is not None)
        require(file["path"].startswith("/") and os.path.normpath(file["path"]) == file["path"])
        require("\x00" not in file["path"])
        require(int(file["bytes"]) >= 0 and int(file["bytes"]) <= 16384)
        require(re.fullmatch(r"[a-f0-9]{64}", file["sha256"]) is not None)
        basenames.append(file["basename"])
    require(basenames == [".env", "cardintel.env", "migrator.env", "web.env"])
    require(type(r["roles"]) is list and len(r["roles"]) == 2)
    for index, role in enumerate(r["roles"]):
        exact_keys(role, "kind key name source_host source_database")
        require(role["kind"] == ("application", "migrator")[index])
        require(re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", role["name"]) is not None)
    require(r["roles"][0]["name"] != r["roles"][1]["name"])
    require(r["database"]["endpoint_port"] > 0 and r["database"]["endpoint_port"] < 65536)
    require(r["database"]["name"] != "postgres")
    require(r["database"]["scheme"] in ("postgresql", "postgresql+psycopg"))
    require(r["database"]["endpoint_host"] != "")


def remote_budget(issued_ns, deadline_ns, now_ns, monotonic_now):
    require(type(issued_ns) is str and re.fullmatch(r"[0-9]{19}", issued_ns) is not None)
    require(type(deadline_ns) is str and re.fullmatch(r"[0-9]{19}", deadline_ns) is not None)
    issued, deadline = int(issued_ns), int(deadline_ns)
    require(0 < deadline - issued <= 50_000_000_000)
    require(-2_000_000_000 <= now_ns - issued <= 15_000_000_000)
    budget = min(43.0, (deadline - now_ns) / 1_000_000_000 - 4.0)
    require(budget > 0)
    return monotonic_now + budget


# Legacy echo-only main. Never writes a file. Preserved for negative-control
# evidence: feeding the SAME wire into main_legacy_echo_only on a synthetic
# tempdir proves the OLD path leaves the filesystem state untouched.
def main_legacy_echo_only():
    accepted = False
    observation = None
    def interrupted(_signum, _frame):
        raise Refusal()
    try:
        signal.signal(signal.SIGALRM, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGHUP, interrupted)
        signal.setitimer(signal.ITIMER_REAL, 46.0)
        raw = sys.stdin.buffer.read(32769)
        require(0 < len(raw) <= 32768)
        wire = decode_json(raw)
        exact_keys(wire, "request issued_ns deadline_ns")
        r = wire["request"]
        require(len(sys.argv) == 2 and r["program_sha256"] == sys.argv[1])
        deadline = remote_budget(wire["issued_ns"], wire["deadline_ns"], time.time_ns(), time.monotonic())
        signal.setitimer(signal.ITIMER_REAL, max(0.001, deadline - time.monotonic()))
        validate_request(r)
        preimages = []
        for file in r["files"]:
            preimages.append({
                "basename": file["basename"], "device": int(file["device"]),
                "inode": int(file["inode"]), "bytes": int(file["bytes"]),
                "mtime_ns": file["mtime_ns"], "ctime_ns": file["ctime_ns"],
                "sha256": file["sha256"],
            })
        observation = {
            "schema_version": "compound.runtime_config_installation_observation.v0.1",
            "request_sha256": canonical_digest(r),
            "program_sha256": r["program_sha256"],
            "host_before": r["host"], "host_after": r["host"],
            "file_matched": True, "preimages": preimages,
            "postures": [{"basename": file["basename"], "written": False, "mode": "0000", "owner": 0, "group": 0} for file in r["files"]],
            "installed_basenames": [file["basename"] for file in r["files"]],
            "configuration_accepted": False,
            "application_activated": False,
            "service_activated": False,
        }
        require(time.monotonic() < deadline)
        accepted = True
    except BaseException:
        accepted = False
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        for signum in (signal.SIGTERM, signal.SIGHUP):
            signal.signal(signum, signal.SIG_IGN)
    if accepted:
        try:
            sys.stdout.write(json.dumps(observation, separators=(",", ":"), sort_keys=True) + "\n")
            sys.stdout.flush()
            return 0
        except BaseException:
            pass
    try:
        sys.stderr.write("runtime_config_install:refused\n")
    except BaseException:
        pass
    return 1


if __name__ == "__main__":
    sys.exit(main_legacy_echo_only())
