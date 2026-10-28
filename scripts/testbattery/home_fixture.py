#!/usr/bin/env python3
"""Restricted source-only home test fixture. Proof authority remains in internal/work."""
import argparse
import hashlib
import contextlib
import errno
import fcntl
import secrets
import re
import time
import io
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import tarfile

SYSTEM_POLICY = Path("/System/Library/Sandbox/Profiles/dyld-support.sb")
LIMIT = 256 << 20
PROBE = r'''package main
import("bytes";"errors";"fmt";"net";"os";"os/exec";"strconv";"syscall")
func denied(e error)bool{return errors.Is(e,syscall.EPERM)||errors.Is(e,syscall.EACCES)}
func main(){
 fd,_:=strconv.Atoi(os.Args[1]);buf:=make([]byte,64)
 n,_:=syscall.Pread(fd,buf,0)
 if n>0&&bytes.Equal(buf[:n],[]byte("outside fixture sentinel")){panic("INHERITED_SENTINEL")}
 for _,p:=range os.Args[2:4]{
  if _,e:=os.ReadFile(p);!denied(e){panic(fmt.Sprintf("outside read: %v",e))}
  if _,e:=os.Stat(p);!denied(e){panic(fmt.Sprintf("outside stat: %v",e))}
 }
 for _,p:=range os.Args[6:]{
  fd,e:=syscall.Open(p,syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW,0)
  if e!=nil{panic(fmt.Sprintf("ancestor no-follow open: %v",e))};syscall.Close(fd)
  if _,e:=os.ReadDir(p);e!=nil{panic(fmt.Sprintf("ancestor enumeration: %v",e))}
 }
 if data,e:=os.ReadFile(os.Args[4]);e!=nil||len(data)==0{panic("positive source read failed")}
 for _,p:=range os.Args[4:6]{
  f,e:=os.OpenFile(p,os.O_WRONLY|os.O_APPEND,0)
  if f!=nil{f.Close()};if !denied(e){panic(fmt.Sprintf("input write: %v",e))}
 }
 if e:=exec.Command("/bin/cat",os.Args[2]).Run();e==nil{panic("child outside read allowed")}
 l,e:=net.Listen("tcp",":0");if l!=nil{l.Close();panic("network allowed")};if !denied(e){panic(fmt.Sprintf("network error: %v",e))}
 fmt.Println("fixture boundary controls passed")
}
'''


class CacheUnavailable(RuntimeError):
    pass

class PinnedStage:
    """Cooperating helpers hold a lock; all destructive I/O is descriptor-relative."""
    def __init__(self, path, create=False):
        self.path = path
        self.fds = []
        self.links = []
        try:
            fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            self.fds.append(fd)
            parts = path.parts[1:]
            for index, name in enumerate(parts):
                if create and index >= len(parts) - 2:
                    try:
                        os.mkdir(name, 0o700, dir_fd=fd)
                    except FileExistsError:
                        pass
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
                self.fds.append(child)
                if index >= len(parts) - 2:
                    st = os.fstat(child)
                    if st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o700:
                        raise RuntimeError("fixture directory identity/permissions")
                self.links.append((fd, name, child))
                fd = child
            self.fd = fd
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise CacheUnavailable("fixture is busy") from error
            self.check()
        except BaseException:
            self.close()
            raise

    def check(self):
        for parent, name, child in self.links:
            live = os.stat(name, dir_fd=parent, follow_symlinks=False)
            pinned = os.fstat(child)
            if not stat.S_ISDIR(live.st_mode) or (live.st_dev, live.st_ino) != (pinned.st_dev, pinned.st_ino):
                raise RuntimeError("fixture ancestor attachment changed")

    def close(self):
        for fd in reversed(self.fds):
            os.close(fd)
        self.fds = []

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

PIN = None
REMOVE_HOOK = lambda *args: None

@contextlib.contextmanager
def directory(path):
    PIN.check()
    parts = path.relative_to(PIN.path).parts
    fd = os.dup(PIN.fd)
    try:
        for name in parts:
            child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
            os.close(fd)
            fd = child
        yield fd
        PIN.check()
    finally:
        os.close(fd)

def regular(info):
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
        raise RuntimeError("fixture leaf is not an owned single-link regular file")

def read_leaf(path):
    with directory(path.parent) as parent:
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        try:
            info = os.fstat(fd)
            regular(info)
            if info.st_size > LIMIT:
                raise RuntimeError("fixture leaf exceeds input bound")
            with os.fdopen(os.dup(fd), "rb") as source:
                data = source.read(LIMIT + 1)
            if len(data) > LIMIT:
                raise RuntimeError("fixture leaf grew beyond input bound")
            return data
        finally:
            os.close(fd)

def write_leaf(path, body, mode=0o600):
    with directory(path.parent) as parent:
        try:
            regular(os.stat(path.name, dir_fd=parent, follow_symlinks=False))
            if read_leaf(path) == body:
                fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
                try:
                    regular(os.fstat(fd))
                    if stat.S_IMODE(os.fstat(fd).st_mode) != mode:
                        os.fchmod(fd, mode)
                finally:
                    os.close(fd)
                return
        except FileNotFoundError:
            pass
        temporary = ".fixture-write-" + secrets.token_hex(12)
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode, dir_fd=parent)
        try:
            with os.fdopen(os.dup(fd), "wb") as destination:
                destination.write(body)
                destination.flush()
            os.fchmod(fd, mode)
            os.fsync(fd)
            os.rename(temporary, path.name, src_dir_fd=parent, dst_dir_fd=parent)
        finally:
            os.close(fd)
            try:
                os.unlink(temporary, dir_fd=parent)
            except FileNotFoundError:
                pass

def make_directory(path, parents=False, mode=0o700):
    if parents and path != PIN.path:
        make_directory(path.parent, parents=True, mode=mode)
    if path == PIN.path:
        return
    with directory(path.parent) as parent:
        try:
            os.mkdir(path.name, mode, dir_fd=parent)
        except FileExistsError:
            pass
        fd = os.open(path.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        try:
            info = os.fstat(fd)
            if info.st_uid != os.getuid():
                raise RuntimeError("fixture directory owner changed")
        finally:
            os.close(fd)

class RemovalBudget:
    def __init__(self, operations=200000, seconds=60):
        self.left = operations
        self.deadline = time.monotonic() + seconds

    def check(self):
        if self.left <= 0 or time.monotonic() >= self.deadline:
            raise CacheUnavailable("exact fixture cleanup budget exhausted; stage retained")

    def consume(self):
        self.check()
        self.left -= 1

def bounded_children(fd, budget):
    with os.scandir(fd) as entries:
        while True:
            budget.check()
            try:
                entry = next(entries)
            except StopIteration:
                return
            yield entry.name

def remove_entry(parent, name, budget):
    if name in ("", ".", "..") or "/" in name:
        raise RuntimeError("invalid fixture child name")
    budget.consume()
    before = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if stat.S_ISDIR(before.st_mode):
        fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        try:
            opened = os.fstat(fd)
            if (before.st_dev, before.st_ino) != (opened.st_dev, opened.st_ino):
                raise RuntimeError("fixture child changed while opening")
            REMOVE_HOOK(parent, name, fd)
            PIN.check()
            for child in bounded_children(fd, budget):
                remove_entry(fd, child, budget)
            live = os.stat(name, dir_fd=parent, follow_symlinks=False)
            if not stat.S_ISDIR(live.st_mode) or (live.st_dev, live.st_ino) != (opened.st_dev, opened.st_ino):
                raise RuntimeError("fixture child changed during cleanup")
            os.rmdir(name, dir_fd=parent)
        finally:
            os.close(fd)
    else:
        os.unlink(name, dir_fd=parent)

def remove_path(path, budget=None):
    with directory(path.parent) as parent:
        remove_entry(parent, path.name, budget or RemovalBudget())

def clear_directory(path, budget=None):
    with directory(path) as fd:
        before = os.fstat(fd)
        REMOVE_HOOK(fd, ".", fd)
        PIN.check()
        with directory(path) as current:
            after = os.fstat(current)
            if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
                raise RuntimeError("fixture cleanup directory changed")
        budget = budget or RemovalBudget()
        for name in bounded_children(fd, budget):
            remove_entry(fd, name, budget)

def move_path(source, target):
    with directory(source.parent) as src, directory(target.parent) as dst:
        os.rename(source.name, target.name, src_dir_fd=src, dst_dir_fd=dst)

def digest(data):
    return hashlib.sha256(data).hexdigest()

def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()

def run(args, *, cwd=None, env=None, capture=True, extra_fds=()):
    result = subprocess.run(args, cwd=cwd, env=env, close_fds=True,
                            pass_fds=ARGS.fds + tuple(extra_fds),
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if result.returncode:
        raise RuntimeError("command failed: " + repr(args[:3]) + "\n" +
                           ((result.stdout or b"") + (result.stderr or b"")).decode(errors="replace")[-8000:])
    return result.stdout or b""

def metadata(path):
    s = path.lstat()
    return [s.st_dev, s.st_ino, s.st_mode, s.st_uid, s.st_gid,
            s.st_size, s.st_mtime_ns, s.st_ctime_ns]

def tree_records(root, metadata_bound):
    rows = [[".", stat.S_IMODE(root.stat().st_mode), ["dir"], metadata(root) if metadata_bound else []]]
    for current, dirs, files in os.walk(root, followlinks=False):
        dirs.sort()
        files.sort()
        current = Path(current)
        for name in sorted(dirs + files):
            path = current / name
            s = path.lstat()
            relative = path.relative_to(root).as_posix()
            if stat.S_ISLNK(s.st_mode):
                body = ["link", os.readlink(path)]
            elif stat.S_ISDIR(s.st_mode):
                body = ["dir"]
            elif stat.S_ISREG(s.st_mode):
                body = ["file", digest(read_leaf(path))]
            else:
                raise RuntimeError("unsupported fixture entry: " + relative)
            rows.append([relative, stat.S_IMODE(s.st_mode), body,
                         metadata(path) if metadata_bound else []])
    return rows

def owned_directory(path):
    if path.exists() or path.is_symlink():
        s = path.lstat()
        if not stat.S_ISDIR(s.st_mode) or s.st_uid != os.getuid() or stat.S_IMODE(s.st_mode) != 0o700:
            raise RuntimeError("fixture directory identity/permissions refused")
    else:
        path.mkdir(mode=0o700)

def build_environment(stage):
    base = {k: os.environ[k] for k in ("PATH", "HOME", "GOCACHE") if k in os.environ}
    base.update(GOENV="off", GOWORK="off", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    base["GOMODCACHE"] = run([ARGS.go, "env", "GOMODCACHE"], env=base).decode().strip()
    base["GOCACHE"] = run([ARGS.go, "env", "GOCACHE"], env=base).decode().strip()
    base["HOME"] = str(stage / "scratch" / "home")
    base["TMPDIR"] = str(stage / "scratch" / "tmp")
    return base

def test_environment(stage):
    return {"HOME": str(stage / "scratch" / "home"),
            "TMPDIR": str(stage / "scratch" / "tmp"),
            "PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
            "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local"}

def install_identical(source, target):
    write_leaf(target, read_leaf(source), 0o700)
    remove_path(source)

def extract_source(archive, target):
    make_directory(target)
    links = []
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        total = 0
        for item in tar:
            name = PurePosixPath(item.name)
            if name.is_absolute() or ".." in name.parts or not name.parts:
                raise RuntimeError("unsafe source archive path")
            path = target.joinpath(*name.parts)
            if item.isdir():
                make_directory(path, parents=True, mode=0o755)
            elif item.isfile():
                total += item.size
                if item.size < 0 or total > LIMIT:
                    raise RuntimeError("source members exceed input bound")
                make_directory(path.parent, parents=True, mode=0o755)
                with tar.extractfile(item) as source:
                    write_leaf(path, source.read(item.size + 1), 0o755 if item.mode & 0o111 else 0o644)
                with directory(path.parent) as parent:
                    os.utime(path.name, (item.mtime, item.mtime), dir_fd=parent, follow_symlinks=False)
            elif item.issym():
                links.append((path, item.linkname))
            else:
                raise RuntimeError("unsupported source archive member")
    for path, link in links:
        make_directory(path.parent, parents=True, mode=0o755)
        with directory(path.parent) as parent:
            os.symlink(link, path.name, dir_fd=parent)

def prepare_source(stage):
    # This checkout is already source-bound by the native proof reservation.
    head = run(["git", "-C", ARGS.repo, "rev-parse", "HEAD"]).decode().strip()
    if head != ARGS.source_head:
        raise RuntimeError("source HEAD drift")
    if run(["git", "-C", ARGS.repo, "status", "--porcelain=v1", "--untracked-files=all"]):
        raise RuntimeError("source checkout is dirty")
    child = subprocess.Popen(["git", "-C", ARGS.repo, "archive", ARGS.source_head],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             close_fds=True, pass_fds=ARGS.fds)
    try:
        archive = child.stdout.read(LIMIT + 1)
        if len(archive) > LIMIT:
            child.kill()
            raise RuntimeError("source fixture exceeds bounded archive input")
        stderr = child.stderr.read()
        if child.wait():
            raise RuntimeError("git archive failed: " + stderr.decode(errors="replace")[-2000:])
    finally:
        if child.poll() is None:
            child.kill()
        child.wait()
    fresh = stage / "scratch" / "source.next"
    if fresh.exists() or fresh.is_symlink():
        raise RuntimeError("incomplete source preparation requires inspection")
    extract_source(archive, fresh)
    source = stage / "source"
    if source.is_symlink():
        raise RuntimeError("source fixture symlink refused")
    same = source.exists() and tree_records(fresh, False) == tree_records(source, False)
    if same:
        remove_path(fresh)
    else:
        if source.exists():
            remove_path(source)
        move_path(fresh, source)

def profile(stage):
    roots = list(stage.parents) + [stage]
    literal = lambda p: "(literal " + json.dumps(str(p)) + ")"
    subpath = lambda p: "(subpath " + json.dumps(str(p)) + ")"
    return "\n".join([
        "(version 1)", "(deny default)",
        '(import "/System/Library/Sandbox/Profiles/dyld-support.sb")',
        "(allow process*)", "(allow signal (target same-sandbox))", "(allow sysctl-read)",
        '(allow file-read* file-map-executable (subpath "/System/Library") (subpath "/usr/lib") (literal "/bin/cat") (literal "/bin/sh") (literal "/usr/bin/true"))',
        '(allow file-read* file-write* (literal "/dev/null") (literal "/dev/zero") (literal "/dev/random") (literal "/dev/urandom"))',
        "(allow file-read* " + " ".join(map(literal, roots)) + ")",
        "(allow file-read* file-map-executable " + subpath(stage / "source") + " " +
        literal(stage / "home.test") + " " + literal(stage / "probe") + ")",
        "(allow file-read* file-write* " + subpath(stage / "scratch") + ")",
    ]) + "\n"

def ancestor_digest(stage):
    paths = set(stage.parents) | {stage}
    readable = set(paths)
    # The imported dyld profile grants reads of these intermediate directories.
    for raw in ["/System/Volumes/Preboot/Cryptexes/App/System",
                "/System/Volumes/Preboot/Cryptexes/Incoming/OS",
                "/System/Volumes/Preboot/Cryptexes/OS",
                "/System/Cryptexes/App", "/System/Cryptexes/OS"]:
        readable.update(Path(raw).parents)
    paths.update(readable)
    rows = []
    for path in sorted(paths):
        if not path.exists():
            rows.append([str(path), "absent"])
            continue
        info = metadata(path)
        if path.is_symlink():
            rows.append([str(path), info, "link", os.readlink(path), metadata(path.resolve())])
        elif path.is_dir():
            entries = sorted([entry.name, entry.inode(), stat.S_IFMT(entry.stat(follow_symlinks=False).st_mode)]
                             for entry in os.scandir(path)) if path in readable else None
            rows.append([str(path), info, entries])
        else:
            raise RuntimeError("fixture ancestor is not a directory")
    return digest(encoded(rows))

def inputs(stage):
    read_only = []
    for name in ("home.test", "probe", "policy.sb"):
        p = stage / name
        if not p.is_file() or p.is_symlink():
            raise RuntimeError("fixture input missing or replaced")
        read_only.append([name, metadata(p), digest(read_leaf(p))])
    host = [os.getuid(), os.geteuid(), os.getgid(), os.getegid(), sorted(os.getgroups()), sys.platform, os.uname(), sys.version, str(Path(sys.executable).resolve()),
            digest(Path(sys.executable).resolve().read_bytes()), os.stat(stage).st_dev]
    # Boot timestamps move with wall-clock corrections; the kernel session UUID
    # identifies a reboot without treating a clock adjustment as a new host.
    for key in ("kern.osversion", "kern.bootsessionuuid", "hw.machine", "hw.logicalcpu"):
        try:
            value = run(["/usr/sbin/sysctl", "-n", key]).decode().strip()
        except RuntimeError as error:
            if key == "kern.bootsessionuuid":
                raise CacheUnavailable("kernel boot session identity unavailable") from error
            raise
        if key == "kern.bootsessionuuid" and not re.fullmatch(
                r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", value):
            raise CacheUnavailable("invalid kernel boot session identity")
        host.append([key, value])
    return {
        "stage_directory": str(stage),
        "binary_sha256": digest(read_leaf(stage / "home.test")),
        "fixture_sha256": digest(encoded([tree_records(stage / "source", True), read_only])),
        "sandbox_sha256": digest(encoded([profile(stage), PROBE, digest((stage / "probe").read_bytes())])),
        "system_policy_sha256": digest(SYSTEM_POLICY.read_bytes()),
        "environment_sha256": digest(encoded(test_environment(stage))),
        "host_sha256": digest(encoded(host)),
        "ancestors_sha256": ancestor_digest(stage),
    }

def sandbox_args(stage, executable, args):
    return ["/usr/bin/sandbox-exec", "-f", str(stage / "policy.sb"), str(executable)] + args

def controls(stage):
    outside = stage / "outside-sentinel"
    write_leaf(outside, b"outside fixture sentinel")
    with directory(stage) as stagefd, os.fdopen(os.open(outside.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=stagefd), "rb") as sentinel:
        args = [str(sentinel.fileno()), str(outside), "/System/Volumes/Data" + str(outside),
                str(stage / "source" / "go.mod"), str(stage / "home.test"), str(stage)] + [str(parent) for parent in stage.parents]
        command = sandbox_args(stage, stage / "probe", args)
        leaked = subprocess.run(command, cwd=stage / "scratch", env=test_environment(stage),
                                close_fds=True, pass_fds=ARGS.fds + (sentinel.fileno(),),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if leaked.returncode == 0 or b"INHERITED_SENTINEL" not in leaked.stderr:
            raise RuntimeError("inherited-FD negative control did not fail")
        run(command, cwd=stage / "scratch", env=test_environment(stage))

def prepare(stage):
    if os.environ.get("GOFLAGS"):
        raise RuntimeError("fixture source phase requires default build flags")
    owner = stage / "owner"
    if owner.exists():
        if owner.is_symlink() or read_leaf(owner).decode() != ARGS.identity:
            raise RuntimeError("fixture cache ownership mismatch")
    else:
        with os.scandir(PIN.fd) as entries:
            nonempty = next(entries, None) is not None
        if nonempty:
            raise RuntimeError("unowned fixture cache is not empty")
        write_leaf(owner, ARGS.identity.encode())
    scratch = stage / "scratch"
    make_directory(scratch)
    clear_directory(scratch)
    for name in ("home", "tmp"):
        make_directory(scratch / name)
    prepare_source(stage)
    env = build_environment(stage)
    run([ARGS.go, "test", "-c", "-trimpath", "-mod=readonly", "-o", str(stage / "scratch" / "home.next"), "./internal/home"],
        cwd=stage / "source", env=env)
    install_identical(stage / "scratch" / "home.next", stage / "home.test")
    write_leaf(stage / "probe.go", PROBE.encode())
    run([ARGS.go, "build", "-trimpath", "-o", str(stage / "scratch" / "probe.next"), str(stage / "probe.go")],
        cwd=stage / "scratch", env=env)
    install_identical(stage / "scratch" / "probe.next", stage / "probe")
    policy = stage / "policy.sb"
    expected = profile(stage)
    if not policy.exists() or read_leaf(policy).decode() != expected:
        write_leaf(policy, expected.encode())
    controls(stage)
    print(json.dumps(inputs(stage), separators=(",", ":")))


def retire(stage):
    if not ARGS.identity or read_leaf(stage / "owner").decode() != ARGS.identity:
        raise RuntimeError("fixture retirement ownership mismatch")
    allowed = {"owner", "scratch", "source", "home.test", "probe", "probe.go", "policy.sb", "outside-sentinel"}
    budget = RemovalBudget(operations=50000, seconds=10)
    names = set()
    for name in bounded_children(PIN.fd, budget):
        budget.consume()
        if name not in allowed:
            raise CacheUnavailable("unexpected cache slots; exact stage retained")
        names.add(name)
    for name in sorted(names - {"owner", "scratch"}):
        remove_entry(PIN.fd, name, budget)
    if "scratch" in names:
        clear_directory(stage / "scratch", budget)
    PIN.check()
    print("home fixture payload retired; exact owner marker retained")

def require_inputs(stage, expected):
    current = inputs(stage)
    if current == expected:
        return
    if {k: v for k, v in current.items() if k != "ancestors_sha256"} == {
            k: v for k, v in expected.items() if k != "ancestors_sha256"}:
        raise CacheUnavailable("observable ancestor metadata changed")
    raise RuntimeError("fixture inputs drifted")

def execute_tests(stage):
    command = sandbox_args(stage, stage / "home.test",
                           ["-test.count=1", "-test.timeout=40m", "-test.paniconexit0", "-test.v"])
    child = subprocess.Popen(command, cwd=stage / "source" / "internal" / "home",
                             env=test_environment(stage), close_fds=True, pass_fds=ARGS.fds,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    unexpected = []
    ownership_skip_reason = False
    size = 0
    try:
        while True:
            line = child.stdout.readline((1 << 20) + 1)
            if not line:
                break
            size += len(line)
            if len(line) > (1 << 20) or size > (64 << 20):
                raise RuntimeError("fixture test output exceeds bounded capture")
            sys.stdout.buffer.write(line)
            sys.stdout.buffer.flush()
            if b"current test user cannot construct foreign-owned fixture:" in line and b"operation not permitted" in line:
                ownership_skip_reason = True
            match = re.match(rb"^\s*--- SKIP: (\S+) \(", line)
            if match:
                name = match.group(1)
                allowed = name == b"TestCapturedRetainedMillionOutcome"
                if name == b"TestRepairPrivateCustodyRejectsForeignOwnedAncestorWhenPermitted":
                    allowed = os.geteuid() != 0 and ownership_skip_reason
                if not allowed:
                    unexpected.append(name.decode(errors="replace"))
        result = child.wait()
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()
        child.stdout.close()
    if result == 0 and unexpected:
        raise CacheUnavailable("restricted test skipped: " + ", ".join(unexpected[:20]))
    return result

def execute_sealed_tests(stage, expected):
    # A test process must never forge a helper protocol disposition via os.Exit.
    if execute_tests(stage) != 0:
        return 1
    try:
        require_inputs(stage, expected)
    except CacheUnavailable as error:
        # The complete source suite ran successfully. Ancestor drift forbids
        # reuse, but an ordinary fresh run observes those same ambient inputs.
        print("home fixture: direct tests passed without reuse certificate: " + str(error),
              file=sys.stderr)
        return 76
    return 0

def main():
    global ARGS, PIN
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("prepare", "execute", "verify", "retire"))
    parser.add_argument("--stage", required=True)
    parser.add_argument("--identity", default="")
    parser.add_argument("--repo", default="")
    parser.add_argument("--source-head", default="")
    parser.add_argument("--go", default="")
    parser.add_argument("--barrier-count", type=int, default=0)
    parser.add_argument("--expected-inputs", default="")
    ARGS = parser.parse_args()
    if ARGS.barrier_count < 0 or ARGS.barrier_count > 16:
        raise RuntimeError("invalid proof barrier descriptor count")
    ARGS.fds = tuple(range(3, 3 + ARGS.barrier_count))
    for fd in ARGS.fds:
        os.fstat(fd)
    stage = Path(ARGS.stage)
    if not stage.is_absolute() or stage != Path(os.path.realpath(stage)):
        raise RuntimeError("fixture stage must be canonical")
    with PinnedStage(stage, create=ARGS.action == "prepare") as pin:
        PIN = pin
        if ARGS.action == "prepare":
            if not ARGS.identity or not ARGS.repo or not ARGS.source_head or not ARGS.go:
                raise RuntimeError("fixture preparation requires bound source identity")
            prepare(stage)
            return
        expected = json.loads(ARGS.expected_inputs)
        require_inputs(stage, expected)
        if ARGS.action == "retire":
            retire(stage)
            return
        if ARGS.action == "execute":
            raise SystemExit(execute_sealed_tests(stage, expected))

if __name__ == "__main__":
    try:
        main()
    except CacheUnavailable as error:
        print("home fixture unavailable: " + str(error), file=sys.stderr)
        raise SystemExit(75)
    except Exception as error:
        print("home fixture: " + str(error), file=sys.stderr)
        raise SystemExit(1)
