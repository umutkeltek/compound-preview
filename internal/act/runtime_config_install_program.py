"""Closed inactive four-file transaction. Values enter only on bounded stdin.

The transport supplies the separately pinned verifier module as _VERIFIER. No
request can select executable code, a filesystem backend, SQL or a command.
Four renames are not atomic. The remote lock, retained inactivity fence and
fsynced recovery manifest permit separately approved reconciliation/rollback.
"""
import ctypes
import errno
import fcntl
import hashlib
import ipaddress
import json
import os
import re
import signal
import stat
import sys
import time
import urllib.parse

SLOTS = ("app", "api", "migrator", "web")
SCHEMA = "compound.runtime_config_installation_request.v0.2"
OBS_SCHEMA = "compound.runtime_config_installation_observation.v0.2"
LIMIT = 131072


class Refusal(Exception):
    pass


def require(value):
    if not value:
        raise Refusal()


def unique_object(pairs):
    result = {}
    for k, v in pairs:
        require(k not in result)
        result[k] = v
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda _: require(False))


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def exact(value, keys):
    require(type(value) is dict and set(value) == set(keys.split()))


def basename(name):
    require(type(name) is str and re.fullmatch(r"[A-Za-z0-9_.-]{1,100}", name)
            and name not in (".", ".."))
    return name


def fingerprint(s, raw=None):
    result = {"device": s.st_dev, "inode": s.st_ino, "bytes": s.st_size,
              "mtime_ns": str(s.st_mtime_ns), "ctime_ns": str(s.st_ctime_ns),
              "mode": format(stat.S_IMODE(s.st_mode), "04o"), "uid": s.st_uid,
              "gid": s.st_gid, "links": s.st_nlink}
    if raw is not None:
        result["sha256"] = digest(raw)
    return result


def image_same(a, b, renamed=False):
    # rename changes ctime; only a recorded publish intent authorizes ignoring
    # it when the exact staged inode is found at the destination after a crash.
    keys = set(a) | set(b)
    if renamed:
        keys.discard("ctime_ns")
    return all(a.get(k) == b.get(k) for k in keys)


def full_write(fd, raw):
    while raw:
        n = os.write(fd, raw)
        require(n > 0)
        raw = raw[n:]


def read_bound(fd, maximum=LIMIT):
    result = bytearray()
    while True:
        part = os.read(fd, min(8192, maximum + 1 - len(result)))
        if not part:
            return bytes(result)
        result.extend(part)
        require(len(result) <= maximum)


def rename_noreplace(source, source_fd, target, target_fd):
    # Do not emulate NOREPLACE with a check followed by overwriting rename.
    require(sys.platform.startswith("linux"))
    libc = ctypes.CDLL(None, use_errno=True)
    fn = getattr(libc, "renameat2", None)
    require(fn is not None)
    fn.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    fn.restype = ctypes.c_int
    if fn(source_fd, source.encode("ascii"), target_fd, target.encode("ascii"), 1) != 0:
        raise OSError(ctypes.get_errno(), "closed rename refused")


class Files:
    """Descriptor authority; tests alone may construct this with their UID/GID."""
    def __init__(self, root, uid=0, gid=0):
        self.root = root
        self.uid, self.gid = uid, gid
        self.fd, self.chain = self.walk(root["path"])
        self.check_root()

    def close(self):
        os.close(self.fd)

    def walk(self, path):
        require(path.startswith("/") and os.path.normpath(path) == path and path != "/")
        fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        chain = []
        try:
            for part in path.split("/")[1:]:
                basename(part)
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
                os.close(fd)
                fd = child
                s = os.fstat(fd)
                require(s.st_uid in (0, self.uid) and s.st_gid in (0, self.gid) and s.st_mode & 0o022 == 0)
                chain.append((s.st_dev, s.st_ino, s.st_uid, s.st_gid, stat.S_IMODE(s.st_mode)))
            return fd, chain
        except BaseException:
            os.close(fd)
            raise

    def check_root(self):
        s = os.fstat(self.fd)
        r = self.root
        require(s.st_dev == r["device"] and s.st_ino == r["inode"]
                and s.st_uid == self.uid and s.st_gid == self.gid
                and format(stat.S_IMODE(s.st_mode), "04o") == r["mode"])
        fd, chain = self.walk(r["path"])
        try:
            require(chain == self.chain)
        finally:
            os.close(fd)

    def read(self, parent, name, mode="0600", absent=False):
        basename(name)
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=parent)
        except FileNotFoundError:
            require(absent)
            return None, None
        try:
            s = os.fstat(fd)
            require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1
                    and s.st_uid == self.uid and s.st_gid == self.gid
                    and format(stat.S_IMODE(s.st_mode), "04o") == mode and s.st_size <= LIMIT)
            raw = read_bound(fd)
            after = os.fstat(fd)
            require(fingerprint(s) == fingerprint(after))
            # The name must still refer to the opened inode after the read.
            require(fingerprint(os.stat(name, dir_fd=parent, follow_symlinks=False)) == fingerprint(after))
            return fingerprint(after, raw), raw
        finally:
            os.close(fd)

    def create(self, parent, name, raw):
        require(len(raw) <= LIMIT)
        fd = os.open(basename(name), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
                     0o600, dir_fd=parent)
        try:
            os.fchmod(fd, 0o600)
            s = os.fstat(fd)
            require(s.st_uid == self.uid and s.st_gid == self.gid and s.st_nlink == 1)
            full_write(fd, raw)
            os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(parent)
        return self.read(parent, name)[0]

    def atomic_create(self, parent, name, raw):
        temporary = ".pending-" + os.urandom(8).hex()
        self.create(parent, temporary, raw)
        rename_noreplace(temporary, parent, name, parent)
        os.fsync(parent)
        return self.read(parent, name)[0]

    def directory(self, name, create=False):
        if create:
            os.mkdir(basename(name), 0o700, dir_fd=self.fd)
            os.fsync(self.fd)
        fd = os.open(basename(name), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
                     dir_fd=self.fd)
        s = os.fstat(fd)
        if not (s.st_dev == self.root["device"] and s.st_uid == self.uid and s.st_gid == self.gid
                and stat.S_IMODE(s.st_mode) == 0o700):
            os.close(fd)
            raise Refusal()
        os.fsync(fd)
        return fd


def dotenv(values):
    lines = []
    for key in sorted(values):
        value = values[key]
        require(re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", key))
        # Reject interpolation, quoting, comments, whitespace and escapes; these
        # bytes have different interpretations across dotenv consumers.
        require(type(value) is str and 0 < len(value) <= 4096)
        require(all(32 <= ord(c) < 127 and c not in "#$\\\"'`" for c in value))
        lines.append(key + "=" + ("'" + value + "'" if " " in value else value) + "\n")
    raw = "".join(lines).encode("ascii")
    require(len(raw) <= 16384)
    return raw


def validate(r, selected, operation="install"):
    exact(r, "schema_version profile_id attempt_id program_sha256 effect_set witness resource ssh host files roles database posture forced_inactive root marker lock profile capture qualification")
    require(r["schema_version"] == SCHEMA and r["effect_set"] == "inactive-aws-staging-runtime-configuration-v2")
    require(re.fullmatch(r"[0-9]{8}t[0-9]{6}z-[0-9a-f]{16}", r["attempt_id"]))
    require(operation in ("install", "reconcile", "rollback"))
    exact(r["database"], "name allow_connections endpoint_host endpoint_port scheme")
    require(re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", r["database"]["name"]))
    require(type(r["database"]["allow_connections"]) is bool and r["database"]["scheme"] in ("postgresql", "postgresql+psycopg"))
    require(type(r["roles"]) is list and len(r["roles"]) == 2)
    for role, kind in zip(r["roles"], ("application", "migrator")):
        exact(role, "kind key name source_host source_database")
        require(role["kind"] == kind and re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", role["name"]))
        require(re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", role["key"]))
        require(re.fullmatch(r"[A-Za-z0-9.-]{1,253}",role["source_host"]) and re.fullmatch(r"[a-z_][a-z0-9_]{0,62}",role["source_database"]))
    require(r["roles"][0]["name"] != r["roles"][1]["name"] and r["roles"][0]["key"] != r["roles"][1]["key"])
    require(r["forced_inactive"] is True and r["posture"] == {"production": True, "live_fetch": False, "checkout_enabled": False})
    exact(r["root"], "path device inode mode")
    require(r["root"]["mode"] in ("0700", "0750", "0755"))
    profile = r["profile"]
    exact(profile, "ref sha256 basenames app_key api_key migrator_key production_key live_fetch_key checkout_key api_fields api_endpoint web_api_key web_dealer_key web_cookie_key web_api_base web_cookie_name web_dealer_source containers")
    require(len(profile["basenames"]) == 4 and profile["basenames"][0] == ".env")
    names = [basename(n) for n in profile["basenames"]]
    require(len(set(names)) == 4)
    require(type(r["files"]) is list and len(r["files"]) == 4)
    for i, f in enumerate(r["files"]):
        exact(f, "slot basename path exists stat")
        require(f["slot"] == SLOTS[i] and f["basename"] == names[i]
                and f["path"] == r["root"]["path"] + "/" + names[i]
                and f["exists"] is (i == 0) and (f["stat"] is not None) == (i == 0))
    for name in (r["marker"]["basename"], r["lock"]["basename"]):
        require(basename(name) not in names and not name.startswith(".rci-"))
    exact(r["marker"], "basename stat")
    exact(r["lock"], "basename stat")
    require(type(profile["api_fields"]) is list and 0 < len(profile["api_fields"]) <= 64)
    require(len(set(profile["api_fields"])) == len(profile["api_fields"]))
    require(all(re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", k) for k in profile["api_fields"]))
    require(profile["web_dealer_source"] in profile["api_fields"])
    require(type(selected) is dict)
    if operation == "install":
        require(set(selected) == set(profile["api_fields"]))
        require(digest(encoded(selected)) == r["capture"]["projection_sha256"])
    else:
        require(selected == {})
    forbidden = {role["key"] for role in r["roles"]} | {profile["app_key"], profile["api_key"], profile["migrator_key"]}
    endpoint = profile["api_endpoint"]
    if endpoint is not None:
        exact(endpoint, "key url")
        require(type(endpoint["key"]) is str and type(endpoint["url"]) is str)
        require(re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", endpoint["key"]) and endpoint["key"].endswith("_OLLAMA_URL") and "DATABASE" not in endpoint["key"])
        semantic = {profile[k] for k in ("app_key", "api_key", "migrator_key", "production_key", "live_fetch_key", "checkout_key", "web_api_key", "web_dealer_key", "web_cookie_key", "web_dealer_source")}
        require(endpoint["key"] not in semantic | forbidden)
        match = re.fullmatch(r"http://([0-9.]+):11434", endpoint["url"])
        require(match is not None)
        try:
            address = ipaddress.IPv4Address(match.group(1))
        except ValueError:
            require(False)
        require(address in ipaddress.ip_network("100.64.0.0/10") and endpoint["url"] == f"http://{address}:11434")
    require(not forbidden.intersection(selected))
    require(all("DATABASE" not in k and not k.endswith("_DSN") for k in selected))
    require(len({profile[k] for k in ("api_key", "production_key", "live_fetch_key", "checkout_key")}) == 4)
    require(len({profile[k] for k in ("web_api_key", "web_dealer_key", "web_cookie_key")}) == 3)
    require(profile["api_key"] not in selected and profile["checkout_key"] not in selected)
    require(re.fullmatch(r"http://[a-z0-9][a-z0-9.-]{0,127}:[0-9]{2,5}", profile["web_api_base"]))
    require(re.fullmatch(r"__Host-[A-Za-z0-9_-]{1,63}", profile["web_cookie_name"]))
    require(0 < len(profile["containers"]) <= 20 and len(set(profile["containers"])) == len(profile["containers"]))
    require(all(re.fullmatch(r"[a-z0-9][a-z0-9_.-]{0,100}", n) for n in profile["containers"]))
    require(re.fullmatch(r"[A-Za-z0-9.-]{1,253}", r["database"]["endpoint_host"]))
    require(r["database"]["scheme"] in ("postgresql", "postgresql+psycopg") and 0 < r["database"]["endpoint_port"] <= 65535)
    dotenv(selected)


def build_payloads(r, selected, original, verifier):
    passwords = verifier.parse_passwords(original, r["roles"])
    p, d = r["profile"], r["database"]
    urls = [d["scheme"] + "://" + urllib.parse.quote(role["name"], safe="") + ":" +
            urllib.parse.quote(password, safe="") + "@" + d["endpoint_host"] + ":" + str(d["endpoint_port"]) + "/" +
            urllib.parse.quote(d["name"], safe="") for role, password in zip(r["roles"], passwords)]
    api = dict(selected)
    api.update({p["api_key"]: urls[0], p["production_key"]: "production", p["live_fetch_key"]: "false", p["checkout_key"]: "false"})
    if p["api_endpoint"] is not None:
        api[p["api_endpoint"]["key"]] = p["api_endpoint"]["url"]
    return [dotenv({p["app_key"]: urls[0]}), dotenv(api), dotenv({p["migrator_key"]: urls[1]}),
            dotenv({p["web_api_key"]: p["web_api_base"], p["web_dealer_key"]: selected[p["web_dealer_source"]],
                    p["web_cookie_key"]: p["web_cookie_name"]})]


class NativeRuntime:
    def __init__(self, verifier, deadline):
        self.verifier = verifier
        self.processes = verifier.Processes(deadline)
        self.native = verifier.NativeRuntime(self.processes)

    def check(self, r):
        self.native.programs(r)
        host = self.native.host(r)
        catalog = self.native.catalog(r, [])
        self.verifier.validate_catalog(catalog, r, [])
        host["system_identifier"], host["version_num"] = catalog["system_identifier"], catalog["version_num"]
        prefix = "\\set app_role '" + r["roles"][0]["name"] + "'\n\\set migrator_role '" + r["roles"][1]["name"] + "'\n\\set app_database '" + r["database"]["name"] + "'\n"
        sql = prefix + "SELECT pg_catalog.count(*) FROM pg_catalog.pg_stat_activity WHERE usename IN (:'app_role', :'migrator_role') OR datname=:'app_database';\n"
        child = self.processes.spawn(self.verifier.client_argv(r, "postgres", privileged=True), self.verifier.clean_env())
        code, out, err = self.processes.complete(child, sql.encode("ascii"))
        require(code == 0 and out == b"0\n" and not err)
        child = self.processes.spawn(["/usr/bin/docker", "container", "ls", "--format", "{{.Names}}"], self.verifier.clean_env())
        code, out, err = self.processes.complete(child, b"")
        require(code == 0 and not err)
        active = out.decode("ascii").splitlines()
        require(not set(active).intersection(r["profile"]["containers"]))
        return host

    def boundary(self, name):
        self.processes.remaining()

    def close(self):
        require(self.processes.cleanup())


class Transaction:
    def __init__(self, r, runtime, files):
        self.r, self.runtime, self.fs = r, runtime, files
        self.recovery_name = ".rci-" + r["attempt_id"]
        self.dir = None
        self.lock = None
        self.manifest = None
        self.operation = None
        self.changed = False
        self.alignment_acceptances = []
        self.alignment_lock = None

    def close(self):
        if self.alignment_lock is not None:
            os.close(self.alignment_lock)
            self.alignment_lock = None
        if self.dir is not None:
            os.close(self.dir)
        if self.lock is not None:
            os.close(self.lock)
        self.fs.close()

    def boundary(self, name):
        self.runtime.boundary(name)

    def guard(self):
        self.fs.check_root()
        self.lock_alignment()
        alignment_fence(self.fs, self.alignment_acceptances)
        for item in (self.r["marker"], self.r["lock"]):
            actual, _ = self.fs.read(self.fs.fd, item["basename"], item["stat"]["mode"])
            require(actual == item["stat"])
        return self.runtime.check(self.r)

    def lock_alignment(self):
        name = ".compound-credential-password-alignment"
        if self.alignment_lock is None:
            try:
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=self.fs.fd)
            except FileNotFoundError:
                return
            try:
                s = os.fstat(fd)
                require(s.st_uid == self.fs.uid and s.st_gid == self.fs.gid and stat.S_IMODE(s.st_mode) == 0o700)
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                self.alignment_lock = fd
            except BaseException:
                os.close(fd)
                raise
        current = os.stat(name, dir_fd=self.fs.fd, follow_symlinks=False)
        held = os.fstat(self.alignment_lock)
        require(stat.S_ISDIR(current.st_mode) and current.st_dev == held.st_dev and current.st_ino == held.st_ino
                and current.st_uid == self.fs.uid and current.st_gid == self.fs.gid and stat.S_IMODE(current.st_mode) == 0o700)

    def acquire(self):
        self.guard()
        self.lock = os.open(self.r["lock"]["basename"], os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=self.fs.fd)
        fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        require(fingerprint(os.fstat(self.lock)) == {k:v for k,v in self.r["lock"]["stat"].items() if k != "sha256"})
        self.guard()

    def images(self):
        return [self.fs.read(self.fs.fd, f["basename"], absent=True)[0] for f in self.r["files"]]

    def originals(self):
        return [f["stat"] for f in self.r["files"]]

    def journal(self, name, record):
        result = self.fs.atomic_create(self.dir, name + ".json", encoded(record))
        self.boundary(name)
        return result

    def journal_read(self, name, absent=False):
        _, raw = self.fs.read(self.dir, name + ".json", absent=absent)
        return decode(raw) if raw is not None else None

    def install(self, selected, verifier):
        self.acquire()
        host_before = self.guard()
        require(self.images() == self.originals())
        # Other transaction evidence blocks a new install, including completed
        # transactions. The owner must reconcile instead of silently replaying.
        require(not any(n.startswith(".rci-") for n in os.listdir(self.fs.fd)))
        _, original = self.fs.read(self.fs.fd, self.r["files"][0]["basename"])
        payloads = build_payloads(self.r, selected, original, verifier)
        self.dir = self.fs.directory(self.recovery_name, create=True)
        self.boundary("recovery-directory")
        backup = self.fs.create(self.dir, "original", original)
        self.boundary("original")
        staged = []
        for i, payload in enumerate(payloads):
            staged.append(self.fs.create(self.dir, "stage-" + SLOTS[i], payload))
            self.boundary("stage-" + SLOTS[i])
        self.manifest = {"schema_version": "compound.runtime_config_manifest.v0.2", "request_sha256": digest(encoded(self.r)),
                         "originals": self.originals(), "backup": backup, "staged": staged}
        self.journal("manifest", self.manifest)
        current = self.originals()
        postimages = []
        for i, f in enumerate(self.r["files"]):
            self.guard()
            require(self.images() == current)
            stage, _ = self.fs.read(self.dir, "stage-" + SLOTS[i])
            require(stage == staged[i])
            self.journal("publish-intent-" + SLOTS[i], {"staged": staged[i], "preimage": current[i]})
            self.guard()
            require(self.images() == current)
            ready, _ = self.fs.read(self.dir, "stage-" + SLOTS[i])
            require(ready == staged[i])
            if i == 0:
                os.rename("stage-app", f["basename"], src_dir_fd=self.dir, dst_dir_fd=self.fs.fd)
            else:
                rename_noreplace("stage-" + SLOTS[i], self.dir, f["basename"], self.fs.fd)
            self.changed = True
            self.boundary("rename-" + SLOTS[i])
            os.fsync(self.dir)
            self.boundary("source-fsync-" + SLOTS[i])
            os.fsync(self.fs.fd)
            self.boundary("target-fsync-" + SLOTS[i])
            image, _ = self.fs.read(self.fs.fd, f["basename"])
            require(image_same(image, staged[i], renamed=True))
            postimages.append(image)
            current[i] = image
            self.journal("postimage-" + SLOTS[i], image)
        host_after = self.guard()
        require(self.images() == postimages)
        self.journal("completed", {"postimages": postimages})
        return self.observation("installed_inactive", host_before, host_after, postimages)

    def observation(self, state, before, after, images):
        return {"schema_version": OBS_SCHEMA, "request_sha256": digest(encoded(self.r)),
                "program_sha256": self.r["program_sha256"], "transaction": self.recovery_name,
                "state": state, "operation": self.operation, "changed": self.changed, "host_before": before, "host_after": after,
                "preimages": self.originals(), "postimages": images,
                "written": [state == "installed_inactive"] * 4, "fences_retained": True,
                "configuration_accepted": state == "installed_inactive",
                "application_activated": False, "service_activated": False}

    def classify(self):
        current = self.images()
        states = []
        restored = self.journal_read("restore-intent", absent=True)
        for i, image in enumerate(current):
            if image == self.originals()[i]:
                states.append("original")
                continue
            if i == 0 and restored and image is not None and image_same(image, restored["image"], renamed=True):
                states.append("restored")
                continue
            if image is not None and self.manifest:
                post = self.journal_read("postimage-" + SLOTS[i], absent=True)
                intent = self.journal_read("publish-intent-" + SLOTS[i], absent=True)
                if post == image or (post is None and intent and intent["staged"] == self.manifest["staged"][i]
                                     and image_same(image, self.manifest["staged"][i], renamed=True)):
                    states.append("published")
                    continue
            raise Refusal()
        return current, states

    def recover(self, operation):
        require(operation in ("reconcile", "rollback"))
        self.acquire()
        before = self.guard()
        try:
            self.dir = self.fs.directory(self.recovery_name)
        except FileNotFoundError:
            # A lost SSH response may precede the first remote mkdir. Fresh
            # all-slot equality plus retained fences proves a zero-effect rollback.
            require(not any(n.startswith(".rci-") for n in os.listdir(self.fs.fd)))
            current = self.images()
            require(current == self.originals())
            os.fsync(self.fs.fd)
            return self.observation("rolled_back" if operation == "rollback" else "recovery_required", before, self.guard(), current)
        self.manifest = self.journal_read("manifest", absent=True)
        if self.manifest:
            require(self.manifest["request_sha256"] == digest(encoded(self.r)) and self.manifest["originals"] == self.originals())
            backup, original = self.fs.read(self.dir, "original")
            require(backup == self.manifest["backup"] and digest(original) == self.originals()[0]["sha256"])
        current, states = self.classify()  # Every slot before ANY recovery write.
        if operation == "reconcile":
            state = "installed_inactive" if states == ["published"] * 4 else "recovery_required"
            os.fsync(self.dir)
            os.fsync(self.fs.fd)
            return self.observation(state, before, self.guard(), current)
        if not self.manifest:
            require(states == ["original"] * 4)
            return self.observation("rolled_back", before, self.guard(), current)
        # An existing restore-intent remains authoritative across interruption.
        restore = self.journal_read("restore-intent", absent=True)
        if states[0] == "published" and restore is None:
            image, restored_raw = self.fs.read(self.dir, "restore-app", absent=True)
            if image is None:
                image = self.fs.atomic_create(self.dir, "restore-app", original)
            else:
                require(restored_raw == original)
            self.boundary("restore-stage")
            restore = {"image": image}
            self.journal("restore-intent", restore)
        for i, f in enumerate(self.r["files"]):
            self.guard()
            observed, classifications = self.classify()
            require(observed == current)
            if classifications[i] != "published":
                continue
            intent_name = "rollback-intent-" + SLOTS[i]
            if self.journal_read(intent_name, absent=True) is None:
                self.journal(intent_name, {"image": current[i]})
            self.guard()
            require(self.images() == current)
            if i == 0:
                image, _ = self.fs.read(self.dir, "restore-app")
                require(image == restore["image"])
                os.rename("restore-app", f["basename"], src_dir_fd=self.dir, dst_dir_fd=self.fs.fd)
            else:
                os.unlink(f["basename"], dir_fd=self.fs.fd)
            self.changed = True
            self.boundary("rollback-effect-" + SLOTS[i])
            os.fsync(self.dir)
            os.fsync(self.fs.fd)
            self.boundary("rollback-fsync-" + SLOTS[i])
            current = self.images()
            self.classify()
        final, states = self.classify()
        require(states[0] in ("original", "restored") and states[1:] == ["original"] * 3)
        after = self.guard()
        if self.journal_read("rolled-back", absent=True) is None:
            self.journal("rolled-back", {"postimages": final})
        return self.observation("rolled_back", before, after, final)


def alignment_fence(files, acceptances):
    """Read safe RESULT records only. Private SCRAM journals stay unopened."""
    require(type(acceptances) is list and len(acceptances) <= 64)
    selected = {}
    for value in acceptances:
        exact(value, "action_intent_id original_action_intent_id request_sha256 observation_sha256 operation effect")
        for key in ("action_intent_id", "original_action_intent_id"):
            require(type(value[key]) is str and re.fullmatch(r"act_[0-9a-f]{12}", value[key]))
        for key in ("request_sha256", "observation_sha256"):
            require(type(value[key]) is str and re.fullmatch(r"[0-9a-f]{64}", value[key]))
        original = value["original_action_intent_id"]
        require(original not in selected)
        selected[original] = value

    def directory(parent, name):
        fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        try:
            s = os.fstat(fd)
            require(s.st_uid == files.uid and s.st_gid == files.gid and stat.S_IMODE(s.st_mode) == 0o700)
            return fd
        except BaseException:
            os.close(fd)
            raise

    def records(fd):
        entries = os.listdir(fd)
        require(len(entries) <= 68)
        recoveries = []
        for name in entries:
            if re.fullmatch(r"recovery_act_[0-9a-f]{12}", name):
                recoveries.append(name[len("recovery_"):])
                continue
            require(name in ("PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"))
            # This is metadata only, including for private verifier records.
            s = os.stat(name, dir_fd=fd, follow_symlinks=False)
            require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and s.st_uid == files.uid and s.st_gid == files.gid
                    and stat.S_IMODE(s.st_mode) == 0o600 and s.st_size <= 1_048_576)
        return recoveries

    try:
        namespace = directory(files.fd, ".compound-credential-password-alignment")
    except FileNotFoundError:
        require(not selected)
        return
    try:
        originals = os.listdir(namespace)
        require(len(originals) <= 64 and set(originals) == set(selected))
        for original in originals:
            require(re.fullmatch(r"act_[0-9a-f]{12}", original))
            fd = directory(namespace, original)
            try:
                recoveries = records(fd)
                require("PREIMAGE.json" in os.listdir(fd))
                for action in recoveries:
                    child = directory(fd, "recovery_" + action)
                    try:
                        require(not records(child))
                        require("PREIMAGE.json" in os.listdir(child))
                    finally:
                        os.close(child)
                accepted = selected[original]
                selected_action = accepted["action_intent_id"]
                result_fd = fd
                if selected_action != original:
                    require(selected_action in recoveries)
                    result_fd = directory(fd, "recovery_" + selected_action)
                try:
                    _, raw = files.read(result_fd, "RESULT.json")
                    result = decode(raw)
                    exact(result, "schema_version operation action_intent_id original_action_intent_id request_sha256 classification effect closed protected_file_unchanged attributes_unchanged configuration_accepted credential_qualification_accepted catalog resolved_recovery_actions")
                    require(result["schema_version"] == "compound.credential_password_alignment_observation.v0.1")
                    for key in ("action_intent_id", "original_action_intent_id", "request_sha256", "operation", "effect"):
                        require(result[key] == accepted[key])
                    require(digest(encoded(result)) == accepted["observation_sha256"])
                    require(result["closed"] is True and result["protected_file_unchanged"] is True and result["attributes_unchanged"] is True
                            and result["configuration_accepted"] is False and result["credential_qualification_accepted"] is False and result["catalog"] is None)
                    resolved = result["resolved_recovery_actions"]
                    require(type(resolved) is list and len(resolved) == len(set(resolved)))
                    require(set(resolved) == set(recoveries) - {selected_action})
                    if selected_action == original:
                        require({"PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"}.issubset(os.listdir(fd)))
                        require(not recoveries and result["operation"] == "align" and result["classification"] == "new_new" and result["effect"] == "committed")
                    elif result["operation"] == "restore":
                        require({"PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"}.issubset(os.listdir(result_fd)))
                        require(result["classification"] == "new_new" and result["effect"] == "restored")
                    else:
                        require(result["operation"] == "reconcile" and result["classification"] in ("old_old", "new_new")
                                and result["effect"] == "reconciled_" + result["classification"])
                finally:
                    if result_fd != fd:
                        os.close(result_fd)
            finally:
                os.close(fd)
    finally:
        os.close(namespace)
    files.check_root()


def dispatch(outer, runtime, files, verifier):
    exact(outer, "request selected operation issued_ns deadline_ns alignment_acceptances")
    r = outer["request"]
    validate(r, outer["selected"], outer["operation"])
    require(outer["operation"] in ("install", "reconcile", "rollback"))
    transaction = Transaction(r, runtime, files)
    transaction.alignment_acceptances = outer["alignment_acceptances"]
    transaction.operation = outer["operation"]
    try:
        if outer["operation"] == "install":
            return transaction.install(outer["selected"], verifier)
        return transaction.recover(outer["operation"])
    finally:
        transaction.close()


def main():
    runtime = None
    accepted = False
    observation = None
    def interrupted(_sig, _frame):
        raise Refusal()
    try:
        for sig in (signal.SIGALRM, signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, interrupted)
        signal.setitimer(signal.ITIMER_REAL, 46)
        raw = sys.stdin.buffer.read(LIMIT + 1)
        require(0 < len(raw) <= LIMIT)
        outer = decode(raw)
        require(len(sys.argv) == 2 and outer["request"]["program_sha256"] == sys.argv[1])
        issued, deadline = int(outer["issued_ns"]), int(outer["deadline_ns"])
        now = time.time_ns()
        require(0 < deadline - issued <= 50_000_000_000 and -2_000_000_000 <= now - issued <= 15_000_000_000)
        budget = min(43, (deadline - now) / 1_000_000_000 - 4)
        require(budget > 0 and os.geteuid() == 0 and os.getegid() == 0)
        signal.setitimer(signal.ITIMER_REAL, budget)
        runtime = NativeRuntime(_VERIFIER, time.monotonic() + budget)
        observation = dispatch(outer, runtime, Files(outer["request"]["root"]), _VERIFIER)
        runtime.close()
        runtime = None
        accepted = True
    except BaseException:
        pass
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        if runtime is not None:
            try:
                runtime.close()
            except BaseException:
                accepted = False
    if accepted:
        try:
            sys.stdout.buffer.write(encoded(observation) + b"\n")
            sys.stdout.flush()
            return 0
        except BaseException:
            pass
    sys.stderr.write("runtime_config_install:refused\n")
    return 1


if __name__ == "__main__":
    sys.exit(main())
