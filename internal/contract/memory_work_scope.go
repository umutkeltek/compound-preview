package contract

import (
	"strings"

	"compound/internal/mem"
)

const addressedWorkStatusScopeReason = "operational status is outside this addressed-work scope; name its subject or request its topic explicitly"

// memoryTaskTokens keeps opaque Work addresses intact. Their structural fragments
// are not natural-language requests for topics such as product or system.
func memoryTaskTokens(task string) map[string]bool {
	tokens := memoryTextTokens(memoryWorkQueryText(task))
	for _, address := range memoryWorkAddresses(task) {
		tokens[address] = true
	}
	return tokens
}

func memoryWorkAddresses(text string) []string {
	parts := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r > 127 || !decisionIdentityByte(byte(r))
	})
	var out []string
	seen := map[string]bool{}
	for _, part := range parts {
		if isMemoryWorkAddress(part) && !seen[part] {
			out = append(out, part)
			seen[part] = true
		}
	}
	return out
}

func isMemoryWorkAddress(token string) bool {
	return strings.HasPrefix(token, "work_") && len(token) > len("work_")
}

func memoryWorkQueryText(task string) string {
	text := strings.ToLower(task)
	masked := []byte(text)
	for _, address := range memoryWorkAddresses(text) {
		for offset := 0; offset < len(text); {
			relative := strings.Index(text[offset:], address)
			if relative < 0 {
				break
			}
			start, end := offset+relative, offset+relative+len(address)
			if (start == 0 || !decisionIdentityByte(text[start-1])) &&
				(end == len(text) || !decisionIdentityByte(text[end])) {
				for i := start; i < end; i++ {
					masked[i] = ' '
				}
			}
			offset = end
		}
	}
	return string(masked)
}

func memoryClaimMentionsWork(claim mem.Claim, address string) bool {
	for _, text := range []string{claim.Entity, claim.Predicate, claim.Object} {
		if textMentionsIdentity(text, address) {
			return true
		}
	}
	for key, value := range claim.Qualifiers {
		if textMentionsIdentity(key+" "+value, address) {
			return true
		}
	}
	for _, ref := range claim.EvidenceRefs {
		if textMentionsIdentity(ref, address) {
			return true
		}
	}
	return false
}

// addressedWorkSubjectScopeAllows is a relevance test, not a truth or privacy
// decision. Provenance can establish an exact target match, never foreign scope.
func addressedWorkSubjectScopeAllows(claim mem.Claim, entities, taskTokens map[string]bool, semanticScore float64, explicitScope bool) bool {
	addressed := false
	for token := range taskTokens {
		if !isMemoryWorkAddress(token) {
			continue
		}
		addressed = true
		if memoryClaimMentionsWork(claim, token) {
			return true
		}
	}
	return !addressed || explicitScope || semanticScore > 0 || entities[memoryKey(claim.Entity)]
}

func addressedWorkStatusExclusionReason(claim mem.Claim, entities, taskTokens map[string]bool, semanticScore float64, explicitScope bool) string {
	if !strings.EqualFold(strings.TrimSpace(claim.Visibility), "operational") ||
		mem.EffectiveCanonClass(claim.ClaimType, claim.CanonClass) != "status" {
		return ""
	}
	if addressedWorkSubjectScopeAllows(claim, entities, taskTokens, semanticScore, explicitScope) {
		return ""
	}
	return addressedWorkStatusScopeReason
}
