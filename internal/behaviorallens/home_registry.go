package behaviorallens

// operatorPrivateLenses are the operator_private lenses a home MAY opt into. They are deliberately
// NOT in DefaultRegistry (which ships public_safe-only, so a stranger clone never even carries the
// TYPE in a shipped-default path). RegistryForHome layers them on top so an opted-in home can
// list/run/compose them.
func operatorPrivateLenses() []Lens {
	return []Lens{
		AgreementAsymmetryLens{},
		ActivityUnaccountedLens{},
	}
}

// RegistryForHome returns the registry a given home should use: the shipped public_safe lenses
// (DefaultRegistry) PLUS the known operator_private lenses. The private lenses are registered
// UNCONDITIONALLY — Compose's existing cfg gate (enabledPrivateSet) is the single place that decides
// whether each actually RUNS for this home (default off; a stranger clone with no config runs none,
// fail-closed by construction). Registering the TYPE even when disabled is what lets
// `behavioral-lens list` show the lens exists (as off) without ever exposing its VALUE, and lets a
// single-lens `run` fail with a clear "operator_private and not enabled" message rather than an
// opaque "no registered lens".
//
// cfg is the home's opt-in config; the visibility gate lives in Compose (and the CLI run guard), so
// this function intentionally does not filter the registered set by it — filtering here would hide
// the lens type from an opted-out home and duplicate the gate that Compose already owns.
func RegistryForHome(cfg Config) *Registry {
	_ = cfg
	r := DefaultRegistry()
	for _, l := range operatorPrivateLenses() {
		if err := r.Register(l); err != nil {
			panic("behaviorallens: home registry registration failed: " + err.Error())
		}
	}
	return r
}
