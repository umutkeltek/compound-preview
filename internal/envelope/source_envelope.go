package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	SourceTrustUntrusted             = "untrusted"
	SourceTrustMixed                 = "mixed"
	SourceTrustAdjudicated           = "adjudicated"
	SourceTrustTrustedControlPlane   = "trusted_control_plane"
	SourceTrustAuthenticatedOperator = "authenticated_operator"

	ConfidentialityPublic       = "public"
	ConfidentialityInternal     = "internal"
	ConfidentialityConfidential = "confidential"
	ConfidentialityRestricted   = "restricted"
	ConfidentialitySecret       = "secret"
)

var (
	SourceTrustClasses = []string{
		SourceTrustUntrusted,
		SourceTrustMixed,
		SourceTrustAdjudicated,
		SourceTrustTrustedControlPlane,
		SourceTrustAuthenticatedOperator,
	}
	ConfidentialityClasses = []string{
		ConfidentialityPublic,
		ConfidentialityInternal,
		ConfidentialityConfidential,
		ConfidentialityRestricted,
		ConfidentialitySecret,
	}
	sourceSinkSlug = regexp.MustCompile(`^[a-z][a-z0-9_.:-]*$`)
)

// SourceOrigin identifies the principal and transport that introduced content.
// Authenticated marks transport authentication only; it cannot raise model,
// repository, web, tool, or connector output to operator authority.
type SourceOrigin struct {
	Kind          string `json:"kind"`
	Locator       string `json:"locator,omitempty"`
	Principal     string `json:"principal,omitempty"`
	Authenticated bool   `json:"authenticated,omitempty"`
}

// SourceParent is the immutable lineage projection needed to prove that a
// transformation did not raise trust, lower confidentiality, or widen sinks.
type SourceParent struct {
	SourceID        string   `json:"source_id"`
	ContentHash     string   `json:"content_hash"`
	TrustClass      string   `json:"trust_class"`
	Confidentiality string   `json:"confidentiality"`
	AllowedSinks    []string `json:"allowed_sinks"`
}

type SourceTransformation struct {
	Kind     string `json:"kind"`
	Producer string `json:"producer"`
	Model    string `json:"model,omitempty"`
	Tool     string `json:"tool,omitempty"`
}

// SourceBinding is the content-addressed lineage carried onto an ActionIntent.
// A source id without its content hash is not an exact action input binding.
type SourceBinding struct {
	SourceID        string `json:"source_id"`
	ContentHash     string `json:"content_hash"`
	TrustClass      string `json:"trust_class"`
	Confidentiality string `json:"confidentiality"`
}

// SourceEnvelope makes hostile and derived content data with enforceable
// lineage. Trust is inherited from the least-trusted ancestor; sinks may only
// narrow and confidentiality may only stay equal or become more restrictive.
type SourceEnvelope struct {
	Header
	Origin          SourceOrigin          `json:"origin"`
	TrustClass      string                `json:"trust_class"`
	Confidentiality string                `json:"confidentiality"`
	AllowedSinks    []string              `json:"allowed_sinks"`
	ContentHash     string                `json:"content_hash"`
	Parents         []SourceParent        `json:"parents,omitempty"`
	Transformation  *SourceTransformation `json:"transformation,omitempty"`
}

func (e *SourceEnvelope) Kind() string { return "source_envelope" }

func (e *SourceEnvelope) Validate() error {
	v := &verr{}
	e.Header.validate(v, SVSourceEnvelope, "src_")
	v.req("origin.kind", e.Origin.Kind)
	v.enum("trust_class", e.TrustClass, SourceTrustClasses, true)
	v.enum("confidentiality", e.Confidentiality, ConfidentialityClasses, true)
	if !validPrefixedSHA256(e.ContentHash) {
		v.addf("content_hash must be sha256:<64 lowercase hex>")
	}
	validateSourceSinks(v, "allowed_sinks", e.AllowedSinks)

	if e.TrustClass == SourceTrustAuthenticatedOperator {
		if e.Origin.Kind != "operator" || !e.Origin.Authenticated ||
			!strings.HasPrefix(strings.TrimSpace(e.Origin.Principal), "operator:") {
			v.addf("authenticated operator trust requires authenticated operator origin and operator provenance")
		}
	}

	if len(e.Parents) == 0 {
		if e.Transformation != nil {
			v.addf("transformation requires parents so lineage cannot be erased")
		}
		return v.result(e.Kind())
	}
	if e.Transformation == nil {
		v.addf("derived source with parents requires transformation")
	} else {
		v.req("transformation.kind", e.Transformation.Kind)
		v.req("transformation.producer", e.Transformation.Producer)
	}

	leastTrust := len(SourceTrustClasses) - 1
	mostConfidential := 0
	seenParents := map[string]struct{}{}
	for i, parent := range e.Parents {
		if strings.TrimSpace(parent.SourceID) == "" {
			v.addf("parents[%d].source_id is required", i)
		} else if _, exists := seenParents[parent.SourceID]; exists {
			v.addf("parents[%d].source_id duplicates earlier parent", i)
		}
		seenParents[parent.SourceID] = struct{}{}
		if !validPrefixedSHA256(parent.ContentHash) {
			v.addf("parents[%d].content_hash must be sha256:<64 lowercase hex>", i)
		}
		trustRank := enumRank(parent.TrustClass, SourceTrustClasses)
		if trustRank < 0 {
			v.addf("parents[%d].trust_class=%q not one of %v", i, parent.TrustClass, SourceTrustClasses)
		} else if trustRank < leastTrust {
			leastTrust = trustRank
		}
		confRank := enumRank(parent.Confidentiality, ConfidentialityClasses)
		if confRank < 0 {
			v.addf("parents[%d].confidentiality=%q not one of %v", i, parent.Confidentiality, ConfidentialityClasses)
		} else if confRank > mostConfidential {
			mostConfidential = confRank
		}
		validateSourceSinks(v, fmt.Sprintf("parents[%d].allowed_sinks", i), parent.AllowedSinks)
	}
	if trustRank := enumRank(e.TrustClass, SourceTrustClasses); trustRank > leastTrust {
		v.addf("trust_class %q raises trust above least-trusted ancestor %q", e.TrustClass, SourceTrustClasses[leastTrust])
	}
	if confRank := enumRank(e.Confidentiality, ConfidentialityClasses); confRank >= 0 && confRank < mostConfidential {
		v.addf("confidentiality %q is lower than parent confidentiality %q", e.Confidentiality, ConfidentialityClasses[mostConfidential])
	}
	for _, sink := range e.AllowedSinks {
		for _, parent := range e.Parents {
			if !containsSourceString(parent.AllowedSinks, sink) {
				v.addf("allowed_sinks contains %q not allowed by parent %s", sink, parent.SourceID)
				break
			}
		}
	}
	return v.result(e.Kind())
}

func NewSourceEnvelope(header Header, origin SourceOrigin, content []byte, trust, confidentiality string, sinks []string) SourceEnvelope {
	header.SchemaVersion = SVSourceEnvelope
	return SourceEnvelope{
		Header:          header,
		Origin:          origin,
		TrustClass:      trust,
		Confidentiality: confidentiality,
		AllowedSinks:    normalizedSourceSinks(sinks),
		ContentHash:     sourceContentHash(content),
	}
}

type SourceDerivationPolicy struct {
	MaximumTrust           string
	MinimumConfidentiality string
	AllowedSinks           []string
}

func DeriveSourceEnvelope(
	header Header,
	origin SourceOrigin,
	transformation SourceTransformation,
	content []byte,
	parents ...SourceEnvelope,
) (SourceEnvelope, error) {
	return DeriveSourceEnvelopeWithPolicy(
		header,
		origin,
		transformation,
		content,
		SourceDerivationPolicy{},
		parents...,
	)
}

// DeriveSourceEnvelopeWithPolicy applies origin-specific restrictions before
// validation. This is required for outputs such as web or subagent results:
// their parent may be an authenticated operator prompt, but the new output
// must be born untrusted rather than temporarily validating as operator input.
func DeriveSourceEnvelopeWithPolicy(
	header Header,
	origin SourceOrigin,
	transformation SourceTransformation,
	content []byte,
	policy SourceDerivationPolicy,
	parents ...SourceEnvelope,
) (SourceEnvelope, error) {
	if len(parents) == 0 {
		return SourceEnvelope{}, fmt.Errorf("derive source envelope: at least one parent is required")
	}
	leastTrust := len(SourceTrustClasses) - 1
	mostConfidential := 0
	parentRefs := make([]SourceParent, 0, len(parents))
	sinks := normalizedSourceSinks(parents[0].AllowedSinks)
	for i := range parents {
		if err := parents[i].Validate(); err != nil {
			return SourceEnvelope{}, fmt.Errorf("derive source envelope parent %s: %w", parents[i].ID, err)
		}
		trustRank := enumRank(parents[i].TrustClass, SourceTrustClasses)
		if trustRank < leastTrust {
			leastTrust = trustRank
		}
		confRank := enumRank(parents[i].Confidentiality, ConfidentialityClasses)
		if confRank > mostConfidential {
			mostConfidential = confRank
		}
		if i > 0 {
			sinks = intersectSourceSinks(sinks, parents[i].AllowedSinks)
		}
		parentRefs = append(parentRefs, SourceParent{
			SourceID:        parents[i].ID,
			ContentHash:     parents[i].ContentHash,
			TrustClass:      parents[i].TrustClass,
			Confidentiality: parents[i].Confidentiality,
			AllowedSinks:    normalizedSourceSinks(parents[i].AllowedSinks),
		})
	}
	header.SchemaVersion = SVSourceEnvelope
	derived := SourceEnvelope{
		Header:          header,
		Origin:          origin,
		TrustClass:      SourceTrustClasses[leastTrust],
		Confidentiality: ConfidentialityClasses[mostConfidential],
		AllowedSinks:    sinks,
		ContentHash:     sourceContentHash(content),
		Parents:         parentRefs,
		Transformation:  &transformation,
	}
	if rank := enumRank(policy.MaximumTrust, SourceTrustClasses); rank >= 0 && rank < leastTrust {
		derived.TrustClass = SourceTrustClasses[rank]
	}
	if rank := enumRank(policy.MinimumConfidentiality, ConfidentialityClasses); rank > mostConfidential {
		derived.Confidentiality = ConfidentialityClasses[rank]
	}
	if policy.AllowedSinks != nil {
		derived.AllowedSinks = intersectSourceSinks(derived.AllowedSinks, policy.AllowedSinks)
	}
	if err := derived.Validate(); err != nil {
		return SourceEnvelope{}, err
	}
	return derived, nil
}

func (e SourceEnvelope) Binding() SourceBinding {
	return SourceBinding{
		SourceID:        e.ID,
		ContentHash:     e.ContentHash,
		TrustClass:      e.TrustClass,
		Confidentiality: e.Confidentiality,
	}
}

func sourceContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validPrefixedSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256(strings.TrimPrefix(value, "sha256:"))
}

func enumRank(value string, values []string) int {
	for i := range values {
		if value == values[i] {
			return i
		}
	}
	return -1
}

func validateSourceSinks(v *verr, field string, sinks []string) {
	seen := map[string]struct{}{}
	for i, sink := range sinks {
		if !sourceSinkSlug.MatchString(sink) {
			v.addf("%s[%d]=%q must be a sink slug", field, i, sink)
		}
		if _, exists := seen[sink]; exists {
			v.addf("%s[%d]=%q duplicates earlier sink", field, i, sink)
		}
		seen[sink] = struct{}{}
	}
}

func normalizedSourceSinks(sinks []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(sinks))
	for _, sink := range sinks {
		sink = strings.TrimSpace(sink)
		if sink == "" {
			continue
		}
		if _, exists := seen[sink]; exists {
			continue
		}
		seen[sink] = struct{}{}
		out = append(out, sink)
	}
	sort.Strings(out)
	return out
}

func intersectSourceSinks(left, right []string) []string {
	rightSet := map[string]struct{}{}
	for _, sink := range right {
		rightSet[sink] = struct{}{}
	}
	out := make([]string, 0, len(left))
	for _, sink := range left {
		if _, ok := rightSet[sink]; ok {
			out = append(out, sink)
		}
	}
	return normalizedSourceSinks(out)
}

func containsSourceString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
