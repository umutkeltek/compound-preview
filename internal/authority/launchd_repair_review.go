package authority

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/work"
)

const launchdExactReviewProtocol = "exact-approval-v2"

type LaunchdReviewRequest struct {
	ApprovalProtocol     string                       `json:"approval_protocol,omitempty"`
	Purpose              string                       `json:"purpose"`
	RollbackAnchor       string                       `json:"rollback_anchor,omitempty"`
	NoEffectContinuation *LaunchdNoEffectContinuation `json:"no_effect_continuation,omitempty"`
	PlanID               string                       `json:"plan_id"`
	PlanDigest           string                       `json:"plan_digest"`
	GrantID              string                       `json:"grant_id"`
	Grant                policy.StandingGrant         `json:"grant"`
	ConfigRevision       string                       `json:"config_revision"`
	ReviewedBy           string                       `json:"reviewed_by"`
	EvidenceRef          string                       `json:"evidence_ref"`
	ExpiresAt            time.Time                    `json:"expires_at"`
	BeforeWork           envelope.WorkEnvelope        `json:"before_work"`
	Action               envelope.ActionIntent        `json:"action"`
}
type LaunchdRepairReview struct {
	ID         string                  `json:"id"`
	Request    LaunchdReviewRequest    `json:"request"`
	Decision   envelope.PolicyDecision `json:"decision"`
	WorkSHA256 string                  `json:"work_sha256"`
}
type LaunchdReviewResult struct {
	Pending        bool                 `json:"pending"`
	Review         *LaunchdRepairReview `json:"review,omitempty"`
	ActionIntentID string               `json:"action_intent_id"`
	IntentPath     string               `json:"intent_path"`
	Next           string               `json:"next,omitempty"`
}
type launchdReviewProposal struct {
	Work     envelope.WorkEnvelope   `json:"work"`
	Decision envelope.PolicyDecision `json:"decision"`
	Recorded bool                    `json:"recorded"`
}

func launchdExactGrant(p LaunchdRepairPlan, cfg policy.Config, id string, at time.Time) (policy.StandingGrant, error) {
	var selected policy.StandingGrant
	found := false
	for _, g := range cfg.StandingGrants {
		if g.ID == id {
			if found {
				return selected, fmt.Errorf("duplicate exact grant ID")
			}
			selected = g
			found = true
		}
	}
	g := selected
	expiry, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if !found || err != nil || !at.Before(expiry) || g.Capability != LaunchdRepairCapability || g.MutationClass != "external_side_effect" || g.TargetPath != p.PlistPath || g.Destination != launchdPlanDestination(p) || g.BlastRadius != "narrow" {
		return g, fmt.Errorf("current exact target/work/digest/expiry launchd grant required")
	}
	if g.NotBefore != "" {
		start, err := time.Parse(time.RFC3339, g.NotBefore)
		if err != nil || at.Before(start) {
			return g, fmt.Errorf("launchd grant not active")
		}
	}
	return g, nil
}
func launchdReviewRequestAction(p LaunchdRepairPlan, r LaunchdReviewRequest) envelope.ActionIntent {
	r.Action = envelope.ActionIntent{}
	a := launchdAction(p, p.ID+"-review-"+launchdDigest(r), "review_exact_launchd_repair", p.CreatedAt)
	a.Capability = LaunchdRepairCapability + ".review"
	if r.ApprovalProtocol == launchdExactReviewProtocol {
		a.RequiresExactApproval = true
	}
	a.ArgumentsHash = launchdDigest(r)
	a.EvidenceRefs = append(a.EvidenceRefs, r.EvidenceRef, "launchd-grant-revision:"+launchdDigest(r.Grant), "policy-revision:"+r.ConfigRevision)
	return a
}

// ReviewLaunchdRepairPlan first persists an exact review proposal. The operator
// approves and records that intent through existing policy/work doors, then this
// same call seals the authentic consumed decision. Merely naming an operator
// or obtaining a baseline allow cannot produce a review.
func ReviewLaunchdRepairPlan(home, planID, grantID, reviewedBy, evidence string, expiry, timeNow time.Time) (LaunchdReviewResult, error) {
	return reviewLaunchdRepairPlan(home, planID, grantID, reviewedBy, evidence, expiry, timeNow, "apply")
}
func ReviewLaunchdRepairRollbackPlan(home, planID, grantID, reviewedBy, evidence string, expiry, timeNow time.Time) (LaunchdReviewResult, error) {
	return reviewLaunchdRepairPlan(home, planID, grantID, reviewedBy, evidence, expiry, timeNow, "rollback")
}
func reviewLaunchdRepairPlan(home, planID, grantID, reviewedBy, evidence string, expiry, timeNow time.Time, purpose string) (LaunchdReviewResult, error) {
	var out LaunchdReviewResult
	p, err := loadLaunchdPlan(home, planID)
	if err != nil {
		return out, err
	}
	unlock, err := lockLaunchdLabel(home, p.Domain+"/"+p.Label)
	if err != nil {
		return out, err
	}
	defer unlock()
	if !policy.HasOperatorProvenance(reviewedBy) || evidence == "" || !expiry.After(timeNow) || expiry.Sub(timeNow) > time.Hour {
		return out, fmt.Errorf("operator evidence and at-most-one-hour review expiry required")
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		return out, err
	}
	grant, err := launchdExactGrant(p, cfg, grantID, timeNow)
	if err != nil {
		return out, err
	}
	grantExpiry, _ := time.Parse(time.RFC3339, grant.ExpiresAt)
	if expiry.After(grantExpiry) {
		return out, fmt.Errorf("review cannot outlive exact grant")
	}
	store, err := work.Open(home)
	if err != nil {
		return out, err
	}
	current, err := store.Get(p.WorkID)
	if err != nil {
		return out, err
	}
	if current.State != "running" {
		return out, fmt.Errorf("running activation work required")
	}
	// Keep legacy requests/actions/approvals unchanged as audit evidence.
	// A new protocol issues a fresh exact-bound action under the same plan.
	prefix := p.ID + "." + purpose + ".exact-v2"
	var continuation *LaunchdNoEffectContinuation
	var failedPrefix []launchdPhaseCommit
	if purpose == "rollback" && p.Handoff != nil {
		var failed launchdPhaseCommit
		if e := readLaunchdArtifact(home, launchdPhaseStem(p, "rollback", 0)+".committed.json", &failed); e == nil && failed.Outcome == "failed" {
			failedPrefix, err = launchdNoEffectPrefix(home, p, -1)
			if err != nil {
				return out, err
			}
			continuation = &LaunchdNoEffectContinuation{Index: len(failedPrefix), PrefixSHA256: launchdNoEffectPrefixDigest(home, p, failedPrefix)}
			if len(continuation.PrefixSHA256) != 64 {
				return out, fmt.Errorf("no-effect prefix digest unavailable")
			}
			prefix = p.ID + ".rollback.no-effect-" + launchdDigest([]any{continuation, grantID, grant, launchdDigest(cfg), reviewedBy, evidence, expiry})[:24] + ".exact-v2"
		} else if e != nil && !os.IsNotExist(e) {
			return out, e
		}
	}
	requestName := prefix + ".review-request.json"
	var req LaunchdReviewRequest
	err = readLaunchdArtifact(home, requestName, &req)
	if os.IsNotExist(err) {
		req = LaunchdReviewRequest{ApprovalProtocol: launchdExactReviewProtocol, Purpose: purpose, NoEffectContinuation: continuation, PlanID: p.ID, PlanDigest: p.Digest, GrantID: grantID, Grant: grant, ConfigRevision: launchdDigest(cfg), ReviewedBy: reviewedBy, EvidenceRef: evidence, ExpiresAt: expiry, BeforeWork: current}
		if purpose == "rollback" {
			anchor, sha, err := launchdRollbackAnchor(home, p)
			if continuation != nil {
				// A fresh exact approval binds the actual current generation,
				// including earlier expired review actions. The failed prefix is
				// separately sealed, linked, and authenticated; it is not replayed.
				anchor, sha, err = continuation.PrefixSHA256, launchdWorkDigest(current), nil
				if !launchdContinuationContainsPrefix(home, p, req, failedPrefix) {
					return out, fmt.Errorf("current work does not retain failed no-effect prefix")
				}
			}
			if err != nil {
				return out, err
			}
			if sha != launchdWorkDigest(current) {
				return out, fmt.Errorf("rollback must follow exact owned phase generation")
			}
			req.RollbackAnchor = anchor
		}
		req.Action = launchdReviewRequestAction(p, req)
		result, err := store.EvaluateAndRecordAction(p.WorkID, req.Action, timeNow)
		if err != nil {
			return out, err
		}
		if err := writeLaunchdArtifact(home, requestName, req); err != nil {
			return out, err
		}
		if err := writeLaunchdArtifact(home, prefix+".review-proposal.json", launchdReviewProposal{Work: result.Work, Decision: result.Decision, Recorded: result.Recorded}); err != nil {
			return out, err
		}
		if err := writeLaunchdArtifact(home, prefix+".review-intent.json", req.Action); err != nil {
			return out, err
		}
		current = result.Work
	} else if err != nil {
		return out, err
	}
	if req.ApprovalProtocol != launchdExactReviewProtocol || req.Purpose != purpose || req.PlanID != p.ID || req.PlanDigest != p.Digest || req.GrantID != grantID || !reflect.DeepEqual(req.Grant, grant) || req.ConfigRevision != launchdDigest(cfg) || req.ReviewedBy != reviewedBy || req.EvidenceRef != evidence || !req.ExpiresAt.Equal(expiry) || !reflect.DeepEqual(req.NoEffectContinuation, continuation) || !reflect.DeepEqual(req.Action, launchdReviewRequestAction(p, req)) {
		return out, fmt.Errorf("review request drift; create a new native plan")
	}
	out.ActionIntentID = req.Action.ID
	out.IntentPath, _ = launchdArtifactPath(home, prefix+".review-intent.json")
	events, err := eventstore.Open(home)
	if err != nil {
		return out, err
	}
	var decision envelope.PolicyDecision
	entry, err := events.Read(current.PolicyDecisionID)
	if err == nil {
		err = json.Unmarshal(entry.Bytes, &decision)
	}
	if err != nil || validateLaunchdReviewDecision(home, req.Action, decision, req.ReviewedBy) != nil {
		out.Pending = true
		out.Next = "Use policy action-plan/approve for action_intent_id, then work act --intent intent_path; repeat this exact review request"
		return out, nil
	}
	var proposal launchdReviewProposal
	if err := readLaunchdArtifact(home, prefix+".review-proposal.json", &proposal); err != nil {
		return out, err
	}
	expected := proposal.Work
	if proposal.Decision.ID != decision.ID {
		expected.ActionIntentIDs = append(append([]string{}, expected.ActionIntentIDs...), req.Action.ID)
		expected.PolicyDecisionID = decision.ID
	}
	if launchdWorkDigest(current) != launchdWorkDigest(expected) {
		return out, fmt.Errorf("unrelated work generation changed during operator review")
	}
	review := LaunchdRepairReview{Request: req, Decision: decision, WorkSHA256: launchdWorkDigest(current)}
	review.ID = "ldrr_" + launchdDigest(review)[:24]
	if err := writeLaunchdArtifact(home, review.ID+".review.json", review); err != nil {
		return out, err
	}
	out.Review = &review
	return out, nil
}
func validateLaunchdReviewEvidence(home string, p LaunchdRepairPlan, r LaunchdRepairReview) error {
	if c := r.Request.NoEffectContinuation; c != nil && (r.Request.Purpose != "rollback" || p.Handoff == nil || c.Index < 1 || len(c.PrefixSHA256) != 64 || r.Request.RollbackAnchor != c.PrefixSHA256) {
		return fmt.Errorf("invalid no-effect continuation review")
	}
	if r.Request.ApprovalProtocol != "" && r.Request.ApprovalProtocol != launchdExactReviewProtocol {
		return fmt.Errorf("unknown launchd review approval protocol")
	}
	copy := r
	copy.ID = ""
	if r.ID != "ldrr_"+launchdDigest(copy)[:24] || r.Request.PlanID != p.ID || r.Request.PlanDigest != p.Digest || !reflect.DeepEqual(r.Request.Action, launchdReviewRequestAction(p, r.Request)) {
		return fmt.Errorf("review identity, digest or expiry invalid")
	}
	if !policy.HasOperatorProvenance(r.Request.ReviewedBy) || r.Request.EvidenceRef == "" {
		return fmt.Errorf("operator review evidence missing")
	}
	if err := validateLaunchdReviewDecision(home, r.Request.Action, r.Decision, r.Request.ReviewedBy); err != nil {
		return fmt.Errorf("authentic consumed operator review required: %w", err)
	}
	return nil
}
func validateLaunchdReview(home string, p LaunchdRepairPlan, r LaunchdRepairReview, now time.Time) error {
	if err := validateLaunchdReviewEvidence(home, p, r); err != nil {
		return err
	}
	if !r.Request.ExpiresAt.After(now) {
		return fmt.Errorf("launchd review expired")
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		return err
	}
	g, err := launchdExactGrant(p, cfg, r.Request.GrantID, now)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(g, r.Request.Grant) || launchdDigest(cfg) != r.Request.ConfigRevision {
		return fmt.Errorf("reviewed policy or exact grant revision changed")
	}
	return nil
}

func validateLaunchdReviewDecision(home string, action envelope.ActionIntent, decision envelope.PolicyDecision, reviewedBy string) error {
	if err := policy.NewExactActionApprovalStore(home).ValidateConsumedDecision(action, decision); err != nil {
		return err
	}
	const prefix = "exact one-use operator approval "
	const suffix = " matches action binding"
	id := ""
	for _, reason := range decision.Reasons {
		if strings.HasPrefix(reason, prefix) && strings.HasSuffix(reason, suffix) {
			id = strings.TrimSuffix(strings.TrimPrefix(reason, prefix), suffix)
		}
	}
	events, err := eventstore.Open(home)
	if err != nil {
		return err
	}
	entry, err := events.Read(id)
	if err != nil {
		return err
	}
	var approval policy.ExactActionApproval
	if err = json.Unmarshal(entry.Bytes, &approval); err != nil {
		return err
	}
	if approval.GrantedBy != reviewedBy {
		return fmt.Errorf("reviewer does not match authentic exact approval grantor")
	}
	return nil
}
