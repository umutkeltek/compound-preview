package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

const PrivateSourcePublishCapability = "source.publish_private"
const PrivateSourcePublishAction = "publish_certified_private_source"
const PrivateSourcePublishSchema = "compound.private_source_publish.v1"
const privatePublicationSuccessSummary = "certified source confirmed at exact authenticated private GitHub destination and bound tracking ref"

// A request is a concrete, source-bound release proposal. Planning does not
// admit it. Applying requires a separate exact one-use operator approval.
type PrivateSourcePublishRequest struct {
	SchemaVersion     string                   `json:"schema_version"`
	WorkID            string                   `json:"work_id"`
	Actor             string                   `json:"actor"`
	Repo              string                   `json:"repo"`
	CreatedAt         string                   `json:"created_at"`
	Source            gitrepository.Snapshot   `json:"source_snapshot"`
	ProofRef          string                   `json:"proof_ref"`
	ProfileDigest     string                   `json:"profile_digest"`
	ReviewActionID    string                   `json:"review_action_id"`
	Destination       PrivateSourceDestination `json:"destination"`
	ExpectedRemoteSHA string                   `json:"expected_remote_sha"`
	Action            envelope.ActionIntent    `json:"action_intent"`
}

type PrivateSourcePublishResult struct {
	Status         string                 `json:"status"`
	ObservationRef string                 `json:"observation_ref,omitempty"`
	RemoteSHA      string                 `json:"remote_sha,omitempty"`
	Action         *ReceiptedActionResult `json:"receipted_action,omitempty"`
}

type privatePublicationObservation struct {
	SchemaVersion      string                   `json:"schema_version"`
	ActionID           string                   `json:"action_id"`
	ObservedAt         string                   `json:"observed_at"`
	Destination        PrivateSourceDestination `json:"destination"`
	RemoteSHA          string                   `json:"remote_sha,omitempty"`
	PushAttempted      bool                     `json:"push_attempted"`
	TransportSucceeded bool                     `json:"transport_succeeded"`
	RemoteConfirmed    bool                     `json:"remote_confirmed"`
	TrackingConfirmed  bool                     `json:"tracking_confirmed"`
}

var privateSourceCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var privateSourcePublishCommand privateSourceCommand = runPrivateSourceCommand

func privatePublishAction(w envelope.WorkEnvelope, request PrivateSourcePublishRequest) (envelope.ActionIntent, error) {
	request.Action = envelope.ActionIntent{}
	body, err := json.Marshal(request)
	if err != nil {
		return envelope.ActionIntent{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, request.CreatedAt)
	if err != nil {
		return envelope.ActionIntent{}, fmt.Errorf("invalid publication creation time")
	}
	digest := finding.CID(string(body))
	ai := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, idFor("act_", w.ID, digest), at, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:  request.Actor, Capability: PrivateSourcePublishCapability, Action: PrivateSourcePublishAction,
		Target:      envelope.ActionTarget{Kind: "repository", ID: request.Source.RepositoryFingerprint, Path: request.Repo},
		Destination: request.Destination.CanonicalURL, ArgumentsHash: digest,
		MutationClass: "public", Reversibility: "irreversible", BlastRadius: "external",
		Preview:      "publish certified source " + request.Source.HeadSHA + " to authenticated private repository " + request.Destination.FullName + " " + request.Destination.TargetRef + "; non-force push from " + request.ExpectedRemoteSHA,
		RollbackPlan: "publication cannot be recalled; retain source, proof, destination observations, and exact outcome receipt",
	}
	return ai, ai.Validate()
}

func (s *Store) PrivateSourcePublishPlan(ctx context.Context, workID, actor, repo, reviewActionID string, now time.Time) (PrivateSourcePublishRequest, error) {
	var request PrivateSourcePublishRequest
	w, err := s.Get(workID)
	if err != nil {
		return request, err
	}
	if w.GitDelivery == nil {
		return request, fmt.Errorf("publication needs bound source delivery")
	}
	source, err := gitrepository.CaptureCleanSnapshot(repo, w.GitDelivery.ExpectedBaseSHA)
	if err != nil {
		return request, err
	}
	authority, err := s.resolvePrivateSourceReview(workID, actor, reviewActionID, repo, source, now, true)
	if err != nil {
		return request, err
	}
	result, found, err := s.latestSourceBoundProofResult(w)
	if err != nil || !found || !strings.HasPrefix(result.VerificationRef, "proof-check:pfrs_") || result.SourceSnapshot == nil || !reflect.DeepEqual(*result.SourceSnapshot, source) {
		return request, fmt.Errorf("publication requires native full source-bound verified proof")
	}
	var reservation ProofResourceReservation
	if err := s.readEventJSON(result.ReservationID, &reservation); err != nil {
		return request, err
	}
	request = PrivateSourcePublishRequest{SchemaVersion: PrivateSourcePublishSchema, WorkID: workID, Actor: actor, Repo: repo,
		CreatedAt: now.UTC().Format(time.RFC3339Nano), Source: source, ProofRef: result.VerificationRef, ProfileDigest: reservation.Profile.Digest,
		ReviewActionID: reviewActionID, Destination: authority.Destination}
	request.ExpectedRemoteSHA, err = observePrivateSourceTip(ctx, repo, request.Destination, privateSourcePublishCommand)
	if err != nil {
		return request, err
	}
	request.Action, err = privatePublishAction(w, request)
	if err != nil {
		return request, err
	}
	return request, s.validatePrivatePublication(ctx, request, now, false)
}

// validatePrivatePublication re-opens the native finalizer chain and the full
// private scan evidence. A caller-supplied proof string is never a certificate.
func (s *Store) validatePrivatePublication(ctx context.Context, request PrivateSourcePublishRequest, now time.Time, remoteMayHaveAdvanced bool) error {
	w, err := s.Get(request.WorkID)
	if err != nil {
		return err
	}
	if request.SchemaVersion != PrivateSourcePublishSchema || !privateSourceCommitSHA.MatchString(request.ExpectedRemoteSHA) || w.GitDelivery == nil || request.Repo == "" {
		return fmt.Errorf("invalid private source publication request")
	}
	expected, err := privatePublishAction(w, request)
	if err != nil || !reflect.DeepEqual(expected, request.Action) {
		return fmt.Errorf("publication action does not bind exact request")
	}
	source, err := gitrepository.CaptureCleanSnapshot(request.Repo, w.GitDelivery.ExpectedBaseSHA)
	if err != nil || !reflect.DeepEqual(source, request.Source) {
		return fmt.Errorf("publication source is dirty or changed")
	}
	authority, err := s.resolvePrivateSourceReview(request.WorkID, request.Actor, request.ReviewActionID, request.Repo, source, now, true)
	if err != nil {
		return err
	}
	if !samePrivateSourceDestination(request.Destination, authority.Destination) {
		return fmt.Errorf("publication authenticated destination changed")
	}
	if err := s.validatePrivateSourceCertificate(w, request, source, authority); err != nil {
		return err
	}
	// Preserve canonical-checkout WIP: read the target ref without checking out
	// or staging anything in that worktree.
	local, err := privateSourcePublishCommand(ctx, w.GitDelivery.TargetWorktreeRoot, "git", "rev-parse", "--verify", w.GitDelivery.TargetLocalRef+"^{commit}")
	if err != nil || strings.TrimSpace(string(local)) != source.HeadSHA {
		return fmt.Errorf("publication source has not landed on exact local target")
	}
	if _, err := privateSourcePublishCommand(ctx, request.Repo, "git", "merge-base", "--is-ancestor", source.BaseSHA, request.ExpectedRemoteSHA); err != nil {
		return fmt.Errorf("private source publication includes an uncertified pre-base range")
	}
	if _, err := privateSourcePublishCommand(ctx, request.Repo, "git", "merge-base", "--is-ancestor", request.ExpectedRemoteSHA, source.HeadSHA); err != nil {
		return fmt.Errorf("private source publication is not a known non-force fast-forward")
	}
	if err := privateSourceTransport(ctx, request, privateSourcePublishCommand); err != nil {
		return err
	}
	tip, err := observePrivateSourceTip(ctx, request.Repo, authority.Destination, privateSourcePublishCommand)
	if err != nil || (tip != request.ExpectedRemoteSHA && (!remoteMayHaveAdvanced || tip != source.HeadSHA)) {
		return fmt.Errorf("publication remote branch changed")
	}
	return nil
}

// This reopens certificate evidence; it never grants a publication action.
func (s *Store) validatePrivateSourceCertificate(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, source gitrepository.Snapshot, authority PrivateSourceAuthority) error {
	result, found, err := s.latestSourceBoundProofResult(w)
	if err != nil || !found || result.VerificationRef != request.ProofRef || !strings.HasPrefix(request.ProofRef, "proof-check:pfrs_") || result.SourceSnapshot == nil || !reflect.DeepEqual(*result.SourceSnapshot, source) {
		return fmt.Errorf("publication native proof changed or is unavailable")
	}
	var reservation ProofResourceReservation
	if err := s.readEventJSON(result.ReservationID, &reservation); err != nil {
		return err
	}
	if reservation.Profile.Kind != ProofResourceProfileCanonicalFull || reservation.Profile.Digest != request.ProfileDigest {
		return fmt.Errorf("publication proof profile changed")
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return err
	}
	body, err := artifacts.Get(result.ReportArtifactRef)
	if err != nil {
		return err
	}
	report, err := decodeOwnedProofReport(body)
	if err != nil {
		return err
	}
	privateSteps := 0
	for _, recorded := range report.Steps {
		step := ProofCheckStep{Name: recorded.Name, Command: recorded.Command, Dir: recorded.Dir, SkipIfMissing: recorded.SkipIfMissing, ProofRef: recorded.ProofRef, Status: recorded.Status, RedactionEvidence: recorded.RedactionEvidence, PrivateSourceEvidence: recorded.PrivateSourceEvidence}
		binding, private, err := PrivateSourceBindingForStep(step)
		if err != nil {
			return err
		}
		if private {
			privateSteps++
			if binding != authority.Binding || step.Status != "ok" {
				return fmt.Errorf("publication private evidence does not bind review")
			}
			if err := ValidatePrivateSourceEvidence(step, &source, request.WorkID); err != nil {
				return err
			}
		}
	}
	if privateSteps != 1 {
		return fmt.Errorf("publication requires one full private source scan")
	}
	return nil
}

func privateSourceTransport(ctx context.Context, request PrivateSourcePublishRequest, command privateSourceCommand) error {
	body, err := command(ctx, request.Repo, "git", "ls-remote", "--get-url", request.Destination.CanonicalURL)
	if err != nil || strings.TrimSpace(string(body)) != request.Destination.CanonicalURL {
		return fmt.Errorf("publication transport URL is unavailable or rewritten")
	}
	return nil
}

func privateSourcePushArguments(request PrivateSourcePublishRequest) []string {
	return []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", request.Destination.CanonicalURL, request.Source.HeadSHA + ":" + request.Destination.TargetRef}
}

func observePrivateSourceTip(ctx context.Context, repo string, destination PrivateSourceDestination, command privateSourceCommand) (string, error) {
	branch := strings.TrimPrefix(destination.TargetRef, "refs/heads/")
	body, err := command(ctx, repo, "gh", "api", "--hostname", "github.com", "repos/"+destination.FullName+"/git/ref/heads/"+url.PathEscape(branch))
	if err != nil {
		return "", err
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal(body, &ref) != nil || ref.Ref != destination.TargetRef || ref.Object.Type != "commit" || !privateSourceCommitSHA.MatchString(ref.Object.SHA) {
		return "", fmt.Errorf("authenticated publication branch unavailable")
	}
	return ref.Object.SHA, nil
}

func (s *Store) authorizePrivatePublication(request PrivateSourcePublishRequest, now time.Time) (envelope.PolicyDecision, error) {
	var decision envelope.PolicyDecision
	err := home.WithAutonomyAdmissionLock(s.home, func() error {
		cfg, _, err := policy.LoadConfig(s.home)
		if err != nil {
			return err
		}
		decision = policy.Evaluate(request.Action, cfg, now)
		if decision.Outcome == "deny" {
			return fmt.Errorf("private publication policy denies action")
		}
		// Even a broad standing grant cannot replace this concrete publication's
		// exact approval. Do not alter the release mutation class or global grants.
		decision.Outcome, decision.StandingGrantID, decision.ExpiresAt = "hold", "", ""
		decision.Obligations = nil
		decision.Reasons = append(decision.Reasons, "native private source publication requires exact one-use operator approval")
		if err := s.writeEventJSON(request.Action.ID, request.Action); err != nil {
			return err
		}
		decision, _, err = policy.NewExactActionApprovalStore(s.home).ApplyMatchingLocked(request.Action, cfg, decision, now)
		if err != nil {
			return err
		}
		if !policy.Allowed(decision) {
			return fmt.Errorf("private publication action %s policy %s", request.Action.ID, decision.Outcome)
		}
		return s.writeEventJSON(decision.ID, decision)
	})
	return decision, err
}

func (s *Store) recheckPrivatePublication(request PrivateSourcePublishRequest, decision envelope.PolicyDecision, now time.Time) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	expiry, err := time.Parse(time.RFC3339Nano, decision.ExpiresAt)
	if err != nil || !now.Before(expiry) || !policy.Allowed(decision) || decision.ActionIntentID != request.Action.ID || decision.IDs.WorkID != request.WorkID || decision.StandingGrantID != "" {
		return fmt.Errorf("private publication exact decision expired or changed")
	}
	var recorded envelope.PolicyDecision
	if err := s.readEventJSON(decision.ID, &recorded); err != nil || !reflect.DeepEqual(recorded, decision) {
		return fmt.Errorf("private publication native decision unavailable or changed")
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return err
	}
	for _, class := range []string{"build-implement", PrivateSourcePublishCapability} {
		halted, _, err := policy.AutonomyHalted(s.home, class)
		if err != nil || halted {
			return fmt.Errorf("private source publication halted")
		}
	}
	if policy.Evaluate(request.Action, cfg, now).Outcome == "deny" || !s.hasConsumedPublicReviewApproval(request.Action, decision, cfg, now) {
		return fmt.Errorf("exact private publication authority unavailable")
	}
	return nil
}

// ApplyPrivateSourcePublication owns the only transport effect. Pending retries
// observe and reconcile; they never repeat a possibly completed push.
func (s *Store) ApplyPrivateSourcePublication(ctx context.Context, request PrivateSourcePublishRequest, now time.Time) (PrivateSourcePublishResult, error) {
	var out PrivateSourcePublishResult
	w, err := s.Get(request.WorkID)
	if err != nil {
		return out, err
	}
	expected, err := privatePublishAction(w, request)
	if err != nil || !reflect.DeepEqual(expected, request.Action) {
		return out, fmt.Errorf("publication request action mismatch")
	}
	pending, authorizedAt, exists, err := s.ReadReceiptedActionReservationInput(request.WorkID, request.Action.ID)
	if err != nil {
		return out, err
	}
	if exists {
		if !reflect.DeepEqual(pending.Action, request.Action) {
			return out, fmt.Errorf("publication reservation action mismatch")
		}
		if !pending.OutcomePending {
			result, err := s.recordReservedReceiptedAction(request.WorkID, pending, authorizedAt, &privatePublicationCompletion{request: request})
			out.Status, out.Action = "receipted", &result
			return out, err
		}
		return s.finishPrivatePublication(ctx, request, pending, authorizedAt, false, false)
	}
	completion, completed, _, err := s.preflightReceiptedActionCompletion(request.WorkID, request.Action.ID)
	if err != nil {
		return out, err
	}
	if completed {
		result, err := s.replayPrivateSourcePublication(request, completion)
		out.Status, out.Action = "receipted", &result
		return out, err
	}
	if w.State != "running" {
		return out, fmt.Errorf("private source publication requires running work before approval consumption")
	}
	if err := s.validatePrivatePublication(ctx, request, now, false); err != nil {
		return out, err
	}
	if err := s.preflightPrivatePublicationPrelink(request, now); err != nil {
		return out, err
	}
	decision, err := s.authorizePrivatePublication(request, now)
	if err != nil {
		return out, err
	}
	pending = ReceiptedActionInput{OutcomePending: true, Action: request.Action, PreEvaluatedDecision: &decision, ExpectedWorkState: w.State,
		Outcome: "blocked", Summary: "private source publication pending observation", MoveType: "outward_delta",
		ProofRefs: []string{request.ProofRef}, BeforeRefs: []string{"git:" + request.Destination.FullName + "@" + request.ExpectedRemoteSHA}}
	err = s.ReserveReceiptedAction(request.WorkID, pending, now, func() error {
		if err := s.recheckPrivatePublication(request, decision, time.Now()); err != nil {
			return err
		}
		if err := s.validatePrivatePublication(ctx, request, time.Now(), false); err != nil {
			return err
		}
		return s.preflightPrivatePublicationPrelink(request, now)
	})
	if err != nil {
		return out, err
	}
	// Recheck after durable reservation, immediately before process launch.
	if err := s.recheckPrivatePublication(request, decision, time.Now()); err != nil {
		return out, err
	}
	if err := s.validatePrivatePublication(ctx, request, time.Now(), false); err != nil {
		return out, err
	}
	if err := s.preflightPrivatePublicationPrelink(request, now); err != nil {
		return out, err
	}
	current, currentAt, present, err := s.ReadReceiptedActionReservationInput(request.WorkID, request.Action.ID)
	if err != nil || !present || !currentAt.Equal(now) || !reflect.DeepEqual(current, pending) {
		return out, fmt.Errorf("private publication lost exact native action reservation before launch")
	}
	_, pushErr := privateSourcePublishCommand(ctx, request.Repo, "git", privateSourcePushArguments(request)...)
	return s.finishPrivatePublication(ctx, request, pending, now, true, pushErr == nil)
}

func (s *Store) replayPrivateSourcePublication(request PrivateSourcePublishRequest, completion receiptedActionCompletion) (ReceiptedActionResult, error) {
	var receipt envelope.Receipt
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(completion.ReceiptID, &receipt); err != nil {
		return ReceiptedActionResult{}, err
	}
	if err := s.readEventJSON(completion.PolicyDecisionID, &decision); err != nil {
		return ReceiptedActionResult{}, err
	}
	if len(receipt.ProofRefs) < 2 {
		return ReceiptedActionResult{}, fmt.Errorf("private publication completion has no exact source observation")
	}
	final := ReceiptedActionInput{Action: request.Action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", Outcome: "completed", Changed: request.ExpectedRemoteSHA != request.Source.HeadSHA,
		Summary: privatePublicationSuccessSummary, MoveType: "outward_delta", ProofRefs: append([]string(nil), receipt.ProofRefs[:2]...),
		BeforeRefs: []string{"git:" + request.Destination.FullName + "@" + request.ExpectedRemoteSHA}, AfterRefs: []string{"git:" + request.Destination.FullName + "@" + request.Source.HeadSHA}}
	return s.replayCompletedReceiptedActionWithPublication(request.WorkID, final, completion, &privatePublicationCompletion{request: request})
}

func (s *Store) finishPrivatePublication(ctx context.Context, request PrivateSourcePublishRequest, pending ReceiptedActionInput, authorizedAt time.Time, attempted, transportOK bool) (PrivateSourcePublishResult, error) {
	out := PrivateSourcePublishResult{Status: "pending"}
	w, err := s.Get(request.WorkID)
	if err != nil || w.GitDelivery == nil {
		return out, fmt.Errorf("publication reconciliation work unavailable")
	}
	observed, destinationErr := privateSourceLiveDestination(ctx, request.Repo, *w.GitDelivery, time.Now())
	tip, tipErr := observePrivateSourceTip(ctx, request.Repo, request.Destination, privateSourcePublishCommand)
	confirmed := destinationErr == nil && samePrivateSourceDestination(request.Destination, observed) && tipErr == nil && tip == request.Source.HeadSHA
	observation := privatePublicationObservation{SchemaVersion: "compound.private_source_publication_observation.v1", ActionID: request.Action.ID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Destination: observed, RemoteSHA: tip, PushAttempted: attempted, TransportSucceeded: transportOK, RemoteConfirmed: confirmed}
	var trackingErr error
	if confirmed {
		// CAS only this bound tracking ref. Concurrent bookkeeping is preserved.
		tracking, err := privateSourcePublishCommand(ctx, request.Repo, "git", "rev-parse", "--verify", request.Destination.PublicationRef+"^{commit}")
		if err != nil {
			trackingErr = err
		} else if strings.TrimSpace(string(tracking)) != request.Source.HeadSHA {
			if strings.TrimSpace(string(tracking)) != request.ExpectedRemoteSHA {
				trackingErr = fmt.Errorf("publication tracking ref changed concurrently")
			} else {
				_, trackingErr = privateSourcePublishCommand(ctx, request.Repo, "git", "update-ref", request.Destination.PublicationRef, request.Source.HeadSHA, request.ExpectedRemoteSHA)
			}
		}
		observation.TrackingConfirmed = trackingErr == nil
	}
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
	out.RemoteSHA = tip
	if !confirmed || trackingErr != nil {
		return out, fmt.Errorf("private publication outcome pending reconciliation; observation %s; no automatic repush", out.ObservationRef)
	}
	final := pending
	final.OutcomePending, final.Outcome, final.Changed = false, "completed", request.ExpectedRemoteSHA != request.Source.HeadSHA
	final.Summary = privatePublicationSuccessSummary
	final.AfterRefs = []string{"git:" + request.Destination.FullName + "@" + tip}
	final.ProofRefs = append(append([]string(nil), pending.ProofRefs...), "evidence:"+out.ObservationRef)
	if err := s.SealReceiptedActionOutcome(request.WorkID, pending, final, authorizedAt); err != nil {
		return out, err
	}
	result, err := s.recordReservedReceiptedAction(request.WorkID, final, authorizedAt, &privatePublicationCompletion{request: request})
	out.Status, out.Action = "published_pending_receipt", &result
	if err == nil {
		out.Status = "published"
	}
	return out, err
}
