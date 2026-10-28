// Package intent is a thin coordinator over Compound's existing capture,
// create-plan, review-artifact, and admission owners. It owns no authority
// format: source bytes stay with mem, meaning resolution stays private in work,
// and apply delegates exactly-once admission back to work.
package intent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const (
	StatusPlanProposed    = "plan_proposed"
	StatusAmbiguityParked = "ambiguity_parked"
	StatusCaptureOnly     = "capture_only"

	ReviewKindPlan = "plan"
	ReviewKindPark = "park"

	parkSchemaVersion = "compound.intent_ambiguity_park.v0.1"
	parkArea          = "intent-ambiguity"
)

const operatorActNonGoal = "do not admit, claim, dispatch, advance, close, or mutate owner state without an operator act"

// Proposal is rented-model output. Deliberately absent are every authority-
// bearing identity and policy field; DecodeProposal rejects unknown fields.
type Proposal struct {
	DesiredOutcome   string               `json:"desired_outcome"`
	SuccessPredicate string               `json:"success_predicate"`
	NonGoals         []string             `json:"non_goals,omitempty"`
	ActionSummary    string               `json:"action_summary,omitempty"`
	EvidenceRefs     []string             `json:"evidence_refs,omitempty"`
	Uncertainty      envelope.Uncertainty `json:"uncertainty,omitempty"`
}

type PlanInput struct {
	Source           string
	Utterance        []byte
	ContextPackID    string
	RecalledRefs     []string
	Actor            string
	Owner            string
	CanonicalSurface string
	Proposal         Proposal
}

type PlanReport struct {
	Status     string           `json:"status"`
	CaptureCID string           `json:"capture_cid"`
	Plan       *work.CreatePlan `json:"plan,omitempty"`
	PlanPath   string           `json:"plan_path,omitempty"`
	Park       *ParkReport      `json:"park,omitempty"`
	ParkPath   string           `json:"park_path,omitempty"`
}

// ParkReport is evidence for a named ambiguity, not a second plan or queue.
// It lives through reviewartifact under the existing reviews tree.
type ParkReport struct {
	SchemaVersion  string   `json:"schema_version"`
	ID             string   `json:"id"`
	CreatedAt      string   `json:"created_at"`
	Status         string   `json:"status"`
	CaptureCID     string   `json:"capture_cid"`
	ContextPackID  string   `json:"context_pack_id"`
	AmbiguousRefs  []string `json:"ambiguous_refs"`
	NamedQuestions []string `json:"named_questions"`
	NextAction     string   `json:"next_action"`
}

type ReviewReport struct {
	Kind string           `json:"kind"`
	Plan *work.CreatePlan `json:"plan,omitempty"`
	Park *ParkReport      `json:"park,omitempty"`
}

func DecodeProposal(data []byte) (Proposal, error) {
	var proposal Proposal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return Proposal{}, fmt.Errorf("decode proposal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Proposal{}, fmt.Errorf("decode proposal: %w", err)
	}
	return proposal, nil
}

func Plan(homePath string, in PlanInput, now time.Time) (PlanReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return PlanReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if len(in.Utterance) == 0 {
		return PlanReport{}, fmt.Errorf("utterance bytes required")
	}
	if strings.TrimSpace(in.Proposal.DesiredOutcome) == "" || strings.TrimSpace(in.Proposal.SuccessPredicate) == "" {
		return PlanReport{}, fmt.Errorf("proposal desired_outcome and success_predicate required")
	}

	memory, err := mem.Open(homePath)
	if err != nil {
		return PlanReport{}, err
	}
	cid, _, err := memory.CaptureExhaust(firstNonEmpty(in.Source, "expressive-intent"), "", string(in.Utterance), now)
	if err != nil {
		return PlanReport{}, fmt.Errorf("capture utterance: %w", err)
	}
	report := PlanReport{Status: StatusCaptureOnly, CaptureCID: cid}

	recalled := uniqueStrings(in.RecalledRefs)
	allowed := map[string]bool{cid: true, "capture:" + cid: true}
	for _, ref := range recalled {
		allowed[ref] = true
	}
	for _, ref := range in.Proposal.EvidenceRefs {
		if !allowed[strings.TrimSpace(ref)] {
			return PlanReport{}, fmt.Errorf("proposal evidence ref %q is outside capture and recalled refs", ref)
		}
	}

	store, err := work.Open(homePath)
	if err != nil {
		return PlanReport{}, err
	}
	nonGoals := uniqueStrings(append(append([]string{}, in.Proposal.NonGoals...), operatorActNonGoal))
	evidenceRefs := uniqueStrings(append([]string{cid}, recalled...))
	plan, err := store.CreatePlan(work.CreateInput{
		Owner:            firstNonEmpty(in.Owner, "compound-core"),
		Actor:            firstNonEmpty(in.Actor, "compound"),
		CanonicalSurface: firstNonEmpty(in.CanonicalSurface, "compound://intent/expressive"),
		SuccessPredicate: strings.TrimSpace(in.Proposal.SuccessPredicate),
		DesiredOutcome:   strings.TrimSpace(in.Proposal.DesiredOutcome),
		NonGoals:         nonGoals,
		EvidenceRefs:     evidenceRefs,
		ContextPackID:    strings.TrimSpace(in.ContextPackID),
		State:            "proposed",
		Privacy:          "private",
		Risk:             "tier1",
		Uncertainty:      in.Proposal.Uncertainty,
	}, now, work.CreatePlanOptions{Actor: firstNonEmpty(in.Actor, "compound"), Record: false})
	if err != nil {
		return PlanReport{}, err
	}

	switch plan.MeaningResolution {
	case "none":
		return report, nil
	case "ambiguous":
		park, path, err := recordPark(homePath, cid, in.ContextPackID, plan.AmbiguousRefs, now)
		if err != nil {
			return PlanReport{}, err
		}
		report.Status, report.Park, report.ParkPath = StatusAmbiguityParked, &park, path
		return report, nil
	case "resolved":
		if plan.ActionContext == nil {
			return PlanReport{}, fmt.Errorf("resolved promoted meaning is missing action context")
		}
		if proposed := strings.TrimSpace(in.Proposal.ActionSummary); proposed != "" && proposed != strings.TrimSpace(plan.ActionContext.ActionSummary) {
			return PlanReport{}, fmt.Errorf("proposal action_summary does not match promoted exact action")
		}
	default:
		return PlanReport{}, fmt.Errorf("unknown work meaning resolution %q", plan.MeaningResolution)
	}

	path, err := store.RecordCreatePlan(plan)
	if err != nil {
		return PlanReport{}, err
	}
	plan.PlanPath = path
	report.Status, report.Plan, report.PlanPath = StatusPlanProposed, &plan, path
	return report, nil
}

func Review(path string) (ReviewReport, error) {
	if isParkPath(path) {
		park, err := loadPark(path)
		if err != nil {
			return ReviewReport{}, err
		}
		return ReviewReport{Kind: ReviewKindPark, Park: &park}, nil
	}
	plan, err := work.LoadCreatePlan(path)
	if err != nil {
		return ReviewReport{}, err
	}
	return ReviewReport{Kind: ReviewKindPlan, Plan: &plan}, nil
}

func Apply(homePath, path, reviewedBy string, now time.Time) (work.AdmitCreatePlanReport, error) {
	if strings.TrimSpace(reviewedBy) == "" {
		return work.AdmitCreatePlanReport{}, fmt.Errorf("reviewed-by is required before apply")
	}
	if isParkPath(path) {
		return work.AdmitCreatePlanReport{}, fmt.Errorf("ambiguity park is not a create plan and cannot be admitted")
	}
	store, err := work.Open(homePath)
	if err != nil {
		return work.AdmitCreatePlanReport{}, err
	}
	return store.AdmitCreatePlanPath(path, now, work.AdmitCreatePlanOptions{
		Actor: strings.TrimSpace(reviewedBy),
		Note:  "reviewed expressive intent admitted through existing create-plan owner",
	})
}

func recordPark(homePath, cid, contextPackID string, refs []string, now time.Time) (ParkReport, string, error) {
	refs = uniqueStrings(refs)
	if len(refs) < 2 {
		return ParkReport{}, "", fmt.Errorf("ambiguity park requires at least two named refs")
	}
	park := ParkReport{
		SchemaVersion: parkSchemaVersion,
		ID:            contentID("ipark_", cid, strings.Join(refs, "|"), now.UTC().Format(time.RFC3339Nano)),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        StatusAmbiguityParked,
		CaptureCID:    cid,
		ContextPackID: strings.TrimSpace(contextPackID),
		AmbiguousRefs: refs,
		NamedQuestions: []string{
			"Which promoted direction should govern this utterance: " + strings.Join(refs, " or ") + "?",
		},
		NextAction: "clarify by speaking again, then create a fresh plan; waiting or apply never turns this park into work",
	}
	markdown := fmt.Sprintf("# Expressive intent ambiguity\n\n- capture: `%s`\n- candidates: `%s`\n\n%s\n", cid, strings.Join(refs, "`, `"), park.NamedQuestions[0])
	paths, err := reviewartifact.Write(homePath, parkArea, park.ID, park, "  ", markdown)
	if err != nil {
		return ParkReport{}, "", err
	}
	return park, paths.PlanPath, nil
}

func loadPark(path string) (ParkReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ParkReport{}, err
	}
	var park ParkReport
	if err := json.Unmarshal(data, &park); err != nil {
		return ParkReport{}, err
	}
	if park.SchemaVersion != parkSchemaVersion || park.Status != StatusAmbiguityParked || len(park.AmbiguousRefs) < 2 {
		return ParkReport{}, fmt.Errorf("invalid expressive intent park")
	}
	return park, nil
}

func isParkPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains(clean, "/reviews/"+parkArea+"/")
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func contentID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
