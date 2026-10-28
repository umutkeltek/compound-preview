package work

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/policy"
)

const (
	PrivateSourceReviewSchema     = "compound.private_source_review.v1"
	PrivateSourceReviewCapability = "proof.redaction.review_private_source"
	PrivateSourceReviewAction     = "classify_private_source_redaction_findings"
	privateSourceReviewMarker     = "<compound-private-source-review-v1>"
)

type PrivateSourceReview struct {
	SchemaVersion       string                   `json:"schema_version"`
	WorkID              string                   `json:"work_id"`
	Source              gitrepository.Snapshot   `json:"source_snapshot"`
	SourceProfileDigest string                   `json:"source_profile_digest"`
	Destination         PrivateSourceDestination `json:"destination"`
	Files               []PublicRedactionFile    `json:"files"`
}

type PrivateSourceEvidence struct {
	Binding     PublicRedactionReviewBinding `json:"binding"`
	Review      []byte                       `json:"review"`
	Scans       []PublicRedactionScan        `json:"scans"`
	Destination PrivateSourceDestination     `json:"destination"`
}

type PrivateSourceAuthority struct {
	Binding     PublicRedactionReviewBinding
	Body        []byte
	Review      PrivateSourceReview
	Destination PrivateSourceDestination
}

// A classification is per exact source finding, not a scanner category waiver.
// The native release-class action binds every classification and evidence ref.
func supportedPrivateSourceClassification(id, classification string) bool {
	switch id {
	case "internal.hostname":
		return classification == "private_service_hostname" || classification == "private_nonsecret_operational_setting"
	case "internal.url_private":
		return classification == "private_service_endpoint"
	case "pii.ip_public":
		return classification == "private_operational_ip" || classification == "private_nonsecret_operational_setting"
	case "pii.email":
		return classification == "private_support_contact" || classification == "private_synthetic_fixture" || classification == "private_nonsecret_operational_setting"
	case "pii.phone.e164", "pii.cc":
		return classification == "private_provider_identifier" || classification == "private_synthetic_fixture" || classification == "private_nonsecret_operational_setting"
	case "env.kv":
		return classification == "private_nonsecret_operational_setting"
	default:
		return false
	}
}

func ParsePrivateSourceReview(body []byte, now time.Time) (PrivateSourceReview, error) {
	var review PrivateSourceReview
	if err := publicReviewStrictJSON(body, &review); err != nil {
		return review, err
	}
	if review.SchemaVersion != PrivateSourceReviewSchema || !strings.HasPrefix(review.WorkID, "work_") || !publicReviewSHA256.MatchString(review.SourceProfileDigest) || len(review.Files) == 0 || len(review.Files) > 4096 {
		return review, fmt.Errorf("invalid private source review identity/inventory")
	}
	if err := gitrepository.ValidateSnapshotShape(review.Source); err != nil || !review.Source.Clean {
		return review, fmt.Errorf("private source review needs a complete clean source snapshot")
	}
	if err := validatePrivateSourceDestination(review.Destination, now); err != nil {
		return review, err
	}
	previous, totalMedium := "", 0
	for _, file := range review.Files {
		if file.Path == "" || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || file.Path == ".." || strings.HasPrefix(file.Path, "../") || strings.ContainsAny(file.Path, "\\\x00\r\n") || file.Path <= previous || !publicReviewSHA256.MatchString(file.SHA256) {
			return review, fmt.Errorf("private source inventory must be complete, ordered and source-hashed")
		}
		previous = file.Path
		findings, err := ParsePublicRedactionReport(file.Report, file.ExitCode)
		if err != nil {
			return review, fmt.Errorf("private source report %s: %w", file.Path, err)
		}
		remaining := append([]PublicRedactionApproval(nil), file.Approvals...)
		for _, finding := range findings {
			if finding.Tier == "HIGH" {
				return review, fmt.Errorf("HIGH findings always block private source publication")
			}
			if finding.Tier != "MEDIUM" {
				continue
			}
			if finding.Severity != "MEDIUM" || finding.ToolFenceDegraded {
				return review, fmt.Errorf("degraded findings cannot be privately classified")
			}
			index := slices.IndexFunc(remaining, func(a PublicRedactionApproval) bool { return reflect.DeepEqual(a.Finding, finding) })
			if index < 0 {
				return review, fmt.Errorf("unclassified MEDIUM private source finding")
			}
			approval := remaining[index]
			if !supportedPrivateSourceClassification(finding.ID, approval.Classification) || len(approval.EvidenceRefs) == 0 || len(approval.EvidenceRefs) > 8 {
				return review, fmt.Errorf("unsupported private source classification or missing evidence")
			}
			for _, ref := range approval.EvidenceRefs {
				if strings.TrimSpace(ref) != ref || ref == "" || len(ref) > 4096 {
					return review, fmt.Errorf("invalid private source classification evidence ref")
				}
			}
			remaining = append(remaining[:index], remaining[index+1:]...)
			totalMedium++
		}
		if len(remaining) != 0 {
			return review, fmt.Errorf("duplicate or unmatched private source classification")
		}
	}
	if totalMedium == 0 {
		return review, fmt.Errorf("private source review has no supported blocking finding")
	}
	return review, nil
}

func privateSourceProfileDigest(plan CanonicalProofPlan) (string, error) {
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	return profile.Digest, err
}

var privateSourceLiveDestination = func(ctx context.Context, repo string, delivery envelope.GitDeliveryContract, now time.Time) (PrivateSourceDestination, error) {
	return observePrivateSourceDestination(ctx, repo, delivery, now, runPrivateSourceCommand)
}

func BindNativeSourceRedactionReview(plan CanonicalProofPlan, homePath, workID, actor, actionID string) (CanonicalProofPlan, error) {
	store, err := OpenReadOnly(homePath)
	if err != nil {
		return plan, err
	}
	var action envelope.ActionIntent
	if err := store.readEventJSON(actionID, &action); err != nil {
		return plan, err
	}
	if action.Capability != PrivateSourceReviewCapability {
		return BindNativePublicRedactionReview(plan, homePath, workID, actor, actionID)
	}
	authority, err := store.resolvePrivateSourceReview(workID, actor, actionID, plan.Repo, plan.Source, time.Now(), false)
	if err != nil {
		return plan, err
	}
	digest, err := privateSourceProfileDigest(plan)
	if err != nil || digest != authority.Review.SourceProfileDigest {
		return plan, fmt.Errorf("private source review does not bind this exact native source profile")
	}
	return bindPrivateSourceReview(plan, authority.Binding)
}

func bindPrivateSourceReview(plan CanonicalProofPlan, binding PublicRedactionReviewBinding) (CanonicalProofPlan, error) {
	plan = cloneCanonicalProofPlan(plan)
	found := false
	for i := range plan.Steps {
		step := &plan.Steps[i]
		if step.Name != "redact-public" {
			continue
		}
		if found || step.ProofRef != "redact-public-clean" || len(step.Command) != 4 || !reflect.DeepEqual(step.Command[:3], []string{"redact", "--repo-visibility", "public"}) {
			return plan, fmt.Errorf("private source review requires one unchanged full public scanner gate")
		}
		found = true
		step.Command = append(step.Command, privateSourceReviewMarker, binding.ActionID, binding.PolicyID, binding.FindingID, binding.CID)
		step.ProofRef = "redact-private-source-reviewed"
	}
	if !found {
		return plan, fmt.Errorf("private source review scanner gate is missing")
	}
	return plan, nil
}

func PrivateSourceBindingForStep(step ProofCheckStep) (PublicRedactionReviewBinding, bool, error) {
	marker := slices.Index(step.Command, privateSourceReviewMarker)
	if marker < 0 {
		if step.PrivateSourceEvidence != nil || step.ProofRef == "redact-private-source-reviewed" {
			return PublicRedactionReviewBinding{}, false, fmt.Errorf("private source evidence is not profile-bound")
		}
		return PublicRedactionReviewBinding{}, false, nil
	}
	if step.Name != "redact-public" || step.ProofRef != "redact-private-source-reviewed" || marker != 4 || len(step.Command) != 9 || !reflect.DeepEqual(step.Command[:3], []string{"redact", "--repo-visibility", "public"}) || step.RedactionEvidence != nil {
		return PublicRedactionReviewBinding{}, false, fmt.Errorf("invalid private source gate identity")
	}
	binding := PublicRedactionReviewBinding{step.Command[5], step.Command[6], step.Command[7], step.Command[8]}
	if !validPublicRedactionBinding(binding) {
		return binding, false, fmt.Errorf("invalid private source authority tuple")
	}
	return binding, true, nil
}

func (s *Store) resolvePrivateSourceReview(workID, actor, actionID, repo string, source gitrepository.Snapshot, now time.Time, requireCurrent bool) (PrivateSourceAuthority, error) {
	var out PrivateSourceAuthority
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	if w.State != "claimed" && w.State != "running" && w.State != "verifying" {
		return out, fmt.Errorf("privacy review needs active work")
	}
	lease, err := s.readLease(workID)
	if err != nil || lease.WorkID != workID || lease.By != actor || actor == "" {
		return out, fmt.Errorf("privacy review actor does not hold this work")
	}
	if w.GitDelivery == nil || w.GitDelivery.RepositoryFingerprint != source.RepositoryFingerprint || !slices.Contains(w.ActionIntentIDs, actionID) {
		return out, fmt.Errorf("review is not performed on this source-owned work")
	}
	var ai envelope.ActionIntent
	if err := s.readEventJSON(actionID, &ai); err != nil {
		return out, err
	}
	if err := ai.Validate(); err != nil {
		return out, err
	}
	if ai.ID != actionID || ai.IDs.WorkID != workID || ai.Actor != actor || ai.Capability != PrivateSourceReviewCapability || ai.Action != PrivateSourceReviewAction || ai.MutationClass != "public" || ai.SecurityFloorOutcome != "" || ai.Target.Kind != "repository" || ai.Target.ID != source.RepositoryFingerprint || ai.Target.Path != repo {
		return out, fmt.Errorf("review needs an exact release-class action, not local editing permission")
	}
	for _, ref := range ai.EvidenceRefs {
		if strings.HasPrefix(ref, "finding:") {
			if out.Binding.FindingID != "" {
				return out, fmt.Errorf("ambiguous review finding")
			}
			out.Binding.FindingID = strings.TrimPrefix(ref, "finding:")
		}
	}
	if !publicReviewFindingID.MatchString(out.Binding.FindingID) {
		return out, fmt.Errorf("native review finding missing")
	}
	f, err := s.readPublicRedactionFinding(out.Binding.FindingID)
	if err != nil {
		return out, err
	}
	if f.WorkID != workID || f.ProducedBy != actor || f.CID != ai.ArgumentsHash {
		return out, fmt.Errorf("review finding provenance or action digest mismatch")
	}
	out.Body = []byte(f.Body)
	out.Review, err = ParsePrivateSourceReview(out.Body, now)
	if err != nil {
		return out, err
	}
	if out.Review.Destination.PublicationRef != w.GitDelivery.PublicationRef || out.Review.Destination.TargetRef != w.GitDelivery.TargetLocalRef || w.GitDelivery.PublicationMode != "required" {
		return out, fmt.Errorf("private source destination does not match work publication contract")
	}
	if out.Review.WorkID != workID || !reflect.DeepEqual(out.Review.Source, source) {
		return out, fmt.Errorf("review work or exact source snapshot mismatch")
	}
	events, err := s.eventsFor(workID)
	if err != nil {
		return out, err
	}
	for _, event := range events {
		if !slices.Contains(event.Attached, actionID) {
			continue
		}
		id, err := oneAttachedWithPrefix(event.Attached, "pol_")
		if err != nil || out.Binding.PolicyID != "" {
			return out, fmt.Errorf("review has no unique native policy transaction")
		}
		out.Binding.PolicyID = id
	}
	var pd envelope.PolicyDecision
	if out.Binding.PolicyID == "" {
		return out, fmt.Errorf("review policy transaction missing")
	}
	if err := s.readEventJSON(out.Binding.PolicyID, &pd); err != nil {
		return out, err
	}
	if err := pd.Validate(); err != nil {
		return out, err
	}
	if pd.ID != out.Binding.PolicyID || pd.IDs.WorkID != workID || pd.ActionIntentID != actionID || !policy.Allowed(pd) {
		return out, fmt.Errorf("review decision is not allowing or exact")
	}
	if pd.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339Nano, pd.ExpiresAt)
		if err != nil || (requireCurrent && !now.Before(expiry)) {
			return out, fmt.Errorf("review decision expired")
		}
	}
	out.Binding.ActionID, out.Binding.CID = actionID, f.CID
	if !requireCurrent {
		return out, nil
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return out, err
	}
	for _, class := range []string{"build-implement", PrivateSourceReviewCapability} {
		halted, _, err := policy.AutonomyHalted(s.home, class)
		if err != nil || halted {
			return out, fmt.Errorf("review authority halted or unavailable")
		}
	}
	fresh := policy.Evaluate(ai, cfg, now)
	if fresh.Outcome == "deny" {
		return out, fmt.Errorf("current policy denies review")
	}
	if !(policy.Allowed(fresh) && fresh.StandingGrantID == pd.StandingGrantID) {
		if pd.StandingGrantID != "" || fresh.Outcome != "hold" || !s.hasConsumedPublicReviewApproval(ai, pd, cfg, now) {
			return out, fmt.Errorf("current private-source authority unavailable")
		}
	}
	observed, err := privateSourceLiveDestination(context.Background(), repo, *w.GitDelivery, now)
	if err != nil || !samePrivateSourceDestination(out.Review.Destination, observed) {
		return out, fmt.Errorf("authenticated private source destination unavailable or changed")
	}
	out.Destination = observed
	return out, nil
}

func privateSourceReviewForExecution(ctx context.Context, step ProofCheckStep, now time.Time) (PrivateSourceAuthority, error) {
	var out PrivateSourceAuthority
	binding, reviewed, err := PrivateSourceBindingForStep(step)
	if err != nil || !reviewed {
		return out, fmt.Errorf("reviewed execution binding missing or invalid")
	}
	identity, ok := ctx.Value(publicRedactionContextKey{}).(publicRedactionContext)
	plan, planned := ctx.Value(canonicalProofPlanContextKey{}).(CanonicalProofPlan)
	if !ok || !planned {
		return out, fmt.Errorf("reviewed execution needs owned work and canonical plan")
	}
	store, err := OpenReadOnly(identity.Home)
	if err != nil {
		return out, err
	}
	out, err = store.resolvePrivateSourceReview(identity.WorkID, identity.Actor, binding.ActionID, plan.Repo, plan.Source, now, true)
	if err != nil {
		return out, err
	}
	if out.Binding != binding {
		return out, fmt.Errorf("reviewed execution authority drift")
	}
	original := cloneCanonicalProofPlan(plan)
	for i := range original.Steps {
		if _, private, err := PrivateSourceBindingForStep(original.Steps[i]); err != nil {
			return out, err
		} else if private {
			original.Steps[i].Command = original.Steps[i].Command[:4]
			original.Steps[i].ProofRef = "redact-public-clean"
			original.Steps[i].PrivateSourceEvidence = nil
		}
	}
	digest, err := privateSourceProfileDigest(original)
	if err != nil || digest != out.Review.SourceProfileDigest {
		return out, fmt.Errorf("reviewed execution original profile drift")
	}
	return out, nil
}
