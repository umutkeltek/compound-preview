# Compound Domain Glossary

This glossary defines the words an operator should see while using Compound.
It is not an implementation spec.

## Core Terms

**Compound**
The local information kernel that turns evidence into current, contextual
knowledge and work.

**Task**
A unit of work Compound can prepare, assign, run, check, or close.

**Continue**
Move the next safe task step forward. Continue may prepare a suggested task,
start ready work, move assigned work into progress, or finish checked work.
It does not run arbitrary task commands.

**Needs me**
A task, source, or decision that genuinely requires operator judgment, private
input, approval, or repair before Compound should continue.

**Evidence**
Immutable, citable raw material. Evidence can support a claim, but it is not
truth by itself.

**Claim**
The smallest durable statement Compound can reason about. A claim can be a
candidate, promoted, superseded, retired, or rejected.

**Position**
The current stance for an entity or topic, derived from claims. Current truth is
a position, not the newest note or highest-ranked search result.

**Source**
The original place information came from: a file, transcript, metric export,
repo, packet, hook event, or external system.

**Proof**
The recorded evidence that a task changed state or produced a result.

**Memory**
Evidence-backed claims and positions that can be recalled later. Stored does not
mean true; it means Compound has a claim with provenance.

**Custody**
The ability to recover an exact durable information body without depending on
the place it originally came from. Custody preserves bytes and provenance; it
does not make the body true or relevant.

**Projection**
A rebuildable form of owned information for a person or client, such as a note,
dashboard, settings screen, harness file, or timeline. A projection can be
edited, but it does not become a second authority.

**Index**
A replaceable search or ranking aid over owned references. An index can help
find information; it cannot decide what is current, visible, or true.

**Setting**
An explicit, current choice that affects behavior within a named scope. A
setting is versioned and explainable; it is not inferred from observed habits.

**Preference**
Something the user appears to or explicitly says they favor. An inferred
preference remains a reviewable claim until the user accepts it as a setting.

**Lifecycle intention**
A behavior needed around a task or session—prepare context, guard a change,
observe an outcome, preserve work, or close with proof—independent of the event
names exposed by a particular harness.

**Hook plan**
A harness-specific projection of lifecycle intentions. It states which native
event or fallback can realize each intention and makes unsupported behavior
visible.

**Execution profile**
The semantic constraints for an execution: its role, capability, effort,
privacy, permissions, and budget. It does not claim which provider, model, or
account actually ran; observed execution evidence does that.

**Retirement**
The verified end of an old path's normal use after its information, consumers,
writes, rollback, and negative controls have transferred. Retirement is not
deletion.

**Product loop**
The path from outside-world signal to reviewed action and later outcome.

**Private overlay**
User data, private adapters, credentials, personal doctrine, local paths, and
product-specific sources outside public core. Public Compound must work with
synthetic fixtures without this overlay. The overlay configures ingress and
secrets; it is not a second memory or truth owner.

## Design Distinctions

**Monolith vs microservice**
Compound is one product/runtime boundary with deep internal modules. Modules
define lifecycle invariants; they are not separate authority services.

**Findable vs centralized**
Findable does not require every source file to move into one folder. Compound can
keep sources in place while centralizing identifiers, claims, positions,
citations, and context assembly.

**Safe vs smooth**
Safe lifecycle plumbing should be quiet. Compound should interrupt only when the
next move needs judgment, private input, or a risky external side effect.
