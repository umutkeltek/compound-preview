package routeevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const ExperimentPlanSchema = "compound.route_experiment_plan.v0.1"

type conformanceProfile struct {
	ID              string            `json:"id"`
	Harness         string            `json:"harness"`
	Route           string            `json:"route"`
	RequestedModel  string            `json:"requested_model"`
	RequestedEffort string            `json:"requested_effort"`
	ClientProfile   string            `json:"client_profile,omitempty"`
	ModelAliases    map[string]string `json:"model_aliases"`
}
type conformanceFile struct {
	Schema              string             `json:"schema"`
	ConformanceRevision string             `json:"conformance_revision"`
	Profile             conformanceProfile `json:"profile"`
}

type ExperimentLane struct {
	ProfileID           string `json:"profile_id"`
	Harness             string `json:"harness"`
	RoleAlias           string `json:"role_alias"`
	RequestedAlias      string `json:"requested_alias"`
	ExpectedRoute       string `json:"expected_route"`
	RequestedEffort     string `json:"requested_effort"`
	ClientProfile       string `json:"client_profile,omitempty"`
	ConformanceRevision string `json:"conformance_revision"`
	ObservedProvider    string `json:"observed_provider"`
	ObservedModel       string `json:"observed_model"`
	ObservedAccountID   string `json:"observed_account_id"`
	EvidenceStatus      string `json:"evidence_status"`
}

type ExperimentPlan struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	RoleAlias     string           `json:"role_alias"`
	RoleAliases   []string         `json:"role_aliases,omitempty"`
	EffortFilters []string         `json:"effort_filters,omitempty"`
	ProfilesRoot  string           `json:"profiles_root"`
	Lanes         []ExperimentLane `json:"lanes"`
	Independence  string           `json:"independence_requirement"`
	Execution     string           `json:"execution"`
	Verdict       string           `json:"verdict"`
}

func WriteExperimentPlanText(w io.Writer, plan ExperimentPlan) {
	fmt.Fprintf(w, "# route experiment-plan %s\n", plan.ID)
	if len(plan.RoleAliases) > 1 {
		fmt.Fprintf(w, "- role_aliases: %s\n", strings.Join(plan.RoleAliases, ","))
	} else {
		fmt.Fprintf(w, "- role_alias: %s\n", plan.RoleAlias)
	}
	if len(plan.EffortFilters) > 0 {
		fmt.Fprintf(w, "- effort_filters: %s\n", strings.Join(plan.EffortFilters, ","))
	}
	for _, lane := range plan.Lanes {
		fmt.Fprintf(w, "- lane: profile=%s role=%s requested_alias=%s expected_route=%s observed_provider=unknown observed_model=unknown observed_account=unknown evidence=%s\n", lane.ProfileID, lane.RoleAlias, lane.RequestedAlias, lane.ExpectedRoute, lane.EvidenceStatus)
	}
	fmt.Fprintf(w, "- independence: %s\n- execution: %s\n- verdict: %s\n", plan.Independence, plan.Execution, plan.Verdict)
}

func PlanRouteExperiment(profilesRoot, role string, profileIDs []string) (ExperimentPlan, error) {
	if len(profileIDs) < 2 {
		return ExperimentPlan{}, fmt.Errorf("at least two --profile values required")
	}
	return PlanRouteExperimentMatrix(profilesRoot, []string{role}, nil, profileIDs)
}

// PlanRouteExperimentMatrix expands owner-published launch profiles into concrete
// profile×role trials. Multiple roles let one subscription profile compare its real
// model aliases; effort filters may select only effort values already materialized by
// distinct Switchback profiles. The planner never invents a runnable effort preset.
func PlanRouteExperimentMatrix(profilesRoot string, roles, effortFilters, profileIDs []string) (ExperimentPlan, error) {
	roles, err := normalizeExperimentRoles(roles)
	if err != nil {
		return ExperimentPlan{}, err
	}
	effortFilters, err = normalizeExperimentValues("effort", effortFilters)
	if err != nil {
		return ExperimentPlan{}, err
	}
	if len(profileIDs) == 0 {
		return ExperimentPlan{}, fmt.Errorf("at least one --profile value required")
	}
	seen := map[string]bool{}
	profiles := make([]conformanceFile, 0, len(profileIDs))
	for _, id := range profileIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return ExperimentPlan{}, fmt.Errorf("profile ids must be non-empty and unique: %q", id)
		}
		seen[id] = true
		body, err := os.ReadFile(filepath.Join(profilesRoot, id+".json"))
		if err != nil {
			return ExperimentPlan{}, fmt.Errorf("read profile %s: %w", id, err)
		}
		var cf conformanceFile
		if err := json.Unmarshal(body, &cf); err != nil {
			return ExperimentPlan{}, fmt.Errorf("decode profile %s: %w", id, err)
		}
		if cf.Profile.ID != id || cf.Schema != "switchback/profile-conformance@1" {
			return ExperimentPlan{}, fmt.Errorf("profile %s conformance identity/schema mismatch", id)
		}
		profiles = append(profiles, cf)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Profile.ID < profiles[j].Profile.ID })
	publishedEfforts := map[string]bool{}
	for _, cf := range profiles {
		publishedEfforts[strings.TrimSpace(cf.Profile.RequestedEffort)] = true
	}
	for _, effort := range effortFilters {
		if !publishedEfforts[effort] {
			return ExperimentPlan{}, fmt.Errorf("effort %s is not materialized by selected launch profiles; Switchback must publish a distinct profile with requested_effort=%s", effort, effort)
		}
	}
	wantedEffort := make(map[string]bool, len(effortFilters))
	for _, effort := range effortFilters {
		wantedEffort[effort] = true
	}
	lanes := make([]ExperimentLane, 0, len(profiles)*len(roles))
	for _, cf := range profiles {
		if len(wantedEffort) > 0 && !wantedEffort[cf.Profile.RequestedEffort] {
			continue
		}
		for _, role := range roles {
			alias := cf.Profile.ModelAliases[role]
			if role == "default" && alias == "" {
				alias = cf.Profile.RequestedModel
			}
			if alias == "" {
				return ExperimentPlan{}, fmt.Errorf("profile %s does not declare role alias %s", cf.Profile.ID, role)
			}
			lanes = append(lanes, ExperimentLane{ProfileID: cf.Profile.ID, Harness: cf.Profile.Harness, RoleAlias: role, RequestedAlias: alias, ExpectedRoute: cf.Profile.Route, RequestedEffort: cf.Profile.RequestedEffort, ClientProfile: cf.Profile.ClientProfile, ConformanceRevision: cf.ConformanceRevision, EvidenceStatus: "unknown_until_route_evidence"})
		}
	}
	if len(lanes) < 2 {
		return ExperimentPlan{}, fmt.Errorf("experiment needs at least two profile×role trials after effort filtering")
	}
	legacyInvocation := len(roles) == 1 && len(effortFilters) == 0
	var seed []byte
	if legacyInvocation {
		// Preserve the v0.1 single-role plan identity. Existing receipts and
		// scripts use this deterministic rexp_* value as their comparison key.
		seed, _ = json.Marshal(struct {
			Role  string
			Lanes []ExperimentLane
		}{roles[0], lanes})
	} else {
		seed, _ = json.Marshal(struct {
			Roles   []string
			Efforts []string
			Lanes   []ExperimentLane
		}{roles, effortFilters, lanes})
	}
	sum := sha256.Sum256(seed)
	roleAliases := []string(nil)
	if len(roles) > 1 {
		roleAliases = append(roleAliases, roles...)
	}
	execution := "experimental only (unsupported in v0.1): execute each owner-published profile×role lane through compound work delegate with --experimental-delegate-mutations; import Switchback trace evidence before comparison"
	if legacyInvocation {
		execution = "experimental only (unsupported in v0.1): execute each lane through compound work delegate with --experimental-delegate-mutations; import Switchback trace evidence before comparison"
	}
	return ExperimentPlan{SchemaVersion: ExperimentPlanSchema, ID: "rexp_" + hex.EncodeToString(sum[:6]), RoleAlias: roles[0], RoleAliases: roleAliases, EffortFilters: effortFilters, ProfilesRoot: profilesRoot, Lanes: lanes, Independence: "terminal comparison requires distinct observed provider/model or account identities; profile and role labels do not prove independence", Execution: execution, Verdict: "plan_only_not_execution_evidence"}, nil
}

func normalizeExperimentRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		roles = []string{"opus"}
	}
	roles, err := normalizeExperimentValues("role", roles)
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		switch role {
		case "default", "opus", "sonnet", "haiku", "subagent":
		default:
			return nil, fmt.Errorf("unsupported role alias %q", role)
		}
	}
	return roles, nil
}

func normalizeExperimentValues(kind string, values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" || seen[value] {
			return nil, fmt.Errorf("%s values must be non-empty and unique: %q", kind, value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}
