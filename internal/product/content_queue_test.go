package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/eventstore"
	"compound/internal/home"
)

var contentQueueNow = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func recordQueueDraft(t *testing.T, homePath, body string, expiresAt, now time.Time) ContentDraft {
	t.Helper()
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         body,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    expiresAt,
	}, now)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	return draft
}

func approveReview(t *testing.T, homePath, id, draftID string) ContentQueueReview {
	t.Helper()
	draft, err := ReadContentDraft(homePath, draftID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ContentPackageDigest(homePath, draft)
	if err != nil {
		t.Fatal(err)
	}
	return ContentQueueReview{
		SchemaVersion: ContentQueueReviewSchemaVersion,
		ID:            id,
		Drafts:        []ContentQueueReviewRow{{DraftID: draftID, PackageDigest: digest, Verdict: "approve"}},
	}
}

func draftJSONPath(homePath, id string) string {
	return filepath.Join(homePath, candidates.InboxDir(candidates.ContentDraft), id+".json")
}

// RED if the plan carries only the body ref: the operator would review a hash,
// not the post. Proves the batch resolves the actual body TEXT from the store.
func TestContentQueueBuildResolvesBodyTextFromArtifactStore(t *testing.T) {
	homePath := t.TempDir()
	body := "launch-day thread for prod_synth_a — line 1\nline 2"
	draft := recordQueueDraft(t, homePath, body, contentQueueNow.Add(72*time.Hour), contentQueueNow)

	plan, err := BuildContentQueuePlan(homePath, contentQueueNow)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.ReviewableCount != 1 || len(plan.Rows) != 1 {
		t.Fatalf("want 1 reviewable row, got %d: %+v", plan.ReviewableCount, plan.Rows)
	}
	row := plan.Rows[0]
	if row.DraftID != draft.ID {
		t.Fatalf("row draft id = %q, want %q", row.DraftID, draft.ID)
	}
	if row.BodyText != body {
		t.Fatalf("row body text = %q, want the resolved artifact body %q", row.BodyText, body)
	}
	if row.BodyRef != draft.BodyRef {
		t.Fatalf("row body ref = %q, want %q", row.BodyRef, draft.BodyRef)
	}
	if row.Expired {
		t.Fatalf("fresh draft marked expired: %+v", row)
	}
}

// RED if approve fails to mint the governed pair, or does not operationalize the
// draft, or S4 cannot find the mints from the draft id. This is the core S3->S4
// contract: one ActionIntent + one PolicyDecision, bound to and findable by the draft.
func TestContentQueueApproveMintsGovernedPostPairAndOperationalizes(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentQueueNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	draft := recordQueueDraft(t, homePath, "ship-day post", contentQueueNow.Add(72*time.Hour), contentQueueNow)

	report, err := ApplyContentQueueReview(homePath, approveReview(t, homePath, "cqr_approve", draft.ID), "", contentQueueNow)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Counts.Approved != 1 || len(report.Rows) != 1 || report.Rows[0].Outcome != "approved" {
		t.Fatalf("expected one approved row, got %+v", report)
	}

	// Draft ended operationalized (through the S1 door).
	after, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("re-read draft: %v", err)
	}
	if after.State != ContentDraftStateOperationalized {
		t.Fatalf("draft state = %q, want operationalized", after.State)
	}

	// Exactly one authorization file, embedding exactly one AI + one PD.
	authFiles, err := filepath.Glob(filepath.Join(homePath, "content-post-authorizations", "*.json"))
	if err != nil || len(authFiles) != 1 {
		t.Fatalf("want exactly one content-post authorization, got %v (err %v)", authFiles, err)
	}

	// S4 finds the mints from the draft id.
	auth, err := LoadContentPostAuthorization(homePath, draft.ID)
	if err != nil {
		t.Fatalf("S4 load authorization by draft id: %v", err)
	}
	if !strings.HasPrefix(auth.ActionIntentID, "act_") || !strings.HasPrefix(auth.PolicyDecisionID, "pol_") {
		t.Fatalf("bad minted ids: ai=%q pd=%q", auth.ActionIntentID, auth.PolicyDecisionID)
	}
	if err := auth.ActionIntent.Validate(); err != nil {
		t.Fatalf("minted action intent invalid: %v", err)
	}
	if err := auth.PolicyDecision.Validate(); err != nil {
		t.Fatalf("minted policy decision invalid: %v", err)
	}
	if auth.ActionIntent.Target.Kind != "content_post" || auth.ActionIntent.Target.ID != draft.ID {
		t.Fatalf("action intent target = %+v, want content_post/%s", auth.ActionIntent.Target, draft.ID)
	}
	if auth.PolicyDecision.ActionIntentID != auth.ActionIntentID {
		t.Fatalf("policy decision authorizes %q, not the minted intent %q", auth.PolicyDecision.ActionIntentID, auth.ActionIntentID)
	}
	if auth.BodyRef != draft.BodyRef {
		t.Fatalf("authorization body ref = %q, want %q (S4 resolves the post bytes)", auth.BodyRef, draft.BodyRef)
	}
	if report.Rows[0].ActionIntentID != auth.ActionIntentID || report.Rows[0].PolicyDecisionID != auth.PolicyDecisionID {
		t.Fatalf("report row ids %+v do not match authorization %s/%s", report.Rows[0], auth.ActionIntentID, auth.PolicyDecisionID)
	}

	// Mints are resolvable through the canonical event store.
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	if _, err := store.Read(auth.ActionIntentID); err != nil {
		t.Fatalf("minted action intent not resolvable in event store: %v", err)
	}
	if _, err := store.Read(auth.PolicyDecisionID); err != nil {
		t.Fatalf("minted policy decision not resolvable in event store: %v", err)
	}
}

// RED if approve on an expired draft mints anything or moves the draft. The mint
// must be strictly downstream of the expiry gate: no ActionIntent/PolicyDecision
// file may appear, and the draft must stay byte-identical.
func TestContentQueueApproveOnExpiredRefusesBeforeAnyMint(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentQueueNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	// Expiry AT contentQueueNow => expired one hour later.
	draft := recordQueueDraft(t, homePath, "stale ship-day post", contentQueueNow, contentQueueNow)
	before, err := os.ReadFile(draftJSONPath(homePath, draft.ID))
	if err != nil {
		t.Fatalf("read draft file: %v", err)
	}

	report, err := ApplyContentQueueReview(homePath, approveReview(t, homePath, "cqr_expired", draft.ID), "", contentQueueNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Counts.Refused != 1 || report.Counts.Approved != 0 || report.Rows[0].Outcome != "refused_expired" {
		t.Fatalf("expected refused_expired, got %+v", report)
	}

	// No mint file of any kind appeared.
	if authFiles, _ := filepath.Glob(filepath.Join(homePath, "content-post-authorizations", "*.json")); len(authFiles) != 0 {
		t.Fatalf("expired approve leaked authorization/mint files: %v", authFiles)
	}
	if _, err := LoadContentPostAuthorization(homePath, draft.ID); !os.IsNotExist(err) {
		t.Fatalf("expired approve must not write an authorization; got err %v", err)
	}

	// Draft byte-identical (still proposed, untouched).
	after, err := os.ReadFile(draftJSONPath(homePath, draft.ID))
	if err != nil {
		t.Fatalf("re-read draft file: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("expired-approve mutated the draft:\nbefore=%s\nafter=%s", before, after)
	}
}

// RED if reject drops the record: rejection is retained exhaust (keep-all-data).
func TestContentQueueRejectRetainsRejectedDraft(t *testing.T) {
	homePath := t.TempDir()
	draft := recordQueueDraft(t, homePath, "off-voice draft", contentQueueNow.Add(72*time.Hour), contentQueueNow)

	review := ContentQueueReview{
		SchemaVersion: ContentQueueReviewSchemaVersion,
		ID:            "cqr_reject",
		Drafts:        []ContentQueueReviewRow{{DraftID: draft.ID, Verdict: "reject"}},
	}
	report, err := ApplyContentQueueReview(homePath, review, "", contentQueueNow)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Counts.Rejected != 1 || report.Rows[0].Outcome != "rejected" {
		t.Fatalf("expected one rejected row, got %+v", report)
	}
	after, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("rejected draft must remain readable: %v", err)
	}
	if after.State != ContentDraftStateRejected {
		t.Fatalf("draft state = %q, want rejected", after.State)
	}
}

// RED if hold has any side effect on the draft record.
func TestContentQueueHoldLeavesDraftByteIdentical(t *testing.T) {
	homePath := t.TempDir()
	draft := recordQueueDraft(t, homePath, "undecided draft", contentQueueNow.Add(72*time.Hour), contentQueueNow)
	before, err := os.ReadFile(draftJSONPath(homePath, draft.ID))
	if err != nil {
		t.Fatalf("read draft file: %v", err)
	}

	review := ContentQueueReview{
		SchemaVersion: ContentQueueReviewSchemaVersion,
		ID:            "cqr_hold",
		Drafts:        []ContentQueueReviewRow{{DraftID: draft.ID, Verdict: "hold"}},
	}
	report, err := ApplyContentQueueReview(homePath, review, "", contentQueueNow)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Counts.Held != 1 || report.Rows[0].Outcome != "held" {
		t.Fatalf("expected one held row, got %+v", report)
	}
	after, err := os.ReadFile(draftJSONPath(homePath, draft.ID))
	if err != nil {
		t.Fatalf("re-read draft file: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("hold mutated the draft:\nbefore=%s\nafter=%s", before, after)
	}
}
