// account_projection.go — the account-admission gate (projections/account-projection.json).
//
// Ratified placement B (2026-07-12): switchback owns the provider-account authority (enrollment,
// aliases, credentials, capacity windows). compound imports a REDUCED, identity-free projection of
// switchback's loopback-only `GET /admin/accounts` response (schema `switchback/admin-accounts@1`)
// and uses it in PlanCapabilityRoute for lane-level ADMISSION only — never account selection, never
// account identity. `route import-account-projection` normalizes the input and writes the
// projection atomically; the normalizer strips every account id, alias, and email — only
// provider-grain counts and timestamps survive. That stripping IS the feature (privacy law: this
// repo is destined public).
//
// Capacity admission consumes this projection at account grain. A provider with zero healthy
// accounts is DEAD and fails closed; it is never resurrected into a route merely to keep the
// chain non-empty.
//
// Like the held patch's capacity snapshot, this is a PROJECTION, not a governed registry: it is
// rebuilt/overwritten wholesale on every import — no append-only evidence log.
package routeevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	homepkg "compound/internal/home"
)

const (
	// AccountProjectionSchema versions compound's normalized projection body.
	AccountProjectionSchema = "compound.account_projection.v0.1"

	// adminAccountsSchema is the switchback source schema this importer accepts (see
	// switchback/crates/sb-server/src/handlers/admin.rs `accounts` handler — read-only evidence,
	// not edited by this repo).
	adminAccountsSchema = "switchback/admin-accounts@1"

	// accountProjectionFreshnessWindow: a projection older than this (by its own imported_at) is
	// treated as ABSENT — fail-open, no gate. Mirrors the held patch's capacityFreshnessWindow.
	accountProjectionFreshnessWindow = 6 * time.Hour

	// accountProjectionRelPath is the home-relative path the projection is written to.
	accountProjectionRelPath = "projections/account-projection.json"
)

// Account admission labels — the per-lane admission decision recorded on CapabilityRouteLane.
const (
	AccountAdmissionOmitted  = "omitted"
	AccountAdmissionAdmitted = "admitted"
	AccountAdmissionUngated  = "ungated"
)

// AccountProjectionEntry is one provider's normalized account-health summary. NO account ids,
// labels, aliases, or emails survive normalization from the switchback source — providers only.
type AccountProjectionEntry struct {
	Provider             string `json:"provider"`
	HealthyAccountCount  int    `json:"healthy_account_count"`
	TotalAccountCount    int    `json:"total_account_count"`
	BestRemainingSummary string `json:"best_remaining_summary,omitempty"`
	FreshestSampledAt    string `json:"freshest_sampled_at,omitempty"`
	SourceRevision       string `json:"source_revision,omitempty"`
	ImportedAt           string `json:"imported_at,omitempty"`
}

// AccountProjection is the full normalized, persisted surface (projections/account-projection.json).
type AccountProjection struct {
	SchemaVersion  string                            `json:"schema_version"`
	ImportedAt     string                            `json:"imported_at"`
	Source         string                            `json:"source,omitempty"`
	SourceRevision string                            `json:"source_revision,omitempty"`
	Providers      map[string]AccountProjectionEntry `json:"providers"`
}

// AccountProjectionImportReport is the normalize+write result of `route import-account-projection`.
type AccountProjectionImportReport struct {
	Path           string   `json:"path"`
	ProjectionPath string   `json:"projection_path"`
	SchemaVersion  string   `json:"schema_version"`
	SourceRevision string   `json:"source_revision,omitempty"`
	ImportedAt     string   `json:"imported_at"`
	ProviderCount  int      `json:"provider_count"`
	Providers      []string `json:"providers"`
}

// adminAccountsDoc is the subset of switchback's `GET /admin/accounts`
// (`switchback/admin-accounts@1`) response this importer reads. Fields carrying account identity
// (id, aliases, credentials, emails) are read only transiently in-memory to compute provider-grain
// counts — never copied into AccountProjection. Shape derived read-only from
// switchback/crates/sb-credentials/src/provider_accounts/types.rs (ProviderAccountSnapshot,
// ProviderAccountEnrollment, AccountCapacityHealth, CapacityWindow, CapacityUsed) and
// switchback/crates/sb-server/src/handlers/admin.rs (the accounts handler / provider_accounts_safe_json
// redaction). Unknown/extra fields (credentials, active_clients, sources, conflicts,
// selection_disabled, configured_account_health, aliases, timestamps other than sampled_at) are
// intentionally NOT modeled here — encoding/json drops them, so they can never leak through.
type adminAccountsDoc struct {
	Schema            string                `json:"schema"`
	AuthorityRevision json.Number           `json:"authority_revision"`
	Accounts          []adminAccountRecord  `json:"accounts"`
	Capacity          []adminCapacityRecord `json:"capacity"`
}

type adminAccountRecord struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	State    string `json:"state"`
}

type adminCapacityRecord struct {
	AccountID string                `json:"account_id"`
	SampledAt string                `json:"sampled_at"`
	Windows   []adminCapacityWindow `json:"windows"`
}

type adminCapacityWindow struct {
	WindowKind string            `json:"window_kind"`
	Used       adminCapacityUsed `json:"used"`
}

type adminCapacityUsed struct {
	Kind        string  `json:"kind"`
	UsedPercent float64 `json:"used_percent"`
}

// AccountProjectionPath is the absolute path the account projection is written to.
func AccountProjectionPath(homePath string) string {
	return filepath.Join(homePath, filepath.FromSlash(accountProjectionRelPath))
}

// ImportAccountProjectionFile reads+validates a switchback/admin-accounts@1 JSON file and writes
// the normalized, identity-free projection to projections/account-projection.json. Serialized under
// the same home lock the model-router import uses, and written atomically (temp file + rename,
// mirroring the model-capabilities/held-capacity-patch idiom) so a concurrent plan never reads a
// half-written projection.
func ImportAccountProjectionFile(homePath, path string, now time.Time) (AccountProjectionImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return AccountProjectionImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(path) == "" {
		return AccountProjectionImportReport{}, fmt.Errorf("account projection source path required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return AccountProjectionImportReport{}, fmt.Errorf("read account projection source %s: %w", path, err)
	}
	var doc adminAccountsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return AccountProjectionImportReport{}, fmt.Errorf("parse account projection source %s: %w", path, err)
	}
	if strings.TrimSpace(doc.Schema) != adminAccountsSchema {
		return AccountProjectionImportReport{}, fmt.Errorf("account projection source schema %q unsupported (want %q)", doc.Schema, adminAccountsSchema)
	}
	normalized := normalizeAdminAccounts(doc, now)
	body, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return AccountProjectionImportReport{}, err
	}
	projectionPath := AccountProjectionPath(homePath)
	err = homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		if err := os.MkdirAll(filepath.Dir(projectionPath), 0o755); err != nil {
			return err
		}
		return atomicWriteAccountProjection(projectionPath, append(body, '\n'))
	})
	if err != nil {
		return AccountProjectionImportReport{}, err
	}
	providers := make([]string, 0, len(normalized.Providers))
	for key := range normalized.Providers {
		providers = append(providers, key)
	}
	sort.Strings(providers)
	return AccountProjectionImportReport{
		Path:           path,
		ProjectionPath: projectionPath,
		SchemaVersion:  normalized.SchemaVersion,
		SourceRevision: normalized.SourceRevision,
		ImportedAt:     normalized.ImportedAt,
		ProviderCount:  len(providers),
		Providers:      providers,
	}, nil
}

// normalizeAdminAccounts strips every account id, alias, and email from the source and produces
// per-provider counts. An account is healthy when its enrollment state is "enrolled" AND no
// capacity window reports used_percent >= 100 for it (not walled); an account with no capacity data
// at all is healthy-by-state (optimistic default — compound has no negative signal for it).
// best_remaining_summary/freshest_sampled_at are informational only (never gate) and are derived
// only from parseable percent-kind capacity windows.
func normalizeAdminAccounts(doc adminAccountsDoc, now time.Time) AccountProjection {
	out := AccountProjection{
		SchemaVersion:  AccountProjectionSchema,
		ImportedAt:     now.UTC().Format(time.RFC3339),
		SourceRevision: strings.TrimSpace(doc.AuthorityRevision.String()),
		Providers:      map[string]AccountProjectionEntry{},
	}

	accountProvider := make(map[string]string, len(doc.Accounts))
	accountEnrolled := make(map[string]bool, len(doc.Accounts))
	for _, acct := range doc.Accounts {
		id := strings.TrimSpace(acct.ID)
		provider := strings.TrimSpace(acct.Provider)
		enrolled := strings.EqualFold(strings.TrimSpace(acct.State), "enrolled")
		// The source authority retains parked/superseded history, but this projection is
		// specifically the CURRENT account-admission surface. Counting historical rows as
		// accounts made two enrolled OpenAI subscriptions appear as 1,292 accounts. Exclude
		// non-enrolled records from both totals and capacity summaries; Switchback remains the
		// owner of their historical lifecycle.
		if id == "" || provider == "" || !enrolled {
			continue
		}
		accountProvider[id] = provider
		accountEnrolled[id] = true
	}

	accountWalled := make(map[string]bool, len(doc.Capacity))
	bestRemaining := map[string]float64{}
	haveBestRemaining := map[string]bool{}
	freshest := map[string]time.Time{}
	haveFreshest := map[string]bool{}
	for _, capEntry := range doc.Capacity {
		id := strings.TrimSpace(capEntry.AccountID)
		provider, ok := accountProvider[id]
		if !ok || provider == "" {
			continue
		}
		if sampledAt, err := time.Parse(time.RFC3339, strings.TrimSpace(capEntry.SampledAt)); err == nil {
			if !haveFreshest[provider] || sampledAt.After(freshest[provider]) {
				freshest[provider] = sampledAt
				haveFreshest[provider] = true
			}
		}
		for _, window := range capEntry.Windows {
			if strings.TrimSpace(window.Used.Kind) != "percent" {
				continue
			}
			used := window.Used.UsedPercent
			if used >= 100 {
				accountWalled[id] = true
			}
			remaining := 100 - used
			if remaining < 0 {
				remaining = 0
			}
			if !haveBestRemaining[provider] || remaining > bestRemaining[provider] {
				bestRemaining[provider] = remaining
				haveBestRemaining[provider] = true
			}
		}
	}

	for id, provider := range accountProvider {
		entry := out.Providers[provider]
		entry.Provider = provider
		entry.TotalAccountCount++
		if accountEnrolled[id] && !accountWalled[id] {
			entry.HealthyAccountCount++
		}
		out.Providers[provider] = entry
	}
	for provider, entry := range out.Providers {
		if haveBestRemaining[provider] {
			entry.BestRemainingSummary = formatAccountRemainingPercent(bestRemaining[provider])
		}
		if haveFreshest[provider] {
			entry.FreshestSampledAt = freshest[provider].UTC().Format(time.RFC3339)
		}
		entry.SourceRevision = out.SourceRevision
		entry.ImportedAt = out.ImportedAt
		out.Providers[provider] = entry
	}
	return out
}

func formatAccountRemainingPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}

// LoadAccountProjection loads the persisted projection. An absent projection is (_, false, nil): an
// expected, not exceptional, state — the caller MUST treat that as "no gate", never a failure.
// Freshness is NOT checked here (a caller wanting the fail-open-vs-stale distinction for warning
// purposes uses resolveAccountAdmissionGate, which also treats a stale/unparseable projection as
// absent for gating purposes but still surfaces a warning).
func LoadAccountProjection(homePath string) (AccountProjection, bool, error) {
	data, err := os.ReadFile(AccountProjectionPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return AccountProjection{}, false, nil
		}
		return AccountProjection{}, false, err
	}
	var projection AccountProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return AccountProjection{}, false, err
	}
	return projection, true, nil
}

// accountAdmissionGate is the resolved gate input for one PlanCapabilityRoute call.
type accountAdmissionGate struct {
	projection AccountProjection
	active     bool
	warning    string
}

// resolveAccountAdmissionGate loads the projection and applies the freshness rule:
//   - absent (no file, or a read error) -> no gate, NO warning (F4: byte-identical to feature absent);
//   - present but unparseable/stale (>6h by its own imported_at) -> no gate, WITH a loud warning (F7);
//   - present and fresh -> gate active.
func resolveAccountAdmissionGate(homePath string, now time.Time) accountAdmissionGate {
	projection, ok, err := LoadAccountProjection(homePath)
	if err != nil {
		// A read error other than not-exist never fails the plan — degrade to no gate, same as an
		// absent projection, but say so (this is not the common/expected case).
		return accountAdmissionGate{warning: "account projection unreadable; routing without account admission gate (" + err.Error() + ")"}
	}
	if !ok {
		return accountAdmissionGate{}
	}
	importedAt, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(projection.ImportedAt))
	if parseErr != nil {
		return accountAdmissionGate{warning: "account projection imported_at unparseable; routing without account admission gate"}
	}
	if now.Sub(importedAt) > accountProjectionFreshnessWindow {
		return accountAdmissionGate{warning: fmt.Sprintf("account projection stale (imported_at=%s, older than %s); routing without account admission gate", projection.ImportedAt, accountProjectionFreshnessWindow)}
	}
	return accountAdmissionGate{projection: projection, active: true}
}

// laneAccountProviderKey maps a lane to its account-admission provider key by harness — EXACTLY
// mirrors the held capacity-gate patch's laneCapacityKey (codex->codex, claude-code->claude,
// zai->zai; everything else is never gated).
func laneAccountProviderKey(lane CapabilityRouteLane) (string, bool) {
	switch strings.TrimSpace(lane.Harness) {
	case "codex":
		return "codex", true
	case "claude-code":
		return "claude", true
	case "zai":
		return "zai", true
	default:
		return "", false
	}
}

// atomicWriteAccountProjection writes data to path via a temp file + rename so readers never see a
// partial projection. Mirrors internal/breadth/vectors.go atomicWrite (unexported there; the idiom
// is reused rather than a novel scheme hand-rolled).
func atomicWriteAccountProjection(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
