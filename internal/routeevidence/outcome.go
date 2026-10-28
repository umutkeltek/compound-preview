package routeevidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/reviewartifact"
	"compound/internal/writeback"
)

const RouteOutcomeReviewSchemaVersion = "compound.route_outcome_review.v0.1"
const RouteOutcomeCandidateSchemaVersion = "compound.route_outcome_observation.v0.1"

type RouteOutcomeReview struct {
	SchemaVersion           string                 `json:"schema_version"`
	ID                      string                 `json:"id"`
	CreatedAt               string                 `json:"created_at"`
	Status                  string                 `json:"status"`
	RouteEvidenceID         string                 `json:"route_evidence_id"`
	RequestID               string                 `json:"request_id"`
	WorkID                  string                 `json:"work_id,omitempty"`
	ReceiptID               string                 `json:"receipt_id"`
	ReceiptOutcome          string                 `json:"receipt_outcome"`
	ReceiptMoveType         string                 `json:"receipt_move_type,omitempty"`
	ReceiptStateChanged     bool                   `json:"receipt_state_changed"`
	RouteStatus             string                 `json:"route_status"`
	SelectedTarget          Target                 `json:"selected_target"`
	CapabilityClasses       []string               `json:"capability_classes,omitempty"`
	Usage                   Usage                  `json:"usage,omitempty"`
	CostMicros              int64                  `json:"cost_micros,omitempty"`
	LatencyMS               int64                  `json:"latency_ms,omitempty"`
	EvidenceRefs            []string               `json:"evidence_refs"`
	TargetPath              string                 `json:"target_path,omitempty"`
	TargetExists            bool                   `json:"target_exists"`
	TargetHash              string                 `json:"target_hash,omitempty"`
	Assessment              RouteOutcomeAssessment `json:"assessment"`
	ReviewDraft             RouteOutcomeDraft      `json:"review_draft"`
	CandidateID             string                 `json:"candidate_id,omitempty"`
	NoPromoteID             string                 `json:"no_promote_id,omitempty"`
	RecordedCandidatePath   string                 `json:"recorded_candidate_path,omitempty"`
	RecordedNoPromote       bool                   `json:"recorded_no_promote,omitempty"`
	RecordNoPromoteInserted bool                   `json:"record_no_promote_inserted,omitempty"`
	Warnings                []string               `json:"warnings,omitempty"`
}

type RouteOutcomeAssessment struct {
	Class              string `json:"class"`
	ShouldReviewPolicy bool   `json:"should_review_policy"`
	CostClass          string `json:"cost_class"`
	QualityBasis       string `json:"quality_basis"`
	Rationale          string `json:"rationale"`
}

type RouteOutcomeDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	Interpretation   string   `json:"interpretation"`
	PolicyQuestion   string   `json:"policy_question"`
	Recommendation   string   `json:"recommendation"`
	Verification     string   `json:"verification"`
	RequiredEvidence []string `json:"required_evidence"`
	NonClaims        []string `json:"non_claims"`
	ReviewNote       string   `json:"review_note"`
}

type RouteOutcomeCandidate struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Subject       string   `json:"subject"`
	Statement     string   `json:"statement"`
	Detail        string   `json:"detail,omitempty"`
	Confidence    float64  `json:"confidence"`
	Derivation    string   `json:"derivation"`
	EvidenceRefs  []string `json:"evidence_refs"`
	PolicyVersion string   `json:"policy_version"`
	Verdict       string   `json:"verdict"`
}

type RouteOutcomeReviewPaths struct {
	PlanPath   string
	ReviewPath string
}

type OutcomeReviewSelector struct {
	RouteID           string
	RouteEvidencePath string
	ReceiptPath       string
	ReceiptID         string
	WorkID            string
}

func LoadEvidenceByID(homePath, evidenceID string) (Evidence, error) {
	id := strings.TrimSpace(evidenceID)
	if id == "" {
		return Evidence{}, fmt.Errorf("route evidence id required")
	}
	path := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return Evidence{}, fmt.Errorf("read route evidence ledger: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		ev, err := Decode(raw)
		if err != nil {
			return Evidence{}, fmt.Errorf("decode route evidence line %d: %w", line, err)
		}
		if ev.ID == id {
			return ev, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Evidence{}, fmt.Errorf("scan route evidence ledger: %w", err)
	}
	return Evidence{}, fmt.Errorf("route evidence %q not found", id)
}

func LoadEvidenceForReceipt(homePath, receiptID, workID string) (Evidence, error) {
	return loadEvidenceForReceiptRefs(homePath, receiptID, workID, nil)
}

func loadEvidenceForReceiptRefs(homePath, receiptID, workID string, proofRefs []string) (Evidence, error) {
	receiptID = strings.TrimSpace(receiptID)
	workID = strings.TrimSpace(workID)
	proofRefSet := routeEvidenceProofRefSet(proofRefs)
	if receiptID == "" && workID == "" && len(proofRefSet) == 0 {
		return Evidence{}, fmt.Errorf("receipt id work id or route proof ref required")
	}
	path := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return Evidence{}, fmt.Errorf("read route evidence ledger: %w", err)
	}
	defer f.Close()
	var matches []Evidence
	bestScore := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		ev, err := Decode(raw)
		if err != nil {
			return Evidence{}, fmt.Errorf("decode route evidence line %d: %w", line, err)
		}
		score := routeOutcomeEvidenceMatchScore(ev, receiptID, workID, proofRefSet)
		if score == 0 {
			continue
		}
		if score > bestScore {
			bestScore = score
			matches = matches[:0]
		}
		if score == bestScore {
			matches = append(matches, ev)
		}
	}
	if err := scanner.Err(); err != nil {
		return Evidence{}, fmt.Errorf("scan route evidence ledger: %w", err)
	}
	if len(matches) == 0 {
		return Evidence{}, fmt.Errorf("route evidence for receipt %q work %q not found", receiptID, workID)
	}
	if len(matches) > 1 {
		return Evidence{}, fmt.Errorf("multiple route evidence rows match receipt %q work %q: %s; pass --route-id", receiptID, workID, joinEvidenceIDs(matches, 5))
	}
	return matches[0], nil
}

func routeOutcomeEvidenceMatchScore(ev Evidence, receiptID, workID string, proofRefSet map[string]bool) int {
	if proofRefSet[strings.TrimSpace(ev.ID)] {
		return 3
	}
	if receiptID != "" && containsRef(ev.EvidenceRefs, receiptID) {
		return 2
	}
	if workID != "" && ev.WorkID == workID {
		return 1
	}
	return 0
}

func routeEvidenceProofRefSet(refs []string) map[string]bool {
	out := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(ref, "route_") {
			out[ref] = true
		}
	}
	return out
}

func containsRef(refs []string, want string) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref) == want {
			return true
		}
	}
	return false
}

func joinEvidenceIDs(evs []Evidence, limit int) string {
	ids := make([]string, 0, len(evs))
	for i, ev := range evs {
		if limit > 0 && i >= limit {
			ids = append(ids, "...")
			break
		}
		ids = append(ids, ev.ID)
	}
	return strings.Join(ids, ",")
}

func LoadEvidenceFile(path string) (Evidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Evidence{}, fmt.Errorf("read route evidence %s: %w", path, err)
	}
	return Decode(data)
}

func LoadReceiptFile(path string) (envelope.Receipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return envelope.Receipt{}, fmt.Errorf("read receipt %s: %w", path, err)
	}
	return decodeReceiptBytes(data, path)
}

func decodeReceiptBytes(data []byte, source string) (envelope.Receipt, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec envelope.Receipt
	if err := dec.Decode(&rec); err != nil {
		return envelope.Receipt{}, fmt.Errorf("decode receipt %s: %w", source, err)
	}
	if err := rec.Validate(); err != nil {
		return envelope.Receipt{}, err
	}
	return rec, nil
}

func LoadReceiptByID(homePath, receiptID string) (envelope.Receipt, error) {
	id, err := bareRuntimeID("receipt", receiptID)
	if err != nil {
		return envelope.Receipt{}, err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return envelope.Receipt{}, err
	}
	record, err := store.Read(id)
	if err != nil {
		return envelope.Receipt{}, fmt.Errorf("read receipt %s: %w", id, err)
	}
	return decodeReceiptBytes(record.Bytes, record.DiagnosticPath)
}

func LoadReceiptForWorkID(homePath, workID string) (envelope.Receipt, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return envelope.Receipt{}, fmt.Errorf("work id required")
	}
	receiptID, err := latestReceiptIDForWork(homePath, workID)
	if err != nil {
		return envelope.Receipt{}, err
	}
	if receiptID == "" {
		return envelope.Receipt{}, fmt.Errorf("receipt for work %q not found", workID)
	}
	rec, err := LoadReceiptByID(homePath, receiptID)
	if err != nil {
		return envelope.Receipt{}, err
	}
	if rec.IDs.WorkID != workID {
		return envelope.Receipt{}, fmt.Errorf("receipt %q work_id %q does not match requested work %q", rec.ID, rec.IDs.WorkID, workID)
	}
	return rec, nil
}

func ResolveOutcomeReviewInputs(homePath string, sel OutcomeReviewSelector) (Evidence, envelope.Receipt, error) {
	routeID := strings.TrimSpace(sel.RouteID)
	routePath := strings.TrimSpace(sel.RouteEvidencePath)
	receiptPath := strings.TrimSpace(sel.ReceiptPath)
	receiptID := strings.TrimSpace(sel.ReceiptID)
	workID := strings.TrimSpace(sel.WorkID)
	if routeID != "" && routePath != "" {
		return Evidence{}, envelope.Receipt{}, fmt.Errorf("use only one of --route-id or --route-evidence")
	}
	if receiptPath != "" && receiptID != "" {
		return Evidence{}, envelope.Receipt{}, fmt.Errorf("use only one of --receipt or --receipt-id")
	}
	if receiptPath == "" && receiptID == "" && workID == "" {
		return Evidence{}, envelope.Receipt{}, fmt.Errorf("receipt selector required: --receipt, --receipt-id, or --work-id")
	}
	var rec envelope.Receipt
	var err error
	switch {
	case receiptPath != "":
		rec, err = LoadReceiptFile(receiptPath)
	case receiptID != "":
		rec, err = LoadReceiptByID(homePath, receiptID)
	default:
		rec, err = LoadReceiptForWorkID(homePath, workID)
	}
	if err != nil {
		return Evidence{}, envelope.Receipt{}, err
	}
	if workID != "" && rec.IDs.WorkID != "" && rec.IDs.WorkID != workID {
		return Evidence{}, envelope.Receipt{}, fmt.Errorf("receipt work_id %q does not match requested work %q", rec.IDs.WorkID, workID)
	}
	var ev Evidence
	switch {
	case routeID != "":
		ev, err = LoadEvidenceByID(homePath, routeID)
	case routePath != "":
		ev, err = LoadEvidenceFile(routePath)
	default:
		ev, err = loadEvidenceForReceiptRefs(homePath, rec.ID, workID, rec.ProofRefs)
	}
	if err != nil {
		return Evidence{}, envelope.Receipt{}, err
	}
	return ev, rec, nil
}

type workReceiptEvent struct {
	WorkID   string   `json:"work_id"`
	To       string   `json:"to"`
	Attached []string `json:"attached,omitempty"`
}

func latestReceiptIDForWork(homePath, workID string) (string, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return "", err
	}
	f, err := store.OpenWorkLog()
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read work event log: %w", err)
	}
	defer f.Close()
	var receiptID string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var event workReceiptEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return "", fmt.Errorf("decode work event line %d: %w", line, err)
		}
		if event.WorkID != workID || !terminalReceiptEventState(event.To) {
			continue
		}
		for _, attached := range event.Attached {
			if strings.HasPrefix(attached, "rec_") {
				receiptID = attached
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan work event log: %w", err)
	}
	return receiptID, nil
}

func terminalReceiptEventState(state string) bool {
	state = strings.TrimSpace(state)
	return state == "completed" || state == "failed" || state == "cancelled"
}

func bareRuntimeID(kind, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("%s id required", kind)
	}
	if id == "." || id == ".." || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("%s id must be bare id, got %q", kind, id)
	}
	return id, nil
}

func BuildRouteOutcomeReview(ev Evidence, rec envelope.Receipt, targetPath string, now time.Time) (RouteOutcomeReview, error) {
	if err := Validate(ev); err != nil {
		return RouteOutcomeReview{}, err
	}
	if err := rec.Validate(); err != nil {
		return RouteOutcomeReview{}, err
	}
	if ev.WorkID != "" && rec.IDs.WorkID != "" && ev.WorkID != rec.IDs.WorkID {
		return RouteOutcomeReview{}, fmt.Errorf("route evidence work_id %q does not match receipt work_id %q", ev.WorkID, rec.IDs.WorkID)
	}
	if now.IsZero() {
		now = time.Now()
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return RouteOutcomeReview{}, err
	}
	workID := reviewartifact.FirstNonEmpty(ev.WorkID, rec.IDs.WorkID)
	refs := dedupeRefs(append([]string{ev.ID, rec.ID}, append(ev.EvidenceRefs, rec.EvidenceRefs...)...))
	assessment := assessRouteOutcome(ev, rec)
	status := "recorded"
	if assessment.ShouldReviewPolicy {
		status = "review_required"
	}
	id := idFor("ror_", ev.ID, rec.ID, rec.Outcome, fmt.Sprintf("%t", rec.StateDelta.Changed))
	review := RouteOutcomeReview{
		SchemaVersion:       RouteOutcomeReviewSchemaVersion,
		ID:                  id,
		CreatedAt:           rfc3339(now),
		Status:              status,
		RouteEvidenceID:     ev.ID,
		RequestID:           ev.RequestID,
		WorkID:              workID,
		ReceiptID:           rec.ID,
		ReceiptOutcome:      rec.Outcome,
		ReceiptMoveType:     rec.MoveType,
		ReceiptStateChanged: rec.StateDelta.Changed,
		RouteStatus:         ev.Status,
		SelectedTarget:      ev.SelectedTarget,
		Usage:               ev.Usage,
		CostMicros:          ev.CostMicros,
		LatencyMS:           ev.LatencyMS,
		EvidenceRefs:        refs,
		TargetPath:          strings.TrimSpace(targetPath),
		TargetExists:        targetExists,
		TargetHash:          targetHash,
		Assessment:          assessment,
		Warnings: []string{
			"does not mutate gateway provider config",
			"does not change routing policy",
			"does not claim task success or billing truth",
		},
	}
	review.ReviewDraft = buildRouteOutcomeDraft(review, rec)
	return review, nil
}

func RecordRouteOutcomeReview(homePath string, review RouteOutcomeReview) (RouteOutcomeReviewPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return RouteOutcomeReviewPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if review.ID == "" {
		return RouteOutcomeReviewPaths{}, fmt.Errorf("route outcome review id required")
	}
	paths, err := reviewartifact.Write(homePath, "route-remorse", review.ID, review, "  ", RenderRouteOutcomeReview(review))
	return RouteOutcomeReviewPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RecordRouteOutcomeObservation(homePath string, review RouteOutcomeReview, now time.Time) (RouteOutcomeReview, error) {
	if !review.Assessment.ShouldReviewPolicy {
		return review, fmt.Errorf("route outcome %s does not require policy observation", review.Assessment.Class)
	}
	enriched, err := enrichRouteOutcomeReview(homePath, review)
	if err != nil {
		return review, err
	}
	review = enriched
	cand := BuildRouteOutcomeCandidate(review)
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return review, err
	}
	path := filepath.Join(dir, cand.ID+".json")
	data, err := json.MarshalIndent(cand, "", "  ")
	if err != nil {
		return review, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return review, err
	}
	store, err := writeback.Open(homePath)
	if err != nil {
		return review, err
	}
	nop, inserted, err := store.RecordNoPromote(RouteOutcomeNoPromote(cand, review, now), now)
	if err != nil {
		return review, err
	}
	review.CandidateID = cand.ID
	review.NoPromoteID = nop.ID
	review.RecordedCandidatePath = path
	review.RecordedNoPromote = true
	review.RecordNoPromoteInserted = inserted
	return review, nil
}

func BuildRouteOutcomeCandidate(review RouteOutcomeReview) RouteOutcomeCandidate {
	target := review.SelectedTarget
	routeKey := strings.Trim(strings.Join([]string{target.Provider, target.Model, target.Protocol}, "/"), "/")
	if routeKey == "" {
		routeKey = "unknown-route"
	}
	subject := "route-outcome:" + routeKey
	statement := fmt.Sprintf(
		"route %s produced receipt outcome %s changed=%t",
		routeKey,
		reviewartifact.FirstNonEmpty(review.ReceiptOutcome, "unknown"),
		review.ReceiptStateChanged,
	)
	detailParts := []string{
		"class=" + review.Assessment.Class,
		"request=" + review.RequestID,
		"work=" + review.WorkID,
		"route_status=" + review.RouteStatus,
		fmt.Sprintf("cost_micros=%d", review.CostMicros),
		fmt.Sprintf("latency_ms=%d", review.LatencyMS),
	}
	if len(review.CapabilityClasses) > 0 {
		detailParts = append(detailParts, "capability_classes="+strings.Join(review.CapabilityClasses, ","))
	}
	detail := strings.Join(detailParts, " ")
	refs := dedupeRefs(append([]string{review.ID}, review.EvidenceRefs...))
	return RouteOutcomeCandidate{
		SchemaVersion: RouteOutcomeCandidateSchemaVersion,
		ID:            idFor("route_outcome_", subject, statement, detail, strings.Join(refs, "|")),
		Kind:          string(candidates.RouteObservation),
		Subject:       subject,
		Statement:     statement,
		Detail:        detail,
		Confidence:    1,
		Derivation:    "route_outcome_review",
		EvidenceRefs:  refs,
		PolicyVersion: "route-outcome-review.v0.1",
		Verdict:       candidates.NoPromote,
	}
}

func RouteOutcomeNoPromote(cand RouteOutcomeCandidate, review RouteOutcomeReview, now time.Time) writeback.NoPromoteDecision {
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CandidateRef:  cand.ID,
		OwnerTarget:   writeback.OwnerTarget{Kind: "route_outcome_review", Ref: review.RouteEvidenceID},
		Basis:         "route outcome requires review before routing policy or provider config changes",
		EvidenceRefs:  dedupeRefs(append([]string{review.ID}, review.EvidenceRefs...)),
		Status:        writeback.NoPromoteActive,
		CreatedAt:     rfc3339(now),
	}
}

func enrichRouteOutcomeReview(homePath string, review RouteOutcomeReview) (RouteOutcomeReview, error) {
	classes, err := routeOutcomeCapabilityClasses(homePath, review.SelectedTarget)
	if err != nil {
		return review, err
	}
	review.CapabilityClasses = reviewartifact.UniqueSorted(append(review.CapabilityClasses, classes...))
	return review, nil
}

func routeOutcomeCapabilityClasses(homePath string, target Target) ([]string, error) {
	manifest, _, err := LoadModelCapabilitiesFromHome(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load route capability authority for outcome review: %w", err)
	}
	targetIDs := routeOutcomeTargetIDs(target)
	var classes []string
	for class, defaults := range manifest.RoutingDefaults {
		for _, modelID := range defaults {
			if targetIDs[strings.ToLower(strings.TrimSpace(modelID))] {
				classes = append(classes, class)
				break
			}
		}
	}
	return reviewartifact.UniqueSorted(classes), nil
}

func routeOutcomeTargetIDs(target Target) map[string]bool {
	provider := strings.TrimSpace(target.Provider)
	model := strings.TrimSpace(target.Model)
	protocol := strings.TrimSpace(target.Protocol)
	out := map[string]bool{}
	add := func(parts ...string) {
		var clean []string
		for _, part := range parts {
			part = strings.Trim(part, "/ ")
			if part != "" {
				clean = append(clean, part)
			}
		}
		if len(clean) == 0 {
			return
		}
		out[strings.ToLower(strings.Join(clean, "/"))] = true
	}
	add(model)
	add(provider, model)
	add(provider, model, protocol)
	return out
}

func RenderRouteOutcomeReview(review RouteOutcomeReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Route Outcome Review: %s\n\n", review.RouteEvidenceID)
	fmt.Fprintf(&b, "- status: %s\n", review.Status)
	fmt.Fprintf(&b, "- review_id: %s\n", review.ID)
	fmt.Fprintf(&b, "- route_evidence: %s\n", review.RouteEvidenceID)
	fmt.Fprintf(&b, "- receipt: %s\n", review.ReceiptID)
	if review.WorkID != "" {
		fmt.Fprintf(&b, "- work: %s\n", review.WorkID)
	}
	fmt.Fprintf(&b, "- selected_target: %s/%s (%s)\n", review.SelectedTarget.Provider, review.SelectedTarget.Model, review.SelectedTarget.Protocol)
	if len(review.CapabilityClasses) > 0 {
		fmt.Fprintf(&b, "- capability_classes: %s\n", strings.Join(review.CapabilityClasses, ", "))
	}
	fmt.Fprintf(&b, "- receipt_outcome: %s\n", review.ReceiptOutcome)
	fmt.Fprintf(&b, "- state_changed: %t\n", review.ReceiptStateChanged)
	fmt.Fprintf(&b, "- assessment_class: %s\n", review.Assessment.Class)
	fmt.Fprintf(&b, "- should_review_policy: %t\n", review.Assessment.ShouldReviewPolicy)
	if review.CandidateID != "" {
		fmt.Fprintf(&b, "- candidate: %s\n", review.CandidateID)
	}
	if review.NoPromoteID != "" {
		fmt.Fprintf(&b, "- no_promote: %s\n", review.NoPromoteID)
	}
	if review.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", review.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", review.TargetExists)
	}
	if review.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", review.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(review.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate gateway/provider config, change routing policy, claim task success, or calculate billing truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Assessment")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- class: %s\n", review.Assessment.Class)
	fmt.Fprintf(&b, "- cost_class: %s\n", review.Assessment.CostClass)
	fmt.Fprintf(&b, "- quality_basis: %s\n", review.Assessment.QualityBasis)
	fmt.Fprintf(&b, "- rationale: %s\n", review.Assessment.Rationale)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Review")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- interpretation: %s\n", review.ReviewDraft.Interpretation)
	fmt.Fprintf(&b, "- policy_question: %s\n", review.ReviewDraft.PolicyQuestion)
	fmt.Fprintf(&b, "- recommendation: %s\n", review.ReviewDraft.Recommendation)
	fmt.Fprintf(&b, "- verification: %s\n", review.ReviewDraft.Verification)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Policy Change")
	fmt.Fprintln(&b)
	for _, evidence := range review.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Non-Claims")
	fmt.Fprintln(&b)
	for _, claim := range review.ReviewDraft.NonClaims {
		fmt.Fprintf(&b, "- %s\n", claim)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", review.ReviewDraft.ReviewNote)
	return b.String()
}

func assessRouteOutcome(ev Evidence, rec envelope.Receipt) RouteOutcomeAssessment {
	routeOK := routeStatusOK(ev.Status)
	outcome := strings.TrimSpace(rec.Outcome)
	class := "route_unproven"
	shouldReview := true
	rationale := "receipt did not prove state change; route should not be treated as validated"
	switch {
	case !routeOK:
		class = "route_transport_problem"
		rationale = "route evidence status was not successful"
	case outcome == "failed" || outcome == "blocked":
		class = "route_remorse"
		rationale = "receipt outcome indicates the selected route did not complete the work"
	case outcome == "completed" && rec.StateDelta.Changed:
		class = "route_supported"
		shouldReview = false
		rationale = "receipt completed with state delta; route may be used as supporting evidence, not proof of general policy"
	case outcome == "completed":
		class = "route_unproven"
		rationale = "receipt completed without state delta; cheap or expensive route quality remains unproven"
	case outcome == "no_op":
		class = "route_no_op"
		shouldReview = false
		rationale = "receipt was an explicit no-op; do not use it as routing success or failure"
	}
	return RouteOutcomeAssessment{
		Class:              class,
		ShouldReviewPolicy: shouldReview,
		CostClass:          costClass(ev),
		QualityBasis:       "receipt_outcome_and_state_delta_only",
		Rationale:          rationale,
	}
}

func buildRouteOutcomeDraft(review RouteOutcomeReview, rec envelope.Receipt) RouteOutcomeDraft {
	target := review.SelectedTarget
	routeKey := strings.Trim(strings.Join([]string{target.Provider, target.Model, target.Protocol}, "/"), "/")
	if routeKey == "" {
		routeKey = "unknown-route"
	}
	recommendation := "Keep this as supporting route evidence only; do not change routing policy from one successful receipt."
	if review.Assessment.ShouldReviewPolicy {
		recommendation = "Review whether this route should be avoided, escalated, or constrained for similar future jobs before changing any router/provider config."
	}
	return RouteOutcomeDraft{
		ReviewID:       review.ID,
		Title:          "Route outcome review for " + routeKey,
		Interpretation: fmt.Sprintf("Route %s produced receipt outcome %s with state_changed=%t.", routeKey, review.ReceiptOutcome, review.ReceiptStateChanged),
		PolicyQuestion: "Should future jobs with this task/risk/capability shape use the same route, escalate earlier, or add a guardrail?",
		Recommendation: recommendation,
		Verification:   "Any accepted routing change must later be cited by fresh ExecutionRouteEvidence and Transition records that still avoid claiming task success from route metadata alone.",
		RequiredEvidence: []string{
			"the original ExecutionRouteEvidence row",
			"the work Receipt and its state_delta verification",
			"at least one comparable successful or failed route outcome",
			"cost, latency, privacy, and capability tradeoff notes for the task class",
		},
		NonClaims: []string{
			"not billing truth",
			"not provider or model ranking",
			"not task success by itself",
			"not permission to mutate gateway/provider/model-router config",
		},
		ReviewNote: reviewartifact.FirstNonEmpty(
			rec.Next.ReviewTrigger,
			"Route outcome review is evidence for a future route_observation policy draft only after owner review.",
		),
	}
}

func routeStatusOK(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "ok", "success", "succeeded":
		return true
	default:
		return false
	}
}

func costClass(ev Evidence) string {
	switch {
	case ev.CostMicros > 0:
		return "known_cost"
	case ev.Usage.TotalTokens > 0:
		return "tokens_without_cost"
	default:
		return "unknown_cost"
	}
}
