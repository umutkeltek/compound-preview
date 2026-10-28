package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

const (
	ProofAdoptionVerificationPrefix = "proof-adopt:"
	ProofManifestArtifactPrefix     = "proof-manifest:"
	ProofMemberManifestSchema       = "compound.proof_member_manifest.v0.1"
)

// ProofMemberManifest records explicit review membership at one exact candidate.
// It is immutable artifact custody, not a new proof, execution or authority owner.
type ProofMemberManifest struct {
	SchemaVersion         string                `json:"schema_version"`
	SourceWorkID          string                `json:"source_work_id"`
	RepositoryFingerprint string                `json:"repository_fingerprint"`
	BaseSHA               string                `json:"base_sha"`
	HeadSHA               string                `json:"head_sha"`
	TargetLocalRef        string                `json:"target_local_ref"`
	Members               []ProofReviewedMember `json:"members"`
}

type ProofReviewedMember struct {
	WorkID               string   `json:"work_id"`
	OriginalCandidateRef string   `json:"original_candidate_ref"`
	ContractDigest       string   `json:"contract_digest"`
	ReviewRefs           []string `json:"review_refs"`
}

type ProofAdoptionInput struct {
	Actor                    string
	ProofResultID            string
	ManifestRef              string
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
}

type ProofManifestResult struct {
	WorkID      string `json:"work_id"`
	ManifestRef string `json:"manifest_ref"`
	ArtifactRef string `json:"artifact_ref"`
}

// ProofReviewInput custodies one holder-supplied review body for this work only.
type ProofReviewInput struct {
	Actor                    string
	Body                     []byte
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
}

type ProofReviewResult struct {
	WorkID    string `json:"work_id"`
	ReviewRef string `json:"review_ref"`
}

// RecordProofReview binds immutable review bytes to a native same-state action.
// Custody follows attachment authority; the same bytes safely finish an
// interrupted body write without minting a second action or foreign attachment.
func (s *Store) RecordProofReview(workID string, in ProofReviewInput, now time.Time) (ProofReviewResult, error) {
	var out ProofReviewResult
	if len(in.Body) > 1<<20 || len(bytes.TrimSpace(in.Body)) == 0 {
		return out, fmt.Errorf("proof review requires 1..1048576 bytes of nonempty content")
	}
	body := append([]byte(nil), in.Body...)
	sum := sha256.Sum256(body)
	ref := home.ArtifactRefPrefix + hex.EncodeToString(sum[:])
	err := s.withLifecycleMutation(func() error {
		w, err := s.proofAdoptionMutableWork(workID, in.Actor, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID, ref, now)
		if err != nil {
			return err
		}
		reader, err := s.proofAdoptionArtifactReader()
		if err != nil {
			return err
		}
		// Refuse corrupt existing custody before appending a new action.
		if _, err := reader.Get(ref); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if oneOf(ref, w.ArtifactIDs) {
			if err := s.validateProofAttachment(w, ref); err != nil {
				return err
			}
		} else {
			_, err = s.advanceLocked(workID, AdvanceInput{
				To: w.State, Actor: in.Actor, ArtifactID: ref,
				ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID,
				ExpectedActionIntentID: latestActionIntentID(w), proofReviewAdmission: true,
				Note: "custody exact work proof review",
			}, now)
			if err != nil {
				return err
			}
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return err
		}
		stored, err := artifacts.Put(body)
		if err != nil {
			return err
		}
		if stored != ref {
			return fmt.Errorf("proof review custody digest mismatch")
		}
		out = ProofReviewResult{WorkID: workID, ReviewRef: ref}
		return nil
	})
	return out, err
}

func proofAttachmentPresent(w envelope.WorkEnvelope, ref string) bool {
	if strings.HasPrefix(ref, home.ArtifactRefPrefix) {
		return oneOf(ref, w.ArtifactIDs)
	}
	return oneOf(ref, w.VerificationIDs)
}

func proofManifestBody(m ProofMemberManifest) ([]byte, string, error) {
	if m.SchemaVersion != ProofMemberManifestSchema || !validProofWorkID(m.SourceWorkID) ||
		!validProofDigest(m.RepositoryFingerprint) || !validLandingObjectID(m.BaseSHA) ||
		!validLandingObjectID(m.HeadSHA) || !strings.HasPrefix(m.TargetLocalRef, "refs/heads/") ||
		len(m.Members) < 2 || len(m.Members) > 64 {
		return nil, "", fmt.Errorf("proof manifest requires exact source, repository, base, candidate, target and 2..64 reviewed members")
	}
	seen := map[string]bool{}
	for _, member := range m.Members {
		if !validProofWorkID(member.WorkID) || seen[member.WorkID] || !validProofDigest(member.ContractDigest) || len(member.ReviewRefs) == 0 || len(member.ReviewRefs) > 8 ||
			!strings.HasPrefix(member.OriginalCandidateRef, "commit:") ||
			!validLandingObjectID(strings.TrimPrefix(member.OriginalCandidateRef, "commit:")) {
			return nil, "", fmt.Errorf("proof manifest member identity or preserved candidate is invalid")
		}
		seen[member.WorkID] = true
	}
	if !seen[m.SourceWorkID] {
		return nil, "", fmt.Errorf("proof manifest must include its original source work")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, home.ArtifactRefPrefix + hex.EncodeToString(sum[:]), nil
}

func validProofWorkID(id string) bool {
	return strings.HasPrefix(id, "work_") && len(id) > len("work_") &&
		len(id) <= 128 && filepath.Base(id) == id && !strings.ContainsAny(id, ":\\ \t\r\n")
}

func validProofDigest(digest string) bool {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func parseProofAdoptionRef(ref string) (resultID, manifestRef string, ok bool) {
	parts := strings.Split(ref, ":")
	if len(parts) != 3 || parts[0] != "proof-adopt" ||
		!strings.HasPrefix(parts[1], "pfrs_") || len(parts[1]) <= 5 ||
		filepath.Base(parts[1]) != parts[1] || strings.ContainsAny(parts[1], "\\ \t\r\n") ||
		!validProofDigest(parts[2]) {
		return "", "", false
	}
	return parts[1], home.ArtifactRefPrefix + parts[2], true
}

func proofAdoptionRef(resultID, manifestRef string) (string, error) {
	digest, err := home.ArtifactHex(manifestRef)
	if err != nil {
		return "", err
	}
	ref := ProofAdoptionVerificationPrefix + resultID + ":" + digest
	if _, _, ok := parseProofAdoptionRef(ref); !ok {
		return "", fmt.Errorf("invalid exact proof adoption reference")
	}
	return ref, nil
}

func proofMemberContractDigest(w envelope.WorkEnvelope) string {
	body, _ := json.Marshal(struct {
		Delivery  *envelope.GitDeliveryContract
		Outcome   string
		Predicate string
		NonGoals  []string
	}{w.GitDelivery, w.DesiredOutcome, w.SuccessPredicate, w.NonGoals})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (s *Store) validateProofManifestWork(m ProofMemberManifest, w envelope.WorkEnvelope) error {
	if w.GitDelivery == nil {
		return fmt.Errorf("work %s has no Git delivery contract", w.ID)
	}
	c := w.GitDelivery
	if c.RepositoryFingerprint != m.RepositoryFingerprint || c.TargetLocalRef != m.TargetLocalRef {
		return fmt.Errorf("work %s delivery contract does not match exact reviewed repository/base/target", w.ID)
	}
	if err := s.validateProofManifestBase(c, m, w.ID); err != nil {
		return err
	}
	_, tip, found, valid := latestCommitArtifact(w.ArtifactIDs)
	if !found || !valid || tip != m.HeadSHA {
		return fmt.Errorf("work %s has not explicitly accepted exact candidate %s", w.ID, m.HeadSHA)
	}
	for _, member := range m.Members {
		if member.WorkID == w.ID {
			reader, err := s.proofAdoptionArtifactReader()
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, ref := range member.ReviewRefs {
				if seen[ref] || (!oneOf(ref, w.ArtifactIDs) && !oneOf(ref, w.EvidenceRefs)) {
					return fmt.Errorf("work %s lacks distinct attached review evidence %s", w.ID, ref)
				}
				seen[ref] = true
				if _, err := home.ArtifactHex(ref); err != nil {
					return err
				}
				body, err := reader.Get(ref)
				if err != nil || len(body) == 0 || len(body) > 1<<20 {
					return fmt.Errorf("work %s review evidence is missing, corrupt or unbounded: %v", w.ID, err)
				}
			}

			if member.ContractDigest != proofMemberContractDigest(w) {
				return fmt.Errorf("work %s reviewed acceptance/delivery contract changed", w.ID)
			}
			if !oneOf(member.OriginalCandidateRef, w.ArtifactIDs) {
				return fmt.Errorf("work %s has no preserved original candidate %s", w.ID, member.OriginalCandidateRef)
			}
			return nil
		}
	}
	return fmt.Errorf("work %s is not an explicit reviewed member", w.ID)
}

// proofContainedBaseReadTimeout bounds the immutable repository reads one
// contained-base admission performs.
const proofContainedBaseReadTimeout = 30 * time.Second

// validateProofManifestBase admits a reviewed member whose immutable creation
// base either precedes the reviewed proof base (the original lineage) or
// is contained between that base and the reviewed head. Ancestry alone never
// supplies coverage: a contained base must additionally prove that every
// content-bearing member change appears in the one proof's own scanned
// inventory and that its own range is independently whitespace-clean.
func (s *Store) validateProofManifestBase(c *envelope.GitDeliveryContract, m ProofMemberManifest, workID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), proofContainedBaseReadTimeout)
	defer cancel()
	// Creation-time delivery bases may differ. The reviewed battery base must
	// descend from each member's original contract, and certify the same H.
	ancestor, err := gitrepository.CommittedAncestor(ctx, c.TargetWorktreeRoot, c.ExpectedBaseSHA, m.BaseSHA)
	if err != nil {
		return fmt.Errorf("work %s creation base is outside reviewed base lineage: %v", workID, err)
	}
	if ancestor {
		return nil
	}
	contained, err := gitrepository.CommittedAncestor(ctx, c.TargetWorktreeRoot, m.BaseSHA, c.ExpectedBaseSHA)
	if err != nil || !contained {
		return fmt.Errorf("work %s creation base is outside reviewed base lineage: %v", workID, err)
	}
	reaches, err := gitrepository.CommittedAncestor(ctx, c.TargetWorktreeRoot, c.ExpectedBaseSHA, m.HeadSHA)
	if err != nil || !reaches {
		return fmt.Errorf("work %s creation base does not reach the reviewed head: %v", workID, err)
	}
	return s.validateProofContainedBaseCoverage(ctx, c, m, workID)
}

// validateProofContainedBaseCoverage proves that the one canonical proof
// covered the contained member's whole delivery. Both inventories are read at
// the same immutable head, so the bodies the proof scanned are the bodies the
// member delivered. A path the member changed that the proof never scanned is
// a coverage hole, not a covered delivery.
func (s *Store) validateProofContainedBaseCoverage(ctx context.Context, c *envelope.GitDeliveryContract, m ProofMemberManifest, workID string) error {
	proofPaths, err := gitrepository.CommittedContentPaths(ctx, c.TargetWorktreeRoot, m.BaseSHA, m.HeadSHA)
	if err != nil {
		return fmt.Errorf("work %s reviewed proof inventory is unreadable: %w", workID, err)
	}
	memberPaths, err := gitrepository.CommittedContentPaths(ctx, c.TargetWorktreeRoot, c.ExpectedBaseSHA, m.HeadSHA)
	if err != nil {
		return fmt.Errorf("work %s member change inventory is unreadable: %w", workID, err)
	}
	covered := make(map[string]struct{}, len(proofPaths))
	for _, path := range proofPaths {
		covered[path] = struct{}{}
	}
	for _, path := range memberPaths {
		if _, ok := covered[path]; !ok {
			return fmt.Errorf("work %s member change %q is not covered by the reviewed proof inventory", workID, path)
		}
	}
	if err := gitrepository.CommittedDiffCheck(ctx, c.TargetWorktreeRoot, c.ExpectedBaseSHA, m.HeadSHA); err != nil {
		return fmt.Errorf("work %s member range diff-check failed: %w", workID, err)
	}
	return nil
}

func (s *Store) proofAdoptionArtifactReader() (*home.ArtifactReader, error) {
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return nil, err
	}
	return home.OpenArtifactReader(canonical)
}

func proofAttachmentArgumentsHash(ref string) string {
	digest := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (s *Store) loadProofManifest(ref string) (ProofMemberManifest, error) {
	var m ProofMemberManifest
	reader, err := s.proofAdoptionArtifactReader()
	if err != nil {
		return m, err
	}
	body, err := reader.Get(ref)
	if err != nil {
		return m, err
	}
	if len(body) > 1<<20 {
		return m, fmt.Errorf("proof manifest exceeds bounded size")
	}
	if err := publicReviewStrictJSON(body, &m); err != nil {
		return m, err
	}
	canonical, canonicalRef, err := proofManifestBody(m)
	if err != nil {
		return m, err
	}
	if canonicalRef != ref || string(canonical) != string(body) {
		return m, fmt.Errorf("proof manifest is not canonical immutable content")
	}
	return m, nil
}

// validateProofAttachment authenticates the historical same-state action and
// policy. Later land/close generations may legitimately replace current policy.
func (s *Store) validateProofAttachment(w envelope.WorkEnvelope, ref string) error {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return err
	}
	matches := 0
	for _, event := range events {
		if !oneOf(ref, event.Attached) {
			continue
		}
		if event.From != "verifying" || event.To != "verifying" || len(event.Attached) < 3 {
			return fmt.Errorf("proof attachment has no exact verifying admission")
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(event.Attached[0], &action); err != nil {
			return err
		}
		if action.ArgumentsHash != proofAttachmentArgumentsHash(ref) {
			return fmt.Errorf("proof attachment action does not bind exact evidence")
		}
		historical := w
		historical.State = event.To
		historical.PolicyDecisionID = event.Attached[1]
		if err := s.validatePendingAdvance(event, historical); err != nil {
			return fmt.Errorf("proof attachment historical authority: %w", err)
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("proof attachment needs one authenticated admission, found %d", matches)
	}
	return nil
}

func (s *Store) proofAdoptionMutableWork(workID, actor, expectedPolicy, expectedAction, idempotentRef string, now time.Time) (envelope.WorkEnvelope, error) {
	w, err := s.Get(workID)
	if err != nil {
		return w, err
	}
	// A foreign caller must not recover another holder's pending mutation.
	if w.State != "verifying" || strings.TrimSpace(actor) == "" {
		return w, fmt.Errorf("proof operation requires verifying work and current holder")
	}
	leaseBefore, err := s.readLease(w.ID)
	if err != nil {
		return w, err
	}
	if leaseBefore.By != actor {
		return w, fmt.Errorf("proof operation requires current work holder")
	}
	if idempotentRef != "" {
		var pending envelope.WorkEnvelope
		if readErr := s.readJSON(s.advancePendingPath(w.ID), &pending); readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				return w, readErr
			}
		} else {
			var action envelope.ActionIntent
			if err := s.readEventJSON(latestActionIntentID(pending), &action); err != nil {
				return w, err
			}
			if !proofAttachmentPresent(pending, idempotentRef) || action.ArgumentsHash != proofAttachmentArgumentsHash(idempotentRef) {
				return w, fmt.Errorf("pending unrelated lifecycle mutation must be recovered before proof adoption")
			}
		}
	}
	if err := validateExpectedExecutionReservation(w, "verifying", expectedPolicy, expectedAction); err != nil {
		// A completed exact retry can return its authenticated existing state.
		// It must not replay some other pending operation to satisfy stale CAS.
		if idempotentRef != "" && proofAttachmentPresent(w, idempotentRef) {
			if strings.HasPrefix(idempotentRef, home.ArtifactRefPrefix) {
				var latest envelope.ActionIntent
				if readErr := s.readEventJSON(latestActionIntentID(w), &latest); readErr != nil {
					return w, readErr
				}
				if latest.ArgumentsHash != proofAttachmentArgumentsHash(idempotentRef) {
					return w, err
				}
				if gateErr := s.leaseHolderAdvanceGate(w, actor, AdvanceInput{To: w.State, Actor: actor}, now); gateErr != nil {
					return w, gateErr
				}
			}
			if authErr := s.validateProofAttachment(w, idempotentRef); authErr != nil {
				return w, authErr
			}
			return w, nil
		}
		return w, err
	}
	// Recover a prior same-state attachment before deciding whether this operation
	// already happened. Otherwise advanceLocked can return a different recovered
	// mutation while this caller mistakenly reports adoption success.
	if recovered, ok, recoveryErr := s.recoverPendingAdvance(w, w.State, nil); recoveryErr != nil {
		return w, recoveryErr
	} else if ok {
		w = recovered
	}
	if w.State != "verifying" || strings.TrimSpace(actor) == "" {
		return w, fmt.Errorf("proof adoption and manifest custody require verifying work and its current holder")
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		return w, err
	}
	if lease.By != actor {
		return w, fmt.Errorf("proof operation requires current work holder")
	}
	if err := s.leaseHolderAdvanceGate(w, actor, AdvanceInput{To: w.State, Actor: actor}, now); err != nil {
		return w, err
	}
	return w, nil
}

// RecordProofManifest attaches custody authority through the existing lifecycle
// before writing the immutable body. Interrupted body writes can be retried by
// the holder using the same content and authenticated attachment.
func (s *Store) RecordProofManifest(workID, actor string, manifest ProofMemberManifest, now time.Time) (ProofManifestResult, error) {
	var out ProofManifestResult
	err := s.withLifecycleMutation(func() error {
		w, err := s.proofAdoptionMutableWork(workID, actor, "", "", "", now)
		if err != nil {
			return err
		}
		if manifest.SourceWorkID != workID {
			return fmt.Errorf("only original source work may custody reviewed membership")
		}
		if len(manifest.Members) < 2 || len(manifest.Members) > 64 {
			return fmt.Errorf("reviewed manifest needs 2..64 members")
		}
		manifest.Members = append([]ProofReviewedMember(nil), manifest.Members...)
		for i := range manifest.Members {
			member := &manifest.Members[i]
			item, err := s.Get(member.WorkID)
			if err != nil {
				return err
			}
			if member.ContractDigest == "" {
				member.ContractDigest = proofMemberContractDigest(item)
			}
			if err := s.validateProofManifestWork(manifest, item); err != nil {
				return err
			}
		}
		body, ref, err := proofManifestBody(manifest)
		if err != nil {
			return err
		}
		attachment := ProofManifestArtifactPrefix + ref
		if oneOf(attachment, w.ArtifactIDs) {
			if err := s.validateProofAttachment(w, attachment); err != nil {
				return err
			}
		} else {
			_, err = s.advanceLocked(workID, AdvanceInput{
				To: w.State, Actor: actor, ArtifactID: attachment,
				ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID,
				ExpectedActionIntentID: latestActionIntentID(w), proofManifestAdmission: true,
				Note: "custody exact reviewed proof membership",
			}, now)
			if err != nil {
				return err
			}
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return err
		}
		stored, err := artifacts.Put(body)
		if err != nil {
			return err
		}
		if stored != ref {
			return fmt.Errorf("proof manifest custody digest mismatch")
		}
		out = ProofManifestResult{WorkID: workID, ManifestRef: ref, ArtifactRef: attachment}
		return nil
	})
	return out, err
}

// AdoptProof records one-hop acceptance of the unchanged original proof. It does
// not attach the source attempt to the destination or satisfy product acceptance.
func (s *Store) AdoptProof(workID string, in ProofAdoptionInput, now time.Time) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	err := s.withLifecycleMutation(func() error {
		ref, err := proofAdoptionRef(in.ProofResultID, in.ManifestRef)
		if err != nil {
			return err
		}
		w, err := s.proofAdoptionMutableWork(workID, in.Actor, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID, ref, now)
		if err != nil {
			return err
		}
		if _, err := s.validateProofAdoptionRef(w, ref, false); err != nil {
			return err
		}
		if oneOf(ref, w.VerificationIDs) {
			if err := s.validateProofAttachment(w, ref); err != nil {
				return err
			}
			out = w
			return nil
		}
		if err := validateExpectedExecutionReservation(w, "verifying", in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID); err != nil {
			return err
		}
		out, err = s.advanceLocked(workID, AdvanceInput{
			To: w.State, Actor: in.Actor, VerificationID: ref,
			ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID,
			ExpectedActionIntentID: latestActionIntentID(w), proofAdoptionAdmission: true,
			Note: "adopt exact original canonical proof for reviewed member",
		}, now)
		return err
	})
	return out, err
}

func (s *Store) validateProofAdoptionRef(w envelope.WorkEnvelope, ref string, historical bool) (ProofResourceResult, error) {
	var zero ProofResourceResult
	resultID, manifestRef, ok := parseProofAdoptionRef(ref)
	if !ok {
		return zero, fmt.Errorf("malformed proof adoption reference")
	}
	m, err := s.loadProofManifest(manifestRef)
	if err != nil {
		return zero, err
	}
	if w.ID == m.SourceWorkID {
		return zero, fmt.Errorf("source work uses its direct proof, not adoption")
	}
	if err := s.validateProofManifestWork(m, w); err != nil {
		return zero, err
	}
	source, err := s.Get(m.SourceWorkID)
	if err != nil {
		return zero, err
	}
	if err := s.validateProofManifestWork(m, source); err != nil {
		return zero, err
	}
	attachment := ProofManifestArtifactPrefix + manifestRef
	if !oneOf(attachment, source.ArtifactIDs) {
		return zero, fmt.Errorf("original source did not custody this membership")
	}
	if err := s.validateProofAttachment(source, attachment); err != nil {
		return zero, err
	}
	directRef := "proof-check:" + resultID
	if !oneOf(directRef, source.VerificationIDs) {
		return zero, fmt.Errorf("source has no direct original proof attachment")
	}
	// Narrow the lookup, retaining the original source work identity and all its
	// lineage. This does not broaden the direct-proof resolver used by publication.
	exact := source
	exact.VerificationIDs = []string{directRef}
	result, found, err := s.latestSourceBoundProofResult(exact)
	if err != nil {
		return zero, err
	}
	if !found || result.SourceSnapshot == nil {
		return zero, fmt.Errorf("original source proof is unavailable")
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(result.ReservationID, &reservation); err != nil {
		return zero, err
	}
	var manifestAction string
	events, err := s.eventsFor(source.ID)
	if err != nil {
		return zero, err
	}
	for _, event := range events {
		if oneOf(attachment, event.Attached) {
			manifestAction = event.Attached[0]
		}
	}
	manifestIndex, reservationIndex := -1, -1
	for i, actionID := range source.ActionIntentIDs {
		if actionID == manifestAction {
			manifestIndex = i
		}
		if actionID == reservation.ActionIntentID {
			reservationIndex = i
		}
	}
	if manifestIndex < 0 || reservationIndex <= manifestIndex {
		return zero, fmt.Errorf("reviewed membership must be admitted before original proof reservation")
	}
	if _, pending, _, err := s.readProofResourceCompletionPending(result.ReservationID); err != nil || pending {
		return zero, fmt.Errorf("original proof completion remains pending or unreadable: %v", err)
	}
	if _, pending, _, err := s.readProofResourceLegacyRecoveryPending(source.ID); err != nil || pending {
		return zero, fmt.Errorf("original proof legacy recovery remains pending or unreadable: %v", err)
	}
	snapshot := result.SourceSnapshot
	if snapshot.RepositoryFingerprint != m.RepositoryFingerprint || snapshot.BaseSHA != m.BaseSHA || snapshot.HeadSHA != m.HeadSHA {
		return zero, fmt.Errorf("original proof does not certify exact reviewed repository/base/candidate")
	}
	if historical {
		if !oneOf(ref, w.VerificationIDs) {
			return zero, fmt.Errorf("adoption is not attached to destination")
		}
		if err := s.validateProofAttachment(w, ref); err != nil {
			return zero, err
		}
	}
	return result, nil
}

// latestLandingProofResolution preserves ordered precedence across direct,
// differential and adoption references. A malformed newer reference never falls
// back to an older green result. Origin result identity is never relabelled.
func (s *Store) latestLandingProofResolution(w envelope.WorkEnvelope) (string, ProofResourceResult, bool, error) {
	for i := len(w.VerificationIDs) - 1; i >= 0; i-- {
		ref := strings.TrimSpace(w.VerificationIDs[i])
		if strings.HasPrefix(ref, ProofAdoptionVerificationPrefix) {
			result, err := s.validateProofAdoptionRef(w, ref, true)
			return ref, result, true, err
		}
		if strings.HasPrefix(ref, "proof-check:") || strings.HasPrefix(ref, DifferentialProofVerificationPrefix) {
			exact := w
			exact.VerificationIDs = []string{ref}
			result, found, err := s.latestSourceBoundProofResult(exact)
			return ref, result, found, err
		}
	}
	return "", ProofResourceResult{}, false, nil
}
