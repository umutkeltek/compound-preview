package hook

import (
	"strings"

	"compound/internal/envelope"
)

// NormalizeCapabilityUse extracts bounded Skill metadata from trusted native
// PostToolUse input. Raw input and output never enter the HookEvent.
func NormalizeCapabilityUse(harnessName, toolName string, input map[string]any) *envelope.CapabilityUse {
	if !strings.EqualFold(strings.TrimSpace(toolName), "skill") {
		return nil
	}
	raw, _ := input["skill"].(string)
	skill, ok := normalizeCapabilitySkill(raw)
	if !ok {
		return nil
	}
	trigger := ""
	for _, key := range []string{"trigger", "args", "reason"} {
		if value, ok := input[key].(string); ok {
			trigger = boundedCapabilityTrigger(value)
			if trigger != "" {
				break
			}
		}
	}
	return &envelope.CapabilityUse{
		Ref:     "capability://skill:" + skill,
		Skill:   skill,
		Trigger: trigger,
		Lane:    NormalizeHarness(harnessName),
		Outcome: "used",
	}
}

func normalizeCapabilitySkill(raw string) (string, bool) {
	skill := strings.ToLower(strings.TrimSpace(raw))
	for _, prefix := range []string{"capability://skill:", "skill://", "skill:"} {
		skill = strings.TrimPrefix(skill, prefix)
	}
	if skill == "" || len(skill) > 128 {
		return "", false
	}
	for _, r := range skill {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", false
	}
	return skill, true
}

func boundedCapabilityTrigger(raw string) string {
	trigger := strings.Join(strings.Fields(raw), " ")
	runes := []rune(trigger)
	if len(runes) > 120 {
		trigger = string(runes[:120])
	}
	return trigger
}
