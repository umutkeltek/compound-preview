package activity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

// newTestHome scaffolds a real COMPOUND_HOME under t.TempDir() (never the operator's ~/.compound).
func newTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := home.Init(dir, "", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("home.Init: %v", err)
	}
	return dir
}

// grantSpec is one grant to import (ImportStandingGrants REPLACES the registry, so every grant a test
// needs must be imported in ONE source — importGrants writes them together).
type grantSpec struct {
	id, capability, expires string
}

// importGrants writes a single standing-grants source covering every spec and imports it. expires
// empty = no expiry.
func importGrants(t *testing.T, homePath string, now time.Time, specs ...grantSpec) {
	t.Helper()
	var grants []string
	for _, s := range specs {
		grants = append(grants, `{"id":"`+s.id+`","capability":"`+s.capability+
			`","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"`+expiresField(s.expires)+`}`)
	}
	src := `{"schema_version":"compound.standing_grants_source.v0.1",` +
		`"authority":{"decision_ref":"grant:test-toggle","reviewed_by":"operator:test"},` +
		`"standing_grants":[` + strings.Join(grants, ",") + `]}`
	srcPath := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, srcPath, now); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
}

// importGrant is the single-grant convenience.
func importGrant(t *testing.T, homePath, id, capability, expires string, now time.Time) {
	t.Helper()
	importGrants(t, homePath, now, grantSpec{id, capability, expires})
}

func expiresField(expires string) string {
	if expires == "" {
		return ""
	}
	return `,"expires_at":"` + expires + `"`
}

func loadCfg(t *testing.T, homePath string) policy.Config {
	t.Helper()
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatalf("policy.LoadConfig: %v", err)
	}
	return cfg
}

// TestFalsifier9_EnableWithoutGrantRefused: enable with no matching standing grant is refused; the
// config key is left unchanged (still disabled), and no config file is created by the refused pass.
func TestFalsifier9_EnableWithoutGrantRefused(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := loadCfg(t, h) // no grants imported

	res, err := SetEnabled(h, cfg, "work_test", true, now)
	if err != nil {
		t.Fatalf("SetEnabled err: %v", err)
	}
	if !res.Refused || res.Applied {
		t.Fatalf("enable without grant must be refused, got %+v", res)
	}
	// Config key unchanged: still disabled.
	if en, _ := Enabled(h); en {
		t.Fatal("sensor must remain disabled after a refused enable")
	}
	// The refused pass must not have materialized the config file.
	if _, statErr := os.Stat(ConfigPath(h)); statErr == nil {
		t.Fatal("refused enable must not create the config file")
	}
}

// TestFalsifier9_EnableWithGrantFlipsKey: enable WITH a matching grant flips the key to enabled,
// appends an action record, and emits a Receipt+Transition via act.Execute.
func TestFalsifier9_EnableWithGrantFlipsKey(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	importGrant(t, h, "grant.activity_sensor", ToggleCapability, "", now)
	cfg := loadCfg(t, h)

	res, err := SetEnabled(h, cfg, "work_test", true, now)
	if err != nil {
		t.Fatalf("SetEnabled err: %v", err)
	}
	if res.Refused || !res.Applied {
		t.Fatalf("enable with grant must apply, got %+v", res)
	}
	if en, _ := Enabled(h); !en {
		t.Fatal("sensor must be enabled after a granted enable")
	}
	if res.ReceiptID == "" || res.TransitionID == "" {
		t.Fatalf("granted toggle must emit Receipt+Transition, got %+v", res)
	}
	// Action ledger row appended.
	data, err := os.ReadFile(ActionLedgerPath(h))
	if err != nil || len(data) == 0 {
		t.Fatalf("action ledger must have a row: err=%v len=%d", err, len(data))
	}

	// And disable flips it back, still governed by the same capability grant.
	res2, err := SetEnabled(h, cfg, "work_test2", false, now)
	if err != nil || !res2.Applied {
		t.Fatalf("disable with grant must apply: res=%+v err=%v", res2, err)
	}
	if en, _ := Enabled(h); en {
		t.Fatal("sensor must be disabled after a granted disable")
	}
}

// TestToggleGrantIsCapabilityExact: a grant for the DRAIN capability must NOT authorize the toggle.
func TestToggleGrantIsCapabilityExact(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	importGrant(t, h, "grant.activity_drain", DrainCapability, "", now) // wrong capability
	cfg := loadCfg(t, h)

	res, err := SetEnabled(h, cfg, "work_test", true, now)
	if err != nil {
		t.Fatalf("SetEnabled err: %v", err)
	}
	if !res.Refused {
		t.Fatalf("a drain-capability grant must not authorize toggle, got %+v", res)
	}
}
