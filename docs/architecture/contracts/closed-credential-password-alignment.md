# Closed PostgreSQL password alignment

This separate owner repairs exactly two role passwords from an unchanged,
already protected credential file. It is implemented in `internal/policy` and
`internal/act`, and consumed by the native configuration installer. It does not
add a mutation mode to the existing credential verifier or a generic env writer.

## Closed operations and authority

`compound act credential-password-align` exposes only `plan --request-file`
and `execute --plan-ref`. The metadata request selects one finite operation:

| Operation | Capability | Result |
| --- | --- | --- |
| `inspect` | `credential.existing_closed_postgres.inspect_password_metadata` | Fresh typed role/catalog metadata; credential body unopened |
| `align` | `credential.existing_closed_postgres.align_passwords` | Both passwords changed in one transaction |
| `reconcile` | `credential.existing_closed_postgres.recover_passwords` | Exact recorded old/old or new/new state observed |
| `restore` | `credential.existing_closed_postgres.recover_passwords` | Both original SCRAM verifiers restored in one transaction |

Every operation requires a fresh exact one-use native approval of at most fifteen
minutes, current running Work and lease, current policy, clear autonomy halt,
and exact admission through OS start. A general approval is not a password
operation grant. Custom, duplicated or independent forbidden-path floors remain
denying, including SSH custody, Docker and recovery custody paths. No request
accepts a credential value, SQL, command, callback, export path or transport.

The immutable original `app_existing` verifier failure must have actual consumed
approval, committed native launch, failed RunAttempt and matching native
Receipt/Transition. A diagnostic finding is not role-revision authority. The
qualified source capture and frozen profile are read as metadata authority only;
the private capture body is never opened by this owner. The original verifier
program remains a separate consumer and its digest is not changed here.

Inspection walks every credential ancestor with no-follow directory descriptors
and takes an anchored metadata-only stat. It does not open/read the credential.
It supplies role OIDs, xmin/ctid revisions, database OID and one opaque canonical
commitment to the complete private catalog, including its two SCRAM verifiers.
No verifier or private catalog row crosses the boundary. PostgreSQL's `oid`
JSON representation is explicitly converted
to numeric pins. A subsequent operation consumes the actual inspection receipt,
not a supplied catalog assertion.

## Transaction and custody

All operations retain connection limit zero, denied application/PUBLIC CONNECT,
zero sessions for either role or the target database, exact host/boot/cluster,
executable, protected-file and catalog pins. The API hostname in the qualified
frozen target profile determines one prefix and two closed topology shapes.
Legacy profiles may select a subset of api, worker, provider-sync, web and
cloudflared, with api required; all five must still be absent. A commercial
profile must select exactly auth, api, worker, billing, billing-reconcile,
provider-sync and web. Its inactivity check covers all eight names in the union
of both shapes, including the legacy cloudflared name and stopped containers.
Unknown, duplicate, extra or incomplete commercial rosters refuse locally and
remotely. This does not change the source capture protocol's five-role grammar
or let a caller trim the frozen target profile.

The root-owned native Python program uses a genuine duplex `psql` client:
SQL travels on stdin, passwords go to native libpq's controlling-terminal
prompts with echo disabled. libpq creates SCRAM verifiers. The program never
implements password hashing or interpolates plaintext passwords into SQL.
Restoration accepts only validated SCRAM syntax from its own private preimage.
One transaction locks the relevant catalogs, changes the two PASSWORD fields
and checks every non-password attribute, membership, database/ACL/config and
fence. Independent preload audit hooks refuse. Session-local PostgreSQL
statement/error logging is suppressed before credential-bearing SQL.

The fixed sibling namespace `.compound-credential-password-alignment` retains
one original native action directory. Root-owned 0700 directories and 0600,
single-link, no-follow files contain PREIMAGE, POSTIMAGE and COMMIT_INTENT.
Old and new private catalog/verifier custody is fully fsynced before COMMIT.
Each recovery has its own `recovery_<native-action>` journal. Its restored
postimage records the new catalog row revisions: restored password equality
alone does not make it the original old row image. Missing, mixed, drifted or
tampered custody refuses; no guessed rollback or blind retry occurs.
Recovery binds the original PREIMAGE request digest to the immutable native
origin request and verifies its complete catalog against that inspection
commitment before any restore. Swapping valid SCRAM values or rewriting the
private record's self-digest cannot authorize different old passwords.

Only a safe RESULT observation crosses the program boundary. It contains no
password, SCRAM verifier, DSN, SQL, stderr or private catalog row. Configuration,
credential qualification, application, source-capture and migration acceptance
remain false. After alignment, the unchanged verifier must separately perform
its four actual correct/wrong maintenance-database authentications.

A launched failure may emit one bounded, fixed stage marker. The native
transport accepts it only with a failed exit, empty stdout, and exactly one
allowlisted marker line. It records `failure_stage` in the failed status
artifact, never in an accepted observation. Inspection distinguishes runtime,
negative authentication, session, catalog read/shape/SCRAM, closed-state,
base-catalog and postcheck failures. Cleanup overrides other stages. Mutation
operations retain the generic `password_operation` stage and the existing
unknown-effect/recovery rules. No exception text, private catalog, verifier,
credential, SQL or raw stderr becomes metadata. The original verifier program
and its four-authentication requirement remain unchanged.

## Interrupted operations and installer consumption

Before OS start, native admission records an immutable
`compound.credential_password_alignment_fence.v0.1` event under `cpaf_` in the
existing eventstore. The event links the exact consumed decision, original
action, complete metadata request and actual RunAttempt. The launch guard reads
its own immutable bytes without recursively taking its RunAttempt lock.
Scoped scans open only these named attempts, avoiding unrelated corrupt or
historical RunAttempt files. This event is launch intent, never outcome truth.

Any prepared/committed or uncertain attempt blocks a new alignment and the
installer. A terminal native pre-start refusal is known to have no remote effect.
Recovery uses a fixed Work ID derived from the original action and attempt.
Its immutable native admission predecessor chain determines the last recovery; successful
authentication and clock ordering cannot erase a later failed recovery.
After a failed recovery, restoration requires a separately approved successful
reconciliation. Failed attempts retain their failed status and original proof.

Native Work reservations retain required-attempt/recovery obligations. Local
publication failures can resume only the same sealed proof; they cannot launch
again or reuse approval. A completed recovery clears the original blocker only
through the existing Work proof owner. Repeating a completed plan returns its
existing immutable result without another OS start.

Installer admission and execution require genuine unchanged-verifier evidence
newer than the selected completed repair/recovery proof. Repair/recovery and the native installer hold the same no-follow namespace
flock for their complete mutation lifetime. The native installer also checks
the remote namespace on each filesystem guard. It opens only safe
RESULT bodies, validates private journal metadata without opening those bodies,
and requires exact native-derived observation digests. A later or unresolved
remote recovery directory blocks installation even if local history is absent.
The selected recovery RESULT must cover every preceding recovery directory.

## Bounded history discovery

Every native discovery check uses the event store's exact fixed-prefix
`EnumerateIDs("cpaf_", false)` inventory. Its complete sealed-frame inventory
proof permits skipping unrelated payloads; missing, changed or incomplete
index evidence triggers strict validation against stored frames. Returned IDs
are discovery hints, never authority for a fence or its outcome.

The owner retains only discovered IDs within the process and unions each exact
inventory with those known IDs. Every admission and launch check still rereads
and validates every known native fence, attempt, approval and applicable outcome.
A missing or unreadable known record fails closed. Newly appended malformed
fences remain visible. Compatibility scan adapters retain their incremental
cursor and conservative rescan behavior. No ID set, cursor, cached request or
cached outcome is accepted from a request, plan or CLI.

The installer and native transport share the same known-ID set. Exact ID
enumeration avoids decoding unrelated history even in a new CLI process, so the
first discovery fits the existing admission budget. This changes neither native
deadlines nor exact approval, recovery ordering, remote namespace checks or proof
requirements.

## Disposable acceptance

Default Go tests cover exact admission, owner proof, stale authority, native
attempt fences, failed-recovery ordering and local finalization. The Python
suite supplies synthetic format tests and opt-in actual Linux/PG17 tests.

The full native Go-owner path is opt-in for an explicitly selected disposable
PG17 container, with a running `work_` ownership label, no published ports,
Python 3, sudo, the native psql client, and a root-custodied real Docker client
with list access to its fixture daemon. No production request can select this
test transport. For example:

```sh
COMPOUND_ALIGNMENT_NATIVE_CONTAINER=owned-fixture \
COMPOUND_ALIGNMENT_ALLOW_FIXTURE=1 \
go test ./internal/act -run TestCredentialPasswordAlignment -count=1
```

The native tests exercise actual original authentication failure, metadata
inspection, repair, unchanged-verifier success, commit acknowledgement loss,
separate reconciliation, restore acknowledgement loss and restored-row recovery.
Production installation, real credential alignment and migration acceptance
still require their own delivered owners, exact grants and actual receipts.

The native inventory also requires the exact commercial-seven profile to finish
the genuine owner, unchanged verifier and inactive installer sequence. Real
stopped auth and legacy tunnel fixtures must refuse through that same owner;
the Python consumer checks every name in the eight-name inactivity union.

Canonical full proof additionally requires `credential-password-alignment-native`.
It runs the closed `scripts/testbattery` mode against the exact sealed source
head, checking clean source before and after execution. Missing Docker or the
exact prerequisite image fails this gate; there is no skip, pull or package
installation fallback. The gate uses only the local Docker Unix socket, creates
a unique labelled container with no network or published ports, and removes only
that exact container and its anonymous data volume after rechecking ownership.

The prerequisite is immutable image
`sha256:e9851137192b495dcc67f03d032446a0bfeca905351b4065ba214173dac9a5fa`.
It is a frozen synthetic test dependency, not a reproducible source build. It
contains PostgreSQL 17.11, Python 3.13.5, sudo and the root-owned Docker 28 CLI.
Its mounted PostgreSQL data and Docker socket were excluded from the freeze.
The test suite copies all current program and fixture bodies from sealed HEAD
before every native consumer; stale program bytes in the dependency layer cannot
select execution. This host prerequisite is explicitly classified outside the
fresh-clone CI subset; it remains mandatory for canonical preland and postland
proof, alongside every existing gate.

The fixed native inventory covers repair followed by unchanged verification and
four-file installation, actual Linux/PG17 primitives, both COMMIT acknowledgement
loss paths and their separate recovery, plus valid-SCRAM OLD2 tampering with and
without rewritten private self-digests. The mode requires fresh Go JSON run/pass
events for every case, terminal package success and zero skips. Missing,
unfinished, failed, duplicate, cached or oversized evidence fails the gate.

The same required inventory includes actual SIGKILL of the genuine psql process
tree and native helper before PREIMAGE, after its fsync, after the first ALTER,
after the second ALTER, and after POSTIMAGE fsync before COMMIT_INTENT. Test-only
transport hooks record safe stage/change-count/process-death evidence; production
helper interfaces remain unchanged. Fresh catalog inspection proves both live
roles retain their exact original verifiers and revisions after client death.
Durable original custody reconciles old/old through a separately approved owner;
missing PREIMAGE conservatively holds. The explicit-close primitive test proves
graceful ROLLBACK only and is not counted as abrupt client-death evidence.

## Exact negative-authentication responses

A wrong-password probe must exit 2, emit no stdout, and return either the exact
role-specific psql password-rejection line or that line followed by the same
connection rejection without the psql prefix. The second form is libpq's TLS
fallback response. Extra lines, another role, generic errors, successful exits
and any stdout still refuse. TLS and PostgreSQL configuration remain unchanged.
The corrected verifier retains actual positive and negative authentications.
Earlier failed plans and verifier source remain historical evidence; a new
alignment request must bind a fresh failure from the currently installed verifier.
