package behaviorallens

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigSchemaVersion versions the per-home behavioral-lens config.
const ConfigSchemaVersion = "compound.behavioral_lens_config.v0.1"

// configRelPath is the per-home config file, relative to COMPOUND_HOME. Absent = no operator_private
// lens enabled (a stranger clone is fail-closed by construction — it never wrote this file).
const configRelPath = "config/behavioral-lenses.json"

// signalLedgerRelPath is the append-only signal ledger, relative to COMPOUND_HOME. Every signal —
// public_safe AND operator_private — persists here (COMPOUND_HOME is never the repo). The boundary
// is not WHERE it is written but what may LEAVE the home: see PublicProjection.
const signalLedgerRelPath = "ledger/behavioral-lens-signals.jsonl"

// Config is the per-home opt-in that turns operator_private lenses on. It is the ONLY thing that
// lets a private lens run; a home that never wrote it (a stranger clone) gets public_safe lenses
// only. Enabling is deliberately per-home and explicit — each user instruments their own instance.
type Config struct {
	SchemaVersion        string   `json:"schema_version,omitempty"`
	EnabledPrivateLenses []string `json:"enabled_private_lenses,omitempty"`
}

func (c Config) enabledPrivateSet() map[string]bool {
	out := map[string]bool{}
	for _, id := range c.EnabledPrivateLenses {
		id = strings.TrimSpace(id)
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// LoadConfig reads a home's behavioral-lens config. An ABSENT file is not an error: it returns the
// zero Config (no operator_private lens enabled) — the stranger-clone fail-closed default. A present
// file with a wrong schema version fails loudly.
func LoadConfig(homePath string) (Config, error) {
	if strings.TrimSpace(homePath) == "" {
		return Config{}, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	path := filepath.Join(homePath, filepath.FromSlash(configRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read behavioral-lens config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse behavioral-lens config %s: %w", path, err)
	}
	if v := strings.TrimSpace(cfg.SchemaVersion); v != "" && v != ConfigSchemaVersion {
		return Config{}, fmt.Errorf("behavioral-lens config schema_version=%q, want %q", v, ConfigSchemaVersion)
	}
	return cfg, nil
}

// RecordSignal appends one signal row to the append-only ledger under COMPOUND_HOME. Signals are
// evidence (they accumulate across measurement runs); no uniqueness gate is imposed because a
// subject can legitimately be re-measured and each run is its own row.
func RecordSignal(homePath string, sig InferredSignal) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	if err := sig.Validate(); err != nil {
		return err
	}
	path := filepath.Join(homePath, filepath.FromSlash(signalLedgerRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(&sig)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// LoadSignals reads every recorded signal row (public_safe AND operator_private). A missing ledger
// is a clean nil. This is the OPERATOR-LOCAL reader — a public-emitting surface must go through
// PublicSignals / PublicProjection, never this, or it would leak operator_private values.
func LoadSignals(homePath string) ([]InferredSignal, error) {
	path := filepath.Join(homePath, filepath.FromSlash(signalLedgerRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []InferredSignal
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var sig InferredSignal
		if err := json.Unmarshal([]byte(line), &sig); err != nil {
			return nil, fmt.Errorf("parse behavioral-lens signal line %d: %w", i+1, err)
		}
		out = append(out, sig)
	}
	return out, nil
}

// PublicProjection is THE visibility gate on the way out: it returns ONLY public_safe signals — the
// sole projection any tracked or public-emitting surface (orient, narrate, an exported projection)
// may consume. operator_private signals are excluded BY CONSTRUCTION here, so their value never
// leaves COMPOUND_HOME. Breaking this filter (a scratch mutation that includes private rows) is
// exactly what the visibility-boundary falsifier catches.
func PublicProjection(signals []InferredSignal) []InferredSignal {
	out := make([]InferredSignal, 0, len(signals))
	for _, s := range signals {
		if s.Visibility == VisibilityOperatorPrivate {
			continue
		}
		out = append(out, s)
	}
	return out
}

// PublicSignals is the read-and-project convenience a public-emitting surface uses: load every
// recorded signal, then keep only the public_safe projection. It is the safe front door; LoadSignals
// alone is operator-local.
func PublicSignals(homePath string) ([]InferredSignal, error) {
	all, err := LoadSignals(homePath)
	if err != nil {
		return nil, err
	}
	return PublicProjection(all), nil
}
