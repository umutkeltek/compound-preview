"""Retire one source-comparison marker after native accepted runtime installation.
The four environment files and inactive marker are inspected only with lstat.
"""
import ctypes
import fcntl
import hashlib
import json
import os
import re
import socket
import stat
import subprocess
import sys
import time

LEGACY_PROGRAM_SHA256 = "b517afa4728bf99bc9a1e347b186641ea921f2cbb2ae7df9adb81ba854c51487"
EXECUTABLE_CHUNK_BYTES = 1024 * 1024
MAX_EXECUTABLE_BYTES = 256 * 1024 * 1024

class Refusal(Exception):
    pass

def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()

def digest(raw):
    return hashlib.sha256(raw).hexdigest()

def owner_ok(s):
    return s.st_uid == 0 and s.st_gid == 0

def metadata(s):
    return {"device": s.st_dev, "inode": s.st_ino, "bytes": s.st_size, "mtime_ns": str(s.st_mtime_ns), "ctime_ns": str(s.st_ctime_ns), "mode": f"{stat.S_IMODE(s.st_mode):04o}", "uid": s.st_uid, "gid": s.st_gid, "links": s.st_nlink, "sha256": ""}

def without_hash(value):
    return dict(value, sha256="")

def exact_file(parent, name, expected):
    value = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if not stat.S_ISREG(value.st_mode) or not owner_ok(value) or value.st_nlink != 1 or metadata(value) != without_hash(expected):
        raise Refusal()
    return metadata(value)

def open_tree(path, leaf_private=True):
    if not path.startswith("/") or os.path.normpath(path) != path:
        raise Refusal()
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in path.split("/")[1:]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            s = os.fstat(fd)
            if not owner_ok(s) or stat.S_IMODE(s.st_mode) & 0o022:
                raise Refusal()
        if leaf_private and stat.S_IMODE(os.fstat(fd).st_mode) != 0o700:
            raise Refusal()
        return fd
    except BaseException:
        os.close(fd)
        raise

def identity():
    with open("/etc/machine-id", "r", encoding="ascii") as file:
        machine = file.read().strip()
    with open("/proc/sys/kernel/random/boot_id", "r", encoding="ascii") as file:
        boot = file.read().strip()
    return {"hostname": socket.gethostname(), "machine_id": machine, "boot_id": boot}

def check_deadline(wire):
    if time.time_ns() >= int(wire["deadline_ns"]):
        raise Refusal()

def executable_pins(request):
    for path, sha in (("/usr/bin/python3", request["ssh"]["interpreter_sha256"]), ("/usr/bin/sudo", request["ssh"]["sudo_sha256"]), ("/usr/bin/docker", request["executables"]["docker_sha256"]), ("/usr/sbin/runuser", request["executables"]["runuser_sha256"]), (request["executables"]["client_path"], request["executables"]["client_sha256"])):
        with open(path, "rb") as file:
            if os.fstat(file.fileno()).st_size > MAX_EXECUTABLE_BYTES:
                raise Refusal()
            sha256 = hashlib.sha256()
            total = 0
            while True:
                chunk = file.read(EXECUTABLE_CHUNK_BYTES)
                if not chunk:
                    break
                total += len(chunk)
                if total > MAX_EXECUTABLE_BYTES:
                    raise Refusal()
                sha256.update(chunk)
            if sha256.hexdigest() != sha:
                raise Refusal()

def remaining_timeout(wire, maximum):
    remaining_ns = int(wire["deadline_ns"]) - time.time_ns()
    if remaining_ns <= 0:
        raise Refusal()
    return min(maximum, remaining_ns / 10**9)

def closed_runtime(request, wire):
    env = {"PATH": "/usr/bin:/bin", "HOME": "/nonexistent", "LC_ALL": "C", "LANG": "C"}
    result = subprocess.run(["/usr/bin/docker", "container", "ls", "--all", "--format", "{{.Names}}"], capture_output=True, text=True, timeout=remaining_timeout(wire, 10), env=env)
    if result.returncode != 0 or any(name.startswith(request["container_prefix"]) for name in result.stdout.splitlines()):
        raise Refusal()
    name = request["database"]["name"]
    if not re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", name):
        raise Refusal()
    query = "SELECT json_build_object('system_identifier',(SELECT system_identifier::text FROM pg_control_system()),'version_num',current_setting('server_version_num'),'oid',(SELECT oid::text FROM pg_database WHERE datname='" + name + "'),'allow_connections',(SELECT datallowconn FROM pg_database WHERE datname='" + name + "'),'connection_limit',(SELECT datconnlimit FROM pg_database WHERE datname='" + name + "'),'clients',(SELECT count(*) FROM pg_stat_activity WHERE datname='" + name + "'))"
    env.update({"PGHOST": "/var/run/postgresql", "PGPORT": "5432", "PGUSER": "postgres", "PGPASSFILE": "/nonexistent", "PGSERVICEFILE": "/nonexistent", "PGSYSCONFDIR": "/nonexistent", "PGOPTIONS": "-c default_transaction_read_only=on -c statement_timeout=10000 -c lock_timeout=2000"})
    result = subprocess.run(["/usr/sbin/runuser", "-u", "postgres", "--", request["executables"]["client_path"], "-XAtqw", "-v", "ON_ERROR_STOP=1", "-d", "postgres", "-c", query], capture_output=True, text=True, timeout=remaining_timeout(wire, 15), env=env)
    expected = {"system_identifier": request["host"]["system_identifier"], "version_num": request["host"]["version_num"], "oid": request["database"]["oid"], "allow_connections": request["database"]["allow_connections"], "connection_limit": 0, "clients": 0}
    if result.returncode != 0 or json.loads(result.stdout) != expected:
        raise Refusal()

def write_all(fd, body):
    view = memoryview(body)
    while view:
        count = os.write(fd, view)
        if count <= 0:
            raise Refusal()
        view = view[count:]

def journal(parent, request):
    body = canonical({"schema": "compound.source_marker_retirement_journal.v0.1", "request_sha256": digest(canonical(request))})
    fd = os.open("PREPARED.json", os.O_RDWR | os.O_NONBLOCK | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600, dir_fd=parent)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or not owner_ok(s) or stat.S_IMODE(s.st_mode) != 0o600 or s.st_nlink != 1 or s.st_size > len(body):
            raise Refusal()
        prefix = os.pread(fd, len(body) + 1, 0)
        if prefix != body[:len(prefix)]:
            raise Refusal()
        if prefix != body:
            write_all(fd, body[len(prefix):])
            os.fsync(fd)
    finally:
        os.close(fd)
    os.fsync(parent)

def rename_no_replace(source_parent, source, target_parent, target):
    libc = ctypes.CDLL(None, use_errno=True)
    rename = libc.renameat2
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(source_parent, source.encode("ascii"), target_parent, target.encode("ascii"), 1) != 0:
        raise Refusal()

def retained_inode(before, after):
    return all(before[key] == after[key] for key in ("device", "inode", "bytes", "mtime_ns", "mode", "uid", "gid", "links")) and int(after["ctime_ns"]) >= int(before["ctime_ns"])

def retire(root, request, check):
    before = [exact_file(root, file["basename"], file["stat"]) for file in request["files"]]
    inactive = exact_file(root, request["inactive"]["basename"], request["inactive"]["stat"])
    check()
    try:
        os.mkdir(request["recovery_basename"], 0o700, dir_fd=root)
        os.fsync(root)
    except FileExistsError:
        pass
    recovery = os.open(request["recovery_basename"], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root)
    try:
        s = os.fstat(recovery)
        if not owner_ok(s) or stat.S_IMODE(s.st_mode) != 0o700:
            raise Refusal()
        if set(os.listdir(recovery)) - {"PREPARED.json", request["marker"]["basename"] + ".preimage"}:
            raise Refusal()
        journal(recovery, request)
        name = request["marker"]["basename"]
        copy = name + ".preimage"
        try:
            marker_before = exact_file(root, name, request["marker"]["stat"])
        except FileNotFoundError:
            marker_before = None
        if marker_before is not None:
            check()
            rename_no_replace(root, name, recovery, copy)
            os.fsync(recovery)
            os.fsync(root)
        copied = os.stat(copy, dir_fd=recovery, follow_symlinks=False)
        if not stat.S_ISREG(copied.st_mode) or not owner_ok(copied) or copied.st_nlink != 1 or not retained_inode(without_hash(request["marker"]["stat"]), metadata(copied)):
            raise Refusal()
        try:
            os.stat(name, dir_fd=root, follow_symlinks=False)
        except FileNotFoundError:
            pass
        else:
            raise Refusal()
        after = [exact_file(root, file["basename"], file["stat"]) for file in request["files"]]
        inactive_after = exact_file(root, request["inactive"]["basename"], request["inactive"]["stat"])
        check()
        return marker_before, metadata(copied), before, after, inactive, inactive_after
    finally:
        os.close(recovery)

def main():
    if os.geteuid() != 0 or len(sys.argv) != 2:
        raise Refusal()
    raw = sys.stdin.buffer.read(65537)
    if len(raw) > 65536:
        raise Refusal()
    wire = json.loads(raw)
    if set(wire) != {"request", "operation", "deadline_ns"} or wire["operation"] not in ("retire", "reconcile"):
        raise Refusal()
    request = wire["request"]
    if request["schema_version"] != "compound.source_marker_retirement_request.v0.1" or request["effect_set"] != "metadata-only-source-marker-rename-after-native-acceptance" or (request["program_sha256"] != sys.argv[1] and not (wire["operation"] == "reconcile" and request["program_sha256"] == LEGACY_PROGRAM_SHA256)) or not re.fullmatch(r"[a-z][a-z0-9.-]{1,79}", request["recovery_basename"]) or len(request["files"]) != 4:
        raise Refusal()
    root_path = request["root"]["path"]
    if not root_path.startswith("/") or os.path.normpath(root_path) != root_path or root_path == "/" or not re.fullmatch(r"[a-z][a-z0-9.-]{1,79}", request["container_prefix"]):
        raise Refusal()
    names = [file["basename"] for file in request["files"]]
    if len(set(names)) != 4 or any(not re.fullmatch(r"[a-zA-Z0-9._-]{1,127}", name) or name in (".", "..") for name in names):
        raise Refusal()
    for file in request["files"]:
        if file["path"] != root_path + "/" + file["basename"]:
            raise Refusal()
    if request["marker"]["basename"] != names[0] + ".SOURCE_COMPARISON_ONLY" or request["marker"]["path"] != root_path + "/" + request["marker"]["basename"] or request["inactive"]["basename"] != ".runtime-staging-inactive" or request["inactive"]["path"] != root_path + "/.runtime-staging-inactive" or request["maintenance_lock"]["path"] != "/run/lock/" + request["maintenance_lock"]["basename"] or not re.fullmatch(r"[a-zA-Z0-9._-]{1,127}", request["maintenance_lock"]["basename"]) or request["maintenance_lock"]["basename"] in (".", ".."):
        raise Refusal()
    host = {key: request["host"][key] for key in ("hostname", "machine_id", "boot_id")}
    if identity() != host:
        raise Refusal()
    executable_pins(request)
    check_deadline(wire)
    root = open_tree(request["root"]["path"])
    lock = None
    try:
        lock_path = request["maintenance_lock"]["path"]
        s = os.lstat(lock_path)
        if not stat.S_ISREG(s.st_mode) or metadata(s) != without_hash(request["maintenance_lock"]["stat"]):
            raise Refusal()
        lock = os.open(lock_path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
        opened = os.fstat(lock)
        if not stat.S_ISREG(opened.st_mode) or metadata(opened) != metadata(s):
            raise Refusal()
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        def check():
            check_deadline(wire)
            if identity() != host:
                raise Refusal()
            closed_runtime(request, wire)
            check_deadline(wire)
        values = retire(root, request, check)
    finally:
        if lock is not None:
            os.close(lock)
        os.close(root)
    check_deadline(wire)
    marker_before, marker_after, before, after, inactive, inactive_after = values
    result = {"schema_version": "compound.source_marker_retirement_observation.v0.1", "operation": wire["operation"], "changed": marker_before is not None, "request_sha256": digest(canonical(request)), "program_sha256": sys.argv[1], "transaction": ".smr-" + request["attempt_id"], "state": "source_marker_retired", "host_before": request["host"], "host_after": request["host"], "runtime_before": before, "runtime_after": after, "marker_before": marker_before, "marker_after": marker_after, "inactive_before": inactive, "inactive_after": inactive_after, "journal_retained": True, "source_marker_retired": True, "marker_inode_preserved": True, "database_closed": True, "application_containers_absent": True, "credential_bodies_opened": False, "configuration_rewritten": False, "database_mutated": False, "application_activated": False, "service_activated": False}
    sys.stdout.buffer.write(canonical(result) + b"\n")
    sys.stdout.buffer.flush()

if __name__ == "__main__":
    try:
        main()
    except BaseException:
        os._exit(1)
