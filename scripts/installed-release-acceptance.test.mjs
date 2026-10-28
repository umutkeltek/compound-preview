import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import test from 'node:test';
import {verifyBundle} from './installed-release-acceptance.mjs';

test('saved continuity uses a capable local tuple and executes the exact worker payload', async t => {
  const acceptance = await import('./installed-release-acceptance.mjs');
  assert.equal(typeof acceptance.createContinuityWorkerFixture, 'function');
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'compound-continuity-fixture-'));
  t.after(() => fs.rmSync(root, {recursive: true}));
  const worker = acceptance.createContinuityWorkerFixture(root);
  assert.equal(worker.profile.models[worker.profileID].harness, 'codex');
  assert.deepEqual(worker.profile.models[worker.profileID].aliases, [worker.executable]);
  assert.equal(worker.profile.models[worker.unsupportedProfileID].harness, 'os-harness-entrypoints');
  const payload = 'COMPOUND_INSTALLED_WORKER_PAYLOAD=' + JSON.stringify({code: 'console.log(JSON.stringify(process.argv.slice(1)))', args: ['two words', '--', 'literal']});
  const options = {encoding: 'utf8', env: {...process.env, PATH: '/usr/bin:/bin:/usr/sbin:/sbin'}};
  const result = spawnSync(worker.executable, ['exec', payload], options);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), ['two words', '--', 'literal']);
  const governed = spawnSync(worker.executable, ['exec', '# Current governed work work_synthetic\n\nCurrent source context.\n\n' + payload], options);
  assert.equal(governed.status, 0, governed.stderr);
  assert.equal(governed.stdout, result.stdout);
  for (const wrong of [payload + payload, '{}']) {
    const refused = spawnSync(worker.executable, ['exec', wrong], options);
    assert.notEqual(refused.status, 0, 'ambiguous or missing payload boundary must refuse');
    assert.equal(refused.stdout, '');
  }
  const invalid = spawnSync(worker.executable, ['unknown', payload], options);
  assert.notEqual(invalid.status, 0, 'wrong launch grammar must not execute the payload');
  assert.equal(invalid.stdout, '');
});

const commit = '0123456789abcdef0123456789abcdef01234567';
const names = ['RELEASE', 'install-release.sh', ...['darwin_arm64', 'linux_amd64'].flatMap(p => [
  `compound_0.1.0_${p}.tar.gz`, `compound_0.1.0_${p}.cdx.json`,
  `compound_0.1.0_${p}_compound-breadth-engine.cdx.json`,
])];
function checksums(dir) {
  fs.writeFileSync(path.join(dir, 'SHA256SUMS'), names.map(name =>
    `${crypto.createHash('sha256').update(fs.readFileSync(path.join(dir, name))).digest('hex')}  ${name}\n`).join(''));
}
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'compound-installed-fixture-'));
  t.after(() => fs.rmSync(dir, {recursive: true}));
  for (const name of names) fs.writeFileSync(path.join(dir, name), 'synthetic artifact\n');
  fs.writeFileSync(path.join(dir, 'RELEASE'), `schema compound.release.v0.2\nversion 0.1.0\ncommit ${commit}\ninstaller install-release.sh\n` +
    ['darwin arm64', 'linux amd64'].map(p => {
      const tag = p.replace(' ', '_');
      return `artifact ${p} compound_0.1.0_${tag}.tar.gz compound_0.1.0_${tag}.cdx.json compound_0.1.0_${tag}_compound-breadth-engine.cdx.json\n`;
    }).join(''));
  checksums(dir);
  return dir;
}
test('exact nine-file candidate inventory is accepted', t => {
  const result = verifyBundle(fixture(t), commit, 'linux_amd64');
  assert.equal(result.commit, commit);
  assert.equal(result.version, '0.1.0');
  assert.equal(result.artifacts.length, 9);
});
test('changed archive bytes fail rather than falling back to another binary', t => {
  const dir = fixture(t);
  fs.appendFileSync(path.join(dir, 'compound_0.1.0_linux_amd64.tar.gz'), 'changed');
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /checksum/);
});
test('internally consistent wrong candidate still fails', t => {
  const dir = fixture(t);
  const file = path.join(dir, 'RELEASE');
  fs.writeFileSync(file, fs.readFileSync(file, 'utf8').replace(commit, 'a'.repeat(40)));
  checksums(dir);
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /candidate/);
});
test('missing and unexpected files fail the closed inventory', t => {
  const dir = fixture(t);
  fs.writeFileSync(path.join(dir, 'unexpected'), 'extra');
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /inventory/);
  fs.unlinkSync(path.join(dir, 'unexpected'));
  fs.unlinkSync(path.join(dir, 'install-release.sh'));
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /inventory/);
});
test('symlinks and duplicate checksum entries are rejected', t => {
  const dir = fixture(t);
  const file = path.join(dir, 'install-release.sh');
  fs.unlinkSync(file);
  fs.symlinkSync('RELEASE', file);
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /regular/);
  fs.unlinkSync(file);
  fs.writeFileSync(file, 'synthetic artifact\n');
  checksums(dir);
  fs.appendFileSync(path.join(dir, 'SHA256SUMS'), fs.readFileSync(path.join(dir, 'SHA256SUMS'), 'utf8').split('\n')[0] + '\n');
  assert.throws(() => verifyBundle(dir, commit, 'linux_amd64'), /checksum inventory/);
});
test('unsupported platform cannot masquerade as a native release profile', t => {
  assert.throws(() => verifyBundle(fixture(t), commit, 'linux_arm64'), /platform/);
});
test('CLI missing inputs refuses instead of silently doing no acceptance', () => {
  const result = spawnSync(process.execPath, [new URL('./installed-release-acceptance.mjs', import.meta.url).pathname], {encoding: 'utf8'});
  assert.equal(result.status, 2);
  assert.match(result.stderr, /usage:/);
});
test('CLI never overwrites an existing acceptance location', t => {
  const dir = fixture(t);
  const sentinel = path.join(dir, 'operator-owned');
  fs.writeFileSync(sentinel, 'keep exact bytes');
  const result = spawnSync(process.execPath, [new URL('./installed-release-acceptance.mjs', import.meta.url).pathname, dir, commit, dir, dir, commit], {encoding: 'utf8'});
  assert.equal(result.status, 1);
  assert.match(result.stderr, /absent/);
  assert.equal(fs.readFileSync(sentinel, 'utf8'), 'keep exact bytes');
});
test('existing Proof workflow actually invokes native installed acceptance without publication', () => {
  const workflow = fs.readFileSync(new URL('../.github/workflows/proof.yml', import.meta.url), 'utf8');
  const native = workflow.slice(workflow.indexOf('\n  installed-release:'));
  assert.match(native, /runs-on: ubuntu-24\.04/);
  assert.match(native, /node --test scripts\/installed-release-acceptance\.test\.mjs/);
  assert.match(native, /node scripts\/installed-release-acceptance\.mjs/);
  assert.match(native, /name: proof-installed-release-evidence/);
  assert.doesNotMatch(native, /contents: write|attestations: write|gh release|git tag/);
});
