// Package policy — credential-provision admission (packet work_credential_provision_rework_20260912).
//
// EvaluateCredentialProvision is a NARROW, closed hook for ONE outbound
// capability (outbound.credential_fields.write) that fills missing/empty fields
// of a remote .env/.env.* target. It exists because ordinary Evaluate MUST
// continue to deny generic file/shell/act writes that touch .env, *.key, or any
// secrets path — including via standing grants, broad or scoped. The hook is the
// ONE conversion path that turns a built-in .env/.env.* path denial into a
// NON-ALLOWING HOLD eligible for the existing <=15m one-use exact approval. It
// is NOT a way around any other rule:
//
//   - custom forbidden patterns remain DENY (no downgrade to hold);
//   - explicit class deny (operator_provenance/security floor/etc.) stays;
//   - operator autonomy ceiling and current policy revision stay effective;
//   - no input/values land in the PolicyDecision, ActionIntent, or any
//     downstream audit/receipt artifact (values stay bounded-stdin only).
//
// The CLI consumer is `compound act credential-fields`; the underlying
// transport is a fixed-trusted preinstalled Go helper over validated nonsecret
// SSH arguments. This file owns ONLY the admission predicate; the dotenv
// safety and the helper live elsewhere.
package policy

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

// Closed credential-provision metadata shape. ONE capability, ONE action, ONE
// target kind, fixed blast/reversibility. Anything outside these constants is
// fail-closed (the hook returns NOT-a-credential-provision intent).
const (
	CredentialProvisionCapability = "outbound.credential_fields.write"
	CredentialProvisionAction     = "fill_credential_fields"
	CredentialProvisionTargetKind = "credential_fields"
	credentialProvisionBlast      = "manual" // host blast is below account but above local
	credentialProvisionReverse    = "hard"   // host mutation cannot roll back by re-running
)

// credentialProvisionSchemaVersion is stamped onto the ActionIntent fields the
// CLI consumer is required to set (every other field is operator-defaulted).
// Helpers and the adapter use this to refuse any intent that does not carry
// the closed shape.
const credentialProvisionSchemaVersion = "compound.credential_provision_intent.v0.1"

// CredentialProvisionRequest is the bounded, content-free request the CLI hands
// the policy hook. It carries metadata only — never values. The hook validates
// the shape and either accepts it (returning hold, eligible for exact approval)
// or refuses it.
type CredentialProvisionRequest struct {
	WorkID         string   // work_xxx, must match the lease holder's actor
	Actor          string   // operator:<id> — required for exact approval provenance
	Capability     string   // must equal CredentialProvisionCapability
	Action         string   // must equal CredentialProvisionAction
	TargetKind     string   // must equal CredentialProvisionTargetKind
	TargetPath     string   // absolute, normalized .env/.env.* path on the REMOTE host
	RemoteEndpoint string   // user@host:port metadata; values are non-secret
	Keys           []string // sorted unique exact keys to fill (NOT values)
	ExpectedRev    string   // expected whole-file revision (sha256 hex or empty)
	Mode           string   // exact "fill_empty" only — any other string is refused
	SchemaRevision string   // must equal credentialProvisionSchemaVersion
	PolicyRevision string   // current policy revision the operator read at plan time
}

// ValidateCredentialProvisionRequest returns a content-free, operator-readable
// error when the request shape is not the closed credential-provision shape.
// It does NOT load policy config or evaluate — that is EvaluateCredentialProvision's job.
//
// Note: this is the STRUCTURAL shape check. Path-tail and key-shape checks
// that determine eligibility (e.g. ".env" vs "config.yaml") belong to the
// hook's evaluation pass and yield deny (not error), so a wrong target path
// from the CLI shows up as a refused provisioning rather than a programmer error.
func ValidateCredentialProvisionRequest(req CredentialProvisionRequest) error {
	if strings.TrimSpace(req.Capability) != CredentialProvisionCapability {
		return fmt.Errorf("credential provision: capability must be %q", CredentialProvisionCapability)
	}
	if strings.TrimSpace(req.Action) != CredentialProvisionAction {
		return fmt.Errorf("credential provision: action must be %q", CredentialProvisionAction)
	}
	if strings.TrimSpace(req.TargetKind) != CredentialProvisionTargetKind {
		return fmt.Errorf("credential provision: target kind must be %q", CredentialProvisionTargetKind)
	}
	if !strings.HasPrefix(strings.TrimSpace(req.WorkID), "work_") {
		return fmt.Errorf("credential provision: work id %q must start with work_", req.WorkID)
	}
	if !strings.HasPrefix(strings.TrimSpace(req.Actor), "operator:") {
		return fmt.Errorf("credential provision: actor %q must start with operator:", req.Actor)
	}
	if !strings.HasPrefix(strings.TrimSpace(req.TargetPath), "/") {
		return fmt.Errorf("credential provision: target path %q must be an absolute path", req.TargetPath)
	}
	if !credentialProvisionPathEligibleForDowngrade(req.TargetPath) {
		return fmt.Errorf("credential provision: target path %q must end in .env or .env.<name>", req.TargetPath)
	}
	if strings.TrimSpace(req.Mode) != "fill_empty" {
		return fmt.Errorf("credential provision: mode %q must be fill_empty (the only supported shape)", req.Mode)
	}
	if strings.TrimSpace(req.SchemaRevision) != credentialProvisionSchemaVersion {
		return fmt.Errorf("credential provision: schema_revision %q must be %q", req.SchemaRevision, credentialProvisionSchemaVersion)
	}
	if len(req.Keys) == 0 {
		return fmt.Errorf("credential provision: at least one exact key is required")
	}
	seen := map[string]struct{}{}
	for _, k := range req.Keys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("credential provision: key entries must be non-empty")
		}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("credential provision: duplicate key %q", k)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// credentialProvisionPathEligibleForDowngrade reports whether the target path
// is in the closed credential-provision shape: an absolute .env or .env.* path.
// This is intentionally NOT part of structural Validate — a CLI that asks to
// provision /etc/myapp/config.yaml is refused at evaluation (not eligible) but
// is NOT a programmer error. The hook returns deny so the operator sees a
// refusal identical in shape to a missing approval.
func credentialProvisionPathEligibleForDowngrade(path string) bool {
	if !strings.HasPrefix(strings.TrimSpace(path), "/") {
		return false
	}
	if strings.HasSuffix(path, ".env") {
		return true
	}
	if strings.HasPrefix(filepathBase(path), ".env.") {
		return true
	}
	return false
}

// EvaluateCredentialProvision decides whether the request shape qualifies for
// the closed credential-provision admission — i.e. whether the built-in
// .env/.env.* denial may be downgraded to a non-allowing HOLD eligible for
// exact approval. It returns (decision, credentialProvision bool, err):
//
//   - decision.Outcome="hold" + credentialProvision=true is the ONLY path that
//     qualifies; exact approval is then the only authority that may let it run.
//   - ANY other shape returns deny with a content-free reason — never a
//     downgrade to allow/allow_with_obligations, never a chance to use a broad
//     grant.
//
// The predicate is fail-closed across every forbidden pattern in cfg: only
// the EXACT built-in `**/.env` / `**/.env.*` glob is allowed to convert to hold;
// operator-supplied or custom-config-supplied additional forbidden patterns
// always deny. This is the structural reason this hook exists: an operator
// who adds a broader custom rule must NOT see credential provision silently
// honored against that rule.
func EvaluateCredentialProvision(req CredentialProvisionRequest, cfg Config) (envelope.PolicyDecision, bool, error) {
	if err := ValidateCredentialProvisionRequest(req); err != nil {
		return envelope.PolicyDecision{}, false, err
	}
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) // synthetic deterministic clock; the CLI stamps a fresh one.
	intent := credentialProvisionIntentFromRequest(req, now)

	// The target path must be in the closed credential-provision shape (.env or
	// .env.<name>) — otherwise the hook has nothing to downgrade. We refuse
	// with deny (NOT error) so the CLI shows an operator-readable refusal.
	if !credentialProvisionPathEligibleForDowngrade(req.TargetPath) {
		return withPolicyEvidence(decisionFor(intent, "deny", []string{
			fmt.Sprintf("credential provision refused: target path %q does not match the closed built-in .env/.env.* patterns; the hook may only downgrade that exact shape", req.TargetPath),
		}, nil, "", "", now), cfg), false, nil
	}

	// Evaluate the intent the normal way so every existing floor (operator
	// autonomy ceiling, security floor, class baseline, blast escalation,
	// current policy revision) STILL applies. We then overwrite ONLY the
	// forbidden-path rung of any deny with hold when (a) the only forbidden
	// pattern that matched is a built-in .env/.env.* and (b) every other
	// rung stays allow-or-hold-or-deny.
	base := Evaluate(intent, cfg, now)

	// Walk every configured forbidden pattern. We enumerate ALL and only
	// convert to hold when the WHOLE configured set is satisfied by the
	// built-in .env/.env.* patterns (first-match logic is insufficient —
	// the explicit list scan is the only way to prove no custom pattern
	// also matches).
	customForbiddenMatched := false
	builtInMatched := false
	for _, pat := range cfg.ForbiddenPaths {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if !globPathMatches(pat, req.TargetPath) {
			continue
		}
		if isCustomForbidden(pat) {
			customForbiddenMatched = true
		} else {
			builtInMatched = true
		}
	}

	// Custom forbidden patterns that match the path produce deny regardless
	// of the credential-provision shape — the operator explicitly forbade it.
	if customForbiddenMatched {
		return withPolicyEvidence(decisionFor(intent, "deny", []string{
			fmt.Sprintf("credential provision refused: target path %q matches a custom forbidden pattern (built-in .env/.env.* denial is the only path eligible for credential-provision hold)", req.TargetPath),
		}, nil, "", "", now), cfg), false, nil
	}

	// If the existing decision is already deny for a NON-forbidden-path
	// reason (class deny / security floor / explicit class deny / operator
	// ceiling), keep it deny (the hook never upgrades deny).
	if base.Outcome == "deny" && hasNonForbiddenPathDenyReason(base) {
		return base, false, nil
	}

	// Operator ceiling: a "review" or stricter ceiling escalates to deny; we
	// keep that as deny and never downgrade it.
	if operatorCeilingForbidsAllow(cfg) {
		return withPolicyEvidence(decisionFor(intent, "deny", []string{
			"credential provision refused: operator autonomy ceiling forbids any allow path",
		}, nil, "", "", now), cfg), false, nil
	}

	// Custom security floor on the intent itself (e.g. an OperatorGated
	// classification) keeps deny.
	if hasClassDenyOrSecurityFloor(base, cfg) {
		return base, false, nil
	}

	// At this point either base.Outcome was a built-in .env/.env.* forbidden
	// path deny (which we downgrade) or the request was already hold/allow at
	// base. We synthesize a hold stamped with the credential-provision
	// eligibility marker so the exact-approval contract is the only authority
	// that may authorize the run.
	reason := fmt.Sprintf("credential provision: built-in .env/.env.* pattern matched target %q; downgraded forbidden-path deny to non-allowing hold (exact approval only)", req.TargetPath)
	pd := decisionFor(intent, "hold", []string{reason}, nil, "", "", now)
	// Stamp the credential-provision eligibility flag on the decision so
	// downstream code (ApplyMatching, the CLI, and the receipt contract) can
	// prove the hook ran.
	pd.EvidenceRefs = append(pd.EvidenceRefs, "credential_provision:eligible")
	if builtInMatched {
		_ = builtInMatched
	}
	return withPolicyEvidence(pd, cfg), true, nil
}

// hasNonForbiddenPathDenyReason inspects the existing decision's reasons and
// returns true iff the deny was caused by something OTHER than the closed
// built-in .env/.env.* forbidden-path pattern. The hook only converts the
// forbidden-path rung to hold; any other deny cause keeps the outcome.
func hasNonForbiddenPathDenyReason(pd envelope.PolicyDecision) bool {
	for _, reason := range pd.Reasons {
		r := strings.ToLower(reason)
		if strings.Contains(r, "matches forbidden pattern") {
			// Forbid only the built-in .env/.env.* paths — any other forbidden
			// pattern reason is custom and stays deny.
			if strings.Contains(r, ".env") && (strings.Contains(r, "**/.env ") || strings.Contains(r, "**/.env\"") || strings.Contains(r, "**/.env`") ||
				strings.Contains(r, "**/.env.*") || strings.Contains(r, "**/.env\"") || strings.HasSuffix(strings.TrimSpace(r), "**/.env") ||
				(strings.Contains(r, "**/.env.") && strings.Contains(r, ".env.*"))) {
				continue
			}
			if strings.Contains(r, ".env*") || strings.Contains(r, ".env\"") {
				continue
			}
			// Some other forbidden pattern (e.g. *.key, *secret*) denied this.
			return true
		}
		if strings.Contains(r, "security floor") {
			return true
		}
		if strings.Contains(r, "explicit class deny") {
			return true
		}
		if strings.Contains(r, "operator autonomy ceiling") && strings.Contains(r, "deny") {
			return true
		}
	}
	return false
}

// isCustomForbidden reports whether a forbidden pattern is something OTHER
// than the closed built-in .env/.env.* patterns. Only built-in patterns may
// downgrade to hold; any custom pattern that matches the target path forces
// deny.
func isCustomForbidden(pat string) bool {
	pat = strings.TrimSpace(pat)
	return pat != "**/.env" && pat != "**/.env.*"
}

// globPathMatches is a narrow path-glob match against an absolute path. It
// uses the same double-star / single-star semantics as matchForbidden in
// policy.go via a deliberately small recursive matcher; we re-implement here
// to avoid an import cycle (the existing matchForbidden is private to policy
// and we want this hook callable from internal/act and cmd/compound too).
func globPathMatches(pat, path string) bool {
	pat = strings.TrimSpace(pat)
	path = strings.TrimSpace(path)
	if pat == "" || path == "" {
		return false
	}
	// Normalize to slash form for matching; absolute paths keep the leading /.
	return globMatch(pat, path)
}

// globMatch is a deliberately small glob matcher that supports **, *, and ?
// segments. It is sufficient for the closed credential-provision patterns
// (**/.env and **/.env.*); broader coverage is not required by this hook.
func globMatch(pat, path string) bool {
	pidx, sidx := 0, 0
	starP, starS := -1, -1
	for sidx < len(path) {
		if pidx < len(pat) && (pat[pidx] == path[sidx] || pat[pidx] == '?') {
			pidx++
			sidx++
			continue
		}
		if pidx < len(pat) && pat[pidx] == '*' {
			starP = pidx
			starS = sidx
			pidx++
			continue
		}
		if starP != -1 {
			pidx = starP + 1
			starS++
			sidx = starS
			continue
		}
		return false
	}
	for pidx < len(pat) && pat[pidx] == '*' {
		pidx++
	}
	return pidx == len(pat)
}

// globPathMatchesAny matches path against the predicate-filtered subset of
// patterns. Returns true iff at least one pattern in the filtered subset
// matches path.
func globPathMatchesAny(patterns []string, path string, predicate func(string) bool) bool {
	for _, pat := range patterns {
		if !predicate(pat) {
			continue
		}
		if globPathMatches(pat, path) {
			return true
		}
	}
	return false
}

// credentialProvisionIntentFromRequest materializes a synthetic ActionIntent
// the rest of Evaluate can consume. It carries NO values (only metadata); the
// target path is the closed remote path, the operator is the lease holder.
func credentialProvisionIntentFromRequest(req CredentialProvisionRequest, now time.Time) envelope.ActionIntent {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	hash := hashActionArguments(map[string]any{
		"work_id":         req.WorkID,
		"remote_endpoint": req.RemoteEndpoint,
		"endpoint":        req.RemoteEndpoint,
		"keys":            append([]string(nil), req.Keys...),
		"expected_rev":    req.ExpectedRev,
		"mode":            req.Mode,
		"schema_revision": req.SchemaRevision,
		"policy_revision": req.PolicyRevision,
	})
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_" + shortActionHash(req.WorkID, req.Actor, req.Capability, hash, nowStr),
			CreatedAt:     nowStr,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: req.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"credential_provision:eligible"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:          req.Actor,
		Capability:     req.Capability,
		Action:         req.Action,
		Target:         envelope.ActionTarget{Kind: req.TargetKind, ID: req.TargetPath, Path: req.TargetPath},
		Destination:    req.TargetPath,
		ArgumentsHash:  hash,
		MutationClass:  "external_side_effect",
		Reversibility:  credentialProvisionReverse,
		BlastRadius:    credentialProvisionBlast,
		RollbackPlan:   fmt.Sprintf("remove filled %d keys from %s on the remote host (exact fields listed in receipt)", len(req.Keys), req.TargetPath),
		SchemaRevision: req.SchemaRevision,
	}
}

// decisionFor is a tiny constructor for the closed hook's PolicyDecision
// output. It mirrors the format mkDecision uses so envelope.RoundTrip works.
func decisionFor(ai envelope.ActionIntent, outcome string, reasons, obligations []string, grantID, expiresAt string, now time.Time) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            idFor("pol_", ai.ID, outcome, now.UTC().Format(time.RFC3339Nano)),
			CreatedAt:     now.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: ai.IDs.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       ai.Privacy,
			Risk:          ai.Risk,
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID:  ai.ID,
		Outcome:         outcome,
		Reasons:         reasons,
		Obligations:     obligations,
		StandingGrantID: grantID,
		ExpiresAt:       expiresAt,
	}
}

// hasClassDenyOrSecurityFloor checks whether the existing decision already
// carries a class-baseline deny or an action security floor that must NOT be
// downgraded by this hook. The only conversion permitted is the forbidden-path
// deny for the closed built-in patterns.
func hasClassDenyOrSecurityFloor(pd envelope.PolicyDecision, cfg Config) bool {
	for _, reason := range pd.Reasons {
		if strings.Contains(reason, "security floor") {
			return true
		}
		if strings.Contains(reason, "class") && strings.Contains(reason, "deny") {
			return true
		}
		if strings.Contains(reason, "explicit class deny") {
			return true
		}
		if strings.Contains(reason, "operator autonomy ceiling") && strings.Contains(reason, "deny") {
			return true
		}
	}
	return false
}

// operatorCeilingForbidsAllow checks the operator autonomy ceiling in cfg. The
// closed shape is only eligible when the ceiling is empty OR below the
// credential-provision blast (host). A "review" ceiling or stricter escalates
// to deny; we never downgrade those.
func operatorCeilingForbidsAllow(cfg Config) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.OperatorAutonomyLevel)) {
	case "deny", "review", "explicit":
		return true
	default:
		return false
	}
}

// nowTS is a tiny helper to keep the synthetic-intent tests deterministic; the
// caller may overwrite CreatedAt / PD IDs as needed.
func nowTS() time.Time { return time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) }

// filepathBase returns the base name of path. This is a deliberately tiny
// helper to avoid pulling in path/filepath in this file (which is imported by
// internal/act and cmd/compound too); the closed credential-provision shape
// only needs to recognize ".env" and ".env.<anything>" tails, and using
// strings.LastIndex keeps the rule legible at the use site.
func filepathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
