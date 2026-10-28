#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {spawnSync} from 'node:child_process';
import assert from 'node:assert/strict';

const [binaryArg, rootArg] = process.argv.slice(2);
if (!binaryArg || !rootArg) {
  console.error('usage: node reproduce.mjs ABSOLUTE_INSTALLED_COMPOUND ABSOLUTE_ABSENT_RUN_DIRECTORY');
  process.exit(2);
}
const binary = path.resolve(binaryArg), root = path.resolve(rootArg);
assert(path.isAbsolute(binaryArg) && path.isAbsolute(rootArg), 'Use absolute paths.');
assert(fs.statSync(binary).isFile(), 'Installed binary must exist.');
assert(!fs.existsSync(root), 'Run directory must be absent; choose a fresh directory.');
fs.mkdirSync(root, {recursive:true, mode:0o700});
const home = path.join(root, 'home'), workspace = path.join(root, 'workspace');
fs.mkdirSync(workspace, {mode:0o700});
const env = {...process.env, COMPOUND_HOME:home};
delete env.COMPOUND_REPO_ROOT;
delete env.COMPOUND_EXECUTION_ROOT;
const steps = [];
function cli(name, args, expected=0) {
  const result = spawnSync(binary, args, {env,cwd:workspace,encoding:'utf8',timeout:90000,maxBuffer:8*1024*1024});
  const record = {name,args,status:result.status,stdout:result.stdout || '',stderr:result.stderr || '',error:result.error?.message || null};
  fs.writeFileSync(path.join(root, String(steps.length+1).padStart(2,'0')+'-'+name+'.json'), JSON.stringify(record,null,2)+'\n', {mode:0o600});
  steps.push({name,status:result.status});
  if (expected === 'nonzero') assert(result.status !== 0 && result.status !== null, name+' must refuse');
  else assert.equal(result.status, expected, name+': '+record.stderr);
  return record;
}
function json(name,args) { return JSON.parse(cli(name,args).stdout); }
const version = json('version',['version','--json']);
cli('initialize',['home','init']);
const context = json('orient',['orient','--harness','codex','--cwd',workspace,'--task','Synthetic local lifecycle demonstration','--json']);
const id = 'work_synthetic_reproduction', actor='agent:synthetic-demo';
cli('create',['work','create','--work-id',id,'--owner',actor,'--by',actor,'--predicate','Exact synthetic output accepted after a rejected adapter','--context-pack',context.id,'--state','ready']);
cli('claim',['work','claim',id,'--by',actor]);
const before = json('before-refusal',['work','show',id,'--json']);
const attemptsBefore = json('attempts-before',['run-attempt','list','--json']);
const rejected = cli('unsupported-adapter',['work','advance',id,'--to','running','--by',actor,'--execute','--harness','synthetic-unsupported-adapter','--workspace',workspace,'--',process.execPath,'-e','throw new Error("must never launch")'],'nonzero');
const after = json('after-refusal',['work','show',id,'--json']);
const attemptsAfter = json('attempts-after',['run-attempt','list','--json']);
assert.deepEqual(after,before,'Refusal must leave the complete work envelope unchanged.');
assert.deepEqual(attemptsAfter,attemptsBefore,'Refusal must not mint a RunAttempt.');
assert.match(rejected.stderr,/harness|adapter/i);
const output = path.join(workspace,'result.txt');
const expected='synthetic governed command completed\n';
const payload='require("node:fs").writeFileSync(process.argv[1],'+JSON.stringify(expected)+'); process.stdout.write('+JSON.stringify(expected)+');';
cli('execute',['work','advance',id,'--to','running','--by',actor,'--execute','--harness','os-harness-entrypoints','--workspace',workspace,'--timeout','90s','--',process.execPath,'-e',payload,output]);
assert.equal(fs.readFileSync(output,'utf8'),expected,'Exact independent output acceptance.');
const verifying = json('verifying',['work','show',id,'--json']);
assert.equal(verifying.state,'verifying');
const priorIDs = new Set(before.run_attempt_ids || []);
const newAttempts = verifying.run_attempt_ids.filter(value => !priorIDs.has(value));
assert.equal(newAttempts.length,1, 'Exactly one new execution attempt.');
const attemptID=newAttempts[0];
assert.match(attemptID,/^ratt_/);
const inventory=json('attempt',['run-attempt','list','--json']);
const attempt=inventory.find(value => value.id === attemptID);
assert(attempt, 'Execution attempt must be independently inspectable.');
assert.equal(attempt.status,'succeeded');
assert.equal(attempt.work_id,id);
const closeResult=cli('close',['work','close',id,'--by',actor,'--outcome','completed','--move-type','audit_only','--summary','Exact local synthetic output independently accepted; no inference or memory benefit claimed']);
const closeBody=JSON.parse(closeResult.stdout.slice(closeResult.stdout.indexOf('{')));
assert.match(closeBody.receipt.id,/^rec_/);
assert.match(closeBody.transition.id,/^tr_/);
const completed = json('completed',['work','show',id,'--json']);
assert.equal(completed.state,'completed');
assert(completed.run_attempt_ids.includes(attemptID));
const doctor=json('proof-chain',['work','proof-chain-doctor','--json']);
assert.equal(doctor.status,'ok');
assert.equal(doctor.counts.complete,1);
const sha = value => crypto.createHash('sha256').update(value).digest('hex');
const report = {
  schema_version:'compound.synthetic_reproduction.v0.1',
  status:'passed', native_inference:false, synthetic:true,
  platform:process.platform, architecture:process.arch,
  binary:{version:version.version,build_sha:version.build_sha,sha256:sha(fs.readFileSync(binary))},
  work_id:id, run_attempt_id:attemptID,
  receipt_id:closeBody.receipt.id, transition_id:closeBody.transition.id,
  execution_policy_decision_id:verifying.lifecycle.findLast(value => value.to === 'running').attached.find(value => value.startsWith('pol_')),
  state_delta:{initial:'ready',claimed:'claimed',rejected_adapter:'claimed',executed:'verifying',accepted:'completed'},
  checks:{refusal_nonzero:true,refusal_work_unchanged:true,refusal_attempts_unchanged:true,exact_output_accepted:true,attempt_succeeded:true,attempt_forward_linked:true,closed_completed:true,proof_chain_doctor_passed:true},
  result_sha256:sha(expected),
  limits:['One local synthetic command.','No model inference, external tester, unattended recovery, automatic learning, or human time saving measured.','Raw run directory includes local paths and must remain private.'],
  steps
};
fs.writeFileSync(path.join(root,'evidence.json'),JSON.stringify(report,null,2)+'\n',{mode:0o600});
console.log(JSON.stringify(report,null,2));
