package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	OperatorApprovalFrontierAuthorityRef          = "compound://registries/operator-approval-frontier"
	OperatorApprovalFrontierSourceAuthoritySchema = "compound.operator_approval_frontier_source_authority.v0.1"
	operatorApprovalFrontierFile                  = "operator-approval-frontier.json"
	operatorApprovalFrontierSourceAuthorityFile   = "operator-approval-frontier-source-authority.jsonl"
	operatorApprovalFrontierImportedVia           = "policy import-operator-approval-frontier"
)

type OperatorApprovalFrontier struct {
	SchemaVersion             int                             `json:"schema_version,omitempty"`
	GeneratedAt               string                          `json:"generated_at,omitempty"`
	RunID                     string                          `json:"run_id,omitempty"`
	Status                    string                          `json:"status,omitempty"`
	AuthorityBoundary         string                          `json:"authority_boundary,omitempty"`
	CurrentState              map[string]interface{}          `json:"current_state,omitempty"`
	DecisionFamilyCount       int                             `json:"decision_family_count,omitempty"`
	ApprovalRequiredCount     int                             `json:"approval_required_count,omitempty"`
	ApprovalRequiredOpenCount int                             `json:"approval_required_open_count,omitempty"`
	ConsumedFamilies          []map[string]interface{}        `json:"consumed_families,omitempty"`
	Families                  []OperatorApprovalFamily        `json:"families,omitempty"`
	ActionApprovalCount       int                             `json:"action_approval_count,omitempty"`
	ActionApprovals           []OperatorActionApprovalRequest `json:"action_approvals,omitempty"`
	ActionApprovalsSeeded     bool                            `json:"action_approvals_seeded,omitempty"`
	ActionApprovalsSeededAt   string                          `json:"action_approvals_seeded_at,omitempty"`
	Raw                       map[string]interface{}          `json:"-"`
}

type OperatorApprovalFamily struct {
	ID                       string   `json:"id"`
	ReviewOrder              int      `json:"review_order,omitempty"`
	EntryCount               int      `json:"entry_count,omitempty"`
	CurrentDecision          string   `json:"current_decision,omitempty"`
	RecommendedDecision      string   `json:"recommended_decision,omitempty"`
	ExactOperatorPhrase      string   `json:"exact_operator_phrase,omitempty"`
	OperatorChoicePending    bool     `json:"operator_choice_pending,omitempty"`
	ApprovalGrantedByThisRun bool     `json:"approval_granted_by_this_run,omitempty"`
	GatedActionMutated       bool     `json:"gated_action_mutated,omitempty"`
	NextAction               string   `json:"next_action,omitempty"`
	SourcePaths              []string `json:"source_paths,omitempty"`
	Consumed                 bool     `json:"consumed,omitempty"`
	ConsumedBy               string   `json:"consumed_by,omitempty"`
}

type OperatorApprovalFrontierImportReport struct {
	AuthorityRef              string                                        `json:"authority_ref"`
	AuthorityPath             string                                        `json:"authority_path"`
	SourceAuthorityID         string                                        `json:"source_authority_id"`
	SourceAuthority           OperatorApprovalFrontierSourceAuthorityRecord `json:"source_authority"`
	GeneratedAt               string                                        `json:"generated_at,omitempty"`
	RunID                     string                                        `json:"run_id,omitempty"`
	Status                    string                                        `json:"status,omitempty"`
	FamilyCount               int                                           `json:"family_count"`
	DeferredCount             int                                           `json:"deferred_count"`
	ApprovalRequiredOpenCount int                                           `json:"approval_required_open_count,omitempty"`
}

type OperatorApprovalFrontierSourceAuthorityRecord struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	FamilyIDs           []string `json:"family_ids,omitempty"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func LoadOperatorApprovalFrontier(path string) (OperatorApprovalFrontier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OperatorApprovalFrontier{}, err
	}
	return parseOperatorApprovalFrontier(data)
}

func parseOperatorApprovalFrontier(data []byte) (OperatorApprovalFrontier, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return OperatorApprovalFrontier{}, fmt.Errorf("parse operator approval frontier: %w", err)
	}
	var frontier OperatorApprovalFrontier
	if err := json.Unmarshal(data, &frontier); err != nil {
		return OperatorApprovalFrontier{}, fmt.Errorf("parse operator approval frontier: %w", err)
	}
	frontier.Raw = raw
	if strings.TrimSpace(frontier.Status) == "" {
		return OperatorApprovalFrontier{}, fmt.Errorf("operator approval frontier status required")
	}
	for i, family := range frontier.Families {
		if strings.TrimSpace(family.ID) == "" {
			return OperatorApprovalFrontier{}, fmt.Errorf("operator approval family %d missing id", i)
		}
	}
	return frontier, nil
}

func ImportOperatorApprovalFrontier(homePath, sourcePath string, now time.Time) (OperatorApprovalFrontierImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OperatorApprovalFrontierImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return OperatorApprovalFrontierImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return OperatorApprovalFrontierImportReport{}, err
	}
	frontier, err := parseOperatorApprovalFrontier(data)
	if err != nil {
		return OperatorApprovalFrontierImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return OperatorApprovalFrontierImportReport{}, err
	}
	authorityPath := OperatorApprovalFrontierPath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return OperatorApprovalFrontierImportReport{}, err
	}
	sourceRevision := digestHex(data)
	rec := OperatorApprovalFrontierSourceAuthorityRecord{
		SchemaVersion:       OperatorApprovalFrontierSourceAuthoritySchema,
		ID:                  operatorApprovalFrontierSourceAuthorityID(sourcePath, sourceRevision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         operatorApprovalFrontierImportedVia,
		AuthorityRef:        OperatorApprovalFrontierAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "Operator approval frontier reads compound://registries/operator-approval-frontier; source run artifact remains rollback/evidence input",
		Rollback:            "re-import source operator-approval-frontier-summary.json or remove COMPOUND_HOME registries/operator-approval-frontier.json",
		FamilyIDs:           operatorApprovalFamilyIDs(frontier),
		EvidenceRefs:        []string{"sha256:" + sourceRevision, OperatorApprovalFrontierAuthorityRef},
	}
	if err := appendOperatorApprovalFrontierSourceAuthority(homePath, rec); err != nil {
		return OperatorApprovalFrontierImportReport{}, err
	}
	return OperatorApprovalFrontierImportReport{
		AuthorityRef:              OperatorApprovalFrontierAuthorityRef,
		AuthorityPath:             authorityPath,
		SourceAuthorityID:         rec.ID,
		SourceAuthority:           rec,
		GeneratedAt:               frontier.GeneratedAt,
		RunID:                     frontier.RunID,
		Status:                    frontier.Status,
		FamilyCount:               len(frontier.Families),
		DeferredCount:             countDeferredApprovalFamilies(frontier.Families),
		ApprovalRequiredOpenCount: frontier.ApprovalRequiredOpenCount,
	}, nil
}

func LoadOperatorApprovalFrontierFromHome(homePath string) (OperatorApprovalFrontier, error) {
	return LoadOperatorApprovalFrontier(OperatorApprovalFrontierPath(homePath))
}

func OperatorApprovalFrontierPath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorApprovalFrontierFile)
}

func OperatorApprovalFrontierSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorApprovalFrontierSourceAuthorityFile)
}

func LoadOperatorApprovalFrontierSourceAuthority(homePath string) ([]OperatorApprovalFrontierSourceAuthorityRecord, error) {
	data, err := os.ReadFile(OperatorApprovalFrontierSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []OperatorApprovalFrontierSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec OperatorApprovalFrontierSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse operator approval frontier source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreatedAt < records[j].CreatedAt
	})
	return records, nil
}

func appendOperatorApprovalFrontierSourceAuthority(homePath string, rec OperatorApprovalFrontierSourceAuthorityRecord) error {
	path := OperatorApprovalFrontierSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, err := LoadOperatorApprovalFrontierSourceAuthority(homePath)
	if err != nil {
		return err
	}
	for _, old := range existing {
		if old.ID == rec.ID {
			return nil
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func operatorApprovalFamilyIDs(frontier OperatorApprovalFrontier) []string {
	var ids []string
	for _, family := range frontier.Families {
		if id := strings.TrimSpace(family.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func countDeferredApprovalFamilies(families []OperatorApprovalFamily) int {
	count := 0
	for _, family := range families {
		if strings.EqualFold(strings.TrimSpace(family.CurrentDecision), "defer") {
			count++
		}
	}
	return count
}

func operatorApprovalFrontierSourceAuthorityID(path, revision string) string {
	sum := digestHex([]byte(path + "\x00" + revision))
	return "opfrsrc_" + sum[:12]
}
