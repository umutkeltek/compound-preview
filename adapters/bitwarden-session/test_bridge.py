#!/usr/bin/env python3
"""Secret-custody and continuation falsifiers; synthetic sessions only."""
import base64
import fcntl
import json
import os
from pathlib import Path
import subprocess
import tempfile
import pty
import select
import signal
import time
import sys
import shutil
import importlib.util

sys.dont_write_bytecode = True

SOURCE = Path(__file__).with_name('main.swift')
SWIFTC = shutil.which('swiftc')
assert SWIFTC, 'swiftc is required on macOS'
with tempfile.TemporaryDirectory(prefix='compound-vault-tests-') as scratch:
    root = Path(scratch)
    binary = root / 'bridge-test'
    subprocess.run([SWIFTC, '-D', 'TESTING', str(SOURCE), '-o', str(binary),
                    '-framework', 'Security', '-framework', 'LocalAuthentication'], check=True)
    fake = root / 'bw'
    fake.write_text('#!' + sys.executable + '\n' + '''import base64,json,os,sys
session=base64.b64encode(b'synthetic-session-' + b'x'*46).decode()
if sys.argv[1]=='status':
 print(json.dumps({'status':'unlocked' if os.environ.get('BW_SESSION')==session else os.environ.get('TEST_VAULT_STATUS','unauthenticated')}))
elif sys.argv[1] in ['login','unlock']:
 if os.environ.get('TEST_INTERACTIVE_READ'):
  print('SYNTHETIC_LOGIN_PROMPT', file=sys.stderr, flush=True)
  if sys.stdin.readline().strip()!='synthetic-input': sys.exit(8)
 print('bad-output-credential-sentinel' if os.environ.get('TEST_BAD_OUTPUT') else session)
elif sys.argv[1]=='lock': pass
else: sys.exit(9)
''')
    fake.chmod(0o700)
    store = root / 'session'
    lock = root / 'lock'
    env = {k:v for k,v in os.environ.items() if k not in ['BW_SESSION','BITWARDENCLI_APPDATA_DIR']}
    env.update(COMPOUND_BW_TEST_BIN=str(fake), COMPOUND_BW_TEST_SESSION=str(store), COMPOUND_BW_TEST_LOCK=str(lock))
    token = base64.b64encode(b'synthetic-session-' + b'x'*46).decode()
    checked = 0
    def call(args, code=0, more=None):
        global checked
        result = subprocess.run([str(binary), *args], env={**env, **(more or {})}, capture_output=True, text=True, timeout=40)
        assert result.returncode == code, (args, result.returncode, result.stdout)
        assert token not in result.stdout + result.stderr
        assert 'bad-output-credential-sentinel' not in result.stdout + result.stderr
        checked += 1
        return json.loads(result.stdout)
    assert call(['status'],3)['status']=='unauthenticated'
    call(['connect'],3)  # refuses an agent's noninteractive input
    marker=root/'child-ran'
    call(['run','--','/usr/bin/touch',str(marker)],3)
    assert not marker.exists()
    call(['connect'],3,{'COMPOUND_BW_TEST_INTERACTIVE':'1','TEST_BAD_OUTPUT':'1'})
    assert not store.exists()
    call(['connect'],more={'COMPOUND_BW_TEST_INTERACTIVE':'1'})
    assert store.read_text()==token and store.stat().st_mode & 0o777 == 0o600
    assert call(['status'])['status']=='unlocked'
    assert call(['connect'])['reused'] is True  # no second sign-in
    # A new process receives the session, while only a boolean reaches stdout.
    assert call(['run','--',sys.executable,'-c',
                 'import os,json; print(json.dumps({"received":bool(os.environ.get("BW_SESSION"))}))'])['received']
    with lock.open('a') as held:
        fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
        call(['connect'],3)
    call(['status'],3,{'BITWARDENCLI_APPDATA_DIR':str(root/'other-home')})
    call(['lock'])
    assert not store.exists()
    assert call(['status'],3,{'TEST_VAULT_STATUS':'locked'})['status']=='locked'
    def real_terminal(args, prompt):
        pid, master = pty.fork()
        if pid == 0:
            os.execve(str(binary), [str(binary), *args], {**env, 'TEST_INTERACTIVE_READ':'1'})
        received=b''; sent=False; result=None
        try:
            deadline=time.monotonic()+8
            while time.monotonic()<deadline:
                if select.select([master],[],[],0.1)[0]:
                    try: received+=os.read(master,65536)
                    except OSError: pass
                if not sent and prompt.encode() in received:
                    os.write(master,b'synthetic-input\n');sent=True
                ended,status=os.waitpid(pid,os.WNOHANG)
                if ended: result=os.waitstatus_to_exitcode(status);break
            assert sent, 'real PTY never displayed its interactive prompt'
            assert result==0, 'real PTY stalled or failed; child must own the terminal while reading input'
            assert token.encode() not in received, 'session leaked to interactive terminal'
        finally:
            # Only the synthetic helper and its exact descendants; no production processes.
            rows=subprocess.check_output(['ps','-axo','pid=,ppid='],text=True)
            descendants={pid}
            pairs=[tuple(map(int,line.split())) for line in rows.splitlines()]
            for _ in range(5): descendants.update(child for child,parent in pairs if parent in descendants)
            for child in sorted(descendants,reverse=True):
                try: os.kill(child,signal.SIGKILL)
                except ProcessLookupError: pass
            if result is None:
                try: os.waitpid(pid,0)
                except ChildProcessError: pass
            os.close(master)
    real_terminal(['connect'],'SYNTHETIC_LOGIN_PROMPT');checked+=1
    assert store.read_text()==token
    real_terminal(['run','--',sys.executable,'-c',
                   'import sys; print("SYNTHETIC_CHILD_PROMPT",flush=True); sys.exit(0 if input()=="synthetic-input" else 8)'],
                  'SYNTHETIC_CHILD_PROMPT');checked+=1
    call(['lock'])
    # The production binary has no test-file session backend.
    production = root / 'bridge-production'
    subprocess.run([SWIFTC, str(SOURCE), '-o', str(production), '-framework', 'Security', '-framework', 'LocalAuthentication'], check=True)
    assert b'COMPOUND_BW_TEST_SESSION' not in production.read_bytes()
    spec = importlib.util.spec_from_file_location('bridge_installer', SOURCE.with_name('install.py'))
    installer = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(installer)
    instance = root / 'compound home'; instance.mkdir()
    installed = installer.install(SOURCE, instance, SWIFTC)
    target = Path(installed['binary'])
    original = target.read_bytes()
    assert installer.install(SOURCE, instance, SWIFTC)['status']=='unchanged'
    checked+=1
    with (instance/'actuator/.bitwarden-session.lock').open('a') as busy:
        fcntl.flock(busy,fcntl.LOCK_SH|fcntl.LOCK_NB)
        try: installer.install(SOURCE,instance,SWIFTC)
        except RuntimeError as error: assert 'active' in str(error)
        else: raise AssertionError('installer must not replace an active vault operation')
    checked+=1
    changed = root/'changed.swift';changed.write_text(SOURCE.read_text()+'\n// synthetic revision\n')
    try: installer.install(changed,instance,'/usr/bin/false')
    except subprocess.CalledProcessError: pass
    else: raise AssertionError('failed compile must fail installation')
    assert target.read_bytes()==original
    checked+=1
    desktop=root/'desktop';desktop.mkdir()
    launcher=Path(installer.desktop_launcher(instance,desktop))
    subprocess.run(['/bin/zsh','-n',str(launcher)],check=True)
    installer.desktop_launcher(instance,desktop)
    launcher.write_text('foreign file')
    try: installer.desktop_launcher(instance,desktop)
    except RuntimeError: pass
    else: raise AssertionError('installer must preserve foreign launcher')
    assert launcher.read_text()=='foreign file'
    checked+=1
    print(json.dumps({'ok':True,'checks':checked,'scope':'session custody, missing auth, local prompt gate, child handoff, reuse, lock contention, shared home, lock removal, real PTY login and child input'}))
