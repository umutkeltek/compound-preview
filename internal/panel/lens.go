package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"compound/internal/capgraph"
)

type Lens struct {
	NodeID       string   `json:"node_id"`
	Name         string   `json:"name"`
	Triggers     []string `json:"triggers"`
	Weight       float64  `json:"weight"`
	PromptPath   string   `json:"prompt_path"`
	PromptSHA256 string   `json:"prompt_sha256"`
}

type LensSelection struct {
	Included          []Lens         `json:"included"`
	IncludedRefs      []IncludedLens `json:"included_refs"`
	Excluded          []ExcludedLens `json:"excluded"`
	MissingDomainLens []string       `json:"missing_domain_lens,omitempty"`
}

type LensContext struct {
	Domain string `json:"domain,omitempty"`
	Task   string `json:"task,omitempty"`
}

const (
	lensReasonTriggerMatch       = "trigger_match"
	lensReasonRetired            = "retired"
	lensReasonEvidenceOnly       = "evidence_only"
	lensReasonNoPromptInRegistry = "no_prompt_in_registry"
	lensReasonTriggerMismatch    = "trigger_mismatch"
	lensReasonDomainTriggerMatch = "domain_trigger_match"
)

func SelectLenses(homePath, targetKind string) (LensSelection, error) {
	return SelectLensesWithContext(homePath, targetKind, LensContext{})
}

func SelectLensesWithContext(homePath, targetKind string, context LensContext) (LensSelection, error) {
	targetKind = strings.TrimSpace(targetKind)
	if targetKind == "" {
		return LensSelection{}, fmt.Errorf("target kind required")
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		return LensSelection{}, err
	}
	nodes, err := store.List()
	if err != nil {
		return LensSelection{}, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	targetTokens := targetKindTokens(targetKind)
	contextTokens := lensContextTokens(context)
	domainTokens := domainLensTokens(context.Domain)
	domainMatched := len(domainTokens) == 0
	selection := LensSelection{}
	for _, node := range nodes {
		if node.Kind != "lens" {
			continue
		}
		if node.Retired {
			selection.Excluded = append(selection.Excluded, ExcludedLens{Ref: node.ID, Reason: lensReasonRetired})
			continue
		}
		if node.EvidenceOnly {
			selection.Excluded = append(selection.Excluded, ExcludedLens{Ref: node.ID, Reason: lensReasonEvidenceOnly})
			continue
		}
		promptPath := filepath.Join(homePath, "registries", "lens-prompts", node.ID+".md")
		body, err := os.ReadFile(promptPath)
		if err != nil {
			if os.IsNotExist(err) {
				selection.Excluded = append(selection.Excluded, ExcludedLens{Ref: node.ID, Reason: lensReasonNoPromptInRegistry})
				continue
			}
			return LensSelection{}, fmt.Errorf("read lens prompt %s: %w", node.ID, err)
		}
		triggerReason := lensTriggerReason(node.Triggers, targetTokens, contextTokens)
		if triggerReason == "" {
			selection.Excluded = append(selection.Excluded, ExcludedLens{Ref: node.ID, Reason: lensReasonTriggerMismatch})
			continue
		}
		if len(domainTokens) > 0 && triggerIntersects(node.Triggers, domainTokens) {
			domainMatched = true
		}
		hash := sha256.Sum256(body)
		promptSHA := hex.EncodeToString(hash[:])
		lens := Lens{
			NodeID:       node.ID,
			Name:         node.Name,
			Triggers:     append([]string(nil), node.Triggers...),
			Weight:       node.Weight,
			PromptPath:   promptPath,
			PromptSHA256: promptSHA,
		}
		selection.Included = append(selection.Included, lens)
		selection.IncludedRefs = append(selection.IncludedRefs, IncludedLens{
			Ref:          node.ID,
			Reason:       triggerReason,
			Weight:       node.Weight,
			PromptSHA256: promptSHA,
		})
	}
	if !domainMatched {
		selection.MissingDomainLens = sortedTokenKeys(domainTokens)
	}
	return selection, nil
}

func triggerMatchesTarget(triggers []string, targetKind string) bool {
	tokens := targetKindTokens(targetKind)
	return triggerIntersects(triggers, tokens)
}

func lensTriggerReason(triggers []string, targetTokens, contextTokens map[string]bool) string {
	if triggerIntersects(triggers, targetTokens) {
		return lensReasonTriggerMatch
	}
	if triggerIntersects(triggers, contextTokens) {
		return lensReasonDomainTriggerMatch
	}
	return ""
}

func triggerIntersects(triggers []string, tokens map[string]bool) bool {
	for _, trigger := range triggers {
		trigger = strings.ToLower(strings.TrimSpace(trigger))
		if trigger == "" {
			continue
		}
		if tokens[trigger] {
			return true
		}
	}
	return false
}

func lensContextTokens(context LensContext) map[string]bool {
	out := targetKindTokens(context.Domain)
	for token := range targetKindTokens(context.Task) {
		out[token] = true
	}
	return out
}

func domainLensTokens(domain string) map[string]bool {
	return targetKindTokens(domain)
}

func targetKindTokens(targetKind string) map[string]bool {
	out := map[string]bool{}
	lower := strings.ToLower(strings.TrimSpace(targetKind))
	if lower == "" {
		return out
	}
	out[lower] = true
	for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '_' || r == '-' || r == ':' || r == '.' || unicode.IsSpace(r)
	}) {
		if token != "" {
			out[token] = true
		}
	}
	return out
}

func sortedTokenKeys(tokens map[string]bool) []string {
	out := make([]string, 0, len(tokens))
	for token := range tokens {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}
