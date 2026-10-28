// capability.go implements the "compound capability" command family: surfacing, adding,
// importing (skills/topology/evaluations/surfacing misses), and reviewing capability graph
// nodes and outbound-distribution / external-intake workflows built on capgraph.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/decisions"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/sourceobs"
	"compound/internal/tooltopology"
)

// cmdCapability is the P8 capability graph: surface relevant capabilities for a task (never
// execute them), and learn which capability fits which context from the distiller's observations.
func cmdCapability(args []string) {
	usage := "usage: compound capability <add|retire|resolve|surface|observe|consume|consume-receipts|usage|list|graph|authority|lifecycle-review|external-intake|outbound-distribution-plan|outbound-distribution-verdict-plan|outbound-distribution-verdict-review|outbound-distribution-dry-run-receipt|outbound-distribution-dry-run-review|external-intake-review-plan|external-intake-evaluation-plan|external-intake-import-reviewed|external-intake-review|tool-boundary-doctor|skill-source-doctor|import-topology|import-connections|import-gateway|import-skills|import-agent-lanes|import-evaluations|import-surfacing-misses|evaluation-plan|evaluation-review|skill-plan|skill-candidate-review|skill-candidate-decision|compact> ..."
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if hasHelpFlag(args) {
		fmt.Println(usage)
		return
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	var store *capgraph.Store
	if sub == "import-skills" && hasFlag(rest, "--dry-run") {
		// Dry-runs inspect source and prior authority without creating graph state.
	} else if sub == "resolve" {
		store, err = capgraph.OpenReadOnly(homePath)
	} else {
		store, err = capgraph.Open(homePath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	flag := func(name string) string {
		for i := 0; i < len(rest); i++ {
			if rest[i] == name && i+1 < len(rest) {
				return rest[i+1]
			}
		}
		return ""
	}
	posn := func(valueFlags map[string]bool) []string {
		var out []string
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if strings.HasPrefix(a, "-") {
				if valueFlags[a] {
					i++
				}
				continue
			}
			out = append(out, a)
		}
		return out
	}
	die := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
	}

	switch sub {
	case "lifecycle-review":
		cmdCapabilityLifecycleReview(homePath, rest)
	case "tool-boundary-doctor":
		report, err := tooltopology.DoctorBoundary(homePath)
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability tool-boundary-doctor %s\n\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- nodes: %d\n", report.NodeCount)
		fmt.Printf("- mcp: %d\n", report.MCPCount)
		fmt.Printf("- tools: %d\n", report.ToolCount)
		fmt.Printf("- plugin_adapters: %d retired=%d surfaced=%d\n", report.PluginAdapterCount, report.RetiredPluginAdapters, report.PluginSurfaceCount)
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
	case "skill-source-doctor":
		jsonOut := hasFlag(rest, "--json")
		p := posn(map[string]bool{})
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability skill-source-doctor <skill-root-or-SKILL.md> [--json]")
			os.Exit(2)
		}
		report, err := capgraph.DoctorSkillSource(p[0])
		die(err)
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", " ")
			die(enc.Encode(report))
			if report.Status != "ok" {
				os.Exit(1)
			}
			return
		}
		fmt.Printf("# capability skill-source-doctor %s\n\n", report.Root)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- skill_files: %d\n", report.SkillFileCount)
		fmt.Printf("- errors: %d\n", report.ErrorCount)
		for _, check := range report.Checks {
			fmt.Printf("- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Printf(" (%s)", check.Detail)
			}
			fmt.Println()
		}
		for _, file := range report.Files {
			if file.Status == "ok" {
				continue
			}
			fmt.Printf("- file: %s\n", file.Path)
			for _, msg := range file.Errors {
				fmt.Printf("  - %s\n", msg)
			}
		}
		if report.Status != "ok" {
			os.Exit(1)
		}
	case "import-topology":
		now := time.Now()
		osResolverPath := flag("--os-resolver-json")
		mcporterPath := flag("--mcporter-json")
		p := posn(map[string]bool{"--os-resolver-json": true, "--mcporter-json": true})
		var topo tooltopology.Topology
		var err error
		switch {
		case osResolverPath != "" && len(p) == 0:
			topo, err = tooltopology.LoadOSResolver(osResolverPath, now)
		case mcporterPath != "" && len(p) == 0:
			topo, err = tooltopology.LoadMCPorterCatalog(mcporterPath, now)
		case osResolverPath == "" && mcporterPath == "" && len(p) == 1:
			topo, err = tooltopology.Load(p[0])
		default:
			fmt.Fprintln(os.Stderr, "usage: compound capability import-topology <tool-topology.yaml> | --os-resolver-json <tool-resolver.json> | --mcporter-json <mcporter-list.json>")
			os.Exit(2)
		}
		die(err)
		report, err := tooltopology.Apply(homePath, topo, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			for _, check := range report.Checks {
				fmt.Fprintf(os.Stderr, "- %s: %s", check.Name, check.Status)
				if check.Detail != "" {
					fmt.Fprintf(os.Stderr, " (%s)", check.Detail)
				}
				fmt.Fprintln(os.Stderr)
			}
			os.Exit(1)
		}
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromToolTopology(topo, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
			SourcePath:          topo.SourceRef.Path,
			SourceKind:          topo.SourceRef.Kind,
			SourceOwner:         topo.SourceRef.Owner,
			SourceClass:         topo.SourceRef.SourceClass,
			SourceRevision:      topo.SourceRef.Revision,
			SourceObservedAt:    topo.ObservedAt,
			ImportedVia:         "compound capability import-topology",
			NodeIDs:             report.NodeIDs,
			EdgeIDs:             report.EdgeIDs,
			SourceObservationID: sourceReport.ObservationID,
			NoPromoteIDs:        sourceReport.NoPromoteIDs,
			EvidenceRefs:        topo.EvidenceRefs,
		}, now)
		die(err)
		fmt.Printf("# tool topology %s\n\n", report.TopologyID)
		fmt.Printf("- status: %s\n", report.Status)
		for _, check := range report.Checks {
			fmt.Printf("- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Printf(" (%s)", check.Detail)
			}
			fmt.Println()
		}
		for _, id := range report.NodeIDs {
			fmt.Printf("- node: %s\n", id)
		}
		for _, id := range report.EdgeIDs {
			fmt.Printf("- edge: %s\n", id)
		}
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		for _, id := range sourceReport.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
		}
		for _, id := range sourceReport.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		fmt.Printf("- source_authority: %s\n", auth.ID)
	case "import-connections":
		now := time.Now()
		jsonOut := hasFlag(rest, "--json")
		harnessFilter := flag("--harness")
		p := posn(map[string]bool{"--harness": true})
		if len(p) != 0 {
			fmt.Fprintln(os.Stderr, "usage: compound capability import-connections [--harness <h>] [--json]")
			os.Exit(2)
		}
		report, err := importCapabilityConnections(store, homePath, harnessFilter, now)
		die(err)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# capability import-connections %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- harnesses: %d\n", len(report.Harnesses))
		for _, h := range report.Harnesses {
			fmt.Printf("- harness: %s node=%s minted=%t config_paths=%d revision=%s asserting_owner=%s connection_surface_added=%t\n",
				h.Harness, h.NodeID, h.NodeMinted, len(h.ConfigPaths), h.SourceRevision, h.AssertingOwner, h.ConnectionSurfaceAdded)
			fmt.Printf("- source_observation: %s\n", h.SourceObservationID)
			for _, id := range h.NoPromoteIDs {
				fmt.Printf("- source_no_promote: %s\n", id)
			}
			fmt.Printf("- source_authority: %s\n", h.SourceAuthorityID)
		}
	case "import-gateway":
		now := time.Now()
		jsonOut := hasFlag(rest, "--json")
		gatewayName := flag("--gateway-name")
		p := posn(map[string]bool{"--gateway-name": true})
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability import-gateway <provider-registry.json> [--gateway-name <name>] [--json]")
			os.Exit(2)
		}
		report, err := importCapabilityGateway(store, homePath, p[0], gatewayName, now)
		die(err)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# capability import-gateway %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- gateway: %s\n", report.GatewayName)
		fmt.Printf("- registry: %s (schema %s)\n", report.RegistryPath, report.RegistrySchema)
		fmt.Printf("- source_revision: %s\n", report.SourceRevision)
		for _, n := range report.Nodes {
			if n.Role == "provider" {
				fmt.Printf("- node: %s role=provider provider=%s minted=%t endpoint=%s auth_mode=%s connection_surface_added=%t\n",
					n.NodeID, n.ProviderID, n.NodeMinted, n.Endpoint, n.AuthMode, n.ConnectionSurfaceAdded)
			} else {
				fmt.Printf("- node: %s role=root minted=%t connection_surface_added=%t\n",
					n.NodeID, n.NodeMinted, n.ConnectionSurfaceAdded)
			}
		}
		fmt.Printf("- skipped_providers: %d\n", report.SkippedProviders)
		for _, id := range report.SkippedProviderIDs {
			fmt.Printf("- skipped_provider: %s\n", id)
		}
		fmt.Printf("- source_observation: %s\n", report.SourceObservationID)
		for _, id := range report.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		fmt.Printf("- source_authority: %s\n", report.SourceAuthorityID)
	case "import-skills":
		osSkillPath := flag("--os-skill-json")
		osPluginBoardPath := flag("--os-plugin-board-json")
		brainRecipeDir := flag("--brain-recipe-dir")
		dryRun := hasFlag(rest, "--dry-run")
		p := posn(map[string]bool{"--os-skill-json": true, "--os-plugin-board-json": true, "--brain-recipe-dir": true})
		now := time.Now()
		manifest, err := capgraph.LoadManifestByFlag(osSkillPath, osPluginBoardPath, brainRecipeDir, p, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if dryRun {
			if brainRecipeDir == "" {
				fmt.Fprintln(os.Stderr, "usage: compound capability import-skills --brain-recipe-dir <dir> --dry-run")
				os.Exit(2)
			}
			os.Exit(capgraph.RunImportSkillsDry(homePath, manifest, os.Stdout, os.Stderr))
		}
		report, err := capgraph.ApplySkillManifest(homePath, manifest, now)
		die(err)
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromSkillManifest(manifest, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
			SourcePath:          manifest.SourceRef.Path,
			SourceKind:          manifest.SourceRef.Kind,
			SourceOwner:         manifest.SourceRef.Owner,
			SourceClass:         manifest.SourceRef.SourceClass,
			SourceRevision:      manifest.SourceRef.Revision,
			SourceObservedAt:    manifest.ObservedAt,
			ImportedVia:         "compound capability import-skills",
			NodeIDs:             report.NodeIDs,
			EdgeIDs:             report.EdgeIDs,
			SourceObservationID: sourceReport.ObservationID,
			NoPromoteIDs:        sourceReport.NoPromoteIDs,
			EvidenceRefs:        manifest.EvidenceRefs,
		}, now)
		die(err)
		capgraph.RunImportSkillsFinalize(manifest, report, sourceReport.ObservationID, sourceReport.CandidateIDs, sourceReport.NoPromoteIDs, auth.ID, os.Stdout)
	case "import-agent-lanes":
		now := time.Now()
		registryPath, overlayPath, launchProfilesPath := flag("--registry"), flag("--overlay"), flag("--launch-profiles")
		p := posn(map[string]bool{"--registry": true, "--overlay": true, "--launch-profiles": true})
		if registryPath == "" || overlayPath == "" || len(p) != 0 {
			fmt.Fprintln(os.Stderr, "usage: compound capability import-agent-lanes --registry <registry.ts> --overlay <overlay.json> [--launch-profiles <profiles.json>]")
			os.Exit(2)
		}
		report, err := capgraph.ImportAgentLaneProfilesWithLaunchProfiles(homePath, registryPath, overlayPath, launchProfilesPath, now)
		die(err)
		printJSON(report)
	case "reachability":
		// Existence is already well served; access is not. This surfaces the
		// difference so "my harnesses cannot find things smoothly" becomes a
		// number with a named next door instead of a recurring impression.
		report, err := store.ComputeReachability()
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		pct := func(have, total int) string {
			if total == 0 {
				return "   n/a"
			}
			return fmt.Sprintf("%5.1f%%", float64(have)/float64(total)*100)
		}
		// A dash, not a 0: zero degraded and "this kind reports no health at all"
		// are different states, and printing 0 for both would claim health data
		// that was never collected.
		degraded := func(n int) string {
			if n == 0 {
				return "-"
			}
			return fmt.Sprintf("%d", n)
		}
		fmt.Printf("%-18s %7s %10s %7s %11s %7s %9s\n", "kind", "named", "reachable", "", "unlockable", "", "degraded")
		fmt.Println(strings.Repeat("-", 74))
		for _, k := range report.Kinds {
			fmt.Printf("%-18s %7d %10d %7s %11d %7s %9s\n",
				k.Kind, k.Nodes, k.Reachable, pct(k.Reachable, k.Nodes), k.Unlockable, pct(k.Unlockable, k.Nodes), degraded(k.Degraded))
		}
		fmt.Println(strings.Repeat("-", 74))
		t := report.Total
		fmt.Printf("%-18s %7d %10d %7s %11d %7s %9s\n",
			"TOTAL", t.Nodes, t.Reachable, pct(t.Reachable, t.Nodes), t.Unlockable, pct(t.Unlockable, t.Nodes), degraded(t.Degraded))
		if t.ValueTransiting > 0 {
			// Worth its own line: these are the surfaces where a secret passes
			// through a process Compound started, which is the exposure an
			// operator would otherwise have to infer by reading mechanisms.
			fmt.Printf("\n%d credential handle(s) pass a value through a Compound-started process.\n", t.ValueTransiting)
		}
		if len(report.Harnesses) > 0 {
			fmt.Println("\nper harness — 'generic' is offered to EVERY harness, runnable or not:")
			for _, h := range report.Harnesses {
				fmt.Printf("  %-24s scoped-to-it %4d   generic %4d   curated %5.1f%%\n",
					h.Harness, h.Bound, h.Generic, h.ScopedShare*100)
			}
		}
		if c := report.Corrections; c.Observations > 0 {
			fmt.Printf("\nrouting corrections already recorded: %d observation(s) over %d wrongly-fired and %d missed capability(ies).\n",
				c.Observations, c.FalseTriggeredNodes, c.MissedNodes)
			if c.UnscopedWithCorrections > 0 {
				fmt.Printf("  %d of those capabilities still carry NO harness scope — the correction was recorded but changed nothing,\n"+
					"  so the same misroute stays available and the operator keeps making it by hand.\n", c.UnscopedWithCorrections)
			}
		}
		if len(report.Gaps) > 0 {
			fmt.Println("\ngaps, worst first — each is one missing import door, not per-node data entry:")
			for _, g := range report.Gaps {
				fmt.Printf("  %-16s %4d of %4d unreachable — %s\n", g.Kind, g.Missing, g.Nodes, g.Consequence)
			}
		}
	case "authority":
		records, err := capgraph.LoadSourceAuthority(homePath)
		die(err)
		stats, err := store.Stats()
		die(err)
		sourceNodeCount := 0
		sourceEdgeCount := 0
		for _, rec := range records {
			sourceNodeCount += len(rec.NodeIDs)
			sourceEdgeCount += len(rec.EdgeIDs)
		}
		sourceSummary := capgraph.SummarizeSourceAuthority(records)
		if hasFlag(rest, "--json") {
			out := struct {
				SchemaVersion               string                           `json:"schema_version"`
				AuthorityRef                string                           `json:"authority_ref"`
				NodeCount                   int                              `json:"node_count"`
				EdgeCount                   int                              `json:"edge_count"`
				EdgeKindCounts              map[string]int                   `json:"edge_kind_counts,omitempty"`
				SourceRecordCount           int                              `json:"source_record_count"`
				SourceNodeCount             int                              `json:"source_node_count"`
				SourceEdgeCount             int                              `json:"source_edge_count"`
				SourceRoleCounts            map[string]int                   `json:"source_role_counts,omitempty"`
				RetiredSourceAuthorityCount int                              `json:"retired_source_authority_count"`
				RetiredSourceAuthorityRefs  []string                         `json:"retired_source_authority_refs,omitempty"`
				Records                     []capgraph.SourceAuthorityRecord `json:"records"`
			}{
				SchemaVersion:               capgraph.SourceAuthoritySchemaVersion,
				AuthorityRef:                "compound://registries/capability-graph",
				NodeCount:                   stats.NodeCount,
				EdgeCount:                   stats.EdgeCount,
				EdgeKindCounts:              stats.EdgeKindCounts,
				SourceRecordCount:           len(records),
				SourceNodeCount:             sourceNodeCount,
				SourceEdgeCount:             sourceEdgeCount,
				SourceRoleCounts:            sourceSummary.SourceRoleCounts,
				RetiredSourceAuthorityCount: sourceSummary.RetiredSourceAuthorityCount,
				RetiredSourceAuthorityRefs:  sourceSummary.RetiredSourceAuthorityRefs,
				Records:                     records,
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			die(enc.Encode(out))
			return
		}
		if len(records) == 0 {
			fmt.Println("(no capability source authority records)")
			return
		}
		fmt.Printf("compound://registries/capability-graph nodes=%d edges=%d source_records=%d source_nodes=%d source_edges=%d retired_source_authority=%d\n", stats.NodeCount, stats.EdgeCount, len(records), sourceNodeCount, sourceEdgeCount, sourceSummary.RetiredSourceAuthorityCount)
		if len(stats.EdgeKindCounts) > 0 {
			fmt.Printf("edge_kinds=%s\n", formatEdgeKindCounts(stats.EdgeKindCounts))
		}
		if len(sourceSummary.SourceRoleCounts) > 0 {
			fmt.Printf("source_roles=%s\n", formatEdgeKindCounts(sourceSummary.SourceRoleCounts))
		}
		for _, rec := range records {
			fmt.Printf("%s %s source=%s authority=%s nodes=%d edges=%d retires=%s\n",
				rec.ID,
				rec.AuthorityStatus,
				rec.SourcePath,
				rec.AuthorityRef,
				len(rec.NodeIDs),
				len(rec.EdgeIDs),
				rec.Retires,
			)
		}
	case "outbound-distribution-plan":
		task := flagValue(rest, "--task")
		if task == "" {
			p := posn(map[string]bool{"--task": true})
			task = strings.TrimSpace(strings.Join(p, " "))
		}
		if task == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability outbound-distribution-plan --task <task> [--json]")
			os.Exit(2)
		}
		report, err := capgraph.BuildOutboundDistributionPlan(homePath, task, time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability outbound-distribution-plan %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- adoption_level: %s\n", report.AdoptionLevel)
		fmt.Printf("- candidates: %d\n", len(report.Candidates))
		for _, gate := range report.RequiredGates {
			fmt.Printf("- gate: %s ref=%s\n", gate.Kind, gate.Ref)
		}
		for _, candidate := range report.Candidates {
			fmt.Printf("- candidate: %s role=%s decision=%s graph=%s node=%s next=%s\n",
				candidate.Repo,
				candidate.Role,
				candidate.Decision,
				candidate.CapabilityGraphStatus,
				candidate.NodeStatus,
				candidate.NextReviewStep,
			)
		}
		for _, boundary := range report.Boundaries {
			fmt.Printf("- boundary: %s\n", boundary)
		}
		if report.NextAction != "" {
			fmt.Printf("- next_action: %s\n", report.NextAction)
		}
	case "outbound-distribution-verdict-plan":
		opts := capgraph.OutboundDistributionVerdictPlanOptions{
			Task:              flagValue(rest, "--task"),
			ProductID:         flagValue(rest, "--product"),
			ChangeEvent:       flagValue(rest, "--change-event"),
			Hypothesis:        flagValue(rest, "--hypothesis"),
			ObservationWindow: flagValue(rest, "--observation-window"),
			DryRunReceiptRef:  flagValue(rest, "--dry-run-receipt"),
			TargetPath:        flagValue(rest, "--target"),
		}
		if opts.ProductID == "" {
			opts.ProductID = flagValue(rest, "--product-id")
		}
		if opts.Task == "" || opts.ProductID == "" || opts.ChangeEvent == "" || opts.Hypothesis == "" || opts.ObservationWindow == "" || opts.DryRunReceiptRef == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability outbound-distribution-verdict-plan --task <task> --product <product_id> --change-event <text> --hypothesis <text> --observation-window <window> --dry-run-receipt <ref> [--target <file>] [--json]")
			os.Exit(2)
		}
		plan, err := capgraph.BuildOutboundDistributionVerdictPlan(homePath, opts, time.Now())
		die(err)
		paths, err := capgraph.RecordOutboundDistributionVerdictPlan(homePath, plan)
		die(err)
		report := struct {
			SchemaVersion  string                                   `json:"schema_version"`
			Status         string                                   `json:"status"`
			Plan           capgraph.OutboundDistributionVerdictPlan `json:"plan"`
			PlanPath       string                                   `json:"plan_path"`
			ReviewPath     string                                   `json:"review_path"`
			ReviewYAMLPath string                                   `json:"review_yaml_path"`
		}{
			SchemaVersion:  capgraph.OutboundDistributionVerdictPlanSchemaVersion,
			Status:         plan.Status,
			Plan:           plan,
			PlanPath:       paths.PlanPath,
			ReviewPath:     paths.ReviewPath,
			ReviewYAMLPath: paths.ReviewYAMLPath,
		}
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability outbound-distribution-verdict-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- candidates: %d\n", len(plan.Candidates))
		fmt.Printf("- adoption_level: %s\n", plan.AdoptionLevel)
		fmt.Printf("- dry_run_receipt: %s\n", plan.DryRunReceiptRef)
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
	case "outbound-distribution-verdict-review":
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability outbound-distribution-verdict-review <review.yaml>")
			os.Exit(2)
		}
		review, err := capgraph.LoadOutboundDistributionVerdictReview(p[0])
		die(err)
		report, err := capgraph.RecordOutboundDistributionVerdictReview(homePath, review, p[0], time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability outbound-distribution-verdict-review %s\n\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- plan: %s\n", report.PlanID)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- reviewed: %s\n", report.ReviewedPath)
	case "outbound-distribution-dry-run-receipt":
		opts := capgraph.OutboundDistributionDryRunReceiptOptions{
			VerdictReviewPath: flagValue(rest, "--verdict-review"),
			AdapterOutputPath: flagValue(rest, "--adapter-output"),
			TargetPath:        flagValue(rest, "--target"),
		}
		if opts.VerdictReviewPath == "" || opts.AdapterOutputPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability outbound-distribution-dry-run-receipt --verdict-review <reviewed.json> --adapter-output <adapter-output.json> [--target <file>] [--json]")
			os.Exit(2)
		}
		report, err := capgraph.BuildOutboundDistributionDryRunReceipt(homePath, opts, time.Now())
		die(err)
		paths, err := capgraph.RecordOutboundDistributionDryRunReceipt(homePath, report)
		die(err)
		out := struct {
			SchemaVersion  string                                           `json:"schema_version"`
			Status         string                                           `json:"status"`
			Report         capgraph.OutboundDistributionDryRunReceiptReport `json:"report"`
			ReportPath     string                                           `json:"report_path"`
			ReviewPath     string                                           `json:"review_path"`
			ReviewYAMLPath string                                           `json:"review_yaml_path"`
		}{
			SchemaVersion:  capgraph.OutboundDistributionDryRunReceiptReportSchemaVersion,
			Status:         report.Status,
			Report:         report,
			ReportPath:     paths.ReportPath,
			ReviewPath:     paths.ReviewPath,
			ReviewYAMLPath: paths.ReviewYAMLPath,
		}
		if hasFlag(rest, "--json") {
			printJSON(out)
			return
		}
		fmt.Printf("# capability outbound-distribution-dry-run-receipt %s\n\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- plan: %s\n", report.PlanID)
		fmt.Printf("- adoption_level: %s\n", report.AdoptionLevel)
		fmt.Printf("- artifacts: %d\n", len(report.Artifacts))
		fmt.Printf("- report: %s\n", paths.ReportPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
	case "outbound-distribution-dry-run-review":
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability outbound-distribution-dry-run-review <review.yaml>")
			os.Exit(2)
		}
		review, err := capgraph.LoadOutboundDistributionDryRunReview(p[0])
		die(err)
		report, err := capgraph.RecordOutboundDistributionDryRunReview(homePath, review, p[0], time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability outbound-distribution-dry-run-review %s\n\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- plan: %s\n", report.PlanID)
		fmt.Printf("- receipt: %s\n", report.ReceiptID)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- reviewed: %s\n", report.ReviewedPath)
	case "external-intake":
		repoQuery := flagValue(rest, "--repo")
		sourceListPath := flagValue(rest, "--source-list")
		if sourceListPath == "" {
			sourceListPath = flagValue(rest, "--repo-file")
		}
		var report capgraph.ExternalRepoIntakeReport
		var err error
		if sourceListPath != "" {
			report, err = capgraph.ExternalRepoIntakeReportForHomeWithSourceList(homePath, sourceListPath, repoQuery)
		} else {
			report, err = capgraph.ExternalRepoIntakeReportForHome(homePath, repoQuery)
		}
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability external-intake %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- batches: %d\n", report.BatchCount)
		if report.LatestBatchID != "" {
			fmt.Printf("- latest_batch: %s\n", report.LatestBatchID)
		}
		fmt.Printf("- candidates: %d\n", report.CandidateCount)
		fmt.Printf("- keep_needs_review_decision: %d\n", report.CountsByDecision["keep_needs_review"])
		fmt.Printf("- open_owner_reviews: %d\n", report.OpenReviewCount)
		fmt.Printf("- merge_existing: %d\n", report.MergeExistingCount)
		fmt.Printf("- reference_only: %d\n", report.ReferenceOnlyCount)
		if report.SourceObservationID != "" {
			fmt.Printf("- source_observation: %s\n", report.SourceObservationID)
		}
		if report.SourceListPath != "" {
			fmt.Printf("- source_list: %s total=%d known=%d unknown=%d\n", report.SourceListPath, report.SourceListTotal, report.SourceListKnown, report.SourceListUnknown)
			for _, decision := range sortedMapKeys(report.SourceListByDecision) {
				fmt.Printf("- source_list_decision.%s: %d\n", decision, report.SourceListByDecision[decision])
			}
			for _, lane := range sortedMapKeys(report.SourceListByLane) {
				fmt.Printf("- source_list_lane.%s: %d\n", lane, report.SourceListByLane[lane])
			}
			for _, owner := range sortedMapKeys(report.SourceListByOwner) {
				fmt.Printf("- source_list_owner.%s: %d\n", owner, report.SourceListByOwner[owner])
			}
			for _, status := range sortedMapKeys(report.SourceListByOwnerReviewStatus) {
				fmt.Printf("- source_list_owner_review.%s: %d\n", status, report.SourceListByOwnerReviewStatus[status])
			}
			for _, status := range sortedMapKeys(report.SourceListByCapabilityEvaluationStatus) {
				fmt.Printf("- source_list_capability_evaluation.%s: %d\n", status, report.SourceListByCapabilityEvaluationStatus[status])
			}
			for _, status := range sortedMapKeys(report.SourceListByCapabilityGraphStatus) {
				fmt.Printf("- source_list_capability_graph.%s: %d\n", status, report.SourceListByCapabilityGraphStatus[status])
			}
			if report.SourceListReviewNextMeaning != "" || report.SourceListEvalNextMeaning != "" {
				fmt.Printf("- source_list_follow_up: review_next=%d meaning=%s evaluation_next=%d meaning=%s\n", len(report.SourceListReviewNext), report.SourceListReviewNextMeaning, len(report.SourceListEvalNext), report.SourceListEvalNextMeaning)
			}
			if report.SourceListGraphImportNextMeaning != "" {
				fmt.Printf("- source_list_graph_import_follow_up: graph_import_next=%d omitted=%d meaning=%s\n", len(report.SourceListGraphImportNext), report.SourceListGraphImportNextOmitted, report.SourceListGraphImportNextMeaning)
			}
			for _, item := range report.SourceListReviewNext {
				first := item.Matches[0]
				fmt.Printf("- source_list_review_next: %s lane=%s owner=%s evidence=%.2f next=%s boundary=%s\n", first.Repo, first.Lane, first.Owner, first.EvidenceStrength, first.NextReviewStep, first.NotToDo)
			}
			for _, item := range report.SourceListEvalNext {
				first := item.Matches[0]
				next := first.OwnerReviewNextAction
				if next == "" {
					next = first.NextReviewStep
				}
				fmt.Printf("- source_list_evaluation_next: %s lane=%s owner=%s evidence=%.2f review=%s next=%s\n", first.Repo, first.Lane, first.Owner, first.EvidenceStrength, first.OwnerReviewPath, next)
			}
			for _, item := range report.SourceListGraphImportNext {
				first := item.Matches[0]
				fmt.Printf("- source_list_graph_import_next: %s node=%s node_status=%s manifest=%s next=%s\n", first.Repo, first.CapabilityEvaluationNodeID, first.CapabilityEvaluationNodeStatus, first.CapabilityEvaluationManifestPath, first.CapabilityEvaluationNextAction)
			}
			knownDetailsOmitted := 0
			for _, item := range report.SourceListItems {
				if item.Status == "unknown" {
					fmt.Printf("- source_list_unknown: %s normalized=%s\n", item.Query, item.Normalized)
					continue
				}
				knownDetailsOmitted++
			}
			if knownDetailsOmitted > 0 {
				fmt.Printf("- source_list_known_omitted: %d details=--json\n", knownDetailsOmitted)
			}
		}
		if report.RepoQuery != "" {
			fmt.Printf("- repo_query: %s matches=%d\n", report.RepoQuery, len(report.Matches))
			for _, row := range report.Matches {
				fmt.Printf("- repo_match: %s decision=%s lane=%s owner=%s next=%s boundary=%s\n", row.Repo, row.Decision, row.Lane, row.Owner, row.NextReviewStep, row.NotToDo)
			}
			return
		}
		for _, row := range report.HighValue {
			fmt.Printf("- high_value: %s decision=%s lane=%s owner_review=%s capability_evaluation=%s graph=%s owner=%s\n", row.Repo, row.Decision, row.Lane, row.OwnerReviewStatus, row.CapabilityEvaluationStatus, row.CapabilityGraphStatus, row.Owner)
		}
	case "external-intake-import-reviewed":
		sourceListPath := flagValue(rest, "--source-list")
		if sourceListPath == "" {
			sourceListPath = flagValue(rest, "--repo-file")
		}
		if sourceListPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability external-intake-import-reviewed --source-list <file> [--limit N] [--json]")
			os.Exit(2)
		}
		limit, err := intFlag(rest, "--limit")
		die(err)
		report, err := capgraph.ImportExternalRepoReviewedManifests(homePath, sourceListPath, limit, time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability external-intake-import-reviewed %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- source_list: %s total=%d\n", report.SourceListPath, report.SourceListTotal)
		fmt.Printf("- pending_import: %d\n", report.PendingImportCount)
		if report.ImportLimit > 0 {
			fmt.Printf("- import_limit: %d\n", report.ImportLimit)
		}
		fmt.Printf("- imported: %d\n", report.ImportedCount)
		if report.OmittedPendingCount > 0 {
			fmt.Printf("- omitted_pending: %d\n", report.OmittedPendingCount)
		}
		for _, item := range report.Items {
			fmt.Printf("- imported_manifest: %s node=%s node_status=%s manifest=%s\n", item.Repo, item.NodeID, item.NodeStatus, item.ManifestPath)
		}
	case "external-intake-review-plan":
		sourceListPath := flagValue(rest, "--source-list")
		if sourceListPath == "" {
			sourceListPath = flagValue(rest, "--repo-file")
		}
		if sourceListPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability external-intake-review-plan --source-list <file> [--json]")
			os.Exit(2)
		}
		report, err := capgraph.WriteExternalRepoIntakeReviewPlan(homePath, sourceListPath, time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# capability external-intake-review-plan %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- review_area: %s\n", report.ReviewArea)
		fmt.Printf("- source_list: %s total=%d known=%d unknown=%d\n", report.SourceListPath, report.SourceListTotal, report.SourceListKnown, report.SourceListUnknown)
		if report.LatestBatchID != "" {
			fmt.Printf("- latest_batch: %s\n", report.LatestBatchID)
		}
		fmt.Printf("- wrote: %d\n", report.WroteCount)
		for _, item := range report.Items {
			fmt.Printf("- review_artifact: %s repo=%s decision=%s owner=%s next=%s json=%s\n", item.ID, item.Repo, item.Decision, item.Owner, item.NextAction, item.JSONPath)
		}
	case "external-intake-evaluation-plan":
		jsonOut := hasFlag(rest, "--json")
		opts := capgraph.ExternalRepoCapabilityEvaluationPlanOptions{
			SourceListPath: flagValue(rest, "--source-list"),
			RepoQuery:      flagValue(rest, "--repo"),
			Capability:     flagValue(rest, "--capability"),
			TargetPath:     flagValue(rest, "--target"),
		}
		if opts.SourceListPath == "" {
			opts.SourceListPath = flagValue(rest, "--repo-file")
		}
		p := posn(map[string]bool{"--source-list": true, "--repo-file": true, "--repo": true, "--capability": true, "--target": true})
		if len(p) != 0 || opts.SourceListPath == "" || opts.RepoQuery == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability external-intake-evaluation-plan --source-list <file> --repo <repo> [--capability <name>] [--target <file>] [--json]")
			os.Exit(2)
		}
		report, err := capgraph.RecordExternalRepoCapabilityEvaluationPlan(homePath, opts, time.Now())
		die(err)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# capability external-intake-evaluation-plan %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- source_list: %s\n", report.SourceListPath)
		fmt.Printf("- repo: %s\n", report.Repo)
		if report.Owner != "" {
			fmt.Printf("- owner: %s\n", report.Owner)
		}
		if report.OwnerReviewPath != "" {
			fmt.Printf("- owner_review: %s\n", report.OwnerReviewPath)
		}
		if report.OwnerReviewNextAction != "" {
			fmt.Printf("- next_action: %s\n", report.OwnerReviewNextAction)
		}
		fmt.Printf("- capability: %s\n", report.Capability)
		fmt.Printf("- evaluation: %s\n", report.EvaluationPath)
		fmt.Printf("- plan: %s\n", report.PlanPath)
		fmt.Printf("- review: %s\n", report.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", report.ReviewYAMLPath)
		fmt.Printf("- manifest_draft: %s\n", report.ManifestDraftPath)
	case "external-intake-review":
		jsonOut := hasFlag(rest, "--json")
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability external-intake-review <review.yaml> [--json]")
			os.Exit(2)
		}
		review, err := capgraph.LoadExternalRepoIntakeReview(p[0])
		die(err)
		report, err := capgraph.RecordExternalRepoIntakeReview(homePath, review, p[0], time.Now())
		die(err)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# capability external-intake-review %s\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- repo: %s\n", report.Repo)
		fmt.Printf("- decision: %s\n", report.Decision)
		if report.Owner != "" {
			fmt.Printf("- owner: %s\n", report.Owner)
		}
		if report.NextAction != "" {
			fmt.Printf("- next_action: %s\n", report.NextAction)
		}
		fmt.Printf("- reviewed: %s\n", report.ReviewPath)
	case "import-evaluations":
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability import-evaluations <capability-evaluation.json|dir>")
			os.Exit(2)
		}
		now := time.Now()
		set, err := capgraph.LoadCapabilityEvaluations(p[0])
		die(err)
		report, err := capgraph.ApplyCapabilityEvaluations(homePath, set, now)
		die(err)
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromCapabilityEvaluations(set, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		fmt.Printf("# capability import-evaluations %s\n", report.EvaluationSetID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- evaluations_read: %d\n", report.EvaluationsRead)
		fmt.Printf("- applied_evaluations: %d\n", report.AppliedEvaluations)
		fmt.Printf("- skipped_unknown_count: %d\n", len(report.SkippedUnknown))
		for _, id := range report.EvaluationNodeIDs {
			fmt.Printf("- eval_node: %s\n", id)
		}
		for _, id := range report.EdgeIDs {
			fmt.Printf("- edge: %s\n", id)
		}
		for _, id := range report.SkippedUnknown {
			fmt.Printf("- skipped_unknown: %s\n", id)
		}
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		for _, id := range sourceReport.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
		}
		for _, id := range sourceReport.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
	case "import-surfacing-misses":
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability import-surfacing-misses <skill-surfacing-feedback.json>")
			os.Exit(2)
		}
		now := time.Now()
		misses, err := capgraph.LoadSkillSurfacingMisses(p[0])
		die(err)
		report, err := capgraph.ApplySkillSurfacingMisses(homePath, misses, now)
		die(err)
		sourceReport, err := sourceobs.Apply(homePath, sourceobs.FromSkillSurfacingMisses(misses, now), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "source observe:", err)
			os.Exit(1)
		}
		actor := firstNonEmpty(os.Getenv("COMPOUND_OPERATOR_ID"), os.Getenv("COMPOUND_REVIEWED_BY"), "operator")
		reviewResolved, reviewResolution, err := capgraph.ResolveSkillSurfacingMissesReviewItem(homePath, p[0], actor, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "review resolve:", err)
			os.Exit(1)
		}
		fmt.Printf("# capability import-surfacing-misses %s\n", report.ReportID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- misses_read: %d\n", report.MissesRead)
		fmt.Printf("- false_triggers_read: %d\n", report.FalseTriggersRead)
		fmt.Printf("- applied_misses: %d\n", report.AppliedMisses)
		fmt.Printf("- applied_false_triggers: %d\n", report.AppliedFalseTriggers)
		fmt.Printf("- applied_edges: %d\n", report.AppliedEdges)
		fmt.Printf("- skipped_unknown_count: %d\n", len(report.SkippedUnknown))
		for _, id := range report.EdgeIDs {
			fmt.Printf("- edge: %s\n", id)
		}
		for _, id := range report.SkippedUnknown {
			fmt.Printf("- skipped_unknown: %s\n", id)
		}
		for _, id := range report.SkippedMissingContext {
			fmt.Printf("- skipped_missing_context: %s\n", id)
		}
		fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
		for _, id := range sourceReport.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
		}
		for _, id := range sourceReport.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		fmt.Printf("- review_resolved: %t\n", reviewResolved)
		if reviewResolved {
			fmt.Printf("- review_receipt: %s\n", reviewResolution.ReceiptID)
			fmt.Printf("- review_undo: %s\n", reviewResolution.UndoHint)
		}
	case "evaluation-plan":
		capabilityName := flag("--capability")
		targetPath := flag("--target")
		p := posn(map[string]bool{"--capability": true, "--target": true})
		if len(p) != 1 || capabilityName == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability evaluation-plan <capability-evaluation.json|dir> --capability <id-or-name> [--target file]")
			os.Exit(2)
		}
		now := time.Now()
		set, err := capgraph.LoadCapabilityEvaluations(p[0])
		die(err)
		plan, err := capgraph.BuildCapabilityEvaluationPlan(homePath, set, capabilityName, targetPath, now)
		die(err)
		paths, err := capgraph.RecordCapabilityEvaluationPlan(homePath, plan)
		die(err)
		fmt.Printf("# capability evaluation-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- evaluation_set: %s\n", plan.EvaluationSet)
		fmt.Printf("- evaluation: %s\n", plan.EvaluationID)
		fmt.Printf("- capability: %s\n", plan.Capability)
		fmt.Printf("- verdict: %s\n", plan.Verdict)
		fmt.Printf("- node_draft: %s\n", plan.NodeDraft.NodeID)
		fmt.Printf("- node_kind: %s\n", plan.NodeDraft.Kind)
		fmt.Printf("- required_evidence: %d\n", len(plan.NodeDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
		fmt.Printf("- manifest_draft: %s\n", paths.ManifestDraftPath)
	case "evaluation-review":
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability evaluation-review <review.yaml>")
			os.Exit(2)
		}
		review, err := capgraph.LoadCapabilityEvaluationReview(p[0])
		die(err)
		report, err := capgraph.RecordCapabilityEvaluationReview(homePath, review, p[0], time.Now())
		die(err)
		fmt.Printf("# capability evaluation-review %s\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- node: %s\n", report.NodeID)
		fmt.Printf("- node_status: %s\n", report.NodeStatus)
		fmt.Printf("- manifest: %s\n", report.ManifestPath)
		if report.PlanPath != "" {
			fmt.Printf("- plan: %s\n", report.PlanPath)
		}
		if report.ReviewedPath != "" {
			fmt.Printf("- reviewed: %s\n", report.ReviewedPath)
		}
	case "skill-candidate-review":
		targetPath := flag("--target")
		jsonOut := hasFlag(rest, "--json")
		p := posn(map[string]bool{"--target": true})
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability skill-candidate-review <external-skill-candidate.json> [--target <file-or-dir>] [--json]")
			os.Exit(2)
		}
		review, err := capgraph.BuildExternalSkillCandidateReview(homePath, p[0], targetPath, time.Now())
		die(err)
		paths, err := capgraph.RecordExternalSkillCandidateReview(homePath, review)
		die(err)
		storedReview, err := capgraph.LoadSkillCandidateReview(paths.PlanPath)
		die(err)
		review = storedReview
		if jsonOut {
			printJSON(struct {
				Review capgraph.SkillCandidateReview `json:"review"`
				Paths  struct {
					PlanPath       string `json:"plan_path"`
					ReviewPath     string `json:"review_path"`
					ReviewYAMLPath string `json:"review_yaml_path"`
				} `json:"paths"`
			}{
				Review: review,
				Paths: struct {
					PlanPath       string `json:"plan_path"`
					ReviewPath     string `json:"review_path"`
					ReviewYAMLPath string `json:"review_yaml_path"`
				}{
					PlanPath:       paths.PlanPath,
					ReviewPath:     paths.ReviewPath,
					ReviewYAMLPath: paths.ReviewYAMLPath,
				},
			})
			return
		}
		fmt.Printf("# capability skill-candidate-review %s\n", review.ID)
		fmt.Printf("- status: %s\n", review.Status)
		fmt.Printf("- candidate_kind: %s\n", review.CandidateKind)
		fmt.Printf("- candidate: %s\n", review.CandidateID)
		fmt.Printf("- classification: %s\n", review.ProposedClassification)
		fmt.Printf("- lifecycle_stage: %s\n", review.LifecycleStage)
		fmt.Printf("- disposition: %s\n", review.RecommendedDisposition)
		fmt.Printf("- overlaps: %d\n", len(review.OverlapRefs))
		fmt.Printf("- evidence_required: %d\n", len(review.EvidenceRequired))
		fmt.Printf("- eval_suggestions: %d\n", len(review.EvalFixtureSuggestions))
		if review.TargetPath != "" {
			fmt.Printf("- target: %s\n", review.TargetPath)
			fmt.Printf("- target_exists: %t\n", review.TargetExists)
		}
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
	case "skill-candidate-decision":
		jsonOut := hasFlag(rest, "--json")
		p := posn(nil)
		if len(p) != 1 {
			fmt.Fprintln(os.Stderr, "usage: compound capability skill-candidate-decision <review.yaml> [--json]")
			os.Exit(2)
		}
		decision, err := capgraph.LoadSkillCandidateDecision(p[0])
		die(err)
		report, err := capgraph.RecordSkillCandidateDecision(homePath, decision, p[0], time.Now())
		die(err)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# capability skill-candidate-decision %s\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- review: %s\n", report.ReviewID)
		fmt.Printf("- decision: %s\n", report.Decision)
		if report.CandidateID != "" {
			fmt.Printf("- candidate: %s\n", report.CandidateID)
			fmt.Printf("- candidate_path: %s\n", report.CandidatePath)
		}
	case "skill-plan":
		candidateID := flag("--candidate")
		targetPath := flag("--target")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound capability skill-plan --candidate <candidate_id> [--target <file>]")
			os.Exit(2)
		}
		plan, err := capgraph.BuildSkillPlan(homePath, candidateID, targetPath, time.Now())
		die(err)
		paths, err := capgraph.RecordSkillPlan(homePath, plan)
		die(err)
		fmt.Printf("# capability skill-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- skill_key: %s\n", plan.SkillKey)
		fmt.Printf("- node_id: %s\n", plan.NodeDraft.NodeID)
		if plan.TargetPath != "" {
			fmt.Printf("- target: %s\n", plan.TargetPath)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- triggers: %d\n", len(plan.NodeDraft.Triggers))
		fmt.Printf("- required_evidence: %d\n", len(plan.NodeDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
	case "add":
		p := posn(map[string]bool{"--triggers": true})
		if len(p) < 3 {
			fmt.Fprintln(os.Stderr, "usage: compound capability add <id> <kind> <name> --triggers a,b,c")
			os.Exit(2)
		}
		var trig []string
		if t := flag("--triggers"); t != "" {
			trig = strings.Split(t, ",")
		}
		die(capgraph.AddGoverned(homePath, capgraph.Node{ID: p[0], Kind: p[1], Name: p[2], Triggers: trig}, time.Now()))
		fmt.Printf("added node %s (%s)\n", p[0], p[1])
	case "retire":
		// `capability retire <id>...` withdraws nodes from surfacing (durable
		// lifecycle fact). The store reloads each node so its kind/name/triggers
		// survive; only Retired flips. Retired nodes never surface and never
		// execute. Distinct from Health (broken-but-named).
		p := posn(map[string]bool{})
		if len(p) == 0 {
			fmt.Fprintln(os.Stderr, "usage: compound capability retire <id> [<id>...]")
			os.Exit(2)
		}
		retired, err := capgraph.RetireGoverned(homePath, p, time.Now())
		if err != nil {
			// Unknown ids are reported; already-retired ones are still listed.
			fmt.Fprintln(os.Stderr, "error:", err)
			if len(retired) > 0 {
				fmt.Fprintf(os.Stderr, "retired: %v\n", retired)
			}
			os.Exit(1)
		}
		fmt.Printf("retired %d node(s): %v\n", len(retired), retired)
	case "resolve":
		jsonOut := hasFlag(rest, "--json")
		harness := flag("--harness")
		p := posn(map[string]bool{"--harness": true})
		if len(p) == 0 {
			fmt.Fprintln(os.Stderr, `usage: compound capability resolve [--harness codex|claude_code] [--privacy-sensitive] [--json] "<task>"`)
			os.Exit(2)
		}
		report, err := buildCapabilityResolutionReport(store, homePath, strings.Join(p, " "), harness, hasFlag(rest, "--privacy-sensitive"), false, time.Now())
		die(err)
		if jsonOut {
			die(json.NewEncoder(os.Stdout).Encode(report))
			return
		}
		printCapabilityResolution(report)
	case "surface":
		jsonOut := hasFlag(rest, "--json")
		harness := flag("--harness")
		p := posn(map[string]bool{"--harness": true})
		if len(p) == 0 {
			fmt.Fprintln(os.Stderr, `usage: compound capability surface [--harness codex|claude_code] [--json] "<task>"`)
			os.Exit(2)
		}
		report, err := buildCapabilitySurfaceReport(store, homePath, p[0], harness)
		die(err)
		if jsonOut {
			die(json.NewEncoder(os.Stdout).Encode(report))
			return
		}
		fmt.Printf("# %d capabilit(ies) surfaced for %q (offered, NOT executed)\n", report.OfferCount, report.Task)
		for _, evidence := range report.MatchedEvidence {
			fmt.Printf(" evidence: %s ref=%s confidence=%.2f detail=%s\n", evidence.Kind, evidence.Ref, evidence.Confidence, evidence.Detail)
		}
		for _, h := range report.Offers {
			fmt.Printf("  %-28s %-12s score %.1f  [%s]\n", h.Node.ID, h.Node.Kind, h.Score, h.Why)
			printCapabilityConstraints(h.Constraints)
		}
	case "observe":
		node := posn(map[string]bool{"--signal": true, "--context": true, "--conf": true})
		signal, context := flag("--signal"), flag("--context")
		conf := 0.8
		if v := flag("--conf"); v != "" {
			parsed, perr := strconv.ParseFloat(v, 64)
			if perr != nil {
				die(fmt.Errorf("--conf %q is not a number: %w", v, perr))
			}
			conf = parsed
		}
		if len(node) == 0 {
			fmt.Fprintln(os.Stderr, "usage: compound capability observe <node> --signal <missed_when|false_triggered_by> --context <phrase> [--conf 0.8]")
			os.Exit(2)
		}
		edges, err := capgraph.ObserveGoverned(homePath, node[0], signal, context, conf, time.Now())
		die(err)
		for _, e := range edges {
			fmt.Printf("recorded %s: %s -> %s (conf %.2f)\n", e.Kind, e.From, e.To, e.Confidence)
		}
	case "consume":
		n, err := store.Consume(homePath, time.Now())
		die(err)
		fmt.Printf("applied %d capability observation(s) from the distiller inbox\n", n)
	case "consume-receipts":
		report, err := store.ConsumeReceipts(homePath, time.Now())
		die(err)
		fmt.Println("# capability consume-receipts")
		fmt.Printf("- receipts_scanned: %d\n", report.ReceiptsScanned)
		fmt.Printf("- used_capabilities_seen: %d\n", report.UsedCapabilitiesSeen)
		fmt.Printf("- missed_capabilities_seen: %d\n", report.MissedCapabilitiesSeen)
		fmt.Printf("- false_triggers_seen: %d\n", report.FalseTriggersSeen)
		fmt.Printf("- applied_edges: %d\n", report.AppliedEdges)
		fmt.Printf("- applied_validated_edges: %d\n", report.AppliedValidatedEdges)
		fmt.Printf("- applied_declared_use_edges: %d\n", report.AppliedDeclaredUseEdges)
		fmt.Printf("- applied_missed_edges: %d\n", report.AppliedMissedEdges)
		fmt.Printf("- applied_false_edges: %d\n", report.AppliedFalseEdges)
		fmt.Printf("- skipped_non_completed: %d\n", report.SkippedNonCompleted)
		fmt.Printf("- skipped_unchanged: %d\n", report.SkippedUnchanged)
		fmt.Printf("- skipped_missing_context: %d\n", report.SkippedMissingContext)
		fmt.Printf("- skipped_invalid_receipt: %d\n", report.SkippedInvalidReceipt)
		for _, id := range report.SkippedUnknown {
			fmt.Printf("- skipped_unknown: %s\n", id)
		}
	case "usage":
		var since time.Time
		if s := strings.TrimSpace(flag("--since")); s != "" {
			parsed, perr := time.Parse(time.RFC3339, s)
			if perr != nil {
				fmt.Fprintln(os.Stderr, "error: --since must be RFC3339 (e.g. 2026-07-01T00:00:00Z):", perr)
				os.Exit(2)
			}
			since = parsed
		}
		top := 0
		if t := strings.TrimSpace(flag("--top")); t != "" {
			parsed, perr := strconv.Atoi(t)
			if perr != nil || parsed < 0 {
				fmt.Fprintln(os.Stderr, "error: --top must be a non-negative integer")
				os.Exit(2)
			}
			top = parsed
		}
		proj, err := capgraph.BuildCapabilityUsage(homePath, capgraph.UsageBuildOptions{
			Since:       since,
			GeneratedAt: time.Now(),
			SummaryTop:  top,
		})
		die(err)
		path, werr := capgraph.WriteCapabilityUsage(homePath, proj)
		die(werr)
		if hasFlag(rest, "--json") {
			printJSON(proj)
			return
		}
		die(capgraph.WriteCapabilityUsageReport(os.Stdout, path, proj, top))
	case "list":
		// "What exists" was already answered here. "What can I actually use,
		// from THIS harness, right now" was not -- and that second question is
		// the one an executor and an operator both ask. The state flags below
		// carry it without a second command to learn.
		nodes, err := store.List()
		die(err)
		inv, err := store.CapabilityInventory()
		die(err)
		harness := valueFlag(rest, "--harness")
		kindFilter := valueFlag(rest, "--kind")
		onlyUnreachable := hasFlag(rest, "--unreachable")
		onlyCorrected := hasFlag(rest, "--corrected")
		showRetired := hasFlag(rest, "--include-retired")
		if hasFlag(rest, "--json") {
			printJSON(inv.Filter(harness, kindFilter, onlyUnreachable, onlyCorrected, showRetired))
			return
		}
		shown := 0
		for _, n := range nodes {
			st := inv.State(n.ID)
			if !st.Visible(harness, kindFilter, onlyUnreachable, onlyCorrected, showRetired) {
				continue
			}
			shown++
			fmt.Printf("%-30s %-16s %-9s %s%s\n", n.ID, n.Kind, st.Flags(), n.Name, st.Note())
		}
		fmt.Printf("\n%d of %d shown. flags: R=reachable  K=unlockable  S=harness-scoped  !=has routing corrections  D=degraded (not offered until healthy)\n",
			shown, len(nodes))
		fmt.Println("filters: --harness <id>  --kind <k>  --unreachable  --corrected  --include-retired  --json")
	case "graph":
		if len(rest) > 1 || (len(rest) == 1 && rest[0] != "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound capability graph [--json]")
			os.Exit(2)
		}
		nodes, err := store.List()
		die(err)
		edges, err := store.Edges()
		die(err)
		decisionRecords, err := decisions.LoadSourceAuthority(homePath)
		die(err)
		out := struct {
			SchemaVersion string                            `json:"schema_version"`
			AuthorityRef  string                            `json:"authority_ref"`
			NodeCount     int                               `json:"node_count"`
			EdgeCount     int                               `json:"edge_count"`
			DecisionCount int                               `json:"decision_count"`
			Nodes         []capgraph.Node                   `json:"nodes"`
			Edges         []capgraph.Edge                   `json:"edges"`
			Decisions     []decisions.SourceAuthorityRecord `json:"decisions"`
		}{
			SchemaVersion: "compound.capability_graph_projection.v0.1",
			AuthorityRef:  "compound://registries/capability-graph",
			NodeCount:     len(nodes),
			EdgeCount:     len(edges),
			DecisionCount: len(decisionRecords),
			Nodes:         nodes,
			Edges:         edges,
			Decisions:     decisionRecords,
		}
		if len(rest) == 1 && rest[0] == "--json" {
			printJSON(out)
			return
		}
		fmt.Printf("# capability graph\n\n")
		fmt.Printf("- nodes: %d\n", out.NodeCount)
		fmt.Printf("- edges: %d\n", out.EdgeCount)
		fmt.Printf("- decisions: %d\n", out.DecisionCount)
	case "compact":
		jsonOut := hasFlag(rest, "--json")
		write := hasFlag(rest, "--yes") || hasFlag(rest, "--confirm")
		os.Exit(capgraph.RunCompact(store, write, jsonOut, os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

// connectionImportReport is the import-connections door result: which harness
// nodes gained connection-surface facts, minted idempotently from compound's OWN
// authority surfaces under the existing capability-graph authority.
type connectionImportReport struct {
	SchemaVersion string                    `json:"schema_version"`
	Status        string                    `json:"status"`
	AuthorityRef  string                    `json:"authority_ref"`
	Harnesses     []connectionImportHarness `json:"harnesses"`
}

type connectionImportHarness struct {
	Harness                string   `json:"harness"`
	NodeID                 string   `json:"node_id"`
	NodeMinted             bool     `json:"node_minted"`
	ConfigPaths            []string `json:"config_paths"`
	SettingsSchemaRef      string   `json:"settings_schema_ref,omitempty"`
	AssertingOwner         string   `json:"asserting_owner"`
	SourceRevision         string   `json:"source_revision"`
	SourceObservationID    string   `json:"source_observation_id"`
	SourceAuthorityID      string   `json:"source_authority_id"`
	NoPromoteIDs           []string `json:"no_promote_ids,omitempty"`
	ConnectionSurfaceAdded bool     `json:"connection_surface_added"`
}

// importCapabilityConnections self-scans COMPOUND_HOME's OWN authority surfaces
// (harness permission authorities + the installed-hooks manifest) and mints one
// ConnectionSurface per configured harness under the EXISTING capability-graph
// authority. It ensures a kind:"harness" node exists (never modifies a
// pre-existing node), runs the same no-promote lineage ceremony as
// import-topology (sourceobs + RecordSourceAuthority), and persists the sidecar
// record. Re-running with unchanged sources is a no-op.
func importCapabilityConnections(store *capgraph.Store, homePath, harnessFilter string, now time.Time) (connectionImportReport, error) {
	report := connectionImportReport{
		SchemaVersion: capgraph.ConnectionSurfaceSchemaVersion,
		Status:        "ok",
		AuthorityRef:  capgraph.CapabilityGraphAuthorityRef,
	}
	harnessFilter = strings.TrimSpace(harnessFilter)

	matches, err := filepath.Glob(filepath.Join(homePath, "config", "harness-permissions", "*.yaml"))
	if err != nil {
		return report, err
	}
	sort.Strings(matches)

	// Installed-hooks config paths per harness. This surface is optional -- an
	// absent manifest is not an error; the harness still has its authority paths.
	hooksByHarness := map[string][]string{}
	manifest, herr := contract.LoadInstalledHooksManifest(homePath)
	if herr != nil && !os.IsNotExist(herr) {
		return report, herr
	}
	if herr == nil {
		for _, hook := range manifest.Hooks {
			ref := strings.TrimSpace(hook.Ref)
			hn := strings.TrimSpace(hook.Harness)
			if ref == "" || hn == "" {
				continue
			}
			hooksByHarness[hn] = append(hooksByHarness[hn], ref)
		}
	}

	// Existing node ids: ensure a harness node, never modify one.
	nodes, err := store.List()
	if err != nil {
		return report, err
	}
	existingNode := map[string]bool{}
	for _, n := range nodes {
		existingNode[n.ID] = true
	}

	var nodeBatch []capgraph.Node
	var surfaceBatch []capgraph.ConnectionSurface
	var pending []struct {
		harnessName string
		nodeMinted  bool
		configPaths []string
		cs          capgraph.ConnectionSurface
	}

	for _, path := range matches {
		harnessName := strings.TrimSuffix(filepath.Base(path), ".yaml")
		if harnessFilter != "" && harnessName != harnessFilter {
			continue
		}
		authority, sourceRevision, err := harness.LoadPermissionAuthority(homePath, harnessName)
		if err != nil {
			return report, err
		}
		configPaths := dedupeStrings(append([]string{
			authority.TargetPath,
			harness.PermissionAuthorityPath(homePath, harnessName),
		}, hooksByHarness[harnessName]...))

		nodeMinted := false
		if !existingNode[harnessName] {
			nodeBatch = append(nodeBatch, capgraph.Node{
				ID:       harnessName,
				Kind:     "harness",
				Name:     harnessName,
				Triggers: []string{harnessName},
			})
			existingNode[harnessName] = true
			nodeMinted = true
		}

		cs := capgraph.ConnectionSurface{
			NodeID:            harnessName,
			ConfigPaths:       configPaths,
			SettingsSchemaRef: authority.SchemaVersion,
			AssertingOwner:    harness.PermissionAuthorityRef(harnessName),
			SourceRevision:    sourceRevision,
		}
		surfaceBatch = append(surfaceBatch, cs)
		pending = append(pending, struct {
			harnessName string
			nodeMinted  bool
			configPaths []string
			cs          capgraph.ConnectionSurface
		}{harnessName, nodeMinted, configPaths, cs})
	}

	if len(nodeBatch) > 0 {
		if err := capgraph.ApplyNodesAndEdgesGoverned(homePath, nodeBatch, nil, now); err != nil {
			return report, err
		}
	}

	for _, item := range pending {
		added, err := capgraph.AddConnectionSurfaceGoverned(homePath, item.cs, now)
		if err != nil {
			return report, err
		}

		obs := connectionImportObservation(item.harnessName, item.cs.ConfigPaths[0], item.cs.SourceRevision, item.configPaths, now)
		sourceReport, err := sourceobs.Apply(homePath, obs, now)
		if err != nil {
			return report, fmt.Errorf("source observe: %w", err)
		}
		auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
			SourcePath:          item.cs.ConfigPaths[0],
			SourceKind:          "harness_permission_authority",
			SourceOwner:         harness.PermissionAuthorityRef(item.harnessName),
			SourceClass:         "source",
			SourceRevision:      item.cs.SourceRevision,
			SourceObservedAt:    obs.ObservedAt,
			ImportedVia:         "capability import-connections",
			NodeIDs:             []string{item.harnessName},
			SourceObservationID: sourceReport.ObservationID,
			NoPromoteIDs:        sourceReport.NoPromoteIDs,
			EvidenceRefs:        obs.EvidenceRefs,
		}, now)
		if err != nil {
			return report, err
		}
		report.Harnesses = append(report.Harnesses, connectionImportHarness{
			Harness:                item.harnessName,
			NodeID:                 item.harnessName,
			NodeMinted:             item.nodeMinted,
			ConfigPaths:            item.configPaths,
			SettingsSchemaRef:      item.cs.SettingsSchemaRef,
			AssertingOwner:         item.cs.AssertingOwner,
			SourceRevision:         item.cs.SourceRevision,
			SourceObservationID:    sourceReport.ObservationID,
			SourceAuthorityID:      auth.ID,
			NoPromoteIDs:           sourceReport.NoPromoteIDs,
			ConnectionSurfaceAdded: added,
		})
	}
	return report, nil
}

// connectionImportObservation builds the no-promote SourceObservation for a
// self-scanned harness authority surface. Mirrors sourceobs.FromToolTopology's
// contract (the tooltopology pattern) but is constructed here because capgraph
// cannot import sourceobs (that would cycle) and the packet forbids modifying
// the sourceobs package.
func connectionImportObservation(harnessName, authorityPath, sourceRevision string, configPaths []string, now time.Time) sourceobs.SourceObservation {
	owner := harness.PermissionAuthorityRef(harnessName)
	sum := sha256.Sum256([]byte(strings.Join([]string{harnessName, authorityPath, sourceRevision}, "|")))
	return sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            "srcobs_conn_" + hex.EncodeToString(sum[:])[:12],
		AdapterID:     "compound.capability-connections",
		SourceSystem:  "compound",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: sourceobs.SourceRef{
			Path:        authorityPath,
			Kind:        "harness_permission_authority",
			Owner:       owner,
			SourceClass: "source",
			Revision:    sourceRevision,
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:    "harness_connection_surface",
			Consumer:    "compound capability import-connections",
			Generator:   "compound capability import-connections",
			Marker:      capgraph.ConnectionSurfaceSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("config_paths=%d", len(configPaths)),
		},
		MapsTo: []sourceobs.MapTarget{{
			ObjectFamily:   "capability",
			CandidateKind:  string(candidates.CapabilityObs),
			EntityHint:     harnessName,
			PromoteAllowed: false,
		}},
		EvidenceRefs: []string{"harness_connection_surface:" + harnessName + ":" + sourceRevision},
	}
}

// gatewayAssertingOwner is the fixed asserting owner for gateway connectivity
// facts imported from a switchback provider registry. The provider registry --
// not compound -- is the source that asserts these connectivity descriptors.
const gatewayAssertingOwner = "switchback://provider-registry"

// gatewayImportNode is one minted node in an import-gateway report: the gateway
// root or one provider, with its minted / connection-surface-added booleans.
type gatewayImportNode struct {
	NodeID                 string `json:"node_id"`
	Role                   string `json:"role"` // "root" | "provider"
	ProviderID             string `json:"provider_id,omitempty"`
	Name                   string `json:"name,omitempty"`
	Endpoint               string `json:"endpoint,omitempty"`
	AuthMode               string `json:"auth_mode,omitempty"`
	NodeMinted             bool   `json:"node_minted"`
	ConnectionSurfaceAdded bool   `json:"connection_surface_added"`
}

// gatewayImportReport summarizes an import-gateway run: the gateway root + one
// node per connectable provider, providers skipped for lack of an endpoint, and
// the no-promote lineage triad ids. Mirrors the connectionImportReport shape.
type gatewayImportReport struct {
	SchemaVersion       string              `json:"schema_version"`
	Status              string              `json:"status"`
	AuthorityRef        string              `json:"authority_ref"`
	GatewayName         string              `json:"gateway_name"`
	RegistryPath        string              `json:"registry_path"`
	RegistrySchema      string              `json:"registry_schema"`
	SourceRevision      string              `json:"source_revision"`
	Nodes               []gatewayImportNode `json:"nodes"`
	SkippedProviders    int                 `json:"skipped_providers"`
	SkippedProviderIDs  []string            `json:"skipped_provider_ids,omitempty"`
	SourceObservationID string              `json:"source_observation_id"`
	SourceAuthorityID   string              `json:"source_authority_id"`
	NoPromoteIDs        []string            `json:"no_promote_ids,omitempty"`
}

// importCapabilityGateway mints gateway-class capgraph nodes (one gateway root +
// one node per connectable provider) from an external switchback provider
// registry, carrying ConnectionSurface connectivity facts (endpoint=base_url,
// auth_mode=auth_scheme, schema ref, whole-file source revision) under the
// EXISTING capability-graph authority. It ensures each node without ever
// rewriting a pre-existing one, runs the same no-promote lineage ceremony as
// import-connections (sourceobs + RecordSourceAuthority), and persists the
// sidecar. It FAILS CLOSED before any persistence on an unreadable file or a
// missing/unknown schema. Facts are connectivity metadata only: no pricing,
// cost, tier, health, or routing semantics, and no secret/env values are read.
func importCapabilityGateway(store *capgraph.Store, homePath, registryPath, gatewayName string, now time.Time) (gatewayImportReport, error) {
	gatewayName = strings.TrimSpace(gatewayName)
	if gatewayName == "" {
		gatewayName = "switchback"
	}
	report := gatewayImportReport{
		SchemaVersion: capgraph.ConnectionSurfaceSchemaVersion,
		Status:        "ok",
		AuthorityRef:  capgraph.CapabilityGraphAuthorityRef,
		GatewayName:   gatewayName,
		RegistryPath:  registryPath,
	}

	// Fail closed BEFORE any write: unreadable file, non-JSON body, or missing/
	// unknown schema returns an error and nothing is persisted (no partial import).
	registry, err := capgraph.LoadProviderRegistry(registryPath)
	if err != nil {
		return report, err
	}
	report.RegistrySchema = registry.Schema
	report.SourceRevision = registry.SourceRevision

	nodes, err := store.List()
	if err != nil {
		return report, err
	}
	existingNode := map[string]bool{}
	for _, n := range nodes {
		existingNode[n.ID] = true
	}

	var nodeBatch []capgraph.Node
	var surfaceBatch []capgraph.ConnectionSurface
	var pending []struct {
		nodeID string
		name   string
		minted bool
		cs     capgraph.ConnectionSurface
	}

	// Root node: config path + schema + owner + revision, but NO endpoint/auth
	// (the registry does not carry the gateway's own listen address).
	rootID := capgraph.GatewayRootNodeID(gatewayName)
	rootName := gatewayName + " gateway"
	rootMinted := false
	if !existingNode[rootID] {
		nodeBatch = append(nodeBatch, capgraph.GatewayNode(rootID, rootName))
		existingNode[rootID] = true
		rootMinted = true
	}
	rootCS := capgraph.ConnectionSurface{
		NodeID:            rootID,
		ConfigPaths:       []string{registryPath},
		SettingsSchemaRef: registry.Schema,
		AssertingOwner:    gatewayAssertingOwner,
		SourceRevision:    registry.SourceRevision,
	}
	surfaceBatch = append(surfaceBatch, rootCS)
	pending = append(pending, struct {
		nodeID string
		name   string
		minted bool
		cs     capgraph.ConnectionSurface
	}{rootID, rootName, rootMinted, rootCS})

	// Provider nodes: endpoint=base_url. A provider with no id or no base_url has
	// no connectivity fact to record -- skip it and count it (never silent-drop).
	for _, p := range registry.Providers {
		providerID := strings.TrimSpace(p.ID)
		if providerID == "" {
			report.SkippedProviders++
			report.SkippedProviderIDs = append(report.SkippedProviderIDs, "(missing id)")
			continue
		}
		if strings.TrimSpace(p.BaseURL) == "" {
			report.SkippedProviders++
			report.SkippedProviderIDs = append(report.SkippedProviderIDs, providerID)
			continue
		}
		nodeID := capgraph.GatewayProviderNodeID(gatewayName, providerID)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = providerID
		}
		minted := false
		if !existingNode[nodeID] {
			nodeBatch = append(nodeBatch, capgraph.GatewayNode(nodeID, name))
			existingNode[nodeID] = true
			minted = true
		}
		cs := capgraph.ConnectionSurface{
			NodeID:            nodeID,
			ConfigPaths:       []string{registryPath},
			Endpoint:          strings.TrimSpace(p.BaseURL),
			AuthMode:          strings.TrimSpace(p.AuthScheme),
			SettingsSchemaRef: registry.Schema,
			AssertingOwner:    gatewayAssertingOwner,
			SourceRevision:    registry.SourceRevision,
		}
		surfaceBatch = append(surfaceBatch, cs)
		pending = append(pending, struct {
			nodeID string
			name   string
			minted bool
			cs     capgraph.ConnectionSurface
		}{nodeID, name, minted, cs})
	}

	if len(nodeBatch) > 0 {
		if err := capgraph.ApplyNodesAndEdgesGoverned(homePath, nodeBatch, nil, now); err != nil {
			return report, err
		}
	}

	var nodeIDs []string
	for _, item := range pending {
		nodeIDs = append(nodeIDs, item.nodeID)
		added, err := capgraph.AddConnectionSurfaceGoverned(homePath, item.cs, now)
		if err != nil {
			return report, err
		}
		var role, providerID string
		if item.nodeID == rootID {
			role = "root"
		} else {
			role = "provider"
			providerID = strings.TrimPrefix(item.nodeID, capgraph.GatewayProviderNodeID(gatewayName, ""))
		}
		report.Nodes = append(report.Nodes, gatewayImportNode{
			NodeID: item.nodeID, Role: role, ProviderID: providerID, Name: item.name,
			Endpoint: item.cs.Endpoint, AuthMode: item.cs.AuthMode,
			NodeMinted: item.minted, ConnectionSurfaceAdded: added,
		})
	}

	// Lineage triad: one no-promote SourceObservation for the whole registry file
	// + a capability-source-authority handoff row; claims.jsonl stays untouched.
	obs := gatewayImportObservation(gatewayName, registryPath, registry.Schema, registry.SourceRevision, len(report.Nodes), now)
	sourceReport, err := sourceobs.Apply(homePath, obs, now)
	if err != nil {
		return report, fmt.Errorf("source observe: %w", err)
	}
	auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:          registryPath,
		SourceKind:          "gateway_provider_registry",
		SourceOwner:         gatewayAssertingOwner,
		SourceClass:         "source",
		SourceRevision:      registry.SourceRevision,
		SourceObservedAt:    obs.ObservedAt,
		ImportedVia:         "capability import-gateway",
		NodeIDs:             nodeIDs,
		SourceObservationID: sourceReport.ObservationID,
		NoPromoteIDs:        sourceReport.NoPromoteIDs,
		EvidenceRefs:        obs.EvidenceRefs,
	}, now)
	if err != nil {
		return report, err
	}
	report.SourceObservationID = sourceReport.ObservationID
	report.SourceAuthorityID = auth.ID
	report.NoPromoteIDs = sourceReport.NoPromoteIDs
	return report, nil
}

// gatewayImportObservation builds the no-promote SourceObservation for an
// imported switchback provider registry. Mirrors connectionImportObservation:
// constructed here because capgraph cannot import sourceobs (that would cycle)
// and the packet forbids modifying the sourceobs package. The registry file
// stays evidence/rollback input; the graph is the surface consumers treat as
// authority.
func gatewayImportObservation(gatewayName, registryPath, schema, sourceRevision string, nodeCount int, now time.Time) sourceobs.SourceObservation {
	sum := sha256.Sum256([]byte(strings.Join([]string{gatewayName, registryPath, sourceRevision}, "|")))
	return sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            "srcobs_gw_" + hex.EncodeToString(sum[:])[:12],
		AdapterID:     "compound.capability-gateway",
		SourceSystem:  "switchback",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: sourceobs.SourceRef{
			Path:        registryPath,
			Kind:        "gateway_provider_registry",
			Owner:       gatewayAssertingOwner,
			SourceClass: "source",
			Revision:    sourceRevision,
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:    "gateway_connection_surface",
			Consumer:    "compound capability import-gateway",
			Generator:   "compound capability import-gateway",
			Marker:      capgraph.ConnectionSurfaceSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("gateway=%s schema=%s nodes=%d", gatewayName, schema, nodeCount),
		},
		MapsTo: []sourceobs.MapTarget{{
			ObjectFamily:   "capability",
			CandidateKind:  string(candidates.CapabilityObs),
			EntityHint:     gatewayName,
			PromoteAllowed: false,
		}},
		EvidenceRefs: []string{"gateway_connection_surface:" + gatewayName + ":" + sourceRevision},
	}
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func printCapabilityConstraints(c capgraph.SurfacingConstraints) {
	printCapabilityConstraintRefs("requires", c.Required)
	printCapabilityConstraintRefs("conflicts", c.Conflicts)
	printCapabilityConstraintRefs("harnesses", c.Harnesses)
	printCapabilityConstraintRefs("connection_surfaces", c.ConnectionSurfaces)
	printCapabilityConstraintRefs("missed_when", c.KnownMissedWhen)
	printCapabilityConstraintRefs("false_triggered_by", c.KnownFalseTriggeredBy)
	printCapabilityConstraintRefs("evidence", c.Evidence)
	if c.CostRiskNote != "" {
		fmt.Printf("    note: %s\n", c.CostRiskNote)
	}
}

func printCapabilityConstraintRefs(label string, values []capgraph.SurfacingConstraint) {
	if len(values) == 0 {
		return
	}
	refs := make([]string, 0, len(values))
	for _, value := range values {
		ref := value.Ref
		if value.Detail != "" {
			ref += " (" + value.Detail + ")"
		}
		refs = append(refs, ref)
	}
	fmt.Printf("    %s: %s\n", label, strings.Join(refs, ", "))
}

func formatEdgeKindCounts(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, counts[kind]))
	}
	return strings.Join(parts, " ")
}
