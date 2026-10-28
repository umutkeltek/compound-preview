// Package panel is a judgment engine over existing candidates: perspectives
// generate decisions, and a panel verdict is evidence, never authority.
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
)

const RunSchemaVersion = "compound.panel_run.v0.1"
const SynthesisSchemaVersion = "compound.panel_synthesis.v0.1"

type Run struct {
	SchemaVersion string                `json:"schema_version"`
	ID            string                `json:"id"`
	CreatedAt     string                `json:"created_at"`
	Status        string                `json:"status"`
	WorkID        string                `json:"work_id,omitempty"`
	WorkBound     bool                  `json:"work_bound,omitempty"`
	TaskContract  envelope.TaskContract `json:"task_contract,omitzero"`
	ReceiptID     string                `json:"receipt_id,omitempty"`
	TransitionID  string                `json:"transition_id,omitempty"`
	TargetKind    string                `json:"target_kind"`
	Supervised    bool                  `json:"supervised"`
	Supervision   string                `json:"supervision,omitempty"`
	Targets       []Target              `json:"targets"`
	LensSet       LensSet               `json:"lens_set"`
	Route         RoutePlan             `json:"route"`
	Budget        Budget                `json:"budget"`
	Findings      []TargetFinding       `json:"findings"`
	CountsBefore  map[string]int        `json:"counts_before"`
	CountsAfter   map[string]int        `json:"counts_after"`
	EvidenceRefs  []string              `json:"evidence_refs"`
	StoppedReason string                `json:"stopped_reason,omitempty"`
	NextAction    string                `json:"next_action,omitempty"`
}

type Target struct {
	Ref         string `json:"ref"`
	Privacy     string `json:"privacy"`
	StateBefore string `json:"state_before"`
}

type LensSet struct {
	Included []IncludedLens `json:"included"`
	Excluded []ExcludedLens `json:"excluded"`
}

type IncludedLens struct {
	Ref          string  `json:"ref"`
	Reason       string  `json:"reason"`
	Weight       float64 `json:"weight"`
	PromptSHA256 string  `json:"prompt_sha256"`
}

type ExcludedLens struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

type RoutePlan struct {
	AuthorityRef string        `json:"authority_ref"`
	LensPlanID   string        `json:"lens_plan_id"`
	LensLane     LaneRef       `json:"lens_lane"`
	JudgePlanID  string        `json:"judge_plan_id"`
	JudgeLane    LaneRef       `json:"judge_lane"`
	OmittedLanes []OmittedLane `json:"omitted_lanes"`
}

type LaneRef struct {
	ModelID    string                       `json:"model_id"`
	Resolution routeevidence.LaneResolution `json:"resolution"`
	Tier       string                       `json:"tier"`
	GateRole   string                       `json:"gate_role"`
}

type OmittedLane struct {
	ModelID string `json:"model_id"`
	Reason  string `json:"reason"`
}

type Budget struct {
	MaxItems       int   `json:"max_items"`
	MaxTotalTokens int   `json:"max_total_tokens"`
	MaxUSDMicros   int64 `json:"max_usd_micros"`
}

func (b Budget) Validate() error {
	var errs []string
	if b.MaxItems < 0 {
		errs = append(errs, "max_items must be non-negative")
	}
	if b.MaxTotalTokens < 0 {
		errs = append(errs, "max_total_tokens must be non-negative")
	}
	if b.MaxUSDMicros < 0 {
		errs = append(errs, "max_usd_micros must be non-negative")
	}
	if len(errs) > 0 {
		return fmt.Errorf("panel budget invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

type TargetFinding struct {
	TargetRef       string        `json:"target_ref"`
	LensFindings    []LensFinding `json:"lens_findings"`
	Synthesis       Synthesis     `json:"synthesis"`
	VerdictArtifact string        `json:"verdict_artifact"`
	Application     Application   `json:"application"`
}

type LensFinding struct {
	LensRef          string    `json:"lens_ref"`
	ModelID          string    `json:"model_id"`
	LaneTier         string    `json:"lane_tier"`
	RouteEvidenceRef string    `json:"route_evidence_ref"`
	Confidence       float64   `json:"confidence"`
	LatencyMS        int64     `json:"latency_ms"`
	Usage            Usage     `json:"usage"`
	Findings         []Finding `json:"findings"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
}

type Usage struct {
	TotalTokens int `json:"total_tokens"`
}

type Finding struct {
	Claim        string   `json:"claim"`
	Severity     string   `json:"severity"`
	EvidenceRefs []string `json:"evidence_refs"`
}

type Synthesis struct {
	SchemaVersion         string    `json:"schema_version"`
	Decision              string    `json:"decision"`
	Confidence            float64   `json:"confidence"`
	Rationale             string    `json:"rationale"`
	Dissent               []Dissent `json:"dissent"`
	GuardrailsApplied     []string  `json:"guardrails_applied"`
	JudgeRouteEvidenceRef string    `json:"judge_route_evidence_ref"`
	JudgeUsage            Usage     `json:"judge_usage"`
}

type Dissent struct {
	LensRef  string `json:"lens_ref"`
	Position string `json:"position"`
	Note     string `json:"note"`
}

type Application struct {
	Applied           bool   `json:"applied"`
	MappedVerdict     string `json:"mapped_verdict"`
	ActionIntentRef   string `json:"action_intent_ref"`
	PolicyDecisionRef string `json:"policy_decision_ref"`
	ClaimRef          string `json:"claim_ref,omitempty"`
	CandidateRef      string `json:"candidate_ref,omitempty"`
	NoPromoteRef      string `json:"no_promote_ref"`
	TransitionRef     string `json:"transition_ref"`
	Note              string `json:"note,omitempty"`
}

type Store struct{ home string }

func Open(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	store := &Store{home: homePath}
	if err := os.MkdirAll(store.runDir(), 0o755); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) SaveRun(run Run) error {
	if err := run.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.runDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&run, "", " ")
	if err != nil {
		return err
	}
	// The run record is the resume anchor; a torn write would strand the run.
	tmp := s.runPath(run.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.runPath(run.ID))
}

func (s *Store) LoadRun(id string) (Run, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Run{}, fmt.Errorf("run id required")
	}
	data, err := os.ReadFile(s.runPath(id))
	if err != nil {
		return Run{}, err
	}
	return Decode(data)
}

func (s *Store) ListRuns() ([]Run, error) {
	entries, err := os.ReadDir(s.runDir())
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	runs := []Run{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.runDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		run, err := Decode(data)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (s *Store) runDir() string           { return filepath.Join(s.home, "panel-runs") }
func (s *Store) runPath(id string) string { return filepath.Join(s.runDir(), id+".json") }

func Decode(data []byte) (Run, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var run Run
	if err := dec.Decode(&run); err != nil {
		return Run{}, fmt.Errorf("decode panel run: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Run{}, fmt.Errorf("decode panel run: trailing JSON")
		}
		return Run{}, fmt.Errorf("decode panel run trailing JSON: %w", err)
	}
	if err := run.Validate(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (r Run) Validate() error {
	var errs []string
	if r.SchemaVersion != RunSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version must be %q", RunSchemaVersion))
	}
	if strings.TrimSpace(r.ID) == "" {
		errs = append(errs, "id required")
	} else if !strings.HasPrefix(r.ID, "prun_") {
		errs = append(errs, "id must start with prun_")
	}
	if strings.TrimSpace(r.CreatedAt) == "" {
		errs = append(errs, "created_at required")
	} else if _, err := time.Parse(time.RFC3339, r.CreatedAt); err != nil {
		errs = append(errs, "created_at must be RFC3339")
	} else if !strings.HasSuffix(r.CreatedAt, "Z") {
		errs = append(errs, "created_at must be UTC")
	}
	if strings.TrimSpace(r.Status) == "" {
		errs = append(errs, "status required")
	} else if !has(panelStatuses, r.Status) {
		errs = append(errs, fmt.Sprintf("status must be one of %s", keys(panelStatuses)))
	}
	if r.WorkBound && strings.TrimSpace(r.WorkID) == "" {
		errs = append(errs, "work_id required when work_bound")
	}
	if err := r.TaskContract.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	if strings.TrimSpace(r.TargetKind) == "" {
		errs = append(errs, "target_kind required")
	} else if !has(targetKinds, r.TargetKind) {
		errs = append(errs, fmt.Sprintf("target_kind must be one of %s", keys(targetKinds)))
	}
	if err := r.Budget.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	for i, finding := range r.Findings {
		for j, lens := range finding.LensFindings {
			if strings.TrimSpace(lens.Status) == "" {
				errs = append(errs, fmt.Sprintf("findings[%d].lens_findings[%d].status required", i, j))
			} else if !has(lensFindingStatuses, lens.Status) {
				errs = append(errs, fmt.Sprintf("findings[%d].lens_findings[%d].status must be one of %s", i, j, keys(lensFindingStatuses)))
			}
		}
		if finding.Synthesis.present() {
			if finding.Synthesis.SchemaVersion != SynthesisSchemaVersion {
				errs = append(errs, fmt.Sprintf("findings[%d].synthesis.schema_version must be %q", i, SynthesisSchemaVersion))
			}
			if strings.TrimSpace(finding.Synthesis.Decision) == "" {
				errs = append(errs, fmt.Sprintf("findings[%d].synthesis.decision required", i))
			} else if !has(synthesisDecisions, finding.Synthesis.Decision) {
				errs = append(errs, fmt.Sprintf("findings[%d].synthesis.decision must be one of %s", i, keys(synthesisDecisions)))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("panel run invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (s Synthesis) present() bool {
	return strings.TrimSpace(s.SchemaVersion) != "" ||
		strings.TrimSpace(s.Decision) != "" ||
		s.Confidence != 0 ||
		strings.TrimSpace(s.Rationale) != "" ||
		len(s.Dissent) > 0 ||
		len(s.GuardrailsApplied) > 0 ||
		strings.TrimSpace(s.JudgeRouteEvidenceRef) != ""
}

var panelStatuses = map[string]bool{
	"planned":           true,
	"running":           true,
	"verdicts_recorded": true,
	"applied":           true,
	"blocked":           true,
	"stopped_budget":    true,
	"failed":            true,
}

var targetKinds = map[string]bool{
	"memory_candidate":       true,
	"skill_candidate":        true,
	"capability_observation": true,
	"decision_candidate":     true,
	"comparison":             true,
	"external_repo":          true,
}

var lensFindingStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"skipped":   true,
}

var synthesisDecisions = map[string]bool{
	"promote":        true,
	"park":           true,
	"reject":         true,
	"merge_existing": true,
	"needs_human":    true,
	"needs_trial":    true,
}

func has(set map[string]bool, value string) bool { return set[value] }

func keys(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}
