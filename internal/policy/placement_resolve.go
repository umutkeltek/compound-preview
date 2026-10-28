package policy

import (
	"fmt"
	"strings"

	"compound/internal/candidates"
	"compound/internal/finding"
)

// The placement RESOLVER is the positive half of the placement grammar: the guard DENIES a write to
// the wrong place; the resolver answers "then where DOES this go?". It is a READ FACADE — it composes
// the answer at query time from the registries that already own each fact and holds NO authored data
// of its own (a facade over N sources is not fragmentation; authoring the same fact in N+1 places is).
//
// Sourcing is deliberate (Amendment A): a candidate kind's destination comes from candidates.Spec —
// its REAL owner — never from placement rules, or the facade could answer with a stale enforcement
// glob and lie. The finding home comes from the finding organ; owner homes come from placement rules.

// FindingKind is the reserved kind naming the operational-finding direct-write home.
const FindingKind = "operational-finding"

// FindingRecordCommand is the canonical, copyable write-command template surfaced by the
// operational-finding placement target. It is the single discovery facade for an ordinary agent
// that wants to record a finding — including the body form, the required provenance flags, and
// the typed acknowledgement flag. Bracketed placeholders are intentionally literal so the
// string is trivially copy/mutable.
const FindingRecordCommand = "compound finding record --topic <topic> --stdin --work-id <work_id> --produced-by agent:<id> --json"

// FindingRecordConsumer is the consumer string emitted alongside the operational-finding
// resolve. It names every verb an ordinary agent can drive against the finding home.
const FindingRecordConsumer = "compound finding record/list/show"

// ResolvedTarget is the read-facade answer to "where should a write of this kind/owner go?".
type ResolvedTarget struct {
	Query    string   `json:"query"`
	Kind     string   `json:"kind,omitempty"`
	Owner    string   `json:"owner,omitempty"`
	Mode     string   `json:"mode"`   // governed_door | direct_write | doc_owner
	Target   string   `json:"target"` // canonical write location (COMPOUND_HOME-relative for home dirs)
	Source   string   `json:"source"` // which registry answered
	Consumer string   `json:"consumer,omitempty"`
	Command  string   `json:"command,omitempty"` // copyable write-command template (operational-finding only)
	Verdicts []string `json:"verdicts,omitempty"`
}

// ResolveKind answers "where does a write of KIND go?" by composing the candidate registry (governed
// door), the finding home (direct write), and — reserved — doc owners. An unknown kind is an explicit
// error that lists what IS resolvable.
func ResolveKind(kind string) (ResolvedTarget, error) {
	k := strings.TrimSpace(kind)
	if k == "" {
		return ResolvedTarget{}, fmt.Errorf("kind required")
	}
	if k == FindingKind {
		return ResolvedTarget{
			Query:    k,
			Kind:     k,
			Mode:     "direct_write",
			Target:   finding.Subdir,
			Source:   "finding-home",
			Consumer: FindingRecordConsumer,
			Command:  FindingRecordCommand,
		}, nil
	}
	if spec, ok := candidates.SpecFor(candidates.Kind(k)); ok {
		return ResolvedTarget{
			Query:    k,
			Kind:     k,
			Mode:     "governed_door",
			Target:   spec.InboxDir,
			Source:   "candidates.Spec",
			Consumer: spec.Consumer,
			Verdicts: spec.Verdicts,
		}, nil
	}
	return ResolvedTarget{}, fmt.Errorf("unknown kind %q (known: %s, %s)", k, FindingKind, strings.Join(resolvableKindNames(), ", "))
}

func resolvableKindNames() []string {
	ks := candidates.AllKinds()
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}

// ResolveOwner answers "where does placement owner OWNER write?" from the installed placement rules.
func ResolveOwner(homePath, ownerID string) (ResolvedTarget, error) {
	id := strings.TrimSpace(ownerID)
	if id == "" {
		return ResolvedTarget{}, fmt.Errorf("owner id required")
	}
	rules, err := LoadPlacementRulesFromHome(homePath)
	if err != nil {
		return ResolvedTarget{}, err
	}
	for _, o := range rules.Owners {
		if strings.TrimSpace(o.ID) == id {
			return ResolvedTarget{
				Query:  id,
				Owner:  id,
				Mode:   "doc_owner",
				Target: deglobTarget(o.CanonicalWriteGlobs),
				Source: "placement-rules",
			}, nil
		}
	}
	return ResolvedTarget{}, fmt.Errorf("unknown placement owner %q", id)
}

// PathResolution is the reverse query: given a concrete path, is it allowed, and if not, where should
// it go instead?
type PathResolution struct {
	Path     string `json:"path"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Redirect string `json:"redirect,omitempty"`
}

// ResolvePath answers "may I write to PATH, and if not, where instead?". It runs the SAME
// PlacementContextFromHome the PreToolUse guard runs, so the read-only verdict matches enforcement by
// construction — the resolver never disagrees with the guard.
func ResolvePath(homePath, path string) (PathResolution, error) {
	ctx, _, err := PlacementContextFromHome(homePath, path)
	if err != nil {
		return PathResolution{}, err
	}
	res := PathResolution{Path: path, Allowed: ctx == nil}
	if ctx != nil {
		res.Reason = ctx.Reason
		res.Owner = ctx.OwnerRef
		res.Redirect = ctx.RedirectTarget
	}
	return res, nil
}
