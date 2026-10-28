package work

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/policy"
)

const PrivateReleaseTagSchema = "compound.private_release_tag.v1"
const PrivateReleaseTagCapability = "source.publish_private_release_tag"
const PrivateReleaseTagAction = "publish_immutable_private_release_tag"
const privateReleaseTagSummary = "immutable lightweight release tag confirmed at the certified commit in the same authenticated private repository"

var privateReleaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// The original completed publication is evidence, never authority for this
// separate metadata action. No source bytes or additional refs are published.
type PrivateReleaseTagRequest struct {
	SchemaVersion        string                      `json:"schema_version"`
	CreatedAt            string                      `json:"created_at"`
	Publication          PrivateSourcePublishRequest `json:"publication"`
	PublicationReceiptID string                      `json:"publication_receipt_id"`
	Destination          PrivateSourceDestination    `json:"destination"`
	ReleaseTag           string                      `json:"release_tag"`
	ExpectedTagSHA       string                      `json:"expected_tag_sha"`
	Action               envelope.ActionIntent       `json:"action_intent"`
}

func privateReleaseTagAction(w envelope.WorkEnvelope, request PrivateReleaseTagRequest) (envelope.ActionIntent, error) {
	if len(request.ReleaseTag) > 64 || !privateReleaseVersion.MatchString(request.ReleaseTag) {
		return envelope.ActionIntent{}, fmt.Errorf("release tag must be one numeric vMAJOR.MINOR.PATCH version")
	}
	request.Action = envelope.ActionIntent{}
	body, err := json.Marshal(request)
	if err != nil {
		return envelope.ActionIntent{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, request.CreatedAt)
	if err != nil {
		return envelope.ActionIntent{}, fmt.Errorf("invalid release tag creation time")
	}
	digest := finding.CID(string(body))
	p := request.Publication
	ai := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, idFor("act_", w.ID, digest), at, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:  p.Actor, Capability: PrivateReleaseTagCapability, Action: PrivateReleaseTagAction,
		Target:      envelope.ActionTarget{Kind: "repository", ID: p.Source.RepositoryFingerprint, Path: p.Repo},
		Destination: request.Destination.CanonicalURL, ArgumentsHash: digest,
		MutationClass: "public", Reversibility: "irreversible", BlastRadius: "external",
		Preview:      "create only immutable refs/tags/" + request.ReleaseTag + " at already-published certified commit " + p.Source.HeadSHA + " in private repository " + request.Destination.FullName,
		RollbackPlan: "never rewrite or delete the release tag; retain the publication certificate, exact metadata action, observations and receipt",
	}
	return ai, ai.Validate()
}

func (s *Store) completedPrivateSourcePublication(request PrivateSourcePublishRequest) (ReceiptedActionResult, error) {
	w, err := s.Get(request.WorkID)
	if err != nil {
		return ReceiptedActionResult{}, err
	}
	expected, err := privatePublishAction(w, request)
	if request.SchemaVersion != PrivateSourcePublishSchema || err != nil || !reflect.DeepEqual(expected, request.Action) {
		return ReceiptedActionResult{}, fmt.Errorf("release tag requires the exact original source publication request")
	}
	// Native replay verifies the complete Receipt/Transition/event chain. A
	// read-only store prevents even unrelated recovery from mutating a plan.
	reader, err := OpenReadOnly(s.home)
	if err != nil {
		return ReceiptedActionResult{}, err
	}
	completion, found, _, err := reader.preflightReceiptedActionCompletion(request.WorkID, request.Action.ID)
	if err != nil || !found {
		return ReceiptedActionResult{}, fmt.Errorf("release tag requires a completed native source-publication receipt")
	}
	return reader.replayPrivateSourcePublication(request, completion)
}

func (s *Store) PrivateReleaseTagPlan(ctx context.Context, publication PrivateSourcePublishRequest, tag string, now time.Time) (PrivateReleaseTagRequest, error) {
	var request PrivateReleaseTagRequest
	completed, err := s.completedPrivateSourcePublication(publication)
	if err != nil {
		return request, err
	}
	w := completed.Work
	if w.GitDelivery == nil {
		return request, fmt.Errorf("release tag needs bound source delivery")
	}
	request = PrivateReleaseTagRequest{SchemaVersion: PrivateReleaseTagSchema, CreatedAt: now.UTC().Format(time.RFC3339Nano), Publication: publication, PublicationReceiptID: completed.Receipt.ID, ReleaseTag: tag}
	request.Destination, err = privateSourceLiveDestination(ctx, publication.Repo, *w.GitDelivery, now)
	if err != nil {
		return request, err
	}
	// Validate the version before allowing it into a transport argument.
	if _, err := privateReleaseTagAction(w, request); err != nil {
		return request, err
	}
	request.ExpectedTagSHA, err = observePrivateReleaseTag(ctx, request)
	if err != nil {
		return request, err
	}
	request.Action, err = privateReleaseTagAction(w, request)
	if err != nil {
		return request, err
	}
	return request, s.validatePrivateReleaseTag(ctx, request, now)
}

func (s *Store) validatePrivateReleaseTag(ctx context.Context, request PrivateReleaseTagRequest, now time.Time) error {
	p := request.Publication
	w, err := s.Get(p.WorkID)
	if err != nil {
		return err
	}
	expected, err := privateReleaseTagAction(w, request)
	if request.SchemaVersion != PrivateReleaseTagSchema || err != nil || !reflect.DeepEqual(expected, request.Action) || w.GitDelivery == nil || (request.ExpectedTagSHA != "" && request.ExpectedTagSHA != p.Source.HeadSHA) {
		return fmt.Errorf("invalid or changed immutable release tag request")
	}
	completed, err := s.completedPrivateSourcePublication(p)
	if err != nil || completed.Receipt.ID != request.PublicationReceiptID {
		return fmt.Errorf("release tag source-publication receipt is unavailable or changed")
	}
	source, err := gitrepository.CaptureCleanSnapshot(p.Repo, w.GitDelivery.ExpectedBaseSHA)
	if err != nil || !reflect.DeepEqual(source, p.Source) {
		return fmt.Errorf("release tag certified source is dirty or changed")
	}
	authority, err := s.resolvePrivateSourceReview(p.WorkID, p.Actor, p.ReviewActionID, p.Repo, source, now, false)
	if err != nil || !samePrivateSourceDestination(p.Destination, authority.Review.Destination) {
		return fmt.Errorf("release tag historical source review is unavailable or changed")
	}
	if err := s.validatePrivateSourceCertificate(w, p, source, authority); err != nil {
		return err
	}
	if err := validatePrivateSourceDestination(request.Destination, now); err != nil {
		return err
	}
	observed, err := privateSourceLiveDestination(ctx, p.Repo, *w.GitDelivery, now)
	if err != nil || !samePrivateSourceDestination(p.Destination, observed) || !samePrivateSourceDestination(request.Destination, observed) {
		return fmt.Errorf("release tag destination is not the same authenticated PRIVATE repository")
	}
	local, err := privateSourcePublishCommand(ctx, w.GitDelivery.TargetWorktreeRoot, "git", "rev-parse", "--verify", w.GitDelivery.TargetLocalRef+"^{commit}")
	if err != nil || strings.TrimSpace(string(local)) != source.HeadSHA {
		return fmt.Errorf("release tag source is not the exact landed local target")
	}
	tip, err := observePrivateSourceTip(ctx, p.Repo, observed, privateSourcePublishCommand)
	if err != nil || tip != source.HeadSHA {
		return fmt.Errorf("release tag needs the exact already-published certified main commit")
	}
	if err := privateSourceTransport(ctx, p, privateSourcePublishCommand); err != nil {
		return err
	}
	tagSHA, err := observePrivateReleaseTag(ctx, request)
	if err != nil || tagSHA != request.ExpectedTagSHA {
		return fmt.Errorf("release tag changed or could not be observed")
	}
	return nil
}

func privateReleaseGitArguments(verb string) []string {
	return []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", verb}
}

func observePrivateReleaseTag(ctx context.Context, request PrivateReleaseTagRequest) (string, error) {
	ref := "refs/tags/" + request.ReleaseTag
	args := append(privateReleaseGitArguments("ls-remote"), "--refs", "--", request.Destination.CanonicalURL, ref)
	body, err := privateSourcePublishCommand(ctx, request.Publication.Repo, "git", args...)
	if err != nil {
		return "", fmt.Errorf("authenticated release tag observation failed")
	}
	if len(body) == 0 {
		return "", nil
	}
	fields := strings.Fields(string(body))
	if len(fields) != 2 || fields[1] != ref || !privateSourceCommitSHA.MatchString(fields[0]) {
		return "", fmt.Errorf("release tag observation is not one exact ref")
	}
	return fields[0], nil
}

func privateReleaseTagPushArguments(request PrivateReleaseTagRequest) []string {
	return append(privateReleaseGitArguments("push"), "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", request.Destination.CanonicalURL, request.Publication.Source.HeadSHA+":refs/tags/"+request.ReleaseTag)
}

func (s *Store) authorizePrivateReleaseTag(request PrivateReleaseTagRequest, now time.Time) (envelope.PolicyDecision, error) {
	var decision envelope.PolicyDecision
	err := home.WithAutonomyAdmissionLock(s.home, func() error {
		cfg, _, err := policy.LoadConfig(s.home)
		if err != nil {
			return err
		}
		decision = policy.Evaluate(request.Action, cfg, now)
		if decision.Outcome == "deny" {
			return fmt.Errorf("private release tag policy denies action")
		}
		decision.Outcome, decision.StandingGrantID, decision.ExpiresAt = "hold", "", ""
		decision.Obligations = nil
		decision.Reasons = append(decision.Reasons, "native immutable private release tag requires its own exact one-use operator approval")
		if err := s.writeEventJSON(request.Action.ID, request.Action); err != nil {
			return err
		}
		decision, _, err = policy.NewExactActionApprovalStore(s.home).ApplyMatchingLocked(request.Action, cfg, decision, now)
		if err != nil {
			return err
		}
		if !policy.Allowed(decision) {
			return fmt.Errorf("private release tag action %s policy %s", request.Action.ID, decision.Outcome)
		}
		return s.writeEventJSON(decision.ID, decision)
	})
	return decision, err
}

func (s *Store) recheckPrivateReleaseTag(request PrivateReleaseTagRequest, decision envelope.PolicyDecision, now time.Time) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	expiry, err := time.Parse(time.RFC3339Nano, decision.ExpiresAt)
	if err != nil || !now.Before(expiry) || !policy.Allowed(decision) || decision.ActionIntentID != request.Action.ID || decision.IDs.WorkID != request.Publication.WorkID || decision.StandingGrantID != "" {
		return fmt.Errorf("private release tag exact decision expired or changed")
	}
	var recorded envelope.PolicyDecision
	if err := s.readEventJSON(decision.ID, &recorded); err != nil || !reflect.DeepEqual(recorded, decision) {
		return fmt.Errorf("private release tag native decision unavailable or changed")
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return err
	}
	for _, class := range []string{"build-implement", PrivateReleaseTagCapability} {
		halted, _, err := policy.AutonomyHalted(s.home, class)
		if err != nil || halted {
			return fmt.Errorf("private release tag publication halted")
		}
	}
	if policy.Evaluate(request.Action, cfg, now).Outcome == "deny" || !s.hasConsumedPublicReviewApproval(request.Action, decision, cfg, now) {
		return fmt.Errorf("exact private release tag authority unavailable")
	}
	return nil
}

func privateReleaseTagBefore(request PrivateReleaseTagRequest) []string {
	return []string{"git:" + request.Destination.FullName + "#refs/tags/" + request.ReleaseTag + "@" + request.ExpectedTagSHA}
}

func privateReleaseTagAfter(request PrivateReleaseTagRequest) []string {
	return []string{"git:" + request.Destination.FullName + "#refs/tags/" + request.ReleaseTag + "@" + request.Publication.Source.HeadSHA}
}

func (s *Store) ApplyPrivateReleaseTag(ctx context.Context, request PrivateReleaseTagRequest, now time.Time) (PrivateSourcePublishResult, error) {
	var out PrivateSourcePublishResult
	p := request.Publication
	w, err := s.Get(p.WorkID)
	if err != nil {
		return out, err
	}
	expected, err := privateReleaseTagAction(w, request)
	if err != nil || !reflect.DeepEqual(expected, request.Action) {
		return out, fmt.Errorf("release tag request action mismatch")
	}
	pending, authorizedAt, exists, err := s.ReadReceiptedActionReservationInput(p.WorkID, request.Action.ID)
	if err != nil {
		return out, err
	}
	if exists {
		if !reflect.DeepEqual(pending.Action, request.Action) {
			return out, fmt.Errorf("release tag reservation action mismatch")
		}
		if !pending.OutcomePending {
			result, err := s.RecordReservedReceiptedAction(p.WorkID, pending, authorizedAt)
			out.Status, out.Action = "receipted", &result
			return out, err
		}
		return s.finishPrivateReleaseTag(ctx, request, pending, authorizedAt, false, false)
	}
	completion, completed, _, err := s.preflightReceiptedActionCompletion(p.WorkID, request.Action.ID)
	if err != nil {
		return out, err
	}
	if completed {
		var receipt envelope.Receipt
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(completion.ReceiptID, &receipt); err != nil {
			return out, err
		}
		if err := s.readEventJSON(completion.PolicyDecisionID, &decision); err != nil {
			return out, err
		}
		final := ReceiptedActionInput{Action: request.Action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", Outcome: "completed", Changed: request.ExpectedTagSHA == "", Summary: privateReleaseTagSummary, MoveType: "outward_delta", ProofRefs: receipt.ProofRefs, BeforeRefs: privateReleaseTagBefore(request), AfterRefs: privateReleaseTagAfter(request)}
		result, err := s.replayCompletedReceiptedAction(p.WorkID, final, completion)
		out.Status, out.Action = "receipted", &result
		return out, err
	}
	if w.State != "running" {
		return out, fmt.Errorf("private release tag requires running work before approval consumption")
	}
	if err := s.validatePrivateReleaseTag(ctx, request, now); err != nil {
		return out, err
	}
	decision, err := s.authorizePrivateReleaseTag(request, now)
	if err != nil {
		return out, err
	}
	pending = ReceiptedActionInput{OutcomePending: true, Action: request.Action, PreEvaluatedDecision: &decision, ExpectedWorkState: w.State, Outcome: "blocked", Summary: "immutable private release tag pending observation", MoveType: "outward_delta", ProofRefs: []string{p.ProofRef, "receipt:" + request.PublicationReceiptID}, BeforeRefs: privateReleaseTagBefore(request)}
	err = s.ReserveReceiptedAction(p.WorkID, pending, now, func() error {
		if err := s.recheckPrivateReleaseTag(request, decision, time.Now()); err != nil {
			return err
		}
		return s.validatePrivateReleaseTag(ctx, request, time.Now())
	})
	if err != nil {
		return out, err
	}
	if err := s.recheckPrivateReleaseTag(request, decision, time.Now()); err != nil {
		return out, err
	}
	if err := s.validatePrivateReleaseTag(ctx, request, time.Now()); err != nil {
		return out, err
	}
	current, currentAt, present, err := s.ReadReceiptedActionReservationInput(p.WorkID, request.Action.ID)
	if err != nil || !present || !currentAt.Equal(now) || !reflect.DeepEqual(current, pending) {
		return out, fmt.Errorf("release tag lost exact native action reservation before launch")
	}
	if err := s.recheckPrivateReleaseTag(request, decision, time.Now()); err != nil {
		return out, err
	}
	if request.ExpectedTagSHA != "" {
		return s.finishPrivateReleaseTag(ctx, request, pending, now, false, true)
	}
	_, pushErr := privateSourcePublishCommand(ctx, p.Repo, "git", privateReleaseTagPushArguments(request)...)
	return s.finishPrivateReleaseTag(ctx, request, pending, now, true, pushErr == nil)
}

func (s *Store) finishPrivateReleaseTag(ctx context.Context, request PrivateReleaseTagRequest, pending ReceiptedActionInput, authorizedAt time.Time, attempted, transportOK bool) (PrivateSourcePublishResult, error) {
	out := PrivateSourcePublishResult{Status: "pending"}
	p := request.Publication
	w, err := s.Get(p.WorkID)
	if err != nil || w.GitDelivery == nil {
		return out, fmt.Errorf("release tag reconciliation work unavailable")
	}
	observed, destinationErr := privateSourceLiveDestination(ctx, p.Repo, *w.GitDelivery, time.Now())
	mainSHA, mainErr := observePrivateSourceTip(ctx, p.Repo, request.Destination, privateSourcePublishCommand)
	tagSHA, tagErr := observePrivateReleaseTag(ctx, request)
	confirmed := destinationErr == nil && samePrivateSourceDestination(request.Destination, observed) && mainErr == nil && mainSHA == p.Source.HeadSHA && tagErr == nil && tagSHA == p.Source.HeadSHA
	observation := struct {
		SchemaVersion string                   `json:"schema_version"`
		ActionID      string                   `json:"action_id"`
		ObservedAt    string                   `json:"observed_at"`
		Destination   PrivateSourceDestination `json:"destination"`
		MainSHA       string                   `json:"main_sha"`
		TagSHA        string                   `json:"tag_sha"`
		PushAttempted bool                     `json:"push_attempted"`
		TransportOK   bool                     `json:"transport_succeeded"`
		Confirmed     bool                     `json:"confirmed"`
	}{"compound.private_release_tag_observation.v1", request.Action.ID, time.Now().UTC().Format(time.RFC3339Nano), observed, mainSHA, tagSHA, attempted, transportOK, confirmed}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return out, err
	}
	out.ObservationRef, err = artifacts.PutPrivate(body)
	if err != nil {
		return out, err
	}
	out.RemoteSHA = tagSHA
	if !confirmed {
		return out, fmt.Errorf("private release tag outcome pending reconciliation; observation %s; no automatic repush", out.ObservationRef)
	}
	final := pending
	final.OutcomePending, final.Outcome, final.Changed = false, "completed", request.ExpectedTagSHA == ""
	final.Summary, final.AfterRefs = privateReleaseTagSummary, privateReleaseTagAfter(request)
	final.ProofRefs = append(append([]string(nil), pending.ProofRefs...), "evidence:"+out.ObservationRef)
	if err := s.SealReceiptedActionOutcome(p.WorkID, pending, final, authorizedAt); err != nil {
		return out, err
	}
	result, err := s.RecordReservedReceiptedAction(p.WorkID, final, authorizedAt)
	out.Status, out.Action = "published_pending_receipt", &result
	if err == nil {
		out.Status = "published"
	}
	return out, err
}
