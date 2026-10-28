package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ReleaseGoalHoldsAuthorityRef            = "compound://registries/release-goal-holds"
	ReleaseGoalHoldsSourceAuthoritySchema   = "compound.release_goal_holds_source_authority.v0.1"
	releaseGoalHoldsFile                    = "release-goal-holds.json"
	releaseGoalHoldsSourceAuthorityFile     = "release-goal-holds-source-authority.jsonl"
	releaseGoalHoldsImportedVia             = "policy import-release-goal-holds"
	DefaultReleaseGoalHoldsGateLive         = "2026-06-10T12:00"
	defaultReleaseGoalHoldsLimit            = 25
	releaseGoalHoldsAuthoritySchema         = "compound.release_goal_holds_authority.v0.1"
	releaseGoalHoldsGeneratedViewRetirement = "Release-goal HOLD reads compound://registries/release-goal-holds; release orchestrator sqlite remains approval write owner and rollback/evidence input"
	releaseGoalHoldsGeneratedViewRollback   = "re-import release-orchestrator-state.sqlite or remove COMPOUND_HOME registries/release-goal-holds.json"
)

type ReleaseGoalHoldsAuthority struct {
	SchemaVersion string            `json:"schema_version"`
	GeneratedAt   string            `json:"generated_at"`
	AuthorityRef  string            `json:"authority_ref"`
	SourceDB      string            `json:"source_db"`
	GateLive      string            `json:"gate_live"`
	Limit         int               `json:"limit"`
	HoldCount     int               `json:"hold_count"`
	Holds         []ReleaseGoalHold `json:"holds"`
}

type ReleaseGoalHold struct {
	GoalID      string `json:"goal_id"`
	AppID       string `json:"app_id"`
	Objective   string `json:"objective"`
	State       string `json:"state"`
	ArtifactDir string `json:"artifact_dir,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type ReleaseGoalHoldsImportReport struct {
	AuthorityRef      string                                `json:"authority_ref"`
	AuthorityPath     string                                `json:"authority_path"`
	SourceAuthorityID string                                `json:"source_authority_id"`
	SourceAuthority   ReleaseGoalHoldsSourceAuthorityRecord `json:"source_authority"`
	GateLive          string                                `json:"gate_live"`
	HoldCount         int                                   `json:"hold_count"`
	HoldIDs           []string                              `json:"hold_ids"`
}

type ReleaseGoalHoldsSourceAuthorityRecord struct {
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
	GateLive            string   `json:"gate_live"`
	HoldIDs             []string `json:"hold_ids"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func ImportReleaseGoalHolds(homePath, sourceDB, gateLive string, now time.Time) (ReleaseGoalHoldsImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return ReleaseGoalHoldsImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourceDB) == "" {
		return ReleaseGoalHoldsImportReport{}, fmt.Errorf("source sqlite path required")
	}
	if strings.TrimSpace(gateLive) == "" {
		gateLive = DefaultReleaseGoalHoldsGateLive
	}
	data, err := os.ReadFile(sourceDB)
	if err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	holds, err := LoadReleaseGoalHoldsFromSQLite(sourceDB, gateLive, defaultReleaseGoalHoldsLimit)
	if err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	authority := ReleaseGoalHoldsAuthority{
		SchemaVersion: releaseGoalHoldsAuthoritySchema,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		AuthorityRef:  ReleaseGoalHoldsAuthorityRef,
		SourceDB:      sourceDB,
		GateLive:      gateLive,
		Limit:         defaultReleaseGoalHoldsLimit,
		HoldCount:     len(holds),
		Holds:         holds,
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	encoded, err := json.MarshalIndent(authority, "", "  ")
	if err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	authorityPath := ReleaseGoalHoldsPath(homePath)
	if err := os.WriteFile(authorityPath, append(encoded, '\n'), 0o644); err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	sourceRevision := digestHex(data)
	holdIDs := releaseGoalHoldIDs(holds)
	rec := ReleaseGoalHoldsSourceAuthorityRecord{
		SchemaVersion:       ReleaseGoalHoldsSourceAuthoritySchema,
		ID:                  releaseGoalHoldsSourceAuthorityID(sourceDB, sourceRevision, gateLive),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourceDB,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         releaseGoalHoldsImportedVia,
		AuthorityRef:        ReleaseGoalHoldsAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "sqlite_read_authority:" + sourceDB,
		RetirementCondition: releaseGoalHoldsGeneratedViewRetirement,
		Rollback:            releaseGoalHoldsGeneratedViewRollback,
		GateLive:            gateLive,
		HoldIDs:             holdIDs,
		EvidenceRefs:        []string{"sha256:" + sourceRevision, ReleaseGoalHoldsAuthorityRef},
	}
	if err := appendReleaseGoalHoldsSourceAuthority(homePath, rec); err != nil {
		return ReleaseGoalHoldsImportReport{}, err
	}
	return ReleaseGoalHoldsImportReport{
		AuthorityRef:      ReleaseGoalHoldsAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		GateLive:          gateLive,
		HoldCount:         len(holds),
		HoldIDs:           holdIDs,
	}, nil
}

func LoadReleaseGoalHoldsFromSQLite(sourceDB, gateLive string, limit int) ([]ReleaseGoalHold, error) {
	if limit <= 0 {
		limit = defaultReleaseGoalHoldsLimit
	}
	query := fmt.Sprintf(
		"select goal_id, app_id, objective, state, artifact_dir, created_at, updated_at from goals where state='HOLD' and updated_at >= %s order by updated_at desc limit %d;",
		sqlQuote(gateLive),
		limit,
	)
	out, err := exec.Command("sqlite3", "-json", sourceDB, query).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("query release-goal holds with sqlite3: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var holds []ReleaseGoalHold
	if len(strings.TrimSpace(string(out))) == 0 {
		return holds, nil
	}
	if err := json.Unmarshal(out, &holds); err != nil {
		return nil, fmt.Errorf("parse sqlite release-goal holds json: %w", err)
	}
	for i, hold := range holds {
		if strings.TrimSpace(hold.GoalID) == "" {
			return nil, fmt.Errorf("release-goal HOLD row %d missing goal_id", i)
		}
		if strings.TrimSpace(hold.AppID) == "" {
			return nil, fmt.Errorf("release-goal HOLD row %q missing app_id", hold.GoalID)
		}
		if strings.TrimSpace(hold.State) != "HOLD" {
			return nil, fmt.Errorf("release-goal row %q has non-HOLD state %q", hold.GoalID, hold.State)
		}
	}
	return holds, nil
}

func LoadReleaseGoalHoldsFromHome(homePath string) (ReleaseGoalHoldsAuthority, error) {
	data, err := os.ReadFile(ReleaseGoalHoldsPath(homePath))
	if err != nil {
		return ReleaseGoalHoldsAuthority{}, err
	}
	var authority ReleaseGoalHoldsAuthority
	if err := json.Unmarshal(data, &authority); err != nil {
		return ReleaseGoalHoldsAuthority{}, fmt.Errorf("parse release-goal holds authority: %w", err)
	}
	return authority, nil
}

func ReleaseGoalHoldsPath(homePath string) string {
	return filepath.Join(homePath, "registries", releaseGoalHoldsFile)
}

func ReleaseGoalHoldsSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", releaseGoalHoldsSourceAuthorityFile)
}

func LoadReleaseGoalHoldsSourceAuthority(homePath string) ([]ReleaseGoalHoldsSourceAuthorityRecord, error) {
	data, err := os.ReadFile(ReleaseGoalHoldsSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []ReleaseGoalHoldsSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec ReleaseGoalHoldsSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse release-goal holds source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreatedAt < records[j].CreatedAt
	})
	return records, nil
}

func appendReleaseGoalHoldsSourceAuthority(homePath string, rec ReleaseGoalHoldsSourceAuthorityRecord) error {
	path := ReleaseGoalHoldsSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, err := LoadReleaseGoalHoldsSourceAuthority(homePath)
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
	_, err = f.Write(append(data, '\n'))
	return err
}

func releaseGoalHoldIDs(holds []ReleaseGoalHold) []string {
	ids := make([]string, 0, len(holds))
	for _, hold := range holds {
		if id := strings.TrimSpace(hold.GoalID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func releaseGoalHoldsSourceAuthorityID(path, revision, gateLive string) string {
	sum := digestHex([]byte(path + "\x00" + revision + "\x00" + gateLive))
	return "rghsrc_" + sum[:12]
}

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
