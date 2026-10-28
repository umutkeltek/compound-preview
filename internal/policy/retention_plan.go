package policy

// Reclaim planning turns a consumer's published free-space requirements into an ordered plan
// over declared classes. Compound does not invent thresholds: callers supply current capacity
// and the consumer's byte and/or ratio floor.
//
// Rebuildable artifacts are considered first. Operator-gated classes are reported but never
// placed in automatic execution steps.

import (
	"fmt"
	"sort"
	"strings"
)

// ReclaimTarget is what a consumer needs, in its own terms.
type ReclaimTarget struct {
	// Consumer names who needs the space, for attribution in the plan.
	Consumer string `json:"consumer"`
	// FreeBytes / CapacityBytes describe the filesystem right now.
	FreeBytes     int64 `json:"free_bytes"`
	CapacityBytes int64 `json:"capacity_bytes"`
	// Both floors apply. Taking the stricter requirement prevents a byte-only check from
	// declaring success while the consumer's ratio threshold still fails.
	RequiredFreeBytes int64 `json:"required_free_bytes"`
	RequiredFreeBps   int64 `json:"required_free_bps"`
}

// ShortfallBytes is how many bytes must be freed to satisfy BOTH thresholds.
//
// Returns 0 when already satisfied. The required free space is the stricter of the absolute
// and ratio-derived floors.
func (t ReclaimTarget) ShortfallBytes() int64 {
	need := t.RequiredFreeBytes
	if t.CapacityBytes > 0 && t.RequiredFreeBps > 0 {
		byRatio := t.CapacityBytes * t.RequiredFreeBps / 10_000
		if byRatio > need {
			need = byRatio
		}
	}
	if short := need - t.FreeBytes; short > 0 {
		return short
	}
	return 0
}

// ReclaimStep is one executable action.
type ReclaimStep struct {
	ClassID string `json:"class_id"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	// Command is a safely quoted compatibility/display rendering.
	Command string `json:"command"`
	// CommandArgv is the canonical executable form. Executors must use argv
	// directly rather than reparsing Command through a shell.
	CommandArgv []string `json:"command_argv,omitempty"`
	// CumulativeBytes is the running total after this step, so an executor can stop as soon as
	// the target is met rather than reclaiming everything available.
	CumulativeBytes int64 `json:"cumulative_bytes"`
}

// ReclaimPlan is the answer to "what should I delete, in what order, to get this consumer back".
type ReclaimPlan struct {
	AuthorityRef   string        `json:"authority_ref"`
	Target         ReclaimTarget `json:"target"`
	ShortfallBytes int64         `json:"shortfall_bytes"`
	// Steps are automatic-mode, rebuildable-first, and stop once the shortfall is covered.
	Steps []ReclaimStep `json:"steps,omitempty"`
	// PlannedBytes is what the steps would actually free.
	PlannedBytes int64 `json:"planned_bytes"`
	// Sufficient reports whether the plan closes the shortfall. FALSE is the important case:
	// it means automatic reclamation cannot fix this and a human has to decide.
	Sufficient bool `json:"sufficient"`
	// OperatorGated lists space a human could free that the plan will not touch. Reported so a
	// `sufficient: false` verdict comes with the operator's actual options attached.
	OperatorGated []ReclaimStep `json:"operator_gated,omitempty"`
	// Note explains the verdict in one line.
	Note string `json:"note"`
}

// PlanReclaim builds an ordered plan from a scan and a consumer's target.
func PlanReclaim(scan RetentionScanReport, target ReclaimTarget) ReclaimPlan {
	plan := ReclaimPlan{
		AuthorityRef:   RetentionAuthorityRef,
		Target:         target,
		ShortfallBytes: target.ShortfallBytes(),
	}

	// Rebuildable first, then largest first within that — fewest actions to reach the target.
	candidates := append([]RetentionInstance(nil), scan.Declared...)
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Rebuildable != candidates[j].Rebuildable {
			return candidates[i].Rebuildable
		}
		return candidates[i].Bytes > candidates[j].Bytes
	})

	var running int64
	for _, item := range candidates {
		class, ok := RetentionClassByID(item.ClassID)
		if !ok {
			continue
		}
		step := ReclaimStep{
			ClassID:     item.ClassID,
			Path:        item.Path,
			Bytes:       item.Bytes,
			Command:     class.ReclaimCommand,
			CommandArgv: renderReclaimCommandArgv(class.ReclaimCommandArgv, item),
		}
		if len(step.CommandArgv) > 0 {
			step.Command = renderReclaimCommand(step.CommandArgv)
		}
		if item.RecentlyActive {
			// In use right now. Report it so the operator sees the bytes exist, but never hand
			// it to an unattended reclaimer — a target/ deleted mid-compile breaks that build.
			plan.OperatorGated = append(plan.OperatorGated, step)
			continue
		}
		if class.Mode != ReclaimAutomatic || !class.Rebuildable || len(step.CommandArgv) == 0 {
			// Detect and report, never execute. A worktree is not rebuildable and can hold
			// uncommitted work; reclaiming one unattended is how a lane loses a day. An
			// automatic class without concrete argv is equally non-executable and stays gated.
			plan.OperatorGated = append(plan.OperatorGated, step)
			continue
		}
		// Reclaim only what the consumer needs. A zero shortfall means it needs NOTHING —
		// without this the planner happily proposed deleting every cache on a healthy disk,
		// which is destruction with no benefit and would train anyone to distrust the plan.
		if running >= plan.ShortfallBytes {
			continue
		}
		running += item.Bytes
		step.CumulativeBytes = running
		plan.Steps = append(plan.Steps, step)
	}

	plan.PlannedBytes = running
	plan.Sufficient = plan.ShortfallBytes == 0 || running >= plan.ShortfallBytes

	switch {
	case plan.ShortfallBytes == 0:
		plan.Note = fmt.Sprintf("%s is already satisfied; no reclamation needed", target.Consumer)
	case plan.Sufficient:
		plan.Note = fmt.Sprintf("automatic reclamation covers the %s shortfall", target.Consumer)
	default:
		plan.Note = fmt.Sprintf(
			"automatic reclamation CANNOT cover the %s shortfall (%d bytes short); operator-gated space is listed",
			target.Consumer, plan.ShortfallBytes-running)
	}
	return plan
}

func renderReclaimCommandArgv(template []string, item RetentionInstance) []string {
	if len(template) == 0 {
		return nil
	}
	argv := make([]string, 0, len(template))
	for _, arg := range template {
		switch arg {
		case "<path>":
			if strings.TrimSpace(item.Path) == "" {
				return nil
			}
			argv = append(argv, item.Path)
		default:
			if strings.HasPrefix(arg, "<") && strings.HasSuffix(arg, ">") {
				return nil
			}
			argv = append(argv, arg)
		}
	}
	return argv
}

func renderReclaimCommand(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, quoteReclaimArg(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteReclaimArg(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') &&
			!(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') &&
			!strings.ContainsRune("-_./:", r)
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}
