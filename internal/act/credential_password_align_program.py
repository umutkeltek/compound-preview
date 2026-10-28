"""Closed password alignment. Appended to the unchanged verifier definitions.

Only the installed native owner constructs this wire. Passwords, SCRAM values,
SQL, stderr and private recovery records never cross its observation boundary.
"""
import base64
import fcntl
import pty
import termios
from urllib.parse import urlsplit


ALIGN_SCHEMA = "compound.credential_password_alignment_request.v0.1"
ALIGN_EFFECTS = "closed-postgres-two-role-password-alignment-v1"
ALIGN_NAMESPACE = ".compound-credential-password-alignment"
ALIGN_OBSERVATION = "compound.credential_password_alignment_observation.v0.1"

# Failed-only metadata. Never derive a marker from an exception or catalog value.
ALIGN_FAILURE_STAGES = frozenset({
    "request_validation", "runtime_preconditions", "inspection_negative_auth",
    "inspection_session", "inspection_catalog_read", "inspection_catalog_shape",
    "inspection_catalog_scram", "inspection_closed_state", "inspection_base_catalog",
    "inspection_postchecks", "password_operation", "deadline", "cleanup",
    "observation_encoding",
})


class AlignmentProgress:
    def __init__(self):
        self.stage = "request_validation"

    def mark(self, stage):
        require(stage in ALIGN_FAILURE_STAGES)
        self.stage = stage


def alignment_base_request(r):
    result = {k: v for k, v in r.items() if k not in ("failure", "catalog", "profile", "capture", "operation", "origin")}
    result["schema_version"] = "compound.credential_verification_request.v0.1"
    result["effect_set"] = "existing-local-postgres-maintenance-auth-v1"
    return result


def alignment_validate(r):
    exact_keys(r, "schema_version profile_id attempt_id program_sha256 effect_set witness resource ssh host file roles database failure catalog profile capture operation origin")
    require(r["schema_version"] == ALIGN_SCHEMA and r["effect_set"] == ALIGN_EFFECTS)
    validate_request(alignment_base_request(r))
    exact_keys(r["catalog"], "sha256 roles database_oid docker_sha256")
    if r["operation"] != "inspect":
        require(re.fullmatch(r"[0-9a-f]{64}", r["catalog"]["sha256"]) is not None)
        require(type(r["catalog"]["database_oid"]) is int and r["catalog"]["database_oid"] > 0)
    require(type(r["catalog"]["roles"]) is list and len(r["catalog"]["roles"]) == 2)
    for role in r["catalog"]["roles"]:
        exact_keys(role, "oid xmin ctid")
        if r["operation"] == "inspect":
            continue
        require(type(role["oid"]) is int and role["oid"] > 0)
        require(re.fullmatch(r"[0-9]{1,10}", role["xmin"]) is not None)
        require(re.fullmatch(r"\([0-9]{1,10},[0-9]{1,5}\)", role["ctid"]) is not None)
    if r["operation"] != "inspect":
        require(r["catalog"]["roles"][0]["oid"] != r["catalog"]["roles"][1]["oid"])
    require(re.fullmatch(r"[0-9a-f]{64}", r["catalog"]["docker_sha256"]) is not None)


def alignment_catalog_sql(r):
    a, m = (role["name"] for role in r["roles"])
    database = r["database"]["name"]
    # Names have the verifier's closed lowercase identifier grammar. No caller
    # supplies SQL, expressions, operators or a selectable query.
    return """SELECT pg_catalog.json_build_object(
      'roles',(SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
        'oid',a.oid::bigint,'xmin',a.xmin::text,'ctid',a.ctid::text,
        'attributes',pg_catalog.to_jsonb(a)-'rolpassword','verifier',a.rolpassword)
        ORDER BY CASE WHEN a.rolname='%s' THEN 0 ELSE 1 END)
        FROM pg_catalog.pg_authid a WHERE a.rolname IN ('%s','%s')),
      'memberships',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(m) ORDER BY m.roleid,m.member,m.grantor),'[]'::jsonb)
        FROM pg_catalog.pg_auth_members m WHERE m.roleid IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ('%s','%s'))
        OR m.member IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ('%s','%s'))),
      'database',(SELECT pg_catalog.to_jsonb(d) FROM pg_catalog.pg_database d WHERE d.datname='%s'),
      'database_revision',(SELECT pg_catalog.json_build_object('xmin',d.xmin::text,'ctid',d.ctid::text) FROM pg_catalog.pg_database d WHERE d.datname='%s'),
      'settings',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(s) ORDER BY s.setdatabase,s.setrole),'[]'::jsonb)
        FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase IN (0,(SELECT oid FROM pg_catalog.pg_database WHERE datname='%s'))
        OR s.setrole IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ('%s','%s'))),
      'hba',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(h) ORDER BY h.rule_number),'[]'::jsonb) FROM pg_catalog.pg_hba_file_rules h),
      'preload',pg_catalog.current_setting('shared_preload_libraries'),
      'session_preload',pg_catalog.current_setting('session_preload_libraries'),
      'local_preload',pg_catalog.current_setting('local_preload_libraries'),
      'sessions',(SELECT pg_catalog.count(*) FROM pg_catalog.pg_stat_activity WHERE usename IN ('%s','%s') OR datname='%s'),
      'application_connect',pg_catalog.has_database_privilege('%s','%s','CONNECT'),
      'public_connect',(SELECT EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl,pg_catalog.acldefault('d'::pg_catalog.\"char\",d.datdba))) x
        WHERE x.grantee=0 AND x.privilege_type='CONNECT') FROM pg_catalog.pg_database d WHERE d.datname='%s'),
      'maintenance_connect',pg_catalog.json_build_array(pg_catalog.has_database_privilege('%s','postgres','CONNECT'),pg_catalog.has_database_privilege('%s','postgres','CONNECT')))::text;
    """ % (a, a, m, a, m, a, m, database, database, database, a, m, a, m, database, a, database, database, a, m)


def alignment_public_catalog(value, revisions=True):
    result = decode_json(json.dumps(value).encode())
    for role in result["roles"]:
        del role["verifier"]
        if not revisions:
            del role["xmin"]
            del role["ctid"]
    return result


def alignment_scram(value):
    require(type(value) is str and len(value) <= 1024)
    match = re.fullmatch(r"SCRAM-SHA-256\$([1-9][0-9]{0,8}):([A-Za-z0-9+/]+={0,2})\$([A-Za-z0-9+/]+={0,2}):([A-Za-z0-9+/]+={0,2})", value)
    require(match is not None)
    try:
        salt, stored, server = (base64.b64decode(match[i], validate=True) for i in (2, 3, 4))
    except (ValueError, TypeError):
        raise Refusal()
    require(8 <= len(salt) <= 1024 and len(stored) == 32 and len(server) == 32)


def alignment_closed_catalog(value, r, progress=None):
    exact_keys(value, "roles memberships database database_revision settings hba preload session_preload local_preload sessions application_connect public_connect maintenance_connect")
    require(type(value["roles"]) is list and len(value["roles"]) == 2)
    for index, role in enumerate(value["roles"]):
        if progress is not None:
            progress.mark("inspection_catalog_shape")
        exact_keys(role, "oid xmin ctid attributes verifier")
        require(role["attributes"]["rolname"] == r["roles"][index]["name"])
        require(role["attributes"]["rolcanlogin"] is True)
        if progress is not None:
            progress.mark("inspection_catalog_scram")
        alignment_scram(role["verifier"])
    if progress is not None:
        progress.mark("inspection_closed_state")
    require(value["database"]["datname"] == r["database"]["name"])
    require(value["database"]["datconnlimit"] == 0)
    require(value["database"]["datallowconn"] == r["database"]["allow_connections"])
    require(value["sessions"] == 0 and type(value["sessions"]) is int)
    require(value["application_connect"] is False and value["public_connect"] is False)
    require(value["maintenance_connect"] == [True, True])
    # Unknown preload hooks may log credential-bearing utility statements even
    # after PostgreSQL's own session-local log suppression. Never guess.
    require(value["preload"] == value["session_preload"] == value["local_preload"] == "")


class AlignmentSession:
    """Duplex psql: SQL uses stdin, native password prompts use /dev/tty.

    No plaintext password is interpolated into SQL. psql calls libpq's SCRAM
    implementation. stderr and terminal output remain bounded private memory.
    """
    def __init__(self, r, processes):
        self.processes = processes
        self.pending = bytearray()
        self.master, slave = pty.openpty()
        attrs = termios.tcgetattr(slave)
        attrs[3] &= ~termios.ECHO
        termios.tcsetattr(slave, termios.TCSANOW, attrs)

        def child_session():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)

        try:
            self.child = subprocess.Popen(client_argv(r, "postgres", privileged=True),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                env=clean_env(), cwd="/", pass_fds=(slave,), preexec_fn=child_session)
            processes.children.append(self.child)
        except BaseException:
            os.close(self.master)
            raise
        finally:
            os.close(slave)
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.child.stdout, selectors.EVENT_READ, "out")
        self.selector.register(self.child.stderr, selectors.EVENT_READ, "err")
        self.selector.register(self.master, selectors.EVENT_READ, "tty")
        self.exchange("BEGIN;\nSET LOCAL log_statement='none';\nSET LOCAL log_min_error_statement='panic';\nSET LOCAL log_min_messages='panic';\nSET LOCAL password_encryption='scram-sha-256';\nSET LOCAL lock_timeout='3s';\nSET LOCAL statement_timeout='8s';\n")
        self.exchange("LOCK TABLE pg_catalog.pg_authid, pg_catalog.pg_auth_members, pg_catalog.pg_database, pg_catalog.pg_db_role_setting IN SHARE ROW EXCLUSIVE MODE;")

    def exchange(self, sql, passwords=()):
        require(type(sql) is str and len(sql) <= 32768)
        sentinel = "compound_alignment_" + secrets.token_hex(16)
        self.child.stdin.write((sql + "\n\\echo " + sentinel + "\n").encode("ascii"))
        self.child.stdin.flush()
        terminal = bytearray()
        output = bytearray()
        sent = 0
        while True:
            for key, _ in self.selector.select(min(self.processes.remaining(), 0.1)):
                raw = os.read(key.fd, 4096)
                require(raw and key.data != "err")
                if key.data == "tty":
                    terminal.extend(raw)
                    require(len(terminal) <= 4096)
                    while b": " in terminal:
                        prompt, terminal_tail = terminal.split(b": ", 1)
                        terminal = bytearray(terminal_tail)
                        if prompt.startswith(b"\r\r\n"):
                            prompt = prompt[3:]
                        elif prompt.startswith(b"\r\n"):
                            prompt = prompt[2:]
                        elif prompt.startswith(b"\n"):
                            prompt = prompt[1:]
                        require(sent < len(passwords))
                        expected = (b'Enter new password for user "' + passwords[sent][0].encode("ascii") + b'"') if sent % 2 == 0 else b"Enter it again"
                        require(bytes(prompt) == expected)
                        require(termios.tcgetattr(self.master)[3] & termios.ECHO == 0)
                        os.write(self.master, passwords[sent][1].encode("utf-8") + b"\n")
                        sent += 1
                else:
                    output.extend(raw)
                    require(len(output) <= 1_048_576)
                    marker = sentinel.encode("ascii") + b"\n"
                    if output.endswith(marker):
                        require(sent == len(passwords) and bytes(terminal) in (b"", b"\r\r\n", b"\r\n", b"\n"))
                        value = bytes(output[:-len(marker)])
                        for _, password in passwords:
                            require(password.encode("utf-8") not in value)
                        return value
            require(self.child.poll() is None)

    def catalog(self, r):
        return decode_json(self.exchange(alignment_catalog_sql(r)))

    def align(self, r, passwords):
        for role, password in zip(r["roles"], passwords):
            pairs = [(role["name"], password), (role["name"], password)]
            require(not self.exchange("\\password " + role["name"], pairs))

    def restore(self, r, image):
        for role, old in zip(r["roles"], image["roles"]):
            verifier = old["verifier"]
            alignment_scram(verifier)
            require(not self.exchange("ALTER ROLE " + role["name"] + " PASSWORD '" + verifier + "';"))

    def commit(self):
        require(not self.exchange("COMMIT;"))

    def close(self):
        try:
            if self.child.poll() is None:
                self.child.stdin.write(b"ROLLBACK;\n\\q\n")
                self.child.stdin.flush()
                self.child.stdin.close()
                self.child.stdin = None
                try:
                    require(self.child.wait(timeout=min(self.processes.remaining(), 3)) == 0)
                except subprocess.TimeoutExpired:
                    os.killpg(self.child.pid, signal.SIGKILL)
                    self.child.wait(timeout=1)
        finally:
            self.selector.close()
            os.close(self.master)


def alignment_namespace(r, create):
    parts = r["file"]["path"].split("/")
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in parts[1:-1]:
            require(part not in ("", ".", ".."))
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=directory)
            os.close(directory)
            directory = child
            info = os.fstat(directory)
            require(info.st_uid == info.st_gid == 0 and info.st_mode & 0o022 == 0)
        if create:
            try:
                os.mkdir(ALIGN_NAMESPACE, 0o700, dir_fd=directory)
                os.fsync(directory)
            except FileExistsError:
                pass
        namespace = os.open(ALIGN_NAMESPACE, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=directory)
        try:
            AlignmentCustody.directory_check(namespace)
            return namespace
        except BaseException:
            os.close(namespace)
            raise
    finally:
        os.close(directory)


class AlignmentCustody:
    def __init__(self, r, action, create):
        require(re.fullmatch(r"act_[0-9a-f]{12}", action) is not None)
        namespace = alignment_namespace(r, create)
        try:
            if create:
                # A previous operation always requires its own recovery.
                require(not os.listdir(namespace))
                os.mkdir(action, 0o700, dir_fd=namespace)
                os.fsync(namespace)
            self.fd = os.open(action, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=namespace)
            try:
                self.directory_check(self.fd)
                self.identity = os.fstat(self.fd)
            except BaseException:
                os.close(self.fd)
                raise
        finally:
            os.close(namespace)

    @staticmethod
    def directory_check(fd):
        info = os.fstat(fd)
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == info.st_gid == 0 and stat.S_IMODE(info.st_mode) == 0o700)

    @classmethod
    def recovery(cls, parent, action, create):
        require(re.fullmatch(r"act_[0-9a-f]{12}", action) is not None)
        name = "recovery_" + action
        if create:
            os.mkdir(name, 0o700, dir_fd=parent.fd)
            os.fsync(parent.fd)
        value = cls.__new__(cls)
        value.fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent.fd)
        try:
            cls.directory_check(value.fd)
            value.identity = os.fstat(value.fd)
            return value
        except BaseException:
            os.close(value.fd)
            raise

    def put(self, name, value):
        require(name in ("PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"))
        raw = json.dumps(value, separators=(",", ":"), sort_keys=True).encode("utf-8") + b"\n"
        require(len(raw) <= 1_048_576)
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=self.fd)
        try:
            self.file_check(fd)
            view = memoryview(raw)
            while view:
                written = os.write(fd, view)
                require(written > 0)
                view = view[written:]
            os.fsync(fd)
            self.file_check(fd)
        finally:
            os.close(fd)
        os.fsync(self.fd)
        return hashlib.sha256(raw).hexdigest()

    @staticmethod
    def file_check(fd):
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == info.st_gid == 0 and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1)

    def get(self, name):
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=self.fd)
        try:
            self.file_check(fd)
            before = os.fstat(fd)
            raw = bounded_read(fd, 1_048_576)
            after = os.fstat(fd)
            require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
                    (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns))
            self.file_check(fd)
            return decode_json(raw)
        finally:
            os.close(fd)

    def close(self):
        os.close(self.fd)


def alignment_runtime_inactive(r, processes):
    require(program_hash("/usr/bin/docker") == r["catalog"]["docker_sha256"])
    frozen = r["profile"]["containers"]
    require(type(frozen) is list and all(type(name) is str for name in frozen)
            and len(set(frozen)) == len(frozen))
    endpoint = r["profile"]["web_api_base"]
    require(type(endpoint) is str and re.fullmatch(r"http://[a-z0-9][a-z0-9.-]{0,127}:[0-9]{2,5}", endpoint) is not None)
    api = urlsplit(endpoint).hostname
    require(type(api) is str and api.endswith("-api") and api in frozen)
    prefix = api[:-4]
    require(re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,100}", prefix) is not None)
    # Preserve the source capture's legacy shape. A commercial target must name
    # its exact seven services, and also retain the legacy tunnel absence check.
    legacy = [prefix + suffix for suffix in ("-api", "-worker", "-provider-sync", "-web", "-cloudflared")]
    commercial = [prefix + suffix for suffix in ("-auth", "-api", "-worker", "-billing", "-billing-reconcile", "-provider-sync", "-web")]
    is_legacy = 0 < len(frozen) <= len(legacy) and set(frozen).issubset(legacy)
    is_commercial = len(frozen) == len(commercial) and set(frozen) == set(commercial)
    require(is_legacy or is_commercial)
    names = legacy if is_legacy else list(dict.fromkeys(legacy + commercial))
    child = processes.spawn(["/usr/bin/docker", "container", "ls", "--all", "--format", "{{.Names}}"], clean_env())
    code, out, err = processes.complete(child, b"")
    require(code == 0 and not err and len(out) <= 16384)
    require(not set(out.decode("ascii").splitlines()).intersection(names))


def alignment_catalog_pin(value, r):
    require(int(value["database"]["oid"]) == r["catalog"]["database_oid"])
    require([{k: role[k] for k in ("oid", "xmin", "ctid")} for role in value["roles"]] == r["catalog"]["roles"])
    # Only this opaque commitment crosses the observation boundary. Including
    # the private verifiers binds later recovery to the genuinely inspected OLD2.
    require(canonical_digest(value) == r["catalog"]["sha256"])


def alignment_file_metadata(r):
    """Anchored nofollow lstat only; never open or read the credential body."""
    parts = r["file"]["path"].split("/")
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in parts[1:-1]:
            require(part not in ("", ".", ".."))
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=directory)
            os.close(directory)
            directory = child
            info = os.fstat(directory)
            require(info.st_uid == info.st_gid == 0 and info.st_mode & 0o022 == 0)
        require(stat_matches(os.stat(parts[-1], dir_fd=directory, follow_symlinks=False), r["file"]))
    finally:
        os.close(directory)


def alignment_classify(current, before, after, restored=()):
    require(alignment_public_catalog(current, False) == alignment_public_catalog(before, False))
    if all(current["roles"][i] == before["roles"][i] for i in range(2)):
        return "old_old"
    if after is not None and all(current["roles"][i] == after["roles"][i] for i in range(2)):
        return "new_new"
    matches = [image for image in restored if current["roles"] == image["roles"]]
    require(len(matches) <= 1)
    if matches:
        return "old_old"
    raise Refusal()


def alignment_restored_images(custody, pre, after):
    entries = os.listdir(custody.fd)
    require(len(entries) <= 68)
    result = []
    for name in sorted(entries):
        if name in ("PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"):
            # Verify even a currently unused safe record's type/link custody.
            custody.get(name)
            continue
        require(re.fullmatch(r"recovery_act_[0-9a-f]{12}", name) is not None)
        action = name[len("recovery_"):]
        journal = AlignmentCustody.recovery(custody, action, False)
        try:
            require(set(os.listdir(journal.fd)).issubset({"PREIMAGE.json", "POSTIMAGE.json", "COMMIT_INTENT.json", "RESULT.json"}))
            # Validate all present record custody even when a particular body
            # does not contribute a candidate restored catalog.
            for record in os.listdir(journal.fd):
                journal.get(record)
            old = journal.get("PREIMAGE.json")
            require(old["schema_version"] == "compound.credential_password_recovery_preimage.v0.1")
            require(old["action_intent_id"] == action and old["original_action_intent_id"] == pre["action_intent_id"])
            require(old["request_sha256"] == canonical_digest(old["request"]))
            for key in ("file", "host", "resource", "ssh", "roles", "database", "profile", "capture", "failure"):
                require(old["request"][key] == pre["request"][key])
            if old["request"]["operation"] != "restore":
                require(old["request"]["operation"] == "reconcile")
                continue
            require(after is not None and old["catalog"]["roles"] == after["roles"])
            require(alignment_public_catalog(old["catalog"], False) == alignment_public_catalog(pre["catalog"], False))
            try:
                post = journal.get("POSTIMAGE.json")
            except FileNotFoundError:
                require("COMMIT_INTENT.json" not in os.listdir(journal.fd))
                continue
            require(post["schema_version"] == "compound.credential_password_recovery_postimage.v0.1")
            require(post["action_intent_id"] == action and post["request_sha256"] == old["request_sha256"])
            image = post["catalog"]
            require(alignment_public_catalog(image, False) == alignment_public_catalog(pre["catalog"], False))
            require([row["verifier"] for row in image["roles"]] == [row["verifier"] for row in pre["catalog"]["roles"]])
            try:
                intent = journal.get("COMMIT_INTENT.json")
            except FileNotFoundError:
                continue
            require(intent == {"schema_version": "compound.credential_password_recovery_commit_intent.v0.1", "action_intent_id": action, "request_sha256": old["request_sha256"]})
            result.append(image)
        finally:
            journal.close()
    return result


def alignment_probe(r, deadline):
    probes = Processes(deadline)
    try:
        result = run_protocol(alignment_base_request(r), NativeRuntime(probes))
        require(len(result["authentication"]) == 4)
    finally:
        require(probes.cleanup())


def alignment_protocol(r, operation, action, original_action, processes, progress=None):
    if progress is None:
        progress = AlignmentProgress()
    alignment_validate(r)
    require(operation in ("inspect", "align", "reconcile", "restore"))
    require(operation == r["operation"])
    require(re.fullmatch(r"act_[0-9a-f]{12}", action) is not None)
    require((operation in ("align", "inspect") and original_action == action) or
            (operation not in ("align", "inspect") and re.fullmatch(r"act_[0-9a-f]{12}", original_action) is not None and original_action != action))
    runtime = NativeRuntime(processes)
    progress.mark("runtime_preconditions")
    runtime.programs(r)
    host_before = runtime.host(r)
    if operation == "inspect":
        # Metadata producer deliberately never calls file(), trusted_open() or
        # parse_passwords() for the protected credential path.
        alignment_file_metadata(r)
        alignment_runtime_inactive(r, processes)
        progress.mark("inspection_negative_auth")
        for role in r["roles"]:
            runtime.negative(r, role, secrets.token_urlsafe(48))
        progress.mark("inspection_session")
        session = AlignmentSession(r, processes)
        try:
            progress.mark("inspection_catalog_read")
            catalog = session.catalog(r)
            progress.mark("inspection_catalog_shape")
            alignment_closed_catalog(catalog, r, progress)
            progress.mark("inspection_base_catalog")
            validate_catalog(runtime.catalog(r, []), r, [])
            metadata = {"sha256": canonical_digest(catalog),
                        "roles": [{k: row[k] for k in ("oid", "xmin", "ctid")} for row in catalog["roles"]],
                        "database_oid": int(catalog["database"]["oid"]), "docker_sha256": r["catalog"]["docker_sha256"]}
            progress.mark("inspection_postchecks")
            require(runtime.host(r) == host_before)
            alignment_file_metadata(r)
            alignment_runtime_inactive(r, processes)
            return {"schema_version": ALIGN_OBSERVATION, "operation": operation, "action_intent_id": action,
                    "original_action_intent_id": action, "request_sha256": canonical_digest(r), "classification": "metadata_only",
                    "effect": "observed", "closed": True, "protected_file_unchanged": True, "attributes_unchanged": True,
                    "configuration_accepted": False, "credential_qualification_accepted": False, "catalog": metadata,
                    "resolved_recovery_actions": []}
        finally:
            try:
                session.close()
            except BaseException:
                progress.mark("cleanup")
                raise
    progress.mark("password_operation")
    namespace_lock = alignment_namespace(r, operation == "align")
    try:
        fcntl.flock(namespace_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        raw_before = runtime.file(r)
        passwords = parse_passwords(raw_before, r["roles"])
        alignment_runtime_inactive(r, processes)
    except BaseException:
        os.close(namespace_lock)
        raise
    session = None
    custody = None
    journal = None
    effect = "not_attempted"
    classification = "unknown"
    recovery_actions = []
    try:
        session = AlignmentSession(r, processes)
        before = session.catalog(r)
        alignment_closed_catalog(before, r)
        if operation == "align":
            alignment_catalog_pin(before, r)
            custody = AlignmentCustody(r, action, True)
            pre = {"schema_version": "compound.credential_password_preimage.v0.1", "action_intent_id": action,
                   "request_sha256": canonical_digest(r), "request": r, "host": host_before, "catalog": before}
            custody.put("PREIMAGE.json", pre)
            # Durable preimage exists before the first credential-bearing SQL.
            effect = "transaction_attempted"
            session.align(r, passwords)
            after = session.catalog(r)
            alignment_closed_catalog(after, r)
            require(alignment_public_catalog(before, False) == alignment_public_catalog(after, False))
            require(all(after["roles"][i]["verifier"] != before["roles"][i]["verifier"] for i in range(2)))
            custody.put("POSTIMAGE.json", {"schema_version": "compound.credential_password_postimage.v0.1",
                        "action_intent_id": action, "request_sha256": canonical_digest(r), "catalog": after})
            require(runtime.file(r) == raw_before and runtime.host(r) == host_before)
            alignment_runtime_inactive(r, processes)
            custody.put("COMMIT_INTENT.json", {"schema_version": "compound.credential_password_commit_intent.v0.1",
                        "action_intent_id": action, "request_sha256": canonical_digest(r)})
            effect = "commit_unknown"
            session.commit()
            effect = "committed"
            classification = "new_new"
        else:
            custody = AlignmentCustody(r, original_action, False)
            pre = custody.get("PREIMAGE.json")
            require(pre["schema_version"] == "compound.credential_password_preimage.v0.1")
            require(pre["action_intent_id"] == original_action)
            require(pre["request_sha256"] == canonical_digest(pre["request"]))
            require(pre["request_sha256"] == r["origin"]["request_sha256"])
            alignment_validate(pre["request"])
            alignment_closed_catalog(pre["catalog"], pre["request"])
            alignment_catalog_pin(pre["catalog"], pre["request"])
            # Recovery must retain every original protected resource/host/profile
            # binding. Only freshness and the separately admitted operation vary.
            for key in ("file", "host", "resource", "ssh", "roles", "database", "profile", "capture", "failure"):
                require(pre["request"][key] == r[key])
            require(pre["request"]["catalog"]["docker_sha256"] == r["catalog"]["docker_sha256"])
            alignment_catalog_pin(before, r)
            require(pre["host"] == host_before)
            try:
                post = custody.get("POSTIMAGE.json")
                require(post["schema_version"] == "compound.credential_password_postimage.v0.1")
                require(post["action_intent_id"] == original_action and post["request_sha256"] == pre["request_sha256"])
                after = post["catalog"]
            except FileNotFoundError:
                after = None
            restored_images = alignment_restored_images(custody, pre, after)
            classification = alignment_classify(before, pre["catalog"], after, restored_images)
            recovery_actions = sorted(name[len("recovery_"):] for name in os.listdir(custody.fd) if name.startswith("recovery_"))
            if classification == "new_new":
                require(custody.get("COMMIT_INTENT.json") == {
                    "schema_version": "compound.credential_password_commit_intent.v0.1",
                    "action_intent_id": original_action, "request_sha256": pre["request_sha256"]})
            if operation == "restore" and recovery_actions:
                # An unsuccessful recovery is not a restore retry grant. A
                # completed reconciliation must cover every preceding journal.
                reconciled = False
                for previous in recovery_actions:
                    candidate = AlignmentCustody.recovery(custody, previous, False)
                    try:
                        try:
                            result = candidate.get("RESULT.json")
                        except FileNotFoundError:
                            continue
                        if (result.get("operation") == "reconcile" and result.get("effect") == "reconciled_new_new"
                                and result.get("original_action_intent_id") == original_action
                                and set(result.get("resolved_recovery_actions", ())) == set(recovery_actions) - {previous}):
                            reconciled = True
                    finally:
                        candidate.close()
                require(reconciled)
            journal = AlignmentCustody.recovery(custody, action, True)
            journal.put("PREIMAGE.json", {"schema_version": "compound.credential_password_recovery_preimage.v0.1",
                "action_intent_id": action, "original_action_intent_id": original_action,
                "request_sha256": canonical_digest(r), "request": r, "catalog": before})
            if operation == "restore":
                require(classification == "new_new")
                # Exact recorded postimage is the only legal restore preimage.
                effect = "transaction_attempted"
                session.restore(r, pre["catalog"])
                restored = session.catalog(r)
                require(alignment_public_catalog(restored, False) == alignment_public_catalog(pre["catalog"], False))
                require([x["verifier"] for x in restored["roles"]] == [x["verifier"] for x in pre["catalog"]["roles"]])
                journal.put("POSTIMAGE.json", {"schema_version": "compound.credential_password_recovery_postimage.v0.1",
                    "action_intent_id": action, "request_sha256": canonical_digest(r), "catalog": restored})
                require(runtime.file(r) == raw_before and runtime.host(r) == host_before)
                alignment_runtime_inactive(r, processes)
                journal.put("COMMIT_INTENT.json", {"schema_version": "compound.credential_password_recovery_commit_intent.v0.1",
                    "action_intent_id": action, "request_sha256": canonical_digest(r)})
                effect = "commit_unknown"
                session.commit()
                effect = "restored"
            else:
                require(not session.exchange("ROLLBACK;"))
                effect = "reconciled_" + classification
        session.close()
        session = None
        if operation == "align" or (operation == "reconcile" and classification == "new_new"):
            alignment_probe(r, processes.deadline)
        runtime.recheck(r)
        require(runtime.file(r) == raw_before and runtime.host(r) == host_before)
        alignment_runtime_inactive(r, processes)
        # Privileged TCP probes have exited. Recheck a new transaction rather
        # than treating a pre-commit MVCC snapshot as a postcondition.
        session = AlignmentSession(r, processes)
        final = session.catalog(r)
        alignment_closed_catalog(final, r)
        expected = after if operation == "align" or (operation == "reconcile" and classification == "new_new") else pre["catalog"]
        require(alignment_public_catalog(final, False) == alignment_public_catalog(expected, False))
        require([x["verifier"] for x in final["roles"]] == [x["verifier"] for x in expected["roles"]])
        session.close()
        session = None
        # The observation does not contain any catalog row, password, verifier,
        # DSN, query or diagnostic string. It is not a credential qualification.
        result = {"schema_version": ALIGN_OBSERVATION, "operation": operation, "action_intent_id": action,
                  "original_action_intent_id": original_action, "request_sha256": canonical_digest(r),
                  "classification": classification, "effect": effect, "closed": True,
                  "protected_file_unchanged": True, "attributes_unchanged": True,
                  "configuration_accepted": False, "credential_qualification_accepted": False, "catalog": None,
                  "resolved_recovery_actions": recovery_actions}
        if operation == "align":
            custody.put("RESULT.json", result)
        else:
            journal.put("RESULT.json", result)
        return result
    finally:
        try:
            if session is not None:
                session.close()
            if custody is not None:
                custody.close()
            if journal is not None:
                journal.close()
        finally:
            os.close(namespace_lock)


def alignment_main():
    processes = None
    observation = None
    accepted = False
    progress = AlignmentProgress()

    def interrupted(_signum, _frame):
        raise StageRefusal("deadline")

    try:
        signal.signal(signal.SIGALRM, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGHUP, interrupted)
        signal.setitimer(signal.ITIMER_REAL, 76.0)
        raw = sys.stdin.buffer.read(32769)
        require(0 < len(raw) <= 32768)
        wire = decode_json(raw)
        exact_keys(wire, "request action_intent_id original_action_intent_id issued_ns deadline_ns")
        r = wire["request"]
        require(len(sys.argv) == 2 and r["program_sha256"] == sys.argv[1])
        issued, until = wire["issued_ns"], wire["deadline_ns"]
        require(type(issued) is str and re.fullmatch(r"[0-9]{19}", issued) is not None)
        require(type(until) is str and re.fullmatch(r"[0-9]{19}", until) is not None)
        now = time.time_ns()
        require(0 < int(until) - int(issued) <= 80_000_000_000)
        require(-2_000_000_000 <= now - int(issued) <= 15_000_000_000)
        budget = min(73.0, (int(until) - now) / 1_000_000_000 - 4.0)
        require(budget > 0)
        deadline = time.monotonic() + budget
        signal.setitimer(signal.ITIMER_REAL, budget)
        processes = Processes(deadline)
        observation = alignment_protocol(r, r["operation"], wire["action_intent_id"], wire["original_action_intent_id"], processes, progress)
        require(time.monotonic() < deadline)
        accepted = True
    except BaseException as error:
        if isinstance(error, StageRefusal) and error.stage == "deadline":
            progress.mark("deadline")
        accepted = False
    finally:
        try:
            signal.setitimer(signal.ITIMER_REAL, 0)
        except BaseException:
            progress.mark("cleanup")
            accepted = False
        if processes is not None:
            try:
                if not processes.cleanup():
                    progress.mark("cleanup")
                    accepted = False
            except BaseException:
                progress.mark("cleanup")
                accepted = False
    if accepted:
        try:
            raw = json.dumps(observation, separators=(",", ":"), sort_keys=True).encode("ascii") + b"\n"
            require(len(raw) <= 4096)
            sys.stdout.buffer.write(raw)
            sys.stdout.buffer.flush()
            return 0
        except BaseException:
            progress.mark("observation_encoding")
    # The caller treats any launched failure as unknown effect, regardless of
    # whether the failure happened before ALTER, COMMIT, or acknowledgement.
    try:
        sys.stderr.write("credential_password_align:refused:" + progress.stage + "\n")
        sys.stderr.flush()
    except BaseException:
        pass
    return 1


if __name__ == "__main__":
    sys.exit(alignment_main())
