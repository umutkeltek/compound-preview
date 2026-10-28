#!/usr/bin/env python3
"""Install the shared macOS vault bridge into an existing Compound home."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def regular_or_absent(path):
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise RuntimeError(f'Refusing non-regular install target: {path}')


def install(source, home, compiler):
    home = home.expanduser().resolve(strict=True)
    if not home.is_dir():
        raise RuntimeError('Compound home must be an existing directory')
    actuator = home / 'actuator'
    if actuator.is_symlink():
        raise RuntimeError('Refusing symlinked actuator directory')
    actuator.mkdir(mode=0o700, exist_ok=True)
    binary = actuator / 'bitwarden-session'
    provenance = actuator / 'bitwarden-session.build.json'
    lock_path = actuator / '.bitwarden-session.lock'
    for path in [binary, provenance, lock_path]:
        regular_or_absent(path)
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'a') as held:
        try:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError('A vault operation is active; retry installation after it finishes') from None
        source_hash = digest(source)
        try:
            old = json.loads(provenance.read_text())
        except (OSError, ValueError):
            old = {}
        if (binary.is_file() and old.get('source_sha256') == source_hash
                and old.get('binary_sha256') == digest(binary)
                and stat.S_IMODE(binary.stat().st_mode) == 0o700):
            return {'status': 'unchanged', 'binary': str(binary), **old}
        # A failed build never replaces the installed binary. The same lock used
        # by connect/run/lock prevents an update from interrupting a live user.
        with tempfile.TemporaryDirectory(prefix='.bitwarden-build-', dir=actuator) as scratch:
            built = Path(scratch) / 'bitwarden-session'
            subprocess.run([compiler, str(source), '-o', str(built), '-framework', 'Security',
                            '-framework', 'LocalAuthentication'], check=True)
            subprocess.run([str(built), '--help'], check=True, stdout=subprocess.DEVNULL)
            built.chmod(0o700)
            record = {'schema_version': 'compound.bitwarden_adapter_build.v1',
                      'source_sha256': source_hash, 'binary_sha256': digest(built),
                      'source_path': str(source), 'platform': sys.platform}
            receipt = Path(scratch) / 'build.json'
            receipt.write_text(json.dumps(record, indent=2) + '\n')
            receipt.chmod(0o600)
            for path in [built, receipt]:
                with path.open('rb') as opened:
                    os.fsync(opened.fileno())
            os.replace(built, binary)
            os.replace(receipt, provenance)
        return {'status': 'installed', 'binary': str(binary), **record}


def desktop_launcher(home, desktop):
    home = home.expanduser().resolve(strict=True)
    destination = desktop / 'Bitwarden-connect.command'
    regular_or_absent(destination)
    marker = '# Compound-owned Bitwarden login launcher'
    if destination.exists() and marker not in destination.read_text():
        raise RuntimeError('Desktop launcher already exists without the ownership marker; preserve it')
    binary = shlex.quote(str(home / 'actuator/bitwarden-session'))
    body = f'''#!/bin/zsh
{marker}
set +x
print 'Connect the shared Bitwarden CLI. Enter credentials only in this Terminal.'
print 'The unlock session is stored in macOS Keychain; the master password is not saved.'
COMPOUND_HOME={shlex.quote(str(home))} {binary} connect
result=$?
print ''
if [[ $result -eq 0 ]]; then
  print 'Connected. Authorized harnesses can reuse the shared session.'
else
  print 'Connection did not complete. Read the message above.'
fi
read '?Press Return to close this window. '
exit $result
'''
    fd, temp = tempfile.mkstemp(prefix='.bitwarden-launcher-', dir=desktop)
    try:
        with os.fdopen(fd, 'w') as output:
            output.write(body)
            output.flush()
            os.fchmod(output.fileno(), 0o700)
            os.fsync(output.fileno())
        os.replace(temp, destination)
    finally:
        Path(temp).unlink(missing_ok=True)
    return str(destination)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--home', type=Path, default=Path(os.environ.get('COMPOUND_HOME', Path.home() / '.compound')))
    parser.add_argument('--desktop', action='store_true', help='also create a local login launcher on Desktop')
    args = parser.parse_args()
    if sys.platform != 'darwin':
        parser.error('This Keychain adapter currently supports macOS only')
    compiler = shutil.which('swiftc')
    if not compiler or not shutil.which('bw'):
        parser.error('Install the Swift command-line tools and official Bitwarden bw CLI first')
    try:
        result = install(Path(__file__).with_name('main.swift').resolve(), args.home, compiler)
        if args.desktop:
            result['launcher'] = desktop_launcher(args.home, Path.home() / 'Desktop')
        print(json.dumps({'ok': True, **result}))
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(json.dumps({'ok': False, 'error': str(error)}))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
