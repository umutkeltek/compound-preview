# Naming: accounts, lanes, harnesses, profiles

Every failure in the 2026-07-27 lane investigation traced to one cause: **a name
that meant two things.** Not a missing feature, not a wrong value — an
overloaded identifier that let a reader be confidently wrong.

Four in one day:

| name | meaning A | meaning B | what it cost |
|---|---|---|---|
| `abandoned` (RunAttempt) | a run that died | a claim placeholder correctly superseded | read as a 70% failure rate; 330 of 400 sampled were benign |
| `running` (WorkEnvelope) | actively executing | finished but never closed | one envelope held a lease 252h past completion |
| `profile` | an account home | a provider wire binding | transcripts reported nonexistent; three audits closed as cancelled |
| `key✓` (status board) | a credential resolves | this lane can authenticate | a lane advertised health through every one of its 401s |

The pattern is always the same: the benign meaning is common, so readers learn
to skim it, and the alarming meaning hides inside. This document fixes the
vocabulary so that class of defect cannot re-enter through a name.

## The four nouns

These are distinct things. They are not interchangeable, and no surface may use
one word for two of them.

**Account** — a credentialed identity with durable state. Has a home directory
holding history, transcripts, memory, and settings. You can log into it. Two
accounts on the same vendor are genuinely separate identities.

**Lane** — a wire. "Speak protocol P to model M at endpoint E with credential
C." Stateless plumbing. Has no identity, no history, nothing to log into. An
API key with no console is a pure lane.

**Harness** — an executor program (a coding agent CLI, a runner). Defines which
lifecycle hooks can fire and whether skills and MCP are available. A harness is
*capability*, not identity and not plumbing.

**Profile** — a *binding* of the three above: this harness, over this lane,
optionally as this account. A profile is the only one of the four that is
composite, which is exactly why it must never be used as a synonym for any
single component.

> The word `profile` was the overloaded one. It named both an account home and a
> lane binding, they were stored in one directory tree, and the commands that
> listed them disagreed about which existed. If you take one rule from this
> document: **`profile` is always the composite. Never shorthand for a part.**

## Identifier grammar

```
account   <vendor>/<account-name>          two accounts on one vendor differ only here
lane      <vendor>/<model>[@<variant>]     the wire; no account, no harness
harness   <program>                        snake_case; the executor, not the wire
profile   <harness>.<lane>[.<account>]     the composite binding
```

Rules that follow from it:

1. **A profile id names its parts in order and omits nothing that varies.** If
   two profiles differ by account, the account appears in both ids.
2. **Never encode capability or effort in a name.** A lane whose id carried both
   model and effort ended up with a record that disagreed with itself
   (`effort=xhigh` in one field, `ultra` in another). Effort is a *field*.
3. **Never encode cost in a name, and never let cost be inferable only from
   one.** Two lanes differing by a suffix, one metered and one free, is a
   billing accident waiting to happen. `cost_class` is a required field.
4. **No comparative or superlative suffixes** — `-full`, `-max`, `-pro`,
   `-lite`. They imply an ordering the system does not enforce. A `_full`
   profile once had *fewer* capabilities than the unsuffixed one, because the
   suffix described an intended subscription tier while capability came from
   somewhere else entirely.
5. **A container is not a member.** A directory holding profiles is not itself a
   profile. Any surface that enumerates by listing a directory will report the
   container as a member; enumerate from the registry instead.

## Status vocabulary

A status word must name exactly one condition. When a word covers two, split it
— do not add a second field readers must remember to consult.

| forbidden | why | use instead |
|---|---|---|
| `abandoned` for both death and supersession | the benign case is ~80% of instances, so the word gets skimmed | `superseded` and `died` |
| `running` for both live and unclosed | indistinguishable without a lease-age check nothing performed | `executing` and `unclosed` |
| `key✓` for credential presence | reads as "can authenticate" | `cred:env` / `cred:vault` / `cred:none` |
| `ok` from a config-shape check | reads as "works" | name the axis: `config:coherent` |

**Readiness is behavioural or it is not readiness.** A surface may report only
what it actually verified. Reachability (`a port is listening`), resolution
(`config parsed`), and presence (`a credential exists`) are each legitimate to
report — under their own name. None of them may be labelled or coloured as
readiness, because a lane can pass all three and fail every request. Exactly one
surface asserts readiness, and it does so by performing a real request over the
same protocol and auth header the harness will use.

**`unknown` is never `clean`.** A check that cannot locate what it inspects must
say `unknown` and count as unresolved. Reading an unreadable path as an absence
converts a lookup bug into a confident fact — which is how a lane whose settings
registered no hooks was recorded as having all six.

## Locations

**One resolver per question.** "Where does X live" gets exactly one function,
used by every surface. Today's cost came from three answers to one question: a
doctor reported one path, a launcher used another, and a harvester searched a
third. All three were plausible and only one was real.

**Ask the owner; never keep a second copy.** When another system owns a fact,
query the surface it publishes. A hand-maintained copy of someone else's path
drifts exactly as the stale copy it was meant to replace.

## Capability

**Declared capability is a floor, never an aspiration.** A registry entry
describes what the executor has *now*. If a capability requires an installation
step, it is false until that step is performed and verified.

The asymmetry matters and must be preserved in tooling: **over-claiming is
unsafe, under-claiming is merely wasteful.** An under-claim leaves a capability
unused. An over-claim routes work to an executor that cannot do the job and
cannot report that it could not — the failure arrives as silence. Only
over-claims may fail a build; ranking them equally trains operators to ignore
the check, which is how the original mismatch survived.

**A declaration must be falsifiable by something outside itself.** The test
guarding these entries once asserted the registry matched a literal copy of
itself, so it passed while every claim was false. A guard that cannot disagree
with the thing it guards is not a guard.

## Applying this

Renaming is a migration, not an edit. For each rename: add the new name, make
the old one an alias that warns, move readers, then remove the alias. A rename
landed without the alias step breaks every caller that still spells it the old
way — including operator shell history, which is not in any repository.

New names are cheap. Overloaded names cost days.
