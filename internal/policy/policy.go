// Package policy is governance v0: it evaluates an ActionIntent into a PolicyDecision
// in proportion to risk/blast-radius (Law 16; D0 tiers; D3 humans own irreversible).
// The point of the slice is that mutation is GATED, not asked: a mutating adapter must
// hold an allowing PolicyDecision — no model bypasses this by prompt obedience.
// See docs/architecture/contracts/README.md and docs/architecture/transition.md.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
)

// severity ranks outcomes so escalation only ever raises caution, never lowers it.
var severity = map[string]int{"allow": 0, "allow_with_obligations": 1, "hold": 2, "deny": 3}

// ClassRule is the baseline outcome (and obligations) for a mutation class.
type ClassRule struct {
	Outcome     string   `yaml:"outcome" json:"outcome"`
	Obligations []string `yaml:"obligations" json:"obligations"`
}

// StandingGrant is a pre-authorization envelope (D3: humans own irreversible
// decisions by SETTING THE ENVELOPE, not by being paged each time). A matching
// unexpired grant allows only while the action stays inside its scope.
type StandingGrant struct {
	ID                   string `yaml:"id" json:"id"`
	Capability           string `yaml:"capability" json:"capability"`
	MutationClass        string `yaml:"mutation_class" json:"mutation_class"`
	Reversibility        string `yaml:"reversibility,omitempty" json:"reversibility,omitempty"`
	ReversibilityCeiling string `yaml:"reversibility_ceiling,omitempty" json:"reversibility_ceiling,omitempty"`
	BlastRadius          string `yaml:"blast_radius,omitempty" json:"blast_radius,omitempty"`
	TargetPath           string `yaml:"target_path,omitempty" json:"target_path,omitempty"`
	// DesktopSurface names an operator-desktop surface (operator_browser or
	// foreground_gui) this grant lets a lane use. It is the only thing that
	// lifts the desktop-surface hold; no other grant field does.
	DesktopSurface string   `yaml:"desktop_surface,omitempty" json:"desktop_surface,omitempty"`
	MaxUSD         *float64 `yaml:"max_usd,omitempty" json:"max_usd,omitempty"`
	MaxTokens      *int     `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	Destination    string   `yaml:"destination,omitempty" json:"destination,omitempty"`
	ExpiresAt      string   `yaml:"expires_at" json:"expires_at"` // RFC3339; empty = no expiry
	// NotBefore is the RFC3339 instant the grant becomes eligible; empty =
	// eligible from import. A grant is eligible only while not_before <= now < expires_at,
	// so a December set can be imported now and activate itself.
	NotBefore string `yaml:"not_before,omitempty" json:"not_before,omitempty"`
	// RequiresPresence is the operator presence state the grant needs:
	// any (default) | present | away. Presence is a KEY, not a ceiling: it
	// only decides eligibility for grants that ask for a presence state.
	RequiresPresence string `yaml:"requires_presence,omitempty" json:"requires_presence,omitempty"`
	// Reason and Rollback are operator-authored audit text carried verbatim
	// from the grant source; they never affect a decision.
	Reason   string `yaml:"reason,omitempty" json:"reason,omitempty"`
	Rollback string `yaml:"rollback,omitempty" json:"rollback,omitempty"`
}

// Config is the declarative policy. Shipped as a leak-free built-in default; overridable
// from COMPOUND_HOME/config/policy.yaml. Direct edits while autonomous admission is
// available are unsupported: engage the locked global autonomy halt first, edit and
// validate the projection, then clear the halt through the command.
type Config struct {
	ForbiddenPaths               []string             `yaml:"forbidden_paths" json:"forbidden_paths"`
	ClassOutcome                 map[string]ClassRule `yaml:"class_outcome" json:"class_outcome"`
	HumanReversibilities         []string             `yaml:"human_reversibilities" json:"human_reversibilities"`
	WideBlastRadii               []string             `yaml:"wide_blast_radii" json:"wide_blast_radii"`
	StandingGrants               []StandingGrant      `yaml:"standing_grants" json:"standing_grants"`
	PlacementGuardMode           string               `yaml:"placement_guard_mode,omitempty" json:"placement_guard_mode,omitempty"`
	OperatorAutonomyLevel        string               `yaml:"operator_autonomy_level,omitempty" json:"operator_autonomy_level,omitempty"`
	OperatorAutonomyAuthorityRef string               `yaml:"operator_autonomy_authority_ref,omitempty" json:"operator_autonomy_authority_ref,omitempty"`
	PolicyEvidenceRefs           []string             `yaml:"policy_evidence_refs,omitempty" json:"policy_evidence_refs,omitempty"`
	// OperatorPresence is the presence window from the operator-state
	// authority. policy.yaml cannot set it (yaml:"-"); only
	// compound://registries/operator-state does, via ApplyOperatorState.
	OperatorPresence *OperatorPresence `yaml:"-" json:"operator_presence,omitempty"`
}

// DefaultConfig satisfies the three documented proof cases:
// local_reversible -> allow_with_obligations; forbidden path -> deny;
// public/financial/destructive/security -> hold. Unknown class fails closed to hold.
// EvaluationContext carries live observations that can only raise caution.
// The core policy stays declarative; runtime adapters provide current evidence.
type EvaluationContext struct {
	SourceControl *SourceControlContext  `json:"source_control,omitempty"`
	Placement     *PlacementGuardContext `json:"placement,omitempty"`
	Authority     *AuthorityContext      `json:"authority,omitempty"`
}

// SourceControlContext combines a read-only git observation with live-session path
// attribution supplied by the hook adapter.
type SourceControlContext struct {
	EntityRef                  string                         `json:"entity_ref,omitempty"`
	GitRoot                    string                         `json:"git_root,omitempty"`
	Remote                     string                         `json:"remote,omitempty"`
	Head                       string                         `json:"head,omitempty"`
	Status                     string                         `json:"status,omitempty"`
	Staged                     int                            `json:"staged,omitempty"`
	Unstaged                   int                            `json:"unstaged,omitempty"`
	Untracked                  int                            `json:"untracked,omitempty"`
	ChangedPaths               int                            `json:"changed_paths,omitempty"`
	TargetPath                 string                         `json:"target_path,omitempty"`
	TargetStatus               string                         `json:"target_status,omitempty"`
	SessionOwnershipKnown      bool                           `json:"session_ownership_known,omitempty"`
	SessionOwnedChangedPaths   int                            `json:"session_owned_changed_paths,omitempty"`
	SessionForeignChangedPaths int                            `json:"session_foreign_changed_paths,omitempty"`
	SessionOwnsTarget          bool                           `json:"session_owns_target,omitempty"`
	ClaimedWorkID              string                         `json:"claimed_work_id,omitempty"`
	ChangedStateFingerprint    string                         `json:"changed_state_fingerprint,omitempty"`
	ReviewedMutation           *ReviewedSourceControlMutation `json:"reviewed_mutation,omitempty"`
	EvidenceRefs               []string                       `json:"evidence_refs,omitempty"`
}

// ReviewedSourceControlMutation is an exact, recorded capability. The authority
// adapter verifies the immutable plan/report pair before constructing it; policy
// independently rechecks every field available on the live ActionIntent/snapshot.
type ReviewedSourceControlMutation struct {
	Status                  string   `json:"status"`
	Decision                string   `json:"decision"`
	ReviewID                string   `json:"review_id"`
	PlanID                  string   `json:"plan_id,omitempty"`
	PlanDigest              string   `json:"plan_digest,omitempty"`
	AuthorizationDigest     string   `json:"authorization_digest,omitempty"`
	ReviewedBy              string   `json:"reviewed_by,omitempty"`
	OwnerWorkID             string   `json:"owner_work_id"`
	ActionWorkID            string   `json:"action_work_id"`
	RepoID                  string   `json:"repo_id"`
	GitRoot                 string   `json:"git_root"`
	Remote                  string   `json:"remote,omitempty"`
	Head                    string   `json:"head"`
	ChangedStateFingerprint string   `json:"changed_state_fingerprint"`
	MutationInputHash       string   `json:"mutation_input_hash"`
	Capability              string   `json:"capability"`
	Action                  string   `json:"action"`
	MutationClass           string   `json:"mutation_class"`
	TargetPaths             []string `json:"target_paths"`
	EvidenceRefs            []string `json:"evidence_refs,omitempty"`
}

// PlacementGuardContext is owner-placement evidence for an intended mutation.
// It stages observe -> warn -> block without creating another guard path.
type PlacementGuardContext struct {
	Mode           string   `json:"mode,omitempty"`
	Confidence     string   `json:"confidence,omitempty"`
	OwnerRef       string   `json:"owner_ref,omitempty"`
	TargetRef      string   `json:"target_ref,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	RedirectTarget string   `json:"redirect_target,omitempty"` // where the write SHOULD go (canonical owner home / governed door)
	OpenWrite      bool     `json:"open_write,omitempty"`
	Override       bool     `json:"override,omitempty"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`
}

// DefaultConfig returns the built-in conservative policy.
func DefaultConfig() Config {
	return Config{
		ForbiddenPaths: []string{"**/.git/**", "**/.env", "**/.env.*", "**/*.key", "**/id_rsa*", "**/*secret*"},
		ClassOutcome: map[string]ClassRule{
			"read":                 {Outcome: "allow"},
			"local_reversible":     {Outcome: "allow_with_obligations", Obligations: []string{"emit a Receipt", "record a rollback_plan"}},
			"external_side_effect": {Outcome: "hold", Obligations: []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"}},
			"public":               {Outcome: "hold", Obligations: []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"}},
			"destructive":          {Outcome: "hold", Obligations: []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"}},
			"financial":            {Outcome: "hold", Obligations: []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"}},
			"security":             {Outcome: "hold", Obligations: []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"}},
		},
		HumanReversibilities: []string{"irreversible", "hard", "none"},
		WideBlastRadii:       []string{"production", "public", "account", "fleet", "org", "user-data"},
		PlacementGuardMode:   "observe",
	}
}

// LoadConfig returns the policy from COMPOUND_HOME/config/policy.yaml if present, else
// the built-in default. An empty homePath always falls back to the default.
func LoadConfig(homePath string) (Config, string, error) {
	if homePath != "" {
		p := filepath.Join(homePath, "config", "policy.yaml")
		if data, err := os.ReadFile(p); err == nil {
			c := DefaultConfig()
			if err := yaml.Unmarshal(data, &c); err != nil {
				return Config{}, p, fmt.Errorf("parse %s: %w", p, err)
			}
			applyPlacementGuardEnv(&c)
			if err := ApplyOperatorStateFromHome(&c, homePath); err != nil {
				return Config{}, p, err
			}
			if err := ApplyStandingGrantsFromHome(&c, homePath); err != nil {
				return Config{}, p, err
			}
			return c, p, nil
		}
	}
	c := DefaultConfig()
	applyPlacementGuardEnv(&c)
	if err := ApplyOperatorStateFromHome(&c, homePath); err != nil {
		return Config{}, "built-in", err
	}
	if err := ApplyStandingGrantsFromHome(&c, homePath); err != nil {
		return Config{}, "built-in", err
	}
	return c, "built-in", nil
}

// Evaluate decides an ActionIntent. Precedence: forbidden path (deny) > scoped
// standing grant (allow) > class baseline and risk/blast escalation.
func Evaluate(ai envelope.ActionIntent, cfg Config, now time.Time) envelope.PolicyDecision {
	return EvaluateWithContext(ai, cfg, EvaluationContext{}, now)
}

// EvaluateWithContext applies live runtime observations after the declarative
// baseline. Context can only raise caution or add evidence; it never relaxes a
// decision.
func EvaluateWithContext(ai envelope.ActionIntent, cfg Config, ctx EvaluationContext, now time.Time) envelope.PolicyDecision {
	pd := evaluateBase(ai, cfg, now)
	pd = applyActionSecurityFloor(pd, ai, now)
	// The envelope sits between the declarative baseline and the live context on
	// purpose. Before it, a clearable hold is still a hold. After it, context can
	// raise caution again -- a dirty repo or a placement miss re-escalates a
	// cleared action, which keeps "context only ever raises" true.
	pd = applyAutonomyEnvelope(pd, ai, cfg, now)
	pd = applyDesktopSurfaceObligation(pd, ai, now)
	if ctx.SourceControl != nil {
		pd = applySourceControlContext(pd, ai, cfg, *ctx.SourceControl, now)
	}
	if ctx.Placement != nil {
		pd = applyPlacementGuardContext(pd, ai, cfg, *ctx.Placement, now)
	}
	if ctx.Authority != nil {
		pd = applyAuthorityContext(pd, ai, cfg, *ctx.Authority, now)
	}
	pd = applyExactApprovalRequirement(pd, ai, now)
	return pd
}

// An explicit exact approval requirement cannot be cleared by a standing grant
// or an autonomy target envelope. It is not a security floor: the existing
// exact-approval admission door may clear this hold, while deny and action
// security floors retain their stronger semantics.
func applyExactApprovalRequirement(pd envelope.PolicyDecision, ai envelope.ActionIntent, now time.Time) envelope.PolicyDecision {
	if !ai.RequiresExactApproval || !Allowed(pd) {
		return pd
	}
	pd.Outcome = "hold"
	pd.Reasons = append(pd.Reasons, "action requires exact one-use operator approval")
	pd.StandingGrantID = ""
	pd.ExpiresAt = ""
	pd.ID = idFor("pol_", ai.ID, "hold", "exact-approval-required", ts(now))
	return pd
}

func applyActionSecurityFloor(pd envelope.PolicyDecision, ai envelope.ActionIntent, now time.Time) envelope.PolicyDecision {
	floor := strings.TrimSpace(ai.SecurityFloorOutcome)
	if floor != "hold" && floor != "deny" {
		return pd
	}
	for _, reason := range ai.SecurityReasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			pd.Reasons = append(pd.Reasons, "action security floor: "+reason)
		}
	}
	if severity[pd.Outcome] < severity[floor] {
		pd.Outcome = floor
		pd.Obligations = nil
		pd.StandingGrantID = ""
		pd.ExpiresAt = ""
		pd.ID = idFor("pol_", ai.ID, floor, "action-security-floor", ts(now))
	}
	return pd
}

func evaluateBase(ai envelope.ActionIntent, cfg Config, now time.Time) envelope.PolicyDecision {
	// 1. forbidden path -> deny (hard safety; beats even a standing grant).
	if p := ai.Target.Path; p != "" {
		if pat, ok := matchForbidden(cfg.ForbiddenPaths, p); ok {
			return withPolicyEvidence(mkDecision(ai, "deny",
				[]string{fmt.Sprintf("target path %q matches forbidden pattern %q", p, pat)}, nil, "", "", now), cfg)
		}
	}

	outcome, reasons, obligations := baselineOutcome(ai, cfg)

	// 2. standing grant -> allow only inside its explicit envelope. A non-read grant
	//    still carries its class's trust duties (Receipt, rollback_plan) so the
	//    autonomous/unattended path keeps its accountability -- the one path where
	//    nothing else guards the write. This does NOT gate autonomy: allow_with_obligations
	//    is still Allowed(), so the granted action flows without a per-item operator gate.
	//    A granted read needs no such duty and stays a bare allow.
	if g, missReasons, ok := matchGrant(cfg.StandingGrants, ai, cfg, now); ok {
		reason := fmt.Sprintf("covered by standing grant %s (capability=%q class=%q)", g.ID, g.Capability, g.MutationClass)
		if p := strings.TrimSpace(g.RequiresPresence); p != "" && p != PresenceAny {
			reason += fmt.Sprintf(" requires_presence=%s", p)
		}
		if ai.MutationClass == "read" {
			return withPolicyEvidence(mkDecision(ai, "allow",
				[]string{reason}, nil, g.ID, g.ExpiresAt, now), cfg)
		}
		return withPolicyEvidence(mkDecision(ai, "allow_with_obligations",
			[]string{reason}, grantObligations(ai, cfg), g.ID, g.ExpiresAt, now), cfg)
	} else if len(missReasons) > 0 {
		reasons = append(reasons, missReasons...)
	}

	return withPolicyEvidence(mkDecision(ai, outcome, reasons, obligations, "", "", now), cfg)
}

func baselineOutcome(ai envelope.ActionIntent, cfg Config) (string, []string, []string) {
	var reasons []string
	outcome := "hold"
	base, known := cfg.ClassOutcome[ai.MutationClass]
	if known {
		outcome = base.Outcome
		reasons = append(reasons, fmt.Sprintf("baseline for mutation_class %q is %s", ai.MutationClass, base.Outcome))
	} else {
		reasons = append(reasons, fmt.Sprintf("mutation_class %q has no rule; failing closed to hold", ai.MutationClass))
	}
	if level := strings.TrimSpace(cfg.OperatorAutonomyLevel); level != "" && ai.MutationClass != "read" {
		source := strings.TrimSpace(cfg.OperatorAutonomyAuthorityRef)
		if source == "" {
			source = "policy config"
		}
		reasons = append(reasons, fmt.Sprintf("operator autonomy ceiling %s from %s applies", level, source))
	}

	// 4. reversibility escalation (D3): a non-read mutation that is operator-owned-reversibility
	//    escalates to at least hold.
	if ai.MutationClass != "read" && contains(cfg.HumanReversibilities, ai.Reversibility) {
		if up := escalate(outcome, "hold"); up != outcome {
			reasons = append(reasons, fmt.Sprintf("reversibility %q is operator-owned; escalated to hold", ai.Reversibility))
			outcome = up
		}
	}

	// 4b. blast-radius escalation: a non-read mutation with a wide blast radius escalates to
	//     at least hold even if its class baseline was softer.
	if ai.MutationClass != "read" && contains(cfg.WideBlastRadii, ai.BlastRadius) {
		if up := escalate(outcome, "hold"); up != outcome {
			reasons = append(reasons, fmt.Sprintf("blast_radius %q is wide; escalated to hold", ai.BlastRadius))
			outcome = up
		}
	}

	// 5. obligations attach only to allow_with_obligations (Validate requires >= 1).
	var obligations []string
	if outcome == "allow_with_obligations" {
		if known {
			obligations = append(obligations, base.Obligations...)
		}
		if len(obligations) == 0 {
			obligations = append(obligations, "emit a Receipt")
		}
	}

	return outcome, reasons, obligations
}

// grantObligations returns the trust duties a granted non-read mutation still carries:
// its class's baseline obligations, or at minimum a Receipt so the autonomous path stays
// accountable (allow_with_obligations requires >= 1 obligation).
func grantObligations(ai envelope.ActionIntent, cfg Config) []string {
	var obligations []string
	if rule, known := cfg.ClassOutcome[ai.MutationClass]; known {
		obligations = append(obligations, rule.Obligations...)
	}
	if len(obligations) == 0 {
		obligations = append(obligations, "emit a Receipt")
	}
	return obligations
}

func applySourceControlContext(pd envelope.PolicyDecision, ai envelope.ActionIntent, cfg Config, sc SourceControlContext, now time.Time) envelope.PolicyDecision {
	if len(sc.EvidenceRefs) > 0 {
		pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, sc.EvidenceRefs...)
	}
	if ai.MutationClass == "read" || strings.TrimSpace(sc.Status) != "dirty" {
		return pd
	}
	if reviewedSourceControlMutationMatches(ai, sc) {
		pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, sc.ReviewedMutation.EvidenceRefs...)
		pd.Reasons = append(pd.Reasons, fmt.Sprintf("exact recorded source-control review %s matches repo, full HEAD, changed-state fingerprint, action work, session-bound input, action/class/capability, and target set; dirty escalation not added", sc.ReviewedMutation.ReviewID))
		return pd
	}
	targetStatus := sourceControlTargetStatus(ai, sc)
	if claimedWorkID := strings.TrimSpace(sc.ClaimedWorkID); claimedWorkID != "" {
		pd.Reasons = append(pd.Reasons, fmt.Sprintf("all %d dirty source-control path(s) attributed to claimed WorkEnvelope %s through exact Git-delivery worktree provenance; dirty escalation not added", sc.ChangedPaths, claimedWorkID))
		return pd
	}
	if sc.SessionOwnershipKnown && sc.SessionOwnedChangedPaths > 0 && sc.SessionForeignChangedPaths == 0 {
		pd.Reasons = append(pd.Reasons, fmt.Sprintf("all %d dirty source-control path(s) are attributed to the current governed session; dirty escalation not added", sc.SessionOwnedChangedPaths))
		return pd
	}
	if !sc.SessionOwnershipKnown && targetStatus == "clean" {
		return pd
	}
	// Inside an in-force autonomy envelope the foreign-dirty rule narrows from
	// "the repo has unowned changes" to "this action would capture or destroy
	// them". The old rule keyed on the STATE OF THE REPO rather than on what the
	// action touches, so an operator carrying work in progress across fifteen
	// repositories had every lane held on `git fetch`, on a test run, and on an
	// install -- commands that cannot alter a working tree at all. A hold that
	// fires where the harm is impossible is a broader gate, not a narrower one,
	// and it is what made this read as friction rather than protection. The
	// capturing shapes (see wip_capture_shell.go) still hold, unchanged.
	if autonomyEnvelopeCovers(ai, cfg, now) && !intentCapturesForeignWorkInProgress(ai, sc) {
		pd.Reasons = append(pd.Reasons, fmt.Sprintf(
			"%d foreign dirty source-control path(s) present, but this action neither captures nor destroys working-tree changes; autonomy envelope %s leaves the dirty escalation off",
			sc.SessionForeignChangedPaths, AutonomyLevelFullProjectScope))
		return pd
	}
	entity := strings.TrimSpace(sc.EntityRef)
	if entity == "" {
		entity = "target repo"
	}
	reason := fmt.Sprintf("source_control_status dirty for %s changed_paths=%d; target_status %s for %q", entity, sc.ChangedPaths, targetStatus, ai.Target.Path)
	if sc.SessionOwnershipKnown {
		reason += fmt.Sprintf("; session_owned_changed_paths=%d session_foreign_changed_paths=%d; target mutation requires every dirty path to belong to the current session or explicitly reviewed repo state", sc.SessionOwnedChangedPaths, sc.SessionForeignChangedPaths)
	} else {
		reason += "; target mutation requires a clean target or explicitly reviewed repo state"
	}
	if sc.ReviewedMutation != nil {
		reason += "; recorded review did not exactly match the live state and mutation"
	}
	pd.Reasons = append(pd.Reasons, reason)
	if severity[pd.Outcome] < severity["hold"] {
		pd.Outcome = "hold"
		pd.Obligations = nil
		pd.StandingGrantID = ""
		pd.ExpiresAt = ""
		pd.ID = idFor("pol_", ai.ID, "hold", "source-control-dirty", ts(now))
	}
	return pd
}

func sourceControlTargetStatus(ai envelope.ActionIntent, sc SourceControlContext) string {
	status := strings.TrimSpace(sc.TargetStatus)
	if status != "clean" && status != "dirty" {
		return "unknown"
	}
	target := strings.TrimSpace(sc.TargetPath)
	actionTarget := strings.TrimSpace(ai.Target.Path)
	if target == "" || actionTarget == "" || filepath.Clean(target) != filepath.Clean(actionTarget) {
		return "unknown"
	}
	return status
}

func reviewedSourceControlMutationMatches(ai envelope.ActionIntent, sc SourceControlContext) bool {
	review := sc.ReviewedMutation
	if review == nil || review.Status != "recorded" || review.Decision != "authorize_mutation" {
		return false
	}
	if strings.TrimSpace(review.ReviewID) == "" || strings.TrimSpace(review.PlanDigest) == "" || strings.TrimSpace(review.AuthorizationDigest) == "" ||
		!HasOperatorProvenance(review.ReviewedBy) || strings.TrimSpace(review.OwnerWorkID) == "" {
		return false
	}
	if review.RepoID != sc.EntityRef || strings.TrimSpace(sc.GitRoot) == "" || filepath.Clean(review.GitRoot) != filepath.Clean(sc.GitRoot) || review.Remote != sc.Remote || review.Head != sc.Head || review.ChangedStateFingerprint != sc.ChangedStateFingerprint {
		return false
	}
	if review.ActionWorkID != ai.IDs.WorkID || review.Capability != ai.Capability || review.Action != ai.Action || review.MutationClass != ai.MutationClass {
		return false
	}
	if !contains(review.TargetPaths, filepath.Clean(ai.Target.Path)) || !contains(ai.EvidenceRefs, review.MutationInputHash) {
		return false
	}
	return strings.HasPrefix(review.MutationInputHash, "mutation-input:sha256:")
}

// Allowed reports whether a decision permits the action to proceed.
func Allowed(pd envelope.PolicyDecision) bool {
	return pd.Outcome == "allow" || pd.Outcome == "allow_with_obligations"
}

// Blocked is the gate error for a non-allowing decision.
type Blocked struct {
	Outcome string
	Reasons []string
}

func (b *Blocked) Error() string {
	if len(b.Reasons) == 0 {
		return "blocked by policy: " + b.Outcome
	}
	return fmt.Sprintf("blocked by policy (%s): %s", b.Outcome, strings.Join(b.Reasons, "; "))
}

// Gate is the enforcement seam an adapter calls before mutating: nil iff the decision
// allows; otherwise a *Blocked error. This is what makes governance enforced, not asked.
func Gate(pd envelope.PolicyDecision) error {
	if Allowed(pd) {
		return nil
	}
	return &Blocked{Outcome: pd.Outcome, Reasons: pd.Reasons}
}

func withPolicyEvidence(pd envelope.PolicyDecision, cfg Config) envelope.PolicyDecision {
	if len(cfg.PolicyEvidenceRefs) > 0 {
		pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, cfg.PolicyEvidenceRefs...)
	}
	return pd
}

func applyPlacementGuardContext(pd envelope.PolicyDecision, ai envelope.ActionIntent, cfg Config, pg PlacementGuardContext, now time.Time) envelope.PolicyDecision {
	if len(pg.EvidenceRefs) > 0 {
		pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, pg.EvidenceRefs...)
	}
	if ai.MutationClass == "read" {
		return pd
	}
	mode, unknownMode := placementGuardMode(pg.Mode, cfg.PlacementGuardMode)
	confidence := strings.ToLower(strings.TrimSpace(pg.Confidence))
	reason := placementGuardReason(mode, confidence, pg)
	if unknownMode != "" {
		pd.Reasons = append(pd.Reasons, reason+fmt.Sprintf("; unknown mode %q, fail-open observe", unknownMode))
		return pd
	}
	switch {
	case pg.Override || placementGuardOverrideEnv():
		pd.Reasons = append(pd.Reasons, reason+"; PLACEMENT_OVERRIDE active, fail-open")
		return pd
	case pg.OpenWrite:
		pd.Reasons = append(pd.Reasons, reason+"; OPEN_WRITE declared, placement guard does not block")
		return pd
	case confidence != "block":
		pd.Reasons = append(pd.Reasons, reason+"; confidence below block threshold")
		return pd
	}
	switch mode {
	case "observe":
		pd.Reasons = append(pd.Reasons, reason+"; observe mode records only")
	case "warn":
		pd.Reasons = append(pd.Reasons, reason+"; warn mode requires explicit receipt")
		if severity[pd.Outcome] < severity["allow_with_obligations"] {
			pd.Outcome = "allow_with_obligations"
			pd.ID = idFor("pol_", ai.ID, "warn", "placement-guard", ts(now))
		}
		if severity[pd.Outcome] <= severity["allow_with_obligations"] {
			pd.Obligations = mergeStrings(pd.Obligations, "record placement warning")
		}
	case "block":
		pd.Reasons = append(pd.Reasons, reason+"; block mode holds confidence:block placement")
		if severity[pd.Outcome] < severity["hold"] {
			pd.Outcome = "hold"
			pd.Obligations = nil
			pd.StandingGrantID = ""
			pd.ExpiresAt = ""
			pd.ID = idFor("pol_", ai.ID, "hold", "placement-guard", ts(now))
		}
	}
	return pd
}

func placementGuardMode(values ...string) (string, string) {
	for _, value := range values {
		mode := strings.ToLower(strings.TrimSpace(value))
		if mode == "" {
			continue
		}
		switch mode {
		case "observe", "warn", "block":
			return mode, ""
		default:
			return "observe", mode
		}
	}
	return "observe", ""
}

func placementGuardReason(mode, confidence string, pg PlacementGuardContext) string {
	owner := strings.TrimSpace(pg.OwnerRef)
	if owner == "" {
		owner = "unknown-owner"
	}
	target := strings.TrimSpace(pg.TargetRef)
	if target == "" {
		target = "target"
	}
	if confidence == "" {
		confidence = "unknown"
	}
	reason := fmt.Sprintf("placement_guard mode=%s confidence=%s owner=%s target=%s", mode, confidence, owner, target)
	if detail := strings.TrimSpace(pg.Reason); detail != "" {
		reason += "; " + detail
	}
	if redirect := strings.TrimSpace(pg.RedirectTarget); redirect != "" {
		reason += "; canonical target: " + redirect
	}
	return reason
}

func applyPlacementGuardEnv(c *Config) {
	mode := strings.TrimSpace(os.Getenv("COMPOUND_PLACEMENT_GUARD_MODE"))
	if mode == "" {
		mode = strings.TrimSpace(os.Getenv("PLACEMENT_GUARD_MODE"))
	}
	if mode != "" {
		c.PlacementGuardMode = mode
	}
}

func placementGuardOverrideEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PLACEMENT_OVERRIDE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// --- internals ---

func mkDecision(ai envelope.ActionIntent, outcome string, reasons, obligations []string, grantID, expiresAt string, now time.Time) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            idFor("pol_", ai.ID, outcome, ts(now)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: ai.IDs.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       orDefault(ai.Privacy, "private"),
			Risk:          orDefault(ai.Risk, "tier1"),
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID:  ai.ID,
		Outcome:         outcome,
		Reasons:         orEmpty(reasons),
		Obligations:     orEmpty(obligations),
		StandingGrantID: grantID,
		ExpiresAt:       expiresAt,
	}
}

func matchForbidden(patterns []string, p string) (string, bool) {
	p = filepath.ToSlash(p)
	for _, pat := range patterns {
		re, err := regexp.Compile(globToRegexp(filepath.ToSlash(pat)))
		if err != nil {
			continue
		}
		if re.MatchString(p) {
			return pat, true
		}
	}
	return "", false
}

// matchGrant returns the covering grant. When several grants cover one intent the
// most tightly scoped one wins (destination and target_path bound), ties broken by
// registry order. Registry order alone is not authority: an unscoped product-wide
// grant imported earlier must not shadow the exact per-door grant an executor was
// built to consume (2026-10-07: a product loop's promo-text door refused every apply
// as wrong grant because the broad metadata grant sat seven rows above its own).
func matchGrant(grants []StandingGrant, ai envelope.ActionIntent, cfg Config, now time.Time) (StandingGrant, []string, bool) {
	var misses []string
	var best StandingGrant
	bestScope, found := -1, false
	for _, g := range grants {
		if g.Capability != "" && g.Capability != ai.Capability {
			continue
		}
		if g.MutationClass != "" && g.MutationClass != ai.MutationClass {
			continue
		}
		if g.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, g.ExpiresAt); err == nil && !now.Before(t) {
				misses = append(misses, fmt.Sprintf("standing grant %q expired at %s", grantID(g), g.ExpiresAt))
				continue
			}
		}
		if miss, inactive := grantNotYetActive(g, now); inactive {
			misses = append(misses, miss)
			continue
		}
		reasons := grantEnvelopeMisses(g, ai, cfg, now)
		if len(reasons) != 0 {
			misses = append(misses, reasons...)
			continue
		}
		if scope := grantScope(g); scope > bestScope {
			best, bestScope, found = g, scope, true
		}
	}
	if found {
		return best, nil, true
	}
	return StandingGrant{}, misses, false
}

// grantScope counts the binding scopes a grant carries: a destination and a
// target_path each narrow the envelope, so a grant with both outranks one with
// neither when both cover the same intent.
func grantScope(g StandingGrant) int {
	scope := 0
	if strings.TrimSpace(g.Destination) != "" {
		scope++
	}
	if strings.TrimSpace(g.TargetPath) != "" {
		scope++
	}
	return scope
}

func grantEnvelopeMisses(g StandingGrant, ai envelope.ActionIntent, cfg Config, now time.Time) []string {
	var reasons []string
	id := grantID(g)
	if miss, ok := grantPresenceMiss(g, cfg, now); ok {
		reasons = append(reasons, miss)
	}
	if g.ReversibilityCeiling != "" {
		if !reversibilityWithinCeiling(ai.Reversibility, g.ReversibilityCeiling) {
			reasons = append(reasons, fmt.Sprintf("standing grant %q covers reversibility up to %q, not %q", id, g.ReversibilityCeiling, ai.Reversibility))
		}
	} else if g.Reversibility != "" && g.Reversibility != ai.Reversibility {
		reasons = append(reasons, fmt.Sprintf("standing grant %q covers reversibility %q, not %q", id, g.Reversibility, ai.Reversibility))
	} else if g.Reversibility == "" && ai.MutationClass != "read" && contains(cfg.HumanReversibilities, ai.Reversibility) {
		reasons = append(reasons, fmt.Sprintf("standing grant %q does not cover reversibility %q", id, ai.Reversibility))
	}
	if g.BlastRadius != "" && g.BlastRadius != ai.BlastRadius {
		reasons = append(reasons, fmt.Sprintf("standing grant %q covers blast_radius %q, not %q", id, g.BlastRadius, ai.BlastRadius))
	} else if g.BlastRadius == "" && ai.MutationClass != "read" && contains(cfg.WideBlastRadii, ai.BlastRadius) {
		reasons = append(reasons, fmt.Sprintf("standing grant %q does not cover blast_radius %q", id, ai.BlastRadius))
	}
	if g.TargetPath != "" {
		if ai.Target.Path == "" {
			reasons = append(reasons, fmt.Sprintf("standing grant %q target path empty outside target_path %q", id, g.TargetPath))
		} else if _, ok := matchForbidden([]string{g.TargetPath}, ai.Target.Path); !ok {
			reasons = append(reasons, fmt.Sprintf("standing grant %q target path %q outside target_path %q", id, ai.Target.Path, g.TargetPath))
		}
	}
	if g.Destination != "" && strings.TrimSpace(ai.Destination) != strings.TrimSpace(g.Destination) {
		reasons = append(reasons, fmt.Sprintf("standing grant %q covers destination %q, not %q", id, g.Destination, ai.Destination))
	}
	return append(reasons, grantCostMisses(g, ai.EstimatedCost)...)
}

// grantNotYetActive reports a grant whose not_before is still ahead of now.
// An unparseable not_before fails closed: the grant stays inactive, because a
// window the policy cannot read is not a window anyone authorized.
func grantNotYetActive(g StandingGrant, now time.Time) (string, bool) {
	raw := strings.TrimSpace(g.NotBefore)
	if raw == "" {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fmt.Sprintf("standing grant %q has unreadable not_before %q; inactive", grantID(g), g.NotBefore), true
	}
	if now.Before(t) {
		return fmt.Sprintf("standing grant %q not active before %s", grantID(g), raw), true
	}
	return "", false
}

// grantPresenceMiss reports a grant whose requires_presence the operator's
// EFFECTIVE presence at now does not satisfy. Presence is a key here: a grant
// that names no presence state (or "any") never misses, so this check alone
// never moves a decision for such a grant. An unknown value fails closed. (The
// one other thing away changes is the L3 envelope, which it only narrows; see
// autonomyEnvelopeNarrowedAway.)
func grantPresenceMiss(g StandingGrant, cfg Config, now time.Time) (string, bool) {
	required := strings.ToLower(strings.TrimSpace(g.RequiresPresence))
	if required == "" || required == PresenceAny {
		return "", false
	}
	effective := cfg.OperatorPresence.Effective(now)
	if required == effective {
		return "", false
	}
	return fmt.Sprintf("standing grant %q requires_presence %q, operator presence is %q", grantID(g), required, effective), true
}

func grantID(g StandingGrant) string {
	if strings.TrimSpace(g.ID) == "" {
		return "(unnamed)"
	}
	return g.ID
}

// globToRegexp converts a path glob (supporting **, *, ?) to an anchored regexp.
// "**/" matches zero or more leading directories (so "**/*.key" matches "secret.key").
func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				if i+2 < len(glob) && glob[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return b.String()
}

func escalate(cur, floor string) string {
	if severity[floor] > severity[cur] {
		return floor
	}
	return cur
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

func ts(now time.Time) string { return strconv.FormatInt(now.UTC().UnixNano(), 10) }

func mergeStrings(base []string, extra ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+len(extra))
	for _, v := range base {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range extra {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
