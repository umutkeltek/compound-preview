"""Closed, ephemeral credential owner. This module never writes a remote file.

The Go owner binds this exact source digest, admission and transport. Only a
metadata observation crosses stdout. No underlying exception is rendered.
"""
import hashlib
import json
import os
import re
import secrets
import selectors
import signal
import stat
import subprocess
import sys
import time
import urllib.parse


class Refusal(Exception):
    pass


FAILURE_STAGES = frozenset({
    "request_validation", "executable", "host_identity", "protected_file",
    "source_format", "pre_catalog", "app_existing", "app_wrong",
    "migrator_existing", "migrator_wrong", "post_catalog", "postchecks",
    "deadline", "cleanup",
})


class StageRefusal(Refusal):
    def __init__(self, stage):
        self.stage = stage if stage in FAILURE_STAGES else None


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


def canonical_digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":"),
                                     ensure_ascii=True).encode("ascii")).hexdigest()


def exact_keys(value, keys):
    require(type(value) is dict and set(value) == set(keys.split()))


def valid_client_version(version):
    if type(version) is not str or len(version) > 128:
        return False
    match = re.fullmatch(r"psql \(PostgreSQL\) ([0-9]{1,2}\.[0-9]{1,2})(?: \(Debian ([0-9]{1,2}\.[0-9]{1,2})-([0-9][0-9A-Za-z.+~]{0,63})\))?", version)
    return match is not None and (match[2] is None or match[1] == match[2])


def validate_request(r):
    exact_keys(r, "schema_version profile_id attempt_id program_sha256 effect_set witness resource ssh host file roles database")
    for key, keys in {
        "witness": "ref sha256 observed_at",
        "resource": "account region instance address",
        "ssh": "alias user port identity_file known_hosts_file known_hosts_sha256 host_key_sha256 interpreter_sha256 client_path client_sha256 client_version sudo_sha256",
        "host": "hostname machine_id boot_id system_identifier version_num",
        "file": "path device inode bytes mtime_ns ctime_ns sha256",
        "database": "name allow_connections",
    }.items():
        exact_keys(r[key], keys)
    require(r["schema_version"] == "compound.credential_verification_request.v0.1")
    require(r["effect_set"] == "existing-local-postgres-maintenance-auth-v1")
    require(type(r["roles"]) is list and len(r["roles"]) == 2)
    for index, role in enumerate(r["roles"]):
        exact_keys(role, "kind key name source_host source_database")
        require(role["kind"] == ("application", "migrator")[index])
        require(re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", role["name"]) is not None)
        require(re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", role["key"]) is not None)
        require(re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", role["source_database"]) is not None)
        require(re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,127}", role["source_host"]) is not None)
    require(r["roles"][0]["key"] != r["roles"][1]["key"])
    require(r["roles"][0]["name"] != r["roles"][1]["name"])
    require(re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", r["database"]["name"]) is not None)
    require(type(r["database"]["allow_connections"]) is bool)
    require(re.fullmatch(r"/usr/(?:lib/postgresql/[0-9]{1,2}/bin|bin)/psql", r["ssh"]["client_path"]) is not None)
    require(valid_client_version(r["ssh"]["client_version"]))
    for field in ("mtime_ns", "ctime_ns"):
        require(type(r["file"][field]) is str and re.fullmatch(r"[1-9][0-9]{0,18}", r["file"][field]) is not None)
        require(int(r["file"][field]) <= 9223372036854775807)
    for field in ("device", "inode", "bytes"):
        require(type(r["file"][field]) is int and r["file"][field] > 0)
    require(r["file"]["bytes"] <= 16384)
    for value in (r["file"]["sha256"], r["program_sha256"],
                  r["ssh"]["interpreter_sha256"], r["ssh"]["client_sha256"], r["ssh"]["sudo_sha256"]):
        require(type(value) is str and re.fullmatch(r"[a-f0-9]{64}", value) is not None)
    path = r["file"]["path"]
    require(type(path) is str and path.isascii() and path.startswith("/") and os.path.normpath(path) == path)
    require(os.path.basename(path) == ".env" and "\x00" not in path)


def trusted_open(path, secret=False):
    require(path.startswith("/") and os.path.normpath(path) == path)
    parts = path.split("/")[1:]
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in parts[:-1]:
            require(part not in ("", ".", ".."))
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
                            dir_fd=directory)
            os.close(directory)
            directory = child
            info = os.fstat(directory)
            require(info.st_uid == 0 and info.st_gid == 0 and info.st_mode & 0o022 == 0)
        fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=directory)
        try:
            info = os.fstat(fd)
            require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_gid == 0)
            require(info.st_mode & 0o022 == 0)
            if secret:
                require(stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1)
            return fd
        except BaseException:
            os.close(fd)
            raise
    finally:
        os.close(directory)


def bounded_read(fd, limit):
    os.lseek(fd, 0, os.SEEK_SET)
    result = bytearray()
    while True:
        chunk = os.read(fd, min(8192, limit + 1 - len(result)))
        if not chunk:
            return bytes(result)
        result.extend(chunk)
        require(len(result) <= limit)


def stat_matches(info, expected):
    return (info.st_dev == expected["device"] and info.st_ino == expected["inode"]
            and info.st_size == expected["bytes"] and str(info.st_mtime_ns) == expected["mtime_ns"]
            and str(info.st_ctime_ns) == expected["ctime_ns"] and info.st_uid == 0 and info.st_gid == 0
            and stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1)


def read_custodied_file(r):
    fd = trusted_open(r["file"]["path"], secret=True)
    try:
        require(stat_matches(os.fstat(fd), r["file"]))
        raw = bounded_read(fd, 16384)
        require(stat_matches(os.fstat(fd), r["file"]))
        require(hashlib.sha256(raw).hexdigest() == r["file"]["sha256"])
        return raw
    finally:
        os.close(fd)


def program_hash(path):
    fd = trusted_open(path)
    try:
        digest = hashlib.sha256()
        before = os.fstat(fd)
        while True:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(fd)
        require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns))
        return digest.hexdigest()
    finally:
        os.close(fd)


def fixed_read(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        return bounded_read(fd, limit).decode("ascii").strip()
    finally:
        os.close(fd)


def host_identity(r):
    value = {"hostname": os.uname().nodename,
             "machine_id": fixed_read("/etc/machine-id", 128),
             "boot_id": fixed_read("/proc/sys/kernel/random/boot_id", 128),
             "system_identifier": r["host"]["system_identifier"],
             "version_num": r["host"]["version_num"]}
    require(value == r["host"])
    return value


def parse_passwords(raw, roles):
    text = raw.decode("utf-8", "strict")
    require("\x00" not in text and "\r" not in text)
    values = {}
    for line in text.splitlines():
        if not line:
            continue
        require("=" in line)
        key, value = line.split("=", 1)
        require(key in [role["key"] for role in roles] and key not in values)
        if value.startswith(("\"", "'")):
            quote = value[0]
            require(len(value) >= 2 and value[-1] == quote and quote not in value[1:-1])
            value = value[1:-1]
        require(value and value == value.strip() and "\\" not in value)
        values[key] = value
    require(set(values) == {role["key"] for role in roles})
    result = []
    for role in roles:
        url = values[role["key"]]
        require(all(33 <= ord(char) < 127 for char in url))
        require(re.search(r"%(?![0-9a-fA-F]{2})", url) is None)
        parsed = urllib.parse.urlsplit(url)
        require(parsed.scheme in ("postgres", "postgresql") and not parsed.fragment)
        require(parsed.hostname == role["source_host"] and parsed.port == 5432)
        require(urllib.parse.unquote(parsed.username or "", errors="strict") == role["name"])
        require(urllib.parse.unquote(parsed.path, errors="strict") == "/" + role["source_database"])
        allowed = {"sslmode", "sslcert", "sslkey", "sslrootcert", "sslcrl", "sslsni",
                   "ssl_min_protocol_version", "ssl_max_protocol_version"}
        seen = set()
        for key, value in urllib.parse.parse_qsl(parsed.query, keep_blank_values=True, strict_parsing=True):
            require(key in allowed and key not in seen and bool(value))
            seen.add(key)
        password = urllib.parse.unquote(parsed.password or "", errors="strict")
        require(password and all(c not in password for c in ("\x00", "\r", "\n")))
        result.append(password)
    require(result[0] != result[1])
    return result


def negative_passwords(passwords, generate=secrets.token_urlsafe):
    wrong = []
    for _ in range(2):
        for attempt in range(8):
            candidate = generate(48)
            if candidate not in passwords and candidate not in wrong:
                wrong.append(candidate)
                break
        else:
            raise Refusal()
    return wrong


class Processes:
    def __init__(self, deadline):
        self.deadline = deadline
        self.children = []

    def remaining(self):
        value = self.deadline - time.monotonic()
        if value <= 0:
            raise StageRefusal("deadline")
        return value

    def spawn(self, argv, env):
        self.remaining()
        child = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 env=env, cwd="/", start_new_session=True, close_fds=True)
        self.children.append(child)
        return child

    def row(self, child):
        selector = selectors.DefaultSelector()
        outputs = {"out": bytearray(), "err": bytearray()}
        try:
            selector.register(child.stdout, selectors.EVENT_READ, "out")
            selector.register(child.stderr, selectors.EVENT_READ, "err")
            while True:
                for key, _ in selector.select(min(self.remaining(), 0.1)):
                    raw = os.read(key.fd, 4096)
                    require(raw)
                    outputs[key.data].extend(raw)
                    require(len(outputs[key.data]) <= 16384 and not outputs["err"])
                    if b"\n" in outputs["out"]:
                        line, rest = bytes(outputs["out"]).split(b"\n", 1)
                        require(not rest and child.poll() is None)
                        return decode_json(line)
                require(child.poll() is None)
        finally:
            selector.close()

    def complete(self, child, input_bytes):
        # Fixed small SQL only. Credential values are exclusively child env.
        child.stdin.write(input_bytes)
        child.stdin.close()
        child.stdin = None
        selector = selectors.DefaultSelector()
        outputs = {"out": bytearray(), "err": bytearray()}
        try:
            selector.register(child.stdout, selectors.EVENT_READ, "out")
            selector.register(child.stderr, selectors.EVENT_READ, "err")
            while selector.get_map():
                for key, _ in selector.select(min(self.remaining(), 0.1)):
                    raw = os.read(key.fd, 4096)
                    if not raw:
                        selector.unregister(key.fileobj)
                    else:
                        outputs[key.data].extend(raw)
                        require(len(outputs[key.data]) <= 16384)
            code = child.wait(timeout=self.remaining())
            return code, bytes(outputs["out"]), bytes(outputs["err"])
        finally:
            selector.close()

    def cleanup(self):
        cleanup_deadline = time.monotonic() + 2.0
        success = True
        for child in self.children:
            if child.poll() is None:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        for child in self.children:
            try:
                child.wait(timeout=max(0.001, cleanup_deadline - time.monotonic()))
            except BaseException:
                success = False
            for stream in (child.stdin, child.stdout, child.stderr):
                if stream is not None:
                    try:
                        stream.close()
                    except BaseException:
                        success = False
        return success


def clean_env(password=None):
    env = {"PATH": "/usr/bin:/bin", "HOME": "/nonexistent", "LC_ALL": "C", "LANG": "C",
           "PGCONNECT_TIMEOUT": "4", "PGAPPNAME": "compound-credential-verifier"}
    if password is not None:
        env["PGPASSWORD"] = password
    return env


def client_argv(r, user, privileged=False):
    argv = [r["ssh"]["client_path"], "-X", "-w", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1",
            "--host=" + ("/var/run/postgresql" if privileged else "127.0.0.1"),
            "--port=5432", "--username=" + user, "--dbname=postgres"]
    if privileged:
        argv = ["/usr/bin/sudo", "-n", "-u", "postgres", "--"] + argv
    return argv


IDENTITY_SQL = ("SELECT pg_catalog.json_build_object('user',CURRENT_USER,'session_user',SESSION_USER,"
                "'database',pg_catalog.current_database(),'address',pg_catalog.host(pg_catalog.inet_server_addr()),"
                "'port',pg_catalog.inet_server_port(),'version_num',pg_catalog.current_setting('server_version_num'),"
                "'pid',pg_catalog.pg_backend_pid())::pg_catalog.text;\n")


def catalog_sql(r, pids):
    # psql interpolation occurs on stdin. Names were closed-grammar validated;
    # :'name' supplies SQL literal quoting, never shell or -c substitution.
    prefix = ("\\set app_role '" + r["roles"][0]["name"] + "'\n"
              "\\set migrator_role '" + r["roles"][1]["name"] + "'\n"
              "\\set app_database '" + r["database"]["name"] + "'\n")
    sessions = "'[]'::pg_catalog.json"
    if pids:
        require(len(pids) == 2 and all(type(pid) is int and 0 < pid <= 2_147_483_647 for pid in pids))
        require(pids[0] != pids[1])
        prefix += "\\set pid_one '" + str(pids[0]) + "'\n\\set pid_two '" + str(pids[1]) + "'\n"
        sessions = ("(SELECT pg_catalog.json_agg(pg_catalog.json_build_object('pid',s.pid,'user',s.usename,"
                    "'database',s.datname,'address',pg_catalog.host(s.client_addr)) "
                    "ORDER BY CASE WHEN s.pid=:'pid_one'::pg_catalog.int4 THEN 0 ELSE 1 END) "
                    "FROM pg_catalog.pg_stat_activity s WHERE s.pid IN (:'pid_one'::pg_catalog.int4,:'pid_two'::pg_catalog.int4))")
    return prefix + """SELECT pg_catalog.json_build_object(
'system_identifier',(SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()),
'version_num',pg_catalog.current_setting('server_version_num'),
'port',pg_catalog.current_setting('port')::pg_catalog.int4,
'privileged',CURRENT_USER='postgres' AND pg_catalog.inet_server_addr() IS NULL
  AND (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=CURRENT_USER),
'roles',(SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
  'name',a.rolname,'login',a.rolcanlogin,'scram_present',a.rolpassword IS NOT NULL AND a.rolpassword LIKE 'SCRAM-SHA-256$%',
  'superuser',a.rolsuper,'createdb',a.rolcreatedb,'createrole',a.rolcreaterole,
  'replication',a.rolreplication,'bypassrls',a.rolbypassrls,'inherit',a.rolinherit,
  'connection_limit',a.rolconnlimit,'role_config',r.rolconfig,'valid_until',a.rolvaliduntil,
  'membership_rows',(SELECT pg_catalog.count(*) FROM pg_catalog.pg_auth_members m WHERE m.roleid=a.oid OR m.member=a.oid))
  ORDER BY CASE WHEN a.rolname=:'app_role' THEN 0 ELSE 1 END)
  FROM pg_catalog.pg_authid a JOIN pg_catalog.pg_roles r ON r.oid=a.oid
  WHERE a.rolname IN (:'app_role',:'migrator_role')),
'database',(SELECT pg_catalog.json_build_object('name',d.datname,'owner',pg_catalog.pg_get_userbyid(d.datdba),
  'connection_limit',d.datconnlimit,'allow_connections',d.datallowconn,
  'application_connect',pg_catalog.has_database_privilege(:'app_role',d.oid,'CONNECT'),
  'public_connect',EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl,pg_catalog.acldefault('d'::pg_catalog.\"char\",d.datdba))) x
    WHERE x.grantee=0 AND x.privilege_type='CONNECT')) FROM pg_catalog.pg_database d WHERE d.datname=:'app_database'),
'sessions',""" + sessions + ")::pg_catalog.text;\n"


def validate_catalog(value, r, sessions):
    exact_keys(value, "system_identifier version_num port privileged roles database sessions")
    require(value["system_identifier"] == r["host"]["system_identifier"])
    require(value["version_num"] == r["host"]["version_num"] and value["port"] == 5432 and value["privileged"] is True)
    require(type(value["roles"]) is list and len(value["roles"]) == 2)
    for index, actual in enumerate(value["roles"]):
        expected = {"name": r["roles"][index]["name"], "login": True, "scram_present": True,
                    "superuser": False, "createdb": False, "createrole": False, "replication": False,
                    "bypassrls": False, "inherit": True, "connection_limit": -1,
                    "role_config": None, "valid_until": None, "membership_rows": 0}
        require(actual == expected)
    require(value["database"] == {"name": r["database"]["name"], "owner": r["roles"][1]["name"],
                                 "connection_limit": 0, "allow_connections": r["database"]["allow_connections"],
                                 "application_connect": False, "public_connect": False})
    require(value["sessions"] == sessions)


class NativeRuntime:
    def __init__(self, processes):
        self.processes = processes

    def host(self, r):
        return host_identity(r)

    def file(self, r):
        return read_custodied_file(r)

    def recheck(self, r):
        require(os.geteuid() == 0 and os.getegid() == 0)
        executable = os.readlink("/proc/self/exe")
        require(program_hash(executable) == r["ssh"]["interpreter_sha256"])
        require(program_hash(r["ssh"]["client_path"]) == r["ssh"]["client_sha256"])
        require(program_hash("/usr/bin/sudo") == r["ssh"]["sudo_sha256"])

    def programs(self, r):
        self.recheck(r)
        child = self.processes.spawn([r["ssh"]["client_path"], "--version"], clean_env())
        code, out, err = self.processes.complete(child, b"")
        require(code == 0 and not err and out == (r["ssh"]["client_version"] + "\n").encode("ascii"))

    def catalog(self, r, pids):
        child = self.processes.spawn(client_argv(r, "postgres", privileged=True), clean_env())
        code, out, err = self.processes.complete(child, catalog_sql(r, pids).encode("ascii"))
        require(code == 0 and not err and len(out.splitlines()) == 1)
        return decode_json(out)

    def positive(self, r, role, password):
        child = self.processes.spawn(client_argv(r, role["name"]), clean_env(password))
        child.stdin.write(IDENTITY_SQL.encode("ascii"))
        child.stdin.flush()
        return self.processes.row(child)

    def negative(self, r, role, password):
        child = self.processes.spawn(client_argv(r, role["name"]), clean_env(password))
        code, out, err = self.processes.complete(child, b"SELECT 1;\n")
        expected = ('psql: error: connection to server at "127.0.0.1", port 5432 failed: '
                    'FATAL:  password authentication failed for user "' + role["name"] + '"\n').encode("ascii")
        # libpq may retry without TLS after a rejected TLS authentication.
        # Its second exact rejection omits the psql prefix; no other output is valid.
        fallback_pair = expected + expected[len(b"psql: error: "):]
        require(code == 2 and not out and err in (expected, fallback_pair))


def run_protocol(r, runtime, generate=secrets.token_urlsafe):
    stage = "request_validation"
    try:
        validate_request(r)
        stage = "executable"
        runtime.programs(r)
        stage = "host_identity"
        host_before = runtime.host(r)
        stage = "protected_file"
        raw_before = runtime.file(r)
        stage = "source_format"
        passwords = parse_passwords(raw_before, r["roles"])
        wrong = negative_passwords(passwords, generate)
        stage = "pre_catalog"
        before = runtime.catalog(r, [])
        validate_catalog(before, r, [])
        authentication = []
        sessions = []
        pids = []
        for index, role in enumerate(r["roles"]):
            stage = "app_existing" if index == 0 else "migrator_existing"
            identity = runtime.positive(r, role, passwords[index])
            exact_keys(identity, "user session_user database address port version_num pid")
            pid = identity["pid"]
            require(type(pid) is int and 0 < pid <= 2_147_483_647 and pid not in pids)
            require(identity == {"user": role["name"], "session_user": role["name"], "database": "postgres",
                                 "address": "127.0.0.1", "port": 5432, "version_num": r["host"]["version_num"], "pid": pid})
            pids.append(pid)
            sessions.append({"pid": pid, "user": role["name"], "database": "postgres", "address": "127.0.0.1"})
            authentication.append({"kind": role["kind"] + "_existing", "role": role["name"],
                                   "outcome": "authenticated", "identity": identity})
            stage = "app_wrong" if index == 0 else "migrator_wrong"
            runtime.negative(r, role, wrong[index])
            authentication.append({"kind": role["kind"] + "_wrong", "role": role["name"],
                                   "outcome": "password_rejected", "identity": None})
        stage = "post_catalog"
        after = runtime.catalog(r, pids)
        validate_catalog(after, r, sessions)
        stage = "postchecks"
        require(runtime.file(r) == raw_before)
        runtime.recheck(r)
        host_after = runtime.host(r)
        require(host_before == r["host"] and host_after == r["host"])
        return {"schema_version": "compound.credential_verification_observation.v0.1",
                "request_sha256": canonical_digest(r), "program_sha256": r["program_sha256"],
                "host_before": host_before, "host_after": host_after, "file_matched": True,
                "before": before, "after": after, "authentication": authentication,
                "configuration_accepted": False, "application_access_accepted": False,
                "source_capture_accepted": False, "migration_accepted": False}
    except StageRefusal:
        raise
    except (TimeoutError, subprocess.TimeoutExpired):
        raise StageRefusal("deadline") from None
    except BaseException:
        raise StageRefusal(stage) from None


def remote_budget(issued_ns, deadline_ns, now_ns, monotonic_now):
    require(type(issued_ns) is str and re.fullmatch(r"[0-9]{19}", issued_ns) is not None)
    require(type(deadline_ns) is str and re.fullmatch(r"[0-9]{19}", deadline_ns) is not None)
    issued, deadline = int(issued_ns), int(deadline_ns)
    require(0 < deadline - issued <= 50_000_000_000)
    # Refuse clocks outside the declared two-second tolerance. Even if SSH is
    # lost, the helper owns a shorter deadline and two-second cleanup reserve.
    require(-2_000_000_000 <= now_ns - issued <= 15_000_000_000)
    budget = min(43.0, (deadline - now_ns) / 1_000_000_000 - 4.0)
    require(budget > 0)
    return monotonic_now + budget


def main():
    processes = None
    observation = None
    accepted = False
    failure_stage = "request_validation"

    def interrupted(_signum, _frame):
        raise StageRefusal("deadline")

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
        failure_stage = "deadline"
        deadline = remote_budget(wire["issued_ns"], wire["deadline_ns"], time.time_ns(), time.monotonic())
        processes = Processes(deadline)
        signal.setitimer(signal.ITIMER_REAL, max(0.001, deadline - time.monotonic()))
        observation = run_protocol(r, NativeRuntime(processes))
        require(time.monotonic() < deadline)
        accepted = True
    except BaseException as error:
        if isinstance(error, StageRefusal):
            failure_stage = error.stage
        accepted = False
    finally:
        cleaned = True
        try:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for signum in (signal.SIGTERM, signal.SIGHUP):
                signal.signal(signum, signal.SIG_IGN)
        except BaseException:
            cleaned = False
        if processes is not None:
            try:
                cleaned = processes.cleanup() and cleaned
            except BaseException:
                cleaned = False
        if not cleaned:
            failure_stage = "cleanup"
            accepted = False
    if accepted:
        try:
            sys.stdout.write(json.dumps(observation, separators=(",", ":"), sort_keys=True) + "\n")
            sys.stdout.flush()
            return 0
        except BaseException:
            failure_stage = "postchecks"
    try:
        marker = "credential_verify:refused"
        if failure_stage in FAILURE_STAGES:
            marker += ":" + failure_stage
        sys.stderr.write(marker + "\n")
    except BaseException:
        pass
    return 1


if __name__ == "__main__":
    sys.exit(main())
