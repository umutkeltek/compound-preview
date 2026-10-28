package reviewartifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

const (
	ReviewResolutionSchemaVersion       = "compound.review_resolution.v0.1"
	ReviewResolutionResultSchemaVersion = "compound.review_resolution_result.v0.1"
)

var reviewResolutionDecisions = map[string]bool{
	"reviewed":  true,
	"parked":    true,
	"dismissed": true,
}

// ReviewResolution is the generic, authority-neutral disposition sidecar for
// one review artifact. The reviewed_by alias preserves the existing archive
// convention while actor is the canonical provenance field for this record.
type ReviewResolution struct {
	SchemaVersion     string `json:"schema_version"`
	ID                string `json:"id"`
	Status            string `json:"status"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	Actor             string `json:"actor"`
	ReviewedBy        string `json:"reviewed_by"`
	ResolvedAt        string `json:"resolved_at"`
	SourceArtifactRef string `json:"source_artifact_ref"`
	PreviousStatus    string `json:"previous_status"`
	PreviousReadiness string `json:"previous_readiness"`
	PreviousBand      string `json:"previous_band"`
	ReceiptID         string `json:"receipt_id"`
	UndoHint          string `json:"undo_hint"`
}

type ResolutionInput struct {
	Decision string
	Reason   string
	Actor    string
}

type UndoResolutionInput struct {
	Reason string
	Actor  string
}

type ReviewResolutionResult struct {
	SchemaVersion      string            `json:"schema_version"`
	Operation          string            `json:"operation"`
	ItemID             string            `json:"item_id"`
	Area               string            `json:"area"`
	Decision           string            `json:"decision,omitempty"`
	Reason             string            `json:"reason"`
	Status             string            `json:"status"`
	Readiness          string            `json:"readiness"`
	SidecarPath        string            `json:"sidecar_path"`
	ReceiptID          string            `json:"receipt_id"`
	ReceiptPath        string            `json:"receipt_path"`
	UndoHint           string            `json:"undo_hint,omitempty"`
	RestoredVisibility map[string]string `json:"restored_visibility,omitempty"`
}

func ParseReviewResolution(data []byte) (ReviewResolution, error) {
	var resolution ReviewResolution
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resolution); err != nil {
		return ReviewResolution{}, fmt.Errorf("decode review resolution: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return ReviewResolution{}, fmt.Errorf("decode review resolution: trailing JSON")
	}
	if err := resolution.Validate(); err != nil {
		return ReviewResolution{}, err
	}
	return resolution, nil
}

func LoadReviewResolution(path string) (ReviewResolution, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewResolution{}, err
	}
	resolution, err := ParseReviewResolution(data)
	if err != nil {
		return ReviewResolution{}, fmt.Errorf("review resolution %s: %w", path, err)
	}
	return resolution, nil
}

func reviewResolutionForCore(corePath string) (*ReviewResolution, string) {
	sidecarPath := strings.TrimSuffix(corePath, ".json") + ".reviewed.json"
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, ""
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.SchemaVersion != ReviewResolutionSchemaVersion {
		return nil, ""
	}
	resolution, err := ParseReviewResolution(data)
	if err != nil {
		return nil, fmt.Sprintf("invalid generic review resolution %s: %v", sidecarPath, err)
	}
	return &resolution, ""
}

func (r ReviewResolution) Validate() error {
	if r.SchemaVersion != ReviewResolutionSchemaVersion {
		return fmt.Errorf("review resolution schema_version = %q, want %q", r.SchemaVersion, ReviewResolutionSchemaVersion)
	}
	if !strings.HasPrefix(strings.TrimSpace(r.ID), "rres_") {
		return fmt.Errorf("review resolution id must start with rres_")
	}
	if strings.TrimSpace(r.Status) != "recorded" {
		return fmt.Errorf("review resolution status must be recorded")
	}
	if !reviewResolutionDecisions[strings.TrimSpace(r.Decision)] {
		return fmt.Errorf("review resolution decision must be reviewed, parked, or dismissed")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("review resolution reason required")
	}
	if strings.TrimSpace(r.Actor) == "" {
		return fmt.Errorf("review resolution actor required")
	}
	if strings.TrimSpace(r.ReviewedBy) == "" || strings.TrimSpace(r.ReviewedBy) != strings.TrimSpace(r.Actor) {
		return fmt.Errorf("review resolution reviewed_by must match actor")
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(r.ResolvedAt)); err != nil {
		return fmt.Errorf("review resolution resolved_at must be RFC3339: %w", err)
	}
	if strings.TrimSpace(r.SourceArtifactRef) == "" {
		return fmt.Errorf("review resolution source_artifact_ref required")
	}
	if !strings.HasPrefix(strings.TrimSpace(r.ReceiptID), "rec_") {
		return fmt.Errorf("review resolution receipt_id must start with rec_")
	}
	if strings.TrimSpace(r.UndoHint) == "" {
		return fmt.Errorf("review resolution undo_hint required")
	}
	return nil
}

// ApplyGenericResolutions is deliberately idempotent. Recount invokes it after
// every owner annotator so a valid generic sidecar remains the final queue
// disposition without changing any owner-specific annotator.
func ApplyGenericResolutions(report *QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Resolution == nil {
			continue
		}
		item.Status = "recorded"
		if item.Resolution.Decision == "reviewed" {
			item.Readiness = "review_recorded"
		} else {
			item.Readiness = "no_action"
		}
		item.Band = BandFiled
		item.Actions = nil
		item.DecisionCard = nil
		item.BlockedReason = ""
		item.NextAction = "generic review disposition recorded: " + item.Resolution.Decision
	}
}

func ResolveQueueItem(homePath string, item QueueItem, input ResolutionInput, now time.Time) (ReviewResolutionResult, error) {
	decision := strings.TrimSpace(input.Decision)
	reason := strings.TrimSpace(input.Reason)
	actor := strings.TrimSpace(input.Actor)
	if !reviewResolutionDecisions[decision] {
		return ReviewResolutionResult{}, fmt.Errorf("decision must be reviewed, parked, or dismissed")
	}
	if reason == "" {
		return ReviewResolutionResult{}, fmt.Errorf("reason required")
	}
	if actor == "" {
		return ReviewResolutionResult{}, fmt.Errorf("actor required")
	}
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.JSONPath) == "" {
		return ReviewResolutionResult{}, fmt.Errorf("review item id and artifact path required")
	}
	if item.Resolution != nil {
		return ReviewResolutionResult{}, fmt.Errorf("review item %s is already resolved; undo with: %s", item.ID, item.Resolution.UndoHint)
	}
	if terminalResolutionStatus(item.Status) {
		return ReviewResolutionResult{}, fmt.Errorf("review item %s is already terminal with status %s; generic resolution cannot replace an owner disposition", item.ID, item.Status)
	}
	if now.IsZero() {
		now = time.Now()
	}
	sidecarPath, sourceRef, err := resolutionPaths(homePath, item)
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	if _, err := os.Stat(sidecarPath); err == nil {
		return ReviewResolutionResult{}, fmt.Errorf("review item %s already has a reviewed sidecar; use its canonical owner path or undo the generic resolution", item.ID)
	} else if !os.IsNotExist(err) {
		return ReviewResolutionResult{}, err
	}
	timestamp := Timestamp(now)
	undoHint := fmt.Sprintf("compound reviews resolve --item %s --undo --reason '<why>'", item.ID)
	receiptID := ID("rec_", "review-resolve", item.Area, item.ID, decision, reason, timestamp)
	resolution := ReviewResolution{
		SchemaVersion:     ReviewResolutionSchemaVersion,
		ID:                ID("rres_", item.Area, item.ID, decision, reason, timestamp),
		Status:            "recorded",
		Decision:          decision,
		Reason:            reason,
		Actor:             actor,
		ReviewedBy:        actor,
		ResolvedAt:        timestamp,
		SourceArtifactRef: sourceRef,
		PreviousStatus:    item.Status,
		PreviousReadiness: item.Readiness,
		PreviousBand:      item.Band,
		ReceiptID:         receiptID,
		UndoHint:          undoHint,
	}
	if err := resolution.Validate(); err != nil {
		return ReviewResolutionResult{}, err
	}
	sidecarRef := relativeHomeRef(homePath, sidecarPath)
	receipt := resolutionReceipt(receiptID, actor, item.Area, now, "resolve", decision, reason, sourceRef, sidecarRef, true)
	store, err := eventstore.Open(homePath)
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	put, err := writeResolutionPair(store, receipt, sidecarPath, resolution)
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	readiness := "no_action"
	if decision == "reviewed" {
		readiness = "review_recorded"
	}
	return ReviewResolutionResult{
		SchemaVersion: ReviewResolutionResultSchemaVersion,
		Operation:     "resolved",
		ItemID:        item.ID,
		Area:          item.Area,
		Decision:      decision,
		Reason:        reason,
		Status:        "recorded",
		Readiness:     readiness,
		SidecarPath:   sidecarPath,
		ReceiptID:     receipt.ID,
		ReceiptPath:   put.DiagnosticPath,
		UndoHint:      undoHint,
	}, nil
}

func UndoQueueItem(homePath string, item QueueItem, input UndoResolutionInput, now time.Time) (ReviewResolutionResult, error) {
	reason := strings.TrimSpace(input.Reason)
	actor := strings.TrimSpace(input.Actor)
	if reason == "" {
		return ReviewResolutionResult{}, fmt.Errorf("reason required")
	}
	if actor == "" {
		return ReviewResolutionResult{}, fmt.Errorf("actor required")
	}
	if item.Resolution == nil {
		return ReviewResolutionResult{}, fmt.Errorf("review item %s has no active generic resolution to undo", item.ID)
	}
	if now.IsZero() {
		now = time.Now()
	}
	sidecarPath, sourceRef, err := resolutionPaths(homePath, item)
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	active := *item.Resolution
	timestamp := Timestamp(now)
	receiptID := ID("rec_", "review-resolve-undo", item.Area, item.ID, active.ID, reason, timestamp)
	sidecarRef := relativeHomeRef(homePath, sidecarPath)
	receipt := resolutionReceipt(receiptID, actor, item.Area, now, "undo", active.Decision, reason, sourceRef, sidecarRef, true)
	receipt.EvidenceRefs = UniqueSorted(append(receipt.EvidenceRefs, "receipt:"+active.ReceiptID))
	receipt.ProofRefs = UniqueSorted(append(receipt.ProofRefs, "review-resolution:"+active.ID))
	if err := receipt.Validate(); err != nil {
		return ReviewResolutionResult{}, err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	put, err := store.PutJSONExclusive(receipt.ID, receipt, " ")
	if err != nil {
		return ReviewResolutionResult{}, err
	}
	if err := os.Remove(sidecarPath); err != nil {
		_ = store.Remove(receipt.ID)
		return ReviewResolutionResult{}, fmt.Errorf("remove active review resolution: %w", err)
	}
	return ReviewResolutionResult{
		SchemaVersion: ReviewResolutionResultSchemaVersion,
		Operation:     "undone",
		ItemID:        item.ID,
		Area:          item.Area,
		Decision:      active.Decision,
		Reason:        reason,
		Status:        active.PreviousStatus,
		Readiness:     active.PreviousReadiness,
		SidecarPath:   sidecarPath,
		ReceiptID:     receipt.ID,
		ReceiptPath:   put.DiagnosticPath,
		RestoredVisibility: map[string]string{
			"status":    active.PreviousStatus,
			"readiness": active.PreviousReadiness,
			"band":      active.PreviousBand,
		},
	}, nil
}

func resolutionPaths(homePath string, item QueueItem) (sidecarPath, sourceRef string, err error) {
	homePath, err = filepath.Abs(strings.TrimSpace(homePath))
	if err != nil {
		return "", "", err
	}
	sourcePath, err := filepath.Abs(strings.TrimSpace(item.JSONPath))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(homePath, sourcePath)
	if err != nil {
		return "", "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || !strings.HasPrefix(rel, "reviews/") {
		return "", "", fmt.Errorf("review artifact %s is outside COMPOUND_HOME/reviews", sourcePath)
	}
	if filepath.Ext(sourcePath) != ".json" || !coreReviewJSON(sourcePath) {
		return "", "", fmt.Errorf("review artifact must be a core JSON file: %s", sourcePath)
	}
	return strings.TrimSuffix(sourcePath, ".json") + ".reviewed.json", rel, nil
}

func resolutionReceipt(id, actor, area string, now time.Time, operation, decision, reason, sourceRef, sidecarRef string, changed bool) envelope.Receipt {
	receipt := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     Timestamp(now),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Scope:         envelope.Scope{Surface: "reviews/" + area},
			Authority:     envelope.Authority{SourceClass: "review", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  UniqueSorted([]string{sourceRef, sidecarRef}),
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:  "completed",
		MoveType: "audit_only",
		StateDelta: envelope.StateDelta{
			Changed:      changed,
			Summary:      fmt.Sprintf("review resolution %s: %s (%s) by %s: %s", operation, decision, area, actor, reason),
			FilesChanged: []string{},
			Artifacts:    UniqueSorted([]string{sourceRef, sidecarRef}),
			Verification: []string{},
		},
		Cost: envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{
			Used:         []string{},
			Missed:       []string{},
			Candidate:    []string{},
			UsedDeclared: false,
		},
		Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}, Rejected: []string{}},
		Next:   envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: []string{
			"review-resolution:" + operation,
		},
	}
	return receipt
}

func writeResolutionPair(store *eventstore.Store, receipt envelope.Receipt, sidecarPath string, resolution ReviewResolution) (eventstore.PutResult, error) {
	if err := receipt.Validate(); err != nil {
		return eventstore.PutResult{}, fmt.Errorf("review resolution receipt invalid: %w", err)
	}
	if store == nil {
		return eventstore.PutResult{}, fmt.Errorf("review resolution event store required")
	}
	put, err := store.PutJSONExclusive(receipt.ID, receipt, " ")
	if err != nil {
		return eventstore.PutResult{}, err
	}
	if err := writeExclusiveJSON(sidecarPath, resolution); err != nil {
		_ = store.Remove(receipt.ID)
		return eventstore.PutResult{}, err
	}
	return put, nil
}

func writeExclusiveJSON(path string, payload any) error {
	data, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	return nil
}

func terminalResolutionStatus(status string) bool {
	return TerminalStatus(status)
}

func relativeHomeRef(homePath, path string) string {
	rel, err := filepath.Rel(homePath, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
