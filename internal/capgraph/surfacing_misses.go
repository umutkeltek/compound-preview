package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

// skillSurfacingReviewArea is the reviews/ subdirectory the skill-surfacing
// feedback bridge and session-mining backfill write review artifacts into
// (compound.tools.skill_surfacing_feedback_bridge, packet backfill jobs).
const skillSurfacingReviewArea = "skill-surfacing"

type SkillSurfacingMisses struct {
	ID            string
	SourcePath    string
	Revision      string
	Generated     string
	Scanned       int
	Skipped       int
	LaneFails     int
	Remaining     int
	Proposals     []SkillSurfacingMissSummary `json:"proposals"`
	AllMisses     []SkillSurfacingMissSummary `json:"all_misses"`
	FalseTriggers []SkillSurfacingMissSummary `json:"false_triggers"`
	PerReceipt    []SkillSurfacingMissReceipt `json:"perReceipt"`
}

type SkillSurfacingMissSummary struct {
	Skill    string   `json:"skill"`
	Count    int      `json:"count"`
	Receipts []string `json:"receipts"`
	Reasons  []string `json:"reasons"`
	MaxConf  float64  `json:"maxConf"`
}

type SkillSurfacingMissReceipt struct {
	Dir                 string                    `json:"dir"`
	Kind                string                    `json:"kind"`
	Lanes               string                    `json:"lanes"`
	Real                []SkillSurfacingMissEntry `json:"real"`
	FalseTriggered      []SkillSurfacingMissEntry `json:"false_triggered"`
	FalseTriggeredCamel []SkillSurfacingMissEntry `json:"falseTriggered"`
}

type SkillSurfacingMissEntry struct {
	Skill      string  `json:"skill"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type SkillSurfacingMissesReport struct {
	Status                string   `json:"status"`
	ReportID              string   `json:"report_id"`
	MissesRead            int      `json:"misses_read"`
	FalseTriggersRead     int      `json:"false_triggers_read"`
	AppliedMisses         int      `json:"applied_misses"`
	AppliedFalseTriggers  int      `json:"applied_false_triggers"`
	AppliedEdges          int      `json:"applied_edges"`
	SkippedUnknown        []string `json:"skipped_unknown,omitempty"`
	SkippedMissingContext []string `json:"skipped_missing_context,omitempty"`
	EdgeIDs               []string `json:"edge_ids"`
}

func LoadSkillSurfacingMisses(path string) (SkillSurfacingMisses, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillSurfacingMisses{}, fmt.Errorf("read skill surfacing misses %s: %w", path, err)
	}
	var report SkillSurfacingMisses
	if err := json.Unmarshal(data, &report); err != nil {
		return SkillSurfacingMisses{}, fmt.Errorf("parse skill surfacing misses %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	report.SourcePath = path
	report.Revision = "sha256:" + hex.EncodeToString(sum[:])
	report.ID = "skill_surfacing_misses:" + hex.EncodeToString(sum[:])[:12]
	if err := report.Validate(); err != nil {
		return SkillSurfacingMisses{}, fmt.Errorf("validate skill surfacing misses %s: %w", path, err)
	}
	return report, nil
}

func (m SkillSurfacingMisses) Validate() error {
	if strings.TrimSpace(m.Generated) == "" {
		return fmt.Errorf("generated required")
	}
	if len(m.AllMisses) == 0 && len(m.FalseTriggers) == 0 && len(m.PerReceipt) == 0 {
		return fmt.Errorf("all_misses, false_triggers, or perReceipt required")
	}
	for _, receipt := range m.PerReceipt {
		falseTriggers := receipt.falseTriggered()
		if strings.TrimSpace(receipt.Dir) == "" && (len(receipt.Real) > 0 || len(falseTriggers) > 0) {
			return fmt.Errorf("receipt dir required for real misses or false triggers")
		}
		for _, miss := range receipt.Real {
			if err := validateSkillSurfacingEntry("miss", miss); err != nil {
				return err
			}
		}
		for _, trigger := range falseTriggers {
			if err := validateSkillSurfacingEntry("false-trigger", trigger); err != nil {
				return err
			}
		}
	}
	return nil
}

func ApplySkillSurfacingMisses(homePath string, misses SkillSurfacingMisses, now time.Time) (SkillSurfacingMissesReport, error) {
	report := SkillSurfacingMissesReport{
		Status:   "ok",
		ReportID: misses.ID,
	}
	store, err := Open(homePath)
	if err != nil {
		report.Status = "fail"
		return report, err
	}
	nodes, err := store.loadNodes()
	if err != nil {
		report.Status = "fail"
		return report, err
	}
	edges, err := store.loadEdges()
	if err != nil {
		report.Status = "fail"
		return report, err
	}
	existingEdges := map[string]bool{}
	for _, edge := range edges {
		existingEdges[edgeKey(edge)] = true
	}
	unknown := map[string]bool{}
	missingContext := map[string]bool{}
	var newEdges []Edge
	for _, receipt := range misses.PerReceipt {
		if err := collectSkillSurfacingFeedback(&newEdges, nodes, existingEdges, unknown, missingContext, receipt, receipt.Real, EdMissedWhen, now, &report); err != nil {
			report.Status = "fail"
			return report, err
		}
		if err := collectSkillSurfacingFeedback(&newEdges, nodes, existingEdges, unknown, missingContext, receipt, receipt.falseTriggered(), EdFalseTriggeredBy, now, &report); err != nil {
			report.Status = "fail"
			return report, err
		}
	}
	if len(newEdges) > 0 {
		if err := ApplyNodesAndEdgesGoverned(homePath, nil, newEdges, now); err != nil {
			report.Status = "fail"
			return report, err
		}
	}
	sort.Strings(report.SkippedUnknown)
	sort.Strings(report.SkippedMissingContext)
	sort.Strings(report.EdgeIDs)
	if len(report.SkippedUnknown) > 0 {
		report.Status = "review"
	}
	return report, nil
}

func collectSkillSurfacingFeedback(edges *[]Edge, nodes []Node, existingEdges, unknown, missingContext map[string]bool, receipt SkillSurfacingMissReceipt, entries []SkillSurfacingMissEntry, edgeKind string, now time.Time, report *SkillSurfacingMissesReport) error {
	for _, entry := range entries {
		if edgeKind == EdFalseTriggeredBy {
			report.FalseTriggersRead++
		} else {
			report.MissesRead++
		}
		nodeID := resolveCapabilityNode(nodes, entry.Skill)
		if nodeID == "" {
			nodeID = capabilityNodeID(entry.Skill)
		}
		if nodeID == "" || !capabilityNodeKnown(nodes, nodeID) {
			if !unknown[entry.Skill] {
				report.SkippedUnknown = append(report.SkippedUnknown, entry.Skill)
				unknown[entry.Skill] = true
			}
			continue
		}
		context := strings.TrimSpace(entry.Reason)
		if context == "" {
			context = strings.TrimSpace(receipt.Dir)
		}
		tokens := skillSurfacingContextTokens(context)
		if len(tokens) == 0 {
			key := firstEvalNonEmpty(receipt.Dir, entry.Skill)
			if !missingContext[key] {
				report.SkippedMissingContext = append(report.SkippedMissingContext, key)
				missingContext[key] = true
			}
			continue
		}
		applied := false
		for _, tok := range tokens {
			edge := Edge{
				Kind:       edgeKind,
				From:       "context:" + tok,
				To:         nodeID,
				Confidence: entry.Confidence,
				CreatedAt:  now.UTC().Format(time.RFC3339),
			}
			added, err := addEdgeOnceCollect(edges, existingEdges, edge)
			if err != nil {
				return err
			}
			if added {
				report.AppliedEdges++
				report.EdgeIDs = append(report.EdgeIDs, edgeID(edge))
				applied = true
			}
		}
		if applied {
			if edgeKind == EdFalseTriggeredBy {
				report.AppliedFalseTriggers++
			} else {
				report.AppliedMisses++
			}
		}
	}
	return nil
}

func skillSurfacingEvidenceRefs(misses SkillSurfacingMisses) []string {
	refs := []string{"skill_surfacing_misses:" + strings.TrimPrefix(misses.Revision, "sha256:")}
	seen := map[string]bool{refs[0]: true}
	for _, receipt := range misses.PerReceipt {
		if strings.TrimSpace(receipt.Dir) == "" {
			continue
		}
		ref := "receipt:" + strings.TrimSpace(receipt.Dir)
		if !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	return refs
}

func validateSkillSurfacingEntry(kind string, entry SkillSurfacingMissEntry) error {
	if strings.TrimSpace(entry.Skill) == "" {
		return fmt.Errorf("%s skill required", kind)
	}
	if entry.Confidence <= 0 || entry.Confidence > 1 {
		return fmt.Errorf("%s confidence must be > 0 and <= 1", kind)
	}
	return nil
}

func (r SkillSurfacingMissReceipt) falseTriggered() []SkillSurfacingMissEntry {
	if len(r.FalseTriggeredCamel) == 0 {
		return r.FalseTriggered
	}
	out := make([]SkillSurfacingMissEntry, 0, len(r.FalseTriggered)+len(r.FalseTriggeredCamel))
	out = append(out, r.FalseTriggered...)
	out = append(out, r.FalseTriggeredCamel...)
	return out
}

func capabilityNodeKnown(nodes []Node, nodeID string) bool {
	for _, node := range nodes {
		if node.ID == nodeID {
			return true
		}
	}
	return false
}

// skillSurfacingContextTokens is tokenList: one rule for what a task word is.
// This file used to keep a second, shorter stop list and its own length floor;
// two tokenizers in one package meant a miss could be recorded on a token the
// surfacer would never match.
func skillSurfacingContextTokens(context string) []string {
	return tokenList(context)
}

func SkillSurfacingMissesEvidenceRefs(misses SkillSurfacingMisses) []string {
	return skillSurfacingEvidenceRefs(misses)
}

func SkillSurfacingMissesSourceName(path string) string {
	if strings.TrimSpace(path) == "" {
		return "skill-surfacing-proposals.json"
	}
	return filepath.Base(path)
}

// AnnotateSkillSurfacingQueue attaches the "fold these misses into the
// capability graph" one-click action to every OPEN skill-surfacing review
// item, so the card carries the verb that has existed since 07-10
// (`compound capability import-surfacing-misses <path>`) instead of "owner
// has not exposed a safe action for this decision group". It never applies
// misses, writes graph edges, or resolves anything itself — read-only
// annotation, same contract as every other domain queue annotator.
func AnnotateSkillSurfacingQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != skillSurfacingReviewArea {
			continue
		}
		annotateSkillSurfacingItem(item)
	}
}

func annotateSkillSurfacingItem(item *reviewartifact.QueueItem) {
	// Resolved items (a generic resolution sidecar already recorded, or the
	// owner marked the item terminal by a native status field) get NO
	// action — the card is already closed, one-shot import stays one-shot.
	// Terminal words are owned by reviewartifact.TerminalStatus; the local
	// mirror this replaced omitted "completed", so finished items were
	// re-offered their one-shot import.
	if item.Resolution != nil || reviewartifact.TerminalStatus(item.Status) {
		item.Actions = nil
		return
	}
	if strings.TrimSpace(item.JSONPath) == "" {
		return
	}
	argv := []string{"compound", "capability", "import-surfacing-misses", item.JSONPath}
	item.Actions = []reviewartifact.Action{{
		ID:             "skill-surfacing.import",
		Label:          "Fold these misses into the capability graph",
		Command:        "compound capability import-surfacing-misses " + shellQuote(item.JSONPath),
		Argv:           argv,
		RequiresReview: true,
	}}
}

// ResolveSkillSurfacingMissesReviewItem closes the skill-surfacing review
// card that produced sourcePath, if sourcePath resolves to one under
// homePath/reviews/skill-surfacing: it records a "reviewed" generic
// resolution (internal/reviewartifact.ResolveQueueItem) so the import verb
// CLEARS the card out of the needs-you queue, the same mechanism
// `compound reviews resolve` uses. The verb also runs on arbitrary export
// files that are not review-queue items at all — those return resolved=false
// with no error. An item that already carries a resolution sidecar (a prior
// import already closed it) also returns resolved=false with no error —
// idempotent: no duplicate sidecar, no duplicate receipt.
func ResolveSkillSurfacingMissesReviewItem(homePath, sourcePath, actor string, now time.Time) (bool, reviewartifact.ReviewResolutionResult, error) {
	absPath, err := filepath.Abs(strings.TrimSpace(sourcePath))
	if err != nil {
		return false, reviewartifact.ReviewResolutionResult{}, err
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: skillSurfacingReviewArea}, now)
	if err != nil {
		return false, reviewartifact.ReviewResolutionResult{}, err
	}
	for _, item := range queue.Items {
		itemAbs, err := filepath.Abs(item.JSONPath)
		if err != nil || itemAbs != absPath {
			continue
		}
		if item.Resolution != nil {
			return false, reviewartifact.ReviewResolutionResult{}, nil
		}
		result, err := reviewartifact.ResolveQueueItem(homePath, item, reviewartifact.ResolutionInput{
			Decision: "reviewed",
			Reason:   "misses imported into the capability graph",
			Actor:    actor,
		}, now)
		if err != nil {
			return false, reviewartifact.ReviewResolutionResult{}, err
		}
		return true, result, nil
	}
	return false, reviewartifact.ReviewResolutionResult{}, nil
}
