package terminal

import (
	"sort"
	"sync"
)

// Registry maps terminal name → Terminal impl. Names are the user-facing
// CLI keys (e.g. "cmux", "iterm2").
//
// Registration model: subpackages call Register(t) from their init() to
// populate the default registry. The CLI (cmd/compound/terminal_cli.go)
// blank-imports each subpackage to ensure init() runs before NewRegistry
// is called. This avoids the import cycle that would result from a parent
// package listing child constructors explicitly.
//
// Tests can build a custom registry via FromImpls without touching init.
type Registry struct {
	byName map[string]Terminal
}

// default is the package-level registry populated by subpackage init()s.
// mu guards registration order during tests that race.
var (
	defaultReg = &Registry{byName: map[string]Terminal{}}
	regMu      sync.RWMutex
)

// Register adds t to the default registry. Called from subpackage init()
// functions. Last-registered wins on duplicate names.
func Register(t Terminal) {
	if t == nil {
		return
	}
	regMu.Lock()
	defaultReg.byName[t.Name()] = t
	regMu.Unlock()
}

// NewRegistry returns the default registry. The returned value is a fresh
// snapshot — subsequent Register calls do not mutate it.
func NewRegistry() *Registry {
	regMu.RLock()
	out := &Registry{byName: make(map[string]Terminal, len(defaultReg.byName))}
	for k, v := range defaultReg.byName {
		out.byName[k] = v
	}
	regMu.RUnlock()
	return out
}

// FromImpls builds a one-off registry from a list. Useful for tests and
// for callers that want to register a custom set without touching the
// default registry.
func FromImpls(impls ...Terminal) *Registry {
	r := &Registry{byName: make(map[string]Terminal, len(impls))}
	for _, t := range impls {
		if t == nil {
			continue
		}
		r.byName[t.Name()] = t
	}
	return r
}

// Get returns the Terminal registered under name, or ok=false.
func (r *Registry) Get(name string) (Terminal, bool) {
	if r == nil || name == "" {
		return nil, false
	}
	t, ok := r.byName[name]
	return t, ok
}

// Names returns the sorted list of registered terminal names. Used by the
// CLI for `compound terminal` (no args) listing.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for k := range r.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Has reports whether name is registered. Convenience over Get.
func (r *Registry) Has(name string) bool {
	_, ok := r.Get(name)
	return ok
}
