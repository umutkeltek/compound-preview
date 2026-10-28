// Package writeback governs candidate updates to owner surfaces.
//
// It is the first OwnerWritebackGate / NoPromoteDecision slice: observations
// and candidates may be useful, but they do not become owner writes or repeat
// as "new" without an explicit gate and durable lifecycle record.
package writeback

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/policy"
)

const (
	GateSchemaVersion      = "compound.owner_writeback_gate.v0.1"
	NoPromoteSchemaVersion = "compound.no_promote_decision.v0.1"

	GateAllowed  = "allowed"
	GateHeld     = "held"
	GateDenied   = "denied"
	GateNoPolicy = "no_policy"

	NoPromoteActive  = "active"
	NoPromoteExpired = "expired"
)

// OwnerTarget is the canonical owner surface a candidate would update.
type OwnerTarget struct {
	Kind string `json:"kind"` // e.g. brain_page, project_contract, generated_view, registry
	Ref  string `json:"ref"`
}

// OwnerWritebackGate links candidate -> owner target -> policy.
// Mutating writeback is allowed only when the linked PolicyDecision allows it.
type OwnerWritebackGate struct {
	SchemaVersion     string      `json:"schema_version"`
	ID                string      `json:"id"`
	CreatedAt         string      `json:"created_at"`
	CandidateRef      string      `json:"candidate_ref"`
	CandidateKind     string      `json:"candidate_kind,omitempty"`
	OwnerTarget       OwnerTarget `json:"owner_target"`
	GateBasis         string      `json:"gate_basis"`
	ActionIntentRef   string      `json:"action_intent_ref,omitempty"`
	MutationClass     string      `json:"mutation_class"`
	PolicyDecisionRef string      `json:"policy_decision_ref,omitempty"`
	ReceiptRef        string      `json:"receipt_ref,omitempty"`
	EvidenceRefs      []string    `json:"evidence_refs"`
	Status            string      `json:"status"`
	Reasons           []string    `json:"reasons"`
}

// NoPromoteDecision is a durable "seen, deliberately not promoted" verdict.
// It is distinct from park/reject: park means revisit; no_promote means do not
// re-introduce the same candidate as new while this decision is active.
type NoPromoteDecision struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	CreatedAt     string      `json:"created_at"`
	CandidateRef  string      `json:"candidate_ref"`
	CandidateKind string      `json:"candidate_kind,omitempty"`
	OwnerTarget   OwnerTarget `json:"owner_target,omitempty"`
	Basis         string      `json:"basis"`
	Expiry        string      `json:"expiry,omitempty"`
	EvidenceRefs  []string    `json:"evidence_refs"`
	PolicyVersion string      `json:"policy_version,omitempty"`
	Status        string      `json:"status"`
}

// Store appends writeback lifecycle records under COMPOUND_HOME.
type Store struct {
	home string
}

// Open returns a writeback store rooted in COMPOUND_HOME.
func Open(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	if err := os.MkdirAll(filepath.Join(homePath, "ledger"), 0o755); err != nil {
		return nil, err
	}
	return &Store{home: homePath}, nil
}

func (s *Store) gatesLog() string {
	return filepath.Join(s.home, "ledger", "owner-writeback-gates.jsonl")
}

func (s *Store) noPromoteLog() string {
	return filepath.Join(s.home, "ledger", "no-promote-decisions.jsonl")
}

// EvaluateGate validates the gate and stamps the enforceable status.
func EvaluateGate(g OwnerWritebackGate, pd *envelope.PolicyDecision, now time.Time) (OwnerWritebackGate, error) {
	g = normalizeGate(g, now)
	if err := validateGateShape(g); err != nil {
		return g, err
	}
	if !mutating(g.MutationClass) {
		g.Status = GateAllowed
		g.Reasons = append(g.Reasons, "read-only writeback gate does not require policy")
		return g, nil
	}
	if g.PolicyDecisionRef == "" {
		g.Status = GateNoPolicy
		g.Reasons = append(g.Reasons, "mutating owner writeback requires policy_decision_ref")
		return g, nil
	}
	if pd == nil {
		g.Status = GateNoPolicy
		g.Reasons = append(g.Reasons, "mutating owner writeback requires a valid PolicyDecision")
		return g, nil
	}
	if err := pd.Validate(); err != nil {
		return g, fmt.Errorf("policy decision invalid: %w", err)
	}
	if pd.ID != g.PolicyDecisionRef {
		return g, fmt.Errorf("policy_decision_ref %q does not match policy id %q", g.PolicyDecisionRef, pd.ID)
	}
	if g.ActionIntentRef != "" && pd.ActionIntentID != g.ActionIntentRef {
		return g, fmt.Errorf("action_intent_ref %q does not match policy action_intent_id %q", g.ActionIntentRef, pd.ActionIntentID)
	}
	if !policy.Allowed(*pd) {
		g.Status = gateStatusForPolicy(pd.Outcome)
		g.Reasons = append(g.Reasons, fmt.Sprintf("policy outcome %q does not allow owner writeback", pd.Outcome))
		return g, nil
	}
	g.Status = GateAllowed
	g.Reasons = append(g.Reasons, fmt.Sprintf("policy %s allows mutating owner writeback", pd.ID))
	return g, nil
}

// RecordGate appends a checked gate result. It records both allowed and held
// outcomes because denied writeback is evidence too; adapters enforce by exit
// status before mutating the owner.
func (s *Store) RecordGate(g OwnerWritebackGate) error {
	if g.Status == "" {
		return fmt.Errorf("owner writeback gate status empty; call EvaluateGate first")
	}
	if err := validateGateShape(g); err != nil {
		return err
	}
	return appendJSONL(s.gatesLog(), &g)
}

// RecordNoPromote records a no-promote decision once per active candidate.
// Replays return the existing active decision without appending duplicate ledger
// rows, which is the anti-churn behavior this lifecycle exists to provide.
func (s *Store) RecordNoPromote(d NoPromoteDecision, now time.Time) (NoPromoteDecision, bool, error) {
	d = normalizeNoPromote(d, now)
	if err := validateNoPromote(d, now); err != nil {
		return d, false, err
	}
	existing, ok, err := s.ActiveNoPromote(d.CandidateRef, now)
	if err != nil {
		return d, false, err
	}
	if ok {
		return existing, false, nil
	}
	return d, true, appendJSONL(s.noPromoteLog(), &d)
}

// ActiveNoPromote returns an active no-promote decision for candidateRef.
func (s *Store) ActiveNoPromote(candidateRef string, now time.Time) (NoPromoteDecision, bool, error) {
	file, err := os.Open(s.noPromoteLog())
	if os.IsNotExist(err) {
		return NoPromoteDecision{}, false, nil
	}
	if err != nil {
		return NoPromoteDecision{}, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var d NoPromoteDecision
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			return NoPromoteDecision{}, false, fmt.Errorf("parse no-promote ledger: %w", err)
		}
		if d.CandidateRef != candidateRef {
			continue
		}
		active, err := d.Active(now)
		if err != nil {
			return NoPromoteDecision{}, false, err
		}
		if active {
			return d, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return NoPromoteDecision{}, false, err
	}
	return NoPromoteDecision{}, false, nil
}

// Active reports whether the no-promote decision still suppresses candidate churn.
func (d NoPromoteDecision) Active(now time.Time) (bool, error) {
	if d.Status == NoPromoteExpired {
		return false, nil
	}
	if d.Expiry == "" {
		return true, nil
	}
	expiry, err := time.Parse(time.RFC3339, d.Expiry)
	if err != nil {
		return false, fmt.Errorf("parse no-promote expiry %q: %w", d.Expiry, err)
	}
	return !now.After(expiry), nil
}

func normalizeGate(g OwnerWritebackGate, now time.Time) OwnerWritebackGate {
	if g.SchemaVersion == "" {
		g.SchemaVersion = GateSchemaVersion
	}
	if g.CreatedAt == "" {
		g.CreatedAt = rfc3339(now)
	}
	if g.ID == "" {
		g.ID = idFor("owg_", g.CandidateRef, g.OwnerTarget.Kind, g.OwnerTarget.Ref, g.ActionIntentRef, g.PolicyDecisionRef)
	}
	if g.EvidenceRefs == nil {
		g.EvidenceRefs = []string{}
	}
	if g.Reasons == nil {
		g.Reasons = []string{}
	}
	return g
}

func normalizeNoPromote(d NoPromoteDecision, now time.Time) NoPromoteDecision {
	if d.SchemaVersion == "" {
		d.SchemaVersion = NoPromoteSchemaVersion
	}
	if d.CreatedAt == "" {
		d.CreatedAt = rfc3339(now)
	}
	if d.ID == "" {
		d.ID = idFor("nop_", d.CandidateRef)
	}
	if d.EvidenceRefs == nil {
		d.EvidenceRefs = []string{}
	}
	if d.Status == "" {
		d.Status = NoPromoteActive
	}
	return d
}

func validateGateShape(g OwnerWritebackGate) error {
	var errs []string
	req := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, name+" required")
		}
	}
	req("schema_version", g.SchemaVersion)
	req("id", g.ID)
	req("created_at", g.CreatedAt)
	req("candidate_ref", g.CandidateRef)
	req("owner_target.kind", g.OwnerTarget.Kind)
	req("owner_target.ref", g.OwnerTarget.Ref)
	req("gate_basis", g.GateBasis)
	req("mutation_class", g.MutationClass)
	if g.SchemaVersion != GateSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", g.SchemaVersion, GateSchemaVersion))
	}
	if g.CandidateKind != "" && !candidates.Known(candidates.Kind(g.CandidateKind)) {
		errs = append(errs, fmt.Sprintf("candidate_kind %q unknown", g.CandidateKind))
	}
	if !contains(envelope.MutationClasses, g.MutationClass) {
		errs = append(errs, fmt.Sprintf("mutation_class %q unknown", g.MutationClass))
	}
	if _, err := time.Parse(time.RFC3339, g.CreatedAt); g.CreatedAt != "" && err != nil {
		errs = append(errs, fmt.Sprintf("created_at invalid: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("owner writeback gate invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateNoPromote(d NoPromoteDecision, now time.Time) error {
	var errs []string
	req := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, name+" required")
		}
	}
	req("schema_version", d.SchemaVersion)
	req("id", d.ID)
	req("created_at", d.CreatedAt)
	req("candidate_ref", d.CandidateRef)
	req("basis", d.Basis)
	if d.SchemaVersion != NoPromoteSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", d.SchemaVersion, NoPromoteSchemaVersion))
	}
	if d.CandidateKind != "" && !candidates.Known(candidates.Kind(d.CandidateKind)) {
		errs = append(errs, fmt.Sprintf("candidate_kind %q unknown", d.CandidateKind))
	}
	if len(d.EvidenceRefs) == 0 {
		errs = append(errs, "evidence_refs requires at least one evidence ref")
	}
	if _, err := time.Parse(time.RFC3339, d.CreatedAt); d.CreatedAt != "" && err != nil {
		errs = append(errs, fmt.Sprintf("created_at invalid: %v", err))
	}
	if d.Expiry != "" {
		expiry, err := time.Parse(time.RFC3339, d.Expiry)
		if err != nil {
			errs = append(errs, fmt.Sprintf("expiry invalid: %v", err))
		} else if now.After(expiry) {
			errs = append(errs, "expiry is already elapsed")
		}
	}
	switch d.Status {
	case NoPromoteActive, NoPromoteExpired:
	default:
		errs = append(errs, fmt.Sprintf("status %q unknown", d.Status))
	}
	if len(errs) > 0 {
		return fmt.Errorf("no-promote decision invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func gateStatusForPolicy(outcome string) string {
	if outcome == "deny" {
		return GateDenied
	}
	return GateHeld
}

func mutating(mutationClass string) bool {
	return mutationClass != "read"
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(h[:])[:16]
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
