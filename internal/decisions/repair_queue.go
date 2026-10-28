package decisions

import (
	"strings"

	"compound/internal/reviewartifact"
)

const lifecycleRepairArea = "decision-lifecycle-repairs"

// AnnotateLifecycleRepairQueue attaches decision-owner apply hints to generic
// review artifacts. It does not load owner files, mutate artifacts, or apply
// writebacks; it only makes the existing reviewed-apply seam explicit to queue
// consumers.
func AnnotateLifecycleRepairQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	report.Counts.ReadyToApply = 0
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != lifecycleRepairArea {
			continue
		}
		annotateLifecycleRepairItem(item)
		if item.Readiness == "ready_to_apply" {
			report.Counts.ReadyToApply++
		}
	}
}

func annotateLifecycleRepairItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		item.Readiness = "blocked"
		item.NextAction = "repair plan path missing"
		return
	}
	switch {
	case strings.TrimSpace(item.Status) == "ok":
		if item.RepairCount == 0 {
			item.Readiness = "no_action"
			item.NextAction = "no lifecycle repairs required"
			return
		}
		item.Readiness = "needs_review"
		item.NextAction = "review unexpected ok repair plan before applying"
	case LifecycleRepairPlanReviewed(item.Status):
		item.Readiness = "ready_to_apply"
		item.NextAction = "run reviewed writeback apply"
	default:
		item.Readiness = "needs_review"
		item.NextAction = "review markdown artifact; dry-run safe, write-reviewed only after source-owner review"
	}
	item.Actions = lifecycleRepairActions(item.JSONPath, item.Readiness)
}

func lifecycleRepairActions(planPath, readiness string) []reviewartifact.Action {
	dryRunArgv := []string{"compound", "decisions", "repair-apply", "--plan", planPath}
	writeArgv := append([]string{}, dryRunArgv...)
	writeArgv = append(writeArgv, "--write-reviewed")
	actions := []reviewartifact.Action{
		{
			ID:      "dry_run",
			Label:   "Dry-run apply",
			Command: shellCommand(dryRunArgv),
			Argv:    dryRunArgv,
		},
	}
	writeAction := reviewartifact.Action{
		ID:             "write_reviewed",
		Label:          "Apply reviewed repair",
		Command:        shellCommand(writeArgv),
		Argv:           writeArgv,
		RequiresReview: true,
	}
	if readiness == "ready_to_apply" {
		writeAction.RequiresReview = false
	}
	return append(actions, writeAction)
}

func shellCommand(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}
