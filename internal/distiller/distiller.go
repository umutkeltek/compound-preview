// Package distiller is the P6 distillation front-end: it turns spine artifacts (a trace) into a
// typed, adjudicated CandidateSet — the layer that makes memory COMPOUND rather than accumulate.
// Invariants (also enforced by internal/mem): the model EXTRACTS, code DECIDES; every candidate
// is adjudicated (reject | park | promote | operationalize | retire); NOTHING reaches ambient
// recall unadjudicated; a self-improvement signal (a better method, a distiller mistake, a
// capability miss) becomes a CANDIDATE — never a silent prompt edit or auto-applied change.
// It extends internal/mem (memory-kind candidates flow through the mem ledger); it is not a
// parallel memory system. See docs/roadmap/phases.md (P6 TraceDistiller) and the
// "Distillation & capability-growth contracts" in docs/architecture/contracts/README.md.
package distiller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/writeback"
)

// Candidate kinds — the typed fan-out. memory routes through the ledger; the rest are candidate
// records; the last three are the self-improving signals that close the loop.
const (
	KindMemory        = "memory"
	KindSkill         = "skill"
	KindDecision      = "decision"
	KindFailure       = "failure_pattern"
	KindTaste         = "taste"
	KindProduct       = "product_hypothesis"
	KindRoute         = "route_observation"
	KindCapabilityObs = "capability_observation"
	KindMethodUpgrade = "method_upgrade"
	KindEvalCase      = "eval_case"
	KindPolicyPatch   = "policy_patch"
)

// Kinds is the typed allowlist. An unknown kind is rejected — it must never flow into a filesystem
// path (the inbox is keyed by kind), and a typed fan-out must stay typed.
var Kinds = []string{
	KindMemory, KindSkill, KindDecision, KindFailure, KindTaste, KindProduct,
	KindRoute, KindCapabilityObs, KindMethodUpgrade, KindEvalCase, KindPolicyPatch,
}

// Verdicts an adjudication can reach.
const (
	Reject         = "reject"
	Park           = "park"
	Promote        = "promote"
	Operationalize = "operationalize"
	Retire         = "retire"
)

// confidenceFloor: below this, a candidate parks regardless of kind (low-confidence never promotes).
const confidenceFloor = 0.5

// Candidate is a typed, untrusted extraction. memory-kind carries entity/predicate/object; other
// kinds carry subject/statement/detail. All cite evidence and the policy version that produced them.
type Candidate struct {
	ID             string                    `json:"id"`
	Kind           string                    `json:"kind"`
	Entity         string                    `json:"entity,omitempty"`
	Predicate      string                    `json:"predicate,omitempty"`
	Object         string                    `json:"object,omitempty"`
	Subject        string                    `json:"subject,omitempty"`
	Statement      string                    `json:"statement,omitempty"`
	Detail         string                    `json:"detail,omitempty"`
	Confidence     float64                   `json:"confidence"`
	Derivation     string                    `json:"derivation"`
	ClaimType      string                    `json:"claim_type,omitempty"` // memory-kind: the claim taxonomy (mem.ClaimTypes)
	Qualifiers     map[string]string         `json:"qualifiers,omitempty"` // memory-kind: e.g. {"domain": "languages"} → Topic()
	Tags           []string                  `json:"tags,omitempty"`
	Salience       string                    `json:"salience,omitempty"`
	Expectation    *mem.CandidateExpectation `json:"expectation,omitempty"`
	ExpiresAt      string                    `json:"expires_at,omitempty"`
	Visibility     string                    `json:"visibility,omitempty"`
	State          string                    `json:"state,omitempty"`
	CreatedAt      string                    `json:"created_at,omitempty"`
	PromotionBasis string                    `json:"promotion_basis,omitempty"` // memory-kind: the basis that allows promotion (candidates.PromotionBases); empty = none
	EvidenceRefs   []string                  `json:"evidence_refs"`
	PolicyVersion  string                    `json:"policy_version"`
	Verdict        string                    `json:"verdict,omitempty"` // stamped at adjudication, so a consumer respects it
}

// Promotion modes: in live_candidate_only (the DEFAULT for real traces) a memory candidate parks
// unless it carries a promotion basis — a model extraction's confidence is never a basis. The
// fixture mode promotes on confidence and is for synthetic fixtures/tests only.
const (
	ModeLiveCandidateOnly  = "live_candidate_only"
	ModeFixtureAutoPromote = "fixture_auto_promote"
)

// Policy is the VERSIONED distillation method — so a method change is legible, not silent prompt drift.
type Policy struct {
	ID            string   `json:"id"`
	Version       int      `json:"version"`
	PromotionMode string   `json:"promotion_mode"`
	Lenses        []string `json:"lenses"`
}

// DefaultPolicy is live_candidate_only: nothing model-extracted promotes to canon without a basis.
func DefaultPolicy() Policy {
	return Policy{ID: "default", Version: 1, PromotionMode: ModeLiveCandidateOnly,
		Lenses: []string{"memory", "skill", "decision", "failure", "capability", "method"}}
}

// FixturePolicy is DefaultPolicy in fixture_auto_promote mode — synthetic fixtures/tests only.
func FixturePolicy() Policy { p := DefaultPolicy(); p.PromotionMode = ModeFixtureAutoPromote; return p }

// PolicyVersionString is the stable stamp every candidate carries.
func (p Policy) Stamp() string { return fmt.Sprintf("%s.v%d", p.ID, p.Version) }

// PriorState is RetrievedPriorState: the canon the critic compares an extraction against, so it can
// tell new from duplicate from supersession/contradiction (distill against canon, not blind).
type PriorState struct {
	PolicyVersion      string
	Current            []mem.Claim
	ClaimsByType       map[string][]mem.Claim
	CandidatesByKind   map[string][]PriorCandidate
	CapabilityNodes    []PriorCapabilityNode
	NoPromoteDecisions []PriorNoPromote
}

func LoadPriorState(store *mem.Store) (PriorState, error) {
	return LoadPriorStateFromHome("", store, DefaultPolicy())
}

// Critique is the per-candidate verdict before adjudication.
type Critique struct {
	IsNew                bool   `json:"is_new"`
	Duplicate            bool   `json:"duplicate"`
	DuplicateClaimID     string `json:"duplicate_claim_id,omitempty"`
	DuplicateCandidateID string `json:"duplicate_candidate_id,omitempty"`
	Supersedes           string `json:"supersedes,omitempty"` // current claim id, for memory kind
	Contradicts          string `json:"contradicts,omitempty"`
	ExposesMethodFailure bool   `json:"exposes_method_failure"`
	Notes                string `json:"notes"`
}

// AdjudicationDecision is the durable verdict a candidate receives.
type AdjudicationDecision struct {
	CandidateID      string   `json:"candidate_id"`
	Kind             string   `json:"kind"`
	Verdict          string   `json:"verdict"`
	Relation         string   `json:"relation,omitempty"`           // vs prior canon: new|duplicate|supersedes|contradicts
	PriorClaimID     string   `json:"prior_claim_id,omitempty"`     // the canon claim the relation points at
	PriorCandidateID string   `json:"prior_candidate_id,omitempty"` // the candidate inbox row the relation points at
	Basis            string   `json:"basis"`                        // the reason text for the verdict
	PromotionBasis   string   `json:"promotion_basis,omitempty"`    // the structured basis that allowed a memory promote
	EvidenceRefs     []string `json:"evidence_refs"`
	PolicyVersion    string   `json:"policy_version"`
	ClaimID          string   `json:"claim_id,omitempty"` // set when a memory candidate promotes to a claim
	NoPromoteID      string   `json:"no_promote_id,omitempty"`
	CreatedAt        string   `json:"created_at"`
}

// Trace is the normalized candidate payload every extractor must produce. Reference fixtures and
// LLM-shaped responses share this shape so policy stamps, evidence refs, kind validation, and
// deterministic IDs cannot drift.
type Trace struct {
	TraceRef   string      `json:"trace_ref"`
	Candidates []Candidate `json:"candidates"`
}

// Extractor turns raw input into normalized Trace candidates.
type Extractor interface {
	Extract(raw []byte, policy Policy) (Trace, error)
}

// ReferenceExtractor parses an already-structured trace and stamps stable ids + the policy version.
type ReferenceExtractor struct{}

func (ReferenceExtractor) Extract(raw []byte, policy Policy) (Trace, error) {
	var t Trace
	if err := json.Unmarshal(raw, &t); err != nil {
		return Trace{}, fmt.Errorf("parse trace: %w", err)
	}
	return normalizeTrace(t, policy)
}

func normalizeTrace(t Trace, policy Policy) (Trace, error) {
	if strings.TrimSpace(t.TraceRef) == "" {
		return Trace{}, fmt.Errorf("trace_ref is required")
	}
	for i := range t.Candidates {
		c := &t.Candidates[i]
		if policy.PromotionMode != ModeFixtureAutoPromote && strings.EqualFold(strings.TrimSpace(c.PromotionBasis), candidates.BasisExplicitConfirmation) {
			// Explicit confirmation is authority minted only by the governed
			// operator-confirmation adapter. Reference/model-shaped traces are
			// evidence, so they cannot carry this reserved basis forward.
			c.PromotionBasis = ""
		}
		if !contains(Kinds, c.Kind) {
			return Trace{}, fmt.Errorf("candidate %d has unknown kind %q (allowed: %v)", i, c.Kind, Kinds)
		}
		if err := validateCandidateShape(i, *c); err != nil {
			return Trace{}, err
		}
		if c.Derivation == "" {
			c.Derivation = "extracted"
		}
		normalizeSeedCandidate(c)
		c.PolicyVersion = policy.Stamp()
		// Stable deterministic id over ALL identity-significant content (incl. Detail, which the
		// capability bridge keys on) ⇒ replay = same ids, and two candidates differing only in
		// Detail get DISTINCT ids (no silent inbox-file collision).
		if isSeedCandidate(*c) {
			c.ID = idFor("cand_", c.Kind, c.Entity, c.Predicate, c.Object, c.Subject, c.Statement, c.Detail, expectationKey(c.Expectation), c.ExpiresAt, t.TraceRef)
		} else {
			c.ID = idFor("cand_", c.Kind, c.Entity, c.Predicate, c.Object, c.Subject, c.Statement, c.Detail, t.TraceRef)
		}
		if len(c.EvidenceRefs) == 0 {
			c.EvidenceRefs = []string{t.TraceRef}
		}
	}
	return t, nil
}

func validateCandidateShape(index int, c Candidate) error {
	if c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("candidate %d confidence %.2f outside [0,1]", index, c.Confidence)
	}
	if c.Kind == KindMemory {
		var missing []string
		if strings.TrimSpace(c.Entity) == "" {
			missing = append(missing, "entity")
		}
		if strings.TrimSpace(c.Predicate) == "" {
			missing = append(missing, "predicate")
		}
		if strings.TrimSpace(c.Object) == "" {
			missing = append(missing, "object")
		}
		if len(missing) > 0 {
			return fmt.Errorf("candidate %d memory requires %s", index, strings.Join(missing, ", "))
		}
		if c.Expectation != nil && (strings.TrimSpace(c.Expectation.Predicate) == "" || strings.TrimSpace(c.Expectation.Object) == "") {
			return fmt.Errorf("candidate %d seed expectation requires predicate and object", index)
		}
		return nil
	}
	var missing []string
	if strings.TrimSpace(c.Subject) == "" {
		missing = append(missing, "subject")
	}
	if strings.TrimSpace(c.Statement) == "" {
		missing = append(missing, "statement")
	}
	if len(missing) > 0 {
		return fmt.Errorf("candidate %d %s requires %s", index, c.Kind, strings.Join(missing, ", "))
	}
	return nil
}

// Critique grades candidate against prior canon and candidate inboxes.
// Memory compares against current claims; every other typed family compares against
// the same family's prior inbox rows so a following/critic agent cannot keep
// reintroducing the same parked insight as new just because the trace changed.
func critique(c Candidate, prior PriorState) Critique {
	if c.Kind != KindMemory {
		if dup, ok := duplicatePriorCandidate(c, prior.CandidatesByKind[c.Kind]); ok {
			return Critique{
				Duplicate:            true,
				DuplicateCandidateID: dup.ID,
				Notes:                "matches prior " + c.Kind + " candidate " + dup.ID,
			}
		}
	}
	if c.Kind == KindEvalCase {
		return Critique{IsNew: true, ExposesMethodFailure: true, Notes: "distiller mistake → eval corpus"}
	}
	if c.Kind != KindMemory {
		return Critique{IsNew: true}
	}
	var exact *mem.Claim
	candidateSlot := mem.ClaimSlotKey(c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, "")
	for i := range prior.Current {
		cl := &prior.Current[i]
		claimSlot := mem.ClaimSlotKey(cl.Entity, cl.Predicate, cl.ClaimType, cl.Qualifiers, cl.ID)
		if claimSlot == candidateSlot && (exact == nil || cl.Seq > exact.Seq) {
			exact = cl
		}
	}
	if exact != nil {
		return critiqueMemorySlot(c, *exact)
	}
	for _, cl := range prior.Current {
		if mem.ClusterKey(cl.Entity, cl.Topic(), cl.ClaimType) == mem.ClusterKey(c.Entity, memoryTopic(c), c.ClaimType) &&
			mem.SameClaimObject(cl.Object, c.Object) && mem.SameClaimQualifiers(cl.Qualifiers, c.Qualifiers) {
			return Critique{Duplicate: true, DuplicateClaimID: cl.ID, Notes: "matches current claim " + cl.ID}
		}
	}
	if dup, ok := duplicatePriorCandidate(c, prior.CandidatesByKind[KindMemory]); ok {
		return Critique{
			Duplicate:            true,
			DuplicateCandidateID: dup.ID,
			Notes:                "matches prior memory candidate " + dup.ID,
		}
	}
	return Critique{IsNew: true}
}

func critiqueMemorySlot(c Candidate, current mem.Claim) Critique {
	if mem.SameClaimObject(current.Object, c.Object) && mem.SameClaimQualifiers(current.Qualifiers, c.Qualifiers) {
		return Critique{Duplicate: true, DuplicateClaimID: current.ID, Notes: "matches current claim " + current.ID}
	}
	if c.Confidence < current.Confidence {
		// A weaker candidate conflicting with stronger canon is a contradiction,
		// not a supersession; it must not silently overwrite current truth.
		return Critique{Contradicts: current.ID, Notes: fmt.Sprintf("conflicts with higher-confidence %s (%.2f < %.2f)", current.ID, c.Confidence, current.Confidence)}
	}
	return Critique{Supersedes: current.ID, Notes: "changes " + current.Object + " → " + c.Object}
}

func normalizeSeedCandidate(c *Candidate) {
	if c == nil || !isSeedCandidate(*c) {
		return
	}
	c.Tags = appendUniqueTag(c.Tags, "seed")
	c.Salience = "low"
	c.Visibility = "contextual"
	c.PromotionBasis = ""
}

func isSeedCandidate(c Candidate) bool {
	if c.Expectation != nil {
		return true
	}
	for _, tag := range c.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "seed") {
			return true
		}
	}
	return false
}

func appendUniqueTag(tags []string, tag string) []string {
	for _, got := range tags {
		if strings.EqualFold(strings.TrimSpace(got), tag) {
			return tags
		}
	}
	return append(tags, tag)
}

func expectationKey(exp *mem.CandidateExpectation) string {
	if exp == nil {
		return ""
	}
	return strings.Join([]string{strings.TrimSpace(exp.Entity), strings.TrimSpace(exp.Predicate), strings.TrimSpace(exp.Object), strings.TrimSpace(exp.Detail)}, "\x00")
}

// adjudicate decides the verdict. Code decides (not the model): low confidence parks; a duplicate
// rejects; new/changed memory promotes; a distiller mistake or a capability observation
// OPERATIONALIZES (into its corpus/graph); a method upgrade or policy patch PARKS (an operator
// reviews — a method change is never auto-applied); other kinds park as candidates.
func adjudicate(c Candidate, crit Critique, mode string) (string, string) {
	if c.Kind == KindMemory && isSeedCandidate(c) {
		return Park, "seed expectation parked at low salience until later evidence corroborates it"
	}
	if crit.Duplicate {
		if crit.DuplicateCandidateID != "" {
			return Reject, "duplicate prior " + c.Kind + " candidate " + crit.DuplicateCandidateID
		}
		return Reject, "duplicate of current canon"
	}
	if c.Confidence < confidenceFloor {
		return Park, fmt.Sprintf("confidence %.2f below floor %.2f", c.Confidence, confidenceFloor)
	}
	switch c.Kind {
	case KindMemory:
		if crit.Contradicts != "" {
			return Park, "contradicts higher-confidence canon " + crit.Contradicts + " — operator review"
		}
		// memory is evidence, not truth: in live mode a model-extracted fact PARKS unless it carries
		// a promotion basis. High confidence is not a basis — that is the whole point.
		if mode != ModeFixtureAutoPromote && !candidates.ValidBasis(c.PromotionBasis) {
			return Park, "no promotion basis (extraction confidence is not a basis) — parked for operator/basis"
		}
		return Promote, "new or superseding memory (basis: " + c.PromotionBasis + ")" // mem ledger performs the actual supersession
	case KindEvalCase:
		return Operationalize, "added to the distiller's eval corpus"
	case KindCapabilityObs:
		return Operationalize, "feeds the capability graph (missed_when/false_triggered_by)"
	case KindMethodUpgrade, KindPolicyPatch:
		return Park, "method/policy change requires operator review (never auto-applied)"
	default:
		return Park, "candidate parked for operationalization/review"
	}
}

// RunResult is the outcome of distilling one trace.
type RunResult struct {
	TraceRef  string                 `json:"trace_ref"`
	Policy    string                 `json:"policy"`
	Decisions []AdjudicationDecision `json:"decisions"`
}

// Run is the loop: load prior state → extract → critique → adjudicate → route. memory candidates
// flow through the mem ledger (which performs the real dup/supersede); all other candidates are
// written as candidate records under COMPOUND_HOME/inbox/<kind>/ and NEVER become claims here.
// Every candidate yields an AdjudicationDecision (appended to the ledger), so nothing is silent.
func Run(homePath string, store *mem.Store, raw []byte, policy Policy, ext Extractor, now time.Time) (RunResult, error) {
	res := RunResult{Policy: policy.Stamp()}
	prior, err := LoadPriorStateFromHome(homePath, store, policy)
	if err != nil {
		return res, err
	}
	t, err := ext.Extract(raw, policy)
	if err != nil {
		return res, err
	}
	res.TraceRef = t.TraceRef

	var writebackStore *writeback.Store
	for _, c := range t.Candidates {
		wroteCandidate := false
		crit := critique(c, prior)
		verdict, basis := adjudicate(c, crit, policy.PromotionMode)
		if c.Kind == KindMemory && verdict == Promote && !candidates.ValidBasis(c.PromotionBasis) && policy.PromotionMode == ModeFixtureAutoPromote {
			c.PromotionBasis = candidates.BasisTrustedDeterministic
			basis = "fixture auto-promote memory (basis: " + c.PromotionBasis + ")"
		}
		c.Verdict = verdict // stamp onto the written record so a consumer respects adjudication
		if c.Kind == KindMemory {
			if strings.TrimSpace(c.CreatedAt) == "" {
				c.CreatedAt = rfc3339(now)
			}
			switch verdict {
			case Park:
				c.State = "parked"
			case Reject:
				c.State = "rejected"
			}
		}
		rel, priorClaim, priorCandidate := "new", "", ""
		switch {
		case crit.Duplicate && crit.DuplicateCandidateID != "":
			rel, priorCandidate = "duplicate", crit.DuplicateCandidateID
		case crit.Duplicate:
			rel, priorClaim = "duplicate", crit.DuplicateClaimID
		case crit.Contradicts != "":
			rel, priorClaim = "contradicts", crit.Contradicts
		case crit.Supersedes != "":
			rel, priorClaim = "supersedes", crit.Supersedes
		}
		dec := AdjudicationDecision{
			CandidateID: c.ID, Kind: c.Kind, Verdict: verdict, Relation: rel, PriorClaimID: priorClaim, PriorCandidateID: priorCandidate,
			Basis: basis, PromotionBasis: c.PromotionBasis, EvidenceRefs: c.EvidenceRefs,
			PolicyVersion: c.PolicyVersion, CreatedAt: rfc3339(now),
		}

		switch {
		case c.Kind == KindMemory && verdict == Promote:
			// route through the mem ledger — it is the source of truth for memory dup/supersede.
			visibility := c.Visibility
			if strings.TrimSpace(visibility) == "" {
				visibility = "operational"
			}
			got, err := store.Capture([]mem.Candidate{{
				Entity: c.Entity, Predicate: c.Predicate, Object: c.Object,
				Confidence: c.Confidence, Derivation: c.Derivation,
				ClaimType: c.ClaimType, Qualifiers: c.Qualifiers, // taxonomy + topic axis
				Tags: c.Tags, Salience: c.Salience, Expectation: c.Expectation, ExpiresAt: c.ExpiresAt,
				PromotionBasis: c.PromotionBasis, // persist the "why" of promotion onto the claim
				SourceTraceRef: t.TraceRef, Visibility: visibility,
			}}, now)
			if err != nil {
				return res, err
			}
			if len(got) == 0 {
				dec.Verdict, dec.Relation, dec.Basis = Reject, "duplicate", "duplicate per mem ledger"
				break
			}
			ar, err := store.Adjudicate(got[0].ID, "promote", "", now)
			if err != nil {
				return res, err
			}
			if ar.Claim != nil {
				dec.ClaimID = ar.Claim.ID
			} else {
				// mem detected a duplicate the prior-state critic missed (race / same-trace dup).
				dec.Verdict, dec.Relation, dec.Basis = Reject, "duplicate", "duplicate per mem ledger"
			}
		case c.Kind == KindMemory && verdict == Reject && crit.Duplicate && crit.DuplicateCandidateID != "":
			// A parked memory candidate is already in the inbox. Do not create a
			// second parked row; the no-promote record carries the repeat evidence.
		case c.Kind == KindMemory && verdict == Reject && crit.Duplicate:
			// Current canon already says this; no candidate row is needed.
		case c.Kind != KindMemory && verdict == Reject && crit.Duplicate:
			// Anti-churn: the adjudication and no-promote row are the durable
			// record. Do not write another candidate inbox file for the same
			// prior meaning.
		default:
			// candidate-first: write the record to its inbox; it never becomes a claim here.
			if c.Kind == KindMemory && strings.TrimSpace(c.Visibility) == "" {
				c.Visibility = "operational"
			}
			if err := writeCandidate(homePath, c); err != nil {
				return res, err
			}
			wroteCandidate = true
		}
		if wroteCandidate && c.Kind != KindMemory {
			prior.CandidatesByKind[c.Kind] = append(prior.CandidatesByKind[c.Kind], priorCandidateFromCandidate(c))
		}
		if needsNoPromote(dec) {
			if writebackStore == nil {
				writebackStore, err = writeback.Open(homePath)
				if err != nil {
					return res, err
				}
			}
			nop, _, err := writebackStore.RecordNoPromote(noPromoteDecision(c, dec, now), now)
			if err != nil {
				return res, err
			}
			dec.NoPromoteID = nop.ID
		}
		if err := appendDecision(homePath, dec); err != nil {
			return res, err
		}
		res.Decisions = append(res.Decisions, dec)
	}
	return res, nil
}

func needsNoPromote(d AdjudicationDecision) bool {
	return d.Verdict == Reject && d.Relation == "duplicate"
}

func noPromoteDecision(c Candidate, d AdjudicationDecision, now time.Time) writeback.NoPromoteDecision {
	ownerTarget := writeback.OwnerTarget{Kind: "candidate_family", Ref: "distiller:" + c.Kind}
	if d.PriorClaimID != "" {
		ownerTarget = writeback.OwnerTarget{Kind: "memory_claim", Ref: d.PriorClaimID}
	} else if d.PriorCandidateID != "" {
		ownerTarget = writeback.OwnerTarget{Kind: "candidate", Ref: d.PriorCandidateID}
	}
	evidenceRefs := append([]string(nil), d.EvidenceRefs...)
	if d.PriorClaimID != "" {
		evidenceRefs = append(evidenceRefs, "prior_claim:"+d.PriorClaimID)
	}
	if d.PriorCandidateID != "" {
		evidenceRefs = append(evidenceRefs, "prior_candidate:"+d.PriorCandidateID)
	}
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CreatedAt:     rfc3339(now),
		CandidateRef:  c.ID,
		CandidateKind: c.Kind,
		OwnerTarget:   ownerTarget,
		Basis:         d.Basis,
		EvidenceRefs:  evidenceRefs,
		PolicyVersion: d.PolicyVersion,
		Status:        writeback.NoPromoteActive,
	}
}

func duplicatePriorCandidate(c Candidate, priors []PriorCandidate) (PriorCandidate, bool) {
	key := candidateMeaningKey(c)
	if key == "" {
		return PriorCandidate{}, false
	}
	for _, prior := range priors {
		if prior.ID == c.ID {
			return prior, true
		}
		if priorCandidateMeaningKey(prior) == key {
			return prior, true
		}
	}
	return PriorCandidate{}, false
}

func candidateMeaningKey(c Candidate) string {
	return meaningKey(c.Kind, c.Entity, c.Predicate, c.Object, c.Subject, c.Statement, c.Detail)
}

func memoryTopic(c Candidate) string {
	if d := c.Qualifiers["domain"]; d != "" {
		return d
	}
	return c.Predicate
}

func priorCandidateMeaningKey(c PriorCandidate) string {
	return meaningKey(c.Kind, c.Entity, c.Predicate, c.Object, c.Subject, c.Statement, c.Detail)
}

func priorCandidateFromCandidate(c Candidate) PriorCandidate {
	return PriorCandidate{
		ID:           c.ID,
		Kind:         c.Kind,
		Subject:      c.Subject,
		Statement:    c.Statement,
		Detail:       c.Detail,
		Entity:       c.Entity,
		Predicate:    c.Predicate,
		Object:       c.Object,
		Verdict:      c.Verdict,
		Confidence:   c.Confidence,
		EvidenceRefs: append([]string(nil), c.EvidenceRefs...),
	}
}

func meaningKey(kind, entity, predicate, object, subject, statement, detail string) string {
	parts := []string{
		norm(kind),
		norm(entity),
		norm(predicate),
		norm(object),
		norm(subject),
		norm(statement),
		norm(detail),
	}
	hasPayload := false
	for _, part := range parts[1:] {
		if part != "" {
			hasPayload = true
			break
		}
	}
	if !hasPayload {
		return ""
	}
	return strings.Join(parts, "\x00")
}

func norm(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// --- persistence (under COMPOUND_HOME) ---

func writeCandidate(homePath string, c Candidate) error {
	if !contains(Kinds, c.Kind) { // belt-and-suspenders: Kind keys a path, so it must be a known typed kind
		return fmt.Errorf("refusing to write candidate with unknown kind %q", c.Kind)
	}
	return candidates.WriteCandidate(homePath, candidates.Kind(c.Kind), c.ID, &c)
}

func appendDecision(homePath string, d AdjudicationDecision) error {
	dir := filepath.Join(homePath, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "adjudications.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(&d)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}
func rfc3339(now time.Time) string { return now.UTC().Format(time.RFC3339) }

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
