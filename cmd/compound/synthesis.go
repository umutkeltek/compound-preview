package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const (
	synthesisSweepArea         = "synthesis-sweeps"
	synthesisContradictionArea = "synthesis-contradictions"
	seedCorroborationArea      = "seed-corroborations"
)

type synthesisSweepOutput struct {
	SchemaVersion                string                   `json:"schema_version"`
	WorkID                       string                   `json:"work_id"`
	ReceiptID                    string                   `json:"receipt_id"`
	TransitionID                 string                   `json:"transition_id"`
	ReportPath                   string                   `json:"report_path"`
	ContradictionReviewPaths     []string                 `json:"contradiction_review_paths,omitempty"`
	SeedCorroborationReviewPaths []string                 `json:"seed_corroboration_review_paths,omitempty"`
	Report                       mem.SynthesisSweepReport `json:"report"`
}

type synthesisSweepArtifact struct {
	SchemaVersion string                   `json:"schema_version"`
	ID            string                   `json:"id"`
	CreatedAt     string                   `json:"created_at"`
	Status        string                   `json:"status"`
	Readiness     string                   `json:"readiness"`
	WorkID        string                   `json:"work_id"`
	Owner         string                   `json:"owner,omitempty"`
	SourceKind    string                   `json:"source_kind,omitempty"`
	NextAction    string                   `json:"next_action,omitempty"`
	NotToDo       string                   `json:"not_to_do,omitempty"`
	Boundaries    []string                 `json:"boundaries,omitempty"`
	EvidenceRefs  []string                 `json:"evidence_refs,omitempty"`
	Report        mem.SynthesisSweepReport `json:"report"`
}

type synthesisContradictionArtifact struct {
	SchemaVersion string                             `json:"schema_version"`
	ID            string                             `json:"id"`
	CreatedAt     string                             `json:"created_at"`
	Status        string                             `json:"status"`
	Readiness     string                             `json:"readiness"`
	WorkID        string                             `json:"work_id"`
	Owner         string                             `json:"owner,omitempty"`
	SourceKind    string                             `json:"source_kind,omitempty"`
	NextAction    string                             `json:"next_action,omitempty"`
	NotToDo       string                             `json:"not_to_do,omitempty"`
	Boundaries    []string                           `json:"boundaries,omitempty"`
	EvidenceRefs  []string                           `json:"evidence_refs,omitempty"`
	Proposal      mem.SynthesisContradictionProposal `json:"proposal"`
}

type synthesisSeedCorroborationArtifact struct {
	SchemaVersion string                             `json:"schema_version"`
	ID            string                             `json:"id"`
	CreatedAt     string                             `json:"created_at"`
	Status        string                             `json:"status"`
	Readiness     string                             `json:"readiness"`
	WorkID        string                             `json:"work_id"`
	Owner         string                             `json:"owner,omitempty"`
	SourceKind    string                             `json:"source_kind,omitempty"`
	CandidateID   string                             `json:"candidate_id,omitempty"`
	NextAction    string                             `json:"next_action,omitempty"`
	NotToDo       string                             `json:"not_to_do,omitempty"`
	Boundaries    []string                           `json:"boundaries,omitempty"`
	EvidenceRefs  []string                           `json:"evidence_refs,omitempty"`
	Proposal      mem.SynthesisCorroborationProposal `json:"proposal"`
}

const synthesisUsage = "usage: compound synthesis sweep [--stage cluster,contradict,corroborate,decay,reconcile] [--json]\n" +
	"       compound synthesis digest [--since <ts>] [--json]"

func cmdSynthesis(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, synthesisUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "sweep":
		cmdSynthesisSweep(args[1:])
	case "digest":
		cmdSynthesisDigest(args[1:])
	default:
		fmt.Fprintln(os.Stderr, synthesisUsage)
		os.Exit(2)
	}
}

func cmdSynthesisSweep(args []string) {
	var stages []string
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--stage":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --stage requires value")
				os.Exit(2)
			}
			i++
			stages = append(stages, args[i])
		default:
			fmt.Fprintln(os.Stderr, "unknown synthesis sweep arg:", args[i])
			os.Exit(2)
		}
	}
	out, err := runSynthesisSweep(stages, time.Now())
	die(err)
	if jsonOut {
		printJSON(out)
		return
	}
	fmt.Printf("synthesis sweep work=%s receipt=%s transition=%s clusters=%d echo_fold=%d contradictions=%d decayed=%d volatile_extended=%d volatile_cooled=%d volatile_carded=%d report=%s\n",
		out.WorkID, out.ReceiptID, out.TransitionID, out.Report.ClusterCount, out.Report.EchoFoldProposalCount, out.Report.ContradictionProposalCount, out.Report.DecayedCount, out.Report.VolatileExtendedCount, out.Report.VolatileCooledCount, out.Report.VolatileCardedCount, out.ReportPath)
}

// cmdSynthesisDigest renders the blast-tiered reconcile digest: the operator's read receipts
// page for everything reconcile already did autonomously (parent packet decisions-down §2).
// Strictly read-only — unlike sweep it never opens a WorkEnvelope, writes a checkpoint, or
// appends to the ledger; it only folds existing claims/positions/checkpoint state for display.
func cmdSynthesisDigest(args []string) {
	var since string
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --since requires value")
				os.Exit(2)
			}
			i++
			since = args[i]
		default:
			fmt.Fprintln(os.Stderr, "unknown synthesis digest arg:", args[i])
			os.Exit(2)
		}
	}
	out, err := runSynthesisDigest(since, time.Now())
	die(err)
	if jsonOut {
		printJSON(out)
		return
	}
	fmt.Print(out.RenderText())
}

func runSynthesisDigest(since string, now time.Time) (mem.SynthesisReconcileDigest, error) {
	homePath, err := home.Resolve("")
	if err != nil {
		return mem.SynthesisReconcileDigest{}, err
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		return mem.SynthesisReconcileDigest{}, err
	}
	var opts mem.SynthesisReconcileDigestOptions
	if since = strings.TrimSpace(since); since != "" {
		parsed, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return mem.SynthesisReconcileDigest{}, fmt.Errorf("invalid --since value %q: %w", since, err)
		}
		opts.Since = parsed
	}
	return memStore.SynthesisReconcileDigest(now, opts)
}

func runSynthesisSweep(stages []string, now time.Time) (synthesisSweepOutput, error) {
	var out synthesisSweepOutput
	homePath, err := home.Resolve("")
	if err != nil {
		return out, err
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		return out, err
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		return out, err
	}
	created, err := workStore.Create(work.CreateInput{
		Owner:            "compound synthesis",
		Actor:            "compound synthesis",
		State:            "ready",
		DesiredOutcome:   "memory synthesis sweep report with claim-review debt markers",
		SuccessPredicate: "synthesis sweep emits report artifacts; contradiction stage may append needs_review markers without candidate adjudication or claim promotion; the volatile stage alone may append clocked claim versions (extension/cool via supersession, receipt + one-line undo)",
		EvidenceRefs:     []string{"compound://synthesis/sweep"},
	}, now)
	if err != nil {
		return out, err
	}
	if _, err := workStore.Claim(created.ID, "compound synthesis", now); err != nil {
		return out, err
	}
	if _, err := workStore.Advance(created.ID, work.AdvanceInput{To: "running", Actor: "compound synthesis", Note: "running memory synthesis sweep"}, now); err != nil {
		return out, err
	}
	report, err := memStore.SynthesisSweep(stages, now)
	if err != nil {
		return out, err
	}
	reportID := reviewartifact.ID("synthesis_sweep_", created.ID, strings.Join(report.Stages, ","), report.GeneratedAt)
	reportArtifact := synthesisSweepArtifact{
		SchemaVersion: "compound.synthesis_sweep_artifact.v0.1",
		ID:            reportID,
		CreatedAt:     reviewartifact.Timestamp(now),
		Status:        "recorded",
		Readiness:     "review_recorded",
		WorkID:        created.ID,
		Owner:         "Memory synthesis",
		SourceKind:    "memory-synthesis-sweep",
		NextAction:    synthesisSweepNextAction,
		NotToDo:       synthesisNotToDo,
		Boundaries:    synthesisReviewBoundaries(),
		EvidenceRefs:  []string{"compound://synthesis/sweep", created.ID},
		Report:        report,
	}
	reportPaths, err := reviewartifact.Write(homePath, synthesisSweepArea, reportID, reportArtifact, " ", renderSynthesisSweepMarkdown(reportArtifact))
	if err != nil {
		return out, err
	}
	var contradictionPaths []string
	for _, proposal := range report.ContradictionProposals {
		reviewID := reviewartifact.ID("synthesis_contradiction_", created.ID, proposal.ID)
		artifact := synthesisContradictionArtifact{
			SchemaVersion: "compound.synthesis_contradiction_review.v0.1",
			ID:            reviewID,
			CreatedAt:     reviewartifact.Timestamp(now),
			Status:        "recorded",
			Readiness:     "review_recorded",
			WorkID:        created.ID,
			Owner:         "Memory synthesis",
			SourceKind:    "memory-synthesis-contradiction",
			NextAction:    synthesisContradictionNextAction,
			NotToDo:       synthesisNotToDo,
			Boundaries:    synthesisReviewBoundaries(),
			EvidenceRefs:  synthesisContradictionEvidenceRefs(created.ID, proposal),
			Proposal:      proposal,
		}
		paths, err := reviewartifact.Write(homePath, synthesisContradictionArea, reviewID, artifact, " ", renderSynthesisContradictionMarkdown(artifact))
		if err != nil {
			return out, err
		}
		contradictionPaths = append(contradictionPaths, paths.PlanPath)
	}
	var seedCorroborationPaths []string
	for _, proposal := range report.CorroborationProposals {
		reviewID := reviewartifact.ID("seed_corroboration_", created.ID, proposal.ID)
		artifact := synthesisSeedCorroborationArtifact{
			SchemaVersion: "compound.seed_corroboration_review.v0.1",
			ID:            reviewID,
			CreatedAt:     reviewartifact.Timestamp(now),
			Status:        "review_required",
			Readiness:     "needs_review",
			WorkID:        created.ID,
			Owner:         "Memory synthesis",
			SourceKind:    "memory-seed-corroboration",
			CandidateID:   proposal.SeedCandidateID,
			NextAction:    seedCorroborationNextAction,
			NotToDo:       seedCorroborationNotToDo,
			Boundaries:    seedCorroborationBoundaries(),
			EvidenceRefs:  seedCorroborationEvidenceRefs(created.ID, proposal),
			Proposal:      proposal,
		}
		paths, err := reviewartifact.Write(homePath, seedCorroborationArea, reviewID, artifact, " ", renderSeedCorroborationMarkdown(artifact))
		if err != nil {
			return out, err
		}
		seedCorroborationPaths = append(seedCorroborationPaths, paths.PlanPath)
	}
	if _, err := workStore.Advance(created.ID, work.AdvanceInput{
		To:             "verifying",
		Actor:          "compound synthesis",
		ArtifactID:     reportID,
		VerificationID: "synthesis_sweep_report",
		Note:           "synthesis sweep report written",
	}, now); err != nil {
		return out, err
	}
	moveType := "audit_only"
	changed := false
	summary := "memory synthesis sweep completed without claim-review mutation"
	memoryRefs := []string(nil)
	if report.ReviewDebtCount > 0 {
		moveType = "memory_delta"
		changed = true
		summary = fmt.Sprintf("memory synthesis sweep raised %d item(s) for review", report.ReviewDebtCount)
		memoryRefs = append(reopenedClaimRefs(report.ReopenedClaims), raisedSeedRefs(report.RaisedSeeds)...)
	}
	closed, err := workStore.Close(created.ID, work.CloseInput{
		Outcome:          "completed",
		MoveType:         moveType,
		Actor:            "compound synthesis",
		Summary:          summary,
		Changed:          changed,
		ProofRefs:        []string{reportID, "synthesis_sweep_report"},
		MemoryCandidates: memoryRefs,
	}, now)
	if err != nil {
		return out, err
	}
	return synthesisSweepOutput{
		SchemaVersion:                "compound.synthesis_sweep_run.v0.1",
		WorkID:                       created.ID,
		ReceiptID:                    closed.Receipt.ID,
		TransitionID:                 closed.Transition.ID,
		ReportPath:                   reportPaths.PlanPath,
		ContradictionReviewPaths:     contradictionPaths,
		SeedCorroborationReviewPaths: seedCorroborationPaths,
		Report:                       report,
	}, nil
}

func renderSynthesisSweepMarkdown(a synthesisSweepArtifact) string {
	return fmt.Sprintf("# Synthesis sweep\n\n- work: `%s`\n- stages: `%s`\n- clusters: %d\n- echo_fold_proposals: %d\n- contradiction_proposals: %d\n- review_debt: %d\n",
		a.WorkID, strings.Join(a.Report.Stages, ","), a.Report.ClusterCount, a.Report.EchoFoldProposalCount, a.Report.ContradictionProposalCount, a.Report.ReviewDebtCount)
}

func renderSynthesisContradictionMarkdown(a synthesisContradictionArtifact) string {
	return fmt.Sprintf("# Synthesis contradiction proposal\n\n- work: `%s`\n- cluster_key: `%s`\n- claims: `%s`\n- objects: `%s`\n",
		a.WorkID, a.Proposal.ClusterKey, strings.Join(a.Proposal.ClaimIDs, ","), strings.Join(a.Proposal.Objects, ","))
}

const (
	synthesisSweepNextAction         = "synthesis sweep recorded; inspect proposals and create a focused WorkEnvelope only for one evidence-backed repair"
	synthesisContradictionNextAction = "review the claim group; create a focused memory or decision repair packet only if current evidence proves a supersession"
	synthesisNotToDo                 = "do not bulk-apply, auto-adjudicate, promote, reject, or supersede memory from synthesis proposals alone"
)

func synthesisReviewBoundaries() []string {
	return []string{
		"cluster proposals are report-only",
		"contradiction stage may append needs_review claim markers",
		"no candidate adjudication",
		"no claim promotion",
		"no claim supersession",
	}
}

func synthesisContradictionEvidenceRefs(workID string, proposal mem.SynthesisContradictionProposal) []string {
	refs := []string{"compound://synthesis/sweep", workID, proposal.ID}
	for _, id := range proposal.ClaimIDs {
		refs = append(refs, "claim:"+id)
	}
	for _, id := range proposal.CandidateIDs {
		refs = append(refs, "candidate:"+id)
	}
	return reviewartifact.Unique(refs)
}

func reopenedClaimRefs(reopened []mem.SynthesisClaimReview) []string {
	refs := make([]string, 0, len(reopened))
	for _, item := range reopened {
		refs = append(refs, "claim_review:"+item.ClaimID)
	}
	return reviewartifact.Unique(refs)
}

func renderSeedCorroborationMarkdown(a synthesisSeedCorroborationArtifact) string {
	return fmt.Sprintf("# Seed corroboration\n\n%s\n", seedCorroborationClaim(a.Proposal))
}

const (
	seedCorroborationNextAction = "review corroborated seed; promote only through normal memory adjudication if the cited later outcome is a sufficient basis"
	seedCorroborationNotToDo    = "do not auto-promote, bulk-promote, or make seed claims ambient from corroboration alone"
)

func seedCorroborationEvidenceRefs(workID string, proposal mem.SynthesisCorroborationProposal) []string {
	refs := []string{"compound://synthesis/sweep", workID, proposal.ID, "candidate:" + proposal.SeedCandidateID}
	refs = append(refs, proposal.EvidenceRefs...)
	return reviewartifact.Unique(refs)
}

func raisedSeedRefs(raised []mem.SynthesisSeedReview) []string {
	refs := make([]string, 0, len(raised))
	for _, item := range raised {
		refs = append(refs, "candidate_review:"+item.CandidateID)
	}
	return reviewartifact.Unique(refs)
}

func seedCorroborationBoundaries() []string {
	return reviewartifact.Unique(append(synthesisReviewBoundaries(),
		"seed corroboration raises review debt only",
		"no automatic memory promotion",
		"promoted stance seeds remain contextual unless an operator changes visibility",
	))
}

func seedCorroborationClaim(proposal mem.SynthesisCorroborationProposal) string {
	return fmt.Sprintf("You said %s on %s. It just turned out to matter, because %s happened. Promote it?",
		proposal.SeedText, shortReviewDate(proposal.SeedCreatedAt), proposal.EvidenceSummary)
}

func shortReviewDate(raw string) string {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if i := strings.Index(raw, "T"); i > 0 {
		return raw[:i]
	}
	return raw
}

func loadSeedCorroborationArtifact(path string) (synthesisSeedCorroborationArtifact, error) {
	var artifact synthesisSeedCorroborationArtifact
	data, err := os.ReadFile(path)
	if err != nil {
		return artifact, err
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		return artifact, err
	}
	return artifact, nil
}

func annotateSynthesisReviewQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		switch item.Area {
		case synthesisSweepArea:
			// Capture the pre-annotation band: annotateSynthesisQueueItem
			// forces Status to "recorded" for writer-debt (empty/unknown)
			// artifacts, and SetOperatorCard would otherwise recompute the
			// band from that new status, losing the needs_you/filed split
			// the original on-disk status already carried.
			band := item.Band
			annotateSynthesisQueueItem(item, "memory-synthesis-sweep", synthesisSweepNextAction)
			annotateSynthesisSweepOperatorCard(item, band)
		case synthesisContradictionArea:
			band := item.Band
			annotateSynthesisQueueItem(item, "memory-synthesis-contradiction", synthesisContradictionNextAction)
			annotateSynthesisContradictionOperatorCard(item, band)
		case seedCorroborationArea:
			annotateSeedCorroborationQueueItem(item)
		}
	}
}

func annotateSynthesisSweepOperatorCard(item *reviewartifact.QueueItem, band string) {
	if item.JSONPath == "" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return
	}
	var artifact synthesisSweepArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return
	}
	artifact.WorkID = reviewartifact.FirstNonEmpty(artifact.WorkID, item.ID)
	card, err := operatorCardForSynthesisSweep(artifact)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "synthesis sweep operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "synthesis sweep operator card invalid: "+err.Error())
	}
}

func annotateSynthesisContradictionOperatorCard(item *reviewartifact.QueueItem, band string) {
	if item.JSONPath == "" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return
	}
	var artifact synthesisContradictionArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return
	}
	artifact.WorkID = reviewartifact.FirstNonEmpty(artifact.WorkID, item.ID)
	card, err := operatorCardForSynthesisContradiction(artifact)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "synthesis contradiction operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "synthesis contradiction operator card invalid: "+err.Error())
	}
}

func init() {
	reviewartifact.RegisterGroupSummary(synthesisContradictionArea, func(group reviewartifact.QueueGroup) reviewartifact.GroupSummaryResult {
		return reviewartifact.GroupSummaryResult{
			Summary:                fmt.Sprintf("Memory found %d pairs of claims that disagree with each other. Settle them one by one?", group.Count),
			Unlock:                 "memory stops carrying both versions",
			BatchUnavailableReason: "each contradiction needs its own read",
		}
	})
}

func annotateSeedCorroborationQueueItem(item *reviewartifact.QueueItem) {
	item.Status = "review_required"
	item.Owner = reviewartifact.FirstNonEmpty(item.Owner, "Memory synthesis")
	item.SourceKind = reviewartifact.FirstNonEmpty(item.SourceKind, "memory-seed-corroboration")
	item.Readiness = "needs_review"
	item.NextAction = reviewartifact.FirstNonEmpty(item.NextAction, seedCorroborationNextAction)
	item.NotToDo = reviewartifact.FirstNonEmpty(item.NotToDo, seedCorroborationNotToDo)
	item.Boundaries = reviewartifact.Unique(append(item.Boundaries, seedCorroborationBoundaries()...))
	artifact, err := loadSeedCorroborationArtifact(item.JSONPath)
	if err != nil {
		item.BlockedReason = "seed corroboration artifact unreadable: " + err.Error()
		return
	}
	item.CandidateID = reviewartifact.FirstNonEmpty(item.CandidateID, artifact.CandidateID, artifact.Proposal.SeedCandidateID)
	item.EvidenceRefs = reviewartifact.Unique(append(item.EvidenceRefs, artifact.EvidenceRefs...))
	card := seedCorroborationDecisionCard(artifact)
	item.DecisionCard = card
	item.Actions = seedCorroborationActions(item.CandidateID, card)
}

func seedCorroborationDecisionCard(artifact synthesisSeedCorroborationArtifact) *reviewartifact.DecisionCard {
	command := fmt.Sprintf("compound memory adjudicate %q --verdict promote", artifact.Proposal.SeedCandidateID)
	return &reviewartifact.DecisionCard{
		Claim: seedCorroborationClaim(artifact.Proposal),
		Why: []reviewartifact.DecisionCardReason{
			{Label: "later-outcome", Detail: artifact.Proposal.EvidenceSummary},
			{Label: "evidence", Detail: strings.Join(artifact.Proposal.EvidenceRefs, ",")},
		},
		Choices: []reviewartifact.DecisionCardChoice{{
			ID:             "promote_seed_candidate",
			Label:          "Promote",
			Effect:         "Runs the normal memory adjudication gate; corroboration supplies runtime-proof basis but does not auto-promote.",
			Command:        command,
			Argv:           []string{"compound", "memory", "adjudicate", artifact.Proposal.SeedCandidateID, "--verdict", "promote"},
			RequiresReview: true,
		}},
		CanonicalActionHint: command,
		Boundary:            seedCorroborationBoundaries(),
		Meta: map[string]string{
			"candidate_id":      artifact.Proposal.SeedCandidateID,
			"review_ref":        artifact.Proposal.ID,
			"evidence_claim_id": artifact.Proposal.EvidenceClaimID,
		},
	}
}

func seedCorroborationActions(candidateID string, card *reviewartifact.DecisionCard) []reviewartifact.Action {
	if strings.TrimSpace(candidateID) == "" || card == nil || len(card.Choices) == 0 {
		return nil
	}
	choice := card.Choices[0]
	return []reviewartifact.Action{{
		ID:             choice.ID,
		Label:          choice.Label,
		Command:        choice.Command,
		Argv:           append([]string(nil), choice.Argv...),
		RequiresReview: true,
	}}
}

func annotateSynthesisQueueItem(item *reviewartifact.QueueItem, sourceKind, nextAction string) {
	if item.Status == "" || item.Status == "unknown" {
		item.Status = "recorded"
	}
	item.Owner = reviewartifact.FirstNonEmpty(item.Owner, "Memory synthesis")
	item.SourceKind = reviewartifact.FirstNonEmpty(item.SourceKind, sourceKind)
	item.Readiness = reviewartifact.FirstNonEmpty(item.Readiness, "review_recorded")
	item.NextAction = reviewartifact.FirstNonEmpty(item.NextAction, nextAction)
	item.NotToDo = reviewartifact.FirstNonEmpty(item.NotToDo, synthesisNotToDo)
	item.Boundaries = reviewartifact.Unique(append(item.Boundaries, synthesisReviewBoundaries()...))
	item.Actions = nil
}
