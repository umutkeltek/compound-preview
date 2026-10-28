package hook

// NormalizeNativeTool maps known native tool aliases at live ingress, before
// policy actions and provenance identities are built. Unknown tools stay opaque.
func NormalizeNativeTool(harness, tool string) string {
	if NormalizeHarness(harness) != "oh_my_pi" {
		return tool
	}
	switch tool {
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "glob":
		return "Glob"
	case "grep":
		return "Grep"
	default:
		return tool
	}
}
