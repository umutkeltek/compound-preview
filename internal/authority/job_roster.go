package authority

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
)

const (
	JobRosterAuthorityRef          = "compound://registries/job-roster"
	JobRosterAuthoritySchema       = "compound.job_roster_authority.v0.1"
	JobRosterSourceAuthoritySchema = "compound.job_roster_source_authority.v0.1"

	jobRosterFile                = "job-roster.json"
	jobRosterSourceAuthorityFile = "job-roster-source-authority.jsonl"
	jobRosterImportedVia         = "authority import-job-roster"
	jobTokenImportedVia          = "authority import-job-token-governance"
	jobRosterRetirement          = "job status reads compound://registries/job-roster; source job registry remains scheduler write owner and rollback/evidence input"
	jobRosterRollback            = "re-import source job registry or remove COMPOUND_HOME registries/job-roster.json after review"
	jobTokenRetirement           = "job token governance reads compound://registries/job-roster; source token usage view remains rollback/evidence input"
	jobTokenRollback             = "re-import source token usage view or remove token fields from COMPOUND_HOME registries/job-roster.json after review"
)

type JobRosterAuthority struct {
	SchemaVersion      string       `json:"schema_version"`
	GeneratedAt        string       `json:"generated_at"`
	AuthorityRef       string       `json:"authority_ref"`
	SourcePath         string       `json:"source_path"`
	SourceRevision     string       `json:"source_revision"`
	JobCount           int          `json:"job_count"`
	EnabledCount       int          `json:"enabled_count"`
	RecentRunCount     int          `json:"recent_run_count"`
	ErrorCount         int          `json:"error_count"`
	ActiveErrorCount   int          `json:"active_error_count"`
	InactiveErrorCount int          `json:"inactive_error_count,omitempty"`
	Jobs               []JobSummary `json:"jobs"`
	RecentErrors       []JobSummary `json:"recent_errors,omitempty"`
	ActiveErrors       []JobSummary `json:"active_errors,omitempty"`
	InactiveErrors     []JobSummary `json:"inactive_errors,omitempty"`

	TokenUsageSourcePath     string       `json:"token_usage_source_path,omitempty"`
	TokenUsageSourceRevision string       `json:"token_usage_source_revision,omitempty"`
	TokenMetric              string       `json:"token_metric,omitempty"`
	TokenSessionsCount       int          `json:"token_sessions_count"`
	EnabledAgentCount        int          `json:"enabled_agent_count"`
	HighTokenEnabledCount    int          `json:"high_token_enabled_count"`
	TokenRiskJobs            []JobSummary `json:"token_risk_jobs,omitempty"`

	// ProbeRefreshedAt records the last time a probe-refresh collector run
	// (jobprobe) re-observed current unit liveness for declared probe targets
	// and wrote the observed state back onto rows below. It is empty on a plain
	// source import; the imported snapshot is a blind claim until a probe pass
	// re-observes it.
	ProbeRefreshedAt string `json:"probe_refreshed_at,omitempty"`
}

type JobSummary struct {
	JobID             string `json:"job_id,omitempty"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	State             string `json:"state,omitempty"`
	Schedule          string `json:"schedule,omitempty"`
	Deliver           string `json:"deliver,omitempty"`
	Script            string `json:"script,omitempty"`
	NoAgent           bool   `json:"no_agent,omitempty"`
	Lane              string `json:"lane,omitempty"`
	AccountableOwner  string `json:"accountable_owner"`
	Harness           string `json:"harness"`
	RequestedModel    string `json:"requested_model"`
	ProviderRoute     string `json:"provider_route"`
	IdentitySource    string `json:"identity_source"`
	LastStatus        string `json:"last_status,omitempty"`
	LastError         string `json:"last_error,omitempty"`
	LastRunAt         string `json:"last_run_at,omitempty"`
	RecommendedAction string `json:"recommended_action,omitempty"`
	RunsObserved      int    `json:"runs_observed,omitempty"`
	TotalTokens       int64  `json:"total_tokens,omitempty"`
	AvgTokensPerRun   int64  `json:"avg_tokens_per_run,omitempty"`
	RiskClass         string `json:"risk_class,omitempty"`

	// ProbeState / ProbeObservedAt / ProbeDetail are the OBSERVED liveness of the
	// row's real unit, written by a probe-refresh collector run — distinct from
	// LastStatus/LastError, which the source import claims. A probe that observes
	// the unit dead also bridges LastStatus to "error" so job_health derives from
	// observed state, not the stale imported claim. These stay empty until a
	// probe target for this row has been collected.
	ProbeState      string `json:"probe_state,omitempty"`
	ProbeObservedAt string `json:"probe_observed_at,omitempty"`
	ProbeDetail     string `json:"probe_detail,omitempty"`
}

type JobRosterSourceAuthorityRecord struct {
	SchemaVersion         string   `json:"schema_version"`
	ID                    string   `json:"id"`
	CreatedAt             string   `json:"created_at"`
	SourcePath            string   `json:"source_path"`
	SourceRevision        string   `json:"source_revision"`
	ImportedVia           string   `json:"imported_via"`
	AuthorityRef          string   `json:"authority_ref"`
	AuthorityStatus       string   `json:"authority_status"`
	SourceRole            string   `json:"source_role"`
	Retires               string   `json:"retires"`
	RetirementCondition   string   `json:"retirement_condition"`
	Rollback              string   `json:"rollback"`
	JobCount              int      `json:"job_count"`
	EnabledCount          int      `json:"enabled_count"`
	ErrorCount            int      `json:"error_count"`
	JobIDs                []string `json:"job_ids,omitempty"`
	TokenSessionsCount    int      `json:"token_sessions_count,omitempty"`
	EnabledAgentCount     int      `json:"enabled_agent_count,omitempty"`
	HighTokenEnabledCount int      `json:"high_token_enabled_count,omitempty"`
	EvidenceRefs          []string `json:"evidence_refs"`
}

type JobRosterImportReport struct {
	AuthorityRef      string                         `json:"authority_ref"`
	AuthorityPath     string                         `json:"authority_path"`
	SourceAuthorityID string                         `json:"source_authority_id"`
	SourceAuthority   JobRosterSourceAuthorityRecord `json:"source_authority"`
	JobCount          int                            `json:"job_count"`
	EnabledCount      int                            `json:"enabled_count"`
	RecentRunCount    int                            `json:"recent_run_count"`
	ErrorCount        int                            `json:"error_count"`
	RecentErrors      []JobSummary                   `json:"recent_errors,omitempty"`
}

type JobTokenGovernanceImportReport struct {
	AuthorityRef          string                         `json:"authority_ref"`
	AuthorityPath         string                         `json:"authority_path"`
	SourceAuthorityID     string                         `json:"source_authority_id"`
	SourceAuthority       JobRosterSourceAuthorityRecord `json:"source_authority"`
	TokenSessionsCount    int                            `json:"token_sessions_count"`
	EnabledAgentCount     int                            `json:"enabled_agent_count"`
	HighTokenEnabledCount int                            `json:"high_token_enabled_count"`
	TokenRiskJobs         []JobSummary                   `json:"token_risk_jobs,omitempty"`
}

func ImportJobRoster(homePath, sourcePath string, now time.Time) (JobRosterImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return JobRosterImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return JobRosterImportReport{}, fmt.Errorf("job roster source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return JobRosterImportReport{}, fmt.Errorf("read job roster %s: %w", sourcePath, err)
	}
	jobs, err := parseJobRoster(data, sourcePath)
	if err != nil {
		return JobRosterImportReport{}, fmt.Errorf("parse job roster %s: %w", sourcePath, err)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Name != jobs[j].Name {
			return jobs[i].Name < jobs[j].Name
		}
		return jobs[i].JobID < jobs[j].JobID
	})
	revision := "sha256:" + digestBytes(data)
	authority := JobRosterAuthority{
		SchemaVersion:  JobRosterAuthoritySchema,
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		AuthorityRef:   JobRosterAuthorityRef,
		SourcePath:     sourcePath,
		SourceRevision: revision,
		Jobs:           jobs,
	}
	normalizeJobRosterDerivedFields(&authority, &now)
	authorityPath := JobRosterAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		return JobRosterImportReport{}, err
	}
	body, err := json.MarshalIndent(authority, "", "  ")
	if err != nil {
		return JobRosterImportReport{}, err
	}
	if err := os.WriteFile(authorityPath, append(body, '\n'), 0o644); err != nil {
		return JobRosterImportReport{}, err
	}
	rec := JobRosterSourceAuthorityRecord{
		SchemaVersion:       JobRosterSourceAuthoritySchema,
		ID:                  jobRosterSourceAuthorityID(sourcePath, revision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      revision,
		ImportedVia:         jobRosterImportedVia,
		AuthorityRef:        JobRosterAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "cron_registry_read_authority:" + sourcePath,
		RetirementCondition: jobRosterRetirement,
		Rollback:            jobRosterRollback,
		JobCount:            authority.JobCount,
		EnabledCount:        authority.EnabledCount,
		ErrorCount:          authority.ErrorCount,
		JobIDs:              jobIDs(jobs),
		EvidenceRefs:        []string{revision, JobRosterAuthorityRef},
	}
	if err := appendJobRosterSourceAuthority(homePath, rec); err != nil {
		return JobRosterImportReport{}, err
	}
	return JobRosterImportReport{
		AuthorityRef:      JobRosterAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		JobCount:          authority.JobCount,
		EnabledCount:      authority.EnabledCount,
		RecentRunCount:    authority.RecentRunCount,
		ErrorCount:        authority.ErrorCount,
		RecentErrors:      authority.RecentErrors,
	}, nil
}

func ImportJobTokenGovernance(homePath, sourcePath string, now time.Time) (JobTokenGovernanceImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return JobTokenGovernanceImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return JobTokenGovernanceImportReport{}, fmt.Errorf("job token governance source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return JobTokenGovernanceImportReport{}, fmt.Errorf("read job token governance %s: %w", sourcePath, err)
	}
	var doc struct {
		Metric        string `json:"metric"`
		SessionsCount int    `json:"sessions_count"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return JobTokenGovernanceImportReport{}, fmt.Errorf("parse job token governance metadata %s: %w", sourcePath, err)
	}
	usageJobs, err := parseJobRoster(data, sourcePath)
	if err != nil {
		return JobTokenGovernanceImportReport{}, fmt.Errorf("parse job token governance %s: %w", sourcePath, err)
	}
	revision := "sha256:" + digestBytes(data)
	authority, err := LoadJobRosterAuthority(homePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return JobTokenGovernanceImportReport{}, err
		}
		authority = JobRosterAuthority{
			SchemaVersion:  JobRosterAuthoritySchema,
			AuthorityRef:   JobRosterAuthorityRef,
			SourcePath:     sourcePath,
			SourceRevision: revision,
			Jobs:           usageJobs,
		}
	}
	authority.GeneratedAt = now.UTC().Format(time.RFC3339)
	authority.TokenUsageSourcePath = sourcePath
	authority.TokenUsageSourceRevision = revision
	authority.TokenMetric = strings.TrimSpace(doc.Metric)
	authority.TokenSessionsCount = doc.SessionsCount
	mergeJobTokenUsage(&authority, usageJobs)
	sort.Slice(authority.Jobs, func(i, j int) bool {
		if authority.Jobs[i].Name != authority.Jobs[j].Name {
			return authority.Jobs[i].Name < authority.Jobs[j].Name
		}
		return authority.Jobs[i].JobID < authority.Jobs[j].JobID
	})
	normalizeJobRosterDerivedFields(&authority, nil)
	authorityPath := JobRosterAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		return JobTokenGovernanceImportReport{}, err
	}
	body, err := json.MarshalIndent(authority, "", "  ")
	if err != nil {
		return JobTokenGovernanceImportReport{}, err
	}
	if err := os.WriteFile(authorityPath, append(body, '\n'), 0o644); err != nil {
		return JobTokenGovernanceImportReport{}, err
	}
	rec := JobRosterSourceAuthorityRecord{
		SchemaVersion:         JobRosterSourceAuthoritySchema,
		ID:                    jobRosterSourceAuthorityID(sourcePath, revision),
		CreatedAt:             now.UTC().Format(time.RFC3339),
		SourcePath:            sourcePath,
		SourceRevision:        revision,
		ImportedVia:           jobTokenImportedVia,
		AuthorityRef:          JobRosterAuthorityRef,
		AuthorityStatus:       "compound_primary",
		SourceRole:            "evidence_only_input_after_import",
		Retires:               "cron_token_governance_read_authority:" + sourcePath,
		RetirementCondition:   jobTokenRetirement,
		Rollback:              jobTokenRollback,
		JobCount:              authority.JobCount,
		EnabledCount:          authority.EnabledCount,
		ErrorCount:            authority.ErrorCount,
		TokenSessionsCount:    authority.TokenSessionsCount,
		EnabledAgentCount:     authority.EnabledAgentCount,
		HighTokenEnabledCount: authority.HighTokenEnabledCount,
		JobIDs:                jobIDs(usageJobs),
		EvidenceRefs:          []string{revision, JobRosterAuthorityRef},
	}
	if err := appendJobRosterSourceAuthority(homePath, rec); err != nil {
		return JobTokenGovernanceImportReport{}, err
	}
	return JobTokenGovernanceImportReport{
		AuthorityRef:          JobRosterAuthorityRef,
		AuthorityPath:         authorityPath,
		SourceAuthorityID:     rec.ID,
		SourceAuthority:       rec,
		TokenSessionsCount:    authority.TokenSessionsCount,
		EnabledAgentCount:     authority.EnabledAgentCount,
		HighTokenEnabledCount: authority.HighTokenEnabledCount,
		TokenRiskJobs:         authority.TokenRiskJobs,
	}, nil
}

func mergeJobTokenUsage(authority *JobRosterAuthority, usageJobs []JobSummary) {
	appendMissing := len(authority.Jobs) == 0
	byKey := make(map[string]int, len(authority.Jobs)*2)
	for i, job := range authority.Jobs {
		if job.JobID != "" {
			byKey["id:"+job.JobID] = i
		}
		byKey["name:"+job.Name] = i
	}
	authority.EnabledAgentCount = 0
	authority.HighTokenEnabledCount = 0
	authority.TokenRiskJobs = nil
	for _, usage := range usageJobs {
		if usage.Enabled && !usage.NoAgent {
			authority.EnabledAgentCount++
		}
		if usage.Enabled && !usage.NoAgent && usage.RiskClass == "high_token_agent_job" {
			authority.HighTokenEnabledCount++
			authority.TokenRiskJobs = append(authority.TokenRiskJobs, usage)
		}
		idx, ok := byKey["id:"+usage.JobID]
		if !ok {
			idx, ok = byKey["name:"+usage.Name]
		}
		if ok {
			mergeJobTokenFields(&authority.Jobs[idx], usage)
			continue
		}
		if !appendMissing {
			continue
		}
		authority.Jobs = append(authority.Jobs, usage)
		idx = len(authority.Jobs) - 1
		if usage.JobID != "" {
			byKey["id:"+usage.JobID] = idx
		}
		byKey["name:"+usage.Name] = idx
	}
}

func normalizeJobRosterDerivedFields(authority *JobRosterAuthority, now *time.Time) {
	if authority == nil {
		return
	}
	defer classifyJobRosterErrors(authority)
	if authority == nil {
		return
	}
	authority.JobCount = len(authority.Jobs)
	authority.EnabledCount = 0
	authority.ErrorCount = 0
	authority.RecentErrors = nil
	if now != nil {
		authority.RecentRunCount = 0
	}
	for _, job := range authority.Jobs {
		if job.Enabled {
			authority.EnabledCount++
			if now != nil && job.LastRunAt != "" {
				if t, err := time.Parse(time.RFC3339, job.LastRunAt); err == nil && now.Sub(t) < 48*time.Hour {
					authority.RecentRunCount++
				}
			}
		}
		if job.LastStatus == "error" {
			authority.ErrorCount++
			authority.RecentErrors = append(authority.RecentErrors, job)
		}
	}
}

func classifyJobRosterErrors(authority *JobRosterAuthority) {
	authority.ActiveErrorCount = 0
	authority.InactiveErrorCount = 0
	authority.ActiveErrors = nil
	authority.InactiveErrors = nil
	for _, job := range authority.RecentErrors {
		if actionableJobError(job) {
			authority.ActiveErrorCount++
			authority.ActiveErrors = append(authority.ActiveErrors, job)
			continue
		}
		authority.InactiveErrorCount++
		authority.InactiveErrors = append(authority.InactiveErrors, job)
	}
}

func actionableJobError(job JobSummary) bool {
	if !job.Enabled {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(job.State))
	return state != "paused" && state != "disabled" && state != "archived"
}

func mergeJobTokenFields(dst *JobSummary, src JobSummary) {
	dst.RunsObserved = src.RunsObserved
	dst.TotalTokens = src.TotalTokens
	dst.AvgTokensPerRun = src.AvgTokensPerRun
	dst.RiskClass = src.RiskClass
}

func LoadJobRosterAuthority(homePath string) (JobRosterAuthority, error) {
	data, err := os.ReadFile(JobRosterAuthorityPath(homePath))
	if err != nil {
		return JobRosterAuthority{}, err
	}
	var authority JobRosterAuthority
	if err := json.Unmarshal(data, &authority); err != nil {
		return JobRosterAuthority{}, err
	}
	normalizeJobRosterDerivedFields(&authority, nil)
	return authority, nil
}

func LoadJobRosterSourceAuthority(homePath string) ([]JobRosterSourceAuthorityRecord, error) {
	data, err := os.ReadFile(JobRosterSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []JobRosterSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec JobRosterSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse job roster source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreatedAt < records[j].CreatedAt })
	return records, nil
}

func JobRosterAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", jobRosterFile)
}

func JobRosterSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", jobRosterSourceAuthorityFile)
}

func parseJobRoster(data []byte, sourcePath string) ([]JobSummary, error) {
	var doc struct {
		Jobs json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	raw := doc.Jobs
	if len(raw) == 0 {
		raw = data
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		var byKey map[string]map[string]any
		if err2 := json.Unmarshal(raw, &byKey); err2 != nil {
			return nil, err
		}
		for key, row := range byKey {
			if _, ok := row["name"]; !ok {
				row["name"] = key
			}
			rows = append(rows, row)
		}
	}
	jobs := make([]JobSummary, 0, len(rows))
	for _, row := range rows {
		job := JobSummary{
			JobID:             firstJSONText(row, "job_id", "id"),
			Name:              firstJSONText(row, "name", "label"),
			Enabled:           jsonBool(row["enabled"]),
			State:             firstJSONText(row, "state", "status"),
			Schedule:          firstJSONText(row, "schedule", "schedule_display", "cron"),
			Deliver:           firstJSONText(row, "deliver"),
			Script:            firstJSONText(row, "script", "program"),
			NoAgent:           jsonBool(row["no_agent"]),
			Lane:              firstJSONText(row, "lane"),
			LastStatus:        firstJSONText(row, "last_status"),
			LastError:         firstJSONText(row, "last_error"),
			LastRunAt:         firstJSONText(row, "last_run_at", "last_ran_at"),
			RecommendedAction: firstJSONText(row, "recommended_action"),
			RunsObserved:      jsonInt(row["runs"], row["runs_observed"]),
			TotalTokens:       jsonInt64(row["total_tokens"], row["token_total_observed"]),
			AvgTokensPerRun:   jsonInt64(row["avg_tokens_per_run"]),
			RiskClass:         firstJSONText(row, "risk_class"),
		}
		completeJobIdentity(&job, row, sourcePath)
		if strings.TrimSpace(job.Name) == "" {
			continue
		}
		jobs = append(jobs, job)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("no jobs found")
	}
	return jobs, nil
}

func completeJobIdentity(job *JobSummary, row map[string]any, sourcePath string) {
	explicitOwner := firstJSONText(row, "accountable_owner", "owner", "owned_by")
	explicitHarness := firstJSONText(row, "harness")
	explicitModel := firstJSONText(row, "requested_model", "model")
	explicitProvider := firstJSONText(row, "provider_route", "provider", "route")

	job.AccountableOwner = explicitOwner
	job.Harness = explicitHarness
	job.RequestedModel = explicitModel
	job.ProviderRoute = explicitProvider

	sourceHarness := jobRosterSourceHarness(sourcePath)
	identity := firstNonEmptyJobID(job.JobID, slugToken(job.Name))
	if job.AccountableOwner == "" {
		job.AccountableOwner = sourceHarness + "-job:" + identity
	}
	if job.Harness == "" {
		if job.NoAgent {
			job.Harness = "script"
		} else {
			job.Harness = sourceHarness
		}
	}
	if job.RequestedModel == "" {
		if job.NoAgent {
			job.RequestedModel = "none"
		} else {
			job.RequestedModel = "router-default"
		}
	}
	if job.ProviderRoute == "" {
		if job.NoAgent {
			job.ProviderRoute = "local"
		} else {
			job.ProviderRoute = "router-default"
		}
	}

	explicit := 0
	for _, value := range []string{explicitOwner, explicitHarness, explicitModel, explicitProvider} {
		if value != "" {
			explicit++
		}
	}
	switch explicit {
	case 0:
		job.IdentitySource = "derived"
	case 4:
		job.IdentitySource = "explicit"
	default:
		job.IdentitySource = "mixed"
	}
}

func jobRosterSourceHarness(sourcePath string) string {
	lower := strings.ToLower(filepath.ToSlash(sourcePath))
	if strings.Contains(lower, "/.hermes/") || filepath.Base(lower) == "jobs.json" {
		return "hermes"
	}
	return "scheduler"
}

func firstNonEmptyJobID(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

func appendJobRosterSourceAuthority(homePath string, rec JobRosterSourceAuthorityRecord) error {
	path := JobRosterSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, err := LoadJobRosterSourceAuthority(homePath)
	if err != nil {
		return err
	}
	for _, old := range existing {
		if old.ID == rec.ID {
			return nil
		}
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(body, '\n'))
	return err
}

func jobRosterSourceAuthorityID(sourcePath, revision string) string {
	return "jobsrc_" + digest12(sourcePath+"\n"+revision)
}

func jobIDs(jobs []JobSummary) []string {
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job.JobID != "" {
			ids = append(ids, job.JobID)
		}
	}
	sort.Strings(ids)
	return ids
}

func firstJSONText(row map[string]any, keys ...string) string {
	for _, key := range keys {
		switch v := row[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

func jsonBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func jsonInt(values ...any) int {
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			i, _ := v.Int64()
			return int(i)
		}
	}
	return 0
}

func jsonInt64(values ...any) int64 {
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case json.Number:
			i, _ := v.Int64()
			return i
		}
	}
	return 0
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digest12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
