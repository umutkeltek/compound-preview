package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type OwnedSetting struct {
	NativeKey string `json:"native_key"`
	Purpose   string `json:"purpose"`
}
type SettingsMatrixRow struct {
	Harness           string                `json:"harness"`
	AuthorityRef      string                `json:"authority_ref"`
	AuthorityRevision string                `json:"authority_revision,omitempty"`
	AuthorityStatus   string                `json:"authority_status"`
	NativeStatus      string                `json:"native_status"`
	OverallStatus     string                `json:"overall_status"`
	Settings          []OwnedSetting        `json:"settings"`
	Sources           []SettingSourceReport `json:"declared_sources"`
	Unverified        []string              `json:"unverified"`
	// Selection is the registry row's launch selection capability; nil when
	// the registry carries none (every requested model/effort then refuses).
	Selection *SelectionCapability `json:"selection,omitempty"`
}
type SettingsMatrix struct {
	SchemaVersion string              `json:"schema_version"`
	Coverage      string              `json:"coverage"`
	Equivalence   string              `json:"equivalence"`
	Rows          []SettingsMatrixRow `json:"rows"`
}

// SettingsMatrixFor reads the registry and current owned authorities each time.
// It neither manufactures defaults for gaps nor infers provider equivalence.
func SettingsMatrixFor(homePath string, registry Registry, only string) SettingsMatrix {
	matrix := SettingsMatrix{SchemaVersion: "compound.harness_settings_matrix.v0.1", Coverage: "declared_sources_only", Equivalence: "same-purpose labels; no cross-provider model or effort value equivalence asserted", Rows: []SettingsMatrixRow{}}
	for _, name := range registry.Names() {
		if only != "" && name != only {
			continue
		}
		row := SettingsMatrixRow{Harness: name, AuthorityRef: PermissionAuthorityRef(name), AuthorityStatus: "unavailable", NativeStatus: "unverified", OverallStatus: "unverified", Settings: []OwnedSetting{}, Sources: []SettingSourceReport{}, Unverified: []string{"effective_session_model_and_effort", "undeclared_launch_overrides"}, Selection: registry.Harnesses[name].Selection}
		policy, revision, err := LoadPermissionAuthority(homePath, name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				row.AuthorityStatus = "missing_authority"
			}
			matrix.Rows = append(matrix.Rows, row)
			continue
		}
		row.AuthorityStatus = "owned"
		row.Sources = ObserveSettingSources(policy)
		row.AuthorityRevision = revision
		keys := make([]string, 0, len(policy.Settings))
		for key := range policy.Settings {
			keys = append(keys, key)
		}
		if len(keys) == 0 {
			keys = []string{"permissions.allow", "permissions.deny", "permissions.ask", "autoMode.allow"}
			if policy.Permissions.DefaultMode != "" {
				keys = append(keys, "permissions.defaultMode")
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			row.Settings = append(row.Settings, OwnedSetting{NativeKey: key, Purpose: settingPurpose(key)})
		}
		if _, ok := policy.Settings["model"]; !ok {
			row.Unverified = append(row.Unverified, "model_default_not_owned")
		}
		if len(policy.SettingSources) == 0 {
			row.Unverified = append(row.Unverified, "no_declared_launch_sources")
		}
		native, nerr := permissionNativeDrift(homePath, name)
		drift, derr := PermissionDrift(homePath, name)
		_, after, aerr := LoadPermissionAuthority(homePath, name)
		if aerr != nil || after != revision {
			row.AuthorityStatus = "changed_during_probe"
		} else {
			if nerr == nil {
				row.NativeStatus = "matches"
				if native.Drifted {
					row.NativeStatus = "drifted"
				}
			}
			if derr == nil && nerr == nil && drift.TargetRevision == native.TargetRevision {
				row.Sources = drift.SettingSources
				row.OverallStatus = "matches"
				if drift.Drifted {
					row.OverallStatus = "drifted"
				}
			}
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	return matrix
}

func settingPurpose(key string) string {
	switch key {
	case "model":
		return "model_selection"
	case "model_reasoning_effort":
		return "reasoning_effort_native_scale"
	case "web_search":
		return "search_freshness"
	case "approval_policy", "permissions.defaultMode":
		return "approval_posture"
	case "sandbox_mode":
		return "sandbox_posture"
	}
	if strings.HasPrefix(key, "permissions.") || key == "autoMode.allow" {
		return "permission_rules"
	}
	return "native_only"
}
func RenderSettingsMatrixCLI(root string, args []string, stdout, stderr io.Writer) int {
	only := ""
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--harness":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "--harness requires an identity")
				return 2
			}
			only = args[i]
		default:
			fmt.Fprintln(stderr, "usage: compound harness settings-matrix [--harness H] [--json]")
			return 2
		}
	}
	registry := DefaultRegistry()
	if only != "" {
		if _, ok := registry.Harnesses[only]; !ok {
			fmt.Fprintln(stderr, "unknown registered harness")
			return 2
		}
	}
	report := SettingsMatrixFor(root, registry, only)
	if jsonOut {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintln(stderr, "write matrix failed")
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "harness | authority | native | overall | declared sources | owned native keys")
	for _, row := range report.Rows {
		keys := make([]string, 0, len(row.Settings))
		for _, setting := range row.Settings {
			keys = append(keys, setting.NativeKey)
		}
		sources := make([]string, 0, len(row.Sources))
		for _, source := range row.Sources {
			sources = append(sources, source.ID+":"+source.Status)
		}
		fmt.Fprintf(stdout, "%s | %s | %s | %s | %v | %v\n", row.Harness, row.AuthorityStatus, row.NativeStatus, row.OverallStatus, sources, keys)
	}
	fmt.Fprintln(stdout, "Coverage: declared sources only; effective sessions and undeclared overrides remain unverified.")
	fmt.Fprintln(stdout, report.Equivalence)
	return 0
}
