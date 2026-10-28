package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The MINT RATCHET — the build-layer half of the Two-Class Law (RATIFIED 2026-07-10,
// instruction_rule:two-class-law). Everything the system MINTS is one of two things at creation:
//
//	(1) ENFORCEMENT (in charge): blocks or passes silently at an adapter/doctor/gate; every block
//	    carries a receipt and an undo; exists ONLY as the compilation of a receipted operator ruling.
//	(2) EVIDENCE (perspective): informs but never blocks; must name its consumer (an enforcement
//	    point, an adjudication lane, or orient) or it decays.
//
// The law's own words: "A new term needs one plain sentence and a named consumer, or it dies in the
// session that minted it." This ratchet makes that mechanical for the three surfaces the build layer
// mints new identifiers on:
//
//	organ        — a new internal/ package (a new front-door organ)
//	registry     — a new compound://registries/<name> surface (a new minted term/registry; this is
//	               the two-class law's own "registry" vocabulary and its route destination)
//	status-note  — a new "<Label> [status] note:" marker in the hot status map (docs/architecture/status.md)
//
// A detected identifier that has NEITHER a grandfather grant (pre-law debt, dated below) NOR a
// two-class MintHome FAILS the architecture doctor — so new terminology cannot accrete unclassified.
//
// Detection is DETERMINISTIC (directory listing / regex over tracked files), never model-inferred.
// The grandfather sets are the exact identifiers present when the ratchet landed (2026-07-12); they
// are pre-law debt listed explicitly (per the operator ruling: whitelist genuine pre-existing
// violations with a dated grandfather list rather than weakening the check). A NEW identifier must
// earn a MintHome (enforcement, or evidence with one plain sentence + a named consumer) or be added
// to a grandfather set with a dated reason; it cannot pass silently.

// MintClass is the two-class home every minted artifact must declare.
type MintClass string

const (
	// MintEnforcement — the identifier is an enforcement point: it blocks or passes silently at an
	// adapter/doctor/gate and is the compilation of a receipted ruling (receipt + undo). Its teeth
	// ARE the gate, so a bare registration suffices.
	MintEnforcement MintClass = "enforcement"
	// MintEvidence — the identifier is evidence: it informs but never blocks. It must carry one
	// plain sentence of purpose AND name a consumer (an enforcement point, an adjudication lane, or
	// orient), or it decays.
	MintEvidence MintClass = "evidence"
)

// MintHome is the two-class registration for one minted identifier.
type MintHome struct {
	Class    MintClass
	Purpose  string // one plain sentence — REQUIRED for MintEvidence
	Consumer string // named consumer (enforcement point / adjudication lane / orient) — REQUIRED for MintEvidence
}

// Valid reports whether a home is well-formed for its class. An enforcement point's teeth are the
// gate itself, so it needs no prose; evidence must name a purpose and a consumer or it decays.
func (h MintHome) Valid() bool {
	switch h.Class {
	case MintEnforcement:
		return true
	case MintEvidence:
		return strings.TrimSpace(h.Purpose) != "" && strings.TrimSpace(h.Consumer) != ""
	default:
		return false
	}
}

// mintHomes is the two-class home for identifiers minted AFTER the ratchet landed (2026-07-12).
// Every new organ / registry surface / status note goes here (or is grandfathered with a dated
// reason). Keys are "<surface>:<id>" (e.g. "organ:scratchpad").
var mintHomes = map[string]MintHome{
	// activity is the Tier-0 + Tier-1 ambient-activity observed family: it owns the
	// compound.activity_observation.v0.2 event schema (Tier-0 app focus/idle/lock +
	// Tier-1 KindWindowFocused envelope) + ActivitySpan reductions under
	// observed/activity/. Tier-0 events flow from the Swift launchd collector; the
	// awstream package joins ActivityWatch across hostnames; the tier1 package
	// captures per-app envelope meta (pane_cwd, git_remote, agent_kind) from
	// cmux/iTerm2/Warp/Terminal.app/VSCode/Cursor. It classifies and reconciles
	// activity against the work ledger but blocks nothing and promotes nothing
	// (zero auto memory candidates), so it is evidence, not enforcement.
	"organ:activity": {
		Class:    MintEvidence,
		Purpose:  "Tier-0 + Tier-1 content-free ambient-activity family (compound.activity_observation.v0.2 events + ActivitySpan reductions) under observed/activity/, ingested by a grant-gated fail-closed drain membrane; AW sidecar reader joins events across hostnames; Tier-1 envelope collector captures pane_cwd/git_remote/agent_kind",
		Consumer: "compound activity CLI (enable/disable/status/drain/sweep/aw materialize/tier1 collect) + attention activity-unaccounted watching group + activity-unaccounted operator_private lens",
	},
	// proofscope derives the go-test package scope for a sealed commit pair; it
	// is evidence until testbattery verifies a claim against it; presubmit reads
	// it today without certifying anything.
	"organ:proofscope": {
		Class:    MintEvidence,
		Purpose:  "derive the exact go-test package scope for a sealed base/head commit pair",
		Consumer: "compound proof-check --presubmit (non-certifying pre-landing go-test scope; claim verification and the internal/work canonical go-test step follow in p2)",
	},
	// store is the ADR-0005 pg experiment operator surface; it never promotes or gates
	// memory truth — exports are governed writes with their own PolicyDecision door.
	"organ:store": {
		Class:    MintEvidence,
		Purpose:  "Postgres+pgvector backend operator surface (doctor/init/status/migrate/sync/embed/recall); migrate/sync/embed are governed external_side_effect writes gated by store.postgres_export.write with Receipt+Transition proof in the home event store; migrate reads through mem's locked MigrationSnapshot; owns no authority object",
		Consumer: "cmd/compound cmdStore shim (`compound store ...`); composition-root pg engine injection in cmd/compound/main.go",
	},
	// schedule projects launchd+crontab and dispatches reviewed repairs;
	// authority owns OS effects and their native phase evidence.
	"organ:schedule": {
		Class:    MintEvidence,
		Purpose:  "launchd+crontab runtime projection (list/show/doctor) and thin reviewed repair dispatch to authority; owns no authority object",
		Consumer: "cmd/compound command-registry dispatch (`compound schedule ...`) via schedule.Run",
	},
	"organ:sessionmining": {
		Class:    MintEvidence,
		Purpose:  "deterministic propose-only router for session-mined preferences, rule-retirement plans, and capability lifecycle proposals with semantic-key idempotency across overlapping windows",
		Consumer: "compound sessions mine --apply through cmd/compound's thin applySessionMiningReport adapter; operator review queues consume the emitted memory, distiller, and capability artifacts",
	},
	// lanemanifest is the one declarative unit that names a harness lane. Compound's
	// consumption is the hook projection: `compound hook install --lane-file` derives
	// the install harness and hook roster from it, and governed spawn refuses a lane
	// whose observed roster is not full. It declares and never mutates — hook and
	// harness configs retain authority, so it is evidence, not enforcement.
	"organ:lanemanifest": {
		Class:    MintEvidence,
		Purpose:  "declarative lane-manifest unit (harness + model + capture + route + hooks.roster) that switchback and compound both consume; compound derives the install harness and hook roster from it and governed spawn refuses lanes whose observed roster is not full",
		Consumer: "cmd/compound cmdHookInstall --lane-file + GovernedFullRosterGate (work advance --execute), internal/hookplan InstallHooksForLane",
	},
	// lanematrix answers "which harness can run which lane, and which of those
	// combinations exist yet" by composing the launch owner's published
	// profile-conformance against the harness registry. It reads and never writes,
	// and it deliberately reports an unbound combination as `unknown` rather than
	// `possible` — the projection publishes no wire protocol or tap binding, so
	// claiming compatibility would be the over-claim docs/NAMING.md calls unsafe.
	"organ:lanematrix": {
		Class:    MintEvidence,
		Purpose:  "read-only (harness x lane) combination projection over published launch profile-conformance plus the harness registry; enumerates from registries rather than a launcher-directory listing and asserts no compatibility the projection does not publish",
		Consumer: "cmd/compound command-registry dispatch (`compound lanes matrix`) via cmdLanes",
	},
	// browser is the macOS headless/automation-chrome observation organ; its only
	// mutation is the B-scope `doctor --fix` orphan-idle swiftshader reap with
	// per-target confirmation, never a write to compound authority.
	"organ:browser": {
		Class:    MintEvidence,
		Purpose:  "read-only headless/automation browser observation surface; owns no authority object",
		Consumer: "cmd/compound command-registry dispatch (`compound browser ...`) via browser.Run",
	},
	// adapter is the peer-of-core home for rented external protocol readers (awstream today).
	// It joins/normalizes foreign event shapes for activity and never promotes or gates.
	"organ:adapter": {
		Class:    MintEvidence,
		Purpose:  "rented external-protocol adapters (awstream ActivityWatch multi-host join) that feed owned object families without owning authority or promoting memory",
		Consumer: "internal/activity drain + compound activity aw materialize",
	},
	// eventstore enforces the single physical-layout boundary for COMPOUND_HOME/events. The
	// architecture doctor blocks any production path construction or filesystem access outside
	// this package, so the organ is enforcement rather than a second meaning authority.
	"organ:eventstore": {Class: MintEnforcement},
	// act is the generic governed outbound-write seam: it REFUSES to call an adapter's Write unless
	// handed a matching allowing PolicyDecision for that exact ActionIntent, and every write it does
	// pass carries an outward_delta Receipt (proof) plus a pre-image artifact + RevertIntent (undo).
	// Its teeth ARE the policy gate — it is the compilation of the "no mutation without a valid
	// PolicyDecision" ruling — so it is enforcement, not evidence.
	"organ:act": {Class: MintEnforcement},
	// actuator is the execution router that turns KNOWN work into governed executions within
	// declared limits. Its teeth ARE two gates: a BudgetEnvelope/ledger that REFUSES dispatch once a
	// per-channel spend/rate cap is exhausted, and a Route() classifier that HOLDS every
	// external/public/financial/irreversible or non-easy move for a human (only local_reversible+easy
	// + policy-allowed + under-budget work auto-executes, through the existing harness.Adapter.Run /
	// act.Execute seams). It blocks and passes, so it is enforcement, not evidence — the consumption
	// ledger it records is the audit of that enforcement, the same posture as act's receipts.
	"organ:actuator": {Class: MintEnforcement},
	// promotedriver is the governed candidate-only actuator behind `compound
	// memory promote-driver`. Its teeth are the single-flight lock, exact
	// candidate/current-claim plan, per-operation CAS and idempotency key, exact
	// result frames, same-pass recovery, and terminal-plan bijection. Registry
	// imports and lane-belief mutation remain experimental previews. Dry-run is
	// the default; apply requires --confirm plus a maintenance work identity.
	"organ:promotedriver": {Class: MintEnforcement},
	// verdictflow is the shared plan->record->render->load->annotate scaffolding the three
	// panel-verdict review bridges had copy-pasted. It writes review artifacts through reviewartifact
	// and reads them back, but touches no ledger/store/writeback surface and performs no terminal
	// effect (those stay in the domain callbacks), so it is evidence, not enforcement.
	// reviewqueue is the canonical workbench review-queue builder: pure
	// projection scaffolding (merge + annotate), no gate, no ledger, no
	// terminal effect — evidence, not enforcement.
	"organ:reviewqueue": {
		Class:    MintEvidence,
		Purpose:  "canonical workbench review-queue builder (merge + annotate rows); presentation-side only, no authority object, no terminal effect",
		Consumer: "cmd/compound workbench CLI (canonical queue rendering + outbound verdict/dry-run annotation rows)",
	},
	"organ:verdictflow": {
		Class:    MintEvidence,
		Purpose:  "shared verdict plan->review->apply->annotate scaffolding for panel-verdict bridges (record/load/annotate/reviewed-extras mechanics only; no ledger, store, writeback, or terminal effect)",
		Consumer: "internal/product, internal/sourceobs, and internal/capgraph verdict adapters (Record*Plan / Annotate*Queue / Load*Review / LatestReviewedFromExtras)",
	},
	// routeev is the EV-card composer: a deterministic pure pricing function (policy line vs
	// deviation), the calibration divergence comparator, and content-addressed card persistence
	// under observed/route-evcards. The card is advisory evidence attached to a deviation
	// decision; the gate that binds (route_deviation prediction staking) lives in
	// envelope.Validate and the minting door lives in mem — routeev itself blocks nothing and
	// promotes nothing, so it is evidence, not enforcement.
	"organ:routeev": {
		Class:    MintEvidence,
		Purpose:  "deterministic EV-card composition + calibration comparison for route deviations (expected cost-to-success pricing that refuses to price missing inputs; no gate, no ledger, no terminal effect)",
		Consumer: "cmd/compound route ev (compose + persist the card) and cmd/compound work close (calibrate the card verdict against the actual outcome through the mem observed door)",
	},
	"organ:statussurface": {
		Class:    MintEvidence,
		Purpose:  "body-free adaptive projection of harness-native context and rate-limit metadata with TTL-bound hysteresis and reversible terminal themes",
		Consumer: "compound system status-surface snapshot/render plus Claude Code statusLine and the Codex theme projection",
	},
	// terminal is the unified host-terminal (cmux / iTerm2 / Warp / Terminal.app) operator
	// surface. It is read-mostly: doctor + hooks + wire/unwire + workspace + feed verbs expose
	// per-terminal IPC bridges through a single Terminal interface, with workspace and feed
	// mutations governed at the command-registry layer (FirstPartyCommandGoverned). It does
	// not promote or gate memory, write to a ledger, or own an authority object; the terminal
	// IPC handles the only mutations, and those happen under the operator's explicit policy
	// grant — so it is evidence, not enforcement.
	"organ:terminal": {
		Class:    MintEvidence,
		Purpose:  "unified host-terminal operator surface (cmux/iTerm2/Warp/Terminal.app) — Terminal interface + registry + per-terminal adapters; doctor/hooks are read, wire/unwire/workspace/feed are governed at command-registry layer; no authority object, no ledger write, no memory promotion",
		Consumer: "internal/terminal/cli (terminalcli.Run wired in cmd/compound command registry) + terminal_admission_test.go contract pin",
	},
	// game is the read-only game-catalog + doctor projection surface; aggregates from
	// internal/sourceobs (Source Observation Memory) into a stable gmid_* catalog keyed on
	// (source, sourceKey, kind). It owns no authority object, writes no ledger byte, and
	// promotes no memory candidate — the projection is consumed verbatim by the cmd/compound
	// `game` CLI (doctor / observe / genres / companies / trending), so it is evidence, not
	// enforcement. observe is governed at the command-registry layer (subcommand admission).
	"organ:game": {
		Class:    MintEvidence,
		Purpose:  "read-only game catalog + doctor projection aggregating internal/sourceobs into stable gmid_* keys; no authority object, no ledger write, no memory promotion",
		Consumer: "cmd/compound game CLI (doctor / observe / genres / companies / trending); observe is governed at the command-registry layer",
	},
	// finding is the direct-write home for agent-authored operational findings under
	// COMPOUND_HOME/observed/findings — the POSITIVE counterpart to the placement guard: a governed
	// place to record a durable operational note instead of inventing a path. It writes only a
	// content-addressed evidence record (source_class=evidence, NEVER a ledger byte, NEVER a promoted
	// Claim), so it is evidence, not enforcement; promotion to canon stays the separate governed
	// memory door.
	"organ:finding": {
		Class:    MintEvidence,
		Purpose:  "direct-write, content-addressed home for agent-authored operational findings (durable operational notes) under COMPOUND_HOME/observed/findings, recorded as source_class=evidence with never a ledger byte",
		Consumer: "compound finding list/show recall surface (and orient operational recall); promotion to canon is the separate governed memory door",
	},
	// laneselect landed the same day as the ratchet (6112375) — its registration is the ratchet's
	// first live catch, classified rather than grandfathered.
	"organ:laneselect": {
		Class:    MintEvidence,
		Purpose:  "pure preparation-lane ranking (capability fit, token speed, cost) with a veto seam",
		Consumer: "work dispatch-plan lane selection via the routeevidence adapter",
	},
	// operatornotice is the single missing edge between the attention hold frontier
	// (internal/attention) and the existing act post buzz transport (internal/act). It scans
	// BandNeedsYou groups, emits exactly one governed buzz notification per new hold
	// (idempotent on re-scan, persisted via the home event store), and counts unnotifiable /
	// refused holds in a doctor figure. It reuses attention.Build, act.Execute, and the
	// existing standing-grant registry; it owns no authority object, gates nothing, and
	// promotes nothing — the persistence it does is via the existing event store seam, so it
	// is evidence, not enforcement.
	"organ:operatornotice": {
		Class:    MintEvidence,
		Purpose:  "single missing edge between attention BandNeedsYou groups and the act post buzz transport; one function (Notify) emits exactly one governed buzz notification per new hold (idempotent on re-scan) and counts unnotifiable/refused holds in a doctor figure; reuses attention.Build, act.Execute, and the existing standing-grant registry; owns no authority object",
		Consumer: "compound notice-flush CLI (the operator-facing wiring; no daemon or scheduler)",
	},
	// data-sources and services land together (infra-os registry import door,
	// work_0ccf9361f136): both are evidence, not enforcement — they inform the
	// os-absorption doctor's infra_registry_authority proof mode and future
	// runtime/status projections, never block a policy decision themselves.
	// retention-classes is enforcement, not a catalogue. Its teeth are two refusals: the doctor
	// FAILS on any accumulator no class claims (so a new way to fill the disk cannot appear
	// unowned), and the reclaim planner REFUSES to hand an operator-gated, non-rebuildable, or
	// currently-in-use instance to an unattended reclaimer. It decides what may be deleted
	// without a human, which is a gate rather than an observation.
	"registry:retention-classes":       {Class: MintEnforcement},
	"registry:retention-apply-classes": {Class: MintEnforcement},
	"registry:data-sources": {
		Class:    MintEvidence,
		Purpose:  "infra-os database/data-authority registry (database.yaml) installed verbatim under COMPOUND_HOME with source hash + revision recorded",
		Consumer: "authority os-absorption-doctor's infra_registry_authority proof mode (compound/internal/authority)",
	},
	"registry:services": {
		Class:    MintEvidence,
		Purpose:  "infra-os services/topology registry (services.yaml) installed verbatim under COMPOUND_HOME with source hash + revision recorded",
		Consumer: "authority os-absorption-doctor's infra_registry_authority proof mode (compound/internal/authority)",
	},
	"registry:probe-contracts": {
		Class:    MintEvidence,
		Purpose:  "ordered task-class probe contracts that declare topic-subtree groups and per-group ContextPack memory budgets",
		Consumer: "internal/contract runtime snapshot compiler (buildProbeSelectionPlan and loadProbeGroupedRefsFromView)",
	},
	// narrate is the correspondent composer: a pure read-only projection that
	// informs the operator; it blocks nothing, so it is evidence, not enforcement.
	"organ:narrate": {
		Class:    MintEvidence,
		Purpose:  "deterministic correspondent script composed over the attention projection, orient changed-set, and recent receipt summaries",
		Consumer: "compound narrate CLI and the embedded narrate field of orient --operator",
	},
	// latenttax is the one-week latent-tax rig: a read-only factorial evaluator that
	// derives thrash/tokens/latency/success from existing ledger objects per work_id
	// and evaluates the pre-registered kill-criteria. It informs the operator's
	// obligation-placement adjudication; it blocks nothing and promotes nothing, so it
	// is evidence, not enforcement.
	"organ:latenttax": {
		Class:    MintEvidence,
		Purpose:  "factorial latent-tax rig that derives thrash/tokens/latency/task-success from existing ledger objects per work_id and evaluates three pre-registered kill-criteria against length-matched arms",
		Consumer: "compound latent-tax report + the operator's obligation-placement-law adjudication (an adjudication REQUEST citing run receipts, never auto-promotion)",
	},
	// jobprobe is the job-roster probe-refresh collector: it re-observes current
	// unit liveness for declared probe targets and refreshes the roster through
	// authority; it informs job_health surfacing but blocks nothing, so it is
	// evidence, not enforcement.
	"organ:jobprobe": {
		Class:    MintEvidence,
		Purpose:  "probe-refresh collector that re-observes current unit liveness for declared probe targets (files under COMPOUND_HOME) and refreshes the job-roster authority so job_health derives from observed state, not stale imported claims",
		Consumer: "authority job-roster refresh surface consumed by the director job_health scanner (compound/internal/director)",
	},
	// behaviorallens is the composable behavioral-lens framework: a uniform observe->InferredSignal
	// plug with visibility-gated composition and a first sycophancy (paired-stance shift) lens. It
	// records evidence rows only — it never gates, promotes, or changes a route/role — so it is
	// evidence, not enforcement. The governed modulator that would act on a signal is a DEFERRED
	// enforcement-class consumer (minted only once a second lens exists — two-consumers law).
	"organ:behaviorallens": {
		Class:    MintEvidence,
		Purpose:  "composable behavioral-lens framework — a uniform observe->InferredSignal plug with visibility-gated composition and a per-lane sycophancy (paired-stance endorsement shift) lens; records evidence rows only, never a gate and never auto-acting",
		Consumer: "compound behavioral-lens CLI (list/run/compose/signals) projecting only public_safe signals for operator review; operator_private signals stay under COMPOUND_HOME; the governed modulator is the named later enforcement consumer",
	},
	// landlease is the land-lease coordination hint: a single-holder, TTL-bounded, steal-with-reason
	// advisory MUTEX over the ACT of landing to main (a flock-guarded JSON lease under COMPOUND_HOME).
	// Acquire REFUSES a competing LIVE holder, but that refusal is a cooperative mutex hint, NOT a
	// governed gate: it carries no PolicyDecision, no Receipt, no undo, and compiles no operator ruling
	// (the three things MintEnforcement requires — see organ:act). It never mutates repo state; it only
	// records WHO holds the right to land, and the compound-land ceremony is the enforcement point that
	// ACTS on the hint. An advisory hint that names its consumer is textbook evidence, not enforcement.
	"organ:landlease": {
		Class:    MintEvidence,
		Purpose:  "single-holder, TTL-bounded, steal-with-reason advisory mutex over the ACT of landing to main (a flock-guarded JSON lease under COMPOUND_HOME) so concurrent executor lanes serialize the fast-forward-merge window instead of colliding; records who holds the right to land, never mutates repo state and emits no PolicyDecision/Receipt",
		Consumer: "the compound-land ceremony (skills/compound-land) — a lane reads `compound land-lease status --json` at scope-claim (§0), acquires it with holder `<harness>:<session>:<work_id>` immediately before the ff-merge (§3, `--wait --timeout 120`), and releases it with the same holder string right after the guarded merge lands (§4); steal-with-reason is the auditable override for a stale holder",
	},
	// executorlens is the executor-receipt locator: an evidence-only collector that reads THIS repo's
	// own executor lanes' (Codex, Claude Code) receipts/events under COMPOUND_HOME and builds
	// behaviorallens Observations — public_safe hedging over executor output text, operator_private
	// agreement-asymmetry over receipt verdicts. It records evidence rows only through behaviorallens
	// and emits outward only the public projection, so it is evidence, not enforcement.
	"organ:executorlens": {
		Class:    MintEvidence,
		Purpose:  "evidence-only collector that reads this repo's executor lanes' (Codex, Claude Code) receipts/events and builds behaviorallens Observations (public_safe hedging over output text + operator_private agreement-asymmetry over receipt verdicts), so the agents that execute compound's work are themselves measured for behavior-bending",
		Consumer: "compound executor-lens CLI (run/subjects) composing through behaviorallens.RegistryForHome and recording signals; the outward projection is public_safe only (operator_private executor signals stay under COMPOUND_HOME); the deferred governed modulator is the later enforcement consumer",
	},
	// lensmodulator is the governed modulator behaviorallens named as its later enforcement consumer:
	// it reads recorded lens signals and, when one crosses an operator-armed threshold, applies a
	// REVERSIBLE route/role demote THROUGH act.Execute — which refuses without a matching allowing
	// PolicyDecision. Its teeth ARE that policy gate plus the calibration guard (a threshold cannot arm
	// on an empty signal ledger); it is the compilation of the "no route/role modulation without a
	// valid PolicyDecision and a calibrated threshold" ruling, with a receipt (outward_delta) and undo
	// (RevertIntent) on every act — so it is enforcement, not evidence. Consumer: compound lens-modulate
	// CLI (arm/thresholds/modulate --confirm/revert); default off, operator_private modulations stay
	// operator-local (never a shared route projection).
	"organ:lensmodulator": {Class: MintEnforcement},
	"organ:intent": {
		Class:    MintEvidence,
		Purpose:  "thin expressive-intent coordinator that preserves exact source capture and presents work-owned meaning resolution as a reviewable create-plan or named ambiguity park without owning a plan, context, policy, or admission format",
		Consumer: "compound intent plan|review|apply CLI plus the existing work admission and operator-review path that alone may materialize the proposed WorkEnvelope",
	},
	"organ:retrievalparity": {
		Class:    MintEvidence,
		Purpose:  "privacy-bounded retrieval traffic and replay comparisons over production consumer paths",
		Consumer: "compound memory parity-replay and parity-freeze operator review",
	},
	"organ:persist": {
		Class:    MintEvidence,
		Purpose:  "storage abstraction layer defining LedgerStore/EventStore/WorkStore/VectorStore interfaces; file is the sole supported backend and the Postgres+pgvector adapter is behind fail-closed experimental admission",
		Consumer: "internal/store operator verbs and internal/contract's injected experimental recall-engine factory",
	},
	// stream is the read-only tail organ over the work-event log + ledger seq: it emits JSONL frames
	// (a finite --once snapshot with an end-of-log cursor + generated_at, or a long-running --follow
	// tail) that a client translates. It blocks nothing, promotes nothing, and opens the log read-only
	// (it never takes the work-event write lock), so it is evidence, not enforcement.
	"organ:stream": {
		Class:    MintEvidence,
		Purpose:  "read-only byte-cursor tail over events/work.jsonl + ledger/seq that emits JSONL frames (a finite --once snapshot with an end-of-log cursor + generated_at, or a --follow live tail) without ever taking the work-event write lock",
		Consumer: "the console live stream route (console/src/app/api/stream/route.ts) spawns `compound stream --follow` per SSE client and seeds backlog through the stream-snapshot door (`compound stream --once --json`, console/src/lib/doors.ts); frames translate into the existing ready/transition/memory-pulse SSE vocabulary",
	},
	// runattempt is the concrete harness-execution-attempt store: one RunAttempt per harness run
	// against one work_id, persisted under COMPOUND_HOME/run_attempts. It records who/what ran and
	// when, but performs no gating, no promotion, and no terminal effect — the work package consumes
	// RunAttemptIDs to build proof chains. It is evidence, not enforcement.
	// lanes is the installer's readiness ladder: it refuses to report a lane
	// "fresh" unless the pack's own --drift check is clean, so the gate is the
	// organ itself (enforcement), not a report someone else must act on.
	"organ:lanes": {Class: MintEnforcement},
	"organ:readiness": {
		Class:    MintEvidence,
		Purpose:  "one read-only evaluation of whether a Claude lane is ready to execute (hook binding fresh, instruction pack clean, context pack attached, permission posture attested), with the repair command per unmet item",
		Consumer: "internal/actuator governed execute door (refuses an explicit mutation-capable launch on an unready lane), work delegate --dry-run, and the harness readiness operator face",
	},
	// projectcontract holds the YAML shapes that BOTH readers of the source-in-place
	// project contract must agree on. internal/product and internal/runtimebinding each
	// parse the same `project.yaml`; when a field's shape lived in two structs, one reader
	// accepted a contract the other rejected (`data.databases` as a record failed product
	// import and runtime doctor while the file itself was valid), so a product's deployment
	// silently never reached the ledger. It performs no I/O, records nothing, gates nothing.
	"organ:projectcontract": {
		Class:    MintEvidence,
		Purpose:  "one owner for the YAML shapes of the source-in-place project contract, so its two readers cannot disagree about a field",
		Consumer: "internal/product project-contract import and internal/runtimebinding LoadProjectContract",
	},
	"organ:runattempt": {
		Class:    MintEvidence,
		Purpose:  "concrete record of one harness execution attempt against a work_id (who/what/when), persisted under COMPOUND_HOME/run_attempts for proof-chain linkage",
		Consumer: "internal/work WorkEnvelope RunAttemptIDs slot, work proof-chain doctor, and orient execution history",
	},
	// doctor is the health-signal organ behind `compound doctor`: it answers "is the INSTALLED binary at
	// least as new as the repo's target tip?" and reports the answer as a named signal (installed SHA +
	// target tip + relation), never a bare boolean. It is evidence, not enforcement, on the
	// organ:landlease pattern: RenderBinaryStaleness prints the verdict into the operator's health
	// report (and the --json payload) and the SEPARATE decider — the operator, who runs `make install`
	// — chooses whether to act on it. Nothing here returns a blocking error, changes doctor's exit
	// code, or fails a certifying test, which is exactly what separates it from organ:eventstore and
	// organ:flagaudit, where the organ itself owns the refusal. Every failure seam degrades to
	// RelationUnknown rather than a false "stale" verdict, so the signal informs and never blocks.
	"organ:doctor": {
		Class:    MintEvidence,
		Purpose:  "names how the installed compound binary's build SHA relates to the repo's target tip (equal/descendant/ancestor/unknown) so an out-of-date install is read as a verdict instead of being hidden behind a green health report",
		Consumer: "the `compound doctor` operator surface (cmd/compound/doctor_cli.go), which carries the report in doctorReport.BinaryStaleness for `--json` consumers and renders the installed-SHA/target-tip/relation line through doctor.RenderBinaryStaleness; the operator is the decider that acts on it with `make install`",
	},
	// proveable-pair law status note marks the named discipline: every claim produced by an agent must be
	// paired with a content-bound, mechanically-verifiable, replayable record of what actually changed.
	// The law holds by construction in the close path (work.go close emits Receipt + Transition
	// together, each validated by envelope.Validate) AND by enforcement: checkProveablePairLaw scans
	// the eventstore on every architecture doctor run and fails on any orphan (a work whose close
	// emitted one side of the pair without the other). See ADR-0007 for the audit and law text.
	"status-note:Proveable-pair law note": {
		Class:    MintEnforcement,
		Purpose:  "operator-facing named discipline: every agent claim is paired with a content-bound, replayable proof of what actually changed (Receipt + Transition, ActionIntent + Receipt, model argument + Lean certificate); the architecture doctor's checkProveablePairLaw fails the build on any orphan receipt or transition",
		Consumer: "compound architecture doctor checkProveablePairLaw (which scans the eventstore on every doctor run and fails on any work with one side of the pair but not both) plus the docs/architecture/status.md status surface",
	},
	// flagaudit is the flag-validation ratchet: it enumerates every command path the CLI runtime
	// registry exposes, probes each in a hermetic subprocess with one unknown flag, and censuses the
	// paths that silently accept it (exit 0 AND no mention of the flag). EnforceCeiling pins that
	// census — the set may only shrink, never grow — and cmd/compound's flag_validation_ratchet_test
	// t.Fatalf's on its error, so a new first-party command that swallows unknown flags cannot land.
	// It is enforcement, not evidence, on the eventstore / proveable-pair pattern: the organ OWNS the
	// refusal (EnforceCeiling returns the blocking error; the ceiling is the teeth) and the failure is
	// terminal rather than advisory — nothing downstream decides whether to act on it. That it fails a
	// certifying test rather than a runtime PolicyDecision door is the same shape as organ:eventstore
	// (architecture doctor blocks the boundary violation) and the proveable-pair note (checkProveablePairLaw
	// fails the doctor run): neither carries PolicyDecision/Receipt/undo either. Contrast organ:landlease,
	// which is evidence precisely because its refusal is a cooperative hint that a SEPARATE enforcement
	// point (the compound-land ceremony) chooses to act on; flagaudit has no such downstream decider.
	"organ:flagaudit": {
		Class:    MintEnforcement,
		Purpose:  "flag-validation ratchet over the cmd/compound command registry: censuses first-party command paths that silently accept an unknown flag (exit 0 with no mention of it) and pins that census as a ceiling that may only shrink, so a new command that swallows typo'd flags cannot land",
		Consumer: "cmd/compound/flag_validation_ratchet_test.go, which runs the census over the command registry and t.Fatalf's on EnforceCeiling's error, plus internal/flagaudit.PinnedSilentAcceptors as the recorded ceiling",
	},
	// organ:console owns the `compound console` launch-plan construction, capability
	// gate, sanitized child-launch env, and instance-pinning inputs. Its runtime
	// launch gate (BuildLifecycleLaunchPlan + ValidateMutationLaunchEnvironment)
	// blocks unsafe mutation activation: build-mode refuses mutations, private
	// fallbacks are rejected, mutation-mode requires an explicit COMPOUND_HOME
	// plus a 64-hex out-of-band capability, and the LaunchPlan JSON never carries
	// the capability value. The gate is a permission boundary, so this organ is
	// enforcement.
	"organ:console": {
		Class:    MintEnforcement,
		Purpose:  "console launch-plan construction, capability gate, sanitized child-launch env, and instance-pinning inputs; BuildLifecycleLaunchPlan blocks unsafe mutation activation (build-mode rejects mutations, mutation-mode requires explicit home + 64-hex capability, refutes private fallbacks) so the JSON LaunchPlan only ever exposes CapabilityRequired without leaking the value",
		Consumer: "cmd/compound cmdConsole shim (`compound console dev|build|serve --operator`); tests across internal/console (BuildLifecycleLaunchPlan, ValidateMutationLaunchEnvironment, LaunchEnv) plus console cap-v2 smokes and the public-alpha capability-profile canary",
	},
}

// grandfatheredOrgans — internal/ packages present when the ratchet landed (2026-07-12). Pre-law
// debt: they pass without a fresh two-class home. arch.Classification already records each one's
// architectural class; a NEW organ must additionally declare its two-class MintHome.
var grandfatheredOrgans = map[string]bool{
	"arch": true, "attention": true, "authority": true, "bench": true, "breadth": true,
	"candidates": true, "capgraph": true, "contract": true, "corpus": true, "decisions": true,
	"director": true, "distiller": true, "envelope": true, "eventsource": true, "harness": true,
	"harvest": true, "home": true, "hook": true, "hookplan": true, "knowledge": true,
	"lanebeliefs": true, "localmodel": true, "mem": true, "negatives": true, "observation": true,
	"panel": true, "policy": true, "product": true, "profiles": true, "projection": true,
	"release": true, "reviewartifact": true, "routeevidence": true, "runtimebinding": true,
	"schema": true, "sourceobs": true, "tooltopology": true, "work": true, "writeback": true,
}

// grandfatheredRegistries — compound://registries/<name> surfaces referenced in non-test Go when the
// ratchet landed (2026-07-12). Pre-law debt. A NEW registry surface must declare a two-class MintHome.
var grandfatheredRegistries = map[string]bool{
	"autonomy-halt": true, "capability-graph": true, "decision-freeze": true,
	"decision-source-authority": true, "editable-surfaces": true, "job-roster": true,
	"model-router": true, "operator-approval-frontier": true, "operator-decision-prompts": true,
	"operator-state": true, "os-absorption-source-matrix": true, "placement-rules": true,
	"promotion-criteria": true, "release-goal-holds": true, "source-manifests": true,
	"standing-grants": true, "work-leases": true,
}

// grandfatheredStatusNotes — "<Label> [status] note:" markers present in docs/architecture/status.md
// when the ratchet landed (2026-07-12). Pre-law debt. A NEW status note must declare a two-class
// MintHome. Ids are the marker label with the trailing colon stripped.
var grandfatheredStatusNotes = map[string]bool{
	"ASC daily signals public-boundary note":             true,
	"ASC daily source note":                              true,
	"Active packet proof note":                           true,
	"Admission preview retirement status note":           true,
	"CLI safety note":                                    true,
	"Capability command status note":                     true,
	"Capability evaluation status note":                  true,
	"Capability evaluation surfacing note":               true,
	"Capability semantic surfacing note":                 true,
	"Capability status note":                             true,
	"Capability surfacing status note":                   true,
	"CapabilityGraph meta-token note":                    true,
	"ChatGPT capture edge note":                          true,
	"ContextPack adaptation-offer note":                  true,
	"ContextPack hook status note":                       true,
	"Contract command status note":                       true,
	"Conversation preflight import-safety doctor note":   true,
	"Conversation recall gate note":                      true,
	"Dispatch read-model status note":                    true,
	"Distiller request record note":                      true,
	"Distiller runtime-source note":                      true,
	"Executor liveness and terminal-residue note":        true,
	"External repo context-evaluation note":              true,
	"External repo intake surface note":                  true,
	"Hook context mode status note":                      true,
	"Hook context side-effect note":                      true,
	"Hook event identity note":                           true,
	"Hook input hash note":                               true,
	"Hook readiness authority note":                      true,
	"Install/setup status note":                          true,
	"ObservationBatch status note":                       true,
	"Operator adaptation-intake note":                    true,
	"Operator needs-me candidate authority note":         true,
	"Operator observation-review projection note":        true,
	"Operator orientation adjacent-progress doctor note": true,
	"Operator orientation adjacent-progress note":        true,
	"Operator product-loop projection note":              true,
	"Operator source-control projection note":            true,
	"Outbound distribution dry-run receipt note":         true,
	"Outbound distribution review note":                  true,
	"Panel capability-feedback note":                     true,
	"Panel execution-failure note":                       true,
	"Panel memory-review note":                           true,
	"Panel public-knowledge privacy note":                true,
	"Panel stale-review note":                            true,
	"Product action-card status note":                    true,
	"Product action-context execution note":              true,
	"Product action-context lifecycle note":              true,
	"Product authority status note":                      true,
	"Product command status note":                        true,
	"Product observation-verdict bridge note":            true,
	"Product outbound dry-run bridge note":               true,
	"Product-loop closure proof note":                    true,
	"Product-loop projection note":                       true,
	"Product-loop signal-debt consumption note":          true,
	"Proof-check console note":                           true,
	"Proof-check status note":                            true,
	"Receipt consumer status note":                       true,
	"Reconcile digest and contested-card status note":    true,
	"Review queue surface-coverage note":                 true,
	"Session lens source note":                           true,
	"Source observation-verdict bridge note":             true,
	"Source-intake pending-extraction note":              true,
	"Spine orientation note":                             true,
	"Synthesis status note":                              true,
	"Work ContextPack status note":                       true,
	"Work close proof note":                              true,
	"Work command status note":                           true,
	"Work dispatch status note":                          true,
	"Work lease note":                                    true,
	"Work smoothness note":                               true,
	"Workbench action-card doctor note":                  true,
	"Workbench admission-preview note":                   true,
	"Workbench capability surface note":                  true,
	"Workbench held-valve note":                          true,
	"Workbench run-artifact note":                        true,
}

var registrySurfaceRE = regexp.MustCompile(`compound://registries/([a-z0-9_-]+)`)

// statusNoteRE captures a "<Label> [status] note:" marker: an uppercase-initial run of clean label
// characters (letters, digits, spaces, '/', '-') ending in "note:". The character class stops at the
// first sentence punctuation (., ;, :, backtick, comma), so it never spans descriptive prose — the
// label is bounded to the immediate marker phrase. Lazy so the leftmost clean run wins.
var statusNoteRE = regexp.MustCompile(`([A-Z][A-Za-z0-9 /-]*?note:)`)

// detectOrgans lists internal/ package directories (the same scope checkClassificationTotal walks).
func detectOrgans(repoRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(repoRoot, "internal"))
	if err != nil {
		return nil, err
	}
	var organs []string
	for _, e := range entries {
		if e.IsDir() {
			organs = append(organs, e.Name())
		}
	}
	sort.Strings(organs)
	return organs, nil
}

// detectRegistrySurfaces returns the unique compound://registries/<name> surfaces referenced in
// tracked non-test Go source (the authoritative public scope: git ls-files, with a filesystem-walk
// fallback for non-git checkouts / temp-dir tests). _test.go is excluded so test fixtures naming
// throwaway registries are not policed as real minted surfaces.
func detectRegistrySurfaces(repoRoot string) ([]string, error) {
	seen := map[string]bool{}
	scan := func(rel string, b []byte) {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return
		}
		for _, m := range registrySurfaceRE.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	if tracked, err := gitTrackedFiles(repoRoot); err == nil {
		for _, rel := range tracked {
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				continue
			}
			b, readErr := os.ReadFile(filepath.Join(repoRoot, rel))
			if readErr != nil {
				continue
			}
			scan(rel, b)
		}
	} else {
		if walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, we error) error {
			if we != nil {
				return nil
			}
			if d.IsDir() {
				if path != repoRoot && privateSourceBodySkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				rel = path
			}
			scan(rel, b)
			return nil
		}); walkErr != nil {
			return nil, walkErr
		}
	}
	return sortedKeys(seen), nil
}

// detectStatusNotes returns the unique "<Label> [status] note:" marker labels (trailing colon
// stripped) in docs/architecture/status.md.
func detectStatusNotes(repoRoot string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "docs", "architecture", "status.md"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range statusNoteRE.FindAllStringSubmatch(string(b), -1) {
		label := strings.TrimSpace(strings.TrimSuffix(m[1], ":"))
		if label != "" {
			seen[label] = true
		}
	}
	return sortedKeys(seen), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mintFindings is the PURE ratchet core: given the detected identifiers for one surface, the
// two-class home registry, and the surface's grandfather set, it returns a Fail for every identifier
// that has NEITHER a grandfather grant NOR a valid two-class home. Pure (no filesystem/process
// access) so the falsifiers are testable without a repo.
func mintFindings(surface string, detected []string, homes map[string]MintHome, grandfather map[string]bool) []Finding {
	var f []Finding
	for _, id := range detected {
		if grandfather[id] {
			continue
		}
		home, ok := homes[surface+":"+id]
		if !ok {
			f = append(f, Finding{"mint-ratchet", Fail, "new " + surface + " " + strconv(id) +
				" has no two-class home — the two-class law (RATIFIED 2026-07-10) requires every minted " + surface +
				" to be either an enforcement point (register arch.mintHomes[" + strconv(surface+":"+id) +
				"] = MintHome{Class: MintEnforcement}) or evidence (MintHome{Class: MintEvidence, Purpose: one plain sentence, Consumer: named consumer}); an unclassified mint dies in the session that minted it"})
			continue
		}
		if !home.Valid() {
			f = append(f, Finding{"mint-ratchet", Fail, "new " + surface + " " + strconv(id) +
				" is minted as " + string(home.Class) + " but its home is incomplete — evidence must carry one plain sentence of Purpose AND a named Consumer, or it decays; an unknown MintClass is not a home"})
		}
	}
	return f
}

// checkMintTwoClassHome runs the mint ratchet across all three build-layer surfaces. It also warns
// (never fails) on stale grandfather entries whose identifier no longer exists, so the dated debt
// list is prunable rather than becoming wallpaper.
func checkMintTwoClassHome(repoRoot string) []Finding {
	var f []Finding
	var detectFailed bool

	organs, err := detectOrgans(repoRoot)
	if err != nil {
		f = append(f, Finding{"mint-ratchet", Warn, "could not detect organs (internal/): " + err.Error()})
		detectFailed = true
	} else {
		f = append(f, mintFindings("organ", organs, mintHomes, grandfatheredOrgans)...)
		f = append(f, staleGrandfather("organ", "arch.grandfatheredOrgans", organs, grandfatheredOrgans)...)
	}

	registries, err := detectRegistrySurfaces(repoRoot)
	if err != nil {
		f = append(f, Finding{"mint-ratchet", Warn, "could not detect registry surfaces: " + err.Error()})
		detectFailed = true
	} else {
		f = append(f, mintFindings("registry", registries, mintHomes, grandfatheredRegistries)...)
		// registry grandfather is scanned from Go source; a name can legitimately disappear as code
		// changes, so no stale warn here (avoids churn noise).
	}

	notes, err := detectStatusNotes(repoRoot)
	if err != nil {
		f = append(f, Finding{"mint-ratchet", Warn, "could not detect status notes (docs/architecture/status.md): " + err.Error()})
		detectFailed = true
	} else {
		f = append(f, mintFindings("status-note", notes, mintHomes, grandfatheredStatusNotes)...)
	}

	hasFail := false
	for _, x := range f {
		if x.Severity == Fail {
			hasFail = true
		}
	}
	if !hasFail && !detectFailed {
		f = append(f, Finding{"mint-ratchet", Info, "every minted organ/registry/status-note carries a two-class home or a dated grandfather grant (two-class law mint gate)"})
	}
	return f
}

// staleGrandfather warns when a grandfather entry names an identifier that no longer exists in the
// tree, so the dated pre-law list can be pruned as debt is paid down. varName names the source map
// so the fix is copy-pasteable.
func staleGrandfather(surface, varName string, detected []string, grandfather map[string]bool) []Finding {
	present := map[string]bool{}
	for _, id := range detected {
		present[id] = true
	}
	var stale []string
	for id := range grandfather {
		if !present[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	var f []Finding
	for _, id := range stale {
		f = append(f, Finding{"mint-ratchet", Warn, "grandfathered " + surface + " " + strconv(id) +
			" no longer exists — drop it from " + varName})
	}
	return f
}
