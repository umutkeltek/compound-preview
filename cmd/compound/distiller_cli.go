// distiller_cli.go owns the `compound distiller ...` verb family: the
// dispatcher (cmdDistiller) and its subcommand handlers (cmdDistillerRequest,
// cmdDistillerExtract), plus the small output-formatting helpers
// (printDistillerEval, printDistillerEvalCorpus) the dispatcher routes
// into. Extracted from cmd/compound/main.go so main.go holds only the
// cross-cutting dispatch table + non-distiller verbs. The distiller
// runtime (the actual candidate-set + policy + eval-plan engines) lives
// in compound/internal/distiller and is unchanged; this file is only
// the CLI surface that drives it.
//
// autoConsumeDistillerCapabilityFeedback lives in main.go (called from
// cmdDistiller's close path). Same package, no boundary change.
//
// Composition with the other CLI verb files:
//   - memory_cli.go                 (cmdMemory — smallest verb)
//   - this file                     (cmdDistiller — second verb extracted)
//   - cmd/compound/distiller.go     (does not exist; the runtime is internal/distiller)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/sourceobs"
)

// findingIDsFromArgs collects every --finding occurrence (repeatable) so callers can pass
// one or more finding ids without a separate separator. Empty / whitespace-only values are
// dropped; duplicates are not de-duplicated here (the builder de-dups and resolves).
func findingIDsFromArgs(args []string) []string {
	var ids []string
	for _, id := range valuesFlag(args, "--finding") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func cmdDistiller(args []string) {
	if len(args) == 1 && args[0] == "prior-state" {
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		store, err := mem.Open(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		prior, err := distiller.LoadPriorStateFromHome(homePath, store, distiller.DefaultPolicy())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller prior-state:", err)
			os.Exit(1)
		}
		cov := prior.Coverage()
		fmt.Println("# distiller prior-state")
		fmt.Printf("- policy: %s\n", prior.PolicyVersion)
		fmt.Printf("- current_claims: %d\n", cov.CurrentClaims)
		fmt.Printf("- claims: decisions=%d preferences=%d architectures=%d statuses=%d constraints=%d\n",
			cov.Decisions, cov.Preferences, cov.Architectures, cov.Statuses, cov.Constraints)
		fmt.Printf("- candidates: skills=%d decisions=%d failures=%d taste=%d product_hypotheses=%d routes=%d capability_observations=%d eval_cases=%d method_upgrades=%d policy_patches=%d\n",
			cov.SkillCandidates, cov.DecisionCandidates, cov.FailurePatterns, cov.TasteObservations, cov.ProductHypotheses, cov.RouteObservations, cov.CapabilityObs, cov.EvalCases, cov.MethodUpgrades, cov.PolicyPatches)
		fmt.Printf("- capability_nodes: %d\n", cov.CapabilityNodes)
		fmt.Printf("- no_promote_decisions: %d\n", cov.NoPromoteDecisions)
		return
	}
	if len(args) >= 1 && args[0] == "import-conversation-coverage" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller import-conversation-coverage <coverage-summary.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		now := time.Now()
		coverage, err := distiller.LoadConversationCoverage(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller import-conversation-coverage:", err)
			os.Exit(1)
		}
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromConversationCoverage(sourceobs.ConversationCoverageObservation{
			ID:                coverage.ID,
			ReportPath:        coverage.ReportPath,
			Revision:          coverage.Revision,
			SourceCount:       coverage.SourceCount,
			TotalRows:         coverage.TotalRows,
			RawFiles:          coverage.RawFiles,
			MappedClean:       coverage.MappedClean,
			UnmappedRaw:       coverage.UnmappedRaw,
			Stable:            coverage.Stable,
			DreamCandidates:   coverage.DreamCandidates,
			MissingEmbeddings: coverage.MissingEmbeddings,
			AvgRetainedRatio:  coverage.AvgRetainedRatio,
			TopTagCount:       coverage.TopTagCount,
			ReviewDebt:        coverage.ReviewDebt,
			EvidenceRefs:      coverage.EvidenceRefs,
		}, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller import-conversation-coverage %s\n", coverage.ID)
		fmt.Printf("- sources: %d\n", coverage.SourceCount)
		fmt.Printf("- total_rows: %d\n", coverage.TotalRows)
		fmt.Printf("- raw_files: %d\n", coverage.RawFiles)
		fmt.Printf("- mapped_clean: %d\n", coverage.MappedClean)
		fmt.Printf("- unmapped_raw: %d\n", coverage.UnmappedRaw)
		fmt.Printf("- dream_candidates: %d\n", coverage.DreamCandidates)
		fmt.Printf("- missing_embeddings: %d\n", coverage.MissingEmbeddings)
		fmt.Printf("- avg_retained_ratio: %.3f\n", coverage.AvgRetainedRatio)
		fmt.Printf("- review_debt: %d\n", len(coverage.ReviewDebt))
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		if len(sourceReport.CandidateIDs) > 0 {
			fmt.Printf("- source_candidate: %s\n", sourceReport.CandidateIDs[0])
			fmt.Printf("- review_command: compound distiller improvement-plan --candidate %s\n", sourceReport.CandidateIDs[0])
		}
		if len(sourceReport.NoPromoteIDs) > 0 {
			fmt.Printf("- source_no_promote: %s\n", sourceReport.NoPromoteIDs[0])
		}
		fmt.Println("- row_import: no")
		fmt.Println("- dream_execution: no")
		fmt.Println("- memory_promotion: no")
		return
	}
	if len(args) >= 1 && args[0] == "import-conversation-mining" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller import-conversation-mining <mining-report.json> [--mined-tags <mined-tags.jsonl>] [--lossiness <lossiness.json>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		now := time.Now()
		mining, err := distiller.LoadConversationMining(args[1], distiller.ConversationMiningOptions{
			MinedTagsPath: valueFlag(args[2:], "--mined-tags"),
			LossinessPath: valueFlag(args[2:], "--lossiness"),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller import-conversation-mining:", err)
			os.Exit(1)
		}
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromConversationMining(sourceobs.ConversationMiningObservation{
			ID:                mining.ID,
			ReportPath:        mining.ReportPath,
			MinedTagsPath:     mining.MinedTagsPath,
			LossinessPath:     mining.LossinessPath,
			Revision:          mining.Revision,
			Processed:         mining.Report.Processed,
			Accepted:          mining.Report.Accepted,
			Rejected:          mining.Report.Rejected,
			MinedRows:         mining.MinedRows,
			AcceptedForReview: mining.AcceptedForReview,
			DreamPackFiles:    mining.Report.DreamPackFiles,
			DreamExecution:    mining.DreamExecution(),
			LossinessRecorded: mining.LossinessRecorded,
			LossinessDetail:   mining.LossinessDetail,
			Note:              mining.Report.Note,
			EvidenceRefs:      mining.EvidenceRefs,
		}, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller import-conversation-mining %s\n", mining.ID)
		fmt.Printf("- status: %s\n", mining.Status())
		fmt.Printf("- processed: %d\n", mining.Report.Processed)
		fmt.Printf("- accepted: %d\n", mining.Report.Accepted)
		fmt.Printf("- rejected: %d\n", mining.Report.Rejected)
		fmt.Printf("- mined_rows: %d\n", mining.MinedRows)
		fmt.Printf("- accepted_for_review: %d\n", mining.AcceptedForReview)
		fmt.Printf("- dream_execution: %s\n", mining.DreamExecution())
		fmt.Printf("- dream_pack_files: %d\n", mining.Report.DreamPackFiles)
		fmt.Printf("- lossiness_recorded: %t\n", mining.LossinessRecorded)
		for _, tag := range mining.TopTags {
			fmt.Printf("- top_tag: %s\n", tag)
		}
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		for _, id := range sourceReport.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
			fmt.Printf("- review_command: compound distiller improvement-plan --candidate %s\n", id)
		}
		for _, id := range sourceReport.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		return
	}
	if len(args) >= 1 && args[0] == "import-session-lenses" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller import-session-lenses <multi-lens.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		now := time.Now()
		lenses, err := distiller.LoadSessionLenses(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller import-session-lenses:", err)
			os.Exit(1)
		}
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromSessionLenses(sourceobs.SessionLensObservation{
			ID:             lenses.ID,
			SourcePath:     lenses.SourcePath,
			SourceRevision: lenses.Revision,
			Decisions:      lenses.Decisions,
			Outcomes:       lenses.Outcomes,
			Patterns:       lenses.Patterns,
			Skills:         lenses.Skills,
			Taste:          lenses.Taste,
			Contradictions: lenses.Contradictions,
			TotalItems:     lenses.TotalItems,
			EvidenceRefs:   lenses.EvidenceRefs,
		}, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller import-session-lenses %s\n", lenses.ID)
		fmt.Printf("- decisions: %d\n", lenses.Decisions)
		fmt.Printf("- outcomes: %d\n", lenses.Outcomes)
		fmt.Printf("- patterns: %d\n", lenses.Patterns)
		fmt.Printf("- skills: %d\n", lenses.Skills)
		fmt.Printf("- taste: %d\n", lenses.Taste)
		fmt.Printf("- contradictions: %d\n", lenses.Contradictions)
		fmt.Printf("- total_items: %d\n", lenses.TotalItems)
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		for _, id := range sourceReport.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
			fmt.Printf("- review_command: compound distiller improvement-plan --candidate %s\n", id)
		}
		for _, id := range sourceReport.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		fmt.Println("- row_import: no")
		fmt.Println("- memory_promotion: no")
		return
	}
	if len(args) >= 1 && args[0] == "doctor" {
		writebackEnabled := hasFlag(args[1:], "--writeback")
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		report, err := distiller.Doctor(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("# distiller doctor")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- adjudications: %d\n", report.TotalDecisions)
		fmt.Printf("- memory: promoted=%d parked=%d rejected=%d duplicate_rejected=%d\n",
			report.MemoryPromoted, report.MemoryParked, report.MemoryRejected, report.DuplicateMemoryRejections)
		fmt.Printf("- duplicate_candidates: rejected=%d\n", report.DuplicateCandidateRejections)
		fmt.Printf("- no_promote: total=%d duplicate_memory=%d duplicate_candidates=%d\n",
			report.NoPromoteRows, report.DuplicateMemoryNoPromoteRows, report.DuplicateCandidateNoPromoteRows)
		fmt.Printf("- eval_cases: %d\n", report.EvalCases)
		fmt.Printf("- method_policy_debt: method_upgrades=%d policy_patches=%d total=%d\n",
			report.MethodUpgradeDebt, report.PolicyPatchDebt, report.SelfImprovementDebt)
		fmt.Printf("- capability_observations: candidates=%d operationalized=%d\n",
			report.CapabilityObservations, report.OperationalizedCapabilityObservations)
		for _, check := range report.Checks {
			fmt.Printf("- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Printf(" (%s)", check.Detail)
			}
			fmt.Println()
		}
		if writebackEnabled && distiller.NeedsWriteback(report) {
			result, err := runDistillerWriteback(report, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "distiller writeback:", err)
				os.Exit(1)
			}
			fmt.Printf("- writeback_gate: %s %s\n", result.GateID, result.GateStatus)
			fmt.Printf("- work: %s\n", result.WorkID)
			fmt.Printf("- receipt: %s\n", result.ReceiptID)
			fmt.Printf("- transition: %s\n", result.TransitionID)
		}
		return
	}
	if len(args) >= 1 && args[0] == "eval-plan" {
		candidateID := valueFlag(args[1:], "--candidate")
		corpusDir := valueFlag(args[1:], "--corpus")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound distiller eval-plan --candidate <candidate_id> [--corpus <dir>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		plan, err := distiller.BuildEvalPlan(homePath, candidateID, corpusDir, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller eval-plan:", err)
			os.Exit(1)
		}
		paths, err := distiller.RecordEvalPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller eval-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller eval-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- proposed_case: %s\n", plan.FixtureDraft.ProposedCaseID)
		fmt.Printf("- request_trace_ref: %s\n", plan.FixtureDraft.RequestTraceRef)
		if plan.TargetCorpusDir != "" {
			fmt.Printf("- target_corpus: %s\n", plan.TargetCorpusDir)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_artifacts: %d\n", len(plan.FixtureDraft.RequiredArtifacts))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		return
	}
	if len(args) >= 1 && args[0] == "improvement-plan" {
		candidateID := valueFlag(args[1:], "--candidate")
		targetPath := valueFlag(args[1:], "--target")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound distiller improvement-plan --candidate <candidate_id> [--target <file>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		plan, err := distiller.BuildImprovementPlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller improvement-plan:", err)
			os.Exit(1)
		}
		paths, err := distiller.RecordImprovementPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller improvement-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller improvement-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- candidate_kind: %s\n", plan.CandidateKind)
		fmt.Printf("- improvement_key: %s\n", plan.ImprovementKey)
		fmt.Printf("- improvement_id: %s\n", plan.Draft.ImprovementID)
		fmt.Printf("- area: %s\n", plan.Draft.Area)
		if plan.TargetPath != "" {
			fmt.Printf("- target: %s\n", plan.TargetPath)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(plan.Draft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
		return
	}
	if len(args) >= 1 && args[0] == "improvement-review" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller improvement-review <review.yaml>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		review, err := distiller.LoadImprovementReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller improvement-review:", err)
			os.Exit(1)
		}
		report, err := distiller.RecordImprovementReview(homePath, review, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller improvement-review:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller improvement-review %s\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- reviewed: %s\n", report.ReviewedPath)
		return
	}
	if len(args) >= 1 && args[0] == "rule-plan" {
		candidateID := valueFlag(args[1:], "--candidate")
		targetPath := valueFlag(args[1:], "--target")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound distiller rule-plan --candidate <candidate_id> [--target <file>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		plan, err := distiller.BuildFailureRulePlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller rule-plan:", err)
			os.Exit(1)
		}
		paths, err := distiller.RecordFailureRulePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller rule-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller rule-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- failure_key: %s\n", plan.FailureKey)
		fmt.Printf("- rule_id: %s\n", plan.RuleDraft.RuleID)
		if plan.TargetPath != "" {
			fmt.Printf("- target: %s\n", plan.TargetPath)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(plan.RuleDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		return
	}
	if len(args) >= 1 && args[0] == "taste-plan" {
		candidateID := valueFlag(args[1:], "--candidate")
		targetPath := valueFlag(args[1:], "--target")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound distiller taste-plan --candidate <candidate_id> [--target <file>]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		plan, err := distiller.BuildTastePlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller taste-plan:", err)
			os.Exit(1)
		}
		paths, err := distiller.RecordTastePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller taste-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# distiller taste-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- taste_key: %s\n", plan.TasteKey)
		fmt.Printf("- standard_id: %s\n", plan.StandardDraft.StandardID)
		if plan.TargetPath != "" {
			fmt.Printf("- target: %s\n", plan.TargetPath)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(plan.StandardDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		return
	}
	if len(args) >= 1 && args[0] == "eval" {
		record := hasFlag(args[1:], "--record")
		var paths []string
		for _, arg := range args[1:] {
			if arg == "--record" {
				continue
			}
			paths = append(paths, arg)
		}
		if len(paths) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller eval <case.json|cases-dir> [--record]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		targetPath := paths[0]
		info, err := os.Stat(targetPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller eval:", err)
			os.Exit(1)
		}
		if info.IsDir() {
			report, err := distiller.EvaluateCorpusDirInRoot(targetPath, homePath, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "distiller eval:", err)
				os.Exit(1)
			}
			printDistillerEvalCorpus(report)
			if report.Status != "pass" {
				os.Exit(1)
			}
			if record {
				proof, err := distiller.RecordEvalCorpusReport(homePath, report)
				if err != nil {
					fmt.Fprintln(os.Stderr, "distiller eval record:", err)
					os.Exit(1)
				}
				fmt.Printf("- proof_ref: %s\n", proof.ProofRef)
				fmt.Printf("- proof: %s\n", proof.Path)
			}
			return
		}
		report, err := distiller.EvaluateCaseFileInRoot(targetPath, homePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller eval:", err)
			os.Exit(1)
		}
		printDistillerEval(report)
		if report.Status != "pass" {
			os.Exit(1)
		}
		if record {
			proof, err := distiller.RecordEvalReport(homePath, report)
			if err != nil {
				fmt.Fprintln(os.Stderr, "distiller eval record:", err)
				os.Exit(1)
			}
			fmt.Printf("- proof_ref: %s\n", proof.ProofRef)
			fmt.Printf("- proof: %s\n", proof.Path)
		}
		return
	}
	if len(args) >= 1 && args[0] == "request" {
		cmdDistillerRequest(args[1:])
		return
	}
	if len(args) >= 1 && args[0] == "extract" {
		cmdDistillerExtract(args[1:])
		return
	}
	if len(args) < 2 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: compound distiller eval <case.json|cases-dir> [--record] | eval-plan --candidate C [--corpus dir] | rule-plan --candidate C [--target file] | taste-plan --candidate C [--target file] | request --trace-ref T [--work-id W] [--raw f] [--context-pack f] [--work f] [--receipt f] [--envelope f]... [--event-trace trace_id]... [--finding ID]... [--record] | extract --request <request.json|distiller_request:id> --eval-corpus dir --openai-compatible-url URL --model M [--api-key-env ENV] | import-conversation-mining <report.json> [--mined-tags rows.jsonl] [--lossiness lossiness.json] | import-session-lenses <multi-lens.json> | run [--llm-response response.json [--request <request.json|distiller_request:id>]|trace.json] | doctor [--writeback] | prior-state")
		os.Exit(2)
	}

	extractor := distiller.Extractor(distiller.ReferenceExtractor{})
	tracePath := args[1]
	homePath := ""
	if args[1] == "--llm-response" {
		if len(args) != 3 && len(args) != 5 {
			fmt.Fprintln(os.Stderr, "usage: compound distiller run --llm-response <response.json> [--request <request.json|distiller_request:id>]")
			os.Exit(2)
		}
		tracePath = args[2]
		extractor = distiller.LLMExtractor{}
		if len(args) == 5 {
			if args[3] != "--request" || strings.TrimSpace(args[4]) == "" {
				fmt.Fprintln(os.Stderr, "usage: compound distiller run --llm-response <response.json> [--request <request.json|distiller_request:id>]")
				os.Exit(2)
			}
			var err error
			homePath, err = home.Resolve("")
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
				os.Exit(1)
			}
			req, err := distiller.LoadLLMExtractionRequest(homePath, args[4])
			if err != nil {
				fmt.Fprintln(os.Stderr, "distiller run request:", err)
				os.Exit(1)
			}
			extractor = distiller.NewGroundedLLMExtractor(req)
		}
	}
	if homePath == "" {
		var err error
		homePath, err = home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	res, err := distiller.Run(homePath, store, data, distiller.DefaultPolicy(), extractor, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("# distilled %s under policy %s — %d candidate(s)\n", res.TraceRef, res.Policy, len(res.Decisions))
	for _, d := range res.Decisions {
		extra := ""
		if d.ClaimID != "" {
			extra = " → claim " + d.ClaimID
		} else if d.NoPromoteID != "" {
			extra = " → no_promote " + d.NoPromoteID
		}
		fmt.Printf("%-22s %-14s %s%s\n", d.Kind, d.Verdict, d.Basis, extra)
	}
	autoConsumeDistillerCapabilityFeedback(homePath)
}

func printDistillerEval(report distiller.EvalReport) {
	fmt.Printf("# distiller eval %s\n", report.ID)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- trace_ref: %s\n", report.TraceRef)
	fmt.Printf("- decisions: %d\n", report.Run.DecisionCount)
	fmt.Printf("- current_claims: %d\n", report.Run.CurrentClaims)
	fmt.Printf("- live_model_calls: %d\n", report.Run.LiveModelCalls)
	if report.Run.Error != "" {
		fmt.Printf("- error: %s\n", report.Run.Error)
	}
	fmt.Printf("- prior: current_claims=%d skill_candidates=%d decision_candidates=%d route_observations=%d capability_observations=%d eval_cases=%d method_upgrades=%d policy_patches=%d\n",
		report.Request.PriorCoverage.CurrentClaims,
		report.Request.PriorCoverage.SkillCandidates,
		report.Request.PriorCoverage.DecisionCandidates,
		report.Request.PriorCoverage.RouteObservations,
		report.Request.PriorCoverage.CapabilityObs,
		report.Request.PriorCoverage.EvalCases,
		report.Request.PriorCoverage.MethodUpgrades,
		report.Request.PriorCoverage.PolicyPatches)
	for _, check := range report.Checks {
		fmt.Printf("- check:%s %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Printf(" (%s)", check.Detail)
		}
		fmt.Println()
	}
}

func printDistillerEvalCorpus(report distiller.EvalCorpusReport) {
	fmt.Printf("# distiller eval corpus %s\n", report.Dir)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- cases: %d\n", report.CaseCount)
	fmt.Printf("- pass: %d\n", report.PassCount)
	fmt.Printf("- fail: %d\n", report.FailCount)
	fmt.Printf("- decisions: %d\n", report.DecisionCount)
	fmt.Printf("- current_claims: %d\n", report.CurrentClaims)
	fmt.Printf("- live_model_calls: %d\n", report.LiveModelCalls)
	for _, caseReport := range report.Reports {
		fmt.Printf("- case:%s %s decisions=%d current_claims=%d live_model_calls=%d",
			caseReport.ID,
			caseReport.Status,
			caseReport.Run.DecisionCount,
			caseReport.Run.CurrentClaims,
			caseReport.Run.LiveModelCalls)
		if caseReport.Run.Error != "" {
			fmt.Printf(" error=%q", caseReport.Run.Error)
		}
		fmt.Println()
	}
}

func cmdDistillerRequest(args []string) {
	traceRef := valueFlag(args, "--trace-ref")
	findingIDs := findingIDsFromArgs(args)
	// Default the trace_ref to `finding:<first-id>` BEFORE the required-trace check so the
	// operator does not have to repeat themselves. An explicit --trace-ref still wins.
	if strings.TrimSpace(traceRef) == "" && len(findingIDs) > 0 {
		traceRef = distiller.FindingSourcePrefix + findingIDs[0]
	}
	if traceRef == "" {
		fmt.Fprintln(os.Stderr, "usage: compound distiller request --trace-ref T [--work-id W] [--raw f] [--context-pack f] [--work f] [--receipt f] [--envelope f]... [--event-trace trace_id]... [--finding ID]... [--record]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	prior, err := distiller.LoadPriorStateFromHome(homePath, store, distiller.DefaultPolicy())
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller request prior-state:", err)
		os.Exit(1)
	}
	// When --finding is supplied, derive a ref-only extraction request from the finding ids.
	if len(findingIDs) > 0 {
		if valueFlag(args, "--raw") != "" || valueFlag(args, "--work-id") != "" || valueFlag(args, "--context-pack") != "" || valueFlag(args, "--work") != "" || valueFlag(args, "--receipt") != "" || valueFlag(args, "--envelope") != "" || hasFlag(args, "--event-trace") {
			fmt.Fprintln(os.Stderr, "distiller request: --finding cannot be combined with --raw/--work-id/--context-pack/--work/--receipt/--envelope/--event-trace")
			os.Exit(2)
		}
		req, err := distiller.BuildLLMExtractionRequestFromFindings(homePath, traceRef, findingIDs, distiller.DefaultPolicy(), prior)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller request findings:", err)
			os.Exit(1)
		}
		out, err := json.MarshalIndent(req, "", " ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller request encode:", err)
			os.Exit(1)
		}
		if hasFlag(args, "--record") {
			record, err := distiller.RecordLLMExtractionRequest(homePath, req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "distiller request record:", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "- request_ref: %s\n", record.Ref)
			fmt.Fprintf(os.Stderr, "- request: %s\n", record.Path)
		}
		fmt.Println(string(out))
		return
	}
	src := distiller.LLMExtractionSources{}
	if workID := valueFlag(args, "--work-id"); workID != "" {
		if valueFlag(args, "--context-pack") != "" || valueFlag(args, "--work") != "" || valueFlag(args, "--receipt") != "" {
			fmt.Fprintln(os.Stderr, "distiller request: --work-id cannot be combined with --context-pack, --work, or --receipt")
			os.Exit(2)
		}
		src, err = distiller.LoadRuntimeSources(homePath, workID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller request runtime sources:", err)
			os.Exit(1)
		}
	}
	if rawPath := valueFlag(args, "--raw"); rawPath != "" {
		src.RawTrace, err = os.ReadFile(rawPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read raw trace:", err)
			os.Exit(1)
		}
	}
	if path := valueFlag(args, "--context-pack"); path != "" {
		env := readTypedEnvelopeOrDie(path)
		ctx, ok := env.(*envelope.ContextPack)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s is %s, want context_pack\n", path, env.Kind())
			os.Exit(1)
		}
		src.ContextPack = ctx
	}
	if path := valueFlag(args, "--work"); path != "" {
		env := readTypedEnvelopeOrDie(path)
		w, ok := env.(*envelope.WorkEnvelope)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s is %s, want work\n", path, env.Kind())
			os.Exit(1)
		}
		src.Work = w
	}
	if path := valueFlag(args, "--receipt"); path != "" {
		env := readTypedEnvelopeOrDie(path)
		rec, ok := env.(*envelope.Receipt)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s is %s, want receipt\n", path, env.Kind())
			os.Exit(1)
		}
		src.Receipt = rec
	}
	for _, path := range valuesFlag(args, "--envelope") {
		src.Extra = append(src.Extra, readTypedEnvelopeOrDie(path))
	}
	if traceIDs := valuesFlag(args, "--event-trace"); len(traceIDs) > 0 {
		traces, err := distiller.LoadTraceEnvelopes(homePath, traceIDs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller request event traces:", err)
			os.Exit(1)
		}
		src.Extra = append(src.Extra, traces...)
	}
	req, err := distiller.BuildLLMExtractionRequestFromSources(traceRef, src, distiller.DefaultPolicy(), prior)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller request:", err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller request encode:", err)
		os.Exit(1)
	}
	if hasFlag(args, "--record") {
		record, err := distiller.RecordLLMExtractionRequest(homePath, req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "distiller request record:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "- request_ref: %s\n", record.Ref)
		fmt.Fprintf(os.Stderr, "- request: %s\n", record.Path)
	}
	fmt.Println(string(out))
}

func cmdDistillerExtract(args []string) {
	requestPath := valueFlag(args, "--request")
	evalCorpus := valueFlag(args, "--eval-corpus")
	baseURL := valueFlag(args, "--openai-compatible-url")
	if baseURL == "" {
		baseURL = valueFlag(args, "--base-url")
	}
	model := valueFlag(args, "--model")
	if requestPath == "" || evalCorpus == "" || baseURL == "" || model == "" {
		fmt.Fprintln(os.Stderr, "usage: compound distiller extract --request <request.json|distiller_request:id> --eval-corpus <cases-dir> --openai-compatible-url <url> --model <model> [--api-key-env ENV]")
		os.Exit(2)
	}
	apiKey := ""
	if env := valueFlag(args, "--api-key-env"); env != "" {
		apiKey = os.Getenv(env)
		if apiKey == "" {
			fmt.Fprintf(os.Stderr, "distiller extract: --api-key-env %s is empty\n", env)
			os.Exit(1)
		}
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	req, err := distiller.LoadLLMExtractionRequest(homePath, requestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract request:", err)
		os.Exit(1)
	}
	// Privacy-sensitive route admission for finding-grounded extract is BLOCKED on a missing
	// governed local endpoint/execution adapter (route lanes carry ProviderRoute IDs like
	// `local/mac-fast`; cmdDistillerExtract takes an HTTP baseURL, and there is no existing
	// structured field that maps them). Until that lands, refuse to call the provider for a
	// finding-grounded request — a caller-controlled --openai-compatible-url on a private
	// (source_privacy=private) request would otherwise leak finding bodies to an uncontrolled
	// endpoint. The ref-only builder/hydrator in internal/distiller remain as branch evidence
	// for the later synthetic certifier step.
	if len(req.FindingRefs) > 0 {
		fmt.Fprintln(os.Stderr, "distiller extract: refusal — privacy-sensitive route admission for finding-grounded requests is gated on a missing governed local endpoint/execution adapter; refusing to send to a caller-controlled --openai-compatible-url.")
		os.Exit(1)
	}
	now := time.Now()
	report, err := distiller.ExtractWithOpenAICompatible(context.Background(), req, distiller.OpenAICompatibleOptions{
		BaseURL:       baseURL,
		APIKey:        apiKey,
		Model:         model,
		EvalCorpusDir: evalCorpus,
		EvalRoot:      homePath,
		Now:           now,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract:", err)
		os.Exit(1)
	}
	routeEvidence, err := distiller.BuildExecutionRouteEvidence(req, report, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract route evidence:", err)
		os.Exit(1)
	}
	routeEvidence, routeTransition, err := distiller.JoinExecutionRouteTransition(routeEvidence)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract route transition:", err)
		os.Exit(1)
	}
	if err := distiller.RecordExecutionRouteEvidence(homePath, routeEvidence); err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract route evidence:", err)
		os.Exit(1)
	}
	if err := distiller.RecordExecutionRouteTransition(homePath, routeTransition); err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract route transition:", err)
		os.Exit(1)
	}
	fmt.Printf("# distiller extract %s\n", report.Model)
	fmt.Printf("- provider: %s\n", report.Provider)
	fmt.Printf("- eval_status: %s\n", report.Eval.Status)
	fmt.Printf("- eval_cases: %d\n", report.Eval.CaseCount)
	fmt.Printf("- live_model_calls: %d\n", report.LiveModelCalls)
	fmt.Printf("- route_evidence: %s\n", routeEvidence.ID)
	fmt.Printf("- route_transition: %s\n", routeTransition.ID)
	fmt.Printf("- candidates: %d\n", len(report.Response.Candidates))
	out, err := json.MarshalIndent(report.Response, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "distiller extract encode:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
