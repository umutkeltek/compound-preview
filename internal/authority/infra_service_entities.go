package authority

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/mem"

	"gopkg.in/yaml.v3"
)

// infra_service_entities.go extends the infra-services import door
// (infra_registries.go) with two additive effects that run AFTER
// ImportInfraServicesRegistry installs services.yaml: registering each
// service as a resolvable mem.Entity (so `authority resolve <token>` finds
// it) and enrolling each service as a capability-graph node (so `orient`
// surfaces it for service/deploy/DNS-shaped tasks). This is the entity-import
// attach point settled for work_c8178a5ab583 — not a second resolver source
// and not a capgraph/orient internals edit.

// InfraServiceEntityKind is the mem.Entity kind used for every service:*
// entity registered from services.yaml. mem.EntityKinds has no dedicated
// "service" value and extending it is out of this packet's scope (TOUCH ONLY
// boundary excludes internal/mem/entity.go except for an idempotency guard),
// so this reuses the closest existing kind in the vocabulary: an infra
// service is a networked thing that runs somewhere, the same category as the
// registry's other "server" rows.
const InfraServiceEntityKind = "server"

// InfraServiceCapabilityKind is the capgraph.NodeKinds value minted for
// service nodes (see the one-line NodeKinds addition in capgraph.go).
const InfraServiceCapabilityKind = "service"

// infraServiceCapabilityImportedVia names the source_authority record's
// imported_via so both effects of one `import-infra-services` run cite the
// same verb as infra_registries.go's InfraRegistrySourceAuthorityRecord.
const infraServiceCapabilityImportedVia = "authority import-infra-services"

// InfraServiceEntry is one decoded services.yaml row.
type InfraServiceEntry struct {
	Key       string // full registry key, e.g. "example-svc.internal"
	Bare      string // key with one trailing domain suffix stripped, e.g. "example-svc"
	Role      string
	Authority string
	Status    string
	Notes     string
}

// InfraServiceEntityReport summarizes a RegisterInfraServiceEntities run.
type InfraServiceEntityReport struct {
	RegisteredSlugs []string `json:"registered_slugs,omitempty"`
	SkippedSlugs    []string `json:"skipped_slugs,omitempty"`
}

// InfraServiceCapabilityReport summarizes an EnrollInfraServiceCapabilities run.
type InfraServiceCapabilityReport struct {
	NodeIDs           []string `json:"node_ids,omitempty"`
	SourceAuthorityID string   `json:"source_authority_id,omitempty"`
}

// InfraServiceProvenance is the additive per-service detail `authority
// resolve` prints when a resolved entity is a service:* row.
type InfraServiceProvenance struct {
	Role           string
	Authority      string
	Status         string
	SourceRevision string
}

// RegisterInfraServiceEntities reads the installed services.yaml
// (ImportInfraServicesRegistry must already have run) and registers one
// mem.Entity per service: slug "service:<bare>", identifier "<full key>",
// alias "<bare>". Re-running against an unchanged registry is a no-op per
// slug (checked against the currently loaded entities before registering),
// so re-import never appends duplicate rows to ledger/entities.jsonl.
func RegisterInfraServiceEntities(store *mem.Store, homePath string, now time.Time) (InfraServiceEntityReport, error) {
	entries, err := loadInstalledInfraServiceEntries(homePath)
	if err != nil {
		return InfraServiceEntityReport{}, err
	}
	existing, err := store.Entities()
	if err != nil {
		return InfraServiceEntityReport{}, err
	}
	var report InfraServiceEntityReport
	for _, entry := range entries {
		want := mem.Entity{
			Slug:        "service:" + entry.Bare,
			Kind:        InfraServiceEntityKind,
			Aliases:     []string{entry.Bare},
			Identifiers: []string{entry.Key},
			Visibility:  "operational",
		}
		if infraServiceEntityRegistered(existing, want) {
			report.SkippedSlugs = append(report.SkippedSlugs, want.Slug)
			continue
		}
		registered, err := store.RegisterEntity(want, now)
		if err != nil {
			return report, err
		}
		existing = append(existing, registered)
		report.RegisteredSlugs = append(report.RegisteredSlugs, want.Slug)
	}
	return report, nil
}

// infraServiceEntityRegistered reports whether existing already carries an
// entity matching want on slug, kind, aliases, and identifiers (ignoring
// CreatedAt/Visibility bookkeeping) — the additive idempotency guard called
// for by the packet, kept in this package rather than inside
// mem.RegisterEntity.
func infraServiceEntityRegistered(existing []mem.Entity, want mem.Entity) bool {
	for _, e := range existing {
		if e.Slug != want.Slug || e.Kind != want.Kind {
			continue
		}
		if stringSetEqual(e.Aliases, want.Aliases) && stringSetEqual(e.Identifiers, want.Identifiers) {
			return true
		}
	}
	return false
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string{}, a...)
	sb := append([]string{}, b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// EnrollInfraServiceCapabilities reads the installed services.yaml and mints
// one capgraph "service" node per entry (idempotent: capgraph.Store.Add
// already no-ops an unchanged node by ID), then records source-authority
// provenance mirroring the tool-topology import door
// (cmd/compound/capability.go's import-topology case /
// internal/tooltopology.Apply).
func EnrollInfraServiceCapabilities(homePath string, now time.Time) (InfraServiceCapabilityReport, error) {
	entries, err := loadInstalledInfraServiceEntries(homePath)
	if err != nil {
		return InfraServiceCapabilityReport{}, err
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		return InfraServiceCapabilityReport{}, err
	}
	var nodeIDs []string
	for _, entry := range entries {
		node := capgraph.Node{
			ID:       "service:" + entry.Bare,
			Kind:     InfraServiceCapabilityKind,
			Name:     entry.Key,
			Triggers: infraServiceTriggers(entry),
		}
		if err := store.Add(node); err != nil {
			return InfraServiceCapabilityReport{}, err
		}
		nodeIDs = append(nodeIDs, node.ID)
	}
	installedPath := InfraServicesRegistryPath(homePath)
	data, err := os.ReadFile(installedPath)
	if err != nil {
		return InfraServiceCapabilityReport{}, err
	}
	revision := "sha256:" + digestBytes(data)
	auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:       installedPath,
		SourceKind:       "infra_services_registry",
		SourceOwner:      "infra-os",
		SourceClass:      "operational_registry",
		SourceRevision:   revision,
		SourceObservedAt: now.UTC().Format(time.RFC3339),
		ImportedVia:      infraServiceCapabilityImportedVia,
		NodeIDs:          nodeIDs,
		EvidenceRefs:     []string{InfraServicesAuthorityRef, revision},
	}, now)
	if err != nil {
		return InfraServiceCapabilityReport{}, err
	}
	return InfraServiceCapabilityReport{NodeIDs: nodeIDs, SourceAuthorityID: auth.ID}, nil
}

// infraServiceTriggers is the deliberately generic trigger set from the
// packet: bare name + full-key tokens + role tokens + a fixed
// service/deploy/infra/dns set, so a task like "deploy a service to
// <host>" surfaces every enrolled service node via capgraph's exact
// lowercase token-overlap match.
func infraServiceTriggers(entry InfraServiceEntry) []string {
	triggers := []string{entry.Bare, entry.Key, "service", "deploy", "infra", "dns"}
	if entry.Role != "" {
		triggers = append(triggers, entry.Role)
	}
	return dedupeInfraServiceTriggers(triggers)
}

func dedupeInfraServiceTriggers(values []string) []string {
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

// ResolveInfraServiceProvenance looks up the role/authority/status/
// source_revision detail for a resolved service:<bare> entity slug, feeding
// the CLI's additive `authority resolve` output. Returns found=false (no
// error) when slug is not a service:* entity or the bare name has no
// matching installed services.yaml row (e.g. a stale/renamed entry).
func ResolveInfraServiceProvenance(homePath, slug string) (InfraServiceProvenance, bool, error) {
	const prefix = "service:"
	if !strings.HasPrefix(slug, prefix) {
		return InfraServiceProvenance{}, false, nil
	}
	bare := strings.TrimPrefix(slug, prefix)
	if bare == "" {
		return InfraServiceProvenance{}, false, nil
	}
	entries, err := loadInstalledInfraServiceEntries(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return InfraServiceProvenance{}, false, nil
		}
		return InfraServiceProvenance{}, false, err
	}
	var match *InfraServiceEntry
	for i := range entries {
		if entries[i].Bare == bare {
			match = &entries[i]
			break
		}
	}
	if match == nil {
		return InfraServiceProvenance{}, false, nil
	}
	records, err := LoadInfraServicesSourceAuthority(homePath)
	if err != nil {
		return InfraServiceProvenance{}, false, err
	}
	revision := ""
	if len(records) > 0 {
		revision = records[len(records)-1].SourceRevision
	}
	return InfraServiceProvenance{
		Role:           match.Role,
		Authority:      match.Authority,
		Status:         match.Status,
		SourceRevision: revision,
	}, true, nil
}

func loadInstalledInfraServiceEntries(homePath string) ([]InfraServiceEntry, error) {
	data, err := os.ReadFile(InfraServicesRegistryPath(homePath))
	if err != nil {
		return nil, err
	}
	return decodeInfraServiceEntries(data)
}

func decodeInfraServiceEntries(data []byte) ([]InfraServiceEntry, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse services yaml: %w", err)
	}
	keys := make([]string, 0, len(doc.Services))
	for k := range doc.Services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	entries := make([]InfraServiceEntry, 0, len(keys))
	for _, key := range keys {
		fields := doc.Services[key]
		entries = append(entries, InfraServiceEntry{
			Key:       key,
			Bare:      bareServiceName(key),
			Role:      infraServiceStringField(fields, "role"),
			Authority: infraServiceStringField(fields, "authority"),
			Status:    infraServiceStringField(fields, "status"),
			Notes:     infraServiceStringField(fields, "notes"),
		})
	}
	return entries, nil
}

func infraServiceStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

// bareServiceName strips exactly one trailing dotted domain suffix, e.g.
// "example-svc.internal" -> "example-svc". A key with no dot, or a dot only
// at the start/end, is returned unchanged.
func bareServiceName(key string) string {
	key = strings.TrimSpace(key)
	idx := strings.LastIndex(key, ".")
	if idx <= 0 || idx == len(key)-1 {
		return key
	}
	return key[:idx]
}
