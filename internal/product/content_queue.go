package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/verdictflow"
)

// content_queue.go is the distribution-loop batch adjudication surface for
// ContentDrafts: ONE review artifact that renders EVERY reviewable draft
// (proposed + parked, across products) so a weekly review is a single edit-and-
// apply, not one ceremony per post. It mirrors the verdictflow bridge grammar
// (plan -> record -> load -> apply -> annotate) the panel-verdict and outbound
// bridges use, so the operator sees the SAME review shape everywhere.
//
// The apply verdicts are approve | reject | hold, one per draft:
//   - approve mints a governed post ActionIntent + PolicyDecision BOUND to the
//     draft (the S4 `act post` adapter consumes them) and moves the draft to
//     operationalized THROUGH the S1 door (OperationalizeContentDraft), which
//     itself refuses an expired draft. Approve on an expired draft fails closed
//     BEFORE any mint — no ActionIntent/PolicyDecision ever appears.
//   - reject retains the draft with state=rejected (keep-all-data; rejection
//     exhaust trains voice later).
//   - hold leaves the draft byte-identical.
//
// The mints are recorded to the canonical event store (resolvable by id) AND a
// ContentPostAuthorization is written under COMPOUND_HOME/content-post-
// authorizations/<draft_id>.json, so S4 can go draft_id -> authorization ->
// {action_intent, policy_decision, body_ref} and post through internal/act. This
// bridge NEVER posts publicly, authenticates an account, or mutates a product
// signal ledger.

const (
	ContentQueueReviewArea                = "product-content-queue"
	ContentQueuePlanSchemaVersion         = "compound.product_content_queue_plan.v0.2"
	ContentQueueReviewSchemaVersion       = "compound.product_content_queue_review.v0.2"
	ContentQueueReviewReportSchemaVersion = "compound.product_content_queue_review_report.v0.1"
	ContentPostAuthorizationSchemaVersion = "compound.content_post_authorization.v0.2"

	// ContentPostCapability scopes the minted post ActionIntent. A standing grant
	// for this capability + external_side_effect class is what a later S4 post
	// step needs to actually publish; the S3 approval only mints the governed pair.
	ContentPostCapability = "outbound.content_post.publish"

	contentQueueVerdictApprove = "approve"
	contentQueueVerdictReject  = "reject"
	contentQueueVerdictHold    = "hold"
)

// contentQueueVerdicts is the closed set of per-draft decisions.
var contentQueueVerdicts = []string{contentQueueVerdictApprove, contentQueueVerdictReject, contentQueueVerdictHold}

// ContentQueuePlan is the recorded batch surface: one row per reviewable draft,
// each carrying the RESOLVED body text (the operator reads the actual post, not
// a ref) and the prescribed per-draft apply guidance.
type ContentQueuePlan struct {
	SchemaVersion   string                `json:"schema_version"`
	ID              string                `json:"id"`
	CreatedAt       string                `json:"created_at"`
	Status          string                `json:"status"`
	ReviewableCount int                   `json:"reviewable_count"`
	ExpiredCount    int                   `json:"expired_count"`
	Products        []string              `json:"products,omitempty"`
	Rows            []ContentQueueRow     `json:"rows"`
	ReviewDraft     ContentQueuePlanDraft `json:"review_draft"`
	Warnings        []string              `json:"warnings,omitempty"`
}

// ContentQueueRow is one draft as the operator sees it in the batch.
type ContentQueueRow struct {
	PackageDigest string              `json:"package_digest"`
	Media         []ContentDraftMedia `json:"media,omitempty"`
	NextCommand   string              `json:"next_command,omitempty"`
	DraftID       string              `json:"draft_id"`
	ProductID     string              `json:"product_id"`
	Platform      string              `json:"platform"`
	State         string              `json:"state"`
	BodyRef       string              `json:"body_ref"`
	BodyText      string              `json:"body_text"`
	VoicePackRef  string              `json:"voice_pack_ref,omitempty"`
	EvidenceRefs  []string            `json:"evidence_refs,omitempty"`
	ExpiresAt     string              `json:"expires_at"`
	Expired       bool                `json:"expired"`
	ApplyCommands []string            `json:"apply_commands"`
	// Warnings is the additive content-staleness flag set. Slice 2 of
	// the model-catalog envelope adds a check: a draft body naming a
	// model the installed catalog marks superseded surfaces a visible
	// flag here so the operator adjudicating the weekly review sees
	// the staleness signal next to the post text, not behind a
	// separate verb. The flag is INFORMATIONAL — the operator still
	// owns the post; the gate never refuses a draft.
	Warnings []string `json:"warnings,omitempty"`
}

// ContentQueuePlanDraft is the review-skeleton header the annotator surfaces.
type ContentQueuePlanDraft struct {
	ReviewID        string   `json:"review_id"`
	Title           string   `json:"title"`
	AllowedVerdicts []string `json:"allowed_verdicts"`
	ReviewCommand   string   `json:"review_command"`
	ReviewNote      string   `json:"review_note"`
}

// ContentQueuePlanPaths mirrors the shared plan/review/review-yaml path triple.
type ContentQueuePlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

// ContentQueueReview is the edited review artifact: per-draft verdicts.
type ContentQueueReview struct {
	SchemaVersion string                  `json:"schema_version" yaml:"schema_version"`
	ID            string                  `json:"id" yaml:"id"`
	GeneratedAt   string                  `json:"generated_at,omitempty" yaml:"generated_at,omitempty"`
	Drafts        []ContentQueueReviewRow `json:"drafts" yaml:"drafts"`
}

// ContentQueueReviewRow is one draft's operator decision.
type ContentQueueReviewRow struct {
	PackageDigest string `json:"package_digest" yaml:"package_digest"`
	DraftID       string `json:"draft_id" yaml:"draft_id"`
	ProductID     string `json:"product_id,omitempty" yaml:"product_id,omitempty"`
	Platform      string `json:"platform,omitempty" yaml:"platform,omitempty"`
	Expired       bool   `json:"expired,omitempty" yaml:"expired,omitempty"`
	Verdict       string `json:"verdict" yaml:"verdict"`
}

// ContentQueueReviewReport is the apply result: per-row outcomes + counts.
type ContentQueueReviewReport struct {
	SchemaVersion string                   `json:"schema_version"`
	ID            string                   `json:"id"`
	CreatedAt     string                   `json:"created_at"`
	Status        string                   `json:"status"`
	ReviewID      string                   `json:"review_id"`
	Counts        ContentQueueReviewCounts `json:"counts"`
	Rows          []ContentQueueRowResult  `json:"rows"`
	Warnings      []string                 `json:"warnings,omitempty"`
}

// ContentQueueReviewCounts summarizes the batch outcome for legibility.
type ContentQueueReviewCounts struct {
	Approved int `json:"approved"`
	Rejected int `json:"rejected"`
	Held     int `json:"held"`
	Refused  int `json:"refused"`
	Errors   int `json:"errors"`
}

// ContentQueueRowResult is one draft's applied outcome.
type ContentQueueRowResult struct {
	DraftID           string `json:"draft_id"`
	ProductID         string `json:"product_id,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Verdict           string `json:"verdict"`
	Outcome           string `json:"outcome"`
	State             string `json:"state,omitempty"`
	ActionIntentID    string `json:"action_intent_id,omitempty"`
	PolicyDecisionID  string `json:"policy_decision_id,omitempty"`
	PolicyOutcome     string `json:"policy_outcome,omitempty"`
	AuthorizationPath string `json:"authorization_path,omitempty"`
	Error             string `json:"error,omitempty"`
}

// ContentPostAuthorization is the S4-facing record keyed by draft id: it embeds
// the full minted ActionIntent + PolicyDecision plus the body ref, so `act post`
// can reconstruct and govern the actual publish without re-deriving anything.
type ContentPostAuthorization struct {
	PackageDigest    string                  `json:"package_digest"`
	SchemaVersion    string                  `json:"schema_version"`
	DraftID          string                  `json:"draft_id"`
	ProductID        string                  `json:"product_id"`
	Platform         string                  `json:"platform"`
	BodyRef          string                  `json:"body_ref"`
	ReviewID         string                  `json:"review_id"`
	CreatedAt        string                  `json:"created_at"`
	ActionIntentID   string                  `json:"action_intent_id"`
	PolicyDecisionID string                  `json:"policy_decision_id"`
	PolicyOutcome    string                  `json:"policy_outcome"`
	ActionIntent     envelope.ActionIntent   `json:"action_intent"`
	PolicyDecision   envelope.PolicyDecision `json:"policy_decision"`
}

// BuildContentQueuePlan scans every content draft, keeps the reviewable ones
// (proposed + parked), resolves each body's TEXT from the artifact store, marks
// the expired ones, and returns the batch plan. It performs no terminal effect.
func BuildContentQueuePlan(homePath string, now time.Time) (ContentQueuePlan, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentQueuePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	drafts, err := ListContentDrafts(homePath, "")
	if err != nil {
		return ContentQueuePlan{}, err
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return ContentQueuePlan{}, err
	}

	reviewable := 0
	var (
		rows       []ContentQueueRow
		warnings   []string
		expired    int
		ids        []string
		products   = map[string]bool{}
		stalenessN int
	)
	for _, draft := range drafts {
		if draft.State == ContentDraftStatePosted || draft.State == ContentDraftStateRejected || (draft.Outbound != nil && draft.Outbound.Status == "failed") {
			continue
		}
		if draft.Outbound == nil && draft.Approval != nil {
			if auth, loadErr := LoadContentPostAuthorization(homePath, draft.ID); loadErr == nil && auth.ActionIntentID == draft.Approval.Authorization.ActionIntentID {
				continue
			}
		}
		if draft.State != ContentDraftStateProposed && draft.State != ContentDraftStateParked && draft.Outbound == nil && draft.Approval == nil {
			continue // operationalized / rejected drafts are no longer reviewable
		}
		if draft.State == ContentDraftStateProposed || draft.State == ContentDraftStateParked {
			reviewable++
		}
		bodyText, resolveWarn := resolveContentDraftBody(store, draft)
		if resolveWarn != "" {
			warnings = append(warnings, resolveWarn)
		}
		isExpired := contentDraftExpired(draft, now)
		if isExpired {
			expired++
		}
		row := ContentQueueRow{
			Media:         draft.Media,
			NextCommand:   contentReconcileCommand(draft),
			DraftID:       draft.ID,
			ProductID:     draft.ProductID,
			Platform:      draft.Platform,
			State:         draft.State,
			BodyRef:       draft.BodyRef,
			BodyText:      bodyText,
			VoicePackRef:  draft.VoicePackRef,
			EvidenceRefs:  draft.EvidenceRefs,
			ExpiresAt:     draft.ExpiresAt,
			Expired:       isExpired,
			ApplyCommands: contentQueueRowApplyCommands(isExpired),
			// Content-staleness gate: surface the per-draft warnings
			// computed at record time, so the operator adjudicating the
			// weekly review sees the staleness signal next to the post
			// text. Slice 2 of the model-catalog envelope — read-only,
			// no refusal, the operator still owns the post.
			Warnings: draft.Warnings,
		}
		row.PackageDigest, err = ContentPackageDigest(homePath, draft)
		if err != nil {
			return ContentQueuePlan{}, err
		}
		if draft.Outbound != nil {
			row.State = draft.Outbound.Status
			row.ApplyCommands = []string{row.NextCommand}
		} else if draft.Approval != nil {
			row.State = "approved"
			if _, loadErr := LoadContentPostAuthorization(homePath, draft.ID); loadErr != nil {
				row.State = "authorization_recovery_required"
			}
			row.NextCommand = "compound product content-queue apply --review <original-reviewed.yaml>"
			row.ApplyCommands = []string{row.NextCommand + " (resume frozen approval " + draft.Approval.ReviewID + ")"}
		}
		rows = append(rows, row)
		identity := draft.ID + ":" + row.PackageDigest + ":" + row.State
		if draft.Outbound != nil {
			identity += ":" + draft.Outbound.ID
		}
		ids = append(ids, identity)
		products[draft.ProductID] = true
		if len(draft.Warnings) > 0 {
			stalenessN++
		}
	}

	sort.Strings(ids)
	planID := stableID("cqp_", append([]string{"content-queue"}, ids...)...)
	status := "review_required"
	if reviewable == 0 && len(rows) > 0 {
		status = "reconciliation_required"
	}
	if len(rows) == 0 {
		status = "ok"
	}
	plan := ContentQueuePlan{
		SchemaVersion:   ContentQueuePlanSchemaVersion,
		ID:              planID,
		CreatedAt:       now.UTC().Format(time.RFC3339),
		Status:          status,
		ReviewableCount: reviewable,
		ExpiredCount:    expired,
		Products:        sortedKeys(products),
		Rows:            rows,
		ReviewDraft: ContentQueuePlanDraft{
			ReviewID:        stableID("content_queue_review_", planID),
			Title:           fmt.Sprintf("Content queue: %d draft(s) to review", len(rows)),
			AllowedVerdicts: append([]string{}, contentQueueVerdicts...),
			ReviewCommand:   "compound product content-queue apply --review <review.yaml>",
			ReviewNote:      "Approve mints a governed post ActionIntent+PolicyDecision and operationalizes the draft; it never posts publicly by itself. Expired drafts are not approvable; rejected drafts are retained.",
		},
		Warnings: warnings,
	}
	// Surface a plan-level staleness summary so the operator sees the
	// count at a glance (each per-row warning remains the per-draft
	// detail). The slice-2 gate is INFORMATIONAL — never refuses, never
	// overrides the queue's own status; the operator still owns the post.
	if stalenessN > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d draft(s) name models the installed catalog marks superseded — see per-row warnings for which model", stalenessN))
	}
	return plan, nil
}

// RecordContentQueuePlan writes the json+markdown core pair and the review-yaml
// skeleton through the shared verdictflow scaffolding.
func RecordContentQueuePlan(homePath string, plan ContentQueuePlan) (ContentQueuePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return ContentQueuePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return ContentQueuePlanPaths{}, fmt.Errorf("content queue plan id required")
	}
	arts, err := verdictflow.RecordPlan(homePath, ContentQueueReviewArea, plan.ID, " ", plan,
		renderContentQueuePlanMarkdown(plan), RenderContentQueueReviewYAML(plan))
	if err != nil {
		return ContentQueuePlanPaths{}, err
	}
	return ContentQueuePlanPaths{
		PlanPath:       arts.PlanPath,
		ReviewPath:     arts.ReviewPath,
		ReviewYAMLPath: arts.ReviewYAMLPath,
	}, nil
}

// RenderContentQueueReviewYAML renders the editable per-draft verdict skeleton.
// A fresh draft carries a TODO verdict; an expired draft is pre-set to hold and
// marked expired (it is not approvable).
func RenderContentQueueReviewYAML(plan ContentQueuePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ContentQueueReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "generated_at: %s\n", plan.CreatedAt)
	fmt.Fprintln(&b, "# Set each draft's verdict to approve | reject | hold.")
	fmt.Fprintln(&b, "# EXPIRED drafts are NOT approvable (stale marketing must never post).")
	fmt.Fprintln(&b, "drafts:")
	for _, row := range plan.Rows {
		fmt.Fprintf(&b, "  - draft_id: %s\n", row.DraftID)
		fmt.Fprintf(&b, "    package_digest: %q\n", row.PackageDigest)
		fmt.Fprintf(&b, "    product_id: %q\n", row.ProductID)
		fmt.Fprintf(&b, "    platform: %s\n", row.Platform)
		if row.NextCommand != "" {
			fmt.Fprintln(&b, "    verdict: hold # existing approval/attempt; use the command below")
			fmt.Fprintf(&b, "    # %s\n", row.NextCommand)
		} else if row.Expired {
			fmt.Fprintln(&b, "    expired: true")
			fmt.Fprintln(&b, "    verdict: hold  # EXPIRED: reject or hold only")
		} else {
			fmt.Fprintln(&b, "    verdict: TODO_verdict_approve_reject_hold")
		}
	}
	return b.String()
}

// LoadContentQueueReview reads the edited review YAML.
func LoadContentQueueReview(path string) (ContentQueueReview, error) {
	return verdictflow.LoadReview[ContentQueueReview](path)
}

// ValidateContentQueueReview checks the schema and that no row carries a junk
// verdict (a still-TODO or empty verdict is treated as an undecided hold, not a
// hard error, so a partly-reviewed batch does not block the decided rows).
func ValidateContentQueueReview(review ContentQueueReview) error {
	if review.SchemaVersion != ContentQueueReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ContentQueueReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" {
		return fmt.Errorf("content queue review id required")
	}
	for _, row := range review.Drafts {
		if strings.TrimSpace(row.DraftID) == "" {
			return fmt.Errorf("content queue review row missing draft_id")
		}
		if v := normalizeContentQueueVerdict(row.Verdict); v == "" && !isUndecidedVerdict(row.Verdict) {
			return fmt.Errorf("content queue review draft %s has unknown verdict %q (want approve|reject|hold)", row.DraftID, row.Verdict)
		}
	}
	return nil
}

// ApplyContentQueueReview executes the per-draft verdicts. Each row is applied
// independently so an expired-approve refusal never blocks the decided rows; the
// refused draft stays byte-identical and no ActionIntent/PolicyDecision is minted.
func ApplyContentQueueReview(homePath string, review ContentQueueReview, sourcePath string, now time.Time) (ContentQueueReviewReport, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentQueueReviewReport{Status: "fail"}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := ValidateContentQueueReview(review); err != nil {
		return ContentQueueReviewReport{Status: "fail", ReviewID: review.ID}, err
	}
	report := ContentQueueReviewReport{
		SchemaVersion: ContentQueueReviewReportSchemaVersion,
		ID:            stableID("cqrr_", review.ID, strconv.FormatInt(now.UTC().UnixNano(), 10)),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		ReviewID:      review.ID,
		Warnings: []string{
			"does not post publicly or authenticate any account",
			"does not mutate a product signal ledger",
			"approval mints a governed post ActionIntent+PolicyDecision for the S4 act post adapter",
		},
	}
	for _, row := range review.Drafts {
		res := applyContentQueueRow(homePath, review, row, now)
		report.Rows = append(report.Rows, res)
		switch res.Outcome {
		case "approved":
			report.Counts.Approved++
		case "rejected":
			report.Counts.Rejected++
		case "held", "held_undecided":
			report.Counts.Held++
		case "refused_expired", "refused", "skipped_not_reviewable":
			report.Counts.Refused++
		default:
			report.Counts.Errors++
		}
	}
	if report.Counts.Errors > 0 {
		report.Status = "recorded_with_errors"
	}

	// Best-effort reviewed marker + plan status flip, so a reviewed batch drops
	// off the workbench needs-review surface. Never fails the apply.
	if stem := reviewArtifactStem(sourcePath); stem != "" {
		if plan, err := loadContentQueuePlan(homePath, stem); err == nil {
			_ = recordContentQueueReviewResult(homePath, stem, plan, report)
		}
	}
	return report, nil
}

// applyContentQueueRow applies one verdict. On approve it fails closed on expiry
// BEFORE any mint, operationalizes through the S1 door, then mints + records the
// governed post pair.
func applyContentQueueRow(homePath string, review ContentQueueReview, row ContentQueueReviewRow, now time.Time) ContentQueueRowResult {
	draftID := strings.TrimSpace(row.DraftID)
	verdict := normalizeContentQueueVerdict(row.Verdict)
	res := ContentQueueRowResult{DraftID: draftID, Verdict: firstNonEmpty(verdict, strings.TrimSpace(row.Verdict))}

	draft, err := ReadContentDraft(homePath, draftID)
	if err != nil {
		res.Outcome = "error"
		res.Error = err.Error()
		return res
	}
	res.ProductID = draft.ProductID
	res.Platform = draft.Platform
	res.State = draft.State

	switch verdict {
	case contentQueueVerdictReject:
		rejected, err := RejectContentDraft(homePath, draftID, now)
		if err != nil {
			res.Outcome = "error"
			res.Error = err.Error()
			return res
		}
		res.Outcome = "rejected"
		res.State = rejected.State
		return res

	case contentQueueVerdictHold:
		res.Outcome = "held" // draft left byte-identical
		return res

	case contentQueueVerdictApprove:
		if draft.State != ContentDraftStateProposed && draft.State != ContentDraftStateParked && draft.Approval == nil {
			res.Outcome = "skipped_not_reviewable"
			res.Error = fmt.Sprintf("draft state %q is not reviewable; no re-mint", draft.State)
			return res
		}
		// Fail closed on expiry BEFORE any mint or state move.
		if draft.Approval == nil && contentDraftExpired(draft, now) {
			res.Outcome = "refused_expired"
			res.Error = fmt.Sprintf("draft %s expired at %s: stale marketing must never post", draft.ID, draft.ExpiresAt)
			return res
		}
		// Move state THROUGH the S1 door (never bypassed); it re-checks expiry.
		op, err := approveContentPackage(homePath, review, row, now)
		if err != nil {
			res.Outcome = "refused"
			res.Error = err.Error()
			return res
		}
		res.State = op.State
		auth, err := mintAndRecordContentPost(homePath, review, op, now)
		if err != nil {
			res.Outcome = "error"
			res.Error = err.Error()
			return res
		}
		res.Outcome = "approved"
		res.ActionIntentID = auth.ActionIntentID
		res.PolicyDecisionID = auth.PolicyDecisionID
		res.PolicyOutcome = auth.PolicyOutcome
		res.AuthorizationPath = contentPostAuthorizationPath(homePath, draftID)
		return res

	default:
		res.Outcome = "held_undecided" // TODO/empty verdict: no change
		return res
	}
}

// mintAndRecordContentPost mints the governed post ActionIntent + PolicyDecision
// for an approved (already-operationalized) draft, records both to the canonical
// event store, writes the S4-facing authorization record, and records the loop
// transition. Returns the authorization (with the two minted ids).
func mintAndRecordContentPost(homePath string, review ContentQueueReview, draft ContentDraft, now time.Time) (ContentPostAuthorization, error) {
	if draft.Approval == nil {
		return ContentPostAuthorization{}, fmt.Errorf("missing frozen package approval")
	}
	auth := draft.Approval.Authorization
	ai, pd := auth.ActionIntent, auth.PolicyDecision
	var err error
	now, err = time.Parse(time.RFC3339, auth.CreatedAt)
	if err != nil {
		return ContentPostAuthorization{}, err
	}

	store, err := eventstore.Open(homePath)
	if err != nil {
		return ContentPostAuthorization{}, err
	}
	if _, err := store.PutJSON(ai.ID, &ai, " "); err != nil {
		return ContentPostAuthorization{}, fmt.Errorf("record content post action intent: %w", err)
	}
	if _, err := store.PutJSON(pd.ID, &pd, " "); err != nil {
		return ContentPostAuthorization{}, fmt.Errorf("record content post policy decision: %w", err)
	}

	if err := writeContentPostAuthorization(homePath, auth); err != nil {
		return ContentPostAuthorization{}, err
	}

	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "content_post_authorization",
		ProductID:    draft.ProductID,
		ObjectID:     draft.ID,
		Actor:        "operator",
		SourceTrace:  "content_queue_review:" + review.ID,
		BeforeRefs:   []string{"content_draft:" + draft.ID},
		AfterRefs:    []string{ai.ID, pd.ID, "content_post_authorization:" + draft.ID},
		EvidenceRefs: compact([]string{draft.BodyRef, ai.ID, pd.ID}),
		Summary:      fmt.Sprintf("content draft %s approved and operationalized; minted %s + %s for the S4 act post adapter (no public post)", draft.ID, ai.ID, pd.ID),
	}, now)
	if err != nil {
		return ContentPostAuthorization{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return ContentPostAuthorization{}, err
	}
	return auth, nil
}

// contentPostIntent builds the external_side_effect / easy / local ActionIntent
// for one approved draft, mirroring the governed act-repoint intent shape. It
// carries the body ref in evidence so the S4 post adapter resolves the exact
// bytes, and a rollback_plan (required by the act gate for outbound writes).
func contentPostIntent(draft ContentDraft, now time.Time) envelope.ActionIntent {
	workID := stableID("work_content_", draft.ProductID, draft.ID)
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            stableID("act_", ContentPostCapability, draft.ProductID, draft.ID, draft.BodyRef, draft.Platform),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Scope:         envelope.Scope{ProductID: draft.ProductID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  compact(append([]string{draft.BodyRef}, draft.EvidenceRefs...)),
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    ContentPostCapability,
		Action:        "publish_content_post",
		Target:        envelope.ActionTarget{Kind: "content_post", ID: draft.ID},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("publish approved content draft %s to %s for %s", draft.ID, draft.Platform, draft.ProductID),
		RollbackPlan:  fmt.Sprintf("remove the staged outbound post for draft %s (restore no-post state)", draft.ID),
	}
}

// LoadContentPostAuthorization is the S4 entry point: draft_id -> the minted
// ActionIntent + PolicyDecision + body ref bound to that draft.
func LoadContentPostAuthorization(homePath, draftID string) (ContentPostAuthorization, error) {
	data, err := os.ReadFile(contentPostAuthorizationPath(homePath, draftID))
	if err != nil {
		return ContentPostAuthorization{}, err
	}
	var auth ContentPostAuthorization
	if err := json.Unmarshal(data, &auth); err != nil {
		return ContentPostAuthorization{}, err
	}
	if auth.SchemaVersion != ContentPostAuthorizationSchemaVersion {
		return auth, fmt.Errorf("not a content post authorization: schema_version=%q", auth.SchemaVersion)
	}
	return auth, nil
}

func writeContentPostAuthorization(homePath string, auth ContentPostAuthorization) error {
	dir := filepath.Join(homePath, "content-post-authorizations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(auth, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(contentPostAuthorizationPath(homePath, auth.DraftID), append(data, '\n'), 0o644)
}

func contentPostAuthorizationPath(homePath, draftID string) string {
	return filepath.Join(homePath, "content-post-authorizations", draftID+".json")
}

func loadContentQueuePlan(homePath, stem string) (ContentQueuePlan, error) {
	path := filepath.Join(homePath, "reviews", ContentQueueReviewArea, stem+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ContentQueuePlan{}, err
	}
	var plan ContentQueuePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ContentQueuePlan{}, err
	}
	if strings.TrimSpace(plan.ID) == "" {
		return ContentQueuePlan{}, fmt.Errorf("content queue plan %s missing id", path)
	}
	return plan, nil
}

func recordContentQueueReviewResult(homePath, stem string, plan ContentQueuePlan, report ContentQueueReviewReport) error {
	if _, err := reviewartifact.WriteJSON(homePath, ContentQueueReviewArea, stem+"."+report.ID+".reviewed.json", report, " "); err != nil {
		return err
	}
	plan.Status = "recorded"
	path := filepath.Join(homePath, "reviews", ContentQueueReviewArea, stem+".json")
	data, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// AnnotateContentQueue plugs the content-queue area into the canonical review
// queue builder so `compound workbench`/reviews surface content-queue pressure.
func AnnotateContentQueue(report *reviewartifact.QueueReport) {
	verdictflow.AnnotateQueue(report, ContentQueueReviewArea, annotateContentQueueItem)
}

func annotateContentQueueItem(item *reviewartifact.QueueItem) {
	plan, ok := contentQueuePlanFromQueueItem(*item)
	if !ok {
		item.Readiness = "blocked"
		item.BlockedReason = "content queue plan unreadable"
		item.NextAction = "regenerate content queue plan"
		item.Actions = nil
		return
	}
	var pending []string
	for _, row := range plan.Rows {
		if row.NextCommand != "" {
			pending = append(pending, row.NextCommand)
		}
	}
	if len(pending) > 0 {
		item.Readiness = "needs_review"
		item.BlockedReason = "existing content approval or outbound attempt requires recovery"
		item.NextAction = strings.Join(pending, "\n")
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	if record, ok := contentQueueReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "content queue reviewed"
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	if plan.ReviewableCount == 0 {
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "no content drafts awaiting review"
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	reviewPath := contentQueueReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "content queue review YAML draft missing"
		item.NextAction = "regenerate content queue plan"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = fmt.Sprintf("operator must set per-draft verdicts for %d content draft(s)", plan.ReviewableCount)
	item.NextAction = "edit content queue review YAML verdicts then apply"
	item.DecisionCard = contentQueueDecisionCard(plan, reviewPath)
	argv := []string{"compound", "product", "content-queue", "apply", "--review", reviewPath}
	item.Actions = []reviewartifact.Action{{
		ID:             "apply_content_queue_review",
		Label:          "Apply content queue verdicts",
		Command:        shellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	seedContentQueueReviewAuthority(item, plan, reviewPath)
}

func seedContentQueueReviewAuthority(item *reviewartifact.QueueItem, plan ContentQueuePlan, reviewPath string) {
	if item == nil || len(item.Actions) != 1 {
		return
	}
	workID := reviewartifact.AuthoritySeedWorkID(*item, "")
	if workID == "" {
		return
	}
	evidenceRefs := []string{"content-queue-plan:" + plan.ID}
	for _, row := range plan.Rows {
		evidenceRefs = append(evidenceRefs, row.EvidenceRefs...)
		evidenceRefs = append(evidenceRefs, "content-draft:"+row.DraftID)
	}
	evidenceRefs = reviewartifact.UniqueSorted(evidenceRefs)
	if err := reviewartifact.SeedActionAuthority(&item.Actions[0], reviewartifact.ActionAuthoritySeedSpec{
		WorkID: workID, CreatedAt: plan.CreatedAt, ProducerName: "compound/internal/product",
		Capability: "product.content_queue.review", Action: "apply_content_queue_verdicts",
		Target:        envelope.ActionTarget{Kind: "content_queue_plan", ID: plan.ID, Path: reviewPath},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
		Preview:      "Apply reviewed approve/reject/hold verdicts for " + strconv.Itoa(plan.ReviewableCount) + " content drafts; approval may mint publication authorization but never posts publicly.",
		RollbackPlan: "Record superseding per-draft verdicts and retire any unconsumed publication authorization before outbound execution.",
		Privacy:      "private", Risk: "tier1", EvidenceRefs: evidenceRefs, TargetScope: "private",
		ExpectedReceipt: "Applied reviewed content queue verdicts for plan " + plan.ID + " without public posting.",
		ExpectedProof: append([]string{
			"content-queue-plan:" + plan.ID,
			"content-queue-review:" + reviewPath,
		}, evidenceRefs...),
	}); err != nil {
		item.Warnings = reviewartifact.Unique(append(item.Warnings, "content queue authority seed blocked: "+err.Error()))
	}
}

func contentQueueDecisionCard(plan ContentQueuePlan, reviewPath string) *reviewartifact.DecisionCard {
	argv := []string{"compound", "product", "content-queue", "apply", "--review", reviewPath}
	boundary := []string{
		"approval mints an ActionIntent + PolicyDecision; it does not post publicly by itself",
		"expired drafts are not approvable (stale marketing must never post)",
		"rejected drafts are retained (keep-all-data), never deleted",
	}
	return &reviewartifact.DecisionCard{
		Claim:               fmt.Sprintf("Review %d content draft(s) for distribution (%d expired)", plan.ReviewableCount, plan.ExpiredCount),
		PanelLean:           "approve only on-voice, unexpired drafts; reject off-voice; hold if unsure",
		CanonicalActionHint: "Set each draft's verdict (approve|reject|hold) in the review YAML, then run `compound product content-queue apply --review <review.yaml>`. Approve mints a governed post ActionIntent+PolicyDecision and operationalizes the draft; it never posts publicly by itself.",
		Boundary:            boundary,
		Meta: map[string]string{
			"review_yaml": reviewPath,
			"reviewable":  strconv.Itoa(plan.ReviewableCount),
			"expired":     strconv.Itoa(plan.ExpiredCount),
		},
		Why: []reviewartifact.DecisionCardReason{
			{Label: "drafts", Question: "How many drafts await a verdict?", Detail: fmt.Sprintf("%d reviewable, %d expired", plan.ReviewableCount, plan.ExpiredCount)},
			{Label: "products", Question: "Which products?", Detail: strings.Join(plan.Products, ", ")},
			{Label: "boundary", Severity: "warning", Question: "What must stay outside this bridge?", Detail: strings.Join(boundary, "; ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: contentQueueVerdictApprove, Label: "Approve", Effect: "Mints a governed post ActionIntent+PolicyDecision and operationalizes the draft (no public post).", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: contentQueueVerdictReject, Label: "Reject", Effect: "Retains the draft with state=rejected (keep-all-data).", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: contentQueueVerdictHold, Label: "Hold", Effect: "Leaves the draft unchanged for a later review.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
		},
	}
}

func contentQueuePlanFromQueueItem(item reviewartifact.QueueItem) (ContentQueuePlan, bool) {
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return ContentQueuePlan{}, false
	}
	var plan ContentQueuePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ContentQueuePlan{}, false
	}
	return plan, plan.ID != "" && plan.SchemaVersion == ContentQueuePlanSchemaVersion
}

func contentQueueReviewedFromExtras(paths []string) (ContentQueueReviewReport, bool) {
	var found ContentQueueReviewReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record ContentQueueReviewReport
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Status) == "" {
			continue
		}
		if found.ID == "" || record.CreatedAt > found.CreatedAt {
			found = record
		}
	}
	return found, found.ID != ""
}

func contentQueueReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

// --- small helpers ---

// resolveContentDraftBody reads the draft's body TEXT from the artifact store so
// the operator reviews the actual post, not just a ref. A missing/corrupt body
// yields an empty text and a warning rather than failing the whole batch.
func resolveContentDraftBody(store *home.ArtifactStore, draft ContentDraft) (string, string) {
	if strings.TrimSpace(draft.BodyRef) == "" {
		return "", fmt.Sprintf("draft %s has no body ref", draft.ID)
	}
	body, err := store.Get(draft.BodyRef)
	if err != nil {
		return "", fmt.Sprintf("draft %s body %s unreadable: %v", draft.ID, draft.BodyRef, err)
	}
	return string(body), ""
}

// contentDraftExpired reports whether the draft's expiry is at or before now. An
// unparseable expiry is treated as expired (fail closed): stale marketing must
// never post.
func contentDraftExpired(draft ContentDraft, now time.Time) bool {
	expiresAt, err := time.Parse(time.RFC3339, draft.ExpiresAt)
	if err != nil {
		return true
	}
	return !now.Before(expiresAt)
}

func contentQueueRowApplyCommands(expired bool) []string {
	apply := "compound product content-queue apply --review <review.yaml>"
	if expired {
		return []string{
			"approve -> REFUSED: draft expired, not approvable",
			"reject  -> set verdict: reject in the review YAML, then run: " + apply,
			"hold    -> leave verdict: hold (default for expired)",
		}
	}
	return []string{
		"approve -> set verdict: approve, then run: " + apply + "  (mints ActionIntent+PolicyDecision, operationalizes)",
		"reject  -> set verdict: reject  (retained, keep-all-data)",
		"hold    -> set verdict: hold    (no change)",
	}
}

func renderContentQueuePlanMarkdown(plan ContentQueuePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Content Queue: %d draft(s) to review\n\n", plan.ReviewableCount)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- reviewable: %d\n", plan.ReviewableCount)
	fmt.Fprintf(&b, "- expired: %d\n", plan.ExpiredCount)
	if len(plan.Products) > 0 {
		fmt.Fprintf(&b, "- products: %s\n", strings.Join(plan.Products, ", "))
	}
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Drafts")
	fmt.Fprintln(&b)
	for _, row := range plan.Rows {
		marker := ""
		if row.Expired {
			marker = " [EXPIRED]"
		}
		fmt.Fprintf(&b, "### %s (%s / %s)%s\n\n", row.DraftID, row.ProductID, row.Platform, marker)
		fmt.Fprintf(&b, "- expires_at: %s\n", row.ExpiresAt)
		fmt.Fprintf(&b, "- package_digest: %s\n", row.PackageDigest)
		for i, media := range row.Media {
			fmt.Fprintf(&b, "- media %d: %s (%s, %s); alt text: %q\n", i+1, media.Ref, media.Kind, media.MIMEType, media.AltText)
		}
		if row.NextCommand != "" {
			fmt.Fprintf(&b, "- %s: `%s`\n", row.State, row.NextCommand)
		}
		if row.VoicePackRef != "" {
			fmt.Fprintf(&b, "- voice_pack: %s\n", row.VoicePackRef)
		}
		if len(row.EvidenceRefs) > 0 {
			fmt.Fprintf(&b, "- evidence: %s\n", strings.Join(row.EvidenceRefs, ", "))
		}
		fmt.Fprintln(&b, "- body:")
		for _, line := range strings.Split(row.BodyText, "\n") {
			fmt.Fprintf(&b, "  > %s\n", line)
		}
		fmt.Fprintln(&b, "- prescribed:")
		for _, cmd := range row.ApplyCommands {
			fmt.Fprintf(&b, "  - %s\n", cmd)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func normalizeContentQueueVerdict(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if oneOf(v, contentQueueVerdicts) {
		return v
	}
	return ""
}

// isUndecidedVerdict reports whether a verdict is an unset/TODO placeholder (an
// operator has not decided this row yet), distinct from a junk value.
func isUndecidedVerdict(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.HasPrefix(strings.ToUpper(v), "TODO")
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
