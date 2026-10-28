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
	OperatorPromptsAuthorityRef          = "compound://registries/operator-decision-prompts"
	OperatorPromptsSourceAuthoritySchema = "compound.operator_prompts_source_authority.v0.1"
	operatorPromptsFile                  = "operator-decision-prompts.json"
	operatorPromptsSourceAuthorityFile   = "operator-prompts-source-authority.jsonl"
	operatorPromptsImportedVia           = "policy import-operator-prompts"
)

type OperatorPromptRegistry struct {
	Version        int                    `json:"version,omitempty"`
	GeneratedAt    string                 `json:"generated_at,omitempty"`
	SourceGovernor string                 `json:"source_governor,omitempty"`
	DeliveryMode   string                 `json:"delivery_mode,omitempty"`
	Prompts        []OperatorPrompt       `json:"prompts,omitempty"`
	LastResolved   []map[string]any       `json:"last_resolved,omitempty"`
	Raw            map[string]interface{} `json:"-"`
}

type OperatorPrompt struct {
	ID                string   `json:"id"`
	Topic             string   `json:"topic,omitempty"`
	Target            string   `json:"target,omitempty"`
	Severity          string   `json:"severity,omitempty"`
	Question          string   `json:"question"`
	Action            string   `json:"action,omitempty"`
	DefaultIfNoAnswer string   `json:"default_if_no_answer,omitempty"`
	RecommendedLane   string   `json:"recommended_lane,omitempty"`
	Source            string   `json:"source,omitempty"`
	GeneratedAt       string   `json:"generated_at,omitempty"`
	ReplyOptions      []string `json:"reply_options,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
}

type OperatorPromptsImportReport struct {
	AuthorityRef      string                               `json:"authority_ref"`
	AuthorityPath     string                               `json:"authority_path"`
	SourceAuthorityID string                               `json:"source_authority_id"`
	SourceAuthority   OperatorPromptsSourceAuthorityRecord `json:"source_authority"`
	GeneratedAt       string                               `json:"generated_at,omitempty"`
	PromptCount       int                                  `json:"prompt_count"`
	HighSeverityCount int                                  `json:"high_severity_count"`
}

type OperatorPromptsSourceAuthorityRecord struct {
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
	PromptIDs           []string `json:"prompt_ids,omitempty"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func LoadOperatorPrompts(path string) (OperatorPromptRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OperatorPromptRegistry{}, err
	}
	return parseOperatorPrompts(data)
}

func parseOperatorPrompts(data []byte) (OperatorPromptRegistry, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return OperatorPromptRegistry{}, fmt.Errorf("parse operator prompts: %w", err)
	}
	var registry OperatorPromptRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return OperatorPromptRegistry{}, fmt.Errorf("parse operator prompts: %w", err)
	}
	registry.Raw = raw
	for i, prompt := range registry.Prompts {
		if strings.TrimSpace(prompt.ID) == "" {
			return OperatorPromptRegistry{}, fmt.Errorf("operator prompt %d missing id", i)
		}
		if strings.TrimSpace(prompt.Question) == "" {
			return OperatorPromptRegistry{}, fmt.Errorf("operator prompt %q missing question", prompt.ID)
		}
	}
	return registry, nil
}

func ImportOperatorPrompts(homePath, sourcePath string, now time.Time) (OperatorPromptsImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OperatorPromptsImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return OperatorPromptsImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return OperatorPromptsImportReport{}, err
	}
	registry, err := parseOperatorPrompts(data)
	if err != nil {
		return OperatorPromptsImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return OperatorPromptsImportReport{}, err
	}
	authorityPath := OperatorPromptsPath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return OperatorPromptsImportReport{}, err
	}
	sourceRevision := digestHex(data)
	rec := OperatorPromptsSourceAuthorityRecord{
		SchemaVersion:       OperatorPromptsSourceAuthoritySchema,
		ID:                  operatorPromptsSourceAuthorityID(sourcePath, sourceRevision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         operatorPromptsImportedVia,
		AuthorityRef:        OperatorPromptsAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "Operator prompt cards read compound://registries/operator-decision-prompts; source view remains rollback/evidence input",
		Rollback:            "re-import source operator-decision-prompts.json or remove COMPOUND_HOME registries/operator-decision-prompts.json",
		PromptIDs:           operatorPromptIDs(registry),
		EvidenceRefs:        []string{"sha256:" + sourceRevision, OperatorPromptsAuthorityRef},
	}
	if err := appendOperatorPromptsSourceAuthority(homePath, rec); err != nil {
		return OperatorPromptsImportReport{}, err
	}
	return OperatorPromptsImportReport{
		AuthorityRef:      OperatorPromptsAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		GeneratedAt:       registry.GeneratedAt,
		PromptCount:       len(registry.Prompts),
		HighSeverityCount: countHighSeverityPrompts(registry.Prompts),
	}, nil
}

func LoadOperatorPromptsFromHome(homePath string) (OperatorPromptRegistry, error) {
	return LoadOperatorPrompts(OperatorPromptsPath(homePath))
}

func OperatorPromptsPath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorPromptsFile)
}

func OperatorPromptsSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorPromptsSourceAuthorityFile)
}

func LoadOperatorPromptsSourceAuthority(homePath string) ([]OperatorPromptsSourceAuthorityRecord, error) {
	data, err := os.ReadFile(OperatorPromptsSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []OperatorPromptsSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec OperatorPromptsSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse operator prompts source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreatedAt < records[j].CreatedAt
	})
	return records, nil
}

func appendOperatorPromptsSourceAuthority(homePath string, rec OperatorPromptsSourceAuthorityRecord) error {
	path := OperatorPromptsSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, err := LoadOperatorPromptsSourceAuthority(homePath)
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

func operatorPromptIDs(registry OperatorPromptRegistry) []string {
	var ids []string
	for _, prompt := range registry.Prompts {
		if id := strings.TrimSpace(prompt.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func countHighSeverityPrompts(prompts []OperatorPrompt) int {
	count := 0
	for _, prompt := range prompts {
		if strings.EqualFold(strings.TrimSpace(prompt.Severity), "high") {
			count++
		}
	}
	return count
}

func operatorPromptsSourceAuthorityID(path, revision string) string {
	sum := digestHex([]byte(path + "\x00" + revision))
	return "oppsrc_" + sum[:12]
}
