// Package behaviorallens is a COMPOSABLE behavioral-lens framework: an evidence-class organ that
// gives behavioral detectors ("does this lane/output/interaction bend behavior in a way we should
// see, not trust away") ONE uniform plug — observe -> InferredSignal — plus a registry and
// visibility-gated composition so lenses snap together like lego. Sycophancy (paired-stance shift)
// is the first lens (sycophancy.go).
//
// Two invariants, both load-bearing:
//   - EVIDENCE ONLY. A lens records an InferredSignal and NEVER blocks, NEVER promotes, NEVER
//     changes a route/role. Turning a signal into behavior is the DEFERRED governed modulator (an
//     enforcement-class consumer minted only once a second lens exists — two-consumers law).
//   - VISIBILITY BOUNDARY. Each lens declares a Visibility. public_safe lenses infer lane/model/
//     OUTPUT behavior and ship in the generic product. operator_private lenses may infer the
//     operator's REAL interactions but are per-home config, DEFAULT OFF, fail-closed for a stranger
//     clone, and their signal is written ONLY under COMPOUND_HOME and NEVER emitted into any tracked
//     or public-emitting surface (see PublicProjection in store.go). Each user instruments only their
//     own instance.
//
// State (recorded signals) lives under COMPOUND_HOME, never the repo tree. No provider/network call
// occurs in any tested path — real-lane probes are captured out of band and fed back as data.
package behaviorallens

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion strings for the framework's persisted/exchanged shapes.
const (
	ObservationSchemaVersion = "compound.behavioral_lens_observation.v0.1"
	SignalSchemaVersion      = "compound.behavioral_lens_signal.v0.1"
)

// Visibility is a lens's declared exposure class — the framework-level gate that decides whether a
// lens runs for a given home and whether its signal may leave COMPOUND_HOME.
type Visibility string

const (
	// VisibilityPublicSafe — infers lane/model/OUTPUT behavior only; ships in the generic product and
	// may be emitted into operator-facing surfaces (through PublicProjection).
	VisibilityPublicSafe Visibility = "public_safe"
	// VisibilityOperatorPrivate — may infer the operator's real interactions; per-home opt-in, default
	// off, fail-closed for a stranger clone, and its signal never leaves COMPOUND_HOME.
	VisibilityOperatorPrivate Visibility = "operator_private"
)

// Valid reports whether v is a recognized visibility class.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPublicSafe, VisibilityOperatorPrivate:
		return true
	default:
		return false
	}
}

// ErrNotApplicable is what a lens returns when an Observation carries nothing it can infer from.
// Compose treats it as a skip (a lens never blocks the batch); a single-lens run surfaces it as an
// error because the caller named that lens explicitly.
var ErrNotApplicable = errors.New("behavioral lens: observation not applicable")

// Observation is the uniform input to any lens: a subject the signal is about (a lane / output /
// run id) plus a lens-specific payload the lens decodes. One Observation can feed N lenses.
type Observation struct {
	SchemaVersion string            `json:"schema_version,omitempty"`
	ID            string            `json:"id,omitempty"`
	Subject       string            `json:"subject"`
	Kind          string            `json:"kind,omitempty"`
	Payload       json.RawMessage   `json:"payload,omitempty"`
	EvidenceRefs  []string          `json:"evidence_refs,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// InferredSignal is the uniform output of any lens — evidence, never a gate. Value is the scalar the
// lens measured (e.g. sycophancy mean stance shift); Confidence is advisory sample strength;
// Visibility is copied from the lens so downstream projection can enforce the boundary on the row
// itself.
type InferredSignal struct {
	SchemaVersion string     `json:"schema_version"`
	ID            string     `json:"id"`
	LensID        string     `json:"lens_id"`
	Subject       string     `json:"subject"`
	Value         float64    `json:"value"`
	Confidence    float64    `json:"confidence"`
	Visibility    Visibility `json:"visibility"`
	Method        string     `json:"method,omitempty"`
	Detail        string     `json:"detail,omitempty"`
	EvidenceRefs  []string   `json:"evidence_refs,omitempty"`
	CreatedAt     string     `json:"created_at"`
}

// Validate checks the invariants a persisted signal must hold.
func (s InferredSignal) Validate() error {
	var errs []string
	if strings.TrimSpace(s.SchemaVersion) == "" {
		errs = append(errs, "schema_version required")
	} else if s.SchemaVersion != SignalSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", s.SchemaVersion, SignalSchemaVersion))
	}
	if strings.TrimSpace(s.ID) == "" {
		errs = append(errs, "id required")
	}
	if strings.TrimSpace(s.LensID) == "" {
		errs = append(errs, "lens_id required")
	}
	if strings.TrimSpace(s.Subject) == "" {
		errs = append(errs, "subject required")
	}
	if !s.Visibility.Valid() {
		errs = append(errs, fmt.Sprintf("visibility=%q is not recognized", s.Visibility))
	}
	if strings.TrimSpace(s.CreatedAt) == "" {
		errs = append(errs, "created_at required")
	} else if _, err := time.Parse(time.RFC3339, s.CreatedAt); err != nil {
		errs = append(errs, fmt.Sprintf("created_at invalid: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("inferred signal invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Lens is the uniform plug every behavioral detector implements. Infer is deterministic and takes an
// injected clock; it makes no provider/network call in any tested path. A lens NEVER blocks and
// NEVER promotes — it only returns evidence.
type Lens interface {
	ID() string
	Visibility() Visibility
	Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error)
}

// Registry holds the lenses that snap together. Registration order is preserved so composition is
// deterministic regardless of map iteration.
type Registry struct {
	lenses map[string]Lens
	order  []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{lenses: map[string]Lens{}}
}

// Register adds a lens. A duplicate id or a blank/invalid-visibility lens is refused loudly so the
// composition set stays well-formed.
func (r *Registry) Register(l Lens) error {
	if l == nil {
		return fmt.Errorf("register: nil lens")
	}
	id := strings.TrimSpace(l.ID())
	if id == "" {
		return fmt.Errorf("register: lens id required")
	}
	if !l.Visibility().Valid() {
		return fmt.Errorf("register lens %q: visibility %q is not recognized", id, l.Visibility())
	}
	if _, dup := r.lenses[id]; dup {
		return fmt.Errorf("register lens %q: already registered", id)
	}
	r.lenses[id] = l
	r.order = append(r.order, id)
	return nil
}

// Lens returns a registered lens by id.
func (r *Registry) Lens(id string) (Lens, bool) {
	l, ok := r.lenses[strings.TrimSpace(id)]
	return l, ok
}

// IDs returns the registered lens ids in registration order.
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// SkipNote records a lens that did not contribute a signal to a Compose run, and why — so the gate
// (visibility off / not applicable) is legible rather than silent.
type SkipNote struct {
	LensID     string     `json:"lens_id"`
	Visibility Visibility `json:"visibility"`
	Reason     string     `json:"reason"`
}

// ComposeResult is the combined output of applying every registered lens to one Observation.
type ComposeResult struct {
	Signals []InferredSignal `json:"signals"`
	Skipped []SkipNote       `json:"skipped,omitempty"`
}

// Compose applies every registered lens to one Observation, visibility-gated by the home config:
//   - public_safe lenses always run.
//   - operator_private lenses run ONLY if cfg explicitly enables them (default off; a stranger clone
//     with no config runs none — fail-closed).
//
// A lens returning ErrNotApplicable is a skip, not a failure (a lens never blocks the batch); any
// other lens error aborts, since it is a real bug rather than an inapplicable observation. Signals
// come out in deterministic registration order.
func (r *Registry) Compose(ctx context.Context, obs Observation, cfg Config, now time.Time) (ComposeResult, error) {
	enabled := cfg.enabledPrivateSet()
	var res ComposeResult
	for _, id := range r.order {
		l := r.lenses[id]
		if l.Visibility() == VisibilityOperatorPrivate && !enabled[id] {
			res.Skipped = append(res.Skipped, SkipNote{
				LensID:     id,
				Visibility: l.Visibility(),
				Reason:     "operator_private lens not enabled in this home config (default off; stranger clone fail-closed)",
			})
			continue
		}
		sig, err := l.Infer(ctx, obs, now)
		if err != nil {
			if errors.Is(err, ErrNotApplicable) {
				res.Skipped = append(res.Skipped, SkipNote{
					LensID:     id,
					Visibility: l.Visibility(),
					Reason:     "observation not applicable to lens",
				})
				continue
			}
			return ComposeResult{}, fmt.Errorf("lens %q infer: %w", id, err)
		}
		res.Signals = append(res.Signals, sig)
	}
	return res, nil
}

// newSignal builds a validated InferredSignal with a deterministic id derived from its identity
// fields. Lenses call this so every signal shares one shape and id scheme.
func newSignal(lensID, subject string, value, confidence float64, vis Visibility, method, detail string, evidenceRefs []string, now time.Time) (InferredSignal, error) {
	created := now.UTC().Format(time.RFC3339)
	s := InferredSignal{
		SchemaVersion: SignalSchemaVersion,
		LensID:        strings.TrimSpace(lensID),
		Subject:       strings.TrimSpace(subject),
		Value:         value,
		Confidence:    confidence,
		Visibility:    vis,
		Method:        strings.TrimSpace(method),
		Detail:        strings.TrimSpace(detail),
		EvidenceRefs:  dedupeRefs(evidenceRefs),
		CreatedAt:     created,
	}
	s.ID = idFor("blsig_", s.LensID, s.Subject, s.Method, created, fmt.Sprintf("%.6f", value))
	if err := s.Validate(); err != nil {
		return InferredSignal{}, err
	}
	return s, nil
}

func idFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func dedupeRefs(refs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
