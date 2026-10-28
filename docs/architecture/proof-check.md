# Proof-Check and the Land-Lease

`compound proof-check --work <work_id> --actor <lease-holder> --out <report.json>`
runs the canonical deterministic local proof bundle: tests, vet, build, diff
check, strict architecture doctor, memory doctor, and public redact scan. This
page is the contract language for its lease, refusal, red-battery, and recovery
semantics. The README carries only the summary.

## Admission and custody

A full local run is admitted against the existing lease/work owners before its
first subprocess. It needs no interactive terminal: custody of the `--out`
report under the still-held lease is the proof, so any harness or model invoked
as a capability can certify, as can a scheduled queue with no controlling
terminal.

Custom TTLs below 30s are refused as usage before reservation; accepted windows
refresh at `min(TTL/3, 5s)` and continuously monitor holder loss.

Before the first gate, a `go run`/temporary Compound executable is copied into a
private, synced proof-owned directory and every logical `compound` child uses
that pinned candidate. Direct Go gates and the Bun tools gate also share one
sealed-workspace Go build cache outside the repository, so child `go build`
calls cannot race an unrelated host cache cleanup or pollute public redact.
The `internal/work` proof-runtime module owns both custody decisions; the CLI
only resolves the current candidate and dispatches the canonical gate plan.

The cache lives per worktree under `compound-proof-gocache` in the user cache
dir, stamped with `owner-path` and `last-used`. Each non-dry-run proof-check
prunes that root once before its gates. It keeps the current cache and any
cache used in the last 6 hours, removes caches whose owner worktree is gone or
idle for 3 days, then removes the least recently used until 16 remain. A
retired worktree's cache is removed by the next run, so retirement needs no
cleanup step. Shared-cache runs and repos under the temp dir skip pruning; a
prune error prints one `proof-gocache:` line and never fails the proof.

For a sealed source snapshot, public redaction receives each changed file's complete
committed body on stdin and its repo-relative identity through `--source-path`. The path
is an annotation, not an instruction to read the working file. Edits or deletions after
capture cannot change either input. A nonzero scanner exit fails the ordinary gate.

## Exact public-content review

`--redact-review <act_ID>` consumes a previously performed native public review,
not an allowlist. It is valid only for canonical full proof. The existing finding
owner custodies a `compound.public_redaction_review.v1` evidence body: the same
work and complete clean source snapshot, an ordered inventory of every changed
committed body (including clean files), each SHA-256, original scanner JSON and
exit code, and exact finding classifications with supporting references.

The review needs its own `public` ActionIntent with capability
`proof.redaction.review_public`, action `classify_public_redaction_findings`, the
repository fingerprint and path, the finding reference, and `arguments_hash`
equal to its CID. Only a performed action with its exact allowing native
PolicyDecision can be consumed. Local source-edit permission is insufficient;
the consumer neither creates a grant nor approves an action on the operator's
behalf. Existing standing grants or exact action approvals remain the owners.

Only MEDIUM `pii.phone.e164` findings classified as `public_provider_identifier`
and MEDIUM `pii.email` findings classified as `public_support_contact` are
reviewable. HIGH findings, other MEDIUM classes, new or changed findings, source
drift, omitted clean files, malformed reports and missing authority still fail.
Counts must be explicit and agree with findings and the real exit code. Deciding
JSON refuses duplicate keys under the same Unicode folding as its typed decoder.

The unchanged scanner runs on every complete committed input, now requesting
`--json`; no allowlist, self-email, public-email or auto-redaction flags are passed.
It continues through blocking scanner exits before checking exact coverage. Raw
review and fresh scan bytes stay in the owned v0.5 report; a reviewed gate proves
`redact-public-reviewed`, never `redact-public-clean`. The ordered profile digest
seals the action, policy, finding and CID tuple as Compound metadata, not scanner
arguments. Scanner JSON is bounded to 1 MiB per file and retained evidence to
4 MiB. Overflow fails while retaining earlier bounded partial evidence.

Fresh public authority is checked at admission, execution acceptance and before
starting a new green finalizer. Plan reconstruction authenticates the historical
tuple without granting permission, so exact already-sealed release-pending
completion can reach the existing recovery door after an approval expires.
Recovery still requires the original command, source, profile and owned finalizer
chain; it reruns no gates and mints no new certificate. The same expired review
cannot admit a fresh run. Unreviewed v0.1–v0.4 reports remain readable; reviewed
evidence requires v0.5. This proof does not authorize source publication or product
deployment: those remain separate governed actions.

## Exact private-source audience and publication

`--redact-review <act_ID>` also accepts the distinct native capability
`proof.redaction.review_private_source` and action
`classify_private_source_redaction_findings`. The finding body is
`compound.private_source_review.v1`: the exact work, complete clean source
snapshot, original unreviewed profile digest, complete ordered scan inventory,
per-finding classifications and evidence references, and authenticated destination.
This remains a `public` release-class action. Local editing permission and a
WorkEnvelope's privacy label cannot authorize it.

The destination observation binds GitHub's immutable repository ID and name,
authenticated viewer ID/login, actual single configured push URL, canonical
HTTPS URL, remote, target/publication refs, and observed private visibility.
Unknown hosts, credentials in URLs, multiple push URLs, archived repositories,
missing push permission, offline observations and identity/visibility drift fail.
Observation uses the existing authenticated `gh` session; no credentials or raw
account responses enter the evidence.

The full PUBLIC scanner still runs on every complete changed committed body,
including files after a blocking scanner exit. All HIGH findings block. Exact
MEDIUM findings may be classified as private service hostnames/endpoints,
operational IPs, support contacts, nonsecret provider identifiers or nonsecret
operational settings only under their supported per-finding classification.
Exact email and numeric findings also support `private_synthetic_fixture` and
`private_nonsecret_operational_setting` when evidence identifies a synthetic
fixture or nonsecret configuration value. These labels do not apply to other
detector IDs or waive any individual finding, evidence reference, or HIGH block.
This is neither a category waiver nor permission to include credentials. Raw
findings and blocking exit codes remain in `private_source_evidence`; partial or
overflow evidence may explain a failed run but cannot certify a green one.
The gate retains the name `redact-public` and proves
`redact-private-source-reviewed`, never a public-clean certificate. Public and
default review semantics remain unchanged.

`compound work private-source-publish` is the corresponding narrow publisher:

```sh
compound work private-source-publish --work work_example --actor codex:example \
  --repo /clean/source --review-action act_example --json
compound work private-source-publish --apply --request publication.json --json
```

The first form is a read-only concrete proposal. Save its complete JSON request
for review. The apply form requires running work and its own exact one-use
approval, even if a broader standing grant exists. It reopens the owned full
proof/finalizer chain and exact source/profile/review, observes the authenticated
private destination and expected remote tip again, verifies the landed canonical
target ref, and rejects rewritten transport URLs or non-fast-forward ancestry.
The certified base must be an ancestor of the expected remote tip, which must
itself be an ancestor of the certified head; an unscanned pre-base publication
gap requires a new complete certificate rather than implicit trust.
The source workspace must be clean and share the bound repository identity;
unrelated preserved work in the canonical checkout is not staged, scanned as the
publication payload, reset, or checked out.

The adapter owns a fixed non-force `SHA:refs/heads/branch` push to the canonical
HTTPS URL, with existing authenticated GitHub credentials and ordinary hooks.
Automatic tag following and submodule pushes are explicitly disabled, so local
Git configuration cannot add those effects. It changes no global Git/auth
settings. A durable pending native action precedes
the effect. Authenticated GitHub API postflight must confirm the same private
repository and exact target commit before a local tracking-ref CAS and native
receipt can complete. Transport success alone never proves publication.
Pending retries only observe/reconcile; they never repush after a lost response.
Unconfirmed remote state or failed bookkeeping retains the pending action and a
private observation artifact. Visibility/configuration cannot be locked
atomically with a remote push, so preflight/postflight drift remains an explicit
uncertain outcome rather than a claimed rollback. Completed actions replay their
verified native receipt without repeating the effect.

Immutable lightweight release tags have a distinct metadata action:

```sh
compound work private-source-publish --release-tag v1.2.3 \
  --request completed-publication.json --json
compound work private-source-publish --apply --request release-tag.json --json
```

The plan requires the complete original publication request and its verified
native completed Receipt/Transition. It reopens the exact full certificate and
historical source review as evidence, observes the same authenticated PRIVATE
repository, and requires its main target to equal the certified source commit.
Read-only completion verification reads the current WorkEnvelope without claim
recovery; a crashed claim's work, lease and pending marker stay untouched.
Writable native replay retains its existing owned recovery behavior.
Only one numeric `vMAJOR.MINOR.PATCH` tag is supported, absent or already pointing
at that exact commit. The tag needs its own exact one-use release-class approval;
source review or source publication approval cannot authorize this action.
Apply rechecks the private destination, source, published main commit, exact tag,
native reservation and unexpired decision before a fixed single-ref non-force
push. It creates no local tag, follows no extra tags, pushes no submodules, and
never rewrites an existing tag. Successful empty authenticated `ls-remote`
output proves absence; an offline/error response does not. Identical existing
tags require no push. Postflight and pending retries only observe the exact
main/tag state; a lost response never triggers an automatic second push.

## The execution barrier

Expiry alone never makes a successor executable: every proof gate inherits a
kernel execution barrier from the existing land-lease owner, and ordinary
landing acquire, expiry recovery, generic sweep, and exceptional steal all fail
closed while any descendant still holds it. SIGINT/SIGTERM and governed steal
requests cancel and wait for the whole gate process group.

A stale already-dead predecessor is recovered through the stricter exceptional
policy with `--steal --reason ... --terminated-run <ratt_*>`; Compound itself
attests the released barrier and writes the immutable Work-reachable
termination evidence. Bare/reused PIDs and caller-authored JSON never authorize
takeover. Ambiguous or cyclic recovery lineages still fail closed.

## Certification and the red battery

Every reserved `canonical_full` gate must finish `ok` to certify; a skipped
gate cannot certify. A complete canonical red battery — every gate terminal
`ok|failed`, with at least one `failed` — is retained as non-certifying
governed evidence, releases the scarce resource, prints one truthful
`battery: FAILED` verdict, and exits 1 so it does not wedge a successor.

## Report durability

Refusal leaves an existing report byte-identical and a nonexistent report
absent, while successful publication uses a synced sibling temp plus atomic
rename. Any failure after a complete report is durable prints exactly one final
non-green battery verdict; pre-report usage/admission errors print none. A
post-release installed-runtime publication failure is therefore non-green, and
that marker is also published by synced atomic rename with a completion footer
so an interrupted prefix cannot suppress Workbench action.

## Release-pending recovery

A committed green or red result whose lease release remains pending returns a
non-green command error and retains its exact retry journal until recovery;
its report stays intact and stdout ends `battery: FAILED release_pending=true`.
Invoking that exact command again resumes the owned result before reservation,
atomically restores and verifies `--out` from the immutable owned artifact
before release, returns the prior report, and releases without rerunning the
gates or minting another holder.

## Workbench publication

The installed-runtime Workbench artifact is published only after a green
governed completion has released; release-pending, invalid, failed, dry-run,
and CI reports cannot suppress that action. Dry-run and CI subsets remain
lightweight and non-certifying.
