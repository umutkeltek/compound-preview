package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"compound/internal/envelope"
	"compound/internal/mem"
)

const (
	probeContractsSchemaVersion = "compound.probe_contracts.v0.1"
	probeContractsAuthorityRef  = "compound://registries/probe-contracts"
	// A group base manufactures eligibility inside an explicitly probed subtree, not authority.
	// It stays below the existing +2 exact-entity term so an exact classifier seed wins a tie
	// without letting a probe bypass any ledger or MemoryRule gate.
	probeGroupBaseRank = 1.0
)

type probeContractRegistry struct {
	SchemaVersion string                   `json:"schema_version"`
	DefaultClass  string                   `json:"default_class"`
	TaskClasses   []probeContractTaskClass `json:"task_classes"`
}

type probeContractTaskClass struct {
	Class  string             `json:"class"`
	Match  probeContractMatch `json:"match"`
	Probes []probeContract    `json:"probes"`
}

type probeContractMatch struct {
	TopicsAny []string `json:"topics_any,omitempty"`
}

type probeContract struct {
	Group  string `json:"group"`
	Budget int    `json:"budget"`
}

type probeSelectionPlan struct {
	AddressedDefaultScope bool
	Class                 string
	MatchedTopics         []string
	Groups                []probeGroupPlan
	Revision              string
}

type probeGroupPlan struct {
	ExplicitScope bool
	Group         string
	Root          string
	Budget        int
	Subtree       []string
	topicSet      map[string]bool
}

type probeGroupSelection struct {
	Class   string
	Group   string
	Lines   []string
	Empty   bool
	Overlap bool
}

type probeTopicStore interface {
	TopicEntities() ([]mem.Entity, error)
}

// builtInProbeContracts are portable contracts, not an always-on feature flag. With no registry
// file they activate only when the task classifier produces a registered topic seed and the
// selected class has a resolvable probe root. A home without that complete trigger takes the
// untouched flat path instead of turning a partial taxonomy migration into empty memory.
func builtInProbeContracts() probeContractRegistry {
	return probeContractRegistry{
		SchemaVersion: probeContractsSchemaVersion,
		DefaultClass:  "general",
		TaskClasses: []probeContractTaskClass{
			{
				Class: "system-work",
				Match: probeContractMatch{TopicsAny: []string{"system"}},
				Probes: []probeContract{
					{Group: "standing-law", Budget: 3},
					{Group: "trap", Budget: 3},
					{Group: "system", Budget: 5},
					{Group: "opportunity", Budget: 1},
				},
			},
			{
				Class: "product-work",
				Match: probeContractMatch{TopicsAny: []string{"product"}},
				Probes: []probeContract{
					{Group: "standing-law", Budget: 3},
					{Group: "trap", Budget: 3},
					{Group: "product", Budget: 5},
					{Group: "opportunity", Budget: 1},
				},
			},
			{
				Class: "general",
				Probes: []probeContract{
					{Group: "standing-law", Budget: 3},
					{Group: "trap", Budget: 3},
					{Group: "opportunity", Budget: 1},
				},
			},
		},
	}
}

func loadProbeContractRegistry(homePath string) (probeContractRegistry, bool, error) {
	path := filepath.Join(homePath, "registries", "probe-contracts.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		registry := builtInProbeContracts()
		return registry, false, nil
	}
	if err != nil {
		return probeContractRegistry{}, false, fmt.Errorf("read probe contracts %s: %w", path, err)
	}
	var registry probeContractRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return probeContractRegistry{}, true, fmt.Errorf("parse probe contracts %s: %w", path, err)
	}
	if err := validateProbeContractRegistry(&registry); err != nil {
		return probeContractRegistry{}, true, fmt.Errorf("probe contracts %s: %w", path, err)
	}
	return registry, true, nil
}

func validateProbeContractRegistry(registry *probeContractRegistry) error {
	registry.SchemaVersion = strings.TrimSpace(registry.SchemaVersion)
	if registry.SchemaVersion == "" {
		// Topic imports established omission-as-current for v0.1 fixture/config compatibility.
		// A declared, different version still fails closed instead of being guessed forward.
		registry.SchemaVersion = probeContractsSchemaVersion
	}
	if registry.SchemaVersion != probeContractsSchemaVersion {
		return fmt.Errorf("schema_version %q, want %q", registry.SchemaVersion, probeContractsSchemaVersion)
	}
	registry.DefaultClass = strings.TrimSpace(registry.DefaultClass)
	if registry.DefaultClass == "" {
		return fmt.Errorf("default_class is required")
	}
	seenClasses := map[string]bool{}
	for i := range registry.TaskClasses {
		class := &registry.TaskClasses[i]
		class.Class = strings.TrimSpace(class.Class)
		if class.Class == "" {
			return fmt.Errorf("task_classes[%d].class is required", i)
		}
		if seenClasses[class.Class] {
			return fmt.Errorf("task class %q is duplicated", class.Class)
		}
		seenClasses[class.Class] = true
		class.Match.TopicsAny = compactProbeStrings(class.Match.TopicsAny)
		seenGroups := map[string]bool{}
		for j := range class.Probes {
			probe := &class.Probes[j]
			probe.Group = strings.TrimSpace(probe.Group)
			if probe.Group == "" {
				return fmt.Errorf("task class %q probe %d needs group", class.Class, j)
			}
			if probe.Budget <= 0 {
				return fmt.Errorf("task class %q probe %q budget must be >0", class.Class, probe.Group)
			}
			if seenGroups[probe.Group] {
				return fmt.Errorf("task class %q probe group %q is duplicated", class.Class, probe.Group)
			}
			seenGroups[probe.Group] = true
		}
	}
	return nil
}

func compactProbeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func buildProbeSelectionPlan(homePath string, in Input, store probeTopicStore, view *mem.CurrentView) (*probeSelectionPlan, error) {
	registry, explicit, err := loadProbeContractRegistry(homePath)
	if err != nil {
		return nil, err
	}
	if !explicit {
		if err := validateProbeContractRegistry(&registry); err != nil {
			return nil, fmt.Errorf("built-in probe contracts invalid: %w", err)
		}
	}
	entities, err := store.TopicEntities()
	if err != nil {
		// Built-ins are fail-open migration scaffolding. An unreadable/missing entity registry
		// cannot be allowed to break or alter the legacy prompt path. An explicit registry is an
		// operator contract and therefore reports the configuration dependency failure.
		if !explicit {
			return nil, nil
		}
		return nil, fmt.Errorf("load topic entities for probe contracts: %w", err)
	}
	topics := probeTopicEntities(entities)
	if len(topics) == 0 && !explicit {
		return nil, nil
	}
	matchedTopics := classifyProbeTopics(memoryWorkQueryText(resolvedTaskContract(in).Intent), topics)
	if len(matchedTopics) == 0 && !explicit {
		return nil, nil
	}
	class, ok := selectProbeTaskClass(registry, matchedTopics, topics, view)
	if !ok || len(class.Probes) == 0 {
		return nil, nil
	}
	plan := &probeSelectionPlan{Class: class.Class, MatchedTopics: matchedTopics, AddressedDefaultScope: explicit && class.Class == registry.DefaultClass && len(memoryWorkAddresses(resolvedTaskContract(in).Intent)) > 0}
	hasBuiltInTrigger := false
	for _, probe := range class.Probes {
		group := probeGroupPlan{Group: probe.Group, Budget: probe.Budget, ExplicitScope: plan.AddressedDefaultScope}
		if root, resolved := resolveProbeTopic(topics, probe.Group); resolved {
			group.Root = root
			group.Subtree = view.TopicSubtree(root)
			group.topicSet = probeTopicSet(group.Subtree)
			hasBuiltInTrigger = true
		}
		plan.Groups = append(plan.Groups, group)
	}
	if !explicit && !hasBuiltInTrigger {
		return nil, nil
	}
	plan.Revision = probeSelectionRevision(registry, plan)
	return plan, nil
}

func probeTopicEntities(entities []mem.Entity) []mem.Entity {
	out := make([]mem.Entity, 0, len(entities))
	for _, entity := range entities {
		if strings.EqualFold(strings.TrimSpace(entity.Kind), "topic") {
			out = append(out, entity)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func classifyProbeTopics(task string, topics []mem.Entity) []string {
	taskTokens := probeBoundaryTokens(task)
	matched := make([]string, 0, len(topics))
	for _, topic := range topics {
		identities := append([]string{topic.Slug}, topic.Aliases...)
		for _, identity := range identities {
			if probeTokenSequencePresent(taskTokens, probeBoundaryTokens(identity)) {
				matched = append(matched, topic.Slug)
				break
			}
		}
	}
	sort.Strings(matched)
	return matched
}

func probeBoundaryTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func probeTokenSequencePresent(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		matched := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func selectProbeTaskClass(registry probeContractRegistry, matchedTopics []string, topics []mem.Entity, view *mem.CurrentView) (probeContractTaskClass, bool) {
	matched := probeTopicSet(matchedTopics)
	for _, class := range registry.TaskClasses {
		if len(class.Match.TopicsAny) == 0 {
			continue
		}
		for _, rootToken := range class.Match.TopicsAny {
			root, resolved := resolveProbeTopic(topics, rootToken)
			if !resolved {
				continue
			}
			for _, topic := range view.TopicSubtree(root) {
				if matched[topic] {
					return class, true
				}
			}
		}
	}
	for _, class := range registry.TaskClasses {
		if class.Class == registry.DefaultClass {
			return class, true
		}
	}
	return probeContractTaskClass{}, false
}

func resolveProbeTopic(topics []mem.Entity, token string) (string, bool) {
	token = strings.TrimSpace(token)
	for _, topic := range topics {
		if strings.EqualFold(topic.Slug, token) {
			return topic.Slug, true
		}
	}
	matched := ""
	for _, topic := range topics {
		for _, alias := range topic.Aliases {
			if !strings.EqualFold(strings.TrimSpace(alias), token) {
				continue
			}
			if matched != "" && matched != topic.Slug {
				return "", false
			}
			matched = topic.Slug
		}
	}
	return matched, matched != ""
}

func probeTopicSet(topics []string) map[string]bool {
	set := make(map[string]bool, len(topics))
	for _, topic := range topics {
		set[topic] = true
	}
	return set
}

func probeSelectionRevision(registry probeContractRegistry, plan *probeSelectionPlan) string {
	type revisionGroup struct {
		Group   string   `json:"group"`
		Root    string   `json:"root,omitempty"`
		Budget  int      `json:"budget"`
		Subtree []string `json:"subtree,omitempty"`
	}
	body := struct {
		Registry              probeContractRegistry `json:"registry"`
		AddressedDefaultScope bool                  `json:"addressed_default_scope,omitempty"`
		Class                 string                `json:"class"`
		MatchedTopics         []string              `json:"matched_topics,omitempty"`
		Groups                []revisionGroup       `json:"groups"`
	}{Registry: registry, Class: plan.Class, MatchedTopics: plan.MatchedTopics, AddressedDefaultScope: plan.AddressedDefaultScope}
	for _, group := range plan.Groups {
		body.Groups = append(body.Groups, revisionGroup{
			Group:   group.Group,
			Root:    group.Root,
			Budget:  group.Budget,
			Subtree: group.Subtree,
		})
	}
	data, _ := json.Marshal(body)
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type probeMemoryCandidate struct {
	kind     string
	ref      string
	line     string
	rank     float64
	seq      int64
	memory   contextMemoryRef
	position contextPositionRef
}

func loadProbeGroupedRefsFromView(in Input, rule MemoryRule, semanticRanks map[string]float64, view *mem.CurrentView, dependencies *runtimeMemoryDependencies, plan *probeSelectionPlan) ([]contextMemoryRef, []contextPositionRef, []envelope.ExcludedRef, []probeGroupSelection) {
	if plan == nil || len(rule.Ambient) == 0 {
		return nil, nil, nil, nil
	}
	entities := memoryEntityKeys(in)
	seedProductFenceEntity(entities, dependencies.ProductFence)
	for _, seed := range plan.MatchedTopics {
		entities[memoryKey(seed)] = true
	}
	taskTokens := memoryTaskTokens(resolvedTaskContract(in).Intent)
	claims := view.Claims()
	claimByID := make(map[string]mem.Claim, len(claims))
	for _, claim := range claims {
		claimByID[claim.ID] = claim
	}
	positions := view.Positions()
	selected := map[string]bool{}
	var memoryRefs []contextMemoryRef
	var positionRefs []contextPositionRef
	var excluded []envelope.ExcludedRef
	var selections []probeGroupSelection
	productFenceOmitted := false
	for _, group := range plan.Groups {
		selection := probeGroupSelection{Class: plan.Class, Group: group.Group}
		candidates := make([]probeMemoryCandidate, 0)
		if len(group.topicSet) > 0 {
			for _, claim := range claims {
				candidate, ok, omitted, exclusion := probeClaimCandidate(in, rule, semanticRanks, dependencies, entities, taskTokens, group, claim)
				if omitted {
					productFenceOmitted = true
				}
				if exclusion.Ref != "" {
					excluded = append(excluded, exclusion)
				}
				if ok {
					candidates = append(candidates, candidate)
				}
			}
			for _, position := range positions {
				claim, ok := claimByID[position.ActiveClaimID]
				if !ok {
					continue
				}
				candidate, ok, omitted, exclusion := probePositionCandidate(in, rule, semanticRanks, dependencies, entities, taskTokens, group, position, claim)
				if omitted {
					productFenceOmitted = true
				}
				if exclusion.Ref != "" {
					excluded = append(excluded, exclusion)
				}
				if ok {
					candidates = append(candidates, candidate)
				}
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].rank != candidates[j].rank {
				return candidates[i].rank > candidates[j].rank
			}
			if candidates[i].seq != candidates[j].seq {
				return candidates[i].seq > candidates[j].seq
			}
			// A claim and its folded position have identical score/sequence. Prefer the folded
			// current stance so a tight budget still populates ActivePositions deterministically.
			if candidates[i].kind != candidates[j].kind {
				return candidates[i].kind == "position"
			}
			return candidates[i].ref < candidates[j].ref
		})
		if len(candidates) == 0 {
			selection.Empty = true
			excluded = append(excluded, envelope.ExcludedRef{Ref: "probe://" + group.Group, Reason: "probed-and-empty"})
			selections = append(selections, selection)
			continue
		}
		for _, candidate := range candidates {
			if len(selection.Lines) >= group.Budget {
				break
			}
			if selected[candidate.ref] {
				selection.Overlap = true
				continue
			}
			selected[candidate.ref] = true
			label := "probe:" + plan.Class + "/" + group.Group + "; "
			if candidate.kind == "position" {
				candidate.position.Reason = label + candidate.position.Reason
				positionRefs = append(positionRefs, candidate.position)
			} else {
				candidate.memory.Reason = label + candidate.memory.Reason
				memoryRefs = append(memoryRefs, candidate.memory)
			}
			selection.Lines = append(selection.Lines, candidate.line)
		}
		selections = append(selections, selection)
	}
	if productFenceOmitted {
		excluded = append(excluded, envelope.ExcludedRef{
			Ref:    servedProductFenceExclusionRef,
			Reason: servedProductFenceExclusionReason,
		})
	}
	return memoryRefs, positionRefs, excluded, selections
}

func probeClaimCandidate(in Input, rule MemoryRule, semanticRanks map[string]float64, dependencies *runtimeMemoryDependencies, entities, taskTokens map[string]bool, group probeGroupPlan, claim mem.Claim) (probeMemoryCandidate, bool, bool, envelope.ExcludedRef) {
	if !probeClaimInTopicSet(claim, group.topicSet) || !probeClaimInInputTopic(claim, dependencies) || !memoryAbsoluteGateAllowsOrient(claim) {
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{}
	}
	sem := semanticRanks[claim.ID]
	baseRelevance := memoryRelevanceRank(claim, entities, taskTokens, sem, nil)
	selfEntities := dependencySelfEntities(dependencies)
	if !probeMemoryPolicyAllows(claim, rule, baseRelevance, selfEntities) {
		// audit-20260723 E12: per-claim policy exclusions are NEVER silently dropped —
		// the Excluded ledger is the single observable record of "evaluated, but not
		// loaded, and here's why", regardless of whether the surrounding probe group
		// itself ends up empty.
		if reason := memoryPolicyExclusionReason(claim, rule, baseRelevance, selfEntities); reason != "" {
			return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{
				Ref:    "claim://" + claim.ID,
				Reason: reason,
			}
		}
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{}
	}
	temps := dependencies.Temperatures
	relevance := memoryRelevanceRank(claim, entities, taskTokens, sem, temps)
	if !productRecallFenceAllowsClaim(dependencies.ProductFence, claim) {
		return probeMemoryCandidate{}, false, true, envelope.ExcludedRef{}
	}
	if reason := addressedWorkStatusExclusionReason(claim, entities, taskTokens, sem, group.ExplicitScope || strings.TrimSpace(in.Topic) != ""); reason != "" {
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{Ref: "claim://" + claim.ID, Reason: reason}
	}
	if baseRelevance > 0 {
		if temperature := memoryTemperatureTerm(temps, claim.ID); temperature != 0 {
			dependencies.AppliedTemperatures[claim.ID] = temperature
		}
	}
	line := memoryLine(claim)
	ref := contextMemoryRef{
		Ref:        "claim://" + claim.ID,
		Reason:     "Compound memory ledger claim; source stores and indexes are evidence, not authority: " + line,
		Line:       line,
		Confidence: claim.Confidence,
		Seq:        claim.Seq,
		Rank:       probeGroupBaseRank + relevance,
	}
	return probeMemoryCandidate{kind: "claim", ref: ref.Ref, line: line, rank: ref.Rank, seq: ref.Seq, memory: ref}, true, false, envelope.ExcludedRef{}
}

func probePositionCandidate(in Input, rule MemoryRule, semanticRanks map[string]float64, dependencies *runtimeMemoryDependencies, entities, taskTokens map[string]bool, group probeGroupPlan, position mem.Position, claim mem.Claim) (probeMemoryCandidate, bool, bool, envelope.ExcludedRef) {
	active := activePositionFromFold(position, claim)
	if active.Ref == "" || !probePositionInTopicSet(active, claim, group.topicSet) || !probePositionInInputTopic(active, claim, dependencies) || !memoryAbsoluteGateAllowsOrient(claim) {
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{}
	}
	sem := semanticRanks[claim.ID]
	relevance := memoryRelevanceRank(claim, entities, taskTokens, sem, nil)
	selfEntities := dependencySelfEntities(dependencies)
	if !probeMemoryPolicyAllows(claim, rule, relevance, selfEntities) {
		// audit-20260723 E12: per-claim policy exclusions on the position path are NEVER
		// silently dropped — the Excluded ledger records the claim that backed the
		// position (not the position ref itself, which is derived state) so operators
		// can answer "why didn't this position surface" from a single grep on
		// pack.Excluded / ec.Excluded.
		if reason := memoryPolicyExclusionReason(claim, rule, relevance, selfEntities); reason != "" {
			return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{
				Ref:    "claim://" + claim.ID,
				Reason: reason,
			}
		}
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{}
	}
	if currentStatePositionFreshnessExclusionReason(active.Ref, claim) != "" {
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{}
	}
	if !productRecallFenceAllowsClaim(dependencies.ProductFence, claim) {
		return probeMemoryCandidate{}, false, true, envelope.ExcludedRef{}
	}
	if reason := addressedWorkStatusExclusionReason(claim, entities, taskTokens, sem, group.ExplicitScope || strings.TrimSpace(in.Topic) != ""); reason != "" {
		return probeMemoryCandidate{}, false, false, envelope.ExcludedRef{Ref: "claim://" + claim.ID, Reason: reason}
	}
	ref := contextPositionRef{
		Ref:        active.Ref,
		Reason:     positionMemoryReasonPrefix + active.Line,
		Line:       active.Line,
		Position:   active,
		Confidence: claim.Confidence,
		Seq:        claim.Seq,
		Rank:       probeGroupBaseRank + relevance,
	}
	return probeMemoryCandidate{kind: "position", ref: ref.Ref, line: ref.Line, rank: ref.Rank, seq: ref.Seq, position: ref}, true, false, envelope.ExcludedRef{}
}

func probeMemoryPolicyAllows(claim mem.Claim, rule MemoryRule, relevance float64, selfEntities map[string]bool) bool {
	policyRank := relevance
	if policyRank <= 0 {
		policyRank = probeGroupBaseRank
	}
	if memoryPolicyExclusionReason(claim, rule, policyRank, selfEntities) != "" {
		return false
	}
	// Explicit-only personal memory still requires task relevance. The group base may sweep
	// ordinary standing facts, but it cannot turn an ambient probe into explicit user intent.
	return !(contains(rule.ExplicitOnly, "personal-corpus") && personalMemoryClaim(claim, selfEntities) && relevance <= 0)
}

func probeClaimInTopicSet(claim mem.Claim, topicSet map[string]bool) bool {
	return topicSet[claim.Topic()] || mem.ClaimHasTopicRef(claim, topicSet)
}

func probePositionInTopicSet(position envelope.ActivePosition, claim mem.Claim, topicSet map[string]bool) bool {
	return topicSet[position.Topic] || mem.ClaimHasTopicRef(claim, topicSet)
}

func probeClaimInInputTopic(claim mem.Claim, dependencies *runtimeMemoryDependencies) bool {
	return dependencies == nil || dependencies.TopicSet == nil || probeClaimInTopicSet(claim, dependencies.TopicSet)
}

func probePositionInInputTopic(position envelope.ActivePosition, claim mem.Claim, dependencies *runtimeMemoryDependencies) bool {
	return dependencies == nil || dependencies.TopicSet == nil || probePositionInTopicSet(position, claim, dependencies.TopicSet)
}
