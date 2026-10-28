# Required native consumer proof

External Go, Xcode, Bun, Node/npm, and Python/uv repositories can declare a tracked root compound-proof.json.
Canonical admission reads that regular Git blob from the sealed head, never from
ambient configuration or a caller replacement command. Compound and Rust retain
their existing inventories; a consumer declaration cannot replace them. Module
identity checks consume the whole declaration: trailing comments are allowed,
but a quoted prefix followed by extra tokens is not a Compound module identity.

The sealed declaration must contain 1..131072 bytes. Parsing and dispatch share
this 128-KiB bound, allowing exhaustive required-test identities without trimming
their inventory. Oversize declarations fail closed. The 32-consumer, command
timeout, native-report and aggregate-evidence limits remain unchanged.

```json
{
  "schema_version": "compound.proof_consumers.v1",
  "profile": "go_consumers_v1",
  "consumers": [{
    "id": "buyer-path",
    "command": ["make", "test-buyer-path"],
    "dir": ".",
    "timeout_seconds": 240,
    "required_tools": ["make", "go"],
    "required_environment": ["GOCACHE", "GOMODCACHE"],
    "report_kind": "go_test_json",
    "required_tests": ["example.invalid/product/integration::TestBuyerPath"],
    "allowed_skips": []
  }]
}
```

Every declared consumer is required. Its command, relative directory, and exact
contract-byte digest participate in the existing ordered resource-profile digest.
For a nested Node declaration, its clean repository-relative location is also
bound into that digest. Historical root declaration and serialized native-evidence
identities are unchanged.
The inventory also includes committed diff and public-redaction gates. Generic
unit suites or a success sentence cannot substitute for a declared consumer.
Only the exact Compound profile can publish an installed-Compound-runtime artifact.

The source-owned command must write fresh native evidence to the runtime-supplied
COMPOUND_PROOF_REPORT path. For Go this is the complete go test -json NDJSON
stream, with package start/pass and run/pass for every required exact package::test
identifier. Duplicate, out-of-order, failed, omitted, or unfinished tests/packages
fail. Every skip must be explicitly listed and must not be a required test.
Commands must disable test caching (for example, -count=1) and select their
actual production integration seam.
Native cached-package output is refused. JSON keys duplicated by case variation
are refused too, because the typed decoder would otherwise collapse them.

For xcode_consumers_v1, use report_kind xcresult and write a fresh .xcresult
directory at the supplied path (pass it to xcodebuild -resultBundlePath in a
tracked wrapper). Compound queries both xcresulttool get test-results summary
and tests via xcrun. Required tests are exact native nodeIdentifier values, such
as ActivationTests/testFirstAppearance(). Summary counts and individual leaves
must agree. Failed or expected-failure results, duplicate leaves, absent required
tests, unexpected skips, and summary-only green all fail. An explicitly allowed
physical-device skip does not prove device behavior.
Its native flat Failure Message child is retained as diagnostic structure, never
counted as an additional test; nested cases or hidden result fields are refused.

Passed Test Case nodes may carry flat, passed Arguments children from native parameterized tests.
Each argument requires a nonblank name unique within that parent, with no node identifier,
children, or nested test nodes. Merged native reports may also place one Device layer directly
under a Passed Test Case. Each Device must be Passed, have a nonblank name and nodeIdentifier,
match a unique root devices[].deviceId, and be unique within its logical parent. Root device
metadata is required only when Device diagnostics occur; missing, blank, duplicate, or unknown
IDs fail. A Device has no nested test nodes and may contain only flat Passed Arguments with
names unique within that Device. Device IDs and argument names may recur under separate parents.
Devices and Arguments are diagnostic expansions: each logical Test Case still counts once,
and summary counts and required test identities must agree. Orphan, nested, failed, skipped,
unnamed, duplicate, or unsupported diagnostics are rejected. Missing required tests still fail
after valid Device diagnostics are consumed. No expected argument cardinality is inferred.

Xcode consumers also require xcodebuild and xcrun even if omitted from required_tools.
Before the source-owned command launches, a read-only preflight uses the same
explicit child environment, including DEVELOPER_DIR only when declared. It checks
that xcrun resolves executable xcodebuild and xcresulttool paths, that xcodebuild
reports a full Xcode version/build, and that both native test-results queries are
supported. Successful stderr diagnostics are kept separate from the stdout
identities being parsed; nonzero query exits still refuse. All probes share a
15-second budget within the consumer deadline.
An unavailable selection or unsupported report interface refuses product execution;
Compound does not select, install, initialize, or repair the toolchain. This is a
current capability check, not executable-byte attestation or a pinned-version claim.
The repository-owned recipe still owns schemes, exact simulator destinations and
fresh DerivedData. Preflight does not boot, reserve, or certify simulator readiness.

The existing sealed checkout, reservation, RunAttempt, execution barrier, report
custody, finalizer, and land-check remain the only certification path. Native
report bytes and exact contract bytes are retained inside the content-addressed
canonical report, not as a mutable sidecar path. The owned-report decoder and
finalizer revalidate them before certification and during later chain reads.
A failed run can retain partial native bytes without becoming successful proof.
Native evidence has a 32 MiB aggregate retention budget across all consumers.
Excess evidence fails that gate; its oversized bytes are not added to the owned
report. Earlier bounded evidence remains. The finalizer enforces the same limit.
Execution, custody, and validation belong to the existing work module; the CLI
supplies only its current executable resolver and barrier-aware command adapter.

Prerequisites fail before product execution; Compound installs nothing. Required
environment inputs are restricted to existing absolute tool/cache directories:
PG_BIN_DIR, GOCACHE, GOMODCACHE, and DEVELOPER_DIR. PostgreSQL additionally
requires executable initdb and pg_ctl. The child gets an explicit environment:
absolute PATH entries, user HOME for native tool access, private temporary/cache
defaults, locale, declared directories, and fixed offline Go controls. Ambient
DSNs, provider keys, GOFLAGS, and Go config do not pass through. A caller's
report-path environment value is ignored.

This is provenance for reviewed repository-owned commands, not a sandbox for
hostile code: children still run as the current OS user. A script that fabricates
its native report must be caught by source review and consumer falsifiers.
Offline Go settings are not a general network firewall. Contract/report sizes,
JSON nesting, duplicate keys, cwd containment, report-file type, and deadlines
are checked. A timeout cancels the existing process group and retains available
partial evidence; it never silently falls back to another suite.
Before report capture, an additional inherited EOF fence waits for background
writers as well as the direct command. It does not replace the reservation's
execution-barrier descriptor. Tools remain resolved through the operator's PATH
and existing runtime resolver; this contract does not attest external executable
bytes or authorize a different tool trust policy.
Both inherited-descriptor fences require cooperative descendants. Deliberately
closing those descriptors and continuing to mutate is outside this reviewed-code
trust boundary, just like fabricating a native report in the source-owned script.

Adding the contract is not release proof. Required product commands must actually
consume it through a successful governed canonical run on their sealed candidate,
and their delivery must consume that same certificate.

Before expensive canonical certification, run `redact --repo-visibility public
--json --from-file <changed-file>` against every complete changed source body.
Token-level and architecture-leak checks are complementary preflights, not
public-redaction evidence.

An exact public-content classification can be consumed through
`proof-check --redact-review <act_ID>` without changing the scanner. Its native
finding, performed public ActionIntent, allowing PolicyDecision, source inventory
and raw scan evidence must match the canonical profile. Only the two named MEDIUM
public provider-ID/support-contact classes are supported; this is not local-edit
permission, a value-wide exception or deployment approval. Admission and new green
finalization recheck current authority; exact sealed-result recovery remains
historical and never re-enters execution. The full owner contract and bounds live
in [Proof-Check](../proof-check.md#exact-public-content-review). Reviewed evidence
uses owned report v0.5; unreviewed historical reports keep their existing meaning.

## Bun native JUnit profile

bun_consumers_v1 requires tracked root package.json, bun.lock, and
compound-proof.json with report_kind bun_junit. source-owned command writes Bun
1.4 native JUnit XML directly to COMPOUND_PROOF_REPORT; a Go-shaped translation
does not certify. Required and allowed-skip identities use canonical compact JSON
[file,classname,name,[suite ancestry...]], for example
["tests/example.test.ts","","does the thing",["example.test.ts","feature"]].
empty native classname is valid significant. suite ancestry is ordered outer
file suite through every nested describe suite. Duplicate identities, missing or
skipped required tests, undeclared skips, failures/errors, inconsistent or absent
native summary counts, malformed/trailing XML, namespaces, wrappers, unknown
attributes, zero-leaf suites, ancestry deeper than 32 suites, and empty reports
fail closed.

BUN_INSTALL_CACHE_DIR is an allowed required-environment directory. Bun consumers
receive isolated private HOME plus explicit PATH, TMPDIR, locale, report path, and
declared cache directory; ambient npm configuration provider credentials are not
inherited. Bun 1.4 has no bun install --offline contract. When hydration is
required, reviewed repository-owned command may run bun install --frozen-lockfile
with explicitly declared cache and ordinary network semantics. Compound validates
prerequisites custody but does not install dependencies itself.

## Node/npm native JUnit profile

`node_npm_consumers_v1` uses `report_kind: node_test_junit`. Every consumer directory
must contain tracked regular `package.json` and `package-lock.json` blobs. Consumer
directories remain repository-root-relative, including when the declaration is
nested. A caller cannot select a smaller declaration with an execution flag.

Existing root declarations and Go/Compound, Cargo, and Bun/package root markers
retain their precedence. Only a repository without those root markers may use one
unambiguous tracked nested `compound-proof.json`; nested declarations are supported
only for this Node/npm profile. Multiple candidates, symlinks, trees, submodules,
and unclean paths refuse. Discovery includes tree entries and has a 16 MiB output
limit. Noncanonical development/CI modes refuse a nested consumer declaration just
as they refuse a root declaration. They cannot lend Compound's inventory to it.

Admission reads the exact declaration blob from the captured Git head. A nested
gate seals a domain-separated hash of its location and exact bytes. The runner
resolves the same source-owned location; owned evidence retains `contract_path`
and the finalizer checks the same identity. Relocating identical declaration bytes
does not preserve admitted proof. The existing ordered gates still include every
declared consumer, diff-check, and public redaction.

The native report shape is tested against Node 26.9.0's built-in `node:test`
`junit` reporter. Other Node versions are not certified by this claim; incompatible
native output fails closed. Repository-owned wrappers should pin their supported
runtime. Run the actual tests with `node --test --test-reporter=junit` and write
`--test-reporter-destination` directly to `COMPOUND_PROOF_REPORT`. Register existing
assertions through `node:test`; converting console PASS lines into XML is not proof.
Named `describe` suites are required so each accepted leaf has native suite counts.

Required and allowed-skip identities use canonical compact JSON
`[repository-relative-file,classname,name,[suite ancestry...]]`. Native `classname`
is significant (Node 26 uses named suite ancestry). Unlike Bun, Node's root has no
aggregate count attributes, suite counts refer to immediate non-comment children,
and native diagnostic/log comments are permitted within bounded reports. The
validator checks these actual semantics. Failures, TODOs (even when listed as an
allowed skip), absent/duplicate/required-skipped tests, undeclared skips, inconsistent
counts, namespaces, unknown attributes, malformed/trailing XML, and empty reports
refuse. The portable fixture was captured from genuine Node 26.9 execution; only
root-path, hostname, timestamp, and duration metadata was normalized. Its assertions,
outcomes, structure, and immediate-child counts were not fabricated. Local native
execution tests are optional when the supported Node major is unavailable; portable
fixture and adversarial tests still run.

The runner records its canonical sealed `source_root` alongside unchanged native
XML. Each native absolute file must be a clean contained path under that root;
identity uses the exact relative path, never a basename or prefix-only comparison.
This metadata is runner-owned custody, not an environment variable or XML-supplied
root. It is retained within the owned report and revalidated by the finalizer; the
transient sealed checkout need not still exist for later chain reads. The reviewed
source trust boundary still applies: this is not a signature by Node or protection
against a hostile source command fabricating its report.

Node consumers get private HOME, explicit empty user/global npm config files,
a private writable npm cache, and fixed offline/ignore-scripts/no-audit/no-fund/
zero-fetch-retry npm defaults. Ambient NODE_OPTIONS, npm configuration, credentials,
and provider keys do not pass through. A tracked project config remains source-owned
and must be reviewed with the command. `NPM_CACHE_SEED_DIR` is an optional declared
absolute cache-directory input restricted to this profile. Compound does not install
or copy dependencies: a reviewed source-owned wrapper may seed its private cache
using only lock-integrity-verified artifacts and run `npm ci --offline --ignore-scripts
--include=dev --include=optional --no-audit --no-fund`. Keep platform-native optional
dependencies such as the matching esbuild package; omitting all optional packages
is not equivalent. Canonicalize a declared seed root once before containment checks
so an operator-declared symlinked cache is not confused with a child tarball escape.
Never copy ambient npm configuration or credentials, and never fetch a substitute.

These npm controls are not a general network or filesystem sandbox. A stronger
containment claim requires separate installed acceptance. Hydration feasibility,
native parser tests, and changed capability selection are not customer outcomes or
measured economic benefit. The product's actual governed canonical run and delivery
must still consume the certificate before a changed-work close is accepted.

## Python/uv native pytest profile

`python_uv_consumers_v1` requires tracked regular root `pyproject.toml`,
`uv.lock` and `compound-proof.json`; declarations remain root-only.
Each consumer declares `uv` as a required tool and `report_kind: pytest_junit`.
The reviewed source-owned wrapper runs genuine pytest, with `--runxfail` and
`-o junit_family=xunit1`, writing directly to `COMPOUND_PROOF_REPORT`.
Required and allowed-skip identities are exact compact JSON
`[repository-relative-file,classname,name]`. Parametrized names are significant.

Admission and finalization reject missing/duplicate identities, required or
undeclared skips, failures/errors, xfail outcomes, missing or inconsistent native
summary counts, absent xunit1 files, absolute/escaping paths, namespaces, extra
roots, malformed/trailing XML and hidden/nested outcomes. Bounded native
system-out/system-err text is diagnostic only. A summary-only or empty report
cannot certify. Custom record-property elements are not accepted by this profile.

Python consumers receive private HOME and a disposable project environment,
fixed frozen/offline/no-config/no-Python-download uv controls, disabled user-site
imports and fixed pytest options. Ambient provider keys, DSNs, PYTHONPATH,
PYTHONHOME, pytest options and uv configuration are not inherited.
`UV_CACHE_DIR` may be a declared existing absolute dependency-cache directory;
otherwise it is private and empty. Lock-consistent dependency hydration remains
source-wrapper-owned and must work offline; Compound installs nothing itself.
A PostgreSQL consumer declares PG_BIN_DIR and owns its disposable database,
never an ambient production DSN. These controls are not a hostile-code sandbox
or an attestation of pytest executable bytes. Existing source-review, sealed
checkout, drift, deadline, process-group/EOF, custody and 32-MiB fences apply.
Adding support is not product certification or deployment acceptance.

## Source action admission

Declared source effects require complete Git delivery authority before performed
recording or fresh receipted-action reservation. Admission compares repository
fingerprints, so an execution linked worktree may differ from the landing checkout.
Operator-only same-state legacy binding and final landed-proof gates are unchanged.

Covered declarations are `source.edit`, repository-targeted `build-implement`, and
`file.write` against Git-contained files/directories. The incident's `file.write`
+ `directory` + repository-root shape is included. Read actions, explicit private
runtime/worktree-retirement/package targets, and ignored runtime destinations
without tracked source descendants retain existing authority/evidence rules. Git cwd alone, free-form action names,
generic shell commands and harness prompts do not declare source purpose; this
gate does not claim to classify their contents.

Public repository-target actions and explicit `git-amplify-deployment` or
`shell.git_push_existing_remote` declarations additionally require a clean
committed candidate with a supported native canonical proof profile. This checks
the real profile declaration, not test success: canonical proof, publication,
land-check and closure remain separate requirements. Local source edits may enroll
the profile before publication. Already admitted receipted transactions retain
exact reservation/replay recovery semantics.

The exact native redaction-review capability/action pair
`proof.redaction.review_public` / `classify_public_redaction_findings` classifies
findings; its public mutation class expresses authority rather than source
publication. Source admission therefore does not add a native profile prerequisite
to this preparation step. Existing native review, exact approval, source binding
and proof-consumption checks still apply; the capability alone does not exempt a
different public action.

## Reviewed redaction and native capacity

Native-consumer capacity reconstruction uses the same exact private redaction
gate binder as canonical proof planning. Public and private review suffixes keep
the consumer commands and capacity class unchanged. Admission still compares the
entire sealed source and ordered gate inventory. Capacity classification grants
no review ownership: the native Store independently authenticates the current
work, actor, policy, finding and source before reserving proof resources.
