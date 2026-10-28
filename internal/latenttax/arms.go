package latenttax

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// The four factorial arm kinds. Every dose carries exactly these four, so each
// task is measured under each placement of the obligation, plus the length-matched
// neutral control that is the whole rig's null hypothesis.
const (
	// ArmObligationInContext — the obligation sits in context as standing text; no
	// deterministic gate. Tests whether in-context guidance alone shapes behavior.
	ArmObligationInContext = "obligation_in_context"
	// ArmGateOnly — the obligation is enforced ONLY by a PolicyDecision gate at the
	// action boundary; the rule is REMOVED from context. Tests the reference-monitor
	// placement and the "evict it from context" corollary.
	ArmGateOnly = "gate_only"
	// ArmBoth — obligation in context AND a PolicyDecision gate. The layered placement
	// the synthesis amended the binary claim toward.
	ArmBoth = "both"
	// ArmFiller — LENGTH-MATCHED neutral filler in context, no obligation, no gate. The
	// null control: if this degrades/costs as much as the obligation arms, the latent-tax
	// framing collapses to "keep contexts short".
	ArmFiller = "filler"
)

// armKinds is the required set present at every dose.
var armKinds = []string{ArmObligationInContext, ArmGateOnly, ArmBoth, ArmFiller}

// DefaultToleranceFrac is the length-match tolerance used when a config declares 0
// AND the config opts into the default (see Validate). Kept small: the whole point
// of the filler arm is that it matches the instruction arm's token length closely.
const DefaultToleranceFrac = 0.05

// Arm is one factorial cell: the in-context block (obligations or neutral filler,
// empty for gate-only), its declared token length (which MUST equal the deterministic
// token count of the block), and whether the PolicyDecision gate is active.
type Arm struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ContextBlock string `json:"context_block"`
	TokenLength  int    `json:"token_length"`
	GateEnabled  bool   `json:"gate_enabled"`
}

// DoseArms is the four-arm factorial at one instruction dose (0 = baseline; higher =
// more standing instructions in the instruction arms, with the filler arm matched).
type DoseArms struct {
	Dose int   `json:"dose"`
	Arms []Arm `json:"arms"`
}

// ArmConfig is the versioned factorial arm definition. ToleranceFrac is the fraction
// by which a filler arm's token length may diverge from its matched instruction arm
// before the config is a deterministic error.
type ArmConfig struct {
	SchemaVersion string     `json:"schema_version"`
	ConfigID      string     `json:"config_id"`
	ToleranceFrac float64    `json:"tolerance_frac"`
	Doses         []DoseArms `json:"doses"`
}

// LoadArmConfig reads and validates an arm-config JSON file.
func LoadArmConfig(path string) (ArmConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ArmConfig{}, err
	}
	var ac ArmConfig
	if err := json.Unmarshal(data, &ac); err != nil {
		return ArmConfig{}, fmt.Errorf("parse arm config %s: %w", path, err)
	}
	if err := ac.Validate(); err != nil {
		return ArmConfig{}, err
	}
	return ac, nil
}

// tolerance returns the effective length-match tolerance fraction.
func (ac ArmConfig) tolerance() float64 {
	if ac.ToleranceFrac <= 0 {
		return DefaultToleranceFrac
	}
	return ac.ToleranceFrac
}

// ArmByID resolves an arm and its dose by the arm's id (arm ids are globally unique
// across the config). The second return is false if the id is unknown.
func (ac ArmConfig) ArmByID(id string) (Arm, int, bool) {
	for _, d := range ac.Doses {
		for _, a := range d.Arms {
			if a.ID == id {
				return a, d.Dose, true
			}
		}
	}
	return Arm{}, 0, false
}

// Validate enforces the arm invariants, headlined by the DOSE-MATCHING gate: within
// each dose the filler arm's token length must be within tolerance of the instruction
// arm, and both.token_length must equal the obligation_in_context token length (same
// instructions). A divergent filler arm is a deterministic config error — that is how
// "context length wearing an instruction costume" is caught at load, not in analysis.
func (ac ArmConfig) Validate() error {
	var errs []string
	if ac.SchemaVersion != ArmConfigSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", ac.SchemaVersion, ArmConfigSchemaVersion))
	}
	if strings.TrimSpace(ac.ConfigID) == "" {
		errs = append(errs, "config_id is required")
	}
	if ac.ToleranceFrac < 0 || ac.ToleranceFrac >= 1 {
		errs = append(errs, fmt.Sprintf("tolerance_frac=%g must be in [0,1)", ac.ToleranceFrac))
	}
	if len(ac.Doses) == 0 {
		errs = append(errs, "arm config has no doses")
	}

	globalIDs := map[string]bool{}
	seenDose := map[int]bool{}
	tol := ac.tolerance()
	for _, d := range ac.Doses {
		where := fmt.Sprintf("dose %d", d.Dose)
		if seenDose[d.Dose] {
			errs = append(errs, where+": duplicate dose")
		}
		seenDose[d.Dose] = true

		byKind := map[string]Arm{}
		for _, a := range d.Arms {
			aw := where + " arm " + a.ID
			if strings.TrimSpace(a.ID) == "" {
				errs = append(errs, where+": arm has no id")
				continue
			}
			if globalIDs[a.ID] {
				errs = append(errs, aw+": duplicate arm id (arm ids must be globally unique)")
			}
			globalIDs[a.ID] = true

			if !oneOfStr(a.Kind, armKinds) {
				errs = append(errs, fmt.Sprintf("%s: kind=%q not one of %v", aw, a.Kind, armKinds))
				continue
			}
			if _, dup := byKind[a.Kind]; dup {
				errs = append(errs, fmt.Sprintf("%s: kind %q appears more than once in this dose", aw, a.Kind))
			}
			byKind[a.Kind] = a

			// The declared token length is the dose-matching currency; it must equal the
			// deterministic token count of the block or the whole match is a lie.
			if got := countTokens(a.ContextBlock); got != a.TokenLength {
				errs = append(errs, fmt.Sprintf("%s: token_length=%d != counted %d for context_block", aw, a.TokenLength, got))
			}
			switch a.Kind {
			case ArmGateOnly:
				if strings.TrimSpace(a.ContextBlock) != "" || a.TokenLength != 0 {
					errs = append(errs, aw+": gate_only arm must have an empty context_block (the rule is removed from context)")
				}
				if !a.GateEnabled {
					errs = append(errs, aw+": gate_only arm must set gate_enabled=true")
				}
			case ArmBoth:
				if !a.GateEnabled {
					errs = append(errs, aw+": both arm must set gate_enabled=true")
				}
			case ArmObligationInContext, ArmFiller:
				if a.GateEnabled {
					errs = append(errs, fmt.Sprintf("%s: %s arm must set gate_enabled=false", aw, a.Kind))
				}
			}
		}

		// Every dose must carry the full factorial.
		for _, k := range armKinds {
			if _, ok := byKind[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing required arm kind %q (each dose needs all four arms)", where, k))
			}
		}

		// DOSE-MATCHING gate.
		instr, hasInstr := byKind[ArmObligationInContext]
		both, hasBoth := byKind[ArmBoth]
		filler, hasFiller := byKind[ArmFiller]
		if hasInstr && hasBoth && both.TokenLength != instr.TokenLength {
			errs = append(errs, fmt.Sprintf("%s: both arm token_length=%d must equal obligation_in_context token_length=%d (same instructions)", where, both.TokenLength, instr.TokenLength))
		}
		if hasInstr && hasFiller {
			if !withinTolerance(filler.TokenLength, instr.TokenLength, tol) {
				errs = append(errs, fmt.Sprintf("%s: filler arm token_length=%d diverges from obligation_in_context token_length=%d beyond tolerance %.3g — a length-matched control must match the instruction dose", where, filler.TokenLength, instr.TokenLength, tol))
			}
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("arm config invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// withinTolerance reports whether got is within frac of want. When want is 0 (the
// dose-0 baseline where instruction arms are empty), an exact 0 match is required.
func withinTolerance(got, want int, frac float64) bool {
	if want == 0 {
		return got == 0
	}
	return math.Abs(float64(got-want)) <= frac*float64(want)
}

func oneOfStr(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}
