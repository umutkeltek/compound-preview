package decisions

import (
	"fmt"
	"strings"

	"compound/internal/policy"
)

func isOperationalAuthorityKind(kind string) bool {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case KindGrant, KindFreeze, KindRuling:
		return true
	default:
		return false
	}
}

func validateOperationalAuthorityFrontmatter(f decFrontmatter) error {
	if !isOperationalAuthorityKind(f.Kind) {
		return nil
	}
	if !isCanonicalOperationalStatus(f.Status) {
		return fmt.Errorf("operational decision kind %q requires explicit canonical status", f.Kind)
	}
	if !policy.HasOperatorProvenance(f.ReviewedBy) {
		return fmt.Errorf("operational decision kind %q requires operator provenance in reviewed_by", f.Kind)
	}
	autonomyFields := strings.TrimSpace(f.AutoAdmitClass) != "" ||
		f.MinTerminalSamples != 0 ||
		f.MinWindowDays != 0 ||
		f.MaxFailures != 0 ||
		(strings.TrimSpace(f.Kind) == KindGrant && strings.TrimSpace(f.Scope) == AutonomyGrantScope)
	if !autonomyFields {
		return nil
	}
	if strings.TrimSpace(f.Kind) != KindGrant {
		return fmt.Errorf("autonomy thresholds require kind %q", KindGrant)
	}
	if strings.TrimSpace(f.Scope) != AutonomyGrantScope {
		return fmt.Errorf("autonomy thresholds require scope %q", AutonomyGrantScope)
	}
	if strings.TrimSpace(f.AutoAdmitClass) == "" {
		return fmt.Errorf("autonomy thresholds require auto_admit_class")
	}
	if f.MinTerminalSamples <= 0 {
		return fmt.Errorf("autonomy min_terminal_samples must be positive")
	}
	if f.MinWindowDays <= 0 {
		return fmt.Errorf("autonomy min_window_days must be positive")
	}
	if f.MaxFailures < 0 || f.MaxFailures >= f.MinTerminalSamples {
		return fmt.Errorf("autonomy max_failures must be >= 0 and less than min_terminal_samples")
	}
	return nil
}

func isCanonicalOperationalStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case StatusProposed, StatusAccepted, StatusSuperseded, StatusDeprecated, StatusRejected:
		return true
	default:
		return false
	}
}
