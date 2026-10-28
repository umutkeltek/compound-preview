package product

import (
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/mem"
)

// essenceRetractions are the candidates that retract the form.*, learned.*, and
// taste.* facts the ledger currently holds for the bundle's product but its
// declaration no longer emits, with the retracted predicates in sorted order.
//
// A list-valued product fact such as `channels` stops naming a dropped item
// because the re-import promotes a new value into the same slot, superseding
// the old one. These facts are one slot per entry, so a dropped entry has no
// new value to promote; it gets the same supersession with a retired version
// instead. That version goes through the same capture and promotion as every
// other product claim, in the same slot (same entity, predicate, claim type,
// and qualifiers, copied from a declared candidate), so the current view stops
// serving the slot while the ledger keeps the old value as superseded history.
//
// A contract-sourced bundle retracts nothing: a project contract is not the
// authority for forms, learned facts, or taste. A slot under review is hidden
// from the current view and is left for its reviewer.
func essenceRetractions(store *mem.Store, bundle Bundle, declared []mem.Candidate, sourcePath string) ([]mem.Candidate, []string, error) {
	if bundle.contractSourced || len(declared) == 0 {
		return nil, nil, nil
	}
	view, err := store.CurrentView()
	if err != nil {
		return nil, nil, err
	}
	emitted := map[string]bool{}
	for _, c := range declared {
		emitted[c.Predicate] = true
	}
	current := map[string]bool{}
	for _, c := range view.Claims() {
		if c.Entity == bundle.Product.ID && retractable(c.Predicate) && !emitted[c.Predicate] {
			current[c.Predicate] = true
		}
	}
	predicates := make([]string, 0, len(current))
	for predicate := range current {
		predicates = append(predicates, predicate)
	}
	sort.Strings(predicates)
	object := "retracted: not declared by product-registry:" + filepath.Base(sourcePath)
	template := declared[0]
	retractions := make([]mem.Candidate, 0, len(predicates))
	for _, predicate := range predicates {
		c := template
		c.Predicate = predicate
		c.Object = object
		c.SnippetHash = stableID("prod_snip_", bundle.Product.ID, predicate, object)
		c.Visibility = "retired"
		retractions = append(retractions, c)
	}
	return retractions, predicates, nil
}

// retractable reports whether predicate is a per-entry essence fact that a
// declaration retracts by omission. essence.* is recorded field by field
// whenever an essence is declared, so it has no entries to drop.
func retractable(predicate string) bool {
	return strings.HasPrefix(predicate, "form.") || strings.HasPrefix(predicate, "learned.") || strings.HasPrefix(predicate, "taste.")
}
