package behaviorallens

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fixedNow is an injected clock so signal ids/timestamps are deterministic in tests.
var fixedNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// stubLens is a synthetic lens used only in tests to prove the framework treats lenses as
// interchangeable plugs. It always applies (independent of any other lens's payload), so adding or
// removing it exercises composition without touching any real lens's code.
type stubLens struct {
	id    string
	vis   Visibility
	value float64
}

func (s stubLens) ID() string             { return s.id }
func (s stubLens) Visibility() Visibility { return s.vis }
func (s stubLens) Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error) {
	return newSignal(s.id, obs.Subject, s.value, 1, s.vis, "stub", "synthetic test lens", nil, now)
}

func sycophancyObs(t *testing.T, subject string, cases []SycophancyCase) Observation {
	t.Helper()
	payload, err := json.Marshal(SycophancyPayload{Cases: cases})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return Observation{SchemaVersion: ObservationSchemaVersion, Subject: subject, Payload: payload}
}

func TestRegistryRefusesDuplicateAndBlankLens(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(SycophancyLens{}); err != nil {
		t.Fatalf("register sycophancy: %v", err)
	}
	if err := r.Register(SycophancyLens{}); err == nil {
		t.Fatal("duplicate lens id must be refused")
	}
	if err := r.Register(stubLens{id: "", vis: VisibilityPublicSafe}); err == nil {
		t.Fatal("blank lens id must be refused")
	}
	if err := r.Register(stubLens{id: "bad-vis", vis: "sideways"}); err == nil {
		t.Fatal("unrecognized visibility must be refused")
	}
}

// FALSIFIER 3 — composition is real lego: >=2 lens instances over ONE observation both run and
// combine deterministically, and adding/removing a lens changes the combined output WITHOUT touching
// any other lens's code (the stub lens is defined independently of the sycophancy lens).
func TestCompositionIsRealLego(t *testing.T) {
	obs := sycophancyObs(t, "lane:probe", []SycophancyCase{
		{ArtifactID: "a1", SupportiveEndorsement: 0.9, CriticalEndorsement: 0.2},
		{ArtifactID: "a2", SupportiveEndorsement: 0.8, CriticalEndorsement: 0.3},
	})

	two := NewRegistry()
	mustRegister(t, two, SycophancyLens{})
	mustRegister(t, two, stubLens{id: "verdict-volatility", vis: VisibilityPublicSafe, value: 0.42})

	res, err := two.Compose(context.Background(), obs, Config{}, fixedNow)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(res.Signals) != 2 {
		t.Fatalf("both lenses must run over one observation: got %d signals", len(res.Signals))
	}
	// deterministic registration order.
	if res.Signals[0].LensID != SycophancyLensID || res.Signals[1].LensID != "verdict-volatility" {
		t.Fatalf("signals not in registration order: %s, %s", res.Signals[0].LensID, res.Signals[1].LensID)
	}

	// removing a lens changes the combined output; the remaining lens's code is untouched.
	one := NewRegistry()
	mustRegister(t, one, SycophancyLens{})
	resOne, err := one.Compose(context.Background(), obs, Config{}, fixedNow)
	if err != nil {
		t.Fatalf("compose one: %v", err)
	}
	if len(resOne.Signals) != 1 {
		t.Fatalf("removing a lens must change the combined output: got %d signals", len(resOne.Signals))
	}
	// the surviving sycophancy signal is byte-identical across the two compositions — one lens does
	// not depend on the presence of another.
	if signalJSON(t, resOne.Signals[0]) != signalJSON(t, res.Signals[0]) {
		t.Fatal("a lens's signal must not change when another lens is added/removed")
	}
}

// A lens whose observation carries nothing it can infer from is SKIPPED, not fatal — a lens never
// blocks the batch.
func TestComposeSkipsNotApplicableWithoutFailing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, SycophancyLens{})                                            // needs cases
	mustRegister(t, r, stubLens{id: "always", vis: VisibilityPublicSafe, value: 1}) // always applies
	obs := Observation{SchemaVersion: ObservationSchemaVersion, Subject: "lane:x"}  // no payload
	res, err := r.Compose(context.Background(), obs, Config{}, fixedNow)
	if err != nil {
		t.Fatalf("compose must not fail on a not-applicable lens: %v", err)
	}
	if len(res.Signals) != 1 || res.Signals[0].LensID != "always" {
		t.Fatalf("only the applicable lens should produce a signal, got %d", len(res.Signals))
	}
	if len(res.Skipped) != 1 || res.Skipped[0].LensID != SycophancyLensID {
		t.Fatalf("the not-applicable lens should be a legible skip, got %+v", res.Skipped)
	}
}

// FALSIFIER 2 (part A) — an operator_private lens is OFF BY DEFAULT and a stranger clone (no config)
// runs only public_safe lenses; the private lens emits NOTHING.
func TestVisibilityBoundaryStrangerRunsOnlyPublicSafe(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, stubLens{id: "pub", vis: VisibilityPublicSafe, value: 0.1})
	mustRegister(t, r, stubLens{id: "receipt-asymmetry", vis: VisibilityOperatorPrivate, value: 0.99})

	// stranger clone: zero config (the fail-closed default).
	res, err := r.Compose(context.Background(), Observation{Subject: "lane:x"}, Config{}, fixedNow)
	if err != nil {
		t.Fatalf("compose stranger: %v", err)
	}
	for _, s := range res.Signals {
		if s.Visibility == VisibilityOperatorPrivate {
			t.Fatalf("stranger clone must not run an operator_private lens; got signal from %s", s.LensID)
		}
	}
	if len(res.Signals) != 1 || res.Signals[0].LensID != "pub" {
		t.Fatalf("stranger should get only the public_safe signal, got %+v", res.Signals)
	}
	// the private lens is a legible, gated skip.
	if len(res.Skipped) != 1 || res.Skipped[0].LensID != "receipt-asymmetry" {
		t.Fatalf("private lens should be a gated skip for a stranger, got %+v", res.Skipped)
	}
}

// FALSIFIER 2 (part B) — when the home config explicitly enables the operator_private lens, it runs;
// its signal is recorded under COMPOUND_HOME BUT is excluded from the public projection, so its value
// never reaches a public-emitting surface.
func TestVisibilityBoundaryEnabledPrivateStaysOutOfPublicProjection(t *testing.T) {
	const privateSentinel = 0.987654 // a value a public surface must never carry.
	r := NewRegistry()
	mustRegister(t, r, stubLens{id: "pub", vis: VisibilityPublicSafe, value: 0.1})
	mustRegister(t, r, stubLens{id: "receipt-asymmetry", vis: VisibilityOperatorPrivate, value: privateSentinel})

	cfg := Config{SchemaVersion: ConfigSchemaVersion, EnabledPrivateLenses: []string{"receipt-asymmetry"}}
	res, err := r.Compose(context.Background(), Observation{Subject: "lane:x"}, cfg, fixedNow)
	if err != nil {
		t.Fatalf("compose enabled: %v", err)
	}
	if len(res.Signals) != 2 {
		t.Fatalf("enabled home should run both lenses, got %d signals", len(res.Signals))
	}

	home := t.TempDir()
	for _, s := range res.Signals {
		if err := RecordSignal(home, s); err != nil {
			t.Fatalf("record signal: %v", err)
		}
	}

	// operator-local reader sees everything (both signals live under COMPOUND_HOME).
	all, err := LoadSignals(home)
	if err != nil {
		t.Fatalf("load signals: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("both signals must persist under COMPOUND_HOME, got %d", len(all))
	}

	// THE BOUNDARY: the public projection carries ONLY public_safe rows; the private value is gone.
	pub, err := PublicSignals(home)
	if err != nil {
		t.Fatalf("public signals: %v", err)
	}
	if len(pub) != 1 || pub[0].LensID != "pub" {
		t.Fatalf("public projection must contain only the public_safe signal, got %+v", pub)
	}
	for _, s := range pub {
		if s.Visibility == VisibilityOperatorPrivate {
			t.Fatalf("operator_private signal leaked into the public projection: %s", s.LensID)
		}
	}
	// a byte scan of the marshaled public projection (what orient/narrate/an export would emit) must
	// not contain the private sentinel value anywhere.
	blob, err := json.Marshal(pub)
	if err != nil {
		t.Fatalf("marshal public projection: %v", err)
	}
	if strings.Contains(string(blob), "0.987654") {
		t.Fatalf("private sentinel value leaked into a public-emitting surface: %s", blob)
	}
}

// LoadConfig on a home that never wrote a config is the stranger-clone fail-closed default: no error,
// no operator_private lens enabled.
func TestLoadConfigAbsentIsFailClosed(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("absent config must not error: %v", err)
	}
	if len(cfg.enabledPrivateSet()) != 0 {
		t.Fatal("absent config must enable no operator_private lens")
	}
}

func mustRegister(t *testing.T, r *Registry, l Lens) {
	t.Helper()
	if err := r.Register(l); err != nil {
		t.Fatalf("register %s: %v", l.ID(), err)
	}
}

func signalJSON(t *testing.T, s InferredSignal) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal signal: %v", err)
	}
	return string(b)
}
