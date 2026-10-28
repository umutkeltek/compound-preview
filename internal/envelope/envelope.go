// Package envelope is the typed runtime grammar — the spine every harness, hook,
// and run flows through, hung off one root work_id. Every envelope begins with the
// universal Header (which prevents retrieved != relevant != authoritative != current).
// Typed before clever: schemas + validation + round-trip come before behavior.
// See docs/architecture/runtime.md and docs/architecture/contracts/README.md.
package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Schema versions (compound.<kind>.v0.1) and their id prefixes.
const (
	SVEffectiveContractV01 = "compound.effective_contract.v0.1"
	SVEffectiveContract    = "compound.effective_contract.v0.2"
	SVContextRequest       = "compound.context_request.v0.1"
	SVContextPack          = "compound.context_pack.v0.1"
	SVWork                 = "compound.work.v0.1"
	SVSourceEnvelope       = "compound.source_envelope.v0.1"
	SVActionIntent         = "compound.action_intent.v0.1"
	SVPolicyDecision       = "compound.policy_decision.v0.1"
	SVHookEvent            = "compound.hook_event.v0.1"
	SVTraceEnvelope        = "compound.trace_envelope.v0.1"
	SVReceipt              = "compound.receipt.v0.1"
	SVTransition           = "compound.transition.v0.1"
)

// Enum sets — the canonical vocabularies referenced across the docs.
var (
	Privacies         = []string{"public", "private", "sensitive", "secret"}
	Risks             = []string{"tier0", "tier1", "tier2", "tier3"}
	SourceClasses     = []string{"source", "generated", "evidence", "review", "runtime", "private_overlay"}
	AuthorityStatuses = []string{"current", "stale", "disputed", "superseded", "unknown"}
	UncertaintyLevels = []string{"known", "inferred", "suspected", "unknown"}
	WorkStates        = []string{"proposed", "admitted", "ready", "claimed", "running", "verifying", "completed", "held", "blocked", "failed", "cancelled"}
	MutationClasses   = []string{"read", "local_reversible", "external_side_effect", "public", "destructive", "financial", "security"}
	Reversibilities   = []string{"none", "easy", "manual", "hard", "irreversible"}
	PolicyOutcomes    = []string{"allow", "allow_with_obligations", "hold", "deny"}
	MoveTypes         = []string{"outward_delta", "memory_delta", "rule_delta", "control_plane_repair", "audit_only"}
	// DeltaCauses is the causal CLASS of a source-truth change carried on a Transition's optional
	// Cause (see TransitionCause and docs/architecture/transition.md). It lets a future agent choose
	// repair/retirement/acceptance/invalidation instead of re-litigating the old value.
	DeltaCauses = []string{"repair", "retire", "accept", "invalidate"}
	// BranchKinds is the branch-lifecycle enum a Transition's OpensBranches entry may declare
	// (docs/architecture/transition.md future-branch semantics: opened → expanded → resolved →
	// retired). Only validates|supersedes|retires COVER an opened branch; blocks|routes_to are
	// neutral records; a repeated `opens` is an expansion. Coverage is computed by the read
	// model (internal/work BranchCoverage), never by this schema.
	BranchKinds     = []string{"opens", "blocks", "supersedes", "validates", "retires", "routes_to"}
	ProofMedia      = []string{"web-ui", "ios", "cli", "api", "none"}
	ProjectionKinds = []string{"memory", "skill", "product", "cost", "risk", "taste", "routing", "human_attention", "rule", "control_plane"}
	ProjectionRoles = []string{"offered", "used", "missed", "false_triggered", "validated", "candidate", "promoted", "parked", "rejected", "proof"}
	HookNames       = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd", "Stop"}
	HookStatuses    = []string{"ok", "fail_open", "blocked", "error"}
	// FailOpenReasons name WHY a hook failed open. Empty is always legal (pre-existing
	// events state no reason); a non-empty value must be one of these.
	FailOpenReasons = []string{FailOpenCapabilityAbsent, FailOpenContextUnavailable, FailOpenBudgetExceeded}
	ReceiptOutcomes = []string{"completed", "blocked", "failed", "no_op"}
	RequestedModes  = []string{"observe", "plan", "execute", "review"}
	// CloseOutcomes is the CLOSE outcome enum (CloseInput.Outcome in internal/work): the state a
	// work lands in, completed|failed|cancelled. It is DISTINCT from ReceiptOutcomes (the
	// Receipt.Outcome value, mapped from a close outcome via receiptOutcome — e.g.
	// cancelled->no_op) and from DesiredOutcome (free text staked at `create --outcome`). A staked
	// WorkPrediction.Outcome predicts THIS enum, never the other two.
	CloseOutcomes = []string{"completed", "failed", "cancelled"}
	// RouteDeviationRiskGrades enumerates the three admission tiers a RouteDeviation may declare
	// (see the RouteDeviation type). The values mirror attention.RiskGrade{Gate,Judge,Tidy};
	// envelope keeps its OWN copy because internal/attention imports internal/envelope, so
	// envelope importing attention back would cycle.
	RouteDeviationRiskGrades = []string{RouteDeviationGate, RouteDeviationJudge, RouteDeviationTidy}
	// PlanClasses is how much ALIGNMENT a work owes before it may run, in ascending depth:
	// oneshot (the spec is the request), plan_doc (a written plan exists), phased (the work is
	// decomposed and must show a design sketch first). It is a DEPTH dial, not a size estimate
	// -- what scales with it is the review cost the work would otherwise incur downstream. The
	// enforcement that binds today lives in internal/work's design-sketch gate, which refuses a
	// `phased` work into `running` without an attached `design-sketch:` artifact; the other
	// classes are declarative. EMPTY is legal and means oneshot, so every envelope written
	// before this field keeps validating and nothing retroactively becomes gated.
	PlanClasses = []string{"oneshot", "plan_doc", "phased"}
	// ProposalKinds classifies what KIND of system-originated proposal a WorkEnvelope carries.
	// It closes the EVOLVE-BLOCK omission seam: empty is NO LONGER "ordinary" — it means
	// "classification unknown", and a system-originated admission with an unknown
	// classification is denied (recorded), not silently exempted. ordinary work is the
	// explicit declaration; harness_edit is the explicit declaration of a proposed edit at
	// a named editable surface. The discrimination is stamped by the trusted producer
	// (internal/director.director.go), not derived from TargetSurface being empty.
	ProposalKinds = []string{ProposalKindOrdinary, ProposalKindHarnessEdit}
)

// Route-deviation admission grades mirror attention.RiskGrade{Gate,Judge,Tidy}. envelope carries a
// local copy of the string values instead of importing internal/attention because attention already
// imports envelope; importing back would create an import cycle.
const (
	RouteDeviationGate  = "gate"
	RouteDeviationJudge = "judge"
	RouteDeviationTidy  = "tidy"
	// ProposalKind values. EMPTY means "classification unknown" and is refused at the
	// EVOLVE-BLOCK guard for autonomous admissions — see WorkEnvelope.ProposalKind.
	ProposalKindOrdinary    = "ordinary"
	ProposalKindHarnessEdit = "harness_edit"
)

// ContextPack retrieval posture uses the same bounded outcome and error
// vocabulary as the runtime breadth-dependency trace. Compound's ledger remains
// authority in every state; the rented breadth dependency can only sharpen
// ranking. Non-used outcomes therefore name the existing ledger ranker as the
// fallback rather than suggesting a retry or a second retrieval path.
const (
	RetrievalLevelNotAttempted = "not_attempted"
	RetrievalLevelUsed         = "used"
	RetrievalLevelDegraded     = "degraded"
	RetrievalLevelRefused      = "refused"

	RetrievalErrorUnavailable      = "unavailable"
	RetrievalErrorUnsupported      = "unsupported"
	RetrievalErrorEmbedderMismatch = "embedder_mismatch"
	RetrievalErrorProtocolVersion  = "protocol_version"
	RetrievalErrorEngineAhead      = "engine_ahead"
	RetrievalErrorUnownedClaim     = "unowned_claim"
	RetrievalErrorRetrieval        = "retrieval_error"

	RetrievalFallbackLedgerRanker    = "ledger_ranker"
	RetrievalAuthorityCompoundLedger = "compound_ledger"
)

var (
	retrievalLevels = []string{
		RetrievalLevelNotAttempted,
		RetrievalLevelUsed,
		RetrievalLevelDegraded,
		RetrievalLevelRefused,
	}
	retrievalDegradedErrorClasses = []string{
		RetrievalErrorUnavailable,
		RetrievalErrorUnsupported,
		RetrievalErrorRetrieval,
	}
	retrievalRefusedErrorClasses = []string{
		RetrievalErrorEmbedderMismatch,
		RetrievalErrorProtocolVersion,
		RetrievalErrorEngineAhead,
		RetrievalErrorUnownedClaim,
	}
)

// nextConsumerRe: a Transition cause's next_consumer is a consumer slug/ref
// (component or surface id), never prose — no whitespace, lowercase start.
var nextConsumerRe = regexp.MustCompile(`^[a-z][a-z0-9_.:/-]{0,63}$`)

// Retrieval engine identities are opaque bounded slugs, not locations or
// provider payloads. The first character cannot be punctuation; the remaining
// grammar mirrors the runtime trace sanitizer.
var retrievalEngineIdentityRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// capabilitySkillRe is the native Skill tool's bounded identity grammar.
// It excludes paths, URIs, whitespace, and provider payloads.
var capabilitySkillRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// branchSlugRe: an opens_branches branch is a single machine slug (no path
// separators, no prose) — it names a future-branch handle other transitions
// cover by exact slug match.
var branchSlugRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func oneOf(val string, set []string) bool {
	for _, s := range set {
		if val == s {
			return true
		}
	}
	return false
}

// verr accumulates validation problems so a caller sees all of them at once.
type verr struct{ errs []string }

func (v *verr) addf(f string, a ...any) { v.errs = append(v.errs, fmt.Sprintf(f, a...)) }
func (v *verr) req(field, val string) {
	if strings.TrimSpace(val) == "" {
		v.addf("%s is required", field)
	}
}
func (v *verr) enum(field, val string, s []string, required bool) {
	if val == "" {
		if required {
			v.addf("%s is required", field)
		}
		return
	}
	if !oneOf(val, s) {
		v.addf("%s=%q not one of %v", field, val, s)
	}
}
func (v *verr) result(kind string) error {
	if len(v.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s invalid: %s", kind, strings.Join(v.errs, "; "))
}

// --- Universal header ---

type Producer struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version"`
	LaneID  string `json:"lane_id,omitempty"`
}
type IDs struct {
	WorkID        string `json:"work_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	TransitionID  string `json:"transition_id,omitempty"`
	ContextPackID string `json:"context_pack_id,omitempty"`
	ContractID    string `json:"contract_id,omitempty"`
}
type Scope struct {
	EntityID  string `json:"entity_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	ProductID string `json:"product_id,omitempty"`
	Surface   string `json:"surface,omitempty"`
}
type Authority struct {
	SourceClass    string `json:"source_class"`
	Owner          string `json:"owner,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	Status         string `json:"status,omitempty"`
}
type Uncertainty struct {
	Level  string `json:"level"`
	Reason string `json:"reason,omitempty"`
}

// HostBindings is the optional per-event provenance of WHERE the event was captured. It is
// additive: empty is valid; non-empty fields are advisory join keys (e.g., for joining
// ActivityWatch events that fragment across hostnames). Fields are NOT used to drive any
// privacy decision — that stays in Privacy/Risk — only to enable cross-host correlation.
type HostBindings struct {
	// HostID is a stable per-device identifier (AW "device_id", macOS hardware UUID,
	// or another vendor's equivalent). Stable across renames.
	HostID string `json:"host_id,omitempty"`
	// Hostname is the human-readable hostname at capture time. Volatile across renames.
	Hostname string `json:"hostname,omitempty"`
	// DeviceID is the sensor/collector-specific identifier. Distinct from HostID so a
	// watcher can carry both its own identity and the device identity it runs on.
	DeviceID string `json:"device_id,omitempty"`
}

// Validate enforces a sane pattern on HostBindings fields. Empty is valid (HostBindings is
// optional). Non-empty fields must match a hostname-safe pattern: letters, digits, dot,
// underscore, dash. Length capped at 253 chars (DNS hostname max).
func (h HostBindings) Validate() error {
	if err := validateHostIDField("host_bindings.host_id", h.HostID); err != nil {
		return err
	}
	if err := validateHostIDField("host_bindings.hostname", h.Hostname); err != nil {
		return err
	}
	if err := validateHostIDField("host_bindings.device_id", h.DeviceID); err != nil {
		return err
	}
	return nil
}

// hostIDPattern matches what a sane hostname / UUID / vendor-specific id might contain.
// No slashes, no control bytes, no whitespace, no `=` or `;` (which would let a smuggled
// field escape a future shell-style parser).
var hostIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{0,253}$`)

func validateHostIDField(field, val string) error {
	if val == "" {
		return nil
	}
	if !hostIDPattern.MatchString(val) {
		return fmt.Errorf("%s %q malformed (allowed: letters/digits/./_/-, ≤253 chars)", field, val)
	}
	return nil
}

// Header is on every envelope. Anonymous-embedded so its JSON fields flatten to top level.
type Header struct {
	SchemaVersion string        `json:"schema_version"`
	ID            string        `json:"id"`
	CreatedAt     string        `json:"created_at"`
	Producer      Producer      `json:"producer"`
	IDs           IDs           `json:"ids"`
	Scope         Scope         `json:"scope"`
	Authority     Authority     `json:"authority"`
	Host          *HostBindings `json:"host_bindings,omitempty"`
	Privacy       string        `json:"privacy"`
	Risk          string        `json:"risk"`
	EvidenceRefs  []string      `json:"evidence_refs"`
	Uncertainty   Uncertainty   `json:"uncertainty"`
}

func (h Header) validate(v *verr, expectSV, idPrefix string) {
	if h.SchemaVersion != expectSV {
		v.addf("schema_version=%q, want %q", h.SchemaVersion, expectSV)
	}
	v.req("id", h.ID)
	if h.ID != "" && idPrefix != "" && !strings.HasPrefix(h.ID, idPrefix) {
		v.addf("id %q must start with %q", h.ID, idPrefix)
	}
	v.req("created_at", h.CreatedAt)
	v.enum("privacy", h.Privacy, Privacies, true)
	v.enum("risk", h.Risk, Risks, true)
	v.enum("authority.source_class", h.Authority.SourceClass, SourceClasses, true)
	v.enum("authority.status", h.Authority.Status, AuthorityStatuses, false)
	v.enum("uncertainty.level", h.Uncertainty.Level, UncertaintyLevels, false)
	if h.Host != nil {
		if err := h.Host.Validate(); err != nil {
			v.addf("%v", err)
		}
	}
}

// Envelope is any typed runtime object.
type Envelope interface {
	Kind() string
	Validate() error
}

// --- Typed envelopes ---

type MemoryPolicy struct {
	Ambient      []string `json:"ambient"`
	ExplicitOnly []string `json:"explicit_only"`
	Forbidden    []string `json:"forbidden"`
}
type CapabilityPolicy struct {
	ToolsAllowed     []string `json:"tools_allowed"`
	ToolsOnDemand    []string `json:"tools_available_on_demand"`
	MCPsNotByDefault []string `json:"mcps_not_loaded_by_default"`
}
type MutationPolicy struct {
	RequiresActionIntent      bool `json:"requires_action_intent"`
	IrreversibleRequiresHuman bool `json:"irreversible_requires_human"`
}

// TaskContract is the provider- and harness-neutral meaning of one task. It is
// a value carried by the existing envelope spine, not a new envelope or source
// of identity; concrete tools, models, and routes remain compiled outputs.
type TaskContract struct {
	Intent           string   `json:"intent,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	DesiredOutcome   string   `json:"desired_outcome,omitempty"`
	SuccessPredicate string   `json:"success_predicate,omitempty"`
	NonGoals         []string `json:"non_goals,omitempty"`
	ProofMedium      string   `json:"proof_medium,omitempty"`
}

// IsZero lets additive task contracts remain absent on legacy v0.1 artifacts.
func (t TaskContract) IsZero() bool {
	return strings.TrimSpace(t.Intent) == "" &&
		strings.TrimSpace(t.Mode) == "" &&
		strings.TrimSpace(t.DesiredOutcome) == "" &&
		strings.TrimSpace(t.SuccessPredicate) == "" &&
		len(t.NonGoals) == 0 &&
		strings.TrimSpace(t.ProofMedium) == ""
}

func (t TaskContract) Validate() error {
	v := &verr{}
	validateTaskContract(v, "task_contract", t)
	return v.result("task_contract")
}

func validateTaskContract(v *verr, field string, task TaskContract) {
	v.enum(field+".mode", task.Mode, RequestedModes, false)
	v.enum(field+".proof_medium", task.ProofMedium, ProofMedia, false)
}

type HookReadiness struct {
	Harness         string                `json:"harness"`
	Hook            string                `json:"hook"`
	NativeBlock     bool                  `json:"native_block"`
	Installed       bool                  `json:"installed"`
	InstalledRef    string                `json:"installed_ref,omitempty"`
	Status          string                `json:"status"`
	AdapterRequired bool                  `json:"adapter_required,omitempty"`
	AdapterCommand  string                `json:"adapter_command,omitempty"`
	Reason          string                `json:"reason,omitempty"`
	Actions         []HookReadinessAction `json:"actions,omitempty"`
}

type HookReadinessAction struct {
	Kind             string `json:"kind"`
	Label            string `json:"label"`
	Command          string `json:"command,omitempty"`
	ExpectedExitCode int    `json:"expected_exit_code,omitempty"`
	ExpectedStatus   string `json:"expected_status,omitempty"`
	Boundary         string `json:"boundary,omitempty"`
	Proof            string `json:"proof,omitempty"`
	Rollback         string `json:"rollback,omitempty"`
	RequiresOperator bool   `json:"requires_operator,omitempty"`
}

type HookRosterEntry struct {
	Ref     string `json:"ref"`
	Harness string `json:"harness"`
	Hook    string `json:"hook"`
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

type SourceRev struct {
	Source string `json:"source"`
	Rev    string `json:"rev"`
}
type EffectiveContract struct {
	Header
	// ContextKey identifies the canonical orientation request, not its mutable contents.
	ContextKey         string            `json:"context_key,omitempty"`
	Harness            string            `json:"harness"`
	TaskMode           string            `json:"task_mode,omitempty"`
	TaskContract       TaskContract      `json:"task_contract,omitzero"`
	ActiveInstructions []string          `json:"active_instructions"`
	ActiveHooks        []string          `json:"active_hooks"`
	HookRoster         []HookRosterEntry `json:"hook_roster,omitempty"`
	HookReadiness      []HookReadiness   `json:"hook_readiness,omitempty"`
	ActiveDecisions    []string          `json:"active_decisions"`
	ActiveMemory       []string          `json:"active_memory,omitempty"`
	Excluded           []ExcludedRef     `json:"excluded"`
	CapabilityOffers   []CapabilityOffer `json:"capability_offers,omitempty"`
	MemoryPolicy       MemoryPolicy      `json:"memory_policy"`
	CapabilityPolicy   CapabilityPolicy  `json:"capability_policy"`
	MutationPolicy     MutationPolicy    `json:"mutation_policy"`
	SourceRevisions    []SourceRev       `json:"source_revisions"`
	Unknowns           []string          `json:"unknowns"`
	Conflicts          []string          `json:"conflicts"`
}

func (e *EffectiveContract) Kind() string { return "effective_contract" }
func (e *EffectiveContract) Validate() error {
	v := &verr{}
	version := SVEffectiveContract
	if e.SchemaVersion == SVEffectiveContractV01 {
		version = SVEffectiveContractV01
	}
	e.Header.validate(v, version, "contract_")
	if err := e.ValidateSchemaVersion(); err != nil {
		v.addf("%v", err)
	}
	v.req("harness", e.Harness)
	validateTaskContract(v, "task_contract", e.TaskContract)
	if taskMode, contractMode := strings.TrimSpace(e.TaskMode), strings.TrimSpace(e.TaskContract.Mode); taskMode != "" && contractMode != "" && taskMode != contractMode {
		v.addf("task_mode %q conflicts with task_contract.mode %q", taskMode, contractMode)
	}
	validateCapabilityOffers(v, "capability_offers", e.CapabilityOffers)
	validateUniqueSourceRevisions(v, e.SourceRevisions)
	return v.result(e.Kind())
}

type CtxConstraints struct {
	MaxContextChars         int  `json:"max_context_chars"`
	AllowPrivateMemory      bool `json:"allow_private_memory"`
	AllowRawCorpus          bool `json:"allow_raw_corpus"`
	AllowProvisionalReviews bool `json:"allow_provisional_reviews"`
}
type ContextRequest struct {
	Header
	IntentTextRef string         `json:"intent_text_ref,omitempty"`
	Harness       string         `json:"harness"`
	Cwd           string         `json:"cwd,omitempty"`
	TaskGuess     string         `json:"task_guess,omitempty"`
	RequestedMode string         `json:"requested_mode,omitempty"`
	Constraints   CtxConstraints `json:"constraints"`
}

func (e *ContextRequest) Kind() string { return "context_request" }
func (e *ContextRequest) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVContextRequest, "ctxreq_")
	v.req("harness", e.Harness)
	v.enum("requested_mode", e.RequestedMode, RequestedModes, false)
	return v.result(e.Kind())
}

type IncludedRef struct {
	Ref         string  `json:"ref"`
	SourceClass string  `json:"source_class"`
	Reason      string  `json:"reason"`
	Confidence  float64 `json:"confidence"`
	Status      string  `json:"status"`
}
type ExcludedRef struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}
type CapabilityOfferConstraint struct {
	Kind       string  `json:"kind"`
	Ref        string  `json:"ref"`
	Detail     string  `json:"detail,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}
type CapabilityOffer struct {
	Ref                   string                      `json:"ref"`
	Kind                  string                      `json:"kind"`
	Name                  string                      `json:"name"`
	Score                 float64                     `json:"score"`
	Why                   string                      `json:"why"`
	Required              []CapabilityOfferConstraint `json:"required,omitempty"`
	Conflicts             []CapabilityOfferConstraint `json:"conflicts,omitempty"`
	Harnesses             []CapabilityOfferConstraint `json:"harnesses,omitempty"`
	KnownMissedWhen       []CapabilityOfferConstraint `json:"known_missed_when,omitempty"`
	KnownFalseTriggeredBy []CapabilityOfferConstraint `json:"known_false_triggered_by,omitempty"`
	Evidence              []CapabilityOfferConstraint `json:"evidence,omitempty"`
	ConnectionSurfaces    []CapabilityOfferConstraint `json:"connection_surfaces,omitempty"`
	CostRiskNote          string                      `json:"cost_risk_note,omitempty"`
}

// ActivePosition is the folded current stance digest rendered into ContextPack
// active memory. Line is the canonical compact shape hooks display:
// position-id [predicate=P, claim_type=T] → stance (since-seq N, chain-length L)
// [contested]. Ref carries narrow fact-slot identity while Topic remains the
// broad aggregation axis.
type ActivePosition struct {
	Ref         string `json:"ref"`
	Entity      string `json:"entity"`
	Topic       string `json:"topic"`
	Stance      string `json:"stance"`
	SinceSeq    int64  `json:"since_seq"`
	ChainLength int    `json:"chain_length"`
	Contested   bool   `json:"contested,omitempty"`
	Line        string `json:"line"`
}

// NegativeCard is one already-tried-and-failed approach surfaced at orient time,
// scoped to the incoming task (compound.negatives.v0.1 projection —
// internal/negatives, joined over inbox/failure-patterns + reviews/route-remorse).
// Display-only: read by ContextPack consumers, never by route/dispatch/claim
// decisions. WhatWasTried/WhyItFailed are reduced to "" at build time when the
// source free text looked operator-personal — ApproachKey and Ref remain so the
// card still cites its source.
type NegativeCard struct {
	Source       string  `json:"source"` // "failure_pattern" | "route_remorse"
	ApproachKey  string  `json:"approach_key"`
	WhatWasTried string  `json:"what_was_tried,omitempty"`
	WhyItFailed  string  `json:"why_it_failed,omitempty"`
	Ref          string  `json:"ref"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// BodyPackSection is the additive, OPTIONAL body-delivery section of a ContextPack
// (compound.context_pack.v0.1): task-ranked full content for candidates a body
// resolver could materialize, packed greedily by rank under a token budget, with
// per-item provenance and an explicit omitted ledger. Mirrors the Peers *PeersSection
// precedent: pointer-typed with omitempty, so a pack WITHOUT a body_pack key is
// byte-identical to today (feature off when --body-budget is absent or 0).
//
// LAWS the section carries (load-bearing, pinned by internal/contract materialize
// falsifiers + internal/hook injection-blindness test):
//
//   - A ref in the pack's Excluded list NEVER materializes a body.
//   - A private-overlay IncludedRef (SourceClass == "private_overlay") NEVER
//     materializes a body, regardless of budget — the existing private-overlay law
//     stands verbatim ("refs only, no private body injection").
//   - Whole-item-or-omit: no truncation; an item larger than remaining budget lands
//     in omitted with reason budget_exhausted. Admission is strict greedy-by-rank:
//     each item is independently admitted if it fits the REMAINING budget.
//   - Hook injection NEVER carries bodies. renderInjection
//     (internal/hook/hook.go) ignores this section entirely; bodies are for direct
//     pack consumers (pack JSON under $COMPOUND_HOME/context/packs/, orient stdout,
//     dispatch prompts).
//
// Schema shape: { budget_tokens, estimated_used_tokens, bodies: [{ref, source, rank,
// estimated_tokens, sha256, status, text}], omitted: [{ref, rank, estimated_tokens,
// reason}] }. Validation does NOT require the section (F4).
type BodyPackSection struct {
	BudgetTokens        int                `json:"budget_tokens"`
	EstimatedUsedTokens int                `json:"estimated_used_tokens"`
	Bodies              []BodyPackItem     `json:"bodies"`
	Omitted             []BodyPackOmission `json:"omitted"`
}

// BodyPackItem is one whole candidate body admitted under the budget.
type BodyPackItem struct {
	Ref             string  `json:"ref"`
	Source          string  `json:"source"`
	Rank            float64 `json:"rank"`
	EstimatedTokens int     `json:"estimated_tokens"`
	SHA256          string  `json:"sha256"`
	Status          string  `json:"status"`
	Text            string  `json:"text"`
}

// BodyPackOmission records one candidate NOT packed and why. The reason set is an
// open enum; v1 values emitted by the materializer: budget_exhausted,
// suppressed_private, unresolvable, excluded.
type BodyPackOmission struct {
	Ref             string  `json:"ref"`
	Rank            float64 `json:"rank"`
	EstimatedTokens int     `json:"estimated_tokens"`
	Reason          string  `json:"reason"`
}

// PeersSection is the CONTEXTUAL team-of-lanes view attached to a ContextPack at
// orient time: the active leases (who is holding which work, with deterministic
// staleness) and capped agent-lane cards read from the capability graph. It is
// DISPLAY-ONLY (ratified agent-lane-beliefs Q4): it never influences routing,
// dispatch, or pack-assembly weighting, and building it never writes a capability
// graph or memory byte. It renders ONLY in the orient-time pack — never ambient,
// never a console/doctor dump of its contents (ratified Q2). v1 carries leases +
// lane cards; belief-claim cards join here in slice B (agent-lane beliefs).
type PeersSection struct {
	ActiveLeases []PeerLease `json:"active_leases"`
	// ActiveLeasesOmitted counts the leases the cap dropped from ActiveLeases. The
	// list is bounded (a home accumulates leases without bound), so the count is what
	// keeps the truncation honest: a reader can tell "these are all the leases" from
	// "these are the newest N of M" instead of silently reading a capped list as total.
	ActiveLeasesOmitted int            `json:"active_leases_omitted,omitempty"`
	Lanes               []PeerLaneCard `json:"lanes"`
}

// PeerLease is one work lease surfaced to peers: the work id, the holder, a coarse
// age, and a deterministic staleness flag. Staleness v1 is expiry-based only (age
// past a fixed TTL from claimed_at); process/session liveness is out of v1, so a
// dead-but-unexpired holder renders active — acceptable and documented.
type PeerLease struct {
	WorkID string `json:"work_id"`
	Holder string `json:"holder"`
	Age    string `json:"age"`
	Stale  bool   `json:"stale"`
}

// PeerLaneCard is one agent-lane's evidence-only capability card: its lane identity,
// aliased harness, top capability domains, and tendency slugs — all read from slice
// A's capability-graph edges. Display-only; carries no reputation score and drives
// no routing.
type PeerLaneCard struct {
	Lane            string   `json:"lane"`
	Harness         string   `json:"harness"`
	TopCapabilities []string `json:"top_capabilities"`
	Tendencies      []string `json:"tendencies"`
}

// RetrievalPosture is bounded, machine-readable observability for the single
// breadth dependency used while assembling a ContextPack. It deliberately has
// no room for queries, raw errors, provider payloads, endpoints, or retry advice.
type RetrievalPosture struct {
	Level           string `json:"level"`
	Fallback        string `json:"fallback,omitempty"`
	Authority       string `json:"authority"`
	RequestedEngine string `json:"requested_engine,omitempty"`
	ObservedEngine  string `json:"observed_engine,omitempty"`
	ErrorClass      string `json:"error_class,omitempty"`
}

type ContextPack struct {
	Header
	SummaryMD          string            `json:"summary_md"`
	TaskContract       TaskContract      `json:"task_contract,omitzero"`
	Included           []IncludedRef     `json:"included"`
	Excluded           []ExcludedRef     `json:"excluded"`
	CapabilityOffers   []CapabilityOffer `json:"capability_offers,omitempty"`
	ActiveInstructions []string          `json:"active_instructions"`
	ActiveHooks        []string          `json:"active_hooks"`
	HookRoster         []HookRosterEntry `json:"hook_roster,omitempty"`
	HookReadiness      []HookReadiness   `json:"hook_readiness,omitempty"`
	ActiveDecisions    []string          `json:"active_decisions"`
	ActiveMemory       []string          `json:"active_memory,omitempty"`
	ActivePositions    []ActivePosition  `json:"active_positions,omitempty"`
	Negatives          []NegativeCard    `json:"negatives,omitempty"`
	Peers              *PeersSection     `json:"peers,omitempty"`
	SourceRevisions    []SourceRev       `json:"source_revisions,omitempty"`
	RequiredChecks     []string          `json:"required_checks"`
	Avoid              []string          `json:"avoid"`
	BodyPack           *BodyPackSection  `json:"body_pack,omitempty"`
	Retrieval          *RetrievalPosture `json:"retrieval,omitempty"`
}

func (e *ContextPack) Kind() string { return "context_pack" }
func (e *ContextPack) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVContextPack, "ctx_")
	validateTaskContract(v, "task_contract", e.TaskContract)
	for i, inc := range e.Included {
		v.enum(fmt.Sprintf("included[%d].source_class", i), inc.SourceClass, SourceClasses, true)
		v.enum(fmt.Sprintf("included[%d].status", i), inc.Status, AuthorityStatuses, false)
		v.req(fmt.Sprintf("included[%d].ref", i), inc.Ref)
	}
	validateCapabilityOffers(v, "capability_offers", e.CapabilityOffers)
	validateUniqueSourceRevisions(v, e.SourceRevisions)
	if e.Retrieval != nil {
		validateRetrievalPosture(v, *e.Retrieval)
	}
	// Present sections are shape-checked (the admission LAWS — no private
	// bodies, excluded refs never materialize — stay in the materializer; the
	// schema owns only that a hand-written pack cannot claim empty refs).
	for i, pos := range e.ActivePositions {
		v.req(fmt.Sprintf("active_positions[%d].ref", i), pos.Ref)
		v.req(fmt.Sprintf("active_positions[%d].entity", i), pos.Entity)
	}
	for i, neg := range e.Negatives {
		v.req(fmt.Sprintf("negatives[%d].approach_key", i), neg.ApproachKey)
		v.req(fmt.Sprintf("negatives[%d].ref", i), neg.Ref)
	}
	if e.Peers != nil {
		for i, lease := range e.Peers.ActiveLeases {
			v.req(fmt.Sprintf("peers.active_leases[%d].work_id", i), lease.WorkID)
			v.req(fmt.Sprintf("peers.active_leases[%d].holder", i), lease.Holder)
		}
	}
	if e.BodyPack != nil {
		for i, body := range e.BodyPack.Bodies {
			v.req(fmt.Sprintf("body_pack.bodies[%d].ref", i), body.Ref)
			v.req(fmt.Sprintf("body_pack.bodies[%d].sha256", i), body.SHA256)
			v.req(fmt.Sprintf("body_pack.bodies[%d].status", i), body.Status)
		}
		for i, om := range e.BodyPack.Omitted {
			v.req(fmt.Sprintf("body_pack.omitted[%d].ref", i), om.Ref)
			v.req(fmt.Sprintf("body_pack.omitted[%d].reason", i), om.Reason)
		}
	}
	// the excluded list (what was NOT loaded, and why) is the whole point of replaceability;
	// require its presence (may be empty) so it's never silently dropped.
	if e.Excluded == nil {
		v.addf("excluded must be present (record what was not loaded and why)")
	}
	return v.result(e.Kind())
}

func validateRetrievalPosture(v *verr, posture RetrievalPosture) {
	v.enum("retrieval.level", posture.Level, retrievalLevels, true)
	if posture.Authority != RetrievalAuthorityCompoundLedger {
		v.addf("retrieval.authority must be %q", RetrievalAuthorityCompoundLedger)
	}
	if posture.RequestedEngine != "" && !retrievalEngineIdentityRe.MatchString(posture.RequestedEngine) {
		v.addf("retrieval.requested_engine must be a 1-64 character lowercase engine identifier")
	}
	if posture.ObservedEngine != "" && !retrievalEngineIdentityRe.MatchString(posture.ObservedEngine) {
		v.addf("retrieval.observed_engine must be a 1-64 character lowercase engine identifier")
	}

	requireLedgerFallback := func() {
		if posture.Fallback != RetrievalFallbackLedgerRanker {
			v.addf("retrieval.fallback must be %q for non-used outcomes", RetrievalFallbackLedgerRanker)
		}
	}

	switch posture.Level {
	case RetrievalLevelUsed:
		v.req("retrieval.requested_engine", posture.RequestedEngine)
		v.req("retrieval.observed_engine", posture.ObservedEngine)
		if posture.Fallback != "" {
			v.addf("retrieval level used must not carry fallback")
		}
		if posture.ErrorClass != "" {
			v.addf("retrieval level used must not carry error_class")
		}
	case RetrievalLevelDegraded:
		requireLedgerFallback()
		v.req("retrieval.requested_engine", posture.RequestedEngine)
		if !oneOf(posture.ErrorClass, retrievalDegradedErrorClasses) {
			v.addf("retrieval level degraded requires a degradable error_class")
		}
	case RetrievalLevelRefused:
		requireLedgerFallback()
		v.req("retrieval.requested_engine", posture.RequestedEngine)
		if !oneOf(posture.ErrorClass, retrievalRefusedErrorClasses) {
			v.addf("retrieval level refused requires an integrity-refusal error_class")
		}
	case RetrievalLevelNotAttempted:
		requireLedgerFallback()
		if posture.ObservedEngine != "" {
			v.addf("retrieval level not_attempted must not carry observed_engine")
		}
		if posture.ErrorClass != "" {
			v.addf("retrieval level not_attempted must not carry error_class")
		}
	}
}

func validateUniqueSourceRevisions(v *verr, revisions []SourceRev) {
	seen := map[string]bool{}
	for _, revision := range revisions {
		source := strings.TrimSpace(revision.Source)
		if source != "" && seen[source] {
			v.addf("duplicate source revision %q", source)
		}
		seen[source] = true
	}
}

func validateCapabilityOffers(v *verr, field string, values []CapabilityOffer) {
	for i, offer := range values {
		prefix := fmt.Sprintf("%s[%d]", field, i)
		v.req(prefix+".ref", offer.Ref)
		v.req(prefix+".kind", offer.Kind)
		v.req(prefix+".name", offer.Name)
		validateCapabilityOfferConstraints(v, prefix+".required", offer.Required)
		validateCapabilityOfferConstraints(v, prefix+".conflicts", offer.Conflicts)
		validateCapabilityOfferConstraints(v, prefix+".harnesses", offer.Harnesses)
		validateCapabilityOfferConstraints(v, prefix+".known_missed_when", offer.KnownMissedWhen)
		validateCapabilityOfferConstraints(v, prefix+".known_false_triggered_by", offer.KnownFalseTriggeredBy)
		validateCapabilityOfferConstraints(v, prefix+".evidence", offer.Evidence)
		validateCapabilityOfferConstraints(v, prefix+".connection_surfaces", offer.ConnectionSurfaces)
	}
}

func validateCapabilityOfferConstraints(v *verr, field string, values []CapabilityOfferConstraint) {
	for i, c := range values {
		prefix := fmt.Sprintf("%s[%d]", field, i)
		v.req(prefix+".kind", c.Kind)
		v.req(prefix+".ref", c.Ref)
	}
}

// GitDeliveryContract binds source-controlled completion to one logical
// repository, one exact local target, and one publication policy. The root is
// only a locator; RepositoryFingerprint proves which repository occupies it.
// Land-check callers may assert the contract, but may never replace it.
type GitDeliveryContract struct {
	TargetWorktreeRoot    string `json:"target_worktree_root"`
	RepositoryFingerprint string `json:"repository_fingerprint,omitempty"`
	TargetLocalRef        string `json:"target_local_ref"`
	ExpectedBaseSHA       string `json:"expected_base_sha"`
	PublicationMode       string `json:"publication_mode"`
	PublicationRef        string `json:"publication_ref,omitempty"`
}

// WorkOrigin separates accountable ownership from the executor identity that
// created the work. Owner remains the legacy lifecycle label; this additive
// record answers who is accountable, which harness/model authored the intent,
// and which lane did so without pretending that requested model identity is
// observed provider execution.
type WorkOrigin struct {
	AccountableOwner string `json:"accountable_owner"`
	Actor            string `json:"actor"`
	Harness          string `json:"harness"`
	RequestedModel   string `json:"requested_model"`
	LaneID           string `json:"lane_id"`
	IdentitySource   string `json:"identity_source"`
}

type WorkEnvelope struct {
	Header
	Owner            string `json:"owner"`
	CanonicalSurface string `json:"canonical_surface,omitempty"`
	// TargetSurface, when set, marks this work as an explicit harness-edit proposal
	// aimed at a named editable surface (EVOLVE-BLOCK). It is OPTIONAL and additive: work
	// without it is ordinary work. A system-originated proposal that declares a
	// TargetSurface is admitted only while that surface is actively whitelisted in the
	// policy-owned editable-surfaces registry (see internal/policy/editable_surfaces.go).
	TargetSurface string `json:"target_surface,omitempty"`
	// ProposalKind classifies what KIND of system-originated proposal this work carries.
	// EMPTY is reserved for unknown and is REFUSED at the EVOLVE-BLOCK guard for autonomous
	// admissions; ordinary work and harness_edit work are explicit declarations the trusted
	// producer stamps (internal/director). The discrimination closes the omission seam:
	// autonomous proposal without a ProposalKind is denied as unclassified, not bypassed.
	ProposalKind string `json:"proposal_kind,omitempty"`
	// ExpectedBaseSHA is the legacy, incomplete pre-contract field. It remains
	// readable so existing envelopes do not become corrupt, but new admissions
	// must use GitDelivery and completed commit-bearing work fails closed without it.
	ExpectedBaseSHA   string                  `json:"expected_base_sha,omitempty"`
	GitDelivery       *GitDeliveryContract    `json:"git_delivery,omitempty"`
	NonSourceDelivery []ActionDeliveryBinding `json:"non_source_delivery,omitempty"`
	State             string                  `json:"state"`
	DesiredOutcome    string                  `json:"desired_outcome,omitempty"`
	SuccessPredicate  string                  `json:"success_predicate"`
	NonGoals          []string                `json:"non_goals"`
	ProofMedium       string                  `json:"proof_medium,omitempty"`
	RouteDecisionID   string                  `json:"route_decision_id,omitempty"`
	PolicyDecisionID  string                  `json:"policy_decision_id,omitempty"`
	// ActionIntentIDs holds ActionIntent ids (act_*), NOT run identifiers. The wire name
	// `run_ids` is a LEGACY MISNOMER retained deliberately: the canonical work hash is
	// sha256(json.Marshal(WorkEnvelope)) (see internal/work.workEnvelopeSHA256), and that hash is
	// internal/routeevidence/autonomy/proof.go hard-fails on the mismatch. The key moves to
	// `action_intent_ids` only at the compound.work.v0.2 schema bump, where new envelopes carry the
	// new shape and v0.1 envelopes keep theirs, so continuity is preserved per-version rather than
	// rewritten. Do not "fix" this tag in isolation.
	//
	// v0.1 SEMANTIC MIX (RETIRED by S1 -- historical only): before S1, WorkStore.Advance appended
	// AdvanceInput.RunID into this slice, so a v0.1 envelope could carry `act_*` ids AND legacy
	// run refs (today the only contributor was panel apply's `prun_*` ids from
	// internal/panel/apply.go). The wire-name legacy above silently covered the mix because the
	// field is `[]string` with no prefix validation. S1 retired AdvanceInput.RunID entirely; the
	// panel apply path now mints a RunAttempt via internal/runattempt and attaches the `ratt_*`
	// id on RunAttemptIDs, never on this slot. ActionIntentIDs is act_*-only post-S1. The retired
	// v0.1 mix is no longer pinned by any test -- the surviving pins below all assert the new
	// shape (ActionIntentIDs holds only the Advance's internal act_* id; RunAttemptIDs holds the
	// minted ratt_* id):
	//   - TestAdvanceRecordsPolicyAction
	//   - TestActivePacketProofReportsClosedPacketChain
	//   - TestActivePacketProofReportsArchivedClosedPacketChain
	// Do not reintroduce AdvanceInput.RunID. The dedicated slot exists for a reason -- a single
	// field holding two identity classes is the exact bug that produced the v0.1 mix.
	ActionIntentIDs []string `json:"run_ids"`
	// RunAttemptIDs carries RunAttempt ids (ratt_*). S1 introduces internal/runattempt as the
	// sole writer; WorkStore.Advance attaches RunAttempt ids via AdvanceInput.RunAttemptIDs,
	// and envelope.validateRunAttemptIDs enforces the ratt_* prefix so legacy prun_*/run_*
	// ids cannot leak back into the slot. Hash-neutral when empty (`omitempty`), so introducing
	// the populated form here does not invalidate stored work_sha256.
	RunAttemptIDs      []string `json:"run_attempt_ids,omitempty"`
	ArtifactIDs        []string `json:"artifact_ids"`
	VerificationIDs    []string `json:"verification_ids"`
	MemoryCandidateIDs []string `json:"memory_candidate_ids"`
	// Prediction is an OPTIONAL, additive stake of how this work is expected to CLOSE, set at
	// CREATE time only (v1: no restaking at Claim/Advance -- Claim carries no payload to widen).
	// A work without it is ordinary work; close-time mechanically diffs it against the actual
	// close and bakes a SurpriseSignal into the Receipt (see Receipt.Surprise, and
	// internal/work's surpriseOnClose). It is the harness-layer analog of a prediction-error
	// signal: compound has a policy (orient/routing), a perception (ContextPack), and a
	// transition model (the ledger) -- staking + diffing a prediction is what lets prediction
	// error accumulate as adjudicable evidence.
	Prediction *WorkPrediction `json:"prediction,omitempty"`
	// RouteDeviation, when set, marks this work as an explicit deviation from the served policy
	// line (compound route plan) for a capability class. It is OPTIONAL and additive: work WITHOUT
	// it is ordinary work whose admission is unchanged, and every existing envelope/fixture
	// re-marshals byte-identically because the field is nil + `omitempty`. When present it is the
	// MECHANICAL deviation signal admission binds on -- see the RouteDeviation type and
	// validateRouteDeviation for the fail-closed rule (a judge-grade deviation stakes a Prediction
	// or is refused). Declared LAST so the additive field never reorders existing marshaled bytes.
	RouteDeviation *RouteDeviation `json:"route_deviation,omitempty"`
	// PlanClass declares how much alignment this work owes before it may run: one of
	// PlanClasses (oneshot | plan_doc | phased). It is OPTIONAL and additive -- EMPTY means
	// oneshot, so every envelope that predates the field keeps validating and re-marshals
	// byte-identically (nothing retroactively becomes gated, mirroring freeze's "absent means
	// nothing gated"). Validate enforces the vocabulary so an unrecognized free string can
	// never reach disk and become a silently ungated class. The only class that gates today is
	// `phased`, refused into `running` by internal/work's design-sketch guard without an
	// attached `design-sketch:` artifact. Declared LAST, after RouteDeviation, so the additive
	// field never reorders existing marshaled bytes (see the workEnvelopeSHA256 discipline on
	// ActionIntentIDs above).
	PlanClass string `json:"plan_class,omitempty"`
	// ReworkOfWorkID records the LINEAGE EDGE "this work exists because an earlier landed work
	// broke something" -- the input a rework-rate fold (per class / lane / plan_class) is
	// computed from later. It is OPTIONAL and additive: empty means "not a rework", every
	// envelope that predates the field re-marshals byte-identically, and no schema version
	// moves.
	//
	// Set at CREATE time only and immutable thereafter: immutability comes from "set once",
	// like a content-addressed finding, not from a guard on a mutation path -- there is no
	// attach-at-advance route to police. A later need to attach post-create is a separate
	// change mirroring runattempt.SetPredecessor's append-only / refuse-different semantics.
	//
	// NAMING: this is deliberately NOT `caused_by`. Transition.Cause.CausedByRefs is an
	// unrelated, shipped canon-evolution mechanism with its own consumer (internal/mem's cause
	// consumption); reusing that name here would collapse two different meanings into one
	// searchable term.
	//
	// Validate enforces the shape (work_ prefix, single path element, not the work_live_
	// namespace, not self); internal/work's admission additionally proves the target RESOLVES
	// in the store, so the edge can never point at a phantom. Declared LAST, after PlanClass,
	// so the additive field never reorders existing marshaled bytes (see the workEnvelopeSHA256
	// discipline on ActionIntentIDs above).
	ReworkOfWorkID string `json:"rework_of_work_id,omitempty"`
	// Origin is populated for newly built work. It is optional so pre-origin
	// v0.1 envelopes remain valid and re-marshal byte-identically.
	Origin *WorkOrigin `json:"origin,omitempty"`
	// ExecutionSelection is the work-level saved harness/route/model/effort
	// choice (see execution_selection.go). Optional and declared LAST so
	// pre-selection envelopes re-marshal byte-identically.
	ExecutionSelection *ExecutionSelection `json:"execution_selection,omitempty"`
}

// WorkPrediction is staked at create time and predicts the CLOSE outcome enum
// (CloseOutcomes: completed|failed|cancelled -- CloseInput.Outcome in internal/work), NEVER
// Receipt.Outcome (mapped via receiptOutcome, e.g. cancelled->no_op) and NEVER DesiredOutcome
// (free text staked via `create --outcome`). Naming avoids both scouted landmines: "Expected*"
// already means the legacy git-delivery contract fields, and "outcome" is triply overloaded
// across this envelope/Receipt pair.
type WorkPrediction struct {
	// Outcome is REQUIRED whenever Prediction is present; must be one of CloseOutcomes.
	Outcome string `json:"outcome"`
	// MoveType is OPTIONAL; when set, must be one of MoveTypes. Left empty, the move_type
	// dimension is simply not staked and is never compared at close (an unstaked dimension
	// cannot itself cause a divergence).
	MoveType string `json:"move_type,omitempty"`
	// Note is optional free text (e.g. why this outcome/move_type is expected).
	Note string `json:"note,omitempty"`
}

// RouteDeviation marks a WorkEnvelope as an explicit deviation from the served policy line
// (compound route plan) for a capability class -- the executor is choosing a lane the served
// routing did NOT pick, on receipt-fed belief that the deviation lane is better here.
//
// It is deliberately INDEPENDENT of the EV card (compound.route.evcard.v0.1). The card is advisory
// evidence a renderer may show a human; admission MUST NOT depend on it. If the refusal keyed on
// the card's presence, then simply omitting the card would become the bypass -- so CardRef is a
// free-standing pointer here that validateRouteDeviation never reads. The fail-closed admission
// rule binds on deviation-ness (this marker present) + RiskGrade ALONE.
type RouteDeviation struct {
	// Lane is REQUIRED: the deviation lane id chosen instead of the served policy line.
	Lane string `json:"lane"`
	// CapabilityClass is OPTIONAL: the class the deviation is priced under (echoes the EV card's
	// capability_class). Carried for legibility; it never gates admission.
	CapabilityClass string `json:"capability_class,omitempty"`
	// CardRef is OPTIONAL: a pointer to the advisory EV card that priced this deviation. Evidence
	// ONLY -- admission never consults it, so a missing card is never a bypass.
	CardRef string `json:"card_ref,omitempty"`
	// RiskGrade is OPTIONAL on the wire but load-bearing for admission: gate|judge|tidy, mirroring
	// attention.RiskGrade* (internal/attention owns the canonical three-tier). An envelope-level
	// grade is NOT derivable from the envelope alone at create time -- attention.Class derives from
	// a Group's BlastRadius/Reversibility/Kind, which a bare WorkEnvelope does not carry -- so the
	// grade is stated explicitly here. An EMPTY grade resolves to `judge`, never `tidy`
	// (effectiveRouteDeviationGrade): an ungraded deviation is the fail-closed case, and defaulting
	// it to the strong rule (stake a prediction or be refused) is the safe choice when the caller
	// did not declare how much downside the deviation carries. Defaulting an unstated grade to the
	// ungated `tidy` tier would let an omitted flag silently bypass the stake.
	RiskGrade string `json:"risk_grade,omitempty"`
}

func (e *WorkEnvelope) Kind() string { return "work" }
func (e *WorkEnvelope) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVWork, "work_")
	if strings.HasPrefix(e.ID, "work_live_") {
		v.addf("id %q occupies the ephemeral %q namespace (reserved for unattributed live-hook mutations); a durable work envelope must mint a durable id", e.ID, "work_live_")
	}
	v.enum("state", e.State, WorkStates, true)
	v.enum("proof_medium", e.ProofMedium, ProofMedia, false)
	// Optional: empty is legal and means oneshot (see PlanClasses / PlanClass).
	v.enum("plan_class", e.PlanClass, PlanClasses, false)
	// Optional: empty is reserved for "unknown" and is REFUSED at the EVOLVE-BLOCK guard
	// for autonomous admissions (see internal/work.editableSurfaceGuard). An unrecognized
	// non-empty value is a malformed envelope and is refused here so producers cannot
	// smuggle arbitrary strings into the discriminated field.
	v.enum("proposal_kind", e.ProposalKind, ProposalKinds, false)
	v.req("owner", e.Owner)
	v.req("success_predicate", e.SuccessPredicate)
	if base := strings.TrimSpace(e.ExpectedBaseSHA); base != "" && !validFullGitObjectID(base) {
		v.addf("expected_base_sha must be a lowercase full Git object id (40 or 64 hex characters)")
	}
	if e.GitDelivery != nil {
		validateGitDeliveryContract(v, *e.GitDelivery)
	}
	validateActionDeliveryBindings(v, e.NonSourceDelivery, e.ActionIntentIDs)
	if e.Prediction != nil {
		validateWorkPrediction(v, *e.Prediction)
	}
	if e.RouteDeviation != nil {
		validateRouteDeviation(v, *e.RouteDeviation, e.Prediction)
	}
	validateRunAttemptIDs(v, e.RunAttemptIDs)
	validateReworkOfWorkID(v, e.ReworkOfWorkID, e.ID)
	if e.Origin != nil {
		validateWorkOrigin(v, *e.Origin)
	}
	if e.ExecutionSelection != nil {
		validateExecutionSelection(v, *e.ExecutionSelection)
	}
	return v.result(e.Kind())
}

func validateWorkOrigin(v *verr, origin WorkOrigin) {
	v.req("origin.accountable_owner", origin.AccountableOwner)
	v.req("origin.actor", origin.Actor)
	v.req("origin.harness", origin.Harness)
	v.req("origin.requested_model", origin.RequestedModel)
	v.req("origin.lane_id", origin.LaneID)
	v.enum("origin.identity_source", origin.IdentitySource, []string{"explicit_or_environment", "derived"}, true)
}

// validateReworkOfWorkID enforces the shape of the rework lineage edge (see
// WorkEnvelope.ReworkOfWorkID). Empty is legal and means "not a rework".
//
// The shape rule is deliberately the LOOSE work-id check already used across the tree (see
// reviewartifact.validDurableWorkID), not a strict `work_<12hex>` regex: `work create --work-id`
// admits arbitrary shapes, so a strict rule here would refuse edges to ids the store itself
// accepts and holds on disk. What it does refuse is anything that cannot be a durable work id --
// a value with no `work_` prefix, a bare prefix with no body, the ephemeral `work_live_`
// namespace, and any value that is not a single path element (a phantom edge that reads as data
// is worse than no edge, and an id carrying a separator is also a path-traversal shape once a
// caller joins it onto a store directory).
//
// Self-reference is refused here rather than at the store, because this is the only place both
// ids are visible at once -- and because at create time the work being created does not exist
// yet, so the store's existence check would refuse a self-edge for the wrong reason and would
// silently accept one if an id were ever reused.
func validateReworkOfWorkID(v *verr, reworkOf, selfID string) {
	if reworkOf == "" {
		return
	}
	if !strings.HasPrefix(reworkOf, "work_") || len(reworkOf) <= len("work_") ||
		strings.HasPrefix(reworkOf, "work_live_") || filepath.Base(reworkOf) != reworkOf {
		v.addf("rework_of_work_id %q must be a durable work id (a single path element starting with %q, outside the %q namespace)", reworkOf, "work_", "work_live_")
		return
	}
	if reworkOf == selfID {
		v.addf("rework_of_work_id %q must not point at itself: a work cannot be its own rework", reworkOf)
	}
}

// validateRunAttemptIDs enforces two rules on the reserved run slot:
//
//  1. non-emptiness (the floor that has applied since the slot was reserved, so legacy
//     envelopes with `run_attempt_ids` never serialize a blank);
//  2. ratt_<UTC>_<8 hex> prefix (settled by S1; see internal/runattempt.Prefix / Valid).
//
// The prefix rule is the load-bearing one: it is what stops the slot from silently
// re-accumulating the heterogeneous run-shaped evidence (prun_*, run_*, free strings) that
// the v0.1 ActionIntentIDs mix allowed. Pinned by TestRunAttemptIDsRequireRattPrefix and
// TestRunAttemptIDsRejectEmptyEntries.
//
// No prefix rule is applied to ActionIntentIDs: real v0.1 data already violates one (the
// `prun_*` legacy run ids appended by panel.Apply land there), and the v0.1 SEMANTIC MIX
// comment on the field names the path that retires them.
func validateRunAttemptIDs(v *verr, ids []string) {
	for i, id := range ids {
		if strings.TrimSpace(id) == "" {
			v.addf("run_attempt_ids[%d] must not be empty", i)
			continue
		}
		if !strings.HasPrefix(id, "ratt_") {
			v.addf("run_attempt_ids[%d] %q must start with %q (settled in S1; see internal/runattempt.Prefix)", i, id, "ratt_")
		}
	}
}

// validateWorkPrediction holds a hand-written work envelope's staked prediction to the same rule
// as any other writer: outcome REQUIRED and must be a close outcome (never a Receipt.Outcome
// value like "no_op" or "blocked"), move_type optional but must be one of MoveTypes when set.
func validateWorkPrediction(v *verr, p WorkPrediction) {
	v.enum("prediction.outcome", p.Outcome, CloseOutcomes, true)
	v.enum("prediction.move_type", p.MoveType, MoveTypes, false)
}

// effectiveRouteDeviationGrade resolves the admission grade a RouteDeviation binds on. An empty or
// unrecognized grade resolves to `judge` -- fail-closed: an ungraded deviation gets the strong rule
// (stake a prediction or be refused), never the ungated `tidy` path. An out-of-enum value is
// separately reported by validateRouteDeviation so the caller sees the typo, but for the admission
// decision it still binds at judge rather than silently going ungated.
func effectiveRouteDeviationGrade(grade string) string {
	switch strings.TrimSpace(grade) {
	case RouteDeviationTidy:
		return RouteDeviationTidy
	case RouteDeviationGate:
		return RouteDeviationGate
	default:
		return RouteDeviationJudge
	}
}

// validateRouteDeviation enforces the S3 admission binding for an explicit route deviation: a
// deviation graded `judge` (or ungraded -- see effectiveRouteDeviationGrade) MUST carry a staked
// WorkPrediction or admission REFUSES the envelope. The refusal binds on deviation-ness (this marker
// present) + risk grade ALONE; the advisory CardRef is never read, so omitting the EV card can never
// become the bypass. `gate` rides along to a human via the existing attention.Class -> ActionIntent
// -> PolicyDecision machinery (this validator adds no gate for it); `tidy` flows ungated
// (decisions-down: a reversible in-envelope act needs no per-item stake). Holds a hand-written
// envelope to the same rule as the CLI create path, since both funnel through WorkEnvelope.Validate.
func validateRouteDeviation(v *verr, dev RouteDeviation, prediction *WorkPrediction) {
	v.req("route_deviation.lane", dev.Lane)
	v.enum("route_deviation.risk_grade", dev.RiskGrade, RouteDeviationRiskGrades, false)
	if effectiveRouteDeviationGrade(dev.RiskGrade) == RouteDeviationJudge && prediction == nil {
		v.addf("route_deviation graded %q requires a staked prediction (stake with work create --predicted-outcome …): a judge-grade deviation stakes or is refused, and the advisory EV card's absence is never a bypass", effectiveRouteDeviationGrade(dev.RiskGrade))
	}
}

func validateGitDeliveryContract(v *verr, contract GitDeliveryContract) {
	// An exactly empty fingerprint is the readable legacy v0.1 shape from
	// before repository identity entered GitDeliveryContract. Current writers
	// derive it before admission; work execution gates require governed
	// backfill. Whitespace and every other malformed non-empty value remain
	// invalid rather than masquerading as legacy state.
	if contract.RepositoryFingerprint != "" && !validSHA256(strings.TrimSpace(contract.RepositoryFingerprint)) {
		v.addf("git_delivery.repository_fingerprint must be a lowercase SHA-256 hex digest")
	}
	root := strings.TrimSpace(contract.TargetWorktreeRoot)
	if root == "" {
		v.addf("git_delivery.target_worktree_root is required")
	} else {
		if !filepath.IsAbs(root) {
			v.addf("git_delivery.target_worktree_root must be absolute")
		}
		if filepath.Clean(root) != root {
			v.addf("git_delivery.target_worktree_root must be a clean canonical path")
		}
	}
	localRef := strings.TrimSpace(contract.TargetLocalRef)
	if !validBoundGitRef(localRef, "refs/heads/") {
		v.addf("git_delivery.target_local_ref must be a canonical refs/heads/* ref")
	}
	if !validFullGitObjectID(strings.TrimSpace(contract.ExpectedBaseSHA)) {
		v.addf("git_delivery.expected_base_sha must be a lowercase full Git object id (40 or 64 hex characters)")
	}
	switch strings.TrimSpace(contract.PublicationMode) {
	case "required":
		publicationRef := strings.TrimSpace(contract.PublicationRef)
		if !validBoundGitRef(publicationRef, "refs/remotes/") {
			v.addf("git_delivery.publication_ref must be a canonical remote-tracking refs/remotes/* ref")
		} else if validBoundGitRef(localRef, "refs/heads/") {
			branch := strings.TrimPrefix(localRef, "refs/heads/")
			_, publicationBranch, found := strings.Cut(strings.TrimPrefix(publicationRef, "refs/remotes/"), "/")
			if !found || publicationBranch != branch {
				v.addf("git_delivery.publication_ref must track the target_local_ref branch")
			}
		}
	case "local_only":
		if strings.TrimSpace(contract.PublicationRef) != "" {
			v.addf("git_delivery.publication_ref must be empty when publication_mode is local_only")
		}
	default:
		v.addf("git_delivery.publication_mode must be required or local_only")
	}
}

func validBoundGitRef(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) {
		return false
	}
	if strings.ContainsAny(value, " ~^:?*[\\") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, ".lock") || strings.Contains(value, "@{") {
		return false
	}
	return true
}

func validFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

type ActionTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
}
type EstimatedCost struct {
	USD            *float64 `json:"usd"`
	Tokens         *int     `json:"tokens"`
	HumanReviewMin *int     `json:"human_review_minutes"`
}
type ActionIntent struct {
	Header
	Actor                string          `json:"actor"`
	Capability           string          `json:"capability"`
	Action               string          `json:"action"`
	Target               ActionTarget    `json:"target"`
	Destination          string          `json:"destination,omitempty"`
	ArgumentsHash        string          `json:"arguments_hash,omitempty"`
	InputSources         []SourceBinding `json:"input_sources,omitempty"`
	SchemaRevision       string          `json:"schema_revision,omitempty"`
	SecurityFloorOutcome string          `json:"security_floor_outcome,omitempty"`
	SecurityReasons      []string        `json:"security_reasons,omitempty"`
	// RequiresExactApproval keeps target/autonomy authority separate from a
	// one-use operator approval. It is included in the full intent binding.
	RequiresExactApproval bool `json:"requires_exact_approval,omitempty"`
	// CapturesWorkInProgress marks an action that would COMMIT, STAGE, STASH, or
	// DESTROY uncommitted working-tree changes rather than merely run beside
	// them. The source-control gate uses it to tell the actions a foreign dirty
	// path is actually at risk from apart from the ones that just happen to run
	// in the same repository.
	CapturesWorkInProgress bool          `json:"captures_work_in_progress,omitempty"`
	MutationClass          string        `json:"mutation_class"`
	Reversibility          string        `json:"reversibility"`
	BlastRadius            string        `json:"blast_radius,omitempty"`
	EstimatedCost          EstimatedCost `json:"estimated_cost"`
	Preview                string        `json:"preview,omitempty"`
	RollbackPlan           string        `json:"rollback_plan,omitempty"`

	// DesktopSurface is where the action lands on the operator's machine
	// (isolated_headless, isolated_visible, background_gui, operator_browser,
	// foreground_gui); empty for an action that touches no desktop surface.
	// It travels on the intent because the live hook re-evaluates the intent,
	// not the monitor's decision: policy attaches the operator-live obligation
	// from this field. The unattended hold rides SecurityFloorOutcome.
	DesktopSurface string `json:"desktop_surface,omitempty"`
}

func (e *ActionIntent) Kind() string { return "action_intent" }
func (e *ActionIntent) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVActionIntent, "act_")
	v.req("capability", e.Capability)
	v.enum("mutation_class", e.MutationClass, MutationClasses, true)
	v.enum("reversibility", e.Reversibility, Reversibilities, true)
	if e.SecurityFloorOutcome != "" {
		v.enum("security_floor_outcome", e.SecurityFloorOutcome, []string{"hold", "deny"}, true)
		if len(e.SecurityReasons) == 0 {
			v.addf("security_floor_outcome requires security_reasons")
		}
	}
	if e.MutationClass != "" && e.MutationClass != "read" {
		v.req("target.kind", e.Target.Kind)
		if strings.TrimSpace(e.Target.ID) == "" && strings.TrimSpace(e.Target.Path) == "" {
			v.addf("target.id or target.path is required for mutating action")
		}
	}
	if e.IDs.WorkID == "" {
		v.addf("ids.work_id is required (an action belongs to a work)")
	}
	return v.result(e.Kind())
}

type PolicyDecision struct {
	Header
	ActionIntentID  string   `json:"action_intent_id"`
	Outcome         string   `json:"outcome"`
	Reasons         []string `json:"reasons"`
	Obligations     []string `json:"obligations"`
	StandingGrantID string   `json:"standing_grant_id,omitempty"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
}

func (e *PolicyDecision) Kind() string { return "policy_decision" }
func (e *PolicyDecision) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVPolicyDecision, "pol_")
	v.req("action_intent_id", e.ActionIntentID)
	v.enum("outcome", e.Outcome, PolicyOutcomes, true)
	if e.Outcome == "allow_with_obligations" && len(e.Obligations) == 0 {
		v.addf("outcome=allow_with_obligations requires at least one obligation")
	}
	return v.result(e.Kind())
}

// CapabilityUse is the bounded, normalized fact emitted when a native Skill
// tool completes. Raw tool input and output never enter the envelope.
type CapabilityUse struct {
	Ref     string `json:"ref"`
	Skill   string `json:"skill"`
	Trigger string `json:"trigger,omitempty"`
	Lane    string `json:"lane,omitempty"`
	Outcome string `json:"outcome"`
}

type HookEvent struct {
	Header
	Harness        string   `json:"harness"`
	HookName       string   `json:"hook_name"`
	HookCapability string   `json:"hook_capability,omitempty"`
	InputHash      string   `json:"input_hash,omitempty"`
	SideEffects    []string `json:"side_effects"`
	OutputRef      string   `json:"output_ref,omitempty"`
	DurationMS     int      `json:"duration_ms"`
	Status         string   `json:"status"`
	// FailOpenReason disambiguates the two meanings of Status=="fail_open". The status
	// itself cannot: three writers use it for an ABSENT NATIVE CAPABILITY (an expected,
	// static property of the harness — the honest "an adapter must enforce this") and one
	// uses it for a CONTEXT BUILD FAILURE, where construction errored or blew its budget
	// and the model received nothing. Only the second is alarming, and a consumer reading
	// persisted events could not tell them apart: the breaker's in-memory signal
	// (Result.BreakerTripped) never reaches the event.
	//
	// OPTIONAL and additive, declared LAST: absent means "not stated", every pre-existing
	// event re-marshals byte-identically, and no reader is required to consult it. Values
	// are FailOpenReasons; empty stays legal so this can never fail a stored event.
	FailOpenReason string `json:"fail_open_reason,omitempty"`
	// Budget watchdog responses state policy intent separately from what the
	// harness actually observes. These are optional because ordinary events
	// and pre-budget envelopes predate the distinction.
	PolicyPosture       string         `json:"policy_posture,omitempty"`
	EffectiveOutcome    string         `json:"effective_outcome,omitempty"`
	HarnessExitBehavior string         `json:"harness_exit_behavior,omitempty"`
	EvidencePersistence string         `json:"evidence_persistence,omitempty"`
	CapabilityUse       *CapabilityUse `json:"capability_use,omitempty"`
}

// The meanings of Status=="fail_open". See HookEvent.FailOpenReason.
const (
	// FailOpenCapabilityAbsent: the harness has no native hook, so enforcement or
	// delivery is delegated to an adapter. Expected, static, not alarming.
	FailOpenCapabilityAbsent = "capability_absent"
	// FailOpenContextUnavailable: context construction errored or exceeded its budget,
	// so NO context reached the model. A runtime failure, and the alarming one.
	FailOpenContextUnavailable = "context_unavailable"
	// FailOpenBudgetExceeded: the hook command as a whole exceeded its wall-clock
	// budget and answered fail-open from the watchdog instead of finishing.
	// Lived 2026-08-24: hooks outran the harness's kill timeout and died with
	// no output at all, which reads as a broken hook rather than a bounded
	// one. This reason names that the answer is a budget decision, not a
	// finished evaluation; the event carrying it never reaches disk (the
	// process exits at the watchdog), so it exists to make the RESPONSE
	// truthful.
	FailOpenBudgetExceeded = "budget_exceeded"
)

func (e *HookEvent) Kind() string { return "hook_event" }
func (e *HookEvent) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVHookEvent, "hook_")
	v.req("harness", e.Harness)
	v.enum("hook_name", e.HookName, HookNames, true)
	v.enum("status", e.Status, HookStatuses, true)
	// Optional: empty is legal so every pre-existing event still validates. A stated
	// reason must be a known one, so an unrecognized free string can never reach disk and
	// become a silently unreadable signal.
	v.enum("fail_open_reason", e.FailOpenReason, FailOpenReasons, false)
	if e.Status != "fail_open" && e.FailOpenReason != "" {
		v.addf("fail_open_reason requires status=fail_open")
	}
	v.enum("policy_posture", e.PolicyPosture, []string{"fail_open", "enforced_block"}, false)
	v.enum("effective_outcome", e.EffectiveOutcome, []string{"allow", "block"}, false)
	v.enum("harness_exit_behavior", e.HarnessExitBehavior, []string{"exit_0", "exit_4"}, false)
	v.enum("evidence_persistence", e.EvidencePersistence, []string{"not_persisted_watchdog_response", "persisted"}, false)
	if e.CapabilityUse != nil {
		v.req("capability_use.ref", e.CapabilityUse.Ref)
		v.req("capability_use.skill", e.CapabilityUse.Skill)
		if !capabilitySkillRe.MatchString(e.CapabilityUse.Skill) {
			v.addf("capability_use.skill must be a 1-128 character lowercase skill identifier")
		}
		v.enum("capability_use.outcome", e.CapabilityUse.Outcome, []string{"used"}, true)
		if e.HookName != "PostToolUse" {
			v.addf("capability_use requires hook_name=PostToolUse")
		}
		if e.CapabilityUse.Ref != "capability://skill:"+e.CapabilityUse.Skill {
			v.addf("capability_use.ref must equal capability://skill:<skill>")
		}
		if len(e.CapabilityUse.Trigger) > 512 {
			v.addf("capability_use.trigger exceeds 512 bytes")
		}
	}
	return v.result(e.Kind())
}

type TraceSource struct {
	System      string `json:"system"`
	Stream      string `json:"stream,omitempty"`
	SourceID    string `json:"source_id,omitempty"`
	SourceKind  string `json:"source_kind,omitempty"`
	EventClass  string `json:"event_class,omitempty"`
	RawRef      string `json:"raw_ref,omitempty"`
	Line        int    `json:"line,omitempty"`
	Quality     string `json:"quality,omitempty"`
	MutationRaw string `json:"mutation_raw,omitempty"`
}

type TraceEnvelope struct {
	Header
	Source          TraceSource `json:"source"`
	Actor           string      `json:"actor,omitempty"`
	StartedAt       string      `json:"started_at,omitempty"`
	EndedAt         string      `json:"ended_at,omitempty"`
	MutationClass   string      `json:"mutation_class"`
	Summary         string      `json:"summary,omitempty"`
	ToolsModelsUsed []string    `json:"tools_models_used"`
	FilesTouched    []string    `json:"files_touched"`
	Outputs         []string    `json:"outputs"`
	Cost            ReceiptCost `json:"cost"`
}

func (e *TraceEnvelope) Kind() string { return "trace_envelope" }
func (e *TraceEnvelope) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVTraceEnvelope, "trace_")
	v.req("source.system", e.Source.System)
	v.enum("mutation_class", e.MutationClass, MutationClasses, true)
	return v.result(e.Kind())
}

type StateDelta struct {
	Changed bool `json:"changed"`
	// Empty preserves legacy known-effect encoding. Unknown omits changed.
	ChangeKnowledge string   `json:"change_knowledge,omitempty"`
	Summary         string   `json:"summary"`
	FilesChanged    []string `json:"files_changed"`
	Artifacts       []string `json:"artifacts"`
	Verification    []string `json:"verification"`
}
type ReceiptCost struct {
	ModelCalls   []string `json:"model_calls"`
	Tokens       *int     `json:"tokens"`
	USDActual    *float64 `json:"usd_actual"`
	USDEstimated *float64 `json:"usd_estimated"`
	HumanMinutes *int     `json:"human_minutes"`
}
type ReceiptSkills struct {
	Used               []string `json:"used"`
	Missed             []string `json:"missed"`
	Referenced         []string `json:"referenced,omitempty"`
	ExplicitNonUse     []string `json:"explicit_non_use,omitempty"`
	NonUseEvidence     string   `json:"non_use_evidence,omitempty"`
	ObservationMethod  string   `json:"observation_method,omitempty"`
	ObservationQuality string   `json:"observation_quality,omitempty"`
	ApplicationMethod  string   `json:"application_method,omitempty"`
	NonUseMethod       string   `json:"non_use_method,omitempty"`
	NonUseActor        string   `json:"non_use_actor,omitempty"`
	// UsedDeclared is retained for explicit typed declarations and historical wire
	// compatibility. It does not establish non-use of other injected refs.
	UsedDeclared        bool     `json:"used_declared,omitempty"`
	FalseTriggered      []string `json:"false_triggered,omitempty"`
	Candidate           []string `json:"candidate"`
	CapabilitiesOffered []string `json:"capabilities_offered,omitempty"`
}

// ApplicationObserved requires a recorded mechanism. Legacy Used/UsedDeclared alone
// cannot distinguish a real application from an automatic transcript reference.
func (s ReceiptSkills) ApplicationObserved(ref string) bool {
	if strings.HasPrefix(ref, "claim://") {
		return s.ApplicationMethod == "explicit_declaration"
	}
	return s.ApplicationMethod == "explicit_declaration" || s.ApplicationMethod == "skill_tool_call"
}

func (s ReceiptSkills) ReferenceObserved(ref string) bool {
	if s.ObservationMethod != "transcript_reference_scan" || s.ObservationQuality != "readable" {
		return false
	}
	for _, observed := range s.Referenced {
		if observed == ref {
			return true
		}
	}
	return false
}

// ExplicitNonUseObserved requires an exact ref and a receipt-bound evidence pointer.
func (e Receipt) ExplicitNonUseObserved(ref string) bool {
	if e.Skills.NonUseMethod != "explicit_declaration" || strings.TrimSpace(e.Skills.NonUseEvidence) == "" {
		return false
	}
	if strings.TrimSpace(e.Skills.NonUseActor) == "" || strings.TrimSpace(e.IDs.WorkID) == "" {
		return false
	}
	for _, used := range e.Skills.Used {
		if used == ref {
			return false
		}
	}
	evidenceFound := false
	for _, evidence := range e.EvidenceRefs {
		if evidence == e.Skills.NonUseEvidence {
			evidenceFound = true
		}
	}
	if !evidenceFound {
		return false
	}
	proofFound := false
	for _, proof := range e.ProofRefs {
		if proof == e.Skills.NonUseEvidence {
			proofFound = true
		}
	}
	if !proofFound {
		return false
	}
	for _, declared := range e.Skills.ExplicitNonUse {
		if declared == ref {
			return true
		}
	}
	return false
}

type ReceiptMemory struct {
	Candidates []string `json:"candidates"`
	Promoted   []string `json:"promoted"`
	Parked     []string `json:"parked"`
	Rejected   []string `json:"rejected,omitempty"`
}
type ReceiptNext struct {
	SuggestedWork []string `json:"suggested_work"`
	ReviewTrigger string   `json:"review_trigger,omitempty"`
}

// FailureCause is the OPTIONAL causal-failure annotation on a close receipt:
// what the operator/agent observed (surface_outcome) and the mechanism they
// attribute it to. Additive and omitempty — absence changes no existing behavior;
// present, it lets the usage projection aggregate why work failed.
type FailureCause struct {
	SurfaceOutcome string `json:"surface_outcome,omitempty"`
	Mechanism      string `json:"mechanism,omitempty"`
}
type Receipt struct {
	Header
	Outcome      string        `json:"outcome"`
	MoveType     string        `json:"move_type"`
	StateDelta   StateDelta    `json:"state_delta"`
	Cost         ReceiptCost   `json:"cost"`
	Skills       ReceiptSkills `json:"skills"`
	Memory       ReceiptMemory `json:"memory"`
	Next         ReceiptNext   `json:"next"`
	ProofRefs    []string      `json:"proof_refs,omitempty"`
	FailureCause *FailureCause `json:"failure_cause,omitempty"`
	// Surprise is the mechanical diff between the closing WorkEnvelope's staked WorkPrediction
	// and what actually happened at close (see internal/work's surpriseOnClose). Nil when the
	// work staked no prediction. When staked, ALWAYS set -- Diverged=false (a confirmed
	// prediction) is signal too, not merely the absence of a divergence flag.
	Surprise *SurpriseSignal `json:"surprise,omitempty"`
}

// SurpriseSignal is the harness-layer prediction-error record: what the work staked at create
// vs. what its close actually was. PredictedMoveType is empty when that dimension was left
// unstaked (and is then excluded from the Diverged comparison). Named consumers: the
// adjudication inbox (via the observed candidate ObserveWorkSurprise mints on divergence), the
// ACE/injection-observation weekly compile, and the canonized value-function Revisit trigger.
type SurpriseSignal struct {
	PredictedOutcome  string `json:"predicted_outcome"`
	PredictedMoveType string `json:"predicted_move_type,omitempty"`
	ActualOutcome     string `json:"actual_outcome"`
	ActualMoveType    string `json:"actual_move_type"`
	Diverged          bool   `json:"diverged"`
}

func (e *Receipt) Kind() string { return "receipt" }

func (e Receipt) EffectiveChanged() bool {
	if e.StateDelta.Changed {
		return true
	}
	if e.Outcome != "completed" {
		return false
	}
	proofRefs := e.proofRefs()
	switch e.MoveType {
	case "memory_delta":
		return len(nonEmpty(e.memoryRefs())) > 0
	case "rule_delta":
		return hasPrefixedProof(proofRefs, []string{"failure:", "failure_pattern:", "rule:", "check:", "guard:"})
	case "control_plane_repair", "outward_delta":
		return len(nonEmpty(proofRefs)) > 0
	default:
		return false
	}
}

func (e Receipt) proofRefs() []string {
	proofRefs := append([]string{}, e.ProofRefs...)
	proofRefs = append(proofRefs, e.StateDelta.Artifacts...)
	proofRefs = append(proofRefs, e.StateDelta.Verification...)
	return proofRefs
}

func (e Receipt) memoryRefs() []string {
	refs := append([]string{}, e.Memory.Candidates...)
	refs = append(refs, e.Memory.Promoted...)
	refs = append(refs, e.Memory.Parked...)
	refs = append(refs, e.Memory.Rejected...)
	return refs
}

func (e *Receipt) Validate() error {
	v := &verr{}
	if e.StateDelta.ChangeKnowledge != "" && e.StateDelta.ChangeKnowledge != "unknown" {
		v.addf("state_delta.change_knowledge must be omitted or unknown")
	}
	if e.StateDelta.ChangeKnowledge == "unknown" {
		if e.StateDelta.Changed {
			v.addf("unknown effects cannot assert state_delta.changed")
		}
		if e.Outcome == "completed" || e.Outcome == "no_op" {
			v.addf("completed or no_op receipt requires known effects")
		}
	}

	if len(e.Skills.Referenced) > 0 && (e.Skills.ObservationMethod != "transcript_reference_scan" || e.Skills.ObservationQuality != "readable") {
		v.addf("skills.referenced requires readable transcript_reference_scan")
	}
	if e.Skills.ObservationMethod != "" && e.Skills.ObservationMethod != "transcript_reference_scan" {
		v.addf("skills.observation_method invalid")
	}
	if e.Skills.ObservationQuality != "" && e.Skills.ObservationQuality != "readable" && e.Skills.ObservationQuality != "unobservable" {
		v.addf("skills.observation_quality invalid")
	}
	if e.Skills.ApplicationMethod != "" && e.Skills.ApplicationMethod != "explicit_declaration" && e.Skills.ApplicationMethod != "skill_tool_call" {
		v.addf("skills.application_method invalid")
	}
	if len(e.Skills.ExplicitNonUse) > 0 && strings.TrimSpace(e.Skills.NonUseEvidence) == "" {
		v.addf("skills.explicit_non_use requires skills.non_use_evidence")
	}
	if len(e.Skills.ExplicitNonUse) > 0 {
		if e.Skills.NonUseMethod != "explicit_declaration" {
			v.addf("skills.explicit_non_use requires explicit_declaration method")
		}
		if strings.TrimSpace(e.Skills.NonUseActor) == "" || strings.TrimSpace(e.IDs.WorkID) == "" {
			v.addf("skills.explicit_non_use requires work and actor provenance")
		}
		evidenceFound := false
		for _, ref := range e.EvidenceRefs {
			if ref == e.Skills.NonUseEvidence {
				evidenceFound = true
			}
		}
		if !evidenceFound {
			v.addf("skills.non_use_evidence must appear in evidence_refs")
		}
		proofFound := false
		for _, proof := range e.ProofRefs {
			if proof == e.Skills.NonUseEvidence {
				proofFound = true
			}
		}
		if !proofFound {
			v.addf("skills.non_use_evidence must appear in proof_refs")
		}
		for _, ref := range e.Skills.ExplicitNonUse {
			for _, used := range e.Skills.Used {
				if ref == used {
					v.addf("skills.explicit_non_use contradicts skills.used for %q", ref)
				}
			}
		}
	}
	e.Header.validate(v, SVReceipt, "rec_")
	v.enum("outcome", e.Outcome, ReceiptOutcomes, true)
	v.enum("move_type", e.MoveType, MoveTypes, true)
	proofRefs := e.proofRefs()
	switch e.MoveType {
	case "outward_delta":
		if e.Outcome == "completed" && !e.StateDelta.Changed && len(nonEmpty(proofRefs)) == 0 {
			v.addf("outward_delta completed requires state_delta.changed=true or proof_refs/artifacts/verification")
		}
	case "memory_delta":
		if len(nonEmpty(e.memoryRefs())) == 0 {
			v.addf("memory_delta requires memory candidate/promoted/parked/rejected ref")
		}
	case "rule_delta":
		if !hasPrefixedProof(proofRefs, []string{"failure:", "failure_pattern:", "rule:", "check:", "guard:"}) {
			v.addf("rule_delta requires failure/rule/check/guard proof_ref")
		}
	case "control_plane_repair":
		if len(nonEmpty(proofRefs)) == 0 {
			v.addf("control_plane_repair requires repair/retirement/gate proof_ref, artifact, or verification")
		}
	}
	return v.result(e.Kind())
}

func nonEmpty(values []string) []string {
	out := []string{}
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func hasPrefixedProof(values []string, prefixes []string) bool {
	for _, v := range nonEmpty(values) {
		for _, prefix := range prefixes {
			if strings.HasPrefix(v, prefix) {
				return true
			}
		}
	}
	return false
}

type ProjectionRef struct {
	Kind        string  `json:"kind"`
	Ref         string  `json:"ref"`
	Role        string  `json:"role"`
	EvidenceRef string  `json:"evidence_ref,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
	Status      string  `json:"status,omitempty"`
}

type Projections struct {
	Memory         []string        `json:"memory"`
	Skill          []string        `json:"skill"`
	Product        []string        `json:"product"`
	Cost           []string        `json:"cost"`
	Risk           []string        `json:"risk"`
	Taste          []string        `json:"taste"`
	Routing        []string        `json:"routing"`
	HumanAttention []string        `json:"human_attention"`
	Typed          []ProjectionRef `json:"typed,omitempty"`
}
type SecondOrder struct {
	FutureBehaviorChanged bool   `json:"future_behavior_changed"`
	ShouldHarden          bool   `json:"should_harden"`
	ShouldCreateRule      bool   `json:"should_create_rule"`
	ShouldDecay           bool   `json:"should_decay"`
	RevisitAt             string `json:"revisit_at,omitempty"`
}

// TransitionCause is the OPTIONAL causal cross-ref carried with a Transition: the causal CLASS of
// the source-truth change (delta_cause), the refs that caused it (caused_by_refs), and who is
// expected to act next (next_consumer, empty => audit_only). It ships a proven subset of the
// future-branch semantics in docs/architecture/transition.md; opens_branches[]/prevents_branch are
// deferred until a real consumer exists. Additive + omitempty: absence leaves every existing
// Transition unaffected.
type TransitionCause struct {
	DeltaCause   string   `json:"delta_cause"`             // one of DeltaCauses
	CausedByRefs []string `json:"caused_by_refs"`          // >=1 non-empty: work_id / decision: / commit / source:<path>#rev
	NextConsumer string   `json:"next_consumer,omitempty"` // consumer slug/ref matching nextConsumerRe; empty => audit_only
}

// BranchRef is ONE future-branch statement on a Transition: which branch slug,
// who is expected to act on it next (owner, a consumer ref matching
// nextConsumerRe), and the lifecycle kind (one of BranchKinds). One statement
// per branch per transition. Coverage is explicit-only: a later transition's
// validates|supersedes|retires on the same slug resolves the branch.
type BranchRef struct {
	Branch string `json:"branch"` // branch slug matching branchSlugRe
	Owner  string `json:"owner"`  // consumer slug/ref matching nextConsumerRe
	Kind   string `json:"kind"`   // one of BranchKinds
}

type Transition struct {
	Header
	SourceTraceID string           `json:"source_trace_id,omitempty"`
	BeforeRefs    []string         `json:"before_refs"`
	ActionRefs    []string         `json:"action_refs"`
	AfterRefs     []string         `json:"after_refs"`
	StateDelta    string           `json:"state_delta,omitempty"`
	Projections   Projections      `json:"projections"`
	SecondOrder   SecondOrder      `json:"second_order"`
	Cause         *TransitionCause `json:"cause,omitempty"`
	// OpensBranches ships the deferred future-branch semantics of
	// docs/architecture/transition.md: additive + omitempty, so absence leaves
	// every pre-existing Transition untouched. prevents_branch stays deferred
	// (no consumer yet).
	OpensBranches []BranchRef `json:"opens_branches,omitempty"`
}

func (e *Transition) Kind() string { return "transition" }
func (e *Transition) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVTransition, "tr_")
	if e.IDs.WorkID == "" {
		v.addf("ids.work_id is required (a transition belongs to a work)")
	}
	for i, p := range e.Projections.Typed {
		v.enum(fmt.Sprintf("projections.typed[%d].kind", i), p.Kind, ProjectionKinds, true)
		v.enum(fmt.Sprintf("projections.typed[%d].role", i), p.Role, ProjectionRoles, true)
		v.req(fmt.Sprintf("projections.typed[%d].ref", i), p.Ref)
	}
	if e.Cause != nil {
		v.enum("cause.delta_cause", e.Cause.DeltaCause, DeltaCauses, true)
		if len(nonEmpty(e.Cause.CausedByRefs)) == 0 {
			v.addf("cause.caused_by_refs requires at least one non-empty ref")
		}
		if e.Cause.NextConsumer != "" && !nextConsumerRe.MatchString(e.Cause.NextConsumer) {
			v.addf("cause.next_consumer must be a consumer slug/ref (no prose), got %q", e.Cause.NextConsumer)
		}
	}
	seenBranches := map[string]bool{}
	for i, b := range e.OpensBranches {
		v.enum(fmt.Sprintf("opens_branches[%d].kind", i), b.Kind, BranchKinds, true)
		if !branchSlugRe.MatchString(b.Branch) {
			v.addf("opens_branches[%d].branch must be a branch slug (no prose), got %q", i, b.Branch)
		} else if seenBranches[b.Branch] {
			v.addf("opens_branches[%d].branch %q duplicates an earlier entry (one statement per branch per transition)", i, b.Branch)
		}
		seenBranches[b.Branch] = true
		if !nextConsumerRe.MatchString(b.Owner) {
			v.addf("opens_branches[%d].owner must be a consumer slug/ref (no prose), got %q", i, b.Owner)
		}
	}
	return v.result(e.Kind())
}

// --- Registry / dispatch ---

var registry = map[string]func() Envelope{
	SVEffectiveContractV01: func() Envelope { return &EffectiveContract{} },
	SVEffectiveContract:    func() Envelope { return &EffectiveContract{} },
	SVContextRequest:       func() Envelope { return &ContextRequest{} },
	SVContextPack:          func() Envelope { return &ContextPack{} },
	SVWork:                 func() Envelope { return &WorkEnvelope{} },
	SVSourceEnvelope:       func() Envelope { return &SourceEnvelope{} },
	SVActionIntent:         func() Envelope { return &ActionIntent{} },
	SVPolicyDecision:       func() Envelope { return &PolicyDecision{} },
	SVHookEvent:            func() Envelope { return &HookEvent{} },
	SVTraceEnvelope:        func() Envelope { return &TraceEnvelope{} },
	SVReceipt:              func() Envelope { return &Receipt{} },
	SVTransition:           func() Envelope { return &Transition{} },
}

// New returns a zero envelope for a schema_version, or nil if unknown.
func New(schemaVersion string) Envelope {
	if ctor, ok := registry[schemaVersion]; ok {
		return ctor()
	}
	return nil
}

// Kinds lists "schema_version  kind" for every registered envelope, sorted.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for sv, ctor := range registry {
		out = append(out, fmt.Sprintf("%-36s %s", sv, ctor().Kind()))
	}
	sort.Strings(out)
	return out
}

// ValidateBytes detects the kind from schema_version, decodes into the concrete
// type, and validates. Returns the kind (best-effort) and any error.
func ValidateBytes(data []byte) (string, error) {
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("not valid json: %w", err)
	}
	if probe.SchemaVersion == "" {
		return "", errors.New("missing schema_version")
	}
	e := New(probe.SchemaVersion)
	if e == nil {
		return probe.SchemaVersion, fmt.Errorf("unknown schema_version %q", probe.SchemaVersion)
	}
	if err := json.Unmarshal(data, e); err != nil {
		return e.Kind(), fmt.Errorf("decode: %w", err)
	}
	return e.Kind(), e.Validate()
}

// RoundTrip marshals an envelope, decodes it back into a fresh instance, and
// asserts byte-identical re-marshal — proving the schema is lossless.
func RoundTrip(e Envelope) error {
	b1, err := json.Marshal(e)
	if err != nil {
		return err
	}
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	_ = json.Unmarshal(b1, &probe)
	e2 := New(probe.SchemaVersion)
	if e2 == nil {
		return fmt.Errorf("round-trip: unregistered schema_version %q", probe.SchemaVersion)
	}
	if err := json.Unmarshal(b1, e2); err != nil {
		return err
	}
	b2, err := json.Marshal(e2)
	if err != nil {
		return err
	}
	if !bytes.Equal(b1, b2) {
		return fmt.Errorf("round-trip mismatch for %s", e.Kind())
	}
	return nil
}
