// decisions_cli.go owns the `compound decisions ...` verb dispatcher
// (cmdDecisions). Extracted from cmd/compound/main.go so main.go holds
// only the cross-cutting dispatch table + non-decisions verbs. The
// decisions runtime (BrainDecision import + repair plan/apply seams) lives
// in compound/internal/decisions and is unchanged; this file is only the
// CLI surface that drives it.
//
// Composition with the other CLI verb files:
//   - memory_cli.go                 (cmdMemory)
//   - memory_engine_config.go       (engine config subsystem)
//   - distiller_cli.go              (cmdDistiller)
//   - this file                     (cmdDecisions — third verb extracted)
//
// Subcommand surface (all in cmdDecisions's switch):
//   - show
//   - candidate-plan
//   - import
//   - authority
//   - standing-law
//   - record
//   - doctor
//   - repair-plan
//   - repair-apply
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/home"
	"compound/internal/mem"
)

func cmdDecisions(args []string) {
	values := func(name string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == name && i+1 < len(args):
				out = append(out, args[i+1])
				i++
			case strings.HasPrefix(a, name+"="):
				out = append(out, strings.TrimPrefix(a, name+"="))
			}
		}
		return out
	}
	value := func(name string) string {
		got := values(name)
		if len(got) == 0 {
			return ""
		}
		return got[0]
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "source-doctor" {
		templatePath := value("--template")
		formatDocPath := value("--format-doc")
		if strings.TrimSpace(templatePath) == "" {
			fmt.Fprintln(os.Stderr, "usage: compound decisions source-doctor --template <decision-template.md> [--format-doc <README.md>]")
			os.Exit(2)
		}
		report, err := decisions.DoctorSourceFormat(templatePath, formatDocPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions source-doctor:", err)
			os.Exit(1)
		}
		fmt.Printf("# decisions source-doctor\n\n")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- template: %s\n", report.TemplatePath)
		if report.FormatDocPath != "" {
			fmt.Printf("- format_doc: %s\n", report.FormatDocPath)
		}
		fmt.Printf("- required_lifecycle_fields: %s\n", strings.Join(report.RequiredLifecycleFields, ","))
		fmt.Printf("- missing_template_frontmatter: %d\n", len(report.MissingTemplateFrontmatter))
		for _, field := range report.MissingTemplateFrontmatter {
			fmt.Printf("  - %s\n", field)
		}
		fmt.Printf("- missing_template_guidance: %d\n", len(report.MissingTemplateGuidance))
		for _, field := range report.MissingTemplateGuidance {
			fmt.Printf("  - %s\n", field)
		}
		fmt.Printf("- missing_format_documentation: %d\n", len(report.MissingFormatDocumentation))
		for _, field := range report.MissingFormatDocumentation {
			fmt.Printf("  - %s\n", field)
		}
		if report.Status != decisions.SourceFormatDoctorOK {
			os.Exit(1)
		}
		return
	}
	store, homePath := openMemStore()
	switch sub {
	case "show":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(os.Stderr, "usage: compound decisions show <decision://ref|decision_id|claim_id> [--json]")
			os.Exit(2)
		}
		record, err := decisions.LoadActive(store, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions show:", err)
			os.Exit(1)
		}
		if err := record.Render(os.Stdout, hasFlag(args[2:], "--json")); err != nil {
			fmt.Fprintln(os.Stderr, "decisions show:", err)
			os.Exit(1)
		}
	case "candidate-plan":
		candidateID := valueFlag(args[1:], "--candidate")
		targetPath := valueFlag(args[1:], "--target")
		workID := strings.TrimSpace(valueFlag(args[1:], "--work"))
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound decisions candidate-plan --candidate <candidate_id> [--target <file>] [--work <work_id>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions candidate-plan:", err)
			os.Exit(1)
		}
		plan, err := decisions.BuildDecisionCandidatePlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions candidate-plan:", err)
			os.Exit(1)
		}
		// Binding an owner work id lets the review-authority stamp sign the
		// plan-review action so the operator surface can act on it; without it the
		// action ships unsigned and the group renders fail-closed.
		plan.WorkID = workID
		paths, err := decisions.RecordDecisionCandidatePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions candidate-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# decisions candidate-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- decision_key: %s\n", plan.DecisionKey)
		fmt.Printf("- decision_id: %s\n", plan.LifecycleDraft.DecisionID)
		fmt.Printf("- entity: %s\n", plan.LifecycleDraft.EntityID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(plan.LifecycleDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
	case "import":
		sourceKind := decisionSourceKind(value("--source-kind"))
		var sourceReports []decisions.BrainDecisionReport
		var sourceAuthority []decisions.SourceAuthorityRecord
		for _, path := range values("--source") {
			report, err := importDecisionSource(store, path, sourceKind, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decision source import:", err)
				os.Exit(1)
			}
			rec, err := decisions.RecordSourceAuthority(homePath, decisions.DecisionImportReport(report), "decisions import --source", time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decision source authority:", err)
				os.Exit(1)
			}
			sourceAuthority = append(sourceAuthority, rec)
			sourceReports = append(sourceReports, report)
		}
		for _, path := range values("--brain") {
			report, err := decisions.ImportBrainDecision(store, path, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "brain decision import:", err)
				os.Exit(1)
			}
			rec, err := decisions.RecordSourceAuthority(homePath, decisions.DecisionImportReport(report), "decisions import --brain", time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "brain decision source authority:", err)
				os.Exit(1)
			}
			sourceAuthority = append(sourceAuthority, rec)
			sourceReports = append(sourceReports, report)
		}
		for _, root := range values("--source-dir") {
			reports, err := importDecisionSourceDir(store, root, sourceKind, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decision sources import:", err)
				os.Exit(1)
			}
			for _, report := range reports {
				rec, err := decisions.RecordSourceAuthority(homePath, decisions.DecisionImportReport(report), "decisions import --source-dir", time.Now())
				if err != nil {
					fmt.Fprintln(os.Stderr, "decision sources authority:", err)
					os.Exit(1)
				}
				sourceAuthority = append(sourceAuthority, rec)
			}
			sourceReports = append(sourceReports, reports...)
		}
		var policyReports []decisions.DecisionImportReport
		for _, path := range values("--policy") {
			report, err := decisions.ImportPolicyDecision(store, path, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "policy decision import:", err)
				os.Exit(1)
			}
			rec, err := decisions.RecordSourceAuthority(homePath, report, "decisions import --policy", time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "policy decision source authority:", err)
				os.Exit(1)
			}
			sourceAuthority = append(sourceAuthority, rec)
			policyReports = append(policyReports, report)
		}
		policyDirs := values("--policy-dir")
		if root := strings.TrimSpace(os.Getenv("COMPOUND_POLICY_DECISIONS_DIR")); root != "" {
			policyDirs = append(policyDirs, root)
		}
		for _, root := range policyDirs {
			reports, err := decisions.ImportPolicyDecisionDir(store, root, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "policy decisions import:", err)
				os.Exit(1)
			}
			for _, report := range reports {
				rec, err := decisions.RecordSourceAuthority(homePath, report, "decisions import --policy-dir", time.Now())
				if err != nil {
					fmt.Fprintln(os.Stderr, "policy decisions source authority:", err)
					os.Exit(1)
				}
				sourceAuthority = append(sourceAuthority, rec)
			}
			policyReports = append(policyReports, reports...)
		}
		var facts []mem.Candidate
		if pattern := strings.TrimSpace(os.Getenv("COMPOUND_OPERATOR_DECISIONS_GLOB")); pattern != "" {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				got, err := decisions.ImportOperatorLedger(m)
				if err != nil {
					fmt.Fprintln(os.Stderr, "operator decisions import:", err)
					os.Exit(1)
				}
				facts = append(facts, got...)
			}
		}
		captured, err := store.Capture(facts, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions capture:", err)
			os.Exit(1)
		}
		promoted, parked := 0, 0
		for _, c := range captured {
			verdict := decisions.VerdictForCandidate(c)
			if _, err := store.Adjudicate(c.ID, verdict, "decisions-import", time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "decisions adjudicate:", err)
				os.Exit(1)
			}
			if verdict == "promote" {
				promoted++
			} else {
				parked++
			}
		}
		for _, report := range sourceReports {
			if report.Verdict == candidates.Promote {
				promoted++
			} else {
				parked++
			}
		}
		for _, report := range policyReports {
			if report.Verdict == candidates.Promote {
				promoted++
			} else {
				parked++
			}
		}
		fmt.Printf("processed %d decision sources through spine memory (%d primary, %d parked)\n", len(captured)+len(sourceReports)+len(policyReports), promoted, parked)
		for _, report := range sourceReports {
			fmt.Printf("- source %s: %s kind=%s candidate=%s evidence=%s claim=%s\n", report.Lifecycle.ID, report.Verdict, report.SourceKind, report.CandidateID, report.EvidenceID, report.ClaimID)
		}
		for _, report := range policyReports {
			fmt.Printf("- policy %s: %s status=%s candidate=%s evidence=%s claim=%s\n", report.Lifecycle.ID, report.Verdict, report.Lifecycle.Status, report.CandidateID, report.EvidenceID, report.ClaimID)
		}
		for _, rec := range sourceAuthority {
			fmt.Printf("- source_authority: %s %s -> %s (%s)\n", rec.ID, rec.SourceRole, rec.AuthorityRef, rec.AuthorityStatus)
		}
	case "authority":
		jsonOut := hasFlag(args[1:], "--json")
		sourceKind := decisionSourceKind(value("--source-kind"))
		policyDirs := values("--policy-dir")
		if root := strings.TrimSpace(os.Getenv("COMPOUND_POLICY_DECISIONS_DIR")); root != "" {
			policyDirs = append(policyDirs, root)
		}
		var eligible []string
		eligible = append(eligible, values("--source")...)
		eligible = append(eligible, values("--policy")...)
		eligible = append(eligible, values("--brain")...)
		for _, root := range policyDirs {
			files, err := decisions.EligiblePolicyDecisionFiles(root)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions authority:", err)
				os.Exit(1)
			}
			eligible = append(eligible, files...)
		}
		for _, root := range values("--source-dir") {
			files, err := eligibleDecisionSourceFiles(root, sourceKind)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions authority:", err)
				os.Exit(1)
			}
			eligible = append(eligible, files...)
		}
		report, err := decisions.SourceAuthorityCoverage(homePath, eligible)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions authority:", err)
			os.Exit(1)
		}
		if jsonOut {
			out := struct {
				SchemaVersion   string                            `json:"schema_version"`
				Status          string                            `json:"status"`
				AuthorityRef    string                            `json:"authority_ref"`
				RecordCount     int                               `json:"record_count"`
				Eligible        int                               `json:"eligible"`
				Covered         int                               `json:"covered"`
				Missing         int                               `json:"missing"`
				CountsByKind    map[string]int                    `json:"counts_by_kind"`
				CountsByStatus  map[string]int                    `json:"counts_by_status"`
				EligibleSources []string                          `json:"eligible_sources,omitempty"`
				MissingSources  []string                          `json:"missing_sources,omitempty"`
				Records         []decisions.SourceAuthorityRecord `json:"records,omitempty"`
			}{
				SchemaVersion:   "compound.decision_source_authority_coverage.v0.1",
				Status:          report.Status,
				AuthorityRef:    "compound://registries/decision-source-authority",
				RecordCount:     len(report.Records),
				Eligible:        len(report.Eligible),
				Covered:         len(report.Eligible) - len(report.Missing),
				Missing:         len(report.Missing),
				CountsByKind:    report.CountsByKind,
				CountsByStatus:  report.CountsByStatus,
				EligibleSources: report.Eligible,
				MissingSources:  report.Missing,
				Records:         report.Records,
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", " ")
			if err := enc.Encode(out); err != nil {
				fmt.Fprintln(os.Stderr, "decisions authority:", err)
				os.Exit(1)
			}
			if len(report.Eligible) > 0 && len(report.Missing) > 0 {
				os.Exit(1)
			}
			return
		}
		fmt.Printf("# decisions authority\n\n")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- records: %d\n", len(report.Records))
		fmt.Printf("- eligible: %d\n", len(report.Eligible))
		fmt.Printf("- covered: %d\n", len(report.Eligible)-len(report.Missing))
		fmt.Printf("- missing: %d\n", len(report.Missing))
		for _, kind := range sortedMapKeys(report.CountsByKind) {
			fmt.Printf("- source_kind.%s: %d\n", kind, report.CountsByKind[kind])
		}
		for _, status := range sortedMapKeys(report.CountsByStatus) {
			fmt.Printf("- authority_status.%s: %d\n", status, report.CountsByStatus[status])
		}
		for _, path := range report.Missing {
			fmt.Printf("- missing_source: %s\n", path)
		}
		if len(report.Eligible) > 0 && len(report.Missing) > 0 {
			os.Exit(1)
		}
	case "standing-law":
		// Read-only standing-law view: every ACCEPTED ruling/grant/freeze, its
		// superseded ancestors, corroboration counts, and parked conflict
		// candidates. The operator-facing twin of what orient serves agents
		// (ruling-harvest slice B). Mutates nothing; always exits 0 on success.
		jsonOut := hasFlag(args[1:], "--json")
		view, err := decisions.StandingLaw(store, homePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions standing-law:", err)
			os.Exit(1)
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", " ")
			if err := enc.Encode(view); err != nil {
				fmt.Fprintln(os.Stderr, "decisions standing-law:", err)
				os.Exit(1)
			}
			return
		}
		fmt.Printf("# decisions standing-law\n\n")
		fmt.Printf("- records: %d (rulings %d, grants %d, freezes %d)\n",
			view.Counts.Records, view.Counts.Rulings, view.Counts.Grants, view.Counts.Freezes)
		fmt.Printf("- superseded ancestors: %d\n", view.Counts.Superseded)
		fmt.Printf("- conflicts: %d\n", view.Counts.Conflicts)
		for _, r := range view.Records {
			fmt.Printf("- [%s] %s (corroborations %d", r.Kind, r.Title, r.CorroborationCount)
			if len(r.Supersedes) > 0 {
				fmt.Printf(", supersedes %d", len(r.Supersedes))
			}
			fmt.Printf(")\n")
		}
		for _, c := range view.Conflicts {
			fmt.Printf("- CONFLICT %s: %s (conflicts with %s)\n", c.CandidateID, c.Restatement, c.ConflictsWithClaim)
		}
	case "record":
		jsonOut := hasFlag(args[1:], "--json")
		minTerminalSamples, err := intFlag(args[1:], "--min-terminal-samples")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions record:", err)
			os.Exit(2)
		}
		minWindowDays, err := intFlag(args[1:], "--min-window-days")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions record:", err)
			os.Exit(2)
		}
		maxFailures, err := intFlag(args[1:], "--max-failures")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions record:", err)
			os.Exit(2)
		}
		in := decisions.RecordInput{
			Kind:               value("--kind"),
			ID:                 value("--id"),
			Title:              value("--title"),
			ReviewedBy:         value("--reviewed-by"),
			Scope:              value("--scope"),
			OwnerGlobs:         values("--owner-glob"),
			Keywords:           values("--keyword"),
			Domains:            values("--domain"),
			Falsifier:          value("--falsifier"),
			ReviewTrigger:      value("--review-trigger"),
			UndoOf:             value("--undo-of"),
			AutoAdmitClass:     value("--auto-admit-class"),
			MinTerminalSamples: minTerminalSamples,
			MinWindowDays:      minWindowDays,
			MaxFailures:        maxFailures,
			Now:                time.Now(),
		}
		report, err := decisions.Record(store, homePath, in)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions record:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# decisions record %s\n\n", report.Import.Lifecycle.ID)
		fmt.Printf("- path: %s\n", report.Path)
		fmt.Printf("- kind: %s\n", report.Import.Lifecycle.Kind)
		fmt.Printf("- status: %s\n", report.Import.Lifecycle.Status)
		fmt.Printf("- scope: %s\n", report.Import.Lifecycle.Scope)
		fmt.Printf("- reviewed_by: %s\n", report.Import.Lifecycle.ReviewedBy)
		if report.Import.Lifecycle.AutoAdmitClass != "" {
			fmt.Printf("- auto_admit_class: %s\n", report.Import.Lifecycle.AutoAdmitClass)
			fmt.Printf("- min_terminal_samples: %d\n", report.Import.Lifecycle.MinTerminalSamples)
			fmt.Printf("- min_window_days: %d\n", report.Import.Lifecycle.MinWindowDays)
			fmt.Printf("- max_failures: %d\n", report.Import.Lifecycle.MaxFailures)
		}
		fmt.Printf("- verdict: %s\n", report.Import.Verdict)
		fmt.Printf("- claim: %s\n", report.Import.ClaimID)
	case "doctor":
		writebackEnabled := hasFlag(args[1:], "--writeback")
		claims, err := store.AllCurrent()
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions doctor:", err)
			os.Exit(1)
		}
		cands, err := store.AllCandidates()
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions doctor:", err)
			os.Exit(1)
		}
		report := decisions.DoctorReportFromClaimsAndCandidates(claims, cands)
		fmt.Printf("# decisions doctor\n\n")
		fmt.Printf("- policy: %d\n- doctrine: %d\n- operational: %d\n", report.Counts[decisions.KindPolicy], report.Counts[decisions.KindDoctrine], report.Counts[decisions.KindOperational])
		fmt.Printf("- proposed: %d\n- accepted: %d\n- superseded: %d\n- deprecated: %d\n- rejected: %d\n", report.Statuses[decisions.StatusProposed], report.Statuses[decisions.StatusAccepted], report.Statuses[decisions.StatusSuperseded], report.Statuses[decisions.StatusDeprecated], report.Statuses[decisions.StatusRejected])
		fmt.Printf("- current: %d\n- parked: %d\n", report.Current, report.Parked)
		fmt.Printf("- missing_lifecycle_fields: %d\n", report.MissingLifecycleFields)
		if writebackEnabled && decisions.NeedsWriteback(report) {
			result, err := runDecisionWriteback(report, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions writeback:", err)
				os.Exit(1)
			}
			fmt.Printf("- writeback_gate: %s %s\n", result.GateID, result.GateStatus)
			fmt.Printf("- work: %s\n", result.WorkID)
			if result.ReceiptID != "" {
				fmt.Printf("- receipt: %s\n", result.ReceiptID)
			}
			if result.TransitionID != "" {
				fmt.Printf("- transition: %s\n", result.TransitionID)
			}
		}
	case "repair-plan":
		sourceKind := decisionSourceKind(value("--source-kind"))
		policyDirs := values("--policy-dir")
		sourcePaths := append([]string{}, values("--source")...)
		brainPaths := values("--brain")
		for _, root := range values("--source-dir") {
			files, err := eligibleDecisionSourceFiles(root, sourceKind)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			sourcePaths = append(sourcePaths, files...)
		}
		if root := strings.TrimSpace(os.Getenv("COMPOUND_POLICY_DECISIONS_DIR")); root != "" {
			policyDirs = append(policyDirs, root)
		}
		if len(policyDirs) == 0 && len(sourcePaths) == 0 && len(brainPaths) == 0 {
			fmt.Fprintln(os.Stderr, "usage: compound decisions repair-plan [--policy-dir <dir>] [--source <markdown>] [--source-dir <dir>] [--source-kind note] [--brain <markdown>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# decisions repair-plan\n\n")
		for _, root := range policyDirs {
			plan, err := decisions.BuildLifecycleRepairPlan(root, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			paths, err := decisions.RecordLifecycleRepairPlan(homePath, plan)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			fmt.Printf("- source_root: %s\n", root)
			fmt.Printf("  status: %s\n", plan.Status)
			fmt.Printf("  repairs: %d\n", plan.RepairCount)
			fmt.Printf("  missing_falsifier: %d\n", plan.MissingFieldCounts["falsifier"])
			fmt.Printf("  missing_review_trigger: %d\n", plan.MissingFieldCounts["review_trigger"])
			fmt.Printf("  plan: %s\n", paths.PlanPath)
			fmt.Printf("  review: %s\n", paths.ReviewPath)
		}
		if len(sourcePaths) > 0 {
			plan, err := decisions.BuildSourceLifecycleRepairPlan(sourcePaths, sourceKind, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			paths, err := decisions.RecordLifecycleRepairPlan(homePath, plan)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			fmt.Printf("- source_root: %s\n", plan.SourceRoot)
			fmt.Printf(" source_kind: %s\n", plan.SourceKind)
			fmt.Printf(" status: %s\n", plan.Status)
			fmt.Printf(" repairs: %d\n", plan.RepairCount)
			fmt.Printf(" missing_falsifier: %d\n", plan.MissingFieldCounts["falsifier"])
			fmt.Printf(" missing_review_trigger: %d\n", plan.MissingFieldCounts["review_trigger"])
			fmt.Printf(" plan: %s\n", paths.PlanPath)
			fmt.Printf(" review: %s\n", paths.ReviewPath)
		}
		if len(brainPaths) > 0 {
			plan, err := decisions.BuildBrainLifecycleRepairPlan(brainPaths, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			paths, err := decisions.RecordLifecycleRepairPlan(homePath, plan)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-plan:", err)
				os.Exit(1)
			}
			fmt.Printf("- source_root: %s\n", plan.SourceRoot)
			fmt.Printf(" source_kind: %s\n", plan.SourceKind)
			fmt.Printf(" status: %s\n", plan.Status)
			fmt.Printf(" repairs: %d\n", plan.RepairCount)
			fmt.Printf(" missing_falsifier: %d\n", plan.MissingFieldCounts["falsifier"])
			fmt.Printf(" missing_review_trigger: %d\n", plan.MissingFieldCounts["review_trigger"])
			fmt.Printf(" plan: %s\n", paths.PlanPath)
			fmt.Printf(" review: %s\n", paths.ReviewPath)
		}
	case "repair-apply":
		plans := values("--plan")
		if len(plans) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound decisions repair-apply --plan <repair-plan.json> [--write-reviewed]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions repair-apply:", err)
			os.Exit(1)
		}
		plan, err := decisions.LoadLifecycleRepairPlan(plans[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions repair-apply:", err)
			os.Exit(1)
		}
		dryRun := !hasFlag(args[1:], "--write-reviewed")
		report, err := decisions.ApplyLifecycleRepairPlan(plan, decisions.RepairApplyOptions{DryRun: dryRun}, time.Now())
		if err != nil {
			if report.ID != "" {
				reportPath, recordErr := decisions.RecordLifecycleRepairApplyReport(homePath, report)
				if recordErr == nil {
					fmt.Fprintf(os.Stderr, "decisions repair-apply report: %s\n", reportPath)
				} else {
					fmt.Fprintf(os.Stderr, "decisions repair-apply report: %v\n", recordErr)
				}
			}
			fmt.Fprintln(os.Stderr, "decisions repair-apply:", err)
			os.Exit(1)
		}
		reportPath, err := decisions.RecordLifecycleRepairApplyReport(homePath, report)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions repair-apply:", err)
			os.Exit(1)
		}
		if !dryRun && decisions.LifecycleRepairApplyTerminal(report) {
			if _, err := decisions.RecordAppliedLifecycleRepairPlan(homePath, plan); err != nil {
				fmt.Fprintln(os.Stderr, "decisions repair-apply:", err)
				os.Exit(1)
			}
		}
		fmt.Printf("# decisions repair-apply\n\n")
		fmt.Printf("- plan: %s\n", report.PlanID)
		fmt.Printf("- dry_run: %v\n", report.DryRun)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- applied_fields: %d\n", report.AppliedFields)
		fmt.Printf("- changed_files: %d\n", len(report.ChangedFiles))
		fmt.Printf("- skipped: %d\n", len(report.Skipped))
		fmt.Printf("- report: %s\n", reportPath)
	default:
		kindFilter := ""
		for _, a := range args {
			if strings.HasPrefix(a, "--kind=") {
				kindFilter = strings.TrimPrefix(a, "--kind=")
			}
		}
		claims, err := store.AllCurrent()
		if err != nil {
			fmt.Fprintln(os.Stderr, "decisions:", err)
			os.Exit(1)
		}
		any := false
		for _, c := range claims {
			if c.ClaimType != "decision" {
				continue
			}
			k := decisions.Kind(c)
			if kindFilter != "" && k != kindFilter {
				continue
			}
			l := decisions.LifecycleFromClaim(c)
			fmt.Printf("- [%s/%s] %s - %s\n", l.Kind, l.Status, c.ID, c.Object)
			any = true
		}
		if !any {
			fmt.Println("(no decisions - run compound decisions import)")
		}
	}
}
