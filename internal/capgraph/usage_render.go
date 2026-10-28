package capgraph

import (
	"fmt"
	"io"
	"strings"
)

// WriteCapabilityUsageReport renders the operator-facing usage projection.
func WriteCapabilityUsageReport(w io.Writer, projectionPath string, proj CapabilityUsageProjection, top int) error {
	var out strings.Builder
	fmt.Fprintln(&out, "# capability usage")
	fmt.Fprintf(&out, "- projection: %s\n", projectionPath)
	if proj.Since != "" {
		fmt.Fprintf(&out, "- since: %s\n", proj.Since)
	}
	fmt.Fprintf(&out, "- receipts_scanned: %d (in_window %d)\n", proj.ReceiptsScanned, proj.ReceiptsInWindow)
	fmt.Fprintf(&out, "- hook_events_scanned: %d (in_window %d)\n", proj.HookEventsScanned, proj.HookEventsInWindow)
	fmt.Fprintf(&out, "- context_packs_scanned: %d\n", proj.ContextPacksScanned)
	fmt.Fprintf(&out, "- capabilities: %d  events: %d  injected_refs: %d\n\n", len(proj.Capabilities), len(proj.Events), len(proj.InjectedRefs))

	fmt.Fprintln(&out, "## capability evidence (declarations are not execution or benefit)")
	caps := proj.Capabilities
	limit := top
	if limit <= 0 {
		limit = defaultSummaryTop
	}
	if len(caps) > limit {
		caps = caps[:limit]
	}
	for _, cap := range caps {
		fmt.Fprintf(&out, "- %s declared_use=%d accepted_use_declarations=%d observed_skill_calls=%d\n", cap.Ref, cap.DeclaredUseCount, cap.AcceptedUseDeclarationCount, cap.FiredCount)
	}
	fmt.Fprintln(&out, "\n## recent skill events")
	if len(proj.Events) == 0 {
		fmt.Fprintln(&out, "- (none)")
	}
	for _, event := range recentUsageEvents(proj.Events, top) {
		workID := event.WorkID
		if workID == "" {
			workID = "-"
		}
		fmt.Fprintf(&out, "- %s skill=%s harness=%s lane=%s work=%s trigger=%q outcome=%s\n",
			event.OccurredAt, event.Skill, event.Harness, event.Lane, workID, event.Trigger, event.Outcome)
	}

	fmt.Fprintln(&out, "\n## top ignored (injected, never used)")
	if len(proj.Summary.TopIgnored) == 0 {
		fmt.Fprintln(&out, "- (none)")
	}
	for _, entry := range proj.Summary.TopIgnored {
		fmt.Fprintf(&out, "- %s  ignored=%d\n", entry.Ref, entry.Count)
	}

	fmt.Fprintln(&out, "\n## offers without declared use or observed skill calls")
	if len(proj.Summary.NeverFired) == 0 {
		fmt.Fprintln(&out, "- (none)")
	}
	for _, entry := range proj.Summary.NeverFired {
		fmt.Fprintf(&out, "- %s  offered=%d\n", entry.Ref, entry.Count)
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func recentUsageEvents(events []CapabilityUsageEvent, top int) []CapabilityUsageEvent {
	if top <= 0 {
		top = defaultSummaryTop
	}
	if len(events) <= top {
		return events
	}
	return events[len(events)-top:]
}
