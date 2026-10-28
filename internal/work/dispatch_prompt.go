package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
)

const (
	maxDispatchPromptOffers          = 5
	dispatchWorkerPromptContractRef  = "contract://dispatch-worker-prompt"
	dispatchWorkerPromptContractNote = "dispatch worker prompt is derived from an already-admitted WorkEnvelope plus task-scoped CapabilityGraph offers; no new EffectiveContract is compiled for this capability-only prompt"
)

type dispatchPrompt struct {
	ContextPackID       string
	CapabilitiesOffered []DispatchCapabilityOffer
	Markdown            string
}

func (s *Store) attachDispatchPrompt(workID, actor string, now time.Time) (dispatchPrompt, error) {
	for {
		w, err := s.Get(workID)
		if err != nil {
			return dispatchPrompt{}, err
		}
		if gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery) {
			return dispatchPrompt{}, repositoryIdentityMigrationError(w)
		}
		workSHA256, err := workEnvelopeSHA256(w)
		if err != nil {
			return dispatchPrompt{}, err
		}
		pack, err := s.dispatchContextPack(w, actor, now)
		if err != nil {
			return dispatchPrompt{}, err
		}
		if err := pack.Validate(); err != nil {
			return dispatchPrompt{}, fmt.Errorf("dispatch context pack invalid: %w", err)
		}

		var committed envelope.WorkEnvelope
		retry := false
		err = s.withLifecycleMutation(func() error {
			current, err := s.recoverPendingClaimBeforeMutation(workID)
			if err != nil {
				return err
			}
			currentSHA256, err := workEnvelopeSHA256(current)
			if err != nil {
				return err
			}
			if currentSHA256 != workSHA256 {
				retry = true
				return nil
			}

			packPath := filepath.Join(s.home, "context", "packs", pack.ID+".json")
			if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
				return err
			}
			if err := s.writeJSON(packPath, &pack); err != nil {
				return err
			}
			current.IDs.ContextPackID = pack.ID
			if err := s.writeJSON(s.workPath(current.ID), &current); err != nil {
				return err
			}
			committed = current
			return nil
		})
		if err != nil {
			return dispatchPrompt{}, err
		}
		if retry {
			continue
		}

		offers := dispatchPromptOffers(pack)
		return dispatchPrompt{
			ContextPackID:       pack.ID,
			CapabilitiesOffered: offers,
			Markdown:            renderDispatchPrompt(committed, pack.TaskContract, dispatchWorkspace(committed, ""), pack.RequiredChecks, offers),
		}, nil
	}
}

func (s *Store) dispatchContextPack(w envelope.WorkEnvelope, actor string, now time.Time) (envelope.ContextPack, error) {
	taskContract, contractID, err := s.dispatchTaskContract(w)
	if err != nil {
		return envelope.ContextPack{}, err
	}
	task := strings.TrimSpace(strings.Join(compactStrings(
		w.CanonicalSurface,
		taskContract.Intent,
		taskContract.DesiredOutcome,
		taskContract.SuccessPredicate,
		strings.Join(taskContract.NonGoals, " "),
		taskContract.Mode,
		taskContract.ProofMedium,
	), " "))
	cwd, err := os.Getwd()
	if err != nil || strings.TrimSpace(cwd) == "" {
		cwd = s.home
	}
	pack := envelope.ContextPack{
		Header:             newHeader(envelope.SVContextPack, idFor("ctx_", "dispatch", w.ID, actor, task), now, w.ID, "", w.Privacy, "tier0"),
		SummaryMD:          fmt.Sprintf("Dispatch context for %s. Capability offers are task-scoped hints, not execution.", w.ID),
		TaskContract:       taskContract,
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{{Ref: dispatchWorkerPromptContractRef, Reason: dispatchWorkerPromptContractNote}},
		CapabilityOffers:   []envelope.CapabilityOffer{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{},
		SourceRevisions:    []envelope.SourceRev{{Source: "work://" + w.ID, Rev: dispatchWorkRevision(w)}},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	pack.IDs.ContractID = contractID
	pack.Scope.Surface = cwd
	attachDispatchRecipe(&pack, dispatchWorkspace(w, cwd), s.home)
	if !capgraph.Exists(s.home) {
		return pack, nil
	}
	graph, err := capgraph.Open(s.home)
	if err != nil {
		return envelope.ContextPack{}, err
	}
	surfaced, err := graph.SurfaceForHarness(task, orDefault(actor, "codex"))
	if err != nil {
		return envelope.ContextPack{}, err
	}
	if len(surfaced) > 12 {
		surfaced = surfaced[:12]
	}
	for _, hit := range surfaced {
		ref := "capability://" + hit.Node.ID
		reason := dispatchSurfaceReason(hit)
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref:         ref,
			SourceClass: "runtime",
			Reason:      reason,
			Confidence:  1,
			Status:      "current",
		})
		pack.CapabilityOffers = append(pack.CapabilityOffers, envelope.CapabilityOffer{
			Ref:   ref,
			Kind:  hit.Node.Kind,
			Name:  hit.Node.Name,
			Score: hit.Score,
			Why:   reason,
		})
	}
	if len(surfaced) > 0 {
		rev, err := capgraph.AuthorityRevision(s.home)
		if err != nil {
			return envelope.ContextPack{}, err
		}
		pack.SourceRevisions = append(pack.SourceRevisions, envelope.SourceRev{Source: capgraph.CapabilityGraphAuthorityRef, Rev: rev})
	}
	return pack, nil
}

func (s *Store) dispatchTaskContract(w envelope.WorkEnvelope) (envelope.TaskContract, string, error) {
	fallback := envelope.TaskContract{
		Intent:           strings.TrimSpace(firstNonEmpty(w.DesiredOutcome, w.SuccessPredicate)),
		Mode:             "execute",
		DesiredOutcome:   strings.TrimSpace(w.DesiredOutcome),
		SuccessPredicate: strings.TrimSpace(w.SuccessPredicate),
		NonGoals:         compactStrings(w.NonGoals...),
		ProofMedium:      strings.TrimSpace(w.ProofMedium),
	}
	contextPackID := strings.TrimSpace(w.IDs.ContextPackID)
	if contextPackID == "" {
		return fallback, "", nil
	}
	var previous envelope.ContextPack
	path := filepath.Join(s.home, "context", "packs", contextPackID+".json")
	if err := s.readJSON(path, &previous); err != nil {
		return envelope.TaskContract{}, "", fmt.Errorf("read originating context pack %q: %w", contextPackID, err)
	}
	if previous.ID != contextPackID {
		return envelope.TaskContract{}, "", fmt.Errorf("originating context pack id %q does not match artifact id %q", contextPackID, previous.ID)
	}
	if err := previous.Validate(); err != nil {
		return envelope.TaskContract{}, "", fmt.Errorf("originating context pack %q invalid: %w", contextPackID, err)
	}
	if previous.TaskContract.IsZero() {
		return fallback, previous.IDs.ContractID, nil
	}
	return previous.TaskContract, previous.IDs.ContractID, nil
}

func dispatchWorkRevision(w envelope.WorkEnvelope) string {
	parts := []string{"state:" + w.State}
	if w.GitDelivery != nil {
		parts = append(parts,
			"target_root:"+w.GitDelivery.TargetWorktreeRoot,
			"repository_fingerprint:"+w.GitDelivery.RepositoryFingerprint,
			"target_ref:"+w.GitDelivery.TargetLocalRef,
			"expected_base:"+w.GitDelivery.ExpectedBaseSHA,
			"publication_mode:"+w.GitDelivery.PublicationMode,
			"publication_ref:"+w.GitDelivery.PublicationRef,
		)
	}
	if w.PolicyDecisionID != "" {
		parts = append(parts, "policy:"+w.PolicyDecisionID)
	}
	if w.IDs.ContextPackID != "" {
		parts = append(parts, "context:"+w.IDs.ContextPackID)
	}
	return strings.Join(parts, ";")
}

func dispatchSurfaceReason(hit capgraph.Surfaced) string {
	reason := fmt.Sprintf("capability_graph surface score=%.1f kind=%s why=%s", hit.Score, hit.Node.Kind, hit.Why)
	if len(hit.MatchedTokens) > 0 {
		reason += " matched=" + strings.Join(hit.MatchedTokens, ",")
	}
	return reason
}

func dispatchPromptOffers(pack envelope.ContextPack) []DispatchCapabilityOffer {
	out := []DispatchCapabilityOffer{}
	for _, offer := range pack.CapabilityOffers {
		if offer.Kind != "skill" {
			continue
		}
		out = append(out, dispatchCapabilityOffer(offer))
		if len(out) >= maxDispatchPromptOffers {
			return out
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, offer := range pack.CapabilityOffers {
		out = append(out, dispatchCapabilityOffer(offer))
		if len(out) >= maxDispatchPromptOffers {
			break
		}
	}
	return out
}

func dispatchCapabilityOffer(offer envelope.CapabilityOffer) DispatchCapabilityOffer {
	return DispatchCapabilityOffer{
		Ref:   offer.Ref,
		Kind:  offer.Kind,
		Name:  offer.Name,
		Score: offer.Score,
		Why:   offer.Why,
	}
}

func renderDispatchPrompt(w envelope.WorkEnvelope, task envelope.TaskContract, workspace string, checks []string, offers []DispatchCapabilityOffer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Dispatch Work %s\n\n", w.ID)
	if strings.TrimSpace(w.CanonicalSurface) != "" {
		fmt.Fprintf(&b, "- surface: %s\n", w.CanonicalSurface)
	}
	fmt.Fprintln(&b)
	if !task.IsZero() {
		fmt.Fprintln(&b, "## TASK CONTRACT")
		fmt.Fprintln(&b)
		if task.Intent != "" {
			fmt.Fprintf(&b, "- intent: %s\n", task.Intent)
		}
		if task.Mode != "" {
			fmt.Fprintf(&b, "- mode: %s\n", task.Mode)
		}
		if task.ProofMedium != "" {
			fmt.Fprintf(&b, "- proof_medium: %s\n", task.ProofMedium)
		}
		fmt.Fprintln(&b)
	}
	if len(checks) > 0 {
		fmt.Fprintln(&b, dispatchRecipeHeading)
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- workspace: %s\n", workspace)
		for _, check := range checks {
			fmt.Fprintf(&b, "- %s\n", check)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "## LOAD THESE SKILLS")
	fmt.Fprintln(&b)
	if len(offers) == 0 {
		fmt.Fprintln(&b, "- none surfaced")
	} else {
		for _, offer := range offers {
			fmt.Fprintf(&b, "- %s (%s): %s\n", offer.Ref, offer.Name, firstNonEmpty(offer.Why, "surfaced by capability graph"))
		}
	}
	if w.GitDelivery != nil {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## SOURCE CONTROL CONTRACT")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- target_worktree_root: %s\n", w.GitDelivery.TargetWorktreeRoot)
		fmt.Fprintf(&b, "- repository_fingerprint: %s\n", w.GitDelivery.RepositoryFingerprint)
		fmt.Fprintf(&b, "- target_local_ref: %s\n", w.GitDelivery.TargetLocalRef)
		fmt.Fprintf(&b, "- expected_base_sha: %s\n", w.GitDelivery.ExpectedBaseSHA)
		fmt.Fprintf(&b, "- publication_mode: %s\n", w.GitDelivery.PublicationMode)
		if w.GitDelivery.PublicationRef != "" {
			fmt.Fprintf(&b, "- publication_ref: %s\n", w.GitDelivery.PublicationRef)
		}
		fmt.Fprintln(&b, "- Verify the worktree contains this base before editing; if the target moved, rebase and rerun the full proof battery.")
		fmt.Fprintln(&b, "- Attach the final verified candidate to this WorkEnvelope as commit:<full-sha>.")
		fmt.Fprintf(&b, "- Certify only through: compound work land-check %s --tip <full-sha>; the command reads repository, target, and publication authority from this WorkEnvelope.\n", w.ID)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## TASK")
	fmt.Fprintln(&b)
	if desiredOutcome := strings.TrimSpace(firstNonEmpty(task.DesiredOutcome, w.DesiredOutcome)); desiredOutcome != "" {
		fmt.Fprintln(&b, desiredOutcome)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## SUCCESS")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, firstNonEmpty(task.SuccessPredicate, w.SuccessPredicate))
	nonGoals := task.NonGoals
	if len(nonGoals) == 0 {
		nonGoals = w.NonGoals
	}
	if len(nonGoals) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## NON-GOALS")
		fmt.Fprintln(&b)
		for _, item := range nonGoals {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return strings.TrimSpace(b.String())
}
