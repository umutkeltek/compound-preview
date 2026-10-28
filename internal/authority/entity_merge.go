package authority

import "compound/internal/mem"

// mergeEntity folds an observed identity into the registry: it reuses the
// existing entity whose Slug matches (preserving its other fields) or initializes
// a new operational one, then unions in the given aliases and identifiers. Both
// authority importers (source-control and path-authority) re-implemented this
// identical find-or-init-and-merge; this is the one owner of it.
//
// Scope: this matches by exact slug only — the same tier the importers already
// used. It deliberately does NOT reach for ResolveEntity's wider alias/identifier
// matching or the doctrine's fingerprint/scope tiers; no importer needs that yet,
// and widening the match here would silently merge entities that currently stay
// distinct.
func mergeEntity(existing []mem.Entity, slug, kind string, aliases, identifiers []string) mem.Entity {
	e := mem.Entity{Slug: slug, Kind: kind, Visibility: "operational"}
	for _, ex := range existing {
		if ex.Slug == slug {
			e = ex
			e.Kind = kind
			if e.Visibility == "" {
				e.Visibility = "operational"
			}
			break
		}
	}
	e.Aliases = mergeStrings(e.Aliases, aliases...)
	e.Identifiers = mergeStrings(e.Identifiers, identifiers...)
	return e
}
