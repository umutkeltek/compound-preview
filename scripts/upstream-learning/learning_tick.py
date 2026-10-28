#!/usr/bin/env python3
"""One governed evidence tick; no models, adoption, installs or external writes."""
from __future__ import annotations
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid
import upstream_scout as scout

ACTOR = 'scheduler:upstream-learning'

class TickError(RuntimeError):
    pass

def terminate_child(p):
    try:
        os.killpg(p.pid, signal.SIGTERM)
        try:
            p.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(p.pid, signal.SIGKILL)
            p.communicate()
    except ProcessLookupError:
        p.wait()

class CLI:
    def __init__(self, binary, deadline, command_timeout=90):
        self.binary, self.deadline = binary, deadline
        self.command_timeout = command_timeout

    def call(self, *args):
        left = self.deadline - time.monotonic()
        if left <= 0:
            raise TickError('tick deadline exceeded')
        p = subprocess.Popen([self.binary, *map(str, args)], text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            out, err = p.communicate(timeout=min(self.command_timeout, left))
        except subprocess.TimeoutExpired:
            terminate_child(p)
            raise TickError('Compound command timed out: ' + ' '.join(args[:2]))
        except BaseException:
            terminate_child(p)
            raise
        if p.returncode:
            raise TickError(' '.join(args[:2]) + ': ' + (err or out)[-2000:])
        return out

    def json(self, *args):
        try:
            return json.JSONDecoder().raw_decode(self.call(*args).lstrip())[0]
        except ValueError as e:
            raise TickError('Expected JSON from Compound command') from e

def write(path, value):
    scout.atomic_json(str(path), value)

def pending(output):
    state_file = output / '.state.json'
    paths = sorted((output / 'pending').glob('srcobs_*.json'))
    if not state_file.exists():
        return paths
    state = json.loads(state_file.read_text())
    if state.get('schema_version') != scout.STATE_SCHEMA or not isinstance(state.get('observations'), dict):
        raise TickError('Invalid scout state; do not reset delivery history')
    active = {i for i, value in state['observations'].items() if value.get('status') == 'pending'}
    result = [p for p in paths if p.stem in active]
    if active != {p.stem for p in result}:
        raise TickError('Pending observation is missing its delivery record')
    return result

def safe_file(path, root):
    path = Path(path)
    if path.is_symlink() or not path.is_file():
        raise TickError('Evidence is not a regular file')
    try:
        path.resolve().relative_to(root.resolve())
    except ValueError as e:
        raise TickError('Evidence escaped staging directory') from e
    return path

def finding_id(value):
    if isinstance(value, dict):
        if re.fullmatch(r'find_[a-f0-9]+', str(value.get('id', ''))):
            return value['id']
        for v in value.values():
            found = finding_id(v)
            if found:
                return found
    return None

def intake(cli, work, output, pending_path):
    item = json.loads(pending_path.read_text())
    obs_path = safe_file(item['source_observation_file'], output)
    obs = json.loads(obs_path.read_text())
    if obs.get('id') != item['observation_id'] or obs.get('schema_version') != scout.OBS_SCHEMA:
        raise TickError('Outbox identity/schema mismatch')
    if not obs.get('maps_to') or any(m.get('promote_allowed') is not False for m in obs['maps_to']):
        raise TickError('Upstream observations must remain no-promote')
    refs = {}
    for body_path in item['body_files'].values():
        body = safe_file(body_path, output / 'content')
        digest = hashlib.sha256(body.read_bytes()).hexdigest()
        if body.stem != digest:
            raise TickError('Staged body hash mismatch')
        result = cli.json('finding', 'record', '--topic', 'upstream-content:' + digest,
            '--body-file', str(body), '--work-id', work,
            '--produced-by', 'agent:compound-upstream-scout', '--evidence', obs['source_ref']['path'], '--json')
        identity = finding_id(result)
        if not identity:
            raise TickError('Finding custody returned no identity')
        refs['pending_body:sha256:' + digest] = 'finding:' + identity
    context_path = output / 'ticks' / (obs['id'] + '.context.json')
    write(context_path, {'observation': obs, 'custodied_refs': refs,
        'trust': 'Untrusted external source data; unassessed relevance and usefulness'})
    context = cli.json('finding', 'record', '--topic', 'upstream-candidate:' + obs['id'],
        '--body-file', str(context_path), '--work-id', work,
        '--produced-by', 'agent:compound-upstream-scout', '--json')
    context_id = finding_id(context)
    if not context_id:
        raise TickError('Context custody returned no identity')
    evidence = []
    for ref in obs['evidence_refs']:
        if ref.startswith('pending_body:') and ref not in refs:
            raise TickError('Observation has an uncustodied body')
        evidence.append(refs.get(ref, ref))
    obs['evidence_refs'] = evidence + ['finding:' + context_id]
    for key in ('scout_context', 'content_revision', 'invalidates_observation_id'):
        obs.pop(key, None)
    target = output / 'ticks' / (obs['id'] + '.intake.json')
    write(target, obs)
    result = cli.call('source', 'observe', str(target))
    candidates = re.findall(r'^- candidate: ([A-Za-z0-9_-]+)$', result, re.MULTILINE)
    if not candidates or '- status: ok' not in result:
        raise TickError('Source intake did not prove candidate creation')
    return {'observation_id': obs['id'], 'candidate_ids': candidates, 'finding_refs': list(refs.values()) + ['finding:' + context_id]}

def begin(cli, work, config, output):
    cli.call('work', 'create', '--work-id', work, '--owner', 'compound:capability-learning',
        '--predicate', 'Pinned changed source evidence reaches no-promote intake, or a truthful failure receipt is emitted',
        '--outcome', 'One bounded upstream evidence acquisition and intake pass', '--by', ACTOR,
        '--creator-harness', 'script', '--creator-model', 'none', '--creator-lane', 'upstream-learning',
        '--state', 'ready', '--canonical-surface', 'compound://source/observations', '--risk', 'tier1', '--privacy', 'private')
    cli.call('work', 'claim', work, '--by', ACTOR)
    cli.call('work', 'advance', work, '--to', 'running', '--by', ACTOR, '--note', 'Scheduled bounded evidence tick')
    intent = {'schema_version': 'compound.action_intent.v0.1', 'id': 'act_' + work[5:],
        'created_at': dt.datetime.now(dt.timezone.utc).isoformat(),
        'producer': {'kind': 'cli', 'name': 'compound-upstream-learning'}, 'ids': {'work_id': work},
        'scope': {'entity_id': 'system:compound'}, 'authority': {'source_class': 'runtime'},
        'privacy': 'private', 'risk': 'tier1', 'uncertainty': {'level': 'known'},
        'evidence_refs': ['sha256:' + hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()],
        'actor': ACTOR, 'capability': 'filesystem.write', 'action': 'observe_pinned_upstream',
        'target': {'kind': 'directory', 'path': str(output), 'id': 'upstream-learning'},
        'mutation_class': 'local_reversible', 'reversibility': 'easy', 'blast_radius': 'repo',
        'estimated_cost': {'usd': 0, 'tokens': 0, 'human_review_minutes': None},
        'preview': 'Read <=3 configured public GitHub sources; stage bounded pinned text; custody through finding record and source observe; ack proven intake only. No models, installs, promotion, grants or external writes.',
        'rollback_plan': 'Stop schedule and preserve pending/evidence; no-promote source observations remain evidence only.'}
    path = output / 'ticks' / (work + '.intent.json')
    write(path, intent)
    decision = cli.json('work', 'act', work, '--intent', str(path))
    if decision.get('action_intent_id') != intent['id'] or decision.get('outcome') not in ('allow', 'allow_with_obligations'):
        raise TickError('No allowing policy decision for the exact source action')
    return decision['id']

def tick(cli, config, output):
    work = 'work_upstream_scout_' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S') + '_' + uuid.uuid4().hex[:6]
    implementation = Path(__file__).read_bytes() + Path(scout.__file__).read_bytes()
    report = {'work_id': work, 'consumed': [], 'failures': [],
        'implementation_revision': 'sha256:' + hashlib.sha256(implementation).hexdigest(),
        'config_revision': 'sha256:' + hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()}
    (output / 'ticks').mkdir(mode=0o700, exist_ok=True)
    try:
        report['policy_decision_id'] = begin(cli, work, config, output)
        items = pending(output)
        if not items:
            result = scout.run(config, str(output))
            report['failures'].extend(result.get('failures', []))
            items = pending(output)
        for path in items[:3]:
            result = intake(cli, work, output, path)
            scout.run(config, str(output), ack=result['observation_id'])
            report['consumed'].append(result)
    except Exception as e:
        report['failures'].append({'stage': 'tick', 'error': str(e)[:2000]})
    report_path = output / 'ticks' / (work + '.result.json')
    write(report_path, report)
    cli.deadline = max(cli.deadline, time.monotonic() + 90)
    try:
        proof = 'file:' + str(report_path)
        cli.call('work', 'advance', work, '--to', 'verifying', '--by', ACTOR, '--verification', proof,
            '--note', 'Bounded acquisition/intake result persisted')
        args = ['work', 'close', work, '--outcome', 'failed' if report['failures'] else 'completed',
            '--move-type', 'audit_only', '--by', ACTOR, '--proof', proof, '--summary',
            'upstream scout: consumed=%d failures=%d; no adoption' % (len(report['consumed']), len(report['failures']))]
        if report['consumed']:
            args.append('--changed')
        cli.call(*args)
    except Exception as e:
        report['failures'].append({'stage': 'close', 'error': str(e)[:2000]})
        write(report_path, report)
    return report

def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', required=True)
    p.add_argument('--output-dir', required=True)
    p.add_argument('--compound', default=str(Path.home() / '.local/bin/compound'))
    p.add_argument('--timeout', type=int, default=300)
    p.add_argument('--dry-run', action='store_true')
    args = p.parse_args(argv)
    config = json.loads(Path(args.config).read_text())
    sources = scout.validate_config(config)
    if len(sources) > 3 or not 60 <= args.timeout <= 600:
        p.error('At most three sources and 60..600 second timeout required')
    if args.dry_run:
        print(json.dumps({'dry_run': True, 'sources': len(sources), 'pending': len(pending(Path(args.output_dir))), 'mutations': False}))
        return 0
    output = Path(scout.ensure_output(args.output_dir))
    def interrupted(signum, frame):
        # A first stop enters normal failure finalization; a repeated signal
        # must not interrupt the receipt write halfway through its transaction.
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        raise TickError('tick interrupted by signal ' + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    with open(output / '.tick.lock', 'a+') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(json.dumps({'status': 'skipped_locked'}))
            return 0
        report = tick(CLI(args.compound, time.monotonic() + args.timeout), config, output)
        print(json.dumps(report))
        return 1 if report['failures'] else 0

if __name__ == '__main__':
    sys.exit(main())
