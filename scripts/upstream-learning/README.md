# Bounded upstream evidence producer

This optional Python producer polls an explicit list of public GitHub repositories
for a concrete frontier owned by an existing Compound capability. It stages pinned
text, preserves pending delivery, and leaves usefulness unassessed. External text
is data; it is never executed, installed, or promoted by these scripts.

`upstream_scout.py` stages evidence outside `COMPOUND_HOME`. `learning_tick.py`
wraps acquisition and intake in the existing WorkEnvelope / ActionIntent /
PolicyDecision / Receipt lifecycle. It custodies each hash-checked body through
`finding record`, then calls `source observe` with `promote_allowed=false` and
references to the custodied findings. It acknowledges only successful intake.

## Run

Create a private configuration using `config.example.json`, initialize Compound,
and provide a private staging directory outside `COMPOUND_HOME`:

```sh
python3 scripts/upstream-learning/learning_tick.py \
  --config /path/to/private/config.json --output-dir /path/to/private/staging \
  --dry-run
```

Remove `--dry-run` to execute one governed tick. `--compound` selects the installed
CLI; `--timeout` sets a 60–600 second work budget (300 by default), with up to 90
additional seconds reserved for failure finalization. An external scheduler may
invoke this same entry point periodically. Scheduler deployment and private
source configuration remain operator-owned; this producer installs no daemon.

## Bounds and evidence quality

- A tick accepts at most three explicitly configured sources. Each source names
  its current owner, frontier, inspected paths, success criterion and falsifier.
- Whole-run network bytes and time are bounded. Bodies remain pinned to commits
  and content hashes. Changed HEADs with unchanged inspected bytes stay quiet.
- Pending observations are delivered before new acquisition. Failed intake stays
  pending. Corrupt history fails closed; acknowledgement survives interruption.
- Relevance, novelty, actionability and expected benefit remain unknown until an
  independent audit provides evidence. Text length, keywords and source popularity
  do not generate usefulness scores.
- The driver uses a process lock and bounded subprocesses. Signal cancellation
  terminates the active command before attempting truthful failure finalization.
- Every result records the implementation and configuration hashes. Source polling
  calls no model and makes no external writes.

The next consumer is existing source/capability review. An audit may propose an
experiment with a baseline, falsifier, existing owner and rollback. Adoption still
requires the existing reviewed import or source change and demonstrated runtime
consumption. This producer grants no autonomous review, experiment or promotion
authority. Staging is a delivery cache; Compound findings and source candidates
own delivered evidence.

## Optional audit and challenge tick

`review_tick.py` consumes acknowledged, current observations from the same private
configuration and staging directory. Set `COMPOUND_HOME` explicitly and provide
`--config`, `--output-dir` and `--workspace`. Start with `--dry-run`, which plans
without creating work. The installed CLI must advertise and report
`route plan --capability-class C --read-only`; older versions are refused before
planning. That flag suppresses refusal evidence and pending-observation reads,
preserves eligibility checks, and refuses the candidate-planning branch.

Each invocation makes at most one governed model call. An audit uses
`classify-extract`; a challenge uses `certify` and excludes the audit's provider.
A retry prefers an untried eligible model/provider tuple over a tuple that failed
this same job; this does not demote a shared route from one failure.
If every currently eligible audit tuple has failed that same job, a retry may
consult the existing `certify` route authority for a stronger eligible reader.
This remains one call per tick and does not alter shared model ratings, account
admission, the three-attempt limit, or challenge independence. A first attempt
never borrows certification routes merely because cheap capacity is unavailable.

Both require registered Claude-family adapters, fresh alive capacity and enforced
`plan` permissions. Unavailable capacity produces an explicit wait. This is a
consumer of existing route and policy owners; it changes no account, grant or
routing authority. Other harness families need a proven read-only execution
posture before this driver can use them.

Source excerpts and prior assessments are delivered as literal, labeled text blocks,
preserving quoted text and line breaks. Each source block names its owned finding.
Excerpts remain bounded to 30,000 characters per body and 60,000 total; reviewers
must report unreadable or missing evidence and omit unsupported claims. All packet
content remains untrusted evidence.

A response may be a JSON document or one explicitly labeled JSON code block
with surrounding harness prose. Multiple blocks, malformed reports and invalid
pinned quotes remain rejected. Before launch, new work promises an owned accepted
or rejected validation disposition. A rejected response produces a distinct
rejection finding with the actual attempt and source revision, never an accepted
assessment. That validation pass completes `audit_only`; the successful process
retains its immutable outcome. Rejection replay checks the original transcript
again before it can count toward job-local retries and the three-pass limit.
A rejected attempt hands its validator reason to the next attempt, so a retry
corrects the named defect instead of repeating it blind. The reason is a
deterministic validation message, never a source claim, and it grants no
authority. Legacy work that promised valid hypotheses cannot be completed with a rejection;
it requires a valid response through governed same-work continuation.

Reports contain at most three hypotheses, each with a pinned source quote, named
consumer, relationship, predicted benefit and falsifier. Priority bands are
`park`, `disputed`, `needs-evidence`, `needs-independent-review` and
`needs-experiment`. An empty or duplicate-only audit parks the source without a
challenge call. Independence here means different requested provider routes;
served fallback identity is not attested by these reports. These bands guide review; they are not measured quality scores.
A model's supported verdict remains an opinion. No experiment, skill installation,
memory promotion or adoption runs here.

The driver checks the actual attempt's model, provider, profile revision, harness,
actor, policy and work linkage before recording a finding. Replays reuse completed
reports. A custody interruption preserves verifying work for finalization without
another model call. A finalization hold or missing transcript remains visible while
other observations can progress; malformed responses allow at most three attempts per role and
observation. In-progress work is reported as waiting instead of duplicated. Sources
older than 14 days are excluded. Acquisition and review share one process lock.
The review budget is 450 seconds, with each CLI command capped at 330 seconds.

Scheduling this optional entry point requires the same governed deployment as the
acquisition tick. The scripts install no scheduler. Existing capability review and
experiment owners remain responsible for comparing a baseline, demonstrating
runtime benefit and deciding adoption.

## Verification

```sh
python3 -m unittest discover -s scripts/upstream-learning -p 'test_*.py' -v
bun test tools/upstream_learning.test.ts
```

The Bun test connects these fixtures to the canonical repository test inventory.
Network responses use synthetic hosts and an injected fetcher. One integration
test uses the real CLI with a temporary Compound home to prove intake schema
compatibility. Tests never contact GitHub or mutate the running instance.
