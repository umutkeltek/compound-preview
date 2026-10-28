package routeevidence

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Read-only projection + drift check for the OS compatibility view.
//
// The OS keeps a generated model-router view. Compound owns the authority. These
// two surfaces let the OS regenerate its view from Compound and prove it has not
// drifted — WITHOUT either side guessing:
//
//	model-capabilities --yaml            deterministic YAML projection of authority
//	model-capabilities --check-view FILE semantic drift check, nonzero on any drift
//
// The projection carries no timestamps, credential values, private endpoints,
// or private paths. Authority remains lossless inside Compound; this external
// compatibility view is the redacted consumer boundary.

var (
	modelCapabilitiesProjectionSecret = regexp.MustCompile(
		`(?i)(AKIA[0-9A-Z]{12,}|ASIA[0-9A-Z]{12,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,})`,
	)
	modelCapabilitiesProjectionPrivateEndpoint = regexp.MustCompile(
		`(?i)\bhttps?://(?:localhost|127(?:\.[0-9]{1,3}){3}|10(?:\.[0-9]{1,3}){3}|192\.168(?:\.[0-9]{1,3}){2}|172\.(?:1[6-9]|2[0-9]|3[01])(?:\.[0-9]{1,3}){2})(?::[0-9]+)?(?:/[^\s'\"]*)?`,
	)
)

// ModelCapabilitiesViewCheck is the result of comparing an external view file
// against Compound's authority.
type ModelCapabilitiesViewCheck struct {
	AuthorityRef      string   `json:"authority_ref"`
	AuthorityPath     string   `json:"authority_path"`
	AuthorityRevision string   `json:"authority_revision"`
	ViewPath          string   `json:"view_path"`
	ViewRevision      string   `json:"view_revision"`
	InSync            bool     `json:"in_sync"`
	Drifts            []string `json:"drifts"`
}

// ProjectModelCapabilitiesYAML renders the current Compound authority as a
// deterministic YAML document. yaml.v3 emits map keys in sorted order, so the
// same authority bytes always render the same projection bytes.
func ProjectModelCapabilitiesYAML(homePath string) ([]byte, error) {
	manifest, _, _, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return nil, err
	}
	sanitizeModelCapabilitiesProjection(&manifest)
	body, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal model capabilities projection: %w", err)
	}
	return body, nil
}

// CheckModelCapabilitiesView parses an external view and Compound's authority
// SEMANTICALLY (not as bytes) and reports every model, claim, routing-default,
// capability-class, and rule difference. Formatting, key order, and comments in
// the view are irrelevant; meaning is not.
func CheckModelCapabilitiesView(homePath, viewPath string) (ModelCapabilitiesViewCheck, error) {
	if strings.TrimSpace(viewPath) == "" {
		return ModelCapabilitiesViewCheck{}, fmt.Errorf("view path required")
	}
	manifest, authorityPath, authorityRevision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return ModelCapabilitiesViewCheck{}, err
	}
	sanitizeModelCapabilitiesProjection(&manifest)
	viewData, err := os.ReadFile(viewPath)
	if err != nil {
		return ModelCapabilitiesViewCheck{}, fmt.Errorf("read model capabilities view %s: %w", viewPath, err)
	}
	view, err := DecodeModelCapabilitiesYAML(viewData)
	if err != nil {
		return ModelCapabilitiesViewCheck{}, fmt.Errorf("parse model capabilities view %s: %w", viewPath, err)
	}

	check := ModelCapabilitiesViewCheck{
		AuthorityRef:      ModelCapabilitiesAuthorityRef,
		AuthorityPath:     authorityPath,
		AuthorityRevision: authorityRevision,
		ViewPath:          viewPath,
		ViewRevision:      "sha256:" + digestHex(viewData),
		Drifts:            diffModelCapabilities(manifest, view),
	}
	check.InSync = len(check.Drifts) == 0
	return check, nil
}

// sanitizeModelCapabilitiesProjection redacts only the external compatibility
// view. It never writes the authority snapshot and preserves every structural
// routing key, claim level, model id, and ordering decision. Free-text fields
// and endpoint-bearing tuple fields are sanitized consistently before both
// projection and semantic drift comparison, so redaction cannot become a drift
// bypass.
func sanitizeModelCapabilitiesProjection(manifest *ModelCapabilitiesManifest) {
	manifest.Purpose = sanitizeModelCapabilitiesProjectionText(manifest.Purpose)
	for i := range manifest.Rules {
		manifest.Rules[i] = sanitizeModelCapabilitiesProjectionText(manifest.Rules[i])
	}
	for id, entry := range manifest.Models {
		entry.Lane = sanitizeModelCapabilitiesProjectionText(entry.Lane)
		entry.ProviderRoute = sanitizeModelCapabilitiesProjectionText(entry.ProviderRoute)
		entry.ProviderEndpoint = sanitizeModelCapabilitiesProjectionEndpoint(entry.ProviderEndpoint)
		entry.ProviderRouteKind = sanitizeModelCapabilitiesProjectionText(entry.ProviderRouteKind)
		entry.Notes = sanitizeModelCapabilitiesProjectionText(entry.Notes)
		for i := range entry.Resolution.Targets {
			entry.Resolution.Targets[i].Provider = sanitizeModelCapabilitiesProjectionText(entry.Resolution.Targets[i].Provider)
			entry.Resolution.Targets[i].Model = sanitizeModelCapabilitiesProjectionText(entry.Resolution.Targets[i].Model)
		}
		for class, claim := range entry.Capabilities {
			claim.Evidence = sanitizeModelCapabilitiesProjectionText(claim.Evidence)
			entry.Capabilities[class] = claim
		}
		if entry.CostDetail != nil && entry.CostDetail.Subscription != nil {
			entry.CostDetail.Subscription.PlanRef = sanitizeModelCapabilitiesProjectionText(entry.CostDetail.Subscription.PlanRef)
		}
		if entry.CapacitySource != nil {
			entry.CapacitySource.Ref = sanitizeModelCapabilitiesProjectionText(entry.CapacitySource.Ref)
		}
		for i := range entry.Aliases {
			entry.Aliases[i] = sanitizeModelCapabilitiesProjectionText(entry.Aliases[i])
		}
		manifest.Models[id] = entry
	}
}

func sanitizeModelCapabilitiesProjectionText(value string) string {
	value = modelCapabilitiesProjectionSecret.ReplaceAllString(value, "[credential-redacted]")
	return modelCapabilitiesProjectionPrivateEndpoint.ReplaceAllString(value, "[private-endpoint]")
}

// diffModelCapabilities returns a sorted, human-readable list of every semantic
// difference between authority and view. An empty result means the view is a
// faithful projection.
func sanitizeModelCapabilitiesProjectionEndpoint(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != value {
		return "[endpoint-redacted]"
	}
	return sanitizeModelCapabilitiesProjectionText(value)
}

func diffModelCapabilities(authority, view ModelCapabilitiesManifest) []string {
	var drifts []string

	if authority.Version != view.Version {
		drifts = append(drifts, fmt.Sprintf("version: authority %d, view %d", authority.Version, view.Version))
	}
	if strings.TrimSpace(authority.Purpose) != strings.TrimSpace(view.Purpose) {
		drifts = append(drifts, fmt.Sprintf("purpose: authority %q, view %q", authority.Purpose, view.Purpose))
	}
	drifts = append(drifts, diffStringSets("capability_class", authority.CapabilityClasses, view.CapabilityClasses)...)
	drifts = append(drifts, diffStringSets("rule", authority.Rules, view.Rules)...)

	for _, id := range sortedMapKeys(authority.Models) {
		a := authority.Models[id]
		v, ok := view.Models[id]
		if !ok {
			drifts = append(drifts, "model "+id+": present in authority, absent from view")
			continue
		}
		drifts = append(drifts, diffModelEntry(id, a, v)...)
	}
	for _, id := range sortedMapKeys(view.Models) {
		if _, ok := authority.Models[id]; !ok {
			drifts = append(drifts, "model "+id+": present in view, absent from authority")
		}
	}

	for _, class := range sortedDefaultKeys(authority.RoutingDefaults) {
		a := authority.RoutingDefaults[class]
		v, ok := view.RoutingDefaults[class]
		if !ok {
			drifts = append(drifts, "routing_default "+class+": present in authority, absent from view")
			continue
		}
		if strings.Join(a, ",") != strings.Join(v, ",") {
			drifts = append(drifts, fmt.Sprintf("routing_default %s: authority [%s], view [%s]", class, strings.Join(a, ","), strings.Join(v, ",")))
		}
	}
	for _, class := range sortedDefaultKeys(view.RoutingDefaults) {
		if _, ok := authority.RoutingDefaults[class]; !ok {
			drifts = append(drifts, "routing_default "+class+": present in view, absent from authority")
		}
	}

	sort.Strings(drifts)
	return drifts
}

func diffModelEntry(id string, a, v ModelCapabilityEntry) []string {
	var drifts []string
	scalar := func(field, av, vv string) {
		if strings.TrimSpace(av) != strings.TrimSpace(vv) {
			drifts = append(drifts, fmt.Sprintf("model %s %s: authority %q, view %q", id, field, av, vv))
		}
	}
	scalar("lane", a.Lane, v.Lane)
	scalar("harness", a.Harness, v.Harness)
	scalar("provider_route", a.ProviderRoute, v.ProviderRoute)
	scalar("provider_endpoint", a.ProviderEndpoint, v.ProviderEndpoint)
	scalar("provider_route_kind", a.ProviderRouteKind, v.ProviderRouteKind)
	scalar("tier", a.Tier, v.Tier)
	scalar("packet_ceiling", a.PacketCeiling, v.PacketCeiling)
	scalar("cost_class", a.CostClass, v.CostClass)
	scalar("effort_default", a.EffortDefault, v.EffortDefault)
	scalar("resolution.mode", a.Resolution.Mode, v.Resolution.Mode)
	scalar("notes", a.Notes, v.Notes)

	// EVERY serialized tuple field is compared. A field that round-trips through
	// the projection but is not diffed here would be a silent drift channel: the
	// OS view could carry a different context window, cost, capacity source, or
	// alias set and still read as in sync.
	if a.ContextWindow != v.ContextWindow {
		drifts = append(drifts, fmt.Sprintf("model %s context_window: authority %d, view %d", id, a.ContextWindow, v.ContextWindow))
	}
	if a.MaxOutput != v.MaxOutput {
		drifts = append(drifts, fmt.Sprintf("model %s max_output: authority %d, view %d", id, a.MaxOutput, v.MaxOutput))
	}
	drifts = append(drifts, diffCostDetail(id, a.CostDetail, v.CostDetail)...)
	drifts = append(drifts, diffCapacitySource(id, a.CapacitySource, v.CapacitySource)...)
	if strings.Join(a.Aliases, ",") != strings.Join(v.Aliases, ",") {
		drifts = append(drifts, fmt.Sprintf("model %s aliases: authority [%s], view [%s]", id, strings.Join(a.Aliases, ","), strings.Join(v.Aliases, ",")))
	}
	drifts = append(drifts, diffFloatMap(id, "relative_cost_hint", a.RelativeCostHint, v.RelativeCostHint)...)

	if len(a.Resolution.Targets) != len(v.Resolution.Targets) {
		drifts = append(drifts, fmt.Sprintf("model %s resolution.targets: authority %d, view %d", id, len(a.Resolution.Targets), len(v.Resolution.Targets)))
	} else {
		for i := range a.Resolution.Targets {
			if a.Resolution.Targets[i] != v.Resolution.Targets[i] {
				drifts = append(drifts, fmt.Sprintf("model %s resolution.targets[%d]: authority %+v, view %+v", id, i, a.Resolution.Targets[i], v.Resolution.Targets[i]))
			}
		}
	}

	for _, class := range sortedCapabilityClaimClasses(a.Capabilities) {
		ac := a.Capabilities[class]
		vc, ok := v.Capabilities[class]
		if !ok {
			drifts = append(drifts, fmt.Sprintf("model %s capability %s: present in authority, absent from view", id, class))
			continue
		}
		if strings.TrimSpace(ac.Level) != strings.TrimSpace(vc.Level) {
			drifts = append(drifts, fmt.Sprintf("model %s capability %s level: authority %q, view %q", id, class, ac.Level, vc.Level))
		}
		if strings.TrimSpace(ac.Evidence) != strings.TrimSpace(vc.Evidence) {
			drifts = append(drifts, fmt.Sprintf("model %s capability %s evidence: authority %q, view %q", id, class, ac.Evidence, vc.Evidence))
		}
	}
	for _, class := range sortedCapabilityClaimClasses(v.Capabilities) {
		if _, ok := a.Capabilities[class]; !ok {
			drifts = append(drifts, fmt.Sprintf("model %s capability %s: present in view, absent from authority", id, class))
		}
	}
	return drifts
}

// diffCostDetail compares the optional cost tuple, including the nested
// subscription block. Absent-vs-present is itself a drift.
func diffCostDetail(id string, a, v *LaneCostDetail) []string {
	if a == nil && v == nil {
		return nil
	}
	if a == nil || v == nil {
		return []string{fmt.Sprintf("model %s cost_detail: authority present=%t, view present=%t", id, a != nil, v != nil)}
	}
	var drifts []string
	if a.APIUSDPerMtokIn != v.APIUSDPerMtokIn {
		drifts = append(drifts, fmt.Sprintf("model %s cost_detail.api_usd_per_mtok_in: authority %g, view %g", id, a.APIUSDPerMtokIn, v.APIUSDPerMtokIn))
	}
	if a.APIUSDPerMtokOut != v.APIUSDPerMtokOut {
		drifts = append(drifts, fmt.Sprintf("model %s cost_detail.api_usd_per_mtok_out: authority %g, view %g", id, a.APIUSDPerMtokOut, v.APIUSDPerMtokOut))
	}
	as, vs := a.Subscription, v.Subscription
	if as == nil && vs == nil {
		return drifts
	}
	if as == nil || vs == nil {
		return append(drifts, fmt.Sprintf("model %s cost_detail.subscription: authority present=%t, view present=%t", id, as != nil, vs != nil))
	}
	if as.WindowMinutes != vs.WindowMinutes {
		drifts = append(drifts, fmt.Sprintf("model %s cost_detail.subscription.window_minutes: authority %d, view %d", id, as.WindowMinutes, vs.WindowMinutes))
	}
	if as.BusyMultiplier != vs.BusyMultiplier {
		drifts = append(drifts, fmt.Sprintf("model %s cost_detail.subscription.busy_multiplier: authority %g, view %g", id, as.BusyMultiplier, vs.BusyMultiplier))
	}
	if strings.TrimSpace(as.PlanRef) != strings.TrimSpace(vs.PlanRef) {
		drifts = append(drifts, fmt.Sprintf("model %s cost_detail.subscription.plan_ref: authority %q, view %q", id, as.PlanRef, vs.PlanRef))
	}
	return drifts
}

func diffCapacitySource(id string, a, v *LaneCapacitySourceRef) []string {
	if a == nil && v == nil {
		return nil
	}
	if a == nil || v == nil {
		return []string{fmt.Sprintf("model %s capacity_source: authority present=%t, view present=%t", id, a != nil, v != nil)}
	}
	var drifts []string
	if strings.TrimSpace(a.Kind) != strings.TrimSpace(v.Kind) {
		drifts = append(drifts, fmt.Sprintf("model %s capacity_source.kind: authority %q, view %q", id, a.Kind, v.Kind))
	}
	if a.FreshnessMinutes != v.FreshnessMinutes {
		drifts = append(drifts, fmt.Sprintf("model %s capacity_source.freshness_minutes: authority %d, view %d", id, a.FreshnessMinutes, v.FreshnessMinutes))
	}
	if strings.TrimSpace(a.Ref) != strings.TrimSpace(v.Ref) {
		drifts = append(drifts, fmt.Sprintf("model %s capacity_source.ref: authority %q, view %q", id, a.Ref, v.Ref))
	}
	return drifts
}

func diffFloatMap(id, field string, a, v map[string]float64) []string {
	var drifts []string
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range v {
		keys[k] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, k := range ordered {
		av, aok := a[k]
		vv, vok := v[k]
		switch {
		case aok && !vok:
			drifts = append(drifts, fmt.Sprintf("model %s %s[%s]: present in authority, absent from view", id, field, k))
		case !aok && vok:
			drifts = append(drifts, fmt.Sprintf("model %s %s[%s]: present in view, absent from authority", id, field, k))
		case av != vv:
			drifts = append(drifts, fmt.Sprintf("model %s %s[%s]: authority %g, view %g", id, field, k, av, vv))
		}
	}
	return drifts
}

// diffStringSets compares two ordered string lists as ORDERED sequences, so a
// reordered routing rule set is reported rather than silently accepted.
func diffStringSets(label string, authority, view []string) []string {
	if strings.Join(authority, "\x00") == strings.Join(view, "\x00") {
		return nil
	}
	return []string{fmt.Sprintf("%s list: authority [%s], view [%s]", label, strings.Join(authority, ","), strings.Join(view, ","))}
}

func sortedMapKeys(in map[string]ModelCapabilityEntry) []string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedDefaultKeys(in map[string][]string) []string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
