// system_status.go — the system-status projection (W5 J4).
//
// A rebuildable, READ-ONLY fold of the existing owned authorities into ONE
// status report so the legacy `os status` reader (an absorbed operating
// system's exec-lane/os.ts) can become a passthrough over
// `compound://projections/system-status` (W5 J8) and
// the console home-briefing can consume the SAME projection (W5 G2 — one
// projection, not two).
//
// It is a projection, never truth: rebuilt from scratch on every call (nothing
// is written to the ledger or to projections/ — a status report is inherently
// ephemeral, matching the system_map / system_stats / source_connections
// read-on-call idiom in this package). Every folded section carries its own
// `source_ref` (compound://...) so a consumer can drill down to the owning
// authority. No new state is created; each section reads only an existing owned
// authority.
//
// Scope (W5 J4 objective — the five NAMED authorities):
//   - workbench       → internal/work read model (open/needs-review/needs-claim counts)
//   - job_roster      → internal/authority job-roster authority (cron roster + errors)
//   - route_evidence  → internal/routeevidence execution-route-evidence authority
//   - product_authority → internal/product feedback authority (products/signals/reviews)
//   - capability_authority → capability-graph registry node/edge counts
//
// os.ts's snapshot carries more keys than these five; the field-parity fixture
// in system_status_parity_test.go enumerates every os.ts key and records, for
// each, whether it is folded here, excluded (legacy/gbrain — retires), or
// deferred (a real compound authority outside J4's named scope, to be folded
// before J8 can fully gut os.ts). A documented disposition is honest; a silent
// drop is not.
//
// Layering note: internal/projection is a LOW layer (internal/contract imports
// it), so it cannot import the high-layer authority packages that would cycle
// back through contract. capability and product are folded by reading their
// registry/ledger files directly (the system_map.go idiom): importing capgraph
// cycles via capgraph→sourceobs→projection, and importing product cycles via
// projection→product→panel→distiller (distiller's test imports contract, which
// imports projection). work / authority / routeevidence are cycle-safe and are
// folded through their own load functions so those sections never re-implement
// (and cannot drift from) the authority's own computation.
package projection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/authority"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const (
	// SystemStatusSchemaVersion versions the projection body.
	SystemStatusSchemaVersion = "compound.system_status_projection.v0.1"
	// SystemStatusProjectionRef is the stable ref for this generated view — the
	// target named by the os_status_cli absorption row (W5 J8).
	SystemStatusProjectionRef = "compound://projections/system-status"

	// workbenchSourceRef is the drilldown ref for the folded workbench read model.
	// The workbench is a read model over WorkEnvelope (no `compound://` authority
	// const exists for it); the CLI surface `compound workbench` is its drilldown.
	workbenchSourceRef = "compound://workbench"

	// capabilityGraphSourceRef duplicates capgraph.CapabilityGraphAuthorityRef on
	// purpose — this package must not import capgraph (cycle:
	// capgraph→sourceobs→projection). capgraph.CapabilityGraphAuthorityRef is the
	// source of truth for the value.
	capabilityGraphSourceRef = "compound://registries/capability-graph"

	// productFeedbackSourceRef duplicates product.AuthorityRef on purpose — this
	// package must not import product (cycle: projection→product→panel→distiller,
	// and distiller's test imports contract→projection). product.AuthorityRef is
	// the source of truth for the value; the product section folds the
	// product-feedback ledger files directly (see foldProduct).
	productFeedbackSourceRef = "compound://ledger/product-feedback"
)

// SystemStatusProjection is the full derived surface — a union of the folded
// authority sections plus a fold-scoped verdict. It is returned on-read and
// never persisted.
type SystemStatusProjection struct {
	SchemaVersion string `json:"schema_version"`
	ProjectionRef string `json:"projection_ref"`
	GeneratedAt   string `json:"generated_at"`
	// Verdict is scoped to the FOLDED authorities only ("attention" if any folded
	// authority signals operator work, else "ok"). It is NOT the full os doctor
	// verdict — os.ts's verdict also folds daemon/switchback/gbrain health probes
	// this projection deliberately does not run (see the parity fixture).
	Verdict    string               `json:"verdict"`
	SourceRefs []string             `json:"source_refs"`
	Sections   SystemStatusSections `json:"sections"`
}

// SystemStatusSections holds one folded section per named authority. The JSON
// keys deliberately match os.ts's snapshot keys so J8's passthrough and the
// console G2 seam map field-for-field.
type SystemStatusSections struct {
	Workbench     SystemStatusWorkbench  `json:"workbench"`
	JobRoster     SystemStatusJobRoster  `json:"job_roster"`
	RouteEvidence SystemStatusRoute      `json:"route_evidence"`
	Product       SystemStatusProduct    `json:"product_authority"`
	Capability    SystemStatusCapability `json:"capability_authority"`
}

// sectionStatus values are the per-section health envelope, mirroring os.ts's
// per-probe status: "ok" (authority read), "missing" (authority not yet
// installed in this home), "unavailable" (authority present but unreadable).
const (
	sectionStatusOK          = "ok"
	sectionStatusMissing     = "missing"
	sectionStatusUnavailable = "unavailable"
)

type SystemStatusWorkbench struct {
	SourceRef   string `json:"source_ref"`
	Status      string `json:"status"`
	Installed   bool   `json:"installed"`
	Total       int    `json:"total"`
	Open        int    `json:"open"`
	Terminal    int    `json:"terminal"`
	NeedsClaim  int    `json:"needs_claim"`
	NeedsReview int    `json:"needs_review"`
	InProgress  int    `json:"in_progress"`
}

type SystemStatusJobRoster struct {
	SourceRef        string `json:"source_ref"`
	Status           string `json:"status"`
	Installed        bool   `json:"installed"`
	JobCount         int    `json:"job_count"`
	EnabledCount     int    `json:"enabled_count"`
	RecentRunCount   int    `json:"recent_run_count"`
	ErrorCount       int    `json:"error_count"`
	ActiveErrorCount int    `json:"active_error_count"`
}

type SystemStatusRoute struct {
	SourceRef                   string `json:"source_ref"`
	Status                      string `json:"status"`
	Installed                   bool   `json:"installed"`
	RouteEvidenceCount          int    `json:"route_evidence_count"`
	ProviderLedgerEvidenceCount int    `json:"provider_ledger_evidence_count"`
	TokenUsageRows              int    `json:"token_usage_rows"`
	TotalTokens                 int    `json:"total_tokens"`
}

// SystemStatusProduct folds the product-feedback ledger files directly (see the
// layering note): product_count + signal_count from the signals ledger, and the
// recorded review rows. The richer per-product doctor counts (hypotheses,
// pending/ready reviews) live behind product.Authority, which this low layer
// cannot import; a consumer drills into source_ref for those.
type SystemStatusProduct struct {
	SourceRef    string `json:"source_ref"`
	Status       string `json:"status"`
	Installed    bool   `json:"installed"`
	ProductCount int    `json:"product_count"`
	SignalCount  int    `json:"signal_count"`
	ReviewCount  int    `json:"review_count"`
}

type SystemStatusCapability struct {
	SourceRef string `json:"source_ref"`
	Status    string `json:"status"`
	Installed bool   `json:"installed"`
	NodeCount int    `json:"node_count"`
	EdgeCount int    `json:"edge_count"`
}

// BuildSystemStatus folds the five named authorities into one status report.
// Each section degrades independently: a missing/unreadable authority yields a
// non-"ok" section status, never a failed projection — so the CLI stays robust
// the way os.ts's independent probes are. The returned error is reserved for a
// caller-level failure (an empty home path that no authority can resolve).
func BuildSystemStatus(homePath string, now time.Time) (SystemStatusProjection, error) {
	if now.IsZero() {
		now = time.Now()
	}
	proj := SystemStatusProjection{
		SchemaVersion: SystemStatusSchemaVersion,
		ProjectionRef: SystemStatusProjectionRef,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Sections: SystemStatusSections{
			Workbench:     SystemStatusWorkbench{SourceRef: workbenchSourceRef, Status: sectionStatusMissing},
			JobRoster:     SystemStatusJobRoster{SourceRef: authority.JobRosterAuthorityRef, Status: sectionStatusMissing},
			RouteEvidence: SystemStatusRoute{SourceRef: routeevidence.RouteEvidenceAuthorityRef, Status: sectionStatusMissing},
			Product:       SystemStatusProduct{SourceRef: productFeedbackSourceRef, Status: sectionStatusMissing},
			Capability:    SystemStatusCapability{SourceRef: capabilityGraphSourceRef, Status: sectionStatusMissing},
		},
	}

	foldWorkbench(homePath, now, &proj.Sections.Workbench)
	foldJobRoster(homePath, &proj.Sections.JobRoster)
	foldRouteEvidence(homePath, &proj.Sections.RouteEvidence)
	foldProduct(homePath, &proj.Sections.Product)
	foldCapability(homePath, &proj.Sections.Capability)

	proj.SourceRefs = []string{
		proj.Sections.Workbench.SourceRef,
		proj.Sections.JobRoster.SourceRef,
		proj.Sections.RouteEvidence.SourceRef,
		proj.Sections.Product.SourceRef,
		proj.Sections.Capability.SourceRef,
	}
	sort.Strings(proj.SourceRefs)
	proj.Verdict = systemStatusVerdict(proj.Sections)
	return proj, nil
}

// foldWorkbench reads the work read model. work.Open only fails on an empty home
// path; store.Workbench is a pure read model ("does not dispatch, claim, close,
// or repair work").
func foldWorkbench(homePath string, now time.Time, out *SystemStatusWorkbench) {
	store, err := work.Open(homePath)
	if err != nil {
		out.Status = sectionStatusUnavailable
		return
	}
	report, err := store.Workbench(now, work.WorkbenchOptions{})
	if err != nil {
		out.Status = sectionStatusUnavailable
		return
	}
	out.Status = sectionStatusOK
	out.Installed = true
	out.Total = report.Counts.Total
	out.Open = report.Counts.Open
	out.Terminal = report.Counts.Terminal
	out.NeedsClaim = report.Counts.NeedsClaim
	out.NeedsReview = report.Counts.NeedsReview
	out.InProgress = report.Counts.InProgress
}

// foldJobRoster reads the imported job-roster authority. A not-yet-imported
// roster (file absent) is "missing", not an error — the roster is imported by
// `compound authority import-job-roster` and may not exist in a fresh home.
func foldJobRoster(homePath string, out *SystemStatusJobRoster) {
	roster, err := authority.LoadJobRosterAuthority(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			out.Status = sectionStatusMissing
			return
		}
		out.Status = sectionStatusUnavailable
		return
	}
	out.Status = sectionStatusOK
	out.Installed = true
	out.JobCount = roster.JobCount
	out.EnabledCount = roster.EnabledCount
	out.RecentRunCount = roster.RecentRunCount
	out.ErrorCount = roster.ErrorCount
	out.ActiveErrorCount = roster.ActiveErrorCount
}

// foldRouteEvidence reads the execution-route-evidence authority, which already
// returns its own status/installed envelope (a missing ledger is a valid
// "missing" report, not an error).
func foldRouteEvidence(homePath string, out *SystemStatusRoute) {
	report, err := routeevidence.LoadAuthority(homePath, 0)
	if err != nil {
		out.Status = sectionStatusUnavailable
		return
	}
	if report.Installed {
		out.Status = sectionStatusOK
	} else {
		out.Status = sectionStatusMissing
	}
	out.Installed = report.Installed
	out.RouteEvidenceCount = report.RouteEvidenceCount
	out.ProviderLedgerEvidenceCount = report.ProviderLedgerEvidenceCount
	out.TokenUsageRows = report.TokenUsageRows
	out.TotalTokens = report.TotalTokens
}

// foldProduct reads the product-feedback ledger files directly (this low layer
// cannot import internal/product — see the layering note). An absent signals
// ledger means the product loop has produced nothing in this home yet →
// "missing", consistent with the other file-backed sections.
func foldProduct(homePath string, out *SystemStatusProduct) {
	signalsPath := filepath.Join(homePath, "ledger", "product-signals.jsonl")
	if _, err := os.Stat(signalsPath); err != nil {
		out.Status = sectionStatusMissing
		return
	}
	out.Status = sectionStatusOK
	out.Installed = true
	out.SignalCount = countJSONLLines(signalsPath)
	out.ReviewCount = countJSONLLines(filepath.Join(homePath, "ledger", "product-reviews.jsonl"))
	out.ProductCount = distinctProductIDs(signalsPath)
}

// distinctProductIDs counts the distinct non-empty product_id values in a
// product-signals JSONL ledger. A malformed line is skipped (never fatal) — the
// projection stays robust the way os.ts's independent probes are.
func distinctProductIDs(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			ProductID string `json:"product_id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if id := strings.TrimSpace(row.ProductID); id != "" {
			seen[id] = true
		}
	}
	return len(seen)
}

// foldCapability counts the capability-graph registry JSONL directly (the
// system_map.go idiom) — importing capgraph here would be an import cycle. The
// nodes file's existence is the "installed" signal (an empty count with the file
// present is still installed).
func foldCapability(homePath string, out *SystemStatusCapability) {
	nodesPath := filepath.Join(homePath, "registries", "capability-nodes.jsonl")
	edgesPath := filepath.Join(homePath, "registries", "capability-edges.jsonl")
	if _, err := os.Stat(nodesPath); err != nil {
		out.Status = sectionStatusMissing
		return
	}
	out.Status = sectionStatusOK
	out.Installed = true
	out.NodeCount = countJSONLLines(nodesPath)
	out.EdgeCount = countJSONLLines(edgesPath)
}

// systemStatusVerdict is "attention" if any folded authority signals operator
// work (an active cron error, or work needing a claim/review), else "ok".
// Scoped to the folded authorities only — see the Verdict field doc.
func systemStatusVerdict(s SystemStatusSections) string {
	if s.JobRoster.ActiveErrorCount > 0 ||
		s.Workbench.NeedsClaim > 0 ||
		s.Workbench.NeedsReview > 0 {
		return "attention"
	}
	return "ok"
}
