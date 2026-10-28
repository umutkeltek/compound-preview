package product

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/policy"
)

// ContentApproval is an owner-written witness, not a generic evidence citation.
// Freezing the authorization inside the winning candidate lets a fresh process
// finish publication of that same approval after an interrupted local write.
type ContentApproval struct {
	ReviewID      string                   `json:"review_id"`
	PackageDigest string                   `json:"package_digest"`
	Authorization ContentPostAuthorization `json:"authorization"`
}

func ContentPackageDigest(homePath string, draft ContentDraft) (string, error) {
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return "", err
	}
	refs := []string{draft.BodyRef}
	for _, m := range draft.Media {
		refs = append(refs, m.Ref)
	}
	for _, ref := range refs {
		body, err := store.Get(ref)
		if err != nil {
			return "", err
		}
		if "artifact:"+act.ContentTransportDigest(body) != ref {
			return "", fmt.Errorf("content artifact %s no longer matches its digest", ref)
		}
	}
	body, err := json.Marshal(struct {
		ProductID string              `json:"product_id"`
		Platform  string              `json:"platform"`
		BodyRef   string              `json:"body_ref"`
		Media     []ContentDraftMedia `json:"media"`
		ExpiresAt string              `json:"expires_at"`
	}{draft.ProductID, draft.Platform, draft.BodyRef, draft.Media, draft.ExpiresAt})
	if err != nil {
		return "", err
	}
	return act.ContentTransportDigest(body), nil
}

func approveContentPackage(homePath string, review ContentQueueReview, row ContentQueueReviewRow, now time.Time) (ContentDraft, error) {
	record, err := candidates.ReadRecord(homePath, candidates.ContentDraft, row.DraftID)
	if err != nil {
		return ContentDraft{}, err
	}
	var draft ContentDraft
	if err := json.Unmarshal(record.Body, &draft); err != nil {
		return draft, err
	}
	digest, err := ContentPackageDigest(homePath, draft)
	if err != nil {
		return draft, err
	}
	if row.PackageDigest == "" || row.PackageDigest != digest {
		return draft, fmt.Errorf("review package changed or digest missing; review exact text, media and alt text again")
	}
	if row.ProductID != "" && row.ProductID != draft.ProductID || row.Platform != "" && row.Platform != draft.Platform {
		return draft, fmt.Errorf("review target differs from draft")
	}
	if draft.Approval != nil {
		if draft.Approval.ReviewID != review.ID || draft.Approval.PackageDigest != digest || draft.State != ContentDraftStateOperationalized || draft.Outbound != nil {
			return draft, fmt.Errorf("draft has a different or consumed approval")
		}
		return draft, nil
	}
	if draft.State != ContentDraftStateProposed && draft.State != ContentDraftStateParked {
		return draft, fmt.Errorf("draft is not reviewable")
	}
	if contentDraftExpired(draft, now) {
		return draft, fmt.Errorf("draft expired before approval")
	}
	ai := contentPostIntent(draft, now)
	ai.ArgumentsHash = digest
	ai.EvidenceRefs = append(ai.EvidenceRefs, digest)
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return draft, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if err := ai.Validate(); err != nil {
		return draft, err
	}
	if err := pd.Validate(); err != nil {
		return draft, err
	}
	auth := ContentPostAuthorization{SchemaVersion: ContentPostAuthorizationSchemaVersion, DraftID: draft.ID, ProductID: draft.ProductID, Platform: draft.Platform, BodyRef: draft.BodyRef, PackageDigest: digest, ReviewID: review.ID, CreatedAt: now.UTC().Format(time.RFC3339), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, PolicyOutcome: pd.Outcome, ActionIntent: ai, PolicyDecision: pd}
	draft.State = ContentDraftStateOperationalized
	draft.AdjudicatedAt = auth.CreatedAt
	draft.Approval = &ContentApproval{ReviewID: review.ID, PackageDigest: digest, Authorization: auth}
	replaced, err := candidates.ReplaceCandidateIfCurrent(homePath, record, draft)
	if err != nil {
		return draft, err
	}
	if !replaced.Replaced {
		return draft, fmt.Errorf("reviewed draft generation changed; approval refused")
	}
	return draft, nil
}

func contentReconcileCommand(draft ContentDraft) string {
	if draft.Outbound == nil {
		return ""
	}
	return "compound act post --draft " + draft.ID + " --transport " + draft.Outbound.Transport + " --reconcile --json"
}

func contentPackageMatches(draft ContentDraft, digest string) bool {
	return draft.Approval != nil && strings.TrimSpace(digest) != "" && draft.Approval.PackageDigest == digest && draft.Approval.Authorization.PackageDigest == digest
}
