"""Closed four-existing-file credential handoff. Never installs or activates services."""
import base64
import ctypes
import fcntl
import hashlib
import json
import os
import re
import socket
import stat
import sys
import time

NAMES = ("pgbackrest-secrets.conf", "restic-credentials.json", "restic-password", "offhost-escrow.json")
class Refusal(Exception):
    pass

def digest(raw):
    return hashlib.sha256(raw).hexdigest()

def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()

def deadline(wire):
    if time.time_ns() >= int(wire["deadline_ns"]):
        raise Refusal()

def identity():
    with open("/etc/machine-id", "r", encoding="ascii") as file:
        machine = file.read().strip()
    with open("/proc/sys/kernel/random/boot_id", "r", encoding="ascii") as file:
        boot = file.read().strip()
    return {"hostname": socket.gethostname(), "machine_id": machine, "boot_id": boot}

def owner_ok(s):
    return s.st_uid == 0 and s.st_gid == 0

def write_all(fd, raw):
    view = memoryview(raw)
    while view:
        count = os.write(fd, view)
        if count <= 0:
            raise Refusal()
        view = view[count:]

def publish_directory(parent, source, target):
    libc = ctypes.CDLL(None, use_errno=True)
    rename = libc.renameat2
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(parent, source.encode("ascii"), parent, target.encode("ascii"), 1) != 0:
        raise Refusal()

def journal(parent, name, expected, create):
    if create:
        fd = os.open(name, os.O_RDWR | os.O_NONBLOCK | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600, dir_fd=parent)
    else:
        fd = os.open(name, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=parent)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or not owner_ok(s) or s.st_nlink != 1 or stat.S_IMODE(s.st_mode) != 0o600 or s.st_size > len(expected):
            raise Refusal()
        prefix = os.pread(fd, len(expected) + 1, 0)
        if prefix != expected[:len(prefix)] or (not create and prefix != expected):
            raise Refusal()
        if prefix != expected:
            write_all(fd, expected[len(prefix):])
            os.fsync(fd)
    finally:
        os.close(fd)

def secure_dir(parent, name, create=False):
    if create:
        try:
            os.mkdir(name, 0o700, dir_fd=parent)
            os.fsync(parent)
        except FileExistsError:
            pass
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    s = os.fstat(fd)
    if not owner_ok(s) or stat.S_IMODE(s.st_mode) != 0o700:
        os.close(fd)
        raise Refusal()
    return fd

def secure_file(fd, name, expected):
    f = os.open(name, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=fd)
    try:
        before = os.fstat(f)
        if not stat.S_ISREG(before.st_mode) or not owner_ok(before) or before.st_nlink != 1 or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size != expected["bytes"]:
            raise Refusal()
        raw = os.read(f, 65537)
        after = os.fstat(f)
        if any(getattr(before, key) != getattr(after, key) for key in ("st_dev", "st_ino", "st_size", "st_mtime_ns", "st_ctime_ns", "st_mode", "st_uid", "st_gid", "st_nlink")) or digest(raw) != expected["sha256"]:
            raise Refusal()
        return {"device": after.st_dev, "inode": after.st_ino, "bytes": after.st_size, "mtime_ns": str(after.st_mtime_ns), "ctime_ns": str(after.st_ctime_ns), "mode": "0600", "uid": 0, "gid": 0, "links": 1, "sha256": digest(raw)}
    finally:
        os.close(f)

def publish(parent, request, credentials, hashes, check):
    """Parent is an already opened root-owned0700 recovery directory."""
    files = [dict(item, stat=dict(item["stat"], sha256=sha)) for item, sha in zip(request["files"], hashes)]
    transaction = ".bcs-" + request["attempt_id"]
    lock = os.open(request["profile_id"] + ".lock", os.O_RDWR | os.O_NONBLOCK | os.O_CREAT | os.O_NOFOLLOW, 0o600, dir_fd=parent)
    try:
        ls = os.fstat(lock)
        if not stat.S_ISREG(ls.st_mode) or not owner_ok(ls) or stat.S_IMODE(ls.st_mode) != 0o600 or ls.st_nlink != 1:
            raise Refusal()
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        check()
        final = request["profile_id"]
        try:
            target = secure_dir(parent, final)
        except FileNotFoundError:
            target = None
        marker = canonical({"schema": "compound.backup_credential_stage_journal.v0.1", "request_sha256": digest(canonical(request))})
        if target is not None:
            try:
                journal(parent, transaction + ".json", marker, False)
                if set(os.listdir(target)) != set(NAMES):
                    raise Refusal()
                posts = [secure_file(target, name, file["stat"]) for name, file in zip(NAMES, files)]
                return False, [False] * 4, posts
            finally:
                os.close(target)
        scratch = secure_dir(parent, transaction, create=True)
        try:
            journal(parent, transaction + ".json", marker, True)
            if set(os.listdir(scratch)) - set(NAMES):
                raise Refusal()
            written = [False] * 4
            for i, (name, file, raw) in enumerate(zip(NAMES, files, credentials)):
                check()
                try:
                    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=scratch)
                except FileExistsError:
                    try:
                        secure_file(scratch, name, file["stat"])
                    except Refusal:
                        # Interrupted copies may only append to a verified
                        # prefix in this exact request's private scratch.
                        fd = os.open(name, os.O_RDWR | os.O_NONBLOCK | os.O_APPEND | os.O_NOFOLLOW, dir_fd=scratch)
                        try:
                            before = os.fstat(fd)
                            if not stat.S_ISREG(before.st_mode) or not owner_ok(before) or before.st_nlink != 1 or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size >= len(raw) or os.pread(fd, len(raw) + 1, 0) != raw[:before.st_size]:
                                raise Refusal()
                            write_all(fd, raw[before.st_size:])
                            os.fsync(fd)
                        finally:
                            os.close(fd)
                        written[i] = True
                else:
                    try:
                        write_all(fd, raw)
                        os.fsync(fd)
                    finally:
                        os.close(fd)
                    written[i] = True
                secure_file(scratch, name, file["stat"])
            os.fsync(scratch)
            check()
            # Linux create-only atomic directory publication. No destination
            # directory or existing credential file may ever be overwritten.
            publish_directory(parent, transaction, final)
            os.fsync(parent)
            target = secure_dir(parent, final)
            try:
                if set(os.listdir(target)) != set(NAMES):
                    raise Refusal()
                posts = [secure_file(target, name, file["stat"]) for name, file in zip(NAMES, files)]
                return True, [True] * 4, posts
            finally:
                os.close(target)
        finally:
            os.close(scratch)
    finally:
        os.close(lock)

def main():
    if os.geteuid() != 0 or len(sys.argv) != 2:
        raise Refusal()
    raw = sys.stdin.buffer.read(524289)
    if len(raw) > 524288:
        raise Refusal()
    wire = json.loads(raw)
    if set(wire) != {"request", "credentials", "credential_sha256", "operation", "deadline_ns"} or wire["operation"] not in ("stage", "reconcile"):
        raise Refusal()
    request = wire["request"]
    if request["program_sha256"] != sys.argv[1] or request["schema_version"] != "compound.backup_credential_stage_request.v0.1" or request["effect_set"] != "four-existing-files-root-private-create-only" or not re.fullmatch(r"[a-z][a-z0-9.-]{1,79}", request["profile_id"]) or not re.fullmatch(r"[0-9]{8}t[0-9]{6}z-[a-f0-9]{16}", request["attempt_id"]) or request["root"]["path"] != "/root/private-recovery/" + request["profile_id"]:
        raise Refusal()
    if len(request["files"]) != 4 or len(wire["credentials"]) != 4 or len(wire["credential_sha256"]) != 4 or identity() != request["host"]:
        raise Refusal()
    for path, pin in (("/usr/bin/python3", request["ssh"]["interpreter_sha256"]), ("/usr/bin/sudo", request["ssh"]["sudo_sha256"])):
        with open(path, "rb") as file:
            if digest(file.read(32 * 1024 * 1024)) != pin:
                raise Refusal()
    credentials = []
    for name, file, encoded, sha in zip(NAMES, request["files"], wire["credentials"], wire["credential_sha256"]):
        decoded = base64.b64decode(encoded, validate=True)
        if file["basename"] != name or file["path"] != request["root"]["path"] + "/" + name or len(decoded) != file["stat"]["bytes"] or len(decoded) > 65536 or not re.fullmatch(r"[a-f0-9]{64}", sha) or digest(decoded) != sha or (file["stat"]["sha256"] and file["stat"]["sha256"] != sha):
            raise Refusal()
        credentials.append(decoded)
    check = lambda: deadline(wire)
    check()
    root = os.open("/root", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        rs = os.fstat(root)
        if not owner_ok(rs) or stat.S_IMODE(rs.st_mode) != 0o700:
            raise Refusal()
        parent = secure_dir(root, "private-recovery", create=True)
        try:
            changed, written, posts = publish(parent, request, credentials, wire["credential_sha256"], check)
        finally:
            os.close(parent)
    finally:
        os.close(root)
    check()
    if identity() != request["host"]:
        raise Refusal()
    result = {"schema_version": "compound.backup_credential_stage_observation.v0.1", "operation": wire["operation"], "changed": changed, "request_sha256": digest(canonical(request)), "program_sha256": sys.argv[1], "transaction": ".bcs-" + request["attempt_id"], "state": "staged_backup_credentials", "host_before": request["host"], "host_after": request["host"], "preimages": [None] * 4, "postimages": posts, "written": written, "journal_retained": True, "credentials_staged": True, "application_activated": False, "service_activated": False}
    sys.stdout.buffer.write(canonical(result) + b"\n")
    sys.stdout.buffer.flush()

if __name__ == "__main__":
    try:
        main()
    except BaseException:
        # Credential bytes and parser exceptions are never sent to either log.
        os._exit(1)
