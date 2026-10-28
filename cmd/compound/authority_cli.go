// authority_cli.go owns the `compound authority ...` verb dispatcher
// (cmdAuthority). Extracted from cmd/compound/main.go so main.go holds only
// the cross-cutting dispatch table + non-authority verbs. The authority
// runtime (path authority, OS absorption, job-roster, harness-edit review,
// job-health plan/review, worktree-retire seams) lives in
// compound/internal/authority and is unchanged; this file is only the CLI
// surface that drives it.
//
// Composition with the other CLI verb files:
//   - memory_cli.go                 (cmdMemory)
//   - memory_engine_config.go       (engine config subsystem)
//   - distiller_cli.go              (cmdDistiller)
//   - decisions_cli.go              (cmdDecisions)
//   - policy_cli.go                 (cmdPolicy)
//   - this file                     (cmdAuthority — fifth verb extracted)
//
// Subcommand surface (all in cmdAuthority's switch):
//   - import                       (registry YAML)
//   - path                         (path authority YAML)
//   - path-registry                (os-path-authority YAML)
//   - git                          (repo path)
//   - dirty-plan                   (action intents + work)
//   - worktree-retire              (plan/review/apply seams)
//   - import-os-absorption
//   - os-absorption
//   - os-absorption-doctor
//   - import-infra-database
//   - import-infra-services
//   - import-model-catalog
//   - model-catalog              (read-only query: rows + source revision)
//   - model-catalog-doctor       (refresh reflex: drift/ok/source-absent)
//   - import-job-roster
//   - import-job-token-governance
//   - job-roster-authority
//   - job-health-plan
//   - job-health-review
//   - harness-edit-review
//   - resolve
//   - <entity>                     (default: read by entity)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/authority"
	"compound/internal/mem"

	"gopkg.in/yaml.v3"
)

const gitIndexLockUsage = "usage: compound authority git-index-lock <plan --repo ROOT --work WORK --actor ACTOR | review --plan ID --operator operator:ID --evidence REF --writers-paused --expires RFC3339 | apply --plan ID --review ID --actor ACTOR>"

func runAuthorityGitIndexLock(home string, args []string, out, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, gitIndexLockUsage)
		return 2
	}
	flags := flag.NewFlagSet("git-index-lock "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", false, "emit JSON (always enabled; private patches are never printed)")
	var run func() (any, error)
	switch args[0] {
	case "plan":
		repo := flags.String("repo", "", "primary checkout root")
		work := flags.String("work", "", "running owner work")
		actor := flags.String("actor", "", "current lease holder")
		run = func() (any, error) {
			plan, err := authority.BuildGitIndexLockPlan(context.Background(), *repo, *work, *actor, nil, time.Now())
			if err != nil {
				return nil, err
			}
			if err := authority.RecordGitIndexLockPlan(home, plan, time.Now()); err != nil {
				return nil, err
			}
			return map[string]any{"plan_id": plan.ID, "plan_digest": plan.Digest, "repo": plan.State.Snapshot.Repo, "head": plan.State.Snapshot.Head, "lock": plan.State.Snapshot.Lock, "notice": "Plan only. Applying requires a separate current all-writer pause commitment."}, nil
		}
	case "review":
		plan := flags.String("plan", "", "exact plan ID")
		operator := flags.String("operator", "", "operator provenance")
		evidence := flags.String("evidence", "", "actual all-writer agreement evidence, not general recovery approval")
		paused := flags.Bool("writers-paused", false, "attest every Git writer will remain paused until expiry")
		expires := flags.String("expires", "", "hard end RFC3339, at most five minutes from now")
		run = func() (any, error) {
			end, err := time.Parse(time.RFC3339Nano, *expires)
			if err != nil {
				return nil, err
			}
			return authority.ReviewGitIndexLockPlan(home, *plan, authority.GitIndexLockWindow{ReviewedBy: *operator, EvidenceRef: *evidence, WritersPaused: *paused, ExpiresAt: end}, time.Now())
		}
	case "apply":
		plan := flags.String("plan", "", "exact plan ID")
		review := flags.String("review", "", "exact reviewed window ID")
		actor := flags.String("actor", "", "current lease holder")
		run = func() (any, error) {
			return authority.ApplyGitIndexLockRecovery(context.Background(), home, *plan, *review, *actor, nil, time.Now)
		}
	default:
		fmt.Fprintln(stderr, gitIndexLockUsage)
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, gitIndexLockUsage)
		return 2
	}
	value, err := run()
	if value != nil {
		if encodeErr := json.NewEncoder(out).Encode(value); encodeErr != nil {
			fmt.Fprintln(stderr, encodeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "authority git-index-lock:", err)
		return 1
	}
	return 0
}

func cmdAuthority(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound authority import [registry.yaml] | path <path-authority.yaml> | path-registry <os-path-authority.yaml> | git <repo-path> | dirty-plan <repo-path> [--work-id id] [--action-intent id ...] | worktree-retire <plan|review|apply> | git-index-lock <plan|review|apply> | import-os-absorption <matrix.json> | os-absorption [--json] | os-absorption-doctor [--json] | import-infra-database <database.yaml> | import-infra-services <services.yaml> | import-model-catalog <provider-registry.json> | model-catalog [--family F] [--model M] [--json] | model-catalog-doctor [--source P] [--json] | import-job-roster <cron-jobs.json> | import-job-token-governance <cron-token-usage.json> | job-roster-authority [--json] | job-health-plan | job-health-review <review.yaml> [--decision <enum>] [--rationale <text>] [--confidence <0..1>] [--next-action <text>] [--json] | harness-edit-review <card.json> --decision whitelist|reject --rationale <text> [--actor <id>] [--json] | resolve <token> | <entity>")
		os.Exit(2)
	}
	store, homePath := openMemStore()
	if args[0] == "git-index-lock" {
		if code := runAuthorityGitIndexLock(homePath, args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if args[0] == "worktree-retire" {
		code := runAuthorityWorktreeRetire(homePath, args[1:], os.Stdout, os.Stderr, time.Now())
		if code != 0 {
			os.Exit(code)
		}
		return
	}
	if args[0] == "import-os-absorption" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-os-absorption <matrix.json>")
			os.Exit(2)
		}
		report, err := authority.ImportOSAbsorptionMatrix(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-os-absorption:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "import-infra-database" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-infra-database <database.yaml>")
			os.Exit(2)
		}
		report, err := authority.ImportInfraDatabaseRegistry(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-infra-database:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "import-infra-services" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-infra-services <services.yaml>")
			os.Exit(2)
		}
		now := time.Now()
		report, err := authority.ImportInfraServicesRegistry(homePath, args[1], now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-infra-services:", err)
			os.Exit(1)
		}
		// Additive effects of the same import verb (settled design: entity
		// import is the attach point, not a second resolver source): register
		// each service as a resolvable mem.Entity, then enroll it as a
		// capability-graph node so orient's existing surfacing picks it up.
		entityReport, err := authority.RegisterInfraServiceEntities(store, homePath, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-infra-services: register entities:", err)
			os.Exit(1)
		}
		capabilityReport, err := authority.EnrollInfraServiceCapabilities(homePath, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-infra-services: enroll capabilities:", err)
			os.Exit(1)
		}
		printJSON(struct {
			authority.InfraRegistryImportReport
			RegisteredEntitySlugs []string `json:"registered_entity_slugs,omitempty"`
			SkippedEntitySlugs    []string `json:"skipped_entity_slugs,omitempty"`
			CapabilityNodeIDs     []string `json:"capability_node_ids,omitempty"`
		}{
			InfraRegistryImportReport: report,
			RegisteredEntitySlugs:     entityReport.RegisteredSlugs,
			SkippedEntitySlugs:        entityReport.SkippedSlugs,
			CapabilityNodeIDs:         capabilityReport.NodeIDs,
		})
		return
	}
	if args[0] == "import-model-catalog" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-model-catalog <provider-registry.json>")
			os.Exit(2)
		}
		now := time.Now()
		report, err := authority.ImportModelCatalog(homePath, args[1], now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-model-catalog:", err)
			os.Exit(1)
		}
		// Additive effects of the same import verb (same settled design as
		// import-infra-services): register each enrollable model row as a
		// resolvable mem.Entity, then enroll it as a capability-graph node.
		entityReport, err := authority.RegisterModelCatalogEntities(store, homePath, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-model-catalog: register entities:", err)
			os.Exit(1)
		}
		capabilityReport, err := authority.EnrollModelCatalogCapabilities(homePath, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-model-catalog: enroll capabilities:", err)
			os.Exit(1)
		}
		printJSON(struct {
			authority.ModelCatalogImportReport
			RegisteredEntitySlugs []string `json:"registered_entity_slugs,omitempty"`
			SkippedEntitySlugs    []string `json:"skipped_entity_slugs,omitempty"`
			CapabilityNodeIDs     []string `json:"capability_node_ids,omitempty"`
		}{
			ModelCatalogImportReport: report,
			RegisteredEntitySlugs:    entityReport.RegisteredSlugs,
			SkippedEntitySlugs:       entityReport.SkippedSlugs,
			CapabilityNodeIDs:        capabilityReport.NodeIDs,
		})
		return
	}
	if args[0] == "model-catalog" {
		// model-catalog is the read-only query: reads the INSTALLED registry
		// under $COMPOUND_HOME, never the source repo file. Output carries
		// the installed source revision on every row so an operator can tell
		// at a glance whether the answer is current.
		jsonOut := false
		provider := ""
		modelSlug := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--json":
				jsonOut = true
			case "--family":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "usage: compound authority model-catalog [--family F] [--model M] [--json]")
					os.Exit(2)
				}
				i++
				provider = args[i]
			case "--model":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "usage: compound authority model-catalog [--family F] [--model M] [--json]")
					os.Exit(2)
				}
				i++
				modelSlug = args[i]
			default:
				fmt.Fprintln(os.Stderr, "usage: compound authority model-catalog [--family F] [--model M] [--json]")
				os.Exit(2)
			}
		}
		result, err := authority.QueryModelCatalog(context.Background(), homePath, authority.ModelCatalogQueryFilter{Provider: provider, Model: modelSlug}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority model-catalog:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(result)
			return
		}
		fmt.Printf("# model-catalog authority\n")
		fmt.Printf("- authority: %s\n", result.AuthorityRef)
		if result.SourceRevisionSet {
			fmt.Printf("- source_revision: %s\n", result.SourceRevision)
		} else {
			fmt.Printf("- source_revision: (none — no import record yet)\n")
		}
		fmt.Printf("- rows: %d\n", len(result.Rows))
		if len(result.Rows) == 0 {
			fmt.Println("  (no rows — re-run `compound authority import-model-catalog <provider-registry.json>`)")
			return
		}
		for _, row := range result.Rows {
			fmt.Printf("- %s\n", row.Slug)
			fmt.Printf("    display_name: %s\n", row.DisplayName)
			fmt.Printf("    price_in_usd_per_mtok: %s\n", row.PriceInUSDPerMtok)
			fmt.Printf("    price_out_usd_per_mtok: %s\n", row.PriceOutUSDPerMtok)
			fmt.Printf("    context_window: %d\n", row.ContextWindow)
			if row.Tier != "" {
				fmt.Printf("    tier: %s\n", row.Tier)
			}
			if row.FreeLane {
				fmt.Printf("    free_lane: true\n")
			}
			if row.SupersededBy != "" {
				fmt.Printf("    superseded_by: %s\n", row.SupersededBy)
			}
		}
		return
	}
	if args[0] == "model-catalog-doctor" {
		// model-catalog-doctor is the refresh reflex: compare the installed
		// catalog sha256 against the source on-disk (if path provided and
		// present), report drift/ok/source-absent. Read-only — the import
		// verb stays the governed door.
		jsonOut := false
		sourcePath := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--json":
				jsonOut = true
			case "--source":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "usage: compound authority model-catalog-doctor [--source P] [--json]")
					os.Exit(2)
				}
				i++
				sourcePath = args[i]
			default:
				fmt.Fprintln(os.Stderr, "usage: compound authority model-catalog-doctor [--source P] [--json]")
				os.Exit(2)
			}
		}
		report, err := authority.ModelCatalogDoctor(homePath, sourcePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority model-catalog-doctor:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# model-catalog doctor\n")
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- authority_path: %s\n", report.AuthorityPath)
		fmt.Printf("- status: %s\n", report.Status)
		if report.InstalledDigest != "" {
			fmt.Printf("- installed_digest: %s\n", report.InstalledDigest)
		}
		if report.SourcePath != "" {
			fmt.Printf("- source_path: %s\n", report.SourcePath)
		}
		if report.SourceDigest != "" {
			fmt.Printf("- source_digest: %s\n", report.SourceDigest)
		}
		if report.LastImportID != "" {
			fmt.Printf("- last_import: %s\n", report.LastImportID)
		}
		for _, ev := range report.Checks {
			fmt.Printf("- check: %s %s", ev.Name, ev.Status)
			if ev.Detail != "" {
				fmt.Printf(" (%s)", ev.Detail)
			}
			fmt.Println()
		}
		// The refresh reflex is read-only; it makes drift visible without
		// auto-importing. Exit 0 on ok/source-absent (intentional), non-zero
		// on drift so scripts can chain.
		if report.Status == "drift" {
			os.Exit(1)
		}
		return
	}
	if args[0] == "os-absorption" {
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound authority os-absorption [--json]")
			os.Exit(2)
		}
		matrix, err := authority.LoadOSAbsorptionMatrix(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority os-absorption:", err)
			os.Exit(1)
		}
		if len(args) == 2 && args[1] == "--json" {
			printJSON(matrix)
			return
		}
		absorbed := 0
		pending := 0
		for _, row := range matrix.Rows {
			switch row.Status {
			case "absorbed", "wrapper", "evidence_only", "retired":
				absorbed++
			case "pending":
				pending++
			}
		}
		fmt.Printf("os absorption authority: %s\n", matrix.AuthorityRef)
		fmt.Printf("rows: %d absorbed=%d pending=%d\n", len(matrix.Rows), absorbed, pending)
		fmt.Printf("source: %s %s\n", matrix.SourceMatrixPath, matrix.SourceMatrixRevision)
		return
	}
	if args[0] == "os-absorption-doctor" {
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound authority os-absorption-doctor [--json]")
			os.Exit(2)
		}
		report, err := authority.OSAbsorptionDoctor(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority os-absorption-doctor:", err)
			os.Exit(1)
		}
		if len(args) == 2 && args[1] == "--json" {
			printJSON(report)
			if report.Status != "ok" {
				os.Exit(1)
			}
			return
		}
		fmt.Printf("os absorption doctor: %s\n", report.Status)
		fmt.Printf("rows: %d auto_proved=%d pending_with_gate=%d missing=%d failed_auto=%d\n", report.RowCount, report.AutoProofCount, report.PendingWithGateCount, report.MissingRequiredCount, report.FailedAutoProofCount)
		fmt.Printf("proof split: machine_proven=%d asserted=%d pending=%d failed_proof=%d skipped=%d (absorbed_count=%d is status-string only, not proof)\n", report.MachineProvenCount, report.AssertedCount, report.PendingCount, report.FailedProofCount, report.SkippedProofCount, report.AbsorbedCount)
		fmt.Println(authority.OSAbsorptionDoctorMeterLine(report.Rows))
		for _, check := range report.Checks {
			fmt.Printf("- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Printf(" (%s)", check.Detail)
			}
			fmt.Println()
		}
		if report.Status != "ok" {
			os.Exit(1)
		}
		return
	}
	if args[0] == "import-job-roster" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-job-roster <cron-jobs.json>")
			os.Exit(2)
		}
		report, err := authority.ImportJobRoster(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-job-roster:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "import-job-token-governance" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority import-job-token-governance <cron-token-usage.json>")
			os.Exit(2)
		}
		report, err := authority.ImportJobTokenGovernance(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import-job-token-governance:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "job-roster-authority" {
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound authority job-roster-authority [--json]")
			os.Exit(2)
		}
		type jobRosterAuthorityReport struct {
			AuthorityRef             string                                     `json:"authority_ref"`
			AuthorityPath            string                                     `json:"authority_path"`
			Installed                bool                                       `json:"installed"`
			GeneratedAt              string                                     `json:"generated_at,omitempty"`
			SourcePath               string                                     `json:"source_path,omitempty"`
			SourceRevision           string                                     `json:"source_revision,omitempty"`
			JobCount                 int                                        `json:"job_count,omitempty"`
			EnabledCount             int                                        `json:"enabled_count,omitempty"`
			RecentRunCount           int                                        `json:"recent_run_count,omitempty"`
			ErrorCount               int                                        `json:"error_count,omitempty"`
			RecentErrors             []authority.JobSummary                     `json:"recent_errors,omitempty"`
			TokenUsageSourcePath     string                                     `json:"token_usage_source_path,omitempty"`
			TokenUsageSourceRevision string                                     `json:"token_usage_source_revision,omitempty"`
			TokenMetric              string                                     `json:"token_metric,omitempty"`
			TokenSessionsCount       int                                        `json:"token_sessions_count"`
			EnabledAgentCount        int                                        `json:"enabled_agent_count"`
			HighTokenEnabledCount    int                                        `json:"high_token_enabled_count"`
			TokenRiskJobs            []authority.JobSummary                     `json:"token_risk_jobs,omitempty"`
			SourceAuthority          []authority.JobRosterSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath         string                                     `json:"source_record_path"`
		}
		report := jobRosterAuthorityReport{
			AuthorityRef:     authority.JobRosterAuthorityRef,
			AuthorityPath:    authority.JobRosterAuthorityPath(homePath),
			SourceRecordPath: authority.JobRosterSourceAuthorityPath(homePath),
		}
		roster, err := authority.LoadJobRosterAuthority(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "authority job-roster-authority:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.GeneratedAt = roster.GeneratedAt
			report.SourcePath = roster.SourcePath
			report.SourceRevision = roster.SourceRevision
			report.JobCount = roster.JobCount
			report.EnabledCount = roster.EnabledCount
			report.RecentRunCount = roster.RecentRunCount
			report.ErrorCount = roster.ErrorCount
			report.RecentErrors = roster.RecentErrors
			report.TokenUsageSourcePath = roster.TokenUsageSourcePath
			report.TokenUsageSourceRevision = roster.TokenUsageSourceRevision
			report.TokenMetric = roster.TokenMetric
			report.TokenSessionsCount = roster.TokenSessionsCount
			report.EnabledAgentCount = roster.EnabledAgentCount
			report.HighTokenEnabledCount = roster.HighTokenEnabledCount
			report.TokenRiskJobs = roster.TokenRiskJobs
		}
		records, err := authority.LoadJobRosterSourceAuthority(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority job-roster-authority:", err)
			os.Exit(1)
		}
		report.SourceAuthority = records
		if len(args) == 2 && args[1] == "--json" {
			printJSON(report)
			return
		}
		fmt.Printf("job roster authority: %s\n", report.AuthorityRef)
		fmt.Printf("installed: %t\n", report.Installed)
		fmt.Printf("jobs: %d enabled=%d recent=%d errors=%d\n", report.JobCount, report.EnabledCount, report.RecentRunCount, report.ErrorCount)
		return
	}
	if args[0] == "job-health-plan" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound authority job-health-plan")
			os.Exit(2)
		}
		roster, err := authority.LoadJobRosterAuthority(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority job-health-plan:", err)
			os.Exit(1)
		}
		report, err := authority.RecordJobHealthPlans(homePath, roster, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority job-health-plan:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "job-health-review" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority job-health-review <review.yaml> [--decision repair_required|parked|disabled|no_action|owner_action_done] [--rationale <text>] [--confidence <0..1>] [--next-action <text>] [--json]")
			os.Exit(2)
		}
		reviewPath := args[1]
		fillFlags := args[2:]
		// Fill semantics (settled design #2): only a draft still carrying the
		// TODO placeholder gets filled from flags and written back to the SAME
		// path — the YAML remains the durable record. No --decision -> no fill,
		// so behavior stays byte-identical to before these flags existed.
		if hasFlag(fillFlags, "--decision") {
			fill := authority.JobHealthReviewFillInput{
				Decision:   valueFlag(fillFlags, "--decision"),
				Rationale:  valueFlag(fillFlags, "--rationale"),
				NextAction: valueFlag(fillFlags, "--next-action"),
			}
			if hasFlag(fillFlags, "--confidence") {
				conf, err := strconv.ParseFloat(valueFlag(fillFlags, "--confidence"), 64)
				if err != nil {
					fmt.Fprintln(os.Stderr, "authority job-health-review: --confidence must be a number")
					os.Exit(2)
				}
				fill.Confidence = &conf
			}
			if err := authority.FillJobHealthReviewDraft(reviewPath, fill, time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority job-health-review:", err)
				os.Exit(1)
			}
		}
		review, err := authority.LoadJobHealthReview(reviewPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority job-health-review:", err)
			os.Exit(1)
		}
		report, err := authority.RecordJobHealthReview(homePath, review, reviewPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority job-health-review:", err)
			os.Exit(1)
		}
		printJSON(report)
		return
	}
	if args[0] == "harness-edit-review" {
		runAuthorityHarnessEditReview(args, homePath)
		return
	}
	if args[0] == "resolve" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority resolve <token>")
			os.Exit(2)
		}
		res, err := store.ResolveEntity(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority resolve:", err)
			os.Exit(1)
		}
		fmt.Printf("status: %s\n", res.Status)
		if res.Slug != "" {
			fmt.Printf("entity: %s\n", res.Slug)
		}
		if res.Via != "" {
			fmt.Printf("via: %s\n", res.Via)
		}
		// Additive only: a resolved service:* entity also prints its
		// import provenance (role/authority/status/source_revision) read
		// from the installed services registry + source-authority ledger.
		// Every other entity kind's resolve output is unchanged.
		if res.Status == "resolved" {
			if prov, found, err := authority.ResolveInfraServiceProvenance(homePath, res.Slug); err != nil {
				fmt.Fprintln(os.Stderr, "authority resolve: infra service provenance:", err)
				os.Exit(1)
			} else if found {
				if prov.Role != "" {
					fmt.Printf("role: %s\n", prov.Role)
				}
				if prov.Authority != "" {
					fmt.Printf("authority: %s\n", prov.Authority)
				}
				if prov.Status != "" {
					fmt.Printf("status: %s\n", prov.Status)
				}
				if prov.SourceRevision != "" {
					fmt.Printf("source_revision: %s\n", prov.SourceRevision)
				}
			}
			// Additive only: a resolved model:* entity also prints its
			// import provenance (provider/prices/context/tier/
			// source_revision) read from the installed provider catalog +
			// source-authority ledger. Every other entity kind's resolve
			// output (including service:* above) stays unchanged.
			if prov, found, err := authority.ResolveModelCatalogProvenance(homePath, res.Slug); err != nil {
				fmt.Fprintln(os.Stderr, "authority resolve: model catalog provenance:", err)
				os.Exit(1)
			} else if found {
				if prov.Provider != "" {
					fmt.Printf("provider: %s\n", prov.Provider)
				}
				fmt.Printf("price_in_usd_per_mtok: %s\n", prov.PriceInUSDPerMtok)
				fmt.Printf("price_out_usd_per_mtok: %s\n", prov.PriceOutUSDPerMtok)
				fmt.Printf("context_window: %d\n", prov.ContextWindow)
				if prov.Tier != "" {
					fmt.Printf("tier: %s\n", prov.Tier)
				}
				if prov.SourceRevision != "" {
					fmt.Printf("source_revision: %s\n", prov.SourceRevision)
				}
			}
		}
		return
	}
	if args[0] == "path" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority path <path-authority.yaml>")
			os.Exit(2)
		}
		existing, err := store.Entities()
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority path:", err)
			os.Exit(1)
		}
		_, entity, facts, report, err := authority.ImportPathAuthority(args[1], existing)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority path:", err)
			os.Exit(1)
		}
		if _, err := store.RegisterEntity(entity, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "authority entity:", err)
			os.Exit(1)
		}
		captured, err := store.Capture(facts, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority capture:", err)
			os.Exit(1)
		}
		promoted := 0
		for _, c := range captured {
			if _, err := store.Adjudicate(c.ID, "promote", "path-authority-import", time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority promote:", err)
				os.Exit(1)
			}
			promoted++
		}
		fmt.Printf("# path authority %s\n\n", report.EntityRef)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- identifiers: %d\n", len(report.Identifiers))
		fmt.Printf("- authority_claims: %d\n", promoted)
		return
	}
	if args[0] == "path-registry" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority path-registry <os-path-authority.yaml>")
			os.Exit(2)
		}
		existing, err := store.Entities()
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority path-registry:", err)
			os.Exit(1)
		}
		_, entities, facts, report, err := authority.ImportOSPathRegistry(args[1], existing)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority path-registry:", err)
			os.Exit(1)
		}
		for _, entity := range entities {
			if _, err := store.RegisterEntity(entity, time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority entity:", err)
				os.Exit(1)
			}
		}
		captured, err := store.Capture(facts, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority capture:", err)
			os.Exit(1)
		}
		promoted := 0
		for _, c := range captured {
			if _, err := store.Adjudicate(c.ID, "promote", "path-authority-registry-import", time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority promote:", err)
				os.Exit(1)
			}
			promoted++
		}
		fmt.Printf("# path authority registry\n\n")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- entities: %d\n", report.EntityCount)
		fmt.Printf("- authority_claims: %d\n", promoted)
		return
	}
	if args[0] == "git" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority git <repo-path> [--json]")
			os.Exit(2)
		}
		jsonOut := hasFlag(args[2:], "--json")
		for _, arg := range args[2:] {
			if arg != "--json" {
				fmt.Fprintln(os.Stderr, "usage: compound authority git <repo-path> [--json]")
				os.Exit(2)
			}
		}
		existing, err := store.Entities()
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority git:", err)
			os.Exit(1)
		}
		_, entity, facts, report, err := authority.ImportGitSnapshot(args[1], existing)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority git:", err)
			os.Exit(1)
		}
		if _, err := store.RegisterEntity(entity, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "authority entity:", err)
			os.Exit(1)
		}
		captured, err := store.Capture(facts, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority capture:", err)
			os.Exit(1)
		}
		promoted := 0
		for _, c := range captured {
			res, err := store.Adjudicate(c.ID, "promote", "source-control-import", time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "authority promote:", err)
				os.Exit(1)
			}
			if res.Claim != nil {
				promoted++
			}
		}
		if jsonOut {
			out := struct {
				SchemaVersion   string `json:"schema_version"`
				Status          string `json:"status"`
				AuthorityRef    string `json:"authority_ref"`
				EntityRef       string `json:"entity_ref"`
				GitRoot         string `json:"git_root"`
				Branch          string `json:"branch,omitempty"`
				Head            string `json:"head,omitempty"`
				Remote          string `json:"remote,omitempty"`
				Upstream        string `json:"upstream,omitempty"`
				Ahead           int    `json:"ahead"`
				Behind          int    `json:"behind"`
				Staged          int    `json:"staged"`
				Unstaged        int    `json:"unstaged"`
				Untracked       int    `json:"untracked"`
				ChangedPaths    int    `json:"changed_paths"`
				CandidateCount  int    `json:"candidate_count"`
				AuthorityClaims int    `json:"authority_claims"`
				EvidenceRef     string `json:"evidence_ref"`
			}{
				SchemaVersion:   authority.SourceControlSchemaVersion,
				Status:          report.Status,
				AuthorityRef:    "compound://ledger/source-control",
				EntityRef:       report.EntityRef,
				GitRoot:         report.GitRoot,
				Branch:          report.Branch,
				Head:            report.Head,
				Remote:          report.Remote,
				Upstream:        report.Upstream,
				Ahead:           report.Ahead,
				Behind:          report.Behind,
				Staged:          report.Staged,
				Unstaged:        report.Unstaged,
				Untracked:       report.Untracked,
				ChangedPaths:    report.ChangedPaths,
				CandidateCount:  report.CandidateCount,
				AuthorityClaims: promoted,
				EvidenceRef:     report.EvidenceRef,
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", " ")
			if err := enc.Encode(out); err != nil {
				fmt.Fprintln(os.Stderr, "authority git:", err)
				os.Exit(1)
			}
			return
		}
		fmt.Printf("# source control\n\n")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- entity: %s\n", report.EntityRef)
		fmt.Printf("- root: %s\n", report.GitRoot)
		fmt.Printf("- branch: %s\n", report.Branch)
		fmt.Printf("- head: %s\n", report.Head)
		fmt.Printf("- ahead: %d\n", report.Ahead)
		fmt.Printf("- behind: %d\n", report.Behind)
		fmt.Printf("- staged: %d\n", report.Staged)
		fmt.Printf("- unstaged: %d\n", report.Unstaged)
		fmt.Printf("- untracked: %d\n", report.Untracked)
		fmt.Printf("- changed_paths: %d\n", report.ChangedPaths)
		fmt.Printf("- authority_claims: %d\n", promoted)
		return
	}
	if args[0] == "dirty-plan" {
		usage := "usage: compound authority dirty-plan <repo-path> [--work-id id] [--action-intent id ...] [--json]"
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		jsonOut := hasFlag(args[2:], "--json")
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--json":
			case "--work-id", "--action-intent":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		workID := valueFlag(args[2:], "--work-id")
		actionIntentIDs := valuesFlag(args[2:], "--action-intent")
		var mutationBinding *authority.SourceControlMutationBinding
		if len(actionIntentIDs) > 0 {
			binding, bindingErr := loadSourceControlMutationBinding(homePath, actionIntentIDs)
			if bindingErr != nil {
				fmt.Fprintln(os.Stderr, "authority dirty-plan:", bindingErr)
				os.Exit(1)
			}
			if ownerErr := authority.ValidateSourceControlMutationOwner(homePath, workID); ownerErr != nil {
				fmt.Fprintln(os.Stderr, "authority dirty-plan:", ownerErr)
				os.Exit(1)
			}
			mutationBinding = &binding
		}
		snap, _, _, _, err := authority.ImportGitSnapshot(args[1], nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority dirty-plan:", err)
			os.Exit(1)
		}
		var plan authority.SourceControlReviewPlan
		if mutationBinding == nil {
			plan, err = authority.BuildSourceControlReviewPlan(snap, workID, time.Now())
		} else {
			plan, err = authority.BuildSourceControlMutationReviewPlan(snap, workID, *mutationBinding, time.Now())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority dirty-plan:", err)
			os.Exit(1)
		}
		paths, err := authority.RecordSourceControlReviewPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority dirty-plan:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(struct {
				SchemaVersion string                                 `json:"schema_version"`
				Status        string                                 `json:"status"`
				AuthorityRef  string                                 `json:"authority_ref"`
				ID            string                                 `json:"id"`
				RepoID        string                                 `json:"repo_id"`
				SourceStatus  string                                 `json:"source_status"`
				ChangedPaths  int                                    `json:"changed_paths"`
				Staged        int                                    `json:"staged"`
				Unstaged      int                                    `json:"unstaged"`
				Untracked     int                                    `json:"untracked"`
				Plan          authority.SourceControlReviewPlan      `json:"plan"`
				Paths         authority.SourceControlReviewPlanPaths `json:"paths"`
			}{
				SchemaVersion: authority.SourceControlReviewPlanSchemaVersion,
				Status:        plan.Status,
				AuthorityRef:  "compound://reviews/source-control-cleanup",
				ID:            plan.ID,
				RepoID:        plan.RepoID,
				SourceStatus:  plan.SourceStatus,
				ChangedPaths:  plan.ChangedPaths,
				Staged:        plan.Staged,
				Unstaged:      plan.Unstaged,
				Untracked:     plan.Untracked,
				Plan:          plan,
				Paths:         paths,
			})
			return
		}
		fmt.Printf("# source-control dirty-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- repo: %s\n", plan.RepoID)
		if plan.WorkID != "" {
			fmt.Printf("- work: %s\n", plan.WorkID)
		}
		fmt.Printf("- source_status: %s\n", plan.SourceStatus)
		fmt.Printf("- changed_paths: %d\n", plan.ChangedPaths)
		fmt.Printf("- staged: %d\n", plan.Staged)
		fmt.Printf("- unstaged: %d\n", plan.Unstaged)
		fmt.Printf("- untracked: %d\n", plan.Untracked)
		fmt.Printf("- required_evidence: %d\n", len(plan.ReviewDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		if plan.MutationBinding != nil {
			fmt.Printf("- action_work: %s\n", plan.MutationBinding.ActionWorkID)
			fmt.Printf("- action_intents: %d\n", len(plan.MutationBinding.ActionIntentIDs))
			fmt.Printf("- mutation_bound: true\n")
		}
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
		return
	}
	if args[0] == "dirty-review" {
		jsonOut := hasFlag(args[1:], "--json")
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound authority dirty-review <review.yaml> [--expected-source-digest sha256:...] [--decision D --rationale R --reviewed-by operator:<actor>] [--json]")
			os.Exit(2)
		}
		var decision, rationale, reviewedBy, expectedSourceDigest string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--json":
			case "--decision", "--rationale", "--reviewed-by", "--expected-source-digest":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "authority dirty-review: flag value required")
					os.Exit(2)
				}
				if args[i] == "--decision" {
					decision = args[i+1]
				} else if args[i] == "--rationale" {
					rationale = args[i+1]
				} else if args[i] == "--reviewed-by" {
					reviewedBy = args[i+1]
				} else {
					expectedSourceDigest = args[i+1]
				}
				i++
			default:
				if i != 1 {
					fmt.Fprintln(os.Stderr, "usage: compound authority dirty-review <review.yaml> [--expected-source-digest sha256:...] [--json]")
					os.Exit(2)
				}
			}
		}
		if expectedSourceDigest != "" && (decision != "" || rationale != "" || reviewedBy != "") {
			fmt.Fprintln(os.Stderr, "authority dirty-review: --expected-source-digest cannot be combined with inline review overrides")
			os.Exit(2)
		}
		review, err := authority.LoadSourceControlReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority dirty-review:", err)
			os.Exit(1)
		}
		if decision != "" || rationale != "" || reviewedBy != "" {
			if decision != "" {
				review.Decision = decision
			}
			if rationale != "" {
				review.Rationale = rationale
			}
			if reviewedBy != "" {
				review.ReviewedBy = reviewedBy
			}
			review.Confidence = 1
			review.ReviewedAt = time.Now().UTC().Format(time.RFC3339)
			data, marshalErr := yaml.Marshal(review)
			if marshalErr != nil {
				fmt.Fprintln(os.Stderr, "authority dirty-review:", marshalErr)
				os.Exit(1)
			}
			if writeErr := os.WriteFile(args[1], data, 0o600); writeErr != nil {
				fmt.Fprintln(os.Stderr, "authority dirty-review:", writeErr)
				os.Exit(1)
			}
		}
		var report authority.SourceControlReviewReport
		if expectedSourceDigest != "" {
			report, err = authority.RecordSourceControlReview(homePath, review, args[1], time.Now(), expectedSourceDigest)
		} else {
			report, err = authority.RecordSourceControlReview(homePath, review, args[1], time.Now())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority dirty-review:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# source-control dirty-review %s\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- plan: %s\n", report.PlanID)
		fmt.Printf("- review: %s\n", report.ReviewID)
		fmt.Printf("- repo: %s\n", report.RepoID)
		fmt.Printf("- decision: %s\n", report.Decision)
		if report.ReviewedBy != "" {
			fmt.Printf("- reviewed_by: %s\n", report.ReviewedBy)
		}
		if report.AuthorizationDigest != "" {
			fmt.Printf("- authorization_digest: %s\n", report.AuthorizationDigest)
		}
		if report.NextAction != "" {
			fmt.Printf("- next_action: %s\n", report.NextAction)
		}
		fmt.Printf("- evidence_refs: %d\n", len(report.EvidenceRefs))
		fmt.Printf("- reviewed: %s\n", report.ReviewPath)
		return
	}
	if args[0] == "import" {
		path := authority.DefaultRegistry()
		if len(args) > 1 {
			path = args[1]
		}
		if strings.TrimSpace(path) == "" {
			fmt.Fprintln(os.Stderr, "authority import needs registry path or COMPOUND_AUTHORITY_REGISTRY")
			os.Exit(2)
		}
		entities, facts, err := authority.Import(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority import:", err)
			os.Exit(1)
		}
		for _, e := range entities {
			if _, err := store.RegisterEntity(e, time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority entity:", err)
				os.Exit(1)
			}
		}
		captured, err := store.Capture(facts, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "authority capture:", err)
			os.Exit(1)
		}
		promoted := 0
		for _, c := range captured {
			if _, err := store.Adjudicate(c.ID, "promote", "authority-import", time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "authority promote:", err)
				os.Exit(1)
			}
			promoted++
		}
		fmt.Printf("imported %d entities, %d authority claims through spine memory\n", len(entities), promoted)
		return
	}
	entity := args[0]
	rr, err := store.Recall(entity, mem.RecallOpts{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "authority recall:", err)
		os.Exit(1)
	}
	fmt.Printf("# Authority - %s\n\n", entity)
	found := false
	for _, c := range rr.Claims {
		if c.Entity == entity && c.ClaimType == "status" {
			fmt.Printf("- **%s**: %s\n", c.Predicate, c.Object)
			found = true
		}
	}
	if !found {
		fmt.Printf("(no authority claims for %s; run compound authority import first)\n", entity)
	}
}

// loadSourceControlMutationBinding derives executable identity only from
// persisted ActionIntents. The dirty-plan CLI accepts ids, never caller-authored
// action/class/capability fields, so a review cannot bless fabricated semantics.
