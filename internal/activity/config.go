package activity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EnabledKey is the config key the governed toggle repoints. It lives in
// <home>/config/activity-sensor.json as a pre-seeded {"enabled":"false"} scalar so the
// ConfigKeyAdapter (which repoints, never creates) has something to repoint.
const EnabledKey = "enabled"

// activityConfig is the on-disk shape of <home>/config/activity-sensor.json. Enabled is stored as a
// JSON scalar; the governed adapter may write it as a bool, so readers coerce both bool and string.
type activityConfig map[string]any

// ensureConfig materializes <home>/config/activity-sensor.json with {"enabled":"false"} if absent and
// reports whether THIS call created it. Mirrors ensureLaneEntry (modulator.go): the baseline exists
// so the repoint-only ConfigKeyAdapter has a key to flip. The caller only invokes this on an ALLOWED
// governed path (gate before materialize), so a refused toggle never creates the file.
func ensureConfig(home string) (created bool, err error) {
	path := ConfigPath(home)
	if _, statErr := os.Stat(path); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	cfg := activityConfig{EnabledKey: "false"}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureConfigSeeded pre-seeds the config for status/collector-facing reads. Idempotent.
func EnsureConfigSeeded(home string) error {
	_, err := ensureConfig(home)
	return err
}

// removeConfigIfPresent deletes the config file (compensation when a governed toggle fails AFTER this
// pass created the baseline — mirrors deleteLaneEntry).
func removeConfigIfPresent(home string) error {
	err := os.Remove(ConfigPath(home))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Enabled reports whether the activity sensor is enabled for this home. A missing config or a
// missing/blank key reads as DISABLED (fail-closed default off). Coerces bool true / string "true".
func Enabled(home string) (bool, error) {
	data, err := os.ReadFile(ConfigPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cfg activityConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("activity config parse: %w", err)
	}
	return coerceEnabled(cfg[EnabledKey]), nil
}

func coerceEnabled(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(t))
		return b
	default:
		return false
	}
}
