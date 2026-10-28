# Experimental content transport v0.2

`compound act post --draft` consumes a reviewed package through the existing
product candidate, artifact store, policy, and actuator owners. It remains gated
by `COMPOUND_EXPERIMENTAL_DISTRIBUTION=1`. Synthetic acceptance does not enroll an
account or accept a provider for public use.

## Review and ownership

The content queue displays exact text, ordered media references, MIME types, and
alt text. Approval requires the displayed `package_digest`; the digest covers
product, platform, body artifact, ordered media metadata, and expiry. Artifact
bytes are checked against their content addresses before approval and send.

The winning candidate generation owns a frozen approval witness with the review
ID and exact ActionIntent/PolicyDecision. Reapplying the **original** review
finishes interrupted authorization recording with those same identities, even
after expiry; expiry still forbids a send. A different review or package cannot
consume that witness. Before reservation, rejection withdraws approval while
retaining its evidence. Generic evidence references confer no approval authority.

A confirmed send stores the exact wire request and policy records, then reserves
one outbound attempt with candidate CAS **before** starting the executable. The
request binds ordered media bytes and alt text, account/integration, operation,
package, transport, schedule, and idempotency key. The attempt ID is the
idempotency key. A pre-reservation persistence failure reuses the exactly
matching original ActionIntent timestamp and evaluates policy afresh. Concurrent submissions and retries with a different schedule
cannot acquire a second attempt on that draft.

## Enrolled adapter contract

An enrolled `actuator/transports/NAME.json` must explicitly declare
`"content_contract": "compound.content_transport.v0.2"` alongside its validated
`argv` and optional `timeout_s`. Both the exact config bytes and resolved
executable bytes are hashed into the request binding and checked immediately
before start. Package, expiry, account identity, reservation, and current policy
are also rechecked before a create.

The executable reads one JSON request on stdin and returns one complete JSON
observation on stdout. Stderr is diagnostic. It implements exactly two operations:

- `type: schedule`: create or return the same operation for the supplied
  idempotency key. The adapter must enforce that key and the reviewed
  `expires_at` at its external boundary; Compound refuses a requested schedule
  at or beyond that expiry.
- `type: observe`: read the outcome of that **original** attempt, without creating,
  rescheduling, or publishing anything. It receives the same request with only
  `type` changed. This is an explicit enrolled protocol, not a generic command
  relabeled as a read.

Observations use `compound.content_transport_observation.v0.2` and repeat the
operation, attempt/idempotency IDs, original schedule-request digest, package
digest, transport, account, integration, and full executable/config binding.
The request digest is `sha256:` plus the hex SHA-256 of the original schedule JSON
bytes. An adapter must retain that digest for later observation, rather than
hashing its own differently serialized object.

| Status | Required observation | Local result |
| --- | --- | --- |
| `scheduled` | `object_type: schedule`, schedule ID, exact requested time, no remote post ID | scheduled |
| `processing` | attempt or schedule object | processing; reconcile |
| `published` | publication object, remote post ID, observed publication timestamp no later than response observation time | posted |
| `rejected`, `cancelled`, `error` | attempt or schedule object; definitive terminal failure with no publication | failed |
| `unknown` | attempt or schedule object | reconciliation required |

When an accepted schedule exists, observations must retain its exact identity.
A request-bound `published` observation may omit `schedule_id` only when its
`remote_id` exactly equals the stored schedule ID: some providers publish the
same remote object they previously queued. A different publication ID still
requires the explicit matching schedule ID. Equality alone is not publication:
the positive publication object, observed nonfuture timestamp, and complete
original request/account/transport binding remain mandatory. Unknown effects
cannot use this exception. The stored schedule, original request and receipt
remain unchanged; the provider observation is retained verbatim without filling
in an omitted field, and the original scheduling response artifact is preserved.

A failure whose external effect is uncertain must be `unknown`, not `error`.
A known publication-only adapter may normalize its endpoint's bare ID only after
it has positive publication evidence and can bind the answer to this request.
An untyped scheduling ID, approximate date, successful exit, partial output, or
retained recovery artifact cannot prove publication. Unknown fields, malformed
JSON, trailing bytes, and mismatched bindings are refused.

## Recovery and measurement

The attempt remains owned after a crash, response loss, or local recording
failure. There is no automatic compensation or resend. The queue exposes:

```sh
compound act post --draft D --transport NAME --reconcile --json
```

This issues a fresh complete `observe`, then CASes its bound outcome. A newer
candidate generation is preserved on conflict. Terminal publication/failure
replay makes no additional external call. Legacy manual adjudication and result
doors cannot overwrite an owned outbound attempt. No automatic cancellation,
replacement attempt, or transport-migration door is provided.

The published draft retains the outbound request and observation artifact refs
and actuator receipt. These references can ground the existing product
hypothesis → signal → review loop. An operator still supplies the expected-effect
sentence and measurement verdict; publication is not evidence of causal lift.
The synthetic CLI acceptance continues one exact media package through a fresh
process observation, a PostHog-shaped signal, review, and closed-loop projection.

## Support boundary

Live adapter enrollment, account authority, provider idempotency/observation
behavior, media limits, and publication evidence require separate operator
acceptance. Legacy v0.1 authorizations require a fresh package review. The v0.2
path deliberately refuses legacy identity-only transport configuration; it does
not silently migrate it or make a real post during verification.
