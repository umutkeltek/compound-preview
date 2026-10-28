// Package arch is the architecture doctor: it makes "every package has a logical job, every
// candidate kind one home, no private leak, no silent new front door" a MECHANICAL check rather
// than a doc promise. The package Classification below is the ENFORCED source of truth; docs/
// architecture/status.md narrates it. A new internal/ package that nobody classifies FAILS the
// doctor — so the two-stack convergence debt stays visible and fragmentation cannot grow silently.
package arch

import (
	"bytes"
	"context"
	_ "embed" // for //go:embed baseline-leak-tokens.txt (armed-by-default doc-leak gate)
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"compound/internal/candidates"
	"compound/internal/home"
)

// Class is what job a package has in the architecture (LAYERS.md: one spine + object families).
type Class string

const (
	Spine           Class = "spine"            // canonical runtime / spine-bound Gen-2 — no duplication
	ObjectFamily    Class = "object-family"    // an object family the spine's transitions create/own/adjudicate (LAYERS.md); authority for its objects, never for the runtime grammar
	Leaf            Class = "leaf"             // shared leaf util (e.g. the candidate map)
	MemTarget       Class = "mem-target"       // the convergence TARGET the Gen-1 object model migrates into
	ConvergePending Class = "converge-pending" // Gen-1 capability to migrate onto the spine, THEN retire (test-gated)
	Importer        Class = "importer"         // an importer to upgrade in place (authority/decisions)
)

// Classification — every internal/ package, with its job. This is enforced: see CheckClassificationTotal.
var Classification = map[string]Class{
	// spine (the envelope runtime grammar itself — allowlist-ratcheted, see spineCore)
	"arch":      Spine,
	"contract":  Spine,
	"envelope":  Spine,
	"harness":   Spine,
	"home":      Spine,
	"hook":      Spine,
	"policy":    Spine,
	"schema":    Spine,
	"work":      Spine,
	"writeback": Spine,
	// object families the spine's transitions create/own/adjudicate (LAYERS.md) — authority for
	// their objects, never for the runtime grammar
	"activity":       ObjectFamily,
	"capgraph":       ObjectFamily,
	"director":       ObjectFamily,
	"distiller":      ObjectFamily,
	"localmodel":     ObjectFamily,
	"lanebeliefs":    ObjectFamily,
	"observation":    ObjectFamily,
	"panel":          ObjectFamily,
	"product":        ObjectFamily,
	"profiles":       ObjectFamily,
	"projection":     ObjectFamily,
	"routeevidence":  ObjectFamily,
	"runtimebinding": ObjectFamily,
	// shared leaf
	"act":             Leaf, // generic governed outbound-write seam: the one execute-and-receipt wrapper that gates every external write on a matching allowing PolicyDecision and emits pre/post-image artifacts + an outward_delta Receipt/Transition; owns no authority object (envelope/policy/work stay authority), a new outbound system is a new Adapter not a new governance path
	"adapter":         Leaf, // rented external-protocol adapters that feed owned object families (today: awstream → activity); read-only join/normalize of foreign event shapes; owns no authority object and never promotes
	"actuator":        Leaf, // execution router: turns KNOWN work into governed executions within declared limits and closes the loop; composes policy.Evaluate + a BudgetEnvelope/ledger (hard per-channel spend/rate caps) + a Route() classifier (auto for local_reversible/easy + under-budget, hold for external/public/financial/irreversible) and dispatches through the existing harness.Adapter.Run / act.Execute seams; a ProjectProfile + switchback re-points per-project harness routing + instruction files (AGENTS.md/CLAUDE.md) smoothly; owns no authority object (envelope/policy/work/act stay authority), persists only the budget envelope + consumption ledger under COMPOUND_HOME/actuator/budget
	"attention":       Leaf, // ranked read-only projection over reviews/workbench/executors; owns no authority objects
	"behaviorallens":  Leaf, // composable behavioral-lens framework: uniform observe->InferredSignal plug + visibility-gated composition + sycophancy (paired-stance shift) lens; records evidence rows under COMPOUND_HOME, owns no authority object and never gates/promotes
	"bench":           Leaf, // evaluator organ: read-only scorer over the real surfacing/recall paths; produces evidence reports, owns no object family
	"breadth":         Leaf,
	"browser":         Leaf, // read-only headless/automation browser (agent-browser chrome) observation surface behind `compound browser list|show|doctor`; `doctor --fix` is B-scope (orphan-idle swiftshader reap only, per-target confirmation by default); owns no authority object
	"candidates":      Leaf,
	"console":         Leaf, // console launch-plan construction, capability gate, sanitized child-launch env, instance-pinning inputs; runtime launch gate blocks unsafe mutation activation in BuildLifecycleLaunchPlan and ValidateMutationLaunchEnvironment, so the contract is enforcement; JSON serialization hides the out-of-band runtime capability value while publishing CapabilityRequired (callers can see *that* one was required without learning its bytes); owns no authority object (envelope/policy/work/home stay authority)
	"corpus":          Leaf, // lane-scoped corpus indexes: disposable derivatives over imported evidence; ledger stays authority, hits never ambient
	"doctor":          Leaf, // health-signal computation for the `compound doctor` operator surface: relates the INSTALLED binary's build SHA to the repo's target tip (equal/descendant/ancestor/unknown) so a stale install stops reading as all-clear; pure relation + injected git/exec seams, records nothing under COMPOUND_HOME, owns no authority object, never gates (the operator decides whether to `make install`)
	"executorlens":    Leaf, // evidence-only collector: reads THIS repo's executor lanes' (Codex, Claude Code) receipts/events and builds behaviorallens Observations (public_safe hedging over output text + operator_private agreement-asymmetry over receipt verdicts); records evidence rows only, owns no authority object, never gates/promotes
	"finding":         Leaf, // direct-write home for agent-authored operational findings under COMPOUND_HOME/observed/findings: content-addressed evidence records (source_class=evidence), NEVER a ledger byte, NEVER a promoted Claim; the positive counterpart to the placement guard, owns no authority object (promotion stays the governed memory door)
	"flagaudit":       Leaf, // flag-validation ratchet: censuses first-party command paths that silently accept an unknown flag (exit 0 with no mention of it) and pins that census as a ceiling that may only shrink; enforcement leaf like eventstore — its teeth are EnforceCeiling failing cmd/compound's ratchet test, owns no authority object
	"harvest":         Leaf, // ruling harvester: classifies operator utterances + reconciles them against accepted decision records through mem/decisions/candidates; owns no authority object (corroboration lands on mem, novel rulings on the decision-candidate door proposed-only)
	"hookplan":        Leaf, // LifecyclePlan-to-native hook reconciliation plus relevance-gated coordination delivery; persisted cursors/reports are projections, while governed events, installed-hooks ledger, and harness configs retain authority
	"lanemanifest":    Leaf, // the one declarative unit that names a harness lane (harness + model + capture + route + hooks.roster) that switchback and compound both consume; compound derives install harness and hook roster from it, owns no authority object (hook and harness configs retain authority)
	"intent":          Leaf, // thin expressive-intent coordinator over mem capture, work-owned meaning resolution/create-plan admission, and reviewartifact parking; owns no authority object or parallel plan/context/policy format
	"jobprobe":        Leaf, // job-roster probe-refresh collector: re-observes current unit liveness for declared probe targets (files under COMPOUND_HOME) and refreshes the job-roster authority through authority's additive refresh surface; owns no authority object (authority stays the roster owner)
	"landlease":       Leaf, // land-lease coordination hint: a single-holder, TTL-bounded, steal-with-reason advisory MUTEX (flock-guarded JSON under COMPOUND_HOME) over the ACT of landing to main, so concurrent executor lanes serialize the ff-merge window; records who holds the right to land, never mutates repo state, emits no PolicyDecision/Receipt and gates/promotes nothing
	"laneselect":      Leaf, // pure preparation-lane ranking (fit/speed/cost) shared by work dispatch + routeevidence adapter; owns no authority objects, breaks the work<->routeevidence import cycle
	"lanematrix":      Leaf, // read-only (harness x lane) projection: reads the launch owner's published profile-conformance and composes it against the harness registry, so combinations are enumerated from registries rather than from a launcher-directory listing; decides no compatibility it cannot cite and owns no authority object (launch profiles and the harness registry stay authority)
	"latenttax":       Leaf, // latent-tax rig: read-only factorial evaluator that DERIVES thrash/tokens/latency/success from existing ledger objects per work_id; owns only a thin run-index, produces evidence reports, no promotion path
	"lensmodulator":   Leaf, // governed modulator: composes behaviorallens signals + act into the one enforcement point that turns a crossing signal into a reversible, PolicyDecision-gated route/role demote (calibration-guarded, default off); owns no authority object (envelope/policy/work/act stay authority), operator_private modulations stay operator-local
	"narrate":         Leaf, // composed read-only correspondent script over attention/orient-changed/receipts; owns no authority objects
	"negatives":       Leaf, // rebuildable read-only projection over inbox/failure-patterns + reviews/route-remorse; owns no authority objects, surfaces scope-matched negatives at orient (Weng item 6)
	"operatornotice":  Leaf, // single missing edge between the attention hold frontier (internal/attention) and the existing act post buzz transport (internal/act); one function (Notify) scans BandNeedsYou groups, emits exactly one governed buzz notification per new hold (idempotent on re-scan), and counts unnotifiable/refused holds in a doctor figure; reuses attention.Build, act.Execute, and the existing standing-grant registry, owns no authority object
	"persist":         Leaf, // storage abstraction layer: interfaces (LedgerStore, EventStore, WorkStore, VectorStore) + Postgres/pgvector backend; file backend stays default, owns no authority object (ADR-0005)
	"store":           Leaf, // `compound store` operator surface organ (ADR-0005 pg experiment): doctor/init/status/migrate/sync/embed/recall; governed exports mint Receipt+Transition, owns no authority object
	"projectcontract": Leaf, // shared YAML shapes of the source-in-place project contract (infra-os `project.yaml`), owned once so its two readers — internal/product (product identity + deployment claims) and internal/runtimebinding (runtime bindings) — cannot disagree about what a contract field means; pure parse/normalize types with no I/O, owns no authority object and never gates or promotes (the contracts stay source-in-place authority)
	"proofscope":      Leaf, // pure go-test package-scope derivation for a sealed base/head commit pair: full with one reason, or an exact sorted affected package list; reads git objects only (never the worktree or COMPOUND_HOME), certifies nothing itself and owns no authority object; proof-check --presubmit consumes it for the non-certifying pre-landing go-test scope, claim verification and the canonical go-test step consume it later
	"promotedriver":   Leaf, // governed candidate-only promote actuator: single-flight frozen candidate/current-claim intent, per-operation CAS/idempotency/results, same-pass recovery, terminal-plan bijection, and strict cursor/pending finalization; registry and lane-belief mutation remain experimental preview-only; dry-run default, apply bound to a maintenance work id; enforcement leaf, owns no authority object
	"release":         Leaf,
	"routeev":         Leaf, // deterministic EV-card composer + calibration comparator + content-addressed card store under COMPOUND_HOME/observed/route-evcards; pure pricing evidence for route deviations (refuses to price missing inputs), owns no authority object and never gates/promotes (the deviation-staking gate lives in envelope.Validate, the calibration minting door in mem)
	"lanes":           Leaf, // per-lane readiness ladder (hook install -> contract compile -> instruction-pack render -> --drift) with one LaneVerdict consumed by install.sh, `harness refresh-lane`, and doctor; an actuator over harness profiles, owns no authority object
	"readiness":       Leaf, // read-only harness readiness evaluation (hook binding, instruction pack, context pack, permission posture) shared by `work delegate --dry-run`, `harness readiness`, and the governed execute door; produces evidence, owns no authority object
	"runattempt":      Leaf, // concrete harness-execution-attempt store under COMPOUND_HOME/run_attempts; persistence primitive consumed by work's RunAttemptIDs slot, not meaning authority or envelope spine
	"reviewartifact":  Leaf,
	"retrievalparity": Leaf, // evaluator organ: read-only traffic recorder/replayer over real retrieval paths; produces evidence reports, owns no object family
	"statussurface":   Leaf, // body-free, TTL-bound adaptive projection over harness-native context/rate-limit metadata; Claude/Codex remain live authority, route guidance is evidence only
	"stream":          Leaf, // read-only tail organ over events/work.jsonl (byte cursor) + ledger/seq; opens the log read-only and NEVER takes the work-event write lock; emits evidence frames the console SSE route translates, owns no authority object and promotes nothing
	"reviewqueue":     Leaf, // canonical workbench review-queue builder (merge + annotate rows); presentation-side only, no authority object, no terminal effect
	"schedule":        Leaf, // read-only launchd+crontab projection surface behind `compound schedule list|show|doctor`; no mutation door; owns no authority object
	"sessionmining":   Leaf, // deterministic session-finding router: semantic-key idempotency across overlapping windows, propose-only memory/distiller/capability artifacts, no promotion or execution
	"terminal":        Leaf, // macOS terminal surface behind `compound terminal <name> doctor|wire|unwire|hooks|workspace|feed`; read verbs doctor/hooks, governed mutate verbs wire/unwire/workspace/feed; owns no authority object
	"game":            Leaf, // game studio operator surface behind `compound game doctor|observe|genres|companies|trending|scout|draft|unity`; reads from internal/sourceobs into a stable gmid_* catalog; observe/scaffold/draft are governed at the command-registry layer; owns no authority object
	"verdictflow":     Leaf, // shared plan->record->render->load->annotate scaffolding behind the three panel-verdict review bridges (product/sourceobs/capgraph); writes review artifacts through reviewartifact and reads them back, owns no authority object and performs no terminal effect (ledger/store/writeback stay in the domain callbacks)
	// memory object family — canonical after Gen-1 package retirement
	"mem": MemTarget,
	// importers — keep, upgrade in place
	"authority":    Importer,
	"decisions":    Importer,
	"eventsource":  Importer,
	"eventstore":   Leaf, // sole physical-layout owner for COMPOUND_HOME/events; enforcement leaf, not meaning authority or envelope spine
	"knowledge":    Importer,
	"sourceobs":    Importer,
	"tooltopology": Importer,
}

// spineCore is the ALLOWLIST of packages that ARE the runtime spine (the envelope grammar every
// harness/hook/run flows through). Membership is a conscious edit here, never a default: if
// everything is spine, agents treat everything as equally authoritative. checkClassificationTotal
// fails if a package is classified Spine without a spineCore entry (or a spineCore entry is not
// classified Spine), so a new package must be classified as object-family/leaf/importer instead of
// quietly joining the authoritative spine. Keep this in lockstep with the Spine rows above.
var spineCore = map[string]bool{
	"arch":      true,
	"contract":  true,
	"envelope":  true,
	"harness":   true,
	"home":      true,
	"hook":      true,
	"policy":    true,
	"schema":    true,
	"work":      true,
	"writeback": true,
}

// Severity of a finding. fail = the doctor is not OK; warn/info are reported, not fatal.
type Severity string

const (
	Fail Severity = "fail"
	Warn Severity = "warn"
	Info Severity = "info"
)

// Finding is one doctor result line.
type Finding struct {
	Check    string
	Severity Severity
	Message  string
}

// Report is the doctor outcome.
type Report struct {
	Findings []Finding
	OK       bool // false if any Fail
}

// genericPathLeak and genericEmailLeak match GENERIC operator-private IDENTITY
// shapes (absolute /Users|/home
// paths, email addresses) that must never appear, unresolved, in a public tracked file.
// It now runs over the FULL tracked-file universe (see checkDocLeak / leakScanUniverse) —
// root files, docs, skills, and every source root get the same floor, not just docs/skills.
// Operator-SPECIFIC forbidden tokens (private tool/product names) are deliberately NOT
// hardcoded here — putting them in public core would itself leak them. They load from an
// optional private list (docs_private/leak-tokens.txt, one token per line) so the public
// core stays clean while a private overlay can enforce its own vocabulary. Universal
// secret-credential shapes (API-key/token prefixes, PEM headers) ARE shipped, via the
// embedded baseline below, so the gate is armed by default even without the private overlay.
//
// Path alternates are BOUNDARY-GUARDED: `(^|[^/\w])` must precede the slash, so a match
// requires a real path boundary, not a mid-word/mid-path substring. Without this, a naive
// scan false-positives whenever an unrelated identifier merely contains a slash followed
// by "Users" or "home" mid-string — a directory two levels down a longer path whose
// PARENT segment happens to end in one of those four letters, or a bare relative
// file-path STRING (e.g. a Go package name embedded as a fixture literal) — neither is
// an operator's actual home directory.
//
// The regexes have no allowlist concept of their own (Go's RE2 engine has no
// lookaround to express one) — every candidate match is post-filtered by
// findGenericLeak against the RFC2606/synthetic allowlists below. Keeping email
// and path matching independent is load-bearing: an allowlisted path prefix must
// never consume and hide an overlapping real email. Path captures: 1 = boundary,
// 2 = family, 3 = the whole lexical user segment. Email captures: 1 = full
// email, 2 = domain. The path component stops only at a slash or a source/prose
// delimiter; punctuation such as + and @ remains part of the segment so the
// exact-four synthetic allowlist can never accept only a prefix.
var genericPathLeak = regexp.MustCompile("(?i)(^|[^/\\w])/(Users|home)/([a-z][^/\\s\"'`,;:()\\[\\]{}<>]*)")
var genericEmailLeak = regexp.MustCompile(`(?i)([a-z0-9._%+\-]+@([a-z0-9.\-]+\.[a-z]{2,}))`)

// genericLeakAllowedPathSegments are the synthetic /Users|/home user segments already in
// legitimate use across the tree's own test fixtures (measured 2026-07-17 against the
// full tracked tree): "example", "faketester", "tester", "testoperator". New fixtures
// should prefer "example" — the RFC2606-flavored default — over the other three. The
// letter-first regex keeps hidden directories directly under /home out of this user-home
// class instead of growing the identity allowlist for product/cache directory names.
var genericLeakAllowedPathSegments = map[string]bool{
	"example":      true,
	"faketester":   true,
	"tester":       true,
	"testoperator": true,
}

// genericLeakAllowedEmailDomainsExact and genericLeakAllowedEmailDomainSuffixes are the
// RFC2606/reserved "this is not a real mailbox" domain shapes. A matched email is allowed
// iff its domain is exactly one of the *.com/.org/.net reserved names, or ENDS in one of
// the reserved suffixes (.invalid/.test/.example/.localhost) — everything else fails.
var genericLeakAllowedEmailDomainsExact = map[string]bool{
	"example.com": true,
	"example.org": true,
	"example.net": true,
}
var genericLeakAllowedEmailDomainSuffixes = []string{".invalid", ".test", ".example", ".localhost"}

func isAllowedGenericLeakEmailDomain(domain string) bool {
	low := strings.ToLower(domain)
	if genericLeakAllowedEmailDomainsExact[low] {
		return true
	}
	for _, suf := range genericLeakAllowedEmailDomainSuffixes {
		if strings.HasSuffix(low, suf) {
			return true
		}
	}
	return false
}

func isAllowedGenericLeakPathSegment(segment string) bool {
	return genericLeakAllowedPathSegments[strings.ToLower(segment)]
}

// findGenericLeak returns the first NON-allowlisted generic match on line, or "" when
// the line has no match at all, or every match on it is allowlisted (an RFC2606 email
// domain, or a synthetic /Users|/home user segment). A line can carry more than one
// candidate match; only an unallowlisted one is reported.
func findGenericLeak(line string) string {
	for _, m := range genericEmailLeak.FindAllStringSubmatch(line, -1) {
		if isAllowedGenericLeakEmailDomain(m[2]) {
			continue
		}
		return m[1]
	}
	for _, m := range genericPathLeak.FindAllStringSubmatch(line, -1) {
		if isAllowedGenericLeakPathSegment(m[3]) {
			continue
		}
		return "/" + m[2] + "/" + m[3]
	}
	return ""
}

var statusProofJournalLine = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(?:20[0-9]{2}-[0-9]{2}-[0-9]{2}\b|proof log:|verification run:|test run:|commit [0-9a-f]{7,40}\b)`)

// baselineLeakTokensRaw is the TRACKED, always-shipped generic leak-token list (obvious
// secret-credential shapes only — never operator-specific names). It is embedded into the
// binary so the doc-leak gate is armed by DEFAULT in a fresh clone/worktree, with no
// dependency on the gitignored operator overlay: a stranger's proof-check never fails closed.
//
//go:embed baseline-leak-tokens.txt
var baselineLeakTokensRaw string

// baselineLeakTokens parses the embedded baseline into normalized (lowercased) substring
// tokens. It is always present, so it is the ARMING mechanism: the operator overlay only
// ADDS operator-specific names on top of it.
func baselineLeakTokens() []string {
	return parseLeakTokenLines(baselineLeakTokensRaw)
}

// loadDocLeakTokens returns the armed doc-leak token set: the always-present tracked baseline
// plus, when present, the operator's additive docs_private/leak-tokens.txt overlay.
// overlayPresent reports ONLY whether the operator overlay was found — the gate itself is armed
// regardless (the baseline is embedded), so a fresh clone never fails closed while the operator
// home keeps full strictness (baseline + overlay).
func loadDocLeakTokens(repoRoot string) (toks []string, overlayPresent bool) {
	toks = append(toks, baselineLeakTokens()...)
	overlay, present := loadLeakTokens(filepath.Join(repoRoot, "docs_private", "leak-tokens.txt"))
	toks = append(toks, overlay...)
	return toks, present
}

func loadLeakTokens(path string) (toks []string, present bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return parseLeakTokenLines(string(b)), true
}

// parseLeakTokenLines normalizes a token list body (baseline or overlay): one token per
// line, '#' comments and blanks dropped, lowercased for case-insensitive substring match.
func parseLeakTokenLines(raw string) []string {
	var toks []string
	for _, line := range strings.Split(raw, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			toks = append(toks, strings.ToLower(t))
		}
	}
	return toks
}

// ConvergePendingBudget is the RATCHET: the count of converge→retire packages must never exceed
// this. `--strict` fails if it grows (new fragmentation). Lower it as packages converge onto the
// spine — the budget can only go down, so the debt can never become wallpaper or quietly grow.
const ConvergePendingBudget = 0

// MaxNonTestCompoundFiles is the RATCHET on the count of non-test .go files directly under
// cmd/compound/. `*_test.go` does not count (tests are the spine, not the debt), and the
// `cmd/compound-*` siblings (compound-install-verify, compound-breadth-engine,
// compound-memory-cli-engine, compound-mock-engine) are NOT measured here — this gate is the
// cmd/compound MONOLITH budget, and a sibling that needs to grow either graduates into
// internal/ or ships with its own dedicated gate. `--strict` fails when the actual count
// exceeds this ceiling. The budget only ratchets DOWN, so new code lands by `git mv
// cmd/compound/foo.go internal/<organ>/`, not by appending to cmd/compound/. Raising this
// ceiling to fit is the forbidden weaken-the-gate move.
//
// Ratcheted 114 -> 113 when executorlens_cli.go graduated into internal/executorlens as
// that organ's RunCLI face. Lowering on an extraction is the ratchet doing its job: the
// gain is locked in so the freed slot cannot be spent by the next append.
//
// Ratcheted 113 -> 112 when actuator_cli.go graduated into internal/actuator by the same
// RunCLI-face pattern. cmd/compound holds 111 after the move, so the one free slot it had
// before is preserved and the extracted slot is locked in rather than handed to the next
// append.
const MaxNonTestCompoundFiles = 112

// MaxNonTestCompoundLOC is the RATCHET on total non-test .go physical lines under
// cmd/compound/. Same exclusion rules as MaxNonTestCompoundFiles (no `*_test.go`, no
// `cmd/compound-*` siblings, no `internal/`). Both ratchets are read independently by
// checkCmdCompoundBudget — a gate that only watched files would let a single file bloat
// unchecked, and a gate that only watched LOC would let file count fragment unchecked.
// `--strict` fails when either is exceeded; the budget only ratchets DOWN.
// Ratcheted 52791 -> 52300 when actuator_cli.go graduated into internal/actuator (800
// non-test lines left cmd/compound, which sits at 51956 after the move). The ratchet does
// NOT take all 800: the in-flight release-hardening branch is over the old ceiling and its
// fixes are real, so ~300 lines are deliberately left as landing room for it and 491 are
// locked in. This is the ratchet accepting a smaller step, never a raise. Ratchet the rest
// once that branch lands -- extraction headroom is a loan to a named branch, not a budget.
const MaxNonTestCompoundLOC = 52300

// MaxStatusHotPathNarrationLines is the RATCHET on status.md's PROSE, and it deliberately does not
// count the package-table rows that checkPackageTableSingleSource MANDATES (one row per
// arch.Classification entry). Counting them put two gates of this same doctor in direct conflict:
// minting an organ is required to add a row, and the row pushed the file past a total-line cap — so
// on 2026-08-01 the ADR-0005 `persist` lane went red for doing exactly the right thing, and the
// cheapest way to dodge the gate was to bury new code in cmd/compound instead, where nothing is
// budgeted at all. The threat this bounds is narration/journal accretion, not the parity table,
// whose size is a function of how many packages the system honestly has.
//
// This is NOT a loosened budget: the prose allowance is unchanged. The old cap left 250-61=189 lines
// for narration once the 61 mandated rows were paid for, and 189 is exactly what it is here. Like
// ConvergePendingBudget, it only ever goes DOWN — to add prose, subtract prose.
const MaxStatusHotPathNarrationLines = 189

// Doctor runs EVERY check: the source-scoped checks plus the three runtime audits (the live
// proveable-pair law, the installed build freshness and the installed sidecar freshness).
//
// repoRoot is the compound repo root for filesystem checks (classification totality, doc leak);
// pass "" to skip every repository-scoped check. IMPORTANT: "" does NOT mean "in-memory only" —
// the proveable-pair law still audits the AMBIENT COMPOUND_HOME (it resolves its own home), so
// the empty-repoRoot run is candidate map + live pair law + convergence ratchet. A caller that
// must not touch ambient runtime state wants DoctorSource, not Doctor(""). strict turns the
// convergence-debt ratchet into a hard failure if the debt exceeds its budget. It never mutates.
func Doctor(repoRoot string, strict bool) Report {
	return doctor(repoRoot, strict, true)
}

// DoctorSource runs ONLY the source-scoped checks: the candidate map, every repository
// source/contract/leak/budget/classification check, the mint ratchet, the cmd/compound budget
// and the convergence ratchet.
//
// It NEVER resolves or scans an ambient Compound home, NEVER inspects installed artifacts and
// NEVER executes an installed Compound binary. The three runtime audits Doctor performs are not
// filtered out of a finished report — they are not run at all — so a source qualification cannot
// be perturbed by unrelated mutable runtime records or by the operator's installation.
//
// repoRoot and strict carry the same meaning as in Doctor, so the source findings are the same
// findings with the same ordering, severities and OK calculation; the source report is a strict
// subset of the full report's source findings.
func DoctorSource(repoRoot string, strict bool) Report {
	return doctor(repoRoot, strict, false)
}

// doctor is the SINGLE implementation of the check inventory — there is no second copy of the
// source-check list and no post-hoc filtering. includeRuntime gates exactly the three runtime
// audits named above; every other check is source-scoped and always runs. When includeRuntime is
// true the finding order, severities and OK calculation are byte-identical to the pre-split
// Doctor.
func doctor(repoRoot string, strict, includeRuntime bool) Report {
	var f []Finding
	f = append(f, checkCandidateMap()...)
	if includeRuntime {
		f = append(f, checkProveablePairLaw("", strict)...)
	}
	if repoRoot != "" {
		f = append(f, checkClassificationTotal(repoRoot)...)
		f = append(f, checkPackageClassificationTableContract(repoRoot)...)
		f = append(f, checkTransitionFieldContract(repoRoot)...)
		f = append(f, checkEventStoreOwnership(repoRoot)...)
		f = append(f, checkEventStorePublicContract(repoRoot)...)
		f = append(f, checkDocLeak(repoRoot, strict)...)
		f = append(f, checkPrivateSourceBodyContract(repoRoot)...)
		f = append(f, checkStatusHotPath(repoRoot)...)
		f = append(f, checkRetiredSurfaceReferences(repoRoot)...)
		f = append(f, checkRetiredGen1PackagesStayDead(repoRoot)...)
		f = append(f, checkDistillerDuplicateContract(repoRoot)...)
		f = append(f, checkLLMExtractionGroundingContract(repoRoot)...)
		f = append(f, checkDistillerCandidateShapeContract(repoRoot)...)
		f = append(f, checkRouteOutcomeContract(repoRoot)...)
		f = append(f, checkKnowledgePageContract(repoRoot)...)
		f = append(f, checkWorkPacketContract(repoRoot)...)
		f = append(f, checkWorkProofChainContract(repoRoot)...)
		f = append(f, checkMCPorterTopologyContract(repoRoot)...)
		f = append(f, checkPluginServingContract(repoRoot)...)
		f = append(f, checkMemoryEngineContract(repoRoot)...)
		f = append(f, checkHarnessEntrypointContract(repoRoot)...)
		f = append(f, checkHookSourceControlContract(repoRoot)...)
		f = append(f, checkHookLiveParityContextSpineContract(repoRoot)...)
		f = append(f, checkHookReadinessActionContract(repoRoot)...)
		f = append(f, checkStandingGrantContract(repoRoot)...)
		f = append(f, checkPlacementGuardContract(repoRoot)...)
		f = append(f, checkProductContractContract(repoRoot)...)
		f = append(f, checkProductLoopOpenHypothesisContract(repoRoot)...)
		f = append(f, checkOperatorProductLoopProjectionContract(repoRoot)...)
		f = append(f, checkObservationBatchContract(repoRoot)...)
		f = append(f, checkOperatorObservationReviewProjectionContract(repoRoot)...)
		f = append(f, checkProductObservationVerdictContract(repoRoot)...)
		f = append(f, checkSourceObservationVerdictContract(repoRoot)...)
		f = append(f, checkPanelCapabilityFeedbackContract(repoRoot)...)
		f = append(f, checkCapabilityEvaluationContract(repoRoot)...)
		f = append(f, checkExternalRepoContextEvaluationContract(repoRoot)...)
		f = append(f, checkExternalRepoIntakeStateProjectionContract(repoRoot)...)
		f = append(f, checkConversationMiningContract(repoRoot)...)
		f = append(f, checkConversationCoverageContract(repoRoot)...)
		f = append(f, checkDecisionSourceAuthorityContract(repoRoot)...)
		f = append(f, checkDistillerImprovementReviewContract(repoRoot)...)
		f = append(f, checkEpistemicsDoctrine(repoRoot)...)
		f = append(f, checkPrivateAdaptationSteering(repoRoot)...)
		f = append(f, checkReviewSurfaceCoverageContract(repoRoot)...)
		f = append(f, checkWorkbenchActionCardContract(repoRoot)...)
		f = append(f, checkHomeDoctorOperationalHygieneContract(repoRoot)...)
		f = append(f, checkConversationPreflightImportSafetyContract(repoRoot)...)
		f = append(f, checkOperatorNeedsMeCandidateAuthorityContract(repoRoot)...)
		f = append(f, checkOperatorOrientAdjacentProgressContract(repoRoot)...)
		f = append(f, checkOperatorSourceControlProjectionContract(repoRoot)...)
		f = append(f, checkASCDailySignalsPublicBoundaryContract(repoRoot)...)
		f = append(f, checkPublicAlphaSupportedScopeContract(repoRoot)...)
		if includeRuntime {
			f = append(f, checkBuildFreshnessContract(repoRoot)...)
			f = append(f, checkSidecarFreshnessContract(repoRoot)...)
		}
		f = append(f, checkMintTwoClassHome(repoRoot)...)
		f = append(f, checkCmdCompoundBudget(repoRoot, strict)...)
	}
	f = append(f, convergenceDebt(strict)...)
	ok := true
	for _, x := range f {
		if x.Severity == Fail {
			ok = false
		}
	}
	return Report{Findings: f, OK: ok}
}

// checkCandidateMap (pure): every kind → one non-empty inbox dir, HabitatCatalog creates it, and every
// kind declares a consumer (no orphan inbox). This is the B1 invariant, enforced.
func checkCandidateMap() []Finding {
	var f []Finding
	homeDirs := map[string]bool{}
	for _, d := range home.HabitatDirs() {
		homeDirs[d] = true
	}
	for _, k := range candidates.AllKinds() {
		spec, _ := candidates.SpecFor(k)
		if spec.InboxDir == "" {
			f = append(f, Finding{"candidate-map", Fail, "kind " + string(k) + " has no inbox dir"})
			continue
		}
		if !homeDirs[spec.InboxDir] {
			f = append(f, Finding{"candidate-map", Fail, "kind " + string(k) + " dir " + spec.InboxDir + " is not created by HabitatCatalog"})
		}
		if strings.TrimSpace(spec.Consumer) == "" {
			f = append(f, Finding{"candidate-map", Fail, "kind " + string(k) + " has no consumer (orphan inbox)"})
		}
	}
	f = append(f, Finding{"candidate-map", Info, "checked " + itoa(len(candidates.AllKinds())) + " candidate kinds → one home each"})
	return f
}

// checkClassificationTotal: every dir under internal/ must be classified, and every classification
// must name a real dir. An unclassified package fails — a new front door cannot appear silently.
func checkClassificationTotal(repoRoot string) []Finding {
	var f []Finding
	entries, err := os.ReadDir(filepath.Join(repoRoot, "internal"))
	if err != nil {
		return []Finding{{"classification", Warn, "could not read internal/: " + err.Error()}}
	}
	found := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found[e.Name()] = true
		if _, ok := Classification[e.Name()]; !ok {
			f = append(f, Finding{"classification", Fail, "internal/" + e.Name() + " is UNCLASSIFIED — classify it in arch.Classification (spine/object-family/leaf/mem-target/converge-pending/importer) so it isn't a silent new front door"})
		}
	}
	for pkg := range Classification {
		if !found[pkg] {
			f = append(f, Finding{"classification", Warn, "arch.Classification names internal/" + pkg + " but the dir is gone (retired?) — drop it from the map"})
		}
	}
	f = append(f, checkSpineCoreDrift(Classification, spineCore)...)
	if len(f) == 0 {
		f = append(f, Finding{"classification", Info, "all " + itoa(len(found)) + " internal/ packages classified"})
	}
	return f
}

// checkSpineCoreDrift enforces that Spine membership stays a conscious allowlist: the spineCore set
// and the packages classified Spine must name exactly the same packages. Two Fail directions: a
// package classified Spine with no spineCore entry (someone widened the authoritative spine without
// an allowlist edit), and a spineCore entry not classified Spine (map drift). Pure (map-only) so the
// ratchet is testable without the filesystem.
func checkSpineCoreDrift(classification map[string]Class, core map[string]bool) []Finding {
	var f []Finding
	var unlisted []string
	for pkg, c := range classification {
		if c == Spine && !core[pkg] {
			unlisted = append(unlisted, pkg)
		}
	}
	sort.Strings(unlisted)
	for _, pkg := range unlisted {
		f = append(f, Finding{"classification", Fail, "internal/" + pkg + " is classified Spine but is not in the spineCore allowlist — spine membership is a conscious allowlist edit; classify a new package as object-family/leaf/importer instead of joining the runtime spine"})
	}
	var drift []string
	for pkg := range core {
		if classification[pkg] != Spine {
			drift = append(drift, pkg)
		}
	}
	sort.Strings(drift)
	for _, pkg := range drift {
		f = append(f, Finding{"classification", Fail, "spineCore names internal/" + pkg + " but it is not classified Spine (map drift) — keep spineCore and the Spine classification in lockstep"})
	}
	return f
}

// packageTablePkgToken extracts a backtick-quoted `internal/` package name
// from a package-table cell.
var packageTablePkgToken = regexp.MustCompile("`([a-z][a-z0-9]*)`")

// splitRawTableCells splits one markdown table row into its raw (untrimmed of
// backticks) cell texts, so callers can regex the package-name cell without
// losing internal backtick pairs the way a whole-cell backtick trim would.
func splitRawTableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil
	}
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// parseStatusPackageTable extracts the `## Packages (`internal/`)` table from
// status.md's text into package -> declared Class-string (lowercased). It
// tolerates a first cell naming more than one backtick-quoted package (a
// legacy grouped row) by attributing the row's class to every named package,
// and reports every package name encountered (including duplicates) via
// order so the caller can flag a package listed on more than one row.
func parseStatusPackageTable(text string) (classes map[string]string, order []string) {
	classes = map[string]string{}
	inSection := false
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inSection {
			if strings.HasPrefix(trimmed, "## Packages") {
				inSection = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			break // left the Packages section
		}
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := splitRawTableCells(line)
		if len(cells) < 2 {
			continue
		}
		if !inTable {
			if strings.EqualFold(strings.Trim(cells[0], "`"), "package") {
				inTable = true
			}
			continue
		}
		if isMarkdownSeparatorRow(cells) {
			continue
		}
		names := packageTablePkgToken.FindAllStringSubmatch(cells[0], -1)
		if len(names) == 0 {
			continue
		}
		classText := strings.ToLower(cells[1])
		for _, m := range names {
			pkg := m[1]
			classes[pkg] = classText
			order = append(order, pkg)
		}
	}
	return classes, order
}

// checkPackageClassificationTableContract is the mechanical half of arch.go's
// own package doc comment ("the package Classification below is the ENFORCED
// source of truth; docs/architecture/status.md narrates it"): narration must
// MATCH the enforced map, not merely exist. It fails when docs/architecture/
// status.md's `## Packages (`internal/`)` table diverges from
// arch.Classification in any of three ways: a classified package missing
// from the table, a table row naming a package Classification does not
// classify (stale/retired), or a package present in both whose table Class
// string is not the EXACT lowercase Class value (spine/object-family/leaf/
// mem-target/converge-pending/importer) — a bare "canonical" or prose
// description is not a substitute for the class token, because that is
// exactly the drift that let build-layer packages go unrepresented.
func checkPackageClassificationTableContract(repoRoot string) []Finding {
	path := filepath.Join(repoRoot, "docs", "architecture", "status.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return []Finding{{"package-table-single-source", Fail, "could not read docs/architecture/status.md: " + err.Error()}}
	}
	tableClasses, order := parseStatusPackageTable(string(b))

	seen := map[string]int{}
	for _, pkg := range order {
		seen[pkg]++
	}
	var problems []string
	var duplicates []string
	for pkg, count := range seen {
		if count > 1 {
			duplicates = append(duplicates, pkg)
		}
	}
	sort.Strings(duplicates)
	for _, pkg := range duplicates {
		problems = append(problems, "internal/"+pkg+" appears on more than one status.md package-table row")
	}

	var missing []string
	var wrongClass []string
	for pkg, class := range Classification {
		want := string(class)
		got, ok := tableClasses[pkg]
		if !ok {
			missing = append(missing, pkg)
			continue
		}
		if got != want {
			wrongClass = append(wrongClass, "internal/"+pkg+" is classified "+strconv(want)+" in arch.Classification but status.md's table says "+strconv(got))
		}
	}
	sort.Strings(missing)
	sort.Strings(wrongClass)
	problems = append(problems, wrongClass...)
	if len(missing) > 0 {
		problems = append(problems, "status.md package table is missing "+itoa(len(missing))+" classified package(s): "+strings.Join(missing, ", "))
	}

	var extra []string
	for pkg := range tableClasses {
		if _, ok := Classification[pkg]; !ok {
			extra = append(extra, pkg)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		problems = append(problems, "status.md package table names internal/"+strings.Join(extra, ", internal/")+" which arch.Classification does not classify (stale/retired?)")
	}

	if len(problems) > 0 {
		return []Finding{{"package-table-single-source", Fail, "docs/architecture/status.md package table diverges from arch.Classification: " + strings.Join(problems, "; ")}}
	}
	return []Finding{{"package-table-single-source", Info, "docs/architecture/status.md package table matches arch.Classification for all " + itoa(len(Classification)) + " classified packages"}}
}

type transitionFieldSpec struct {
	GoType  string
	JSONTag string
}

// checkTransitionFieldContract keeps transition.md's direct Go-field contract synchronized with
// envelope.Transition. Header is one anonymous field here; its separately documented universal
// fields flatten into Transition's JSON representation.
func checkTransitionFieldContract(repoRoot string) []Finding {
	actual, err := readStructFieldContract(filepath.Join(repoRoot, "internal", "envelope"), "Transition")
	if err != nil {
		return []Finding{{"transition-field-contract", Fail, "could not inspect envelope.Transition: " + err.Error()}}
	}
	docPath := filepath.Join(repoRoot, "docs", "architecture", "transition.md")
	b, err := os.ReadFile(docPath)
	if err != nil {
		return []Finding{{"transition-field-contract", Fail, "could not read docs/architecture/transition.md: " + err.Error()}}
	}
	documented, duplicates, err := parseTransitionFieldTable(string(b))
	if err != nil {
		return []Finding{{"transition-field-contract", Fail, "could not parse docs/architecture/transition.md Transition field contract: " + err.Error()}}
	}

	var problems []string
	for _, name := range duplicates {
		problems = append(problems, "documented field "+name+" appears more than once")
	}
	var actualNames []string
	for name := range actual {
		actualNames = append(actualNames, name)
	}
	sort.Strings(actualNames)
	for _, name := range actualNames {
		want := actual[name]
		got, ok := documented[name]
		if !ok {
			problems = append(problems, "docs missing envelope.Transition field "+name)
			continue
		}
		if got.GoType != want.GoType {
			problems = append(problems, "field "+name+" has Go type "+strconv(want.GoType)+" but transition.md says "+strconv(got.GoType))
		}
		if got.JSONTag != want.JSONTag {
			problems = append(problems, "field "+name+" has JSON tag "+strconv(want.JSONTag)+" but transition.md says "+strconv(got.JSONTag))
		}
	}
	var extra []string
	for name := range documented {
		if _, ok := actual[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		problems = append(problems, "transition.md documents non-existent envelope.Transition field "+name)
	}
	if len(problems) > 0 {
		return []Finding{{"transition-field-contract", Fail, "docs/architecture/transition.md diverges from envelope.Transition: " + strings.Join(problems, "; ")}}
	}
	return []Finding{{"transition-field-contract", Info, "docs/architecture/transition.md matches all " + itoa(len(actual)) + " direct envelope.Transition fields"}}
}

func readStructFieldContract(dir, structName string) (map[string]transitionFieldSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var found map[string]transitionFieldSpec
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, rawSpec := range gen.Specs {
				typeSpec, ok := rawSpec.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != structName {
					continue
				}
				if found != nil {
					return nil, fmt.Errorf("type %s declared more than once", structName)
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					return nil, fmt.Errorf("type %s is not a struct", structName)
				}
				found, err = structFieldContract(fset, structType)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("type %s not found in %s", structName, dir)
	}
	return found, nil
}

func structFieldContract(fset *token.FileSet, structType *ast.StructType) (map[string]transitionFieldSpec, error) {
	out := map[string]transitionFieldSpec{}
	for _, field := range structType.Fields.List {
		var typeBuf bytes.Buffer
		if err := format.Node(&typeBuf, fset, field.Type); err != nil {
			return nil, err
		}
		tag := ""
		if field.Tag != nil {
			tag = strings.Trim(field.Tag.Value, "`")
		}
		names := field.Names
		if len(names) == 0 {
			name := embeddedFieldName(field.Type)
			if name == "" {
				return nil, fmt.Errorf("unsupported anonymous field type %s", typeBuf.String())
			}
			names = []*ast.Ident{{Name: name}}
		}
		for _, name := range names {
			if _, exists := out[name.Name]; exists {
				return nil, fmt.Errorf("field %s declared more than once", name.Name)
			}
			out[name.Name] = transitionFieldSpec{GoType: typeBuf.String(), JSONTag: tag}
		}
	}
	return out, nil
}

func embeddedFieldName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	case *ast.StarExpr:
		return embeddedFieldName(value.X)
	default:
		return ""
	}
}

func parseTransitionFieldTable(text string) (map[string]transitionFieldSpec, []string, error) {
	out := map[string]transitionFieldSpec{}
	var duplicates []string
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		cells := splitMarkdownTableRow(line)
		if !inTable {
			if len(cells) == 3 && strings.EqualFold(cells[0], "Go field") && strings.EqualFold(cells[1], "Go type") && strings.EqualFold(cells[2], "JSON tag") {
				inTable = true
			}
			continue
		}
		if len(cells) == 0 {
			break
		}
		if isMarkdownSeparatorRow(cells) {
			continue
		}
		if len(cells) != 3 {
			return nil, nil, fmt.Errorf("field row has %d cells, want 3", len(cells))
		}
		name := cleanEpistemicsCell(cells[0])
		goType := cleanEpistemicsCell(cells[1])
		jsonTag := cleanEpistemicsCell(cells[2])
		if jsonTag == "-" {
			jsonTag = ""
		}
		if name == "" || goType == "" {
			return nil, nil, fmt.Errorf("field name and Go type must be non-empty")
		}
		if _, exists := out[name]; exists {
			duplicates = append(duplicates, name)
		}
		out[name] = transitionFieldSpec{GoType: goType, JSONTag: jsonTag}
	}
	if !inTable {
		return nil, nil, fmt.Errorf("missing | Go field | Go type | JSON tag | table")
	}
	sort.Strings(duplicates)
	return out, duplicates, nil
}

// checkStatusHotPath keeps status.md bounded as a hot map, not a runtime proof journal.
func checkStatusHotPath(repoRoot string) []Finding {
	path := filepath.Join(repoRoot, "docs", "architecture", "status.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return []Finding{{"status-hot-path", Warn, "could not read docs/architecture/status.md: " + err.Error()}}
	}
	narration, mandated := statusNarrationLineCount(b)
	if narration > MaxStatusHotPathNarrationLines {
		return []Finding{{"status-hot-path", Fail, "docs/architecture/status.md carries " + itoa(narration) + " narration lines (excluding " + itoa(mandated) + " mandated package-table rows); keep narration at or below " + itoa(MaxStatusHotPathNarrationLines) + " lines — subtract prose or move proof journal entries to docs/architecture/archive/, and never raise the budget to fit"}}
	}
	var journalLines []string
	for i, line := range strings.Split(string(b), "\n") {
		if statusProofJournalLine.MatchString(line) {
			journalLines = append(journalLines, "line "+itoa(i+1))
		}
	}
	if len(journalLines) > 0 {
		return []Finding{{"status-hot-path", Fail, "docs/architecture/status.md contains proof-journal shaped entries (" + strings.Join(journalLines, ", ") + "); keep status as current owner/surface map and move dated proof logs to docs/architecture/archive/"}}
	}
	return []Finding{{"status-hot-path", Info, "docs/architecture/status.md narration is " + itoa(narration) + "/" + itoa(MaxStatusHotPathNarrationLines) + " lines (" + itoa(mandated) + " mandated package-table rows excluded) with no proof-journal entries"}}
}

// statusNarrationLineCount splits status.md into the prose the budget bounds and the package-table
// rows checkPackageTableSingleSource mandates. A row only stops counting while it is inside the
// `## Packages` section AND shaped like a table row — prose that merely mentions a package still
// counts, so narration cannot be smuggled past the ratchet by parking it in the table's section.
func statusNarrationLineCount(b []byte) (narration int, mandated int) {
	inPackages := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "## Packages"):
			inPackages = true
		case strings.HasPrefix(line, "## "):
			inPackages = false
		}
		if inPackages && strings.HasPrefix(strings.TrimSpace(line), "| `") {
			mandated++
			continue
		}
		narration++
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		narration-- // the trailing newline is not a line, matching lineCount's contract
	}
	return narration, mandated
}

func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := strings.Count(string(b), "\n")
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// checkDocLeak: no GENERIC operator-private identity shape (email, absolute /Users|/home
// path) in ANY tracked text file, and no configured product/example leak token in the
// docs/skills prose surfaces or the public source fixtures/tests. docs_private is
// gitignored; public core knows scan mechanics, not private names.
//
// Enumeration is the TRACKED-FILE universe (leakScanUniverse: `git ls-files`, or a
// skip-guarded filesystem walk when repoRoot is not a git worktree) — exactly what a
// public clone ships, root files included. The GENERIC identity pass (findGenericLeak)
// runs over that ENTIRE universe: this is the widened, CI-portable floor — armed with no
// operator overlay, reaching root files (README.md, AGENTS.md, go.mod, .github/**) and
// every source root, not just docs/skills as before. The two TOKEN passes
// (docs_private/leak-tokens.txt, docs_private/source-leak-tokens.txt) keep their
// PRE-EXISTING root scoping (docs+skills prose vs fixtures/internal/cmd/tools/console
// source) by filtering the same enumerated universe by prefix — only the enumeration
// mechanism under them changed (filesystem walk -> tracked-file set), not their reach, so
// existing token-class tests keep pinning identical behavior. One finding per line per
// class: the generic and token passes run in the SAME loop over the SAME file list, so a
// line can never be double-reported by two overlapping walks.
func checkDocLeak(repoRoot string, strict bool) []Finding {
	var f []Finding
	// strict is unused for arming now: the embedded baseline arms the gate by default, so
	// there is no fail-closed-on-missing-overlay branch. It is kept in the signature because
	// the doctor threads --strict uniformly and other gates read it.
	_ = strict
	privateToks, overlayPresent := loadDocLeakTokens(repoRoot)
	if !overlayPresent {
		// The embedded baseline arms the gate, so a missing operator overlay is NOT a
		// fail-closed condition (a fresh clone/worktree must pass proof-check). It is a
		// non-fatal nudge to add the overlay in the operator home for operator-specific
		// tool/product names.
		f = append(f, Finding{"doc-leak", Info, "operator overlay docs_private/leak-tokens.txt absent; tracked baseline leak-token gate armed (" + itoa(len(baselineLeakTokens())) + " generic secret-shape tokens). Add the overlay in the operator home for operator-specific tool/product names."})
	}
	sourceToks, sourcePresent := loadSourceLeakTokens(repoRoot)

	rels, enumerateErr := leakScanUniverse(repoRoot)
	if enumerateErr != nil {
		f = append(f, Finding{"doc-leak", Fail, "tracked-file enumeration failed: " + enumerateErr.Error()})
	} else {
		for _, rel := range rels {
			b, err := readLeakScanBody(repoRoot, rel)
			if err != nil {
				f = append(f, Finding{"doc-leak", Fail, rel + ": cannot read tracked text: " + err.Error()})
				continue
			}
			text, isText := decodeLeakScanBody(b)
			if !isText {
				continue
			}
			inDocRoot := relUnderAnyPrefix(rel, docLeakScanRootPrefixes)
			inSourceRoot := sourcePresent && relUnderAnyPrefix(rel, sourceLeakScanRootPrefixes)
			for i, line := range strings.Split(text, "\n") {
				// GENERIC identity-shape pass: the full tracked universe, no root scoping.
				if m := findGenericLeak(line); m != "" {
					f = append(f, Finding{"doc-leak", Fail, rel + ":" + itoa(i+1) + " leaks " + strconv(m)})
				}
				low := strings.ToLower(line)
				if inDocRoot {
					for _, t := range privateToks {
						if strings.Contains(low, t) {
							f = append(f, Finding{"doc-leak", Fail, rel + ":" + itoa(i+1) + " leaks private token " + strconv(t)})
						}
					}
				}
				if inSourceRoot {
					for _, t := range sourceToks {
						if strings.Contains(low, t) {
							f = append(f, Finding{"doc-leak", Fail, rel + ":" + itoa(i+1) + " leaks private token " + strconv(t)})
						}
					}
				}
			}
		}
	}

	hasFail := false
	for _, finding := range f {
		if finding.Severity == Fail {
			hasFail = true
			break
		}
	}
	if !hasFail {
		msg := "all tracked text blobs clean for generic identity shapes (root + docs + skills + source); public docs + ceremonies clean for baseline leak tokens"
		if overlayPresent {
			msg += " + operator overlay tokens"
		}
		if sourcePresent {
			msg += "; configured source surfaces clean"
		}
		f = append(f, Finding{"doc-leak", Info, msg})
	}
	return f
}

// docLeakScanRootPrefixes are the repo-relative PROSE surfaces the operator-overlay
// token pass (docs_private/leak-tokens.txt) scans: docs/ and the tracked build-ceremony
// bodies under skills/ (fresh-clone reachable, so a leak into a ceremony body is caught
// by the same armed gate that guards docs). Scoping is UNCHANGED from before the
// tracked-file-enumeration rewrite — only the enumeration mechanism changed.
var docLeakScanRootPrefixes = []string{"docs", "skills"}

// sourceLeakScanRootPrefixes are the repo-relative surfaces the optional
// docs_private/source-leak-tokens.txt overlay token pass scans. Scoping unchanged.
// scripts/ is in the list because operator automation is where private tool and
// local-project names actually enter the tree: a budget manifest or a launchd
// template enumerates the operator's real home layout, and nothing else scanned
// it. Adding it was free — the tracked scripts/ tree is already clean.
var sourceLeakScanRootPrefixes = []string{"fixtures", "internal", "cmd", "tools", "console", "scripts"}

// relUnderAnyPrefix reports whether repo-relative path rel is exactly one of prefixes or
// descends into one of them (rel == p or rel starts with p + "/").
func relUnderAnyPrefix(rel string, prefixes []string) bool {
	relSlash := filepath.ToSlash(rel)
	for _, p := range prefixes {
		if relSlash == p || strings.HasPrefix(relSlash, p+"/") {
			return true
		}
	}
	return false
}

func loadSourceLeakTokens(repoRoot string) (toks []string, present bool) {
	return loadLeakTokens(filepath.Join(repoRoot, "docs_private", "source-leak-tokens.txt"))
}

// leakScanEnumerateSkipDirs is the fallback-walk skip set used ONLY when repoRoot is not
// a git worktree (several existing tests build a plain temp-dir fixture repo, not a real
// git repo). The primary enumeration is `git ls-files` (the tracked set — exactly what
// ships publicly), which excludes docs_private/, node_modules/, .next/, dist/ by
// construction; this skip set exists only so the plain-dir fallback keeps the same
// semantics without a git repo to ask.
var leakScanEnumerateSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"docs_private": true,
	".claude":      true,
	".next":        true,
	"dist":         true,
}

// leakScanUniverse enumerates every candidate doc-leak scan target as a
// repo-relative path. A .git file/directory positively identifies repoRoot as a
// Git checkout (normal clone or linked worktree); there, `git ls-files -z` is
// authoritative and ANY enumeration failure is returned to the caller to fail
// closed. Only a root with no .git metadata uses the plain-directory fixture
// fallback. No extension filter belongs here: readLeakScanBody +
// decodeLeakScanBody classify content, so extensionless and unfamiliar tracked
// text blobs receive the same identity/token floor.
func leakScanUniverse(repoRoot string) ([]string, error) {
	_, err := os.Lstat(filepath.Join(repoRoot, ".git"))
	switch {
	case err == nil:
		tracked, trackedErr := gitTrackedFiles(repoRoot)
		if trackedErr != nil {
			return nil, fmt.Errorf("git ls-files: %w", trackedErr)
		}
		return tracked, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("inspect .git metadata: %w", err)
	}

	var rels []string
	err = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != repoRoot && leakScanEnumerateSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk plain-directory fixture: %w", err)
	}
	return rels, nil
}

// readLeakScanBody reads the content that a public checkout actually exposes.
// Symlinks are scanned as their link-target blob/string and are never followed
// into external content. Missing, unreadable, or unexpected tracked path types
// return an error so the caller can fail closed instead of reporting clean.
func readLeakScanBody(repoRoot, rel string) ([]byte, error) {
	path := filepath.Join(repoRoot, rel)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return nil, readErr
		}
		return []byte(target), nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file or symlink")
	}
	return os.ReadFile(path)
}

// decodeLeakScanBody accepts every valid UTF-8 body, including bodies containing
// NUL, and BOM-marked UTF-16LE/BE. A NUL byte is valid text and cannot by itself
// exempt an otherwise textual tracked blob from the leak floor. Invalid bodies
// remain genuine-binary discriminators. This is shared by generic and token
// passes.
func decodeLeakScanBody(body []byte) (string, bool) {
	if utf8.Valid(body) {
		return string(body), true
	}
	if len(body) < 2 {
		return "", false
	}
	littleEndian := body[0] == 0xff && body[1] == 0xfe
	bigEndian := body[0] == 0xfe && body[1] == 0xff
	if !littleEndian && !bigEndian {
		return "", false
	}
	payload := body[2:]
	if len(payload)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(payload)/2)
	for i := 0; i < len(payload); i += 2 {
		if littleEndian {
			units[i/2] = uint16(payload[i]) | uint16(payload[i+1])<<8
		} else {
			units[i/2] = uint16(payload[i])<<8 | uint16(payload[i+1])
		}
	}
	return string(utf16.Decode(units)), true
}

// leakScanFile is the deliberately narrower file-kind filter owned by
// checkPrivateSourceBodyContract. The generic/token doc-leak passes classify
// content instead; keeping these scopes separate prevents this legacy
// semantic-body guard from widening as collateral.
func leakScanFile(path string) bool {
	ext := filepath.Ext(path)
	switch ext {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".md", ".json", ".jsonl", ".yaml", ".yml", ".toml", ".txt":
		return true
	default:
		return false
	}
}

// privateSourceBodyMarker matches a `source_of_truth: <private-source>`
// frontmatter/marker line — the semantic signature of a page whose BODY was
// authored as a private brain / operating-system decision / infra page and
// copied in verbatim. The value alternation covers the private source families
// W5 absorbs (brain pages, operating-system decisions, infra YAML). checkDocLeak
// has no concept of this marker (it only matches path/email SHAPES and an
// operator-configured literal token list), so this closes a distinct gap. The
// `^\s*` anchor is load-bearing: this guard's OWN regex/comment literals in
// arch.go carry the substring `source_of_truth: brain`, but never at line-start,
// so the doctor never flags its own source. (A prose mention like
// "...carry the source_of_truth handoff..." mid-sentence is likewise unmatched.)
var privateSourceBodyMarker = regexp.MustCompile(`(?i)^\s*source_of_truth\s*:\s*(brain|operating-system|decisions|infra|gbrain)\b`)

// privateSourceBodyPathPrefix matches an ABSOLUTE macOS home-directory path
// literal that descends into a private source tree by name — the brain notes
// tree, the operating-system control-plane tree, or the gbrain corpus tree
// (`.gbrain`) — proof a private source tree's actual on-disk LOCATION (not just
// a name) was copied into a tracked file. The absolute `/Users/<user>/` form is
// the reliable body-copy tell because it leaks the operator's real home layout;
// `.compound` is deliberately EXCLUDED because it is compound's OWN home and is
// documented publicly across the repo (`~/.compound` appears legitimately in
// README/console/runtime docs). This is NARROWER than checkDocLeak's genericLeak
// (`/Users/[a-z]`, which matches ANY home path but runs only over docs/); this
// guard catches the brain/operating-system/gbrain path SHAPE wherever it lands
// outside test fixtures. The `[^\s"']` class also spares arch.go's own regex
// literal (the `"'` in the class text stops the match before it can span the
// literal), so the guard never trips over its own source.
var privateSourceBodyPathPrefix = regexp.MustCompile(`(?i)/Users/[^\s"']+?/(brain|operating-system|\.gbrain)(/|$)`)

// privateSourceBodyTildePrefix matches the TILDE home form (`~/…`) of private
// source trees that have ZERO legitimate reference use anywhere in the public
// tree — the gbrain corpus tree (`~/.gbrain`) and the operating-system
// control-plane repo (`~/Projects/operating-system`). Both are verified absent
// from tracked files, so matching them is high-precision. Deliberately NOT
// matched: bare `~/brain` (legitimately used as a generic pointer in
// retirement-narration archives and as example config in synthetic path-authority
// fixtures — a reference, not a copied body) and `~/.compound` (compound's own
// public home). Precision over recall: this catches gbrain/os body-copies that
// carry a tilde path header without turning the doctor red on legitimate
// pointer references. `~/.gbrain` requires the `~/` prefix, so it never collides
// with compound's own `compound.gbrain_db_export` schema naming.
var privateSourceBodyTildePrefix = regexp.MustCompile(`(?i)~/(?:\.gbrain|Projects/operating-system)(?:/|$)`)

// privateSourceBodySkipDirs are the fallback-walk skip set used ONLY when
// repoRoot is not a git worktree (temp-dir unit tests, tarball clones). The
// primary scope is `git ls-files` (tracked files — what actually ships), which
// excludes docs_private/ and every other gitignored path BY CONSTRUCTION, so no
// skip list is needed there. .git/node_modules are noise; docs_private is the
// gitignored private overlay this guard must not police (source material, not a
// leak target); the rest mirror .gitignore top-level entries with no tracked source.
var privateSourceBodySkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"docs_private": true,
	".claude":      true,
	"Plans":        true,
	"memory":       true,
	"bin":          true,
	"home":         true,
}

// checkPrivateSourceBodyContract fails --strict (indeed, fails outright: it is a
// Fail-severity finding regardless of strict) when a TRACKED repo file carries
// evidence that a private brain / operating-system decision / infra / gbrain
// source BODY was copied in — enforcing W5 non_goal #1 ("source-in-place only;
// never copy private bodies into the repo") and the repo TL;DR ("never write a
// personal path into a tracked file"). Not a leaked product name (checkDocLeak's
// job), but the structural signatures of a wholesale body-copy: a
// `source_of_truth: <private-source>` frontmatter marker, an absolute
// /Users/<name>/{brain,operating-system,.gbrain}/ path prefix, or a tilde
// ~/{.gbrain,Projects/operating-system}/ home path.
//
// SCOPE is `git ls-files` (tracked files only — exactly what ships publicly);
// docs_private/ is gitignored so it is excluded by construction, and every other
// gitignored path (generated COMPOUND_HOME state, *.private.md, .compound-private/)
// is excluded too — no hand-maintained skip list can drift out of sync with
// .gitignore. When repoRoot is not a git worktree (temp-dir unit tests, tarball
// clones) the check falls back to a filesystem walk with privateSourceBodySkipDirs;
// the walk is a strict superset of the tracked set, so the fallback is more
// conservative, never blind.
//
// Both checks exempt _test.go files by design: this guard's own tests plant
// synthetic sentinel fixtures (as Go string literals) to prove detection, and
// excluding _test.go keeps those plants from ever being live evidence against the
// real repo when the doctor scans internal/arch/arch_test.go itself — the guard
// proves itself via unit calls and a real synthetic git repo, not by tripping
// over its own test source.
func checkPrivateSourceBodyContract(repoRoot string) []Finding {
	var f []Finding
	if tracked, err := gitTrackedFiles(repoRoot); err == nil {
		// Authoritative tracked scope. Even an empty tracked set is trusted (a
		// real git repo with nothing staged scans nothing) — the walk fallback
		// fires only when repoRoot is not a git worktree at all.
		for _, rel := range tracked {
			if !leakScanFile(rel) || strings.HasSuffix(rel, "_test.go") {
				continue
			}
			b, readErr := os.ReadFile(filepath.Join(repoRoot, rel))
			if readErr != nil {
				continue
			}
			f = append(f, scanPrivateSourceBody(rel, b)...)
		}
	} else {
		_ = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if path != repoRoot && privateSourceBodySkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !leakScanFile(path) || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				rel = path
			}
			f = append(f, scanPrivateSourceBody(rel, b)...)
			return nil
		})
	}
	if len(f) == 0 {
		f = append(f, Finding{"private-source-body", Info, "no tracked file carries a source_of_truth private-source marker or a brain/operating-system/gbrain path-prefix body copy"})
	}
	return f
}

// scanPrivateSourceBody is the pure per-file scan (no filesystem/process access),
// so the detection logic is unit-testable without a git repo. It emits one Fail
// per matched line per signature.
func scanPrivateSourceBody(rel string, content []byte) []Finding {
	var f []Finding
	for i, line := range strings.Split(string(content), "\n") {
		if privateSourceBodyMarker.MatchString(line) {
			f = append(f, Finding{"private-source-body", Fail, rel + ":" + itoa(i+1) + " carries a source_of_truth private-source marker — a private source BODY was copied into a tracked file"})
		}
		if m := privateSourceBodyPathPrefix.FindString(line); m != "" {
			f = append(f, Finding{"private-source-body", Fail, rel + ":" + itoa(i+1) + " carries a private source path prefix " + strconv(m)})
		}
		if m := privateSourceBodyTildePrefix.FindString(line); m != "" {
			f = append(f, Finding{"private-source-body", Fail, rel + ":" + itoa(i+1) + " carries a private source home path " + strconv(m)})
		}
	}
	return f
}

// gitTrackedFiles returns the repo-relative paths of every tracked file (`git
// ls-files`), the authoritative set of what ships publicly. The child receives no
// GIT_* environment overrides and no ambient Git config (gitSourceEnumerationEnv),
// and core.fsmonitor is disabled for this invocation, so nothing outside the
// repoRoot checkout can change WHICH SOURCE FILES the doctor enumerates. A
// non-git repoRoot (or missing git) returns an error so the caller falls back to
// a filesystem walk. Bounded by a timeout so the doctor can never hang on a
// wedged git.
func gitTrackedFiles(repoRoot string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// core.fsmonitor is disabled because the fsmonitor hook is an EXTERNAL PROGRAM
	// git runs while listing the index — `git ls-files` runs it, so an ambient (or
	// repo-local) core.fsmonitor made the enumeration execute whatever helper it
	// names. `-c core.fsmonitor=false` is a command-line override, so it outranks
	// every config file and leaves the tracked inventory byte-identical; it changes
	// no index, worktree or submodule fact.
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "-c", "core.fsmonitor=false", "ls-files", "-z")
	cmd.Env = gitSourceEnumerationEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	return files, nil
}

// gitSourceEnumerationEnv builds the environment the source-enumeration child runs
// under. It is the DETERMINISM CONTRACT of that child, kept as one function so the
// contract is inspectable and testable rather than spread across the call site.
//
// Two classes of ambient input are removed:
//
//  1. Every inherited GIT_* variable. GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, the
//     object directories and the GIT_CONFIG_* family must not redirect the doctor
//     away from the repoRoot checkout it was asked to certify.
//  2. The CONFIG FILES git reads outside the checkout: the system-wide file,
//     ~/.gitconfig and $XDG_CONFIG_HOME/git/config. A malformed one made
//     `git ls-files` FAIL, which changed the doctor's source findings — the doc-leak
//     scan failed closed and the private-body contract silently fell back to a
//     filesystem walk. GIT_CONFIG_NOSYSTEM plus GIT_CONFIG_SYSTEM and
//     GIT_CONFIG_GLOBAL pointing at os.DevNull remove all three.
//
// The repo's OWN .git/config is deliberately NOT touched: the local index, worktree
// and submodule semantics git owns there stay exactly as they are, and the fix only
// removes what the operator's environment contributes.
func gitSourceEnumerationEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			env = append(env, entry)
		}
	}
	// Appended AFTER the inherited-GIT_* filter, so these win over anything the
	// caller exported.
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
	)
}

// checkRetiredSurfaceReferences keeps proven-retired public surfaces from
// reappearing as pending roadmap debt after their replacement loop is proven.
func checkRetiredSurfaceReferences(repoRoot string) []Finding {
	phrases := []string{
		"old flat skill matrix",
		"old skill matrix",
	}
	var hits []string
	hasToolResolverRetirement := false
	hasToolResolverEvidenceOnly := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := strings.ToLower(string(b))
		if strings.Contains(text, "os-tool-resolver-json") || strings.Contains(text, "tool-resolver") {
			hasToolResolverRetirement = hasToolResolverRetirement ||
				(strings.Contains(text, "compound://registries/capability-graph") &&
					(strings.Contains(text, "source_view_authority") || strings.Contains(text, "retired_as_authority")))
			hasToolResolverEvidenceOnly = hasToolResolverEvidenceOnly ||
				(strings.Contains(text, "evidence_only_input") || strings.Contains(text, "fallback/evidence"))
		}
		rel, _ := filepath.Rel(repoRoot, path)
		for i, line := range strings.Split(string(b), "\n") {
			low := strings.ToLower(line)
			for _, phrase := range phrases {
				if strings.Contains(low, phrase) {
					hits = append(hits, rel+":"+itoa(i+1)+" references "+strconv(phrase))
				}
			}
		}
		return nil
	})
	if len(hits) > 0 {
		return []Finding{{"retired-surface", Fail, "retired capability-matrix surface still listed in public docs: " + strings.Join(hits, "; ")}}
	}
	if !hasToolResolverRetirement || !hasToolResolverEvidenceOnly {
		return []Finding{{"retired-surface", Fail, "public docs must state generated tool-resolver authority is retired to compound://registries/capability-graph and remains evidence-only rollback input"}}
	}
	return []Finding{{"retired-surface", Info, "capability graph replaced public old-skill-matrix debt; generated tool-resolver authority retired as evidence-only input"}}
}

// retiredGen1Packages names the Gen-1 packages docs/architecture/status.md declares retired: the
// Gen-1 memory stack migrated into the spine memory family (internal/mem), and these names must
// never be resurrected as internal/ directories. Nothing enforced this mechanically before this
// check — a package could be silently reintroduced as a parallel meaning-owner.
var retiredGen1Packages = []string{
	"memory", "position", "pattern", "recall", "reconcile", "distill", "engine", "scope", "ledger",
}

// checkRetiredGen1PackagesStayDead fails the doctor if any retired Gen-1 package name exists again
// as a directory under internal/. Missing capability lands on internal/mem, internal/distiller, or
// a newly classified package instead of resurrecting a retired name.
func checkRetiredGen1PackagesStayDead(repoRoot string) []Finding {
	var f []Finding
	for _, pkg := range retiredGen1Packages {
		info, err := os.Stat(filepath.Join(repoRoot, "internal", pkg))
		if err == nil && info.IsDir() {
			f = append(f, Finding{"retired-gen1", Fail, "internal/" + pkg + " is a retired Gen-1 package (docs/architecture/status.md) and must not be resurrected — land new capability on internal/mem, internal/distiller, or a newly classified package instead"})
		}
	}
	if len(f) == 0 {
		f = append(f, Finding{"retired-gen1", Info, "all " + itoa(len(retiredGen1Packages)) + " retired Gen-1 packages stay absent from internal/"})
	}
	return f
}

// checkPrivateAdaptationSteering keeps the operator-only goal prompt from
// drifting back into holistic OS migration or duplicate runtime prompts. Public
// clones normally do not carry docs_private/plan; absence is not a failure.
func checkPrivateAdaptationSteering(repoRoot string) []Finding {
	planDir := filepath.Join(repoRoot, "docs_private", "plan")
	if st, err := os.Stat(planDir); err != nil || !st.IsDir() {
		return []Finding{{"adaptation-steering", Info, "docs_private/plan absent; public clone uses roadmap fallback for context-aware OS adaptation"}}
	}

	var failures []string
	canonicalPath := filepath.Join(planDir, "codex-goal-prompt.md")
	rulePath := filepath.Join(planDir, "OPERATING-SYSTEM-ADAPTATION-RULE.md")
	_, canonicalStatErr := os.Stat(canonicalPath)
	_, ruleStatErr := os.Stat(rulePath)
	if os.IsNotExist(canonicalStatErr) && os.IsNotExist(ruleStatErr) {
		return []Finding{{"adaptation-steering", Info, "private adaptation steering files absent; public/packet clone uses roadmap fallback for context-aware OS adaptation"}}
	}
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		failures = append(failures, "missing canonical codex-goal-prompt.md: "+err.Error())
	} else {
		text := strings.ToLower(string(canonical))
		required := []string{
			"## context-aware adaptation rules",
			"context-aware adaptation",
			"not copy/paste",
			"silence condition",
			"proof command",
			"old-owner",
			"public/private boundary",
			"trigger, action, boundary, and proof",
			"consequential bounded move",
			"public compound stays generic",
			"private dogfood adapters feed generic doors",
			"held operator valves",
			"oracle is evidence, not authority",
			"do not report compound as globally blocked",
		}
		failures = append(failures, missingPhrases("codex-goal-prompt.md", text, required)...)
		forbidden := []string{
			"current likely next move",
			"do rebuild asc",
		}
		failures = append(failures, presentPhrases("codex-goal-prompt.md", text, forbidden)...)
		lineCount := strings.Count(string(canonical), "\n") + 1
		if lineCount > 220 {
			failures = append(failures, "codex-goal-prompt.md too long for active steering prompt; keep session archive in CODEX-WORKLOG.md")
		}
		if latestCount := strings.Count(text, "## latest verified state"); latestCount > 1 {
			failures = append(failures, "codex-goal-prompt.md has too many Latest Verified State blocks; keep only current state and move history to CODEX-WORKLOG.md")
		}
	}

	rule, err := os.ReadFile(rulePath)
	if err != nil {
		failures = append(failures, "missing OPERATING-SYSTEM-ADAPTATION-RULE.md: "+err.Error())
	} else {
		text := strings.ToLower(string(rule))
		required := []string{
			"## instruction tuning standard",
			"trigger:",
			"action:",
			"boundary:",
			"silence condition:",
			"proof:",
			"retirement:",
			"only merge and proof count",
		}
		failures = append(failures, missingPhrases("OPERATING-SYSTEM-ADAPTATION-RULE.md", text, required)...)
	}

	pointerPath := filepath.Join(planDir, "codex-goal-prompt-compounding-runtime.md")
	if b, err := os.ReadFile(pointerPath); err == nil {
		text := strings.ToLower(string(b))
		required := []string{
			"this is not a second goal prompt",
			"codex-goal-prompt.md",
			"superseded",
		}
		failures = append(failures, missingPhrases("codex-goal-prompt-compounding-runtime.md", text, required)...)
		if strings.Contains(text, "## current-style candidate moves") || strings.Contains(text, "## goal lanes") {
			failures = append(failures, "codex-goal-prompt-compounding-runtime.md still contains active prompt sections; keep it pointer-only")
		}
	}

	failures = append(failures, duplicateGoalPromptBodies(planDir)...)
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"adaptation-steering", Fail, "private adaptation steering is shallow or duplicated: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"adaptation-steering", Info, "private goal prompt enforces context-aware OS adaptation, consequential bounded moves, generic public/private collector boundary, and duplicate prompt is pointer-only"}}
}

// checkWorkbenchActionCardContract keeps held-valve adjacent actions from
// drifting back into vague "operator required" prose. The Workbench is a
// consumed runtime surface, so operator-gated cards must expose machine-readable
// inputs, validation, and stop gates across JSON, text, tests, and public status.
func checkWorkbenchActionCardContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/work/workbench.go": {
			"RequiredInputs",
			"ExpectedValidation",
			"StopConditions",
			"staleness,omitempty",
			"admit_command,omitempty",
			"retire_command,omitempty",
			"requires_operator",
			"admission_blocked_reason",
			"retire_dry_run_command,omitempty",
			"requires_operator,omitempty",
			"admission_blocked_reason,omitempty",
			"admitCommand = nil",
			"operator-only admission preview",
			"\"--kind\", \"create-plan\"",
			"workbenchAdmissionPreviewStaleAfter",
			"withAdjacentActionGates",
			"RequiresOperator: true",
			"conversation-memory-review-gate",
			"compound\", \"panel\", \"plan\"",
			"\"--target-kind\", \"memory_candidate\"",
			"\"--privacy-sensitive\"",
			"\"--max-items\", \"1\"",
			"no compound memory adjudicate --verdict promote is run",
			"review plan would run adjudication or promotion",
			"GateID",
			"SourceKind",
			"workbenchReviewCards",
			"workbenchOperatorGateCards",
			"RequiresOperator",
			"AdmissionBlockedReason",
			"source_private_config_gate",
			"PromotionSummaryRef",
			"workbenchRunArtifacts",
			"runs\", workID",
			"promotion_summary",
			"hasSourcePrivateConfigCard",
			"hasMemoryCandidateReviewCard",
			"!hasSourcePrivateConfigCard",
			"!hasMemoryCandidateReviewCard",
		},
		"internal/work/review_card_plan.go": {
			"ReviewCardWorkPlanReportSchemaVersion",
			"BuildReviewCardWorkPlan",
			"BuildReviewCardWorkDraft",
			"workbench_review_card",
			"plan_work_from_workbench_review_card",
			"card.RequiresOperator",
			"do not execute review-card command while creating or admitting work-plan",
			"do not clear the source held WorkEnvelope from this proposal",
		},
		"internal/work/review_card_plan_test.go": {
			"TestReviewCardWorkPlanRecordsASCOperatorGateProposalOnly",
			"TestReviewCardWorkPlanRecordsMemoryCandidateCardWithoutPromotion",
			"admitting review-card proposal must not clear source held work",
			"review-card work-plan should not admit work",
		},
		"internal/work/workbench_test.go": {
			"len(action.RequiredInputs) == 0",
			"len(action.ExpectedValidation) == 0",
			"len(action.StopConditions) == 0",
			"TestWorkbenchMarksOldAdmissionPreviewsStale",
			"stale admission preview",
			"RetireCommand",
			"RetireDryRun",
			"source observation would promote truth",
			"conversation-memory-review-gate",
			"--target-kind memory_candidate",
			"--privacy-sensitive",
			"--max-items 1",
			"compound memory adjudicate --verdict promote is not run",
			"TestWorkbenchSurfacesRunArtifactsForHeldWork",
			"TestWorkbenchSurfacesASCOperatorGateCardForHeldWork",
			"run artifact leaked absolute/private path",
			"promotion summary should preserve zero promoted claims",
			"review card should expose exact parked candidate/panel/promotion boundary",
			"ASC operator gate card should expose exact read-only private-config valve",
		},
		"cmd/compound/workbench_cli.go": {
			"action_required_inputs:",
			"action_expected_validation:",
			"action_stop_conditions:",
			"run_artifacts=%d",
			"run_artifact: %s kind=%s status=%s target=%s count=%s",
			"staleness=%s",
			"admit: %s",
			"retire_dry_run: %s",
			"retire: %s",
			"operator_required: true",
			"blocked_reason:",
		},
		"cmd/compound/work_cli.go": {
			"review-card-plan",
			"BuildReviewCardWorkPlan",
		},
		"cmd/compound/main_test.go": {
			"action_required_inputs:",
			"action_expected_validation:",
			"action_stop_conditions:",
			"staleness=fresh",
			"admit: compound work admit-plan",
			"conversation-memory-review-gate",
			"generic conversation review gate should be suppressed by exact review card",
			"run_artifacts=2",
			"kind=review_card",
			"promotion summary remains zero before explicit promotion",
			"generic ASC preflight should be suppressed by exact review card",
			"generic conversation review gate should be suppressed by exact review card",
			"panel-run-single-prun_cli_fixture.json",
			"TestWorkReviewCardPlanCLIAdmitsCardThroughWorkbenchPreview",
			"workbench_review_card",
		},
		"cmd/compound/orient_operator.go": {
			"MemoryReviewCardCount",
			"MemoryReviewCards",
			"OperatorGateCardCount",
			"OperatorGateCards",
			"orientMemoryReviewCards",
			"orientOperatorGateCards",
			"memory_review_cards:",
			"operator_gate_cards:",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorSurfacesMemoryReviewCardsFromHeldRunArtifacts",
			"TestOrientOperatorSurfacesASCGateCardsForHeldWork",
			"cand_orient_conversation_review",
			"asc-private-config",
			"promoted=0",
			"operator_required=true",
			"blocked_reason: operator required inputs",
		},
		"docs/architecture/status.md": {
			"required_inputs[]",
			"expected_validation[]",
			"stop_conditions[]",
			"Admission previews carry staleness",
			"admit_command",
			"retire_dry_run_command",
			"retire_command",
			"archive-history --kind create-plan",
			"import_safety",
			"conversation-memory-review-gate",
			"memory_candidate",
			"review-card-plan --card",
			"workbench_review_card",
			"consumed_non_packet_actions",
			"Workbench run-artifact note",
			"`run_artifacts[]`",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
		if rel == "internal/work/workbench.go" {
			failures = append(failures, missingGoStructFields(rel, text, "WorkbenchItem", map[string]goStructFieldContract{
				"ReviewCards":  {Type: "[]WorkbenchReviewCard", Tag: `json:"review_cards,omitempty"`},
				"RunArtifacts": {Type: "[]WorkbenchRunArtifact", Tag: `json:"run_artifacts,omitempty"`},
			})...)
			failures = append(failures, missingGoStructFields(rel, text, "WorkbenchRunArtifact", map[string]goStructFieldContract{
				"TargetRefs": {Type: "[]string", Tag: `json:"target_refs,omitempty"`},
			})...)
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"workbench-action-card-contract", Fail, "Workbench adjacent action cards lost held-valve gates: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"workbench-action-card-contract", Info, "Workbench operator-gated action cards expose required inputs validation stop conditions through consumed surfaces"}}
}

// checkHomeDoctorOperationalHygieneContract keeps home doctor from degrading
// into text-only hygiene warnings. Dirty operational history must surface exact
// reversible archive-history commands and stop gates without clearing held work.
func checkHomeDoctorOperationalHygieneContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/main.go": {
			"compound.home_doctor_report.v0.1",
			"OperationalHygieneActions",
			"homeDoctorHygieneAction",
			"operational_hygiene_actions",
			"archive-terminal-work",
			"archive-stale-leases",
			"compound\", \"home\", \"archive-history\", \"--kind\", \"work\"",
			"compound\", \"home\", \"archive-history\", \"--kind\", \"lease\"",
			"workbench held/open work would be cleared",
		},
		"cmd/compound/main_test.go": {
			"home doctor --json should parse",
			"archive-terminal-work",
			"archive-stale-leases",
			"dry_run: compound home archive-history --kind work --dry-run",
			"dry_run: compound home archive-history --kind lease --dry-run",
		},
		"docs/architecture/status.md": {
			"home doctor --json",
			"operational_hygiene_actions",
			"archive-history --kind work|lease",
		},
		"docs/architecture/contracts/README.md": {
			"compound.home_doctor_report.v0.1",
			"operational_hygiene_actions",
			"archive-history --kind work|lease",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"home-doctor-operational-hygiene-contract", Fail, "home doctor lost JSON hygiene actions: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"home-doctor-operational-hygiene-contract", Info, "home doctor exposes JSON operational hygiene repair actions through archive-history gates"}}
}

// checkConversationPreflightImportSafetyContract keeps the private
// conversation import preflight from degrading into an export-only smoke test.
// The report itself must prove bounded private evidence/candidates and no temp
// promoted claims before any operator can choose a live import.
func checkConversationPreflightImportSafetyContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"tools/chatgpt_conversation_preflight.ts": {
			"type ImportSafetyReport = {",
			"evidence_count: number;",
			"private_evidence_count: number;",
			"max_imported_quote_chars: number;",
			"candidate_count: number;",
			"private_candidate_count: number;",
			"candidate_trace_refs_match_evidence: boolean;",
			"promoted_claim_count: number;",
			"import_safety: ImportSafetyReport;",
			"function buildImportSafetyReport",
			"imported_evidence_private",
			"imported_evidence_quote_bound",
			"imported_candidates_private",
			"candidate_trace_refs_match_evidence",
			"no_temp_promoted_claims",
		},
		"tools/chatgpt_conversation_preflight.test.ts": {
			"report.import_safety.evidence_count",
			"report.import_safety.private_evidence_count",
			"report.import_safety.max_imported_quote_chars",
			"report.import_safety.candidate_count",
			"report.import_safety.private_candidate_count",
			"report.import_safety.candidate_trace_refs_match_evidence",
			"report.import_safety.promoted_claim_count",
			"imported_evidence_private",
			"candidate_trace_refs_match_evidence",
			"no_temp_promoted_claims",
		},
		"internal/work/workbench.go": {
			"conversation-preflight-private-export",
			"import_safety checks pass",
			"import_safety proves private bounded evidence/candidates and zero temp promoted claims",
		},
		"docs/architecture/status.md": {
			"ChatGPT capture edge note",
			"`tools/chatgpt_conversation_preflight.ts` wraps that edge",
			"`import_safety` privacy/quote/candidate/no-claim checks",
			"Conversation preflight import-safety doctor note",
			"No conversation-specific core package, live import, or promotion path exists.",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"conversation-preflight-import-safety-contract", Fail, "conversation preflight lost import_safety proof gates: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"conversation-preflight-import-safety-contract", Info, "conversation preflight import_safety proves private bounded evidence/candidates and zero temp promoted claims"}}
}

// checkOperatorNeedsMeCandidateAuthorityContract keeps `orient --operator`
// needs-me counts aligned with consumed authorities. Memory candidate debt must
// come from the memory candidate authority, because raw inbox files can remain
// proposed after admission classifies them as parked source evidence.
func checkOperatorNeedsMeCandidateAuthorityContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/orient_operator.go": {
			"memoryCandidateInboxSummary",
			"store.ListCandidates(now, mem.CandidateListOptions{})",
			"if kind == candidates.Memory",
			"candidateStateActionable(item.State)",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorUsesMemoryCandidateAuthorityForNeedsMe",
			"raw candidate state",
			"authority-parked non-actionable source metadata",
			"memory state_counts[parked]",
		},
		"docs/architecture/status.md": {
			"Operator needs-me candidate authority note",
			"`orient --operator` counts memory candidate debt from `compound://memory/candidates`",
			"raw inbox files can remain `proposed`",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"operator-needs-me-candidate-authority-contract", Fail, "operator needs-me candidate counts lost consumed authority: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"operator-needs-me-candidate-authority-contract", Info, "operator needs-me memory debt consumes memory candidate authority instead of raw inbox state"}}
}

// checkOperatorOrientAdjacentProgressContract keeps the compact operator front
// door from hiding Workbench adjacent progress when direct work is held on
// operator valves. This is the anti-loop contract for `orient --operator`.
func checkOperatorOrientAdjacentProgressContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/orient_operator.go": {
			"AdjacentProgress",
			"json:\"adjacent_progress,omitempty\"",
			"AdmissionPreviewCount",
			"AdmissionPreviews",
			"json:\"admission_previews,omitempty\"",
			"orientAdmissionPreviews",
			"workbench_review_card",
			"SessionContext",
			"json:\"session_context,omitempty\"",
			"buildOrientSessionContext",
			"buildOrientAdjacentProgress",
			"orientOpenWorkNextAction",
			"continue held-valve-adjacent Compound work without clearing operator-only valves",
			"held on operator valve; use running.adjacent_progress actions:",
			"HeldWorkIDs",
			"PacketCandidateCount",
			"NonPacketActionIDs",
			"ConsumedNonPacketActionCount",
			"ConsumedNonPacketActionIDs",
			"json:\"consumed_non_packet_action_count,omitempty\"",
			"json:\"consumed_non_packet_action_ids,omitempty\"",
			"SESSION-CONTEXT",
			"admission_previews: %d",
			"source_review_card=%s",
			"[]string{\"compound\", \"orient\", \"--harness\", harness, \"--repo-kind\", repoKind, \"--task\", task}",
			"adjacent_progress: available=%t held=%d non_packet_actions=%d consumed_non_packet_actions=%d packet_candidates=%d",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorSurfacesHeldValveAdjacentProgress",
			"operator-gate-\" + asc.ID + \"-app-store-connect-private-config",
			"conversation-preflight-private-export",
			"conversation-memory-review-gate",
			"second-opinion-evidence-recovery",
			"codex-pretooluse-native-gate-proof",
			"resolve policy/source-control hold",
			"workbench.adjacent_progress",
			"TestOrientOperatorSurfacesAdmissionPreviewReviewCardContext",
			"admission_previews: 1",
			"action_context=workbench_review_card",
			"SESSION-CONTEXT",
			"compound orient --harness codex --repo-kind control-plane --task continue held-valve-adjacent",
			"adjacent_progress: available=true",
		},
		"cmd/compound/proof_check.go": {
			"recordProofCheckRuntimeArtifact",
			"proof-check-installed-runtime",
			"architecture-doctor-strict",
			"workbench-projection-refresh",
			"codex-pretooluse-native-gate-proof",
			"runProofCheckCodexPreToolUseNativeGate",
			"workbench\", \"--json\"",
			"adjacent Workbench action",
			"Do not rerun",
			"type proofCheckReport = work.ProofCheckReport",
		},
		"internal/work/proof_execution.go": {
			"type ProofCheckReport struct",
			"artifact_ref,omitempty",
		},
		"cmd/compound/main_test.go": {
			"TestProofCheckArtifactConsumesVerificationAdjacentActions",
			"proof-check-installed-runtime",
			"architecture-doctor-strict",
			"workbench-projection-refresh",
			"codex-pretooluse-native-gate-proof",
			"ConsumedNonPacketActions",
			"ConsumedEvidenceRefs",
		},
		"docs/architecture/status.md": {
			"Operator orientation adjacent-progress note",
			"compound orient --operator --json.running.adjacent_progress",
			"--json.session_context",
			"running.admission_previews[]",
			"workbench_review_card",
			"ContextPack command",
			"proof-producing non-packet actions",
			"proof-check-installed-runtime",
			"consumed_non_packet_actions",
			"open_work[].next_action",
			"Operator orientation adjacent-progress doctor note",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"operator-orient-adjacent-progress-contract", Fail, "operator orient lost held-valve adjacent-progress projection: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"operator-orient-adjacent-progress-contract", Info, "operator orient surfaces Workbench held-valve adjacent progress through JSON text tests and status"}}
}

func checkOperatorSourceControlProjectionContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/orient_operator.go": {
			"SourceControl",
			"json:\"source_control,omitempty\"",
			"buildOrientSourceControl",
			"SourceControlCleanupReviewArea",
			"authority.ReadGitSnapshot",
			"head_review_recorded",
			"source-control dirty without a cleanup receipt",
			"source_control: live=%s changed=%d",
			"source_control_review:",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorCarriesSourceControlReviewProjection",
			"head_review_recorded=true",
			"source-control cleanup reviews",
			"source-control snapshot",
			"inspect cleanup receipts",
		},
		"docs/architecture/status.md": {
			"Operator source-control projection note",
			"`compound orient --operator --json.needs_me.source_control`",
			"reviewed dirty git evidence",
			"does not stage, clean, commit, reset, stash, push, or discard",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"operator-source-control-projection-contract", Fail, "operator orient lost source-control cleanup projection: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"operator-source-control-projection-contract", Info, "operator orient consumes source-control cleanup reviews plus live git snapshot without mutating repo"}}
}

// checkASCDailySignalsPublicBoundaryContract keeps product-signal source docs
// from drifting back into private collector architecture. App Store Connect
// collection is an edge concern; public Compound owns mapping, validation,
// idempotency, staleness, receipts, and projection.
func checkASCDailySignalsPublicBoundaryContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"docs/operations/asc-daily-signals.md": {
			"Compound does not own App Store Connect fetching",
			"any collector can produce ASC-shaped output",
			"Compound only normalizes",
			"Keep private app IDs, vendor IDs, credentials",
			"The bridge does not call ASC",
			"assume any specific upstream collector",
			"Scheduling is deployment-specific",
		},
		"docs/architecture/status.md": {
			"ASC daily signals public-boundary note",
			"source collector",
			"private adapter",
			"product-signal",
			"apply_results",
			"asc-sales-series-bridge-proof",
			"named private collectors",
		},
		"internal/work/workbench.go": {
			"asc-sales-series-bridge-proof",
			"bun\", \"test\", \"tools/asc_sales_series_bridge.test.ts",
			"temp COMPOUND_HOME bridge apply proof passes",
			"apply_results shows signal_count and transition_count",
			"bridge report lacks apply_results",
		},
		"internal/work/workbench_test.go": {
			"asc-sales-series-bridge-proof",
			"bun test tools/asc_sales_series_bridge.test.ts",
			"apply_results shows signal_count and transition_count",
		},
		"cmd/compound/orient_adjacent_catalog.go": {
			"asc-sales-series-bridge-proof",
		},
		"cmd/compound/orient_operator_test.go": {
			"asc-sales-series-bridge-proof",
		},
		"cmd/compound/main_test.go": {
			"asc-sales-series-bridge-proof",
			"action_command: bun test tools/asc_sales_series_bridge.test.ts",
			"apply_results shows signal_count and transition_count",
		},
		"tools/asc_sales_series_bridge.ts": {
			"type ApplyResult = {",
			"signal_count: number;",
			"transition_count: number;",
			"apply_results",
			"countMatches(stdout, /^# product signal /gm)",
			"countMatches(stdout, /^- transition: /gm)",
		},
		"tools/asc_sales_series_bridge.test.ts": {
			"report.apply_results",
			"signal_count: 1",
			"transition_count: 1",
		},
	}
	forbiddenByFile := map[string][]string{
		"docs/operations/asc-daily-signals.md": {
			"must use launchd",
			"requires launchd",
			"/Users/",
		},
	}

	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	for rel, phrases := range forbiddenByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, presentPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"asc-daily-signals-public-boundary-contract", Fail, "ASC daily signal docs lost generic public/private source boundary: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"asc-daily-signals-public-boundary-contract", Info, "ASC daily signal docs keep collectors private/generic while Compound owns product-signal normalization"}}
}

// checkPublicAlphaSupportedScopeContract keeps the public-alpha support
// boundary from drifting in the public docs: file backend only; one trusted
// OS user on local disk; explicit home authority (ambient ancestor
// discovery is not an authority target); interactive hooks may observe
// while enforcement claims require behavioral proof; the console target is
// projection-only with mutations/private reads off; and Postgres, shared
// homes, server/multi-tenant use, remote/proxied console, delegated mutation,
// and arbitrary private embedding endpoints are unsupported. It also pins the honesty
// phrases: the contract must name any remaining current blockers, while the
// projection-only console default is pinned as current behavior rather than a
// future promise. The earlier ancestor-authority and console-default blockers
// are closed and are no longer "current blocker" entries.
func checkPublicAlphaSupportedScopeContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"docs/security/supported-scope.md": {
			"the only supported storage backend",
			"one trusted OS user and local disk",
			"Ambient ancestor",
			"is not an authority target",
			"Interactive hooks may observe",
			"Enforcement claims require behavioral proof",
			"projection-only",
			"mutations and private reads",
			"Unsupported in the public alpha",
			"shared homes",
			"multi-tenant",
			"proxied console",
			"delegated launch, steer, progress",
			"private embedding endpoints",
			"Current blockers",
			"projection-only by default",
		},
		"README.md": {
			"Supported Scope (Public Alpha)",
			"docs/security/supported-scope.md",
			"ambient ancestor",
			"behavioral proof",
		},
		"docs/getting-started.md": {
			"security/supported-scope.md",
			"file backend only",
		},
		"docs/harness-wiring.md": {
			"security/supported-scope.md",
			"behavioral proof",
			"never mutation-ready",
		},
		"docs/architecture/status.md": {
			"the only supported public-alpha backend",
			"unsupported in the public alpha",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"public-alpha-supported-scope-contract", Fail, "public-alpha supported scope drifted: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"public-alpha-supported-scope-contract", Info, "public docs keep the public-alpha support boundary: file backend only, one trusted user, explicit home authority, proven enforcement, projection-only console default, current blockers named"}}
}

// checkReviewSurfaceCoverageContract keeps review queues from drifting back into
// raw artifact lists. Pending review items must expose whether they have
// decision cards and actions, including missing-affordance counters renderers can
// alert on.
func checkReviewSurfaceCoverageContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/reviewartifact/queue.go": {
			"itemNeedsReviewCard",
		},
		"internal/reviewartifact/reviewartifact_test.go": {
			"DecisionCardCount",
			"ActionableCount",
			"ReviewRequiredMissingDecisionCard",
			"ReviewRequiredMissingAction",
			"FallbackSummaryCount",
			"NeedsYouFallbackGroupCount",
			"missing coverage counts",
		},
		"cmd/compound/main.go": {
			"review_surface decision_cards=",
			"missing_cards=",
			"missing_actions=",
			"fallback_summaries=",
			"needs_you_fallback_groups=",
		},
		"cmd/compound/main_test.go": {
			"review_surface decision_cards=",
			"ReviewRequiredMissingDecisionCard",
			"ReviewRequiredMissingAction",
			"fallback_summaries=",
		},
		"docs/architecture/status.md": {
			"Review queue surface-coverage note",
			"decision_card_count",
			"actionable_count",
			"review_required_missing_decision_card",
			"review_required_missing_action",
			"fallback_summary_count",
			"needs_you_fallback_group_count",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		text := string(b)
		failures = append(failures, missingPhrases(rel, text, phrases)...)
		if rel == "internal/reviewartifact/queue.go" {
			failures = append(failures, missingGoStructFields(rel, text, "QueueCounts", map[string]goStructFieldContract{
				"DecisionCardCount":                 {Type: "int", Tag: `json:"decision_card_count"`},
				"ActionableCount":                   {Type: "int", Tag: `json:"actionable_count"`},
				"ReviewRequiredMissingDecisionCard": {Type: "int", Tag: `json:"review_required_missing_decision_card"`},
				"ReviewRequiredMissingAction":       {Type: "int", Tag: `json:"review_required_missing_action"`},
				"FallbackSummaryCount":              {Type: "int", Tag: `json:"fallback_summary_count,omitempty"`},
				"NeedsYouFallbackGroupCount":        {Type: "int", Tag: `json:"needs_you_fallback_group_count,omitempty"`},
			})...)
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"review-surface-coverage-contract", Fail, "Review queue surface lost decision/action coverage gates: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"review-surface-coverage-contract", Info, "review queue reports decision/action coverage and missing-affordance counters through consumed surfaces"}}
}

func missingPhrases(source, text string, phrases []string) []string {
	var out []string
	for _, phrase := range phrases {
		if !strings.Contains(text, phrase) {
			out = append(out, source+" missing "+strconv(phrase))
		}
	}
	return out
}

type goStructFieldContract struct {
	Type string
	Tag  string
}

// missingGoStructFields checks Go field identity, type, and optional tag
// instead of gofmt's alignment whitespace. Adding a longer sibling field must
// not make an architecture contract report that an unchanged field disappeared.
func missingGoStructFields(source, text, structName string, expected map[string]goStructFieldContract) []string {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, source, text, 0)
	if err != nil {
		return []string{source + " unparseable: " + err.Error()}
	}
	var target *ast.StructType
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != structName {
				continue
			}
			target, _ = typeSpec.Type.(*ast.StructType)
		}
	}
	if target == nil {
		return []string{source + " missing " + strconv("type "+structName+" struct")}
	}
	found := make(map[string]bool, len(expected))
	for _, field := range target.Fields.List {
		var typeText bytes.Buffer
		if err := format.Node(&typeText, fset, field.Type); err != nil {
			continue
		}
		tag := ""
		if field.Tag != nil {
			tag = strings.Trim(field.Tag.Value, "`")
		}
		for _, name := range field.Names {
			contract, ok := expected[name.Name]
			if ok && contract.Type == typeText.String() && (contract.Tag == "" || contract.Tag == tag) {
				found[name.Name] = true
			}
		}
	}
	fields := make([]string, 0, len(expected))
	for field := range expected {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	var missing []string
	for _, field := range fields {
		if !found[field] {
			description := field + " " + expected[field].Type
			if expected[field].Tag != "" {
				description += " `" + expected[field].Tag + "`"
			}
			missing = append(missing, source+" missing "+strconv(description))
		}
	}
	return missing
}

func readPackageText(repoRoot, relDir string) (string, error) {
	dir := filepath.Join(repoRoot, filepath.FromSlash(relDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return "", err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func readContractText(repoRoot, rel string) (string, error) {
	if rel == "cmd/compound" {
		return readPackageText(repoRoot, rel)
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func presentPhrases(source, text string, phrases []string) []string {
	var out []string
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			out = append(out, source+" still contains stale phrase "+strconv(phrase))
		}
	}
	return out
}

func duplicateGoalPromptBodies(planDir string) []string {
	var out []string
	_ = filepath.WalkDir(planDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		name := filepath.Base(path)
		if name == "codex-goal-prompt.md" {
			return nil
		}
		if !strings.Contains(name, "goal") && !strings.Contains(name, "prompt") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := strings.ToLower(string(b))
		if strings.Contains(text, "# compound goal prompt") || strings.Contains(text, "## goal lanes") || strings.Contains(text, "## context-aware adaptation rules") {
			out = append(out, name+" appears to contain a second active goal prompt")
		}
		return nil
	})
	return out
}

// checkDistillerDuplicateContract keeps the public distiller contract aligned
// with the actual anti-churn path. Duplicate suppression is no longer memory-only:
// non-memory candidate meanings also point at prior_candidate_id and no-promote.
func checkDistillerDuplicateContract(repoRoot string) []Finding {
	mentionsCandidateDuplicate := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(b), "\n") {
			low := strings.ToLower(line)
			if strings.Contains(low, "prior_candidate_id") ||
				strings.Contains(low, "duplicate non-memory") ||
				strings.Contains(low, "duplicate candidate") {
				mentionsCandidateDuplicate = true
			}
		}
		return nil
	})
	if !mentionsCandidateDuplicate {
		return []Finding{{"distiller-contract", Fail, "public docs do not describe non-memory duplicate candidate anti-churn (prior_candidate_id/no-promote)"}}
	}
	return []Finding{{"distiller-contract", Info, "distiller duplicate anti-churn covers memory claims and candidate inboxes"}}
}

// checkLLMExtractionGroundingContract keeps the live/eval extractor contract
// from drifting back to generic model-output parsing. Request-aware extraction
// must require candidate evidence refs grounded in LLMExtractionRequest.source_refs.
func checkLLMExtractionGroundingContract(repoRoot string) []Finding {
	hasRequestGrounded := false
	hasEvidenceRefs := false
	hasSourceRefs := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "request-grounded") {
			hasRequestGrounded = true
		}
		if strings.Contains(low, "evidence_refs") {
			hasEvidenceRefs = true
		}
		if strings.Contains(low, "llmextractionrequest.source_refs") || strings.Contains(low, "source_refs[]") {
			hasSourceRefs = true
		}
		return nil
	})
	if !hasRequestGrounded || !hasEvidenceRefs || !hasSourceRefs {
		return []Finding{{"llm-grounding-contract", Fail, "public docs do not describe request-grounded LLM extraction evidence_refs/source_refs gate"}}
	}
	return []Finding{{"llm-grounding-contract", Info, "LLM extraction contract requires request-grounded evidence refs"}}
}

// checkDistillerCandidateShapeContract keeps candidate extraction from drifting
// back to "kind + confidence" only. A candidate must carry enough typed content
// to be reviewed before it can enter an inbox.
func checkDistillerCandidateShapeContract(repoRoot string) []Finding {
	hasShapeContract := false
	hasMemoryShape := false
	hasNonMemoryShape := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "candidate shape") || strings.Contains(low, "thin candidate") {
			hasShapeContract = true
		}
		if strings.Contains(low, "entity") && strings.Contains(low, "predicate") && strings.Contains(low, "object") {
			hasMemoryShape = true
		}
		if strings.Contains(low, "subject") && strings.Contains(low, "statement") {
			hasNonMemoryShape = true
		}
		return nil
	})
	if !hasShapeContract || !hasMemoryShape || !hasNonMemoryShape {
		return []Finding{{"distiller-shape-contract", Fail, "public docs do not describe candidate shape guard (memory entity/predicate/object; non-memory subject/statement)"}}
	}
	return []Finding{{"distiller-shape-contract", Info, "distiller candidate shape guard covers memory and non-memory candidates"}}
}

// checkRouteOutcomeContract keeps route/cost docs from drifting back to
// metadata-only routing. Route evidence is useful only when joined to Receipt
// outcome/state delta, and failed/unproven outcomes must remain review/no-promote
// material rather than direct router/provider mutations.
func checkRouteOutcomeContract(repoRoot string) []Finding {
	hasOutcomeReview := false
	hasReceipt := false
	hasStateDelta := false
	hasNoPromote := false
	hasNoConfigMutation := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "route outcome-review") || strings.Contains(low, "routeoutcomereview") {
			hasOutcomeReview = true
		}
		if strings.Contains(low, "receipt") {
			hasReceipt = true
		}
		if strings.Contains(low, "state delta") || strings.Contains(low, "state_delta") {
			hasStateDelta = true
		}
		if strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote") {
			hasNoPromote = true
		}
		if strings.Contains(low, "does not mutate provider/gateway/model-router config") ||
			strings.Contains(low, "does not mutate gateway/provider/model-router config") ||
			strings.Contains(low, "no gateway or provider config mutation") {
			hasNoConfigMutation = true
		}
		return nil
	})
	if !hasOutcomeReview || !hasReceipt || !hasStateDelta || !hasNoPromote || !hasNoConfigMutation {
		return []Finding{{"route-outcome-contract", Fail, "public docs do not describe receipt-linked route outcome review with state_delta/no-promote and no provider/gateway/model-router mutation"}}
	}
	return []Finding{{"route-outcome-contract", Info, "route outcome review joins route evidence to receipt state delta before policy review"}}
}

// checkKnowledgePageContract keeps curated-source docs from drifting back to a
// vague "knowledge pages become memory" promise. Page import must stay explicit,
// evidence-backed, candidate-only, and non-promoting.
func checkKnowledgePageContract(repoRoot string) []Finding {
	hasImportPage := false
	hasSchema := false
	hasClaims := false
	hasEvidence := false
	hasNoPromotion := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "memory import-page") {
			hasImportPage = true
		}
		if strings.Contains(low, "compound.knowledge_page.v0.1") {
			hasSchema = true
		}
		if strings.Contains(low, "claims[]") {
			hasClaims = true
		}
		if strings.Contains(low, "evidenceitem") {
			hasEvidence = true
		}
		if strings.Contains(low, "no promoted claims") ||
			strings.Contains(low, "creates no promoted claims") ||
			strings.Contains(low, "does not promote") {
			hasNoPromotion = true
		}
		return nil
	})
	if !hasImportPage || !hasSchema || !hasClaims || !hasEvidence || !hasNoPromotion {
		return []Finding{{"knowledge-page-contract", Fail, "public docs do not describe memory import-page with compound.knowledge_page.v0.1 claims[] -> EvidenceItem/proposed candidates and no promoted claims"}}
	}
	return []Finding{{"knowledge-page-contract", Info, "knowledge-page import stays evidence-backed candidate-only no-promotion"}}
}

// checkWorkPacketContract keeps active/execution packet docs from drifting back
// to pointer-only work. Packet markdown must enter the owned spine as proposed
// work identity, while remaining source evidence rather than proof.
func checkWorkPacketContract(repoRoot string) []Finding {
	hasImportPacket := false
	hasPacketHash := false
	hasProposedWork := false
	hasNoMutation := false
	hasNoProof := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "work import-packet") {
			hasImportPacket = true
		}
		if strings.Contains(low, "packet hash") {
			hasPacketHash = true
		}
		if strings.Contains(low, "proposed work") || strings.Contains(low, "proposed workenvelope") {
			hasProposedWork = true
		}
		if strings.Contains(low, "not mutating the packet") ||
			strings.Contains(low, "no source mutation") ||
			strings.Contains(low, "does not mutate the packet") {
			hasNoMutation = true
		}
		if strings.Contains(low, "not proof") ||
			strings.Contains(low, "not as proof") ||
			strings.Contains(low, "not claiming success") ||
			strings.Contains(low, "does not") && strings.Contains(low, "receipt/transition") && strings.Contains(low, "promote") {
			hasNoProof = true
		}
		return nil
	})
	if !hasImportPacket || !hasPacketHash || !hasProposedWork || !hasNoMutation || !hasNoProof {
		return []Finding{{"packet-work-contract", Fail, "public docs do not describe work import-packet as packet-hash proposed WorkEnvelope source evidence with no packet mutation, proof/receipt/transition, or memory promotion"}}
	}
	return []Finding{{"packet-work-contract", Info, "work packet import stays source-backed proposed work, not proof"}}
}

// checkWorkProofChainContract keeps terminal work from drifting into "closed
// because state says so". Completed/failed work must remain mechanically tied to
// readable Receipt and Transition envelopes with the same work_id, AND to
// RunAttempt records that prove the executed attempt is terminal, same-work,
// outcome-consistent, and -- when the RunAttempt carries an ActionIntent --
// that ActionIntent is present on the WorkEnvelope. Current prepared intents
// must also bind the exact terminal close generation. Accepted legacy schemas
// and an empty legacy ActionIntentID remain readable compatibility evidence.
func checkWorkProofChainContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/work/proof_chain_doctor.go": {
			"ProofChainDoctor",
			"MissingReceipt",
			"MissingTransition",
			"MismatchedWorkID",
			"transition_missing_receipt_ref",
			"terminalProofRequired",
			"rec.Validate()",
			"tr.Validate()",
			// v0.3 adds exact RunAttempt outcome and terminalization integrity.
			"compound.work_proof_chain_doctor.v0.3",
			"MissingRunAttempt",
			"run_attempt_work_mismatch",
			"run_attempt_action_mismatch",
			"run_attempt_nonterminal",
			"run_attempt_outcome_mismatch",
			"run_attempt_terminalization_mismatch",
			"run_attempt_unreadable",
			"run_attempt_invalid",
			"work-proof-chain-run-attempt",
			// Validate the EXACT raw entry -- no trimming; whitespace-only
			// and padded canonical refs classify as run_attempt_invalid.
			"runattempt.Valid(rattID)",
			// Identity check (embedded id + schema_version) fires BEFORE WorkID
			// linkage so a corrupt record (multi-corrupt, half-decoded)
			// classifies as run_attempt_invalid, not run_attempt_work_mismatch.
			"ra.ID != rattID",
			"!runattempt.AcceptsSchemaVersion(ra.SchemaVersion)",
			"validateTerminalizationIntentAgainstClose",
			// Empty legacy ActionIntentID stays valid; a non-empty ActionIntent
			// must be present on the work.
			"ra.ActionIntentID != \"\"",
		},
		"cmd/compound/work_cli.go": {
			"proof-chain-doctor",
			"ProofChainDoctorOptions",
			"--include-archived",
		},
		"internal/work/proof_chain_doctor_test.go": {
			"TestProofChainDoctorReportsCleanTerminalWork",
			"TestProofChainDoctorFailsTerminalWorkMissingReceiptTransition",
			"TestProofChainDoctorFailsMismatchedTerminalWorkID",
			// v0.2 RunAttempt regressions plus v0.3 exact-close integrity.
			"TestProofChainDoctorFailsReferencedRunAttemptMissing",
			"TestProofChainDoctorFailsRunAttemptWorkIDMismatch",
			"TestProofChainDoctorFailsRunAttemptActionMismatch",
			"TestProofChainDoctorFailsRunAttemptNonterminal",
			"TestProofChainDoctorFailsRunAttemptUnreadable",
			"TestProofChainDoctorFailsRunAttemptInvalidRecord",
			"TestProofChainDoctorFailsRunAttemptInvalidReference",
			"TestProofChainDoctorAcceptsLegacyTerminalWorkWithZeroRunAttemptRefs",
			"TestProofChainDoctorAcceptsLegacyRunAttemptWithEmptyActionIntentID",
			"TestProofChainDoctorFindsTerminalizationReasonMismatch",
			"TestProofChainDoctorFindsRunAttemptOutcomeMismatch",
			"TestProofChainDoctorRejectsEveryExactTerminalizationBindingTamper",
		},
		"cmd/compound/work_proof_chain_doctor_test.go": {
			"TestWorkProofChainDoctorCLIJSON",
			"TestWorkAdvanceExecuteThenClose",
			// Public E2E must assert the v0.3 schema, the proof ref,
			// the item RunAttemptIDs include the execution attempt, and the
			// corrupted doctor must exit exactly 1 with missing_run_attempt.
			"report.SchemaVersion != \"compound.work_proof_chain_doctor.v0.3\"",
			"slices.Contains(report.ProofRefs, \"work-proof-chain-run-attempt\")",
			"slices.Contains(item.RunAttemptIDs, executionRatt)",
			"exitCode != 1",
			"finding.Code == \"missing_run_attempt\"",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			failures = append(failures, rel+" missing: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"work-proof-chain-contract", Fail, "terminal work proof-chain doctor drifted: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"work-proof-chain-contract", Info, "terminal work proof-chain doctor verifies receipt/transition linkage plus RunAttempt readable schema, same-work/action authority, terminal outcome, and exact prepared-intent close integrity"}}
}

// checkMCPorterTopologyContract keeps real MCP catalog ingestion from drifting
// back to normalized/generated topology only. mcporter catalogs are observations:
// broken/auth/offline servers do not surface, and the source becomes
// SourceObservation/no-promote evidence without executing tools or mutating MCP
// configuration.
func checkMCPorterTopologyContract(repoRoot string) []Finding {
	hasImportTopology := false
	hasMCPorterJSON := false
	hasNonSurfacing := false
	hasNoPromote := false
	hasNoExecution := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "capability import-topology") {
			hasImportTopology = true
		}
		if strings.Contains(low, "mcporter list --json") || strings.Contains(low, "mcporter-json") {
			hasMCPorterJSON = true
		}
		if strings.Contains(low, "non-surfacing") ||
			strings.Contains(low, "auth/error/offline") ||
			strings.Contains(low, "auth_required") {
			hasNonSurfacing = true
		}
		if strings.Contains(low, "sourceobservation") &&
			(strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasNoPromote = true
		}
		if strings.Contains(low, "never executes") ||
			strings.Contains(low, "none of these commands execute") ||
			(strings.Contains(low, "does not execute") && strings.Contains(low, "mutate")) {
			hasNoExecution = true
		}
		return nil
	})
	if !hasImportTopology || !hasMCPorterJSON || !hasNonSurfacing || !hasNoPromote || !hasNoExecution {
		return []Finding{{"mcporter-topology-contract", Fail, "public docs do not describe mcporter list --json topology import with non-surfacing auth/error/offline servers, SourceObservation/no-promote, and no MCP execution/config mutation"}}
	}
	return []Finding{{"mcporter-topology-contract", Info, "mcporter catalog import stays observational non-executing capability graph evidence"}}
}

// checkPluginServingContract keeps plugin enable-state as inventory evidence,
// not an executable capability offer. Plugin adapters may be observed by the
// graph, but they must stay non-surfacing and never mutate harness configs.
func checkPluginServingContract(repoRoot string) []Finding {
	hasPluginBoard := false
	hasRetiredAdapter := false
	hasNoPromote := false
	hasNoSurface := false
	hasNoExecution := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "os-plugin-board-json") || strings.Contains(low, "skills-selection-board.json") {
			hasPluginBoard = true
		}
		if strings.Contains(low, "retired `adapter`") || strings.Contains(low, "retired adapter") {
			hasRetiredAdapter = true
		}
		if strings.Contains(low, "sourceobservation") && (strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasNoPromote = true
		}
		if strings.Contains(low, "non-surfacing") || strings.Contains(low, "zero `adapter:plugin:*` offers") {
			hasNoSurface = true
		}
		if strings.Contains(low, "non-executing") ||
			strings.Contains(low, "must not surface plugin adapters") ||
			strings.Contains(low, "execute plugins") ||
			strings.Contains(low, "mutate harness/plugin configs") {
			hasNoExecution = true
		}
		return nil
	})
	if !hasPluginBoard || !hasRetiredAdapter || !hasNoPromote || !hasNoSurface || !hasNoExecution {
		return []Finding{{"plugin-serving-contract", Fail, "public docs do not describe OS plugin board import as retired adapter inventory with SourceObservation/no-promote, non-surfacing offers, and no plugin execution/config mutation"}}
	}
	return []Finding{{"plugin-serving-contract", Info, "plugin serving inventory stays non-surfacing capability graph evidence"}}
}

// checkMemoryEngineContract keeps rented memory engines observable without
// granting them authority. A configured sidecar must be visible through normal
// memory doctor health, leave ledger bytes unchanged, and stay behind the
// MemoryEngine/engine-compare adapter seam.
func checkMemoryEngineContract(repoRoot string) []Finding {
	hasMemoryDoctor := false
	hasConfiguredSidecar := false
	hasSyncEmbedding := false
	hasSemanticReadiness := false
	hasLedgerUnchanged := false
	hasNoAuthority := false
	hasCompare := false
	hasIndexHit := false
	hasObservedFact := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "memory doctor") {
			hasMemoryDoctor = true
		}
		if strings.Contains(low, "compound_mem_engine_bin") || strings.Contains(low, "configured sidecar") {
			hasConfiguredSidecar = true
		}
		if strings.Contains(low, "sync/embedding") {
			hasSyncEmbedding = true
		}
		if strings.Contains(low, "semantic_readiness") {
			hasSemanticReadiness = true
		}
		if strings.Contains(low, "ledger_unchanged") || strings.Contains(low, "ledger bytes unchanged") || strings.Contains(low, "leaves owned ledger bytes unchanged") {
			hasLedgerUnchanged = true
		}
		if strings.Contains(low, "never authority") || strings.Contains(low, "without making it authority") || strings.Contains(low, "engine never owns") {
			hasNoAuthority = true
		}
		if strings.Contains(low, "engine-compare") {
			hasCompare = true
		}
		if strings.Contains(low, "index_hit") {
			hasIndexHit = true
		}
		if strings.Contains(low, "observed_fact") {
			hasObservedFact = true
		}
		return nil
	})
	if !hasMemoryDoctor || !hasConfiguredSidecar || !hasSyncEmbedding || !hasSemanticReadiness || !hasLedgerUnchanged || !hasNoAuthority || !hasCompare || !hasIndexHit || !hasObservedFact {
		return []Finding{{"memory-engine-contract", Fail, "public docs do not describe configured MemoryEngine health through memory doctor sync/embedding, semantic_readiness, ledger_unchanged proof, engine-compare swap parity, never-authority semantics, and the index_hit/observed_fact hit-authority discriminator"}}
	}
	return []Finding{{"memory-engine-contract", Info, "configured MemoryEngine health stays observable rented ledger-safe"}}
}

// checkHarnessEntrypointContract prevents OS-style entrypoint registries from
// drifting into full capability authority. Entrypoint files prove context
// entrypoints, not hook blocking, receipts, skills, or MCP support.
func checkHarnessEntrypointContract(repoRoot string) []Finding {
	hasHarnessObserve := false
	hasOSEntrypoints := false
	hasSessionStartOnly := false
	hasNoPromote := false
	hasNoHookInstall := false
	hasExplicitCapabilityRegistry := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "compound harness observe") {
			hasHarnessObserve = true
		}
		if strings.Contains(low, "--os-entrypoints") {
			hasOSEntrypoints = true
		}
		if strings.Contains(low, "only session-start context") ||
			strings.Contains(low, "only `session_start_context`") ||
			strings.Contains(low, "infer only `session_start_context`") {
			hasSessionStartOnly = true
		}
		if strings.Contains(low, "sourceobservation") &&
			(strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasNoPromote = true
		}
		if strings.Contains(low, "not hook installation") ||
			strings.Contains(low, "does not install hooks") ||
			strings.Contains(low, "without installing hooks") {
			hasNoHookInstall = true
		}
		if strings.Contains(low, "explicit normalized capability registry") ||
			strings.Contains(low, "require an explicit normalized capability registry") {
			hasExplicitCapabilityRegistry = true
		}
		return nil
	})
	if !hasHarnessObserve || !hasOSEntrypoints || !hasSessionStartOnly || !hasNoPromote || !hasNoHookInstall || !hasExplicitCapabilityRegistry {
		return []Finding{{"harness-entrypoint-contract", Fail, "public docs do not describe --os-entrypoints as session-start-only SourceObservation/no-promote evidence with no hook install and no inferred hook/MCP/skill capability"}}
	}
	return []Finding{{"harness-entrypoint-contract", Info, "OS harness entrypoints stay evidence-only session-start context"}}
}

// checkHookSourceControlContract keeps hook mutation gating from drifting back to
// raw harness path strings. PreToolUse must normalize relative targets before
// policy evaluation, persisted ActionIntent artifacts, and read-only git
// inspection; otherwise one action can have two target identities.
func checkHookSourceControlContract(repoRoot string) []Finding {
	hasPreToolUse := false
	hasNormalizeRelative := false
	hasPolicyEvaluation := false
	hasActionPersistence := false
	hasGitInspection := false
	hasNoMutation := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "hook pre-tool-use") || strings.Contains(low, "pretooluse") {
			hasPreToolUse = true
		}
		if strings.Contains(low, "relative") && strings.Contains(low, "target") &&
			(strings.Contains(low, "normalize") || strings.Contains(low, "cwd")) {
			hasNormalizeRelative = true
		}
		if strings.Contains(low, "policy evaluation") {
			hasPolicyEvaluation = true
		}
		if strings.Contains(low, "actionintent") && (strings.Contains(low, "persistence") || strings.Contains(low, "stored")) {
			hasActionPersistence = true
		}
		if strings.Contains(low, "source-control inspection") || strings.Contains(low, "git snapshot") || strings.Contains(low, "read-only git") {
			hasGitInspection = true
		}
		if strings.Contains(low, "inspected repos are not mutated") || strings.Contains(low, "without mutating the repo") ||
			strings.Contains(low, "no cleanup/staging/push authority") {
			hasNoMutation = true
		}
		return nil
	})
	if !hasPreToolUse || !hasNormalizeRelative || !hasPolicyEvaluation || !hasActionPersistence || !hasGitInspection || !hasNoMutation {
		return []Finding{{"hook-source-control-contract", Fail, "public docs do not describe hook pre-tool-use relative target normalization before policy evaluation, ActionIntent persistence, read-only git inspection, and no repo mutation"}}
	}
	return []Finding{{"hook-source-control-contract", Info, "hook source-control gate normalizes target identity before policy and git inspection"}}
}

// checkHookLiveParityContextSpineContract keeps live hook parity from becoming
// a status-only log. SessionStart parity must cite the generated context spine
// artifacts so harness-native context can be traced back to owned Compound
// state.
func checkHookLiveParityContextSpineContract(repoRoot string) []Finding {
	hasDocs := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "hook-live-parity-context-spine-contract") &&
			strings.Contains(low, "--parity-log") &&
			strings.Contains(low, "context request") &&
			strings.Contains(low, "effective contract") &&
			strings.Contains(low, "context pack") &&
			strings.Contains(low, "injection") {
			hasDocs = true
		}
		return nil
	})

	hookBytes, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "hook_live.go"))
	if err != nil {
		return []Finding{{"hook-live-parity-context-spine-contract", Warn, "could not read cmd/compound/hook_live.go: " + err.Error()}}
	}
	hookText := string(hookBytes)
	hasRecordFields := strings.Contains(hookText, "type liveHookParityRecord struct") &&
		strings.Contains(hookText, "ContextRequestID") &&
		strings.Contains(hookText, "ContextRequestRef") &&
		strings.Contains(hookText, "ContractID") &&
		strings.Contains(hookText, "ContractRef") &&
		strings.Contains(hookText, "ContextPackID") &&
		strings.Contains(hookText, "ContextPackRef") &&
		strings.Contains(hookText, "InjectionRef")
	hasInjectionWrite := strings.Contains(hookText, "record.ContextRequestID = res.Injection.ContextRequestID") &&
		strings.Contains(hookText, "record.ContractRef = res.Injection.ContractRef") &&
		strings.Contains(hookText, "record.ContextPackRef = res.Injection.ContextPackRef") &&
		strings.Contains(hookText, "record.InjectionRef = res.Injection.Ref")

	testBytes, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "hook_live_test.go"))
	if err != nil {
		return []Finding{{"hook-live-parity-context-spine-contract", Warn, "could not read cmd/compound/hook_live_test.go: " + err.Error()}}
	}
	testText := string(testBytes)
	hasRegression := strings.Contains(testText, "TestHookLiveSessionStartParityRecordsContextSpineRefs") &&
		strings.Contains(testText, "context_request") &&
		strings.Contains(testText, "effective_contract") &&
		strings.Contains(testText, "context_pack") &&
		strings.Contains(testText, "os.Stat(filepath.Join(homePath, ref))")

	if !hasDocs || !hasRecordFields || !hasInjectionWrite || !hasRegression {
		return []Finding{{"hook-live-parity-context-spine-contract", Fail, "live hook parity must document and test SessionStart context-spine refs: liveHookParityRecord context request/effective contract/context pack/injection refs, injection writeback, and temp-home artifact proof"}}
	}
	return []Finding{{"hook-live-parity-context-spine-contract", Info, "live hook parity records context spine refs with artifact proof"}}
}

// checkHookReadinessActionContract keeps hook readiness rows actionable without
// letting agents flip registration. Interactive Codex observes by default and
// may block only through explicit enforcement proof, while genuinely missing
// live adapters retain fixture/operator actions.
func checkHookReadinessActionContract(repoRoot string) []Finding {
	statusBytes, err := os.ReadFile(filepath.Join(repoRoot, "docs", "architecture", "status.md"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read docs/architecture/status.md: " + err.Error()}}
	}
	status := strings.ToLower(string(statusBytes))
	hasDocs := strings.Contains(status, "hook-readiness-action-contract") &&
		strings.Contains(status, "codex sessionstart/userpromptsubmit") &&
		strings.Contains(status, "reconciliation `registered` means the active invocation matches while handler-content bytes remain unverified") &&
		strings.Contains(status, "reconciliation `ready` requires matching desired/observed handler-content revisions") &&
		strings.Contains(status, "contextpack") &&
		strings.Contains(status, "effectivecontract") &&
		strings.Contains(status, "default interactive codex pretooluse is observational") &&
		strings.Contains(status, "must not report `native_blocking` or emit a native permission decision") &&
		strings.Contains(status, "`native_blocking` is reserved for an explicitly blocking binding installed with `--enforce`") &&
		strings.Contains(status, "native_blocking") &&
		strings.Contains(status, "codex-pretooluse-native-gate-proof") &&
		strings.Contains(status, "permissiondecision") &&
		strings.Contains(status, "prove_fixture_hook") &&
		strings.Contains(status, "expected_exit_code") &&
		strings.Contains(status, "operator_live_adapter_required") &&
		strings.Contains(status, "compound contract hooks-authority") &&
		strings.Contains(status, "before any registration flip")

	contractBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "contract", "contract.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read internal/contract/contract.go: " + err.Error()}}
	}
	contractText := string(contractBytes)
	envelopeBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "envelope", "envelope.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read internal/envelope/envelope.go: " + err.Error()}}
	}
	envelopeText := string(envelopeBytes)
	hookReadinessActionsPresent := len(missingGoStructFields("internal/envelope/envelope.go", envelopeText, "HookReadiness", map[string]goStructFieldContract{
		"Actions": {Type: "[]HookReadinessAction", Tag: `json:"actions,omitempty"`},
	})) == 0
	hasSource := strings.Contains(contractText, "hookRuntimeReadinessActions") &&
		strings.Contains(contractText, "InstalledHookRuntimeReadinessRows") &&
		strings.Contains(contractText, "row.FixturePresent && row.LiveAdapter") &&
		strings.Contains(contractText, `row.Status = "ready"`) &&
		strings.Contains(envelopeText, "type HookReadinessAction struct") &&
		hookReadinessActionsPresent &&
		strings.Contains(contractText, "envelopeHookReadinessActions") &&
		strings.Contains(contractText, "hookReadinessAction") &&
		strings.Contains(contractText, "InvocationRevision string") &&
		strings.Contains(contractText, "prove_fixture_hook") &&
		strings.Contains(contractText, "ExpectedExitCode") &&
		strings.Contains(contractText, "installedHookStatusForCommand") &&
		strings.Contains(contractText, `canonicalHookEvent(event) == "PreToolUse"`) &&
		strings.Contains(contractText, "compoundLivePreToolUseCommand(command)") &&
		strings.Contains(contractText, `!hookCommandHasExactField(command, "--enforce")`) &&
		strings.Contains(contractText, `return "non_blocking"`) &&
		strings.Contains(contractText, `row.Status = "non_blocking"`) &&
		strings.Contains(contractText, `row.Status = "native_blocking"`) &&
		strings.Contains(contractText, "operator_install_required") &&
		strings.Contains(contractText, "operator_live_adapter_required") &&
		strings.Contains(contractText, "compound contract hooks-authority") &&
		strings.Contains(contractText, "must not flip registration")

	contractTestBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "contract", "contract_test.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read internal/contract/contract_test.go: " + err.Error()}}
	}
	contractTest := string(contractTestBytes)
	hasContractTest := strings.Contains(contractTest, "TestInstalledHookRuntimeReadinessAddsMissingAdapterActions") &&
		strings.Contains(contractTest, "TestImportCodexHooksJSONFeedsRuntimeContract") &&
		strings.Contains(contractTest, "observation-only Codex PreToolUse status") &&
		strings.Contains(contractTest, `gate.Status != "non_blocking"`) &&
		strings.Contains(contractTest, `readiness.Status != "non_blocking"`) &&
		strings.Contains(contractTest, "observation-only Codex PreToolUse projected as blocking") &&
		strings.Contains(contractTest, "TestImportCodexHooksJSONPreservesExplicitPreToolUseEnforcement") &&
		strings.Contains(contractTest, "compound hook pre-tool-use --live --harness codex --enforce") &&
		strings.Contains(contractTest, `readiness.Status != "native_blocking"`) &&
		strings.Contains(contractTest, "prove_fixture_hook") &&
		strings.Contains(contractTest, "ExpectedExitCode") &&
		strings.Contains(contractTest, "native_blocking") &&
		strings.Contains(contractTest, "privacy-safe invocation identity") &&
		strings.Contains(contractTest, "InvocationRevision") &&
		strings.Contains(contractTest, "findEnvelopeHookAction") &&
		strings.Contains(contractTest, "operator_live_adapter_required") &&
		strings.Contains(contractTest, "compound hook session-start --live --harness codex")
	workbenchBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "work", "workbench.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read internal/work/workbench.go: " + err.Error()}}
	}
	workbenchText := string(workbenchBytes)
	hasWorkbenchAction := strings.Contains(workbenchText, "codex-pretooluse-native-gate-proof") &&
		strings.Contains(workbenchText, "mixed valid and misplaced-canon apply_patch receives permissionDecision deny") &&
		strings.Contains(workbenchText, "no hook install")
	proofCheckBytes, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "proof_check.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read cmd/compound/proof_check.go: " + err.Error()}}
	}
	proofCheckText := string(proofCheckBytes)
	hasProofCheck := strings.Contains(proofCheckText, "codex-pretooluse-native-gate-proof") &&
		strings.Contains(proofCheckText, "runProofCheckCodexPreToolUseNativeGate") &&
		strings.Contains(proofCheckText, `"--enforce"`) &&
		strings.Contains(proofCheckText, `"tool_name":"apply_patch"`) &&
		strings.Contains(proofCheckText, `PermissionDecision != "deny"`) &&
		strings.Contains(proofCheckText, "COMPOUND_HOME=")

	permissionPromptTestBytes, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "hook_permission_prompt_test.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read cmd/compound/hook_permission_prompt_test.go: " + err.Error()}}
	}
	permissionPromptTest := string(permissionPromptTestBytes)
	hasPermissionPromptTest := strings.Contains(permissionPromptTest, "TestLivePreToolUseOnlyPublishesNativePermissionDecisionWhenEnforced") &&
		strings.Contains(permissionPromptTest, `[]string{"codex", "claude-code"}`) &&
		strings.Contains(permissionPromptTest, `"pre-tool-use", "--live", "--harness", harnessName`) &&
		strings.Contains(permissionPromptTest, `observed.HookSpecificOutput.PermissionDecision; got != ""`) &&
		strings.Contains(permissionPromptTest, "observe-only PreToolUse did not persist the held PolicyDecision evidence") &&
		strings.Contains(permissionPromptTest, `"pre-tool-use", "--live", "--harness", harnessName, "--enforce"`) &&
		strings.Contains(permissionPromptTest, `enforced.HookSpecificOutput.PermissionDecision; got == ""`)

	cliTestBytes, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "main_test.go"))
	if err != nil {
		return []Finding{{"hook-readiness-action-contract", Warn, "could not read cmd/compound/main_test.go: " + err.Error()}}
	}
	cliTest := string(cliTestBytes)
	hasCLITest := strings.Contains(cliTest, `findHookReadiness(authorityReport.HookReadiness, "codex", "SessionStart")`) &&
		strings.Contains(cliTest, `findHookReadiness(authorityReport.HookReadiness, "codex", "UserPromptSubmit")`) &&
		strings.Contains(cliTest, `findHookReadiness(authorityReport.HookReadiness, "codex", "PreToolUse")`) &&
		strings.Contains(cliTest, `got.Status != "ready"`) &&
		strings.Contains(cliTest, `findHookGateReadiness(authorityReport.PreToolUseReadiness, "codex")`) &&
		strings.Contains(cliTest, `got.Status != "non_blocking" || !got.NativeBlock || got.Installed || got.AdapterRequired || len(got.Actions) != 0`) &&
		strings.Contains(cliTest, "codex-pretooluse-native-gate-proof") &&
		strings.Contains(cliTest, `"status": "blocked"`)

	if !hasDocs || !hasSource || !hasContractTest || !hasWorkbenchAction || !hasProofCheck || !hasPermissionPromptTest || !hasCLITest {
		return []Finding{{"hook-readiness-action-contract", Fail, "hook readiness must distinguish registered invocation from content-verified ready, keep default Codex PreToolUse non_blocking with no native permission decision, prove explicit --enforce native_blocking allow/deny behavior through codex-pretooluse-native-gate-proof, and retain prove_fixture_hook, expected_exit_code, and operator_live_adapter_required actions for missing live adapters"}}
	}
	return []Finding{{"hook-readiness-action-contract", Info, "hook readiness separates registration from content proof, keeps default Codex observation non-blocking, proves explicit native blocking, and retains fixture/operator actions for missing adapters"}}
}

// checkStandingGrantContract keeps autonomy grants from drifting back to
// unconditional allow-list entries. Grants must describe a scoped envelope and
// leave final enforcement in PolicyDecision/Gate.
func checkStandingGrantContract(repoRoot string) []Finding {
	hasStandingGrant := false
	hasScopeBudget := false
	hasBlastReversibility := false
	hasTargetCostFields := false
	hasLeavingEnvelope := false
	hasPolicyGate := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "standinggrant") || strings.Contains(low, "standing grant") {
			hasStandingGrant = true
		}
		if strings.Contains(low, "scope") && strings.Contains(low, "budget") {
			hasScopeBudget = true
		}
		if (strings.Contains(low, "blast_radius") || strings.Contains(low, "blast-radius")) && strings.Contains(low, "reversibility") {
			hasBlastReversibility = true
		}
		if strings.Contains(low, "target_path") && (strings.Contains(low, "estimated_cost") || strings.Contains(low, "max_usd") || strings.Contains(low, "max_tokens")) {
			hasTargetCostFields = true
		}
		if strings.Contains(low, "leaving") && (strings.Contains(low, "escalate") || strings.Contains(low, "hold") || strings.Contains(low, "blocked")) {
			hasLeavingEnvelope = true
		}
		if (strings.Contains(low, "policydecision") || strings.Contains(low, "policy decision")) &&
			(strings.Contains(low, "gate") || strings.Contains(low, "adapter rejects") || strings.Contains(low, "enforces")) {
			hasPolicyGate = true
		}
		return nil
	})
	if !hasStandingGrant || !hasScopeBudget || !hasBlastReversibility || !hasTargetCostFields || !hasLeavingEnvelope || !hasPolicyGate {
		return []Finding{{"standing-grant-contract", Fail, "public docs do not describe scoped StandingGrant envelopes with target_path/budget/blast/reversibility and PolicyDecision/Gate enforcement when leaving the envelope"}}
	}
	for _, rel := range []string{"AGENTS.md", "skills/compound-land/SKILL.md", "skills/compound-execute/SKILL.md"} {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			return []Finding{{"standing-grant-contract", Fail, "could not read standing-grant push doctrine at " + rel + ": " + err.Error()}}
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "never push to a public remote without explicit operator confirmation") {
			return []Finding{{"standing-grant-contract", Fail, rel + " contradicts the standing push grant with the retired ask-before-normal-push rule"}}
		}
		hasExistingRemoteGrant := strings.Contains(low, "normal") &&
			strings.Contains(low, "push") &&
			strings.Contains(low, "existing remote") &&
			strings.Contains(low, "standing") &&
			strings.Contains(low, "grant")
		hasSeparateRedactGate := strings.Contains(low, "clean") &&
			strings.Contains(low, "redact") &&
			(strings.Contains(low, "separate") || strings.Contains(low, "own command"))
		hasConfirmationExceptions := (strings.Contains(low, "force-push") || strings.Contains(low, "force push")) &&
			strings.Contains(low, "history rewrite") &&
			(strings.Contains(low, "new remote") || strings.Contains(low, "new-remote")) &&
			strings.Contains(low, "confirmation")
		if !hasExistingRemoteGrant || !hasSeparateRedactGate || !hasConfirmationExceptions {
			return []Finding{{"standing-grant-contract", Fail, rel + " must say normal push to the existing remote follows the standing grant, clean redact is a separate required gate, and force-push/history rewrite/new remote require confirmation"}}
		}
	}
	return []Finding{{"standing-grant-contract", Info, "StandingGrant remains a scoped policy envelope enforced by PolicyDecision/Gate, with consistent push boundaries across served ceremonies"}}
}

func checkPlacementGuardContract(repoRoot string) []Finding {
	hasPlacementGuard := false
	hasPolicyContext := false
	hasModes := false
	hasConfidenceBlock := false
	hasOpenWriteOverride := false
	hasFailOpen := false
	hasGateEnforce := false
	hasOverrideObservation := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "placementguard") || strings.Contains(low, "placement guard") {
			hasPlacementGuard = true
		}
		if (strings.Contains(low, "policydecision") || strings.Contains(low, "policy decision")) && strings.Contains(low, "context") {
			hasPolicyContext = true
		}
		if strings.Contains(low, "observe") && strings.Contains(low, "warn") && strings.Contains(low, "block") {
			hasModes = true
		}
		if strings.Contains(low, "confidence:block") && (strings.Contains(low, "hard-hold") || strings.Contains(low, "hard hold") || strings.Contains(low, "hold")) {
			hasConfidenceBlock = true
		}
		if strings.Contains(low, "open_write") && strings.Contains(low, "placement_override") {
			hasOpenWriteOverride = true
		}
		if strings.Contains(low, "fail open") || strings.Contains(low, "fail-open") {
			hasFailOpen = true
		}
		if strings.Contains(low, "hook pre-tool-use --enforce") && (strings.Contains(low, "gate exit") || strings.Contains(low, "exit code")) {
			hasGateEnforce = true
		}
		// A PLACEMENT_OVERRIDE fail-open stays fail-open, but its use must now be
		// auditable beyond the in-decision reason string: docs must say the fired
		// override is persisted as an observation, not just reasoned about in place.
		if strings.Contains(low, "placement_override") && strings.Contains(low, "observation") &&
			(strings.Contains(low, "audit") || strings.Contains(low, "persist")) {
			hasOverrideObservation = true
		}
		return nil
	})
	if !hasPlacementGuard || !hasPolicyContext || !hasModes || !hasConfidenceBlock || !hasOpenWriteOverride || !hasFailOpen || !hasGateEnforce || !hasOverrideObservation {
		return []Finding{{"placement-guard-contract", Fail, "public docs do not describe PlacementGuard as PolicyDecision context with observe/warn/block, confidence:block threshold, OPEN_WRITE/PLACEMENT_OVERRIDE fail-open behavior, Gate exit enforcement, and a persisted audit observation whenever PLACEMENT_OVERRIDE fires"}}
	}
	return []Finding{{"placement-guard-contract", Info, "PlacementGuard remains PolicyDecision context with staged fail-open enforcement, and PLACEMENT_OVERRIDE use is persisted as an audit observation"}}
}

// checkProductContractContract keeps project/release source contracts from
// becoming product analytics or strategy authority. They may register product
// identity and deterministic surfaces/channels/privacy, but signals,
// hypotheses, causality, and actions must enter through product signal/review.
func checkProductContractContract(repoRoot string) []Finding {
	hasProjectContractImport := false
	hasReleaseImport := false
	hasDeterministicFields := false
	hasNoAnalytics := false
	hasNoCausality := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "product import") && strings.Contains(low, "--project-contract") {
			hasProjectContractImport = true
		}
		if strings.Contains(low, "--release") || strings.Contains(low, "release contract") {
			hasReleaseImport = true
		}
		if strings.Contains(low, "deterministic") &&
			strings.Contains(low, "identity") &&
			strings.Contains(low, "surface") &&
			strings.Contains(low, "channel") &&
			strings.Contains(low, "privacy") {
			hasDeterministicFields = true
		}
		if strings.Contains(low, "never analytics signals") ||
			strings.Contains(low, "no analytics signal") ||
			strings.Contains(low, "does not create") && strings.Contains(low, "signals") {
			hasNoAnalytics = true
		}
		if strings.Contains(low, "never") && strings.Contains(low, "causality") ||
			strings.Contains(low, "without causal attribution") ||
			strings.Contains(low, "does not declare causality") {
			hasNoCausality = true
		}
		return nil
	})
	if !hasProjectContractImport || !hasReleaseImport || !hasDeterministicFields || !hasNoAnalytics || !hasNoCausality {
		return []Finding{{"product-contract-contract", Fail, "public docs do not describe product --project-contract/--release as deterministic identity/surface/channel/privacy intake only, with no analytics signals or causality"}}
	}
	return []Finding{{"product-contract-contract", Info, "product project/release contracts stay deterministic non-causal intake"}}
}

// convergenceDebt reports the two-stack memory debt as a tracked, visible Info finding —
// the coherent treatment when a duplicate cannot yet be retired (Law 9: make it observable).
// checkCapabilityEvaluationContract keeps scored capability-evaluation records
// from drifting into capability offers or promotion authority. Evaluations
// critique already-known graph nodes; they do not mint executable skills.
// checkProductLoopOpenHypothesisContract keeps product loops from becoming a
// status-only list of hypothesis ids. Open hypotheses must carry the measurement
// and next-action contract so a future agent can advance the loop.
func checkProductLoopOpenHypothesisContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/product/loops.go": {
			"OpenHypothesesByMissingSignalSource",
			"SignalsWithoutHypothesis",
			"LoopSignalHypothesisGap",
			"ClosedReviewsByNextAction",
			"ActionCards",
			"[]LoopActionCard",
			"type LoopActionCard struct",
			"validateLoopActionCard",
			"loopProductActionConsumptionIndex",
			"ConsumedByWorkIDs",
			"product_action_card:",
			"ListWithArchived",
			"product_signal_import",
			"product_signal_hypothesis_review",
			"product_review_plan",
			"product_next_bounded_move",
			"signalWithoutHypothesisActionCard",
			"Metrics:           compact(h.Metrics)",
			"NextAction:        h.NextAction",
		},
		"cmd/compound/product_loops.go": {
			"renderLoopMissingSignalSources",
			"renderLoopSignalHypothesisGaps",
			"renderLoopReviewNextActions",
			"renderLoopActionCards",
			"open_hypotheses_by_missing_signal_source",
			"signals_without_hypothesis",
			"closed_reviews_by_next_action",
			"action_cards",
			"consumed=true",
			"metrics=",
			"window=",
			"next=",
			"renderLoopHypotheses",
		},
		"internal/product/loops_test.go": {
			"open hypothesis should expose measurement/review contract",
			"searchHypothesis.Metrics[0] != \"gsc_clicks\"",
			"searchHypothesis.NextAction",
			"TestDryRunDerivedHypothesisClosesAfterSignalReview",
			"BuildOutboundDryRunEvidencePlan",
			"RecordOutboundDryRunEvidencePlan",
			"ApplyOutboundDryRunEvidenceReview",
			"BuildReviewPlan",
			"findLoopMissingSignalSource",
			"findLoopSignalHypothesisGap",
			"findLoopReviewNextAction",
			"findLoopActionCard",
			"TestBuildLoopModelSuppressesConsumedClosedReviewActionCard",
			"seedCompletedProductActionWork",
			"consumed closed review must not remain in top-level action cards",
			"product_signal_import",
			"product_signal_hypothesis_review",
			"product_review_plan",
			"product_next_bounded_move",
			"signal-without-hypothesis group should expose reviewed synthesis card",
			"reviewed hypothesis should leave open set",
			"loop history should retain applied review and signal ref",
		},
		"cmd/compound/product_loops_test.go": {
			"TestProductLoopsTextPrintsOpenHypothesisContract",
			"metrics=distribution_draft_quality",
			"next=wait for real distribution signal evidence before review",
			"open_hypotheses_by_missing_signal_source",
			"product_signal_import",
			"command=compound product signal",
		},
		"cmd/compound/main_test.go": {
			"cmdProduct([]string{\"loops\", \"--json\", \"--product\", \"product:focus_notes\"})",
			"reviewed hypothesis should leave open_hypotheses",
			"product loops should retain review history with signal ref",
			"product loops should group closed review by next action",
			"TestProductActionPlanAndReviewCLIExposeActionCardsThroughReviewQueue",
			"product.ProductActionReviewArea",
			"action-plan",
			"action-review",
			"action-work-plan",
			"action-work-plan should not admit work",
			"admit-plan",
			"admit-plan should not mutate product timeline",
			"admission_previews=1",
			"runnable action context",
			"prepare ready with reviewed action context",
			"advance-plan text missing action context",
			"claim-plan text missing action context",
			"observe-run text missing action context",
			"action-plan text missing receipt-ready boundary",
			"act --plan did not record planned action",
		},
		"internal/product/action_plan.go": {
			"ProductActionReviewArea",
			"BuildActionPlan",
			"RecordActionPlan",
			"ApplyActionReview",
			"ActionWorkDraft",
			"BuildActionWorkDraft",
			"ExpectedValidation",
			"StopConditions",
			"LoopActionCard",
			"loop.SignalsWithoutHypothesis",
			"product_signal_change_event:",
			"product_signal:",
			"do not run the action command while generating this work-plan preview",
			"does not mutate ProductSignal",
			"does not admit action card into WorkEnvelope execution",
		},
		"internal/product/action_plan_test.go": {
			"TestActionPlanMakesLoopActionCardReviewableWithoutProductMutation",
			"ProductActionReviewArea",
			"product_next_bounded_move",
			"product_signal_hypothesis_review",
			"product_signal:sig_loop_unmatched",
			"BuildActionWorkDraft",
			"action work draft should preserve command/boundary/proof/evidence",
			"action review must not mutate product loop ledgers",
			"recorded action review should make queue terminal",
		},
		"internal/work/workbench.go": {
			"AdmissionPreviews",
			"workbenchAdmissionPreviews",
			"create-plans",
			"workbenchCreatePlanIsAdmissionPreview",
			"prepare ready with reviewed action context",
		},
		"internal/work/workbench_test.go": {
			"TestWorkbenchSurfacesAdmissionPreviewsWithoutAdmittingWork",
			"create-plan should not write work lifecycle events",
			"admission preview must not become dispatchable work",
			"admitted work should retire admission preview",
		},
		"internal/work/create_plan.go": {
			"WorkActionContext",
			"AdmitCreatePlanReport",
			"AdmitCreatePlanPath",
			"AdmitCreatePlan",
			"validateCreatePlanForAdmission",
			"admitted_from_create_plan",
			"create_plan:",
		},
		"internal/work/create_plan_test.go": {
			"TestAdmitCreatePlanCreatesWorkAndRetiresPreview",
			"TestCreatePlanActionContextReachesWorkbenchAndDispatch",
			"admitted work should retire admission preview",
			"dispatch candidate missing action context",
			"ready advance-plan",
			"claim-plan",
			"observe-run",
			"verifying workbench",
			"repeat admission should be idempotent",
		},
		"internal/work/advance_plan.go": {
			"ActionContext",
			"actionContextForWork",
		},
		"internal/work/claim_plan.go": {
			"ActionContext",
			"actionContextForWork",
		},
		"internal/work/run_observation.go": {
			"ActionContext",
			"actionContextForWork",
		},
		"internal/work/action_execution_plan.go": {
			"ActionExecutionPlan",
			"ReceiptDraft",
			"ActionCommand",
			"RequiredInputs",
			"ExpectedValidation",
			"StopConditions",
			"Boundary",
			"Proof",
			"RecordCommand",
			"CloseCommand",
			"close-plan",
			"--action-plan",
			"does not execute action_context command",
		},
		"internal/work/close_plan.go": {
			"ActionPlanPath",
			"ActionExecutionPlanID",
			"ReceiptDraft",
			"ExpectedValidation",
			"not recorded",
			"LoadActionExecutionPlan",
		},
		"internal/work/action_execution_plan_test.go": {
			"TestActionExecutionPlanConsumesActionContextWithoutExecuting",
			"TestActionExecutionPlanRequiresClaimedOrRunningWork",
			"receipt draft missing action proof boundary",
			"action execution planning mutated work",
		},
		"internal/work/close_plan_test.go": {
			"TestClosePlanConsumesRecordedActionExecutionPlanReceiptDraft",
			"not recorded",
			"ReceiptDraft",
			"ExpectedValidation",
		},
		"cmd/compound/product_cli.go": {
			"action-work-plan",
			"product.BuildActionWorkDraft",
			"workStore.CreatePlan",
			"plan_work_from_reviewed_product_action",
		},
		"cmd/compound/work_cli.go": {
			"action-plan",
			"ActionContext",
			"action_context:",
			"ActionExecutionPlan",
			"LoadActionExecutionPlan",
			"record_after_execution",
			"close_after_validation",
			"--action-plan",
			"action_execution_plan:",
			"expected_validation:",
			"plan.ActionContext",
			"result.ActionContext",
			"admit-plan",
			"store.AdmitCreatePlanPath",
		},
		"cmd/compound/workbench_cli.go": {
			"admission_previews=",
			"admission_preview:",
		},
		"internal/product/review_queue.go": {
			"ProductActionReviewArea",
			"annotateProductActionReviewItem",
			"compound\", \"product\", \"action-review\"",
			"record_product_action_review",
		},
		"docs/architecture/status.md": {
			"`compound product loops`",
			"open hypotheses",
			"metrics/window/next-action contract",
			"missing signal source",
			"closed reviews by next action",
			"action cards",
			"product_signal_import",
			"product_review_plan",
			"product_next_bounded_move",
			"dry-run-derived hypothesis",
			"leaves open_hypotheses",
			"ProductSignal",
			"Product action-card status note",
			"`compound product action-plan --product <id> --card <card_id>`",
			"`reviews/product-action-cards`",
			"`compound product action-review <review.yaml>`",
			"`compound product action-work-plan --reviewed <reviewed.json>`",
			"`compound work admit-plan --plan <create-plan.json>`",
			"`action_context`",
			"`action_summary`",
			"review-gate `product action-plan` command is not completion proof",
			"`work advance-plan <id> --to ready`",
			"`claim-plan`",
			"`observe-run`",
			"`work action-plan <id>`",
			"`work act <id> --plan <action-execution-plan.json>`",
			"`work close-plan <id> --action-plan <action-execution-plan.json>`",
			"receipt-close path",
			"does not execute the reviewed action command",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		text := string(b)
		failures = append(failures, missingPhrases(rel, text, phrases)...)
		if rel == "internal/product/loops.go" {
			failures = append(failures, missingGoStructFields(rel, text, "LoopHypothesis", map[string]goStructFieldContract{
				"Metrics":           {Type: "[]string", Tag: `json:"metrics,omitempty"`},
				"ObservationWindow": {Type: "string", Tag: `json:"observation_window,omitempty"`},
				"NextAction":        {Type: "string", Tag: `json:"next_action,omitempty"`},
				"EvidenceRefs":      {Type: "[]string", Tag: `json:"evidence_refs,omitempty"`},
			})...)
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"product-loop-open-hypothesis-contract", Fail, "product loop open hypotheses lost measurement/next-action contract: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"product-loop-open-hypothesis-contract", Info, "product loops expose open hypothesis metrics/window/next-action contracts"}}
}

// checkOperatorProductLoopProjectionContract keeps the operator front door from
// drifting back to work-only status while product loops already expose evidence
// debt. `orient --operator` must consume the product read model, not recreate it.
func checkOperatorProductLoopProjectionContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/orient_operator.go": {
			"ProductLoops",
			"*orientProductLoops",
			"json:\"product_loops,omitempty\"",
			"buildOrientProductLoops",
			"product.BuildLoopModel",
			"OpenHypothesesByMissingSignalSource",
			"ClosedReviewsByNextAction",
			"ActionCardCount",
			"orientLoopActionCard",
			"orientLoopActionCards",
			"product_loops: products=%d open_hypothesis_debt=%d closed_review_actions=%d action_cards=%d",
			"compound\", \"product\", \"loops\"",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorCarriesProductLoopEvidenceDebtProjection",
			"seedOrientProductLoopProjectionHome",
			"missing_signal_source=posthog",
			"closed_review_next=keep screenshot one more window",
			"product_signal_import",
			"product_next_bounded_move",
			"action_cards=",
			"compound product action-plan",
		},
		"docs/architecture/status.md": {
			"Operator product-loop projection note",
			"`compound orient --operator --json.product_loops`",
			"`product.BuildLoopModel` read model",
			"action cards",
			"`compound product action-plan` review gate",
			"`compound product loops` as drill-down",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"operator-product-loop-projection-contract", Fail, "operator orient lost product-loop evidence debt projection: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"operator-product-loop-projection-contract", Info, "operator orient consumes product loop evidence-debt projection"}}
}

// checkObservationBatchContract keeps external platforms behind one generic
// observation grammar instead of letting provider-specific connector concepts
// become core authority.
func checkObservationBatchContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/observation/observation.go": {
			"compound.observation_batch.v0.1",
			"RecordSourceIntakeReport",
			"IntakeProjectionCommand",
			"RouteProductSignal",
			"RouteKnowledgeExport",
			"RouteTraceEnvelope",
			"RouteRouteEvidence",
			"RouteSourceObservation",
			"RouteCandidateOnly",
			"RouteBlockedPrivate",
			"product.AdaptSignalExport",
			"product.ApplySignal",
			"ApplySourceObservations",
			"ApplyKnowledge",
			"ApplyRouteEvidence",
			"ApplyEvents",
			"SourceObservationDocument",
			"sourceobs.Apply",
			"SourceObservationApplyResult",
			"KnowledgeExportDocument",
			"knowledge.Import",
			"KnowledgeApplyResult",
			"RouteEvidenceDocument",
			"routeevidence.RecordJoined",
			"RouteEvidenceApplyResult",
			"EventSourceRow",
			"eventsource.ImportOSEvents",
			"EventApplyResult",
		},
		"internal/observation/intake.go": {
			"compound.source_intake_projection.v0.1",
			"compound://sources/intake",
			"LoadSourceIntakeProjection",
			"RecordSourceIntakeReport",
			"SourceIntakeActionCard",
			"ActionCardCount",
			"RequiredInputs",
			"ExpectedValidation",
			"StopConditions",
			"AppliedIDs",
			"BlockedRows",
			"SourceCapturedAt",
			"NextCommand",
			"knowledge_extraction_pending",
			"sourceIntakeOpenKnowledgeExtractionIDs",
			"compound\", \"knowledge\", \"extract\", \"--item\"",
			"requested action would run bulk --pending extraction",
			"sourceIntakeRows",
		},
		"internal/observation/intake_work.go": {
			"compound.source_intake_work_plan_report.v0.1",
			"BuildSourceIntakeWorkPlan",
			"BuildSourceIntakeWorkDraft",
			"source_intake_action_card",
			"work.CreatePlan",
			"WorkActionContext",
			"RequiredInputs",
			"ExpectedValidation",
			"StopConditions",
			"do not execute the source-intake action command while creating or admitting this work-plan",
		},
		"internal/observation/review.go": {
			"ObservationReviewArea",
			"observation-candidates",
			"CandidateReviewResult",
			"candidates.CapabilityObs",
			"panel.SelectLensesWithContext",
			"reviewartifact.DecisionCard",
			"compound panel plan --target-kind capability_observation",
			"no truth promotion",
		},
		"cmd/compound": {
			"source observe-batch",
			"source intake",
			"intake-work-plan",
			"--dry-run",
			"--apply-product-signals",
			"--apply-source-observations",
			"--apply-knowledge",
			"--apply-route-evidence",
			"--apply-events",
			"--review-candidates",
			"LoadSourceIntakeProjection",
			"BuildSourceIntakeWorkPlan",
		},
		"internal/reviewqueue/reviewqueue.go": {
			"observation.AnnotateReviewQueue",
		},
		"cmd/compound/orient_operator.go": {
			"SourceIntake",
			"buildOrientSourceIntake",
			"ActionCardCount",
			"ActionCards",
			`"compound", "source", "intake", "--json"`,
			"LoadSourceIntakeProjection",
		},
		"cmd/compound/main_test.go": {
			"TestSourceIntakeCLIAndOrientConsumeRecordedObservationBatches",
			"TestSourceIntakeWorkPlanCLIAdmitsActionCardThroughWorkbenchPreview",
			"TestSourceObserveBatchCLIAppliesGeneratedSourceObservation",
			"TestSourceObserveBatchCLIAppliesContentToKnowledge",
			"TestSourceObserveBatchCLIAppliesRouteEvidence",
			"TestSourceObserveBatchCLIAppliesEventsToTraceEnvelope",
			"generated-system-source.json",
			"content-knowledge-export.json",
			"route-evidence.json",
			"event-trace-envelope.json",
			"product-signals.jsonl",
			"TestSourceObserveBatchCLIRecordsCandidateReviewMaterial",
			"writeObservationPanelFixtureResponses",
			"cmdPanel([]string{\"run\", \"--plan\"",
			"\"panel-verdicts\"",
		},
		"internal/observation/observation_test.go": {
			"TestSourceIntakeProjectionSummarizesRecordedObservationBatches",
			"TestSourceIntakeProjectionBuildsActionCardsForRowsNeedingAttention",
			"TestSourceIntakeActionCardBuildsWorkCreatePlanContext",
			"knowledge_extraction_pending",
			"done extraction should not keep source-intake card",
			"stale_source",
			"skipped_route",
			"blocked_private_input",
			"unsupported_kind",
			"LoadSourceIntakeProjection",
			"RouteBlockedPrivate",
			"AppliedIDs",
			"BlockedRows",
		},
		"fixtures/observation/generated-system-source.json": {
			"generated_view",
			"compound_projection",
			"obs_fixture_generated_workbench",
		},
		"fixtures/observation/content-knowledge-export.json": {
			"web_capture",
			"research_note",
			"obs_fixture_content_knowledge",
		},
		"fixtures/observation/route-evidence.json": {
			"route_gateway",
			"execution_route",
			"obs_fixture_route_evidence",
		},
		"fixtures/observation/event-trace-envelope.json": {
			"event_stream",
			"workflow:observation-intake",
			"obs_fixture_event_trace",
		},
		"docs/architecture/status.md": {
			"ObservationBatch status note",
			"`compound source observe-batch <batch.json> --dry-run --json`",
			"`compound.observation_batch.v0.1`",
			"generic observation grammar",
			"ProductSignal",
			"KnowledgeExport",
			"TraceEnvelope",
			"ExecutionRouteEvidence",
			"SourceObservation",
			"`--apply-knowledge`",
			"`--apply-events`",
			"`compound source intake --json`",
			"`compound.source_intake_projection.v0.1`",
			"`knowledge_extraction_pending`",
			"`compound knowledge extract --item`",
			"`compound source intake-work-plan --card <card_id> --json`",
			"`work admit-plan`",
			"`action-plan`",
			"`close-plan`",
			"`compound orient --operator --json.needs_me.source_intake`",
			"`eventsource.ImportOSEvents`",
			"`knowledge.Import`",
			"extraction requests",
			"`--apply-source-observations`",
			"`sourceobs.Apply`",
			"`NoPromoteDecision`",
			"candidate/review material",
			"`--review-candidates`",
			"`compound reviews --area observation-candidates --json`",
			"`compound panel plan --target-kind capability_observation`",
			"`compound panel run --plan <run_id> --fixtures <dir> --json`",
			"`panel-verdicts`",
			"no external account",
			"no credentials",
		},
		"docs/architecture/contracts/README.md": {
			"ObservationBatch contract note",
			"`--apply-source-observations`",
			"`--apply-knowledge`",
			"`knowledge.Import`",
			"`--apply-route-evidence`",
			"`routeevidence.RecordJoined`",
			"`--apply-events`",
			"`compound source intake --json`",
			"`compound.source_intake_projection.v0.1`",
			"`compound source intake-work-plan --card <card_id> --json`",
			"`work admit-plan`",
			"`action-plan`",
			"`close-plan`",
			"`eventsource.ImportOSEvents`",
			"`sourceobs.Apply`",
			"`NoPromoteDecision`",
			"does not mutate ProductSignal except the explicit product-signal mode",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	for _, rel := range []string{
		"fixtures/observation/gsc-product-metric.json",
		"fixtures/observation/asc-product-metric.json",
		"fixtures/observation/social-distribution-feedback.json",
		"fixtures/observation/support-comment.json",
	} {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel))); err != nil {
			failures = append(failures, rel+" missing")
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"observation-batch-contract", Fail, "ObservationBatch generic seam lost: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"observation-batch-contract", Info, "ObservationBatch routes external payloads through existing meaning owners"}}
}

func checkOperatorObservationReviewProjectionContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"cmd/compound/orient_operator.go": {
			"ObservationReviews",
			"json:\"observation_reviews,omitempty\"",
			"buildOrientObservationReviews",
			"observation.ObservationReviewArea",
			"panel-verdicts",
			"candidates.CapabilityObs",
			"observation_reviews: candidate_reviews=%d panel_verdicts=%d total=%d",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorCarriesObservationReviewDebtProjection",
			"observe-batch",
			"--review-candidates",
			"observation_reviews: candidate_reviews=1 panel_verdicts=1 total=2",
			"observation candidates: compound reviews --area observation-candidates",
			"observation panel verdicts: compound reviews --area panel-verdicts",
		},
		"docs/architecture/status.md": {
			"Operator observation-review projection note",
			"`compound orient --operator --json.needs_me.observation_reviews`",
			"`observation-candidates`",
			"`panel-verdicts`",
			"`compound reviews --area observation-candidates`",
			"`compound reviews --area panel-verdicts`",
			"lens output evidence/candidate material",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"operator-observation-review-projection-contract", Fail, "operator orient lost observation review debt projection: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"operator-observation-review-projection-contract", Info, "operator orient consumes observation candidate and panel verdict review debt projection"}}
}

func checkProductObservationVerdictContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/product/observation_verdict.go": {
			"ObservationVerdictReviewArea",
			"product-observation-verdicts",
			"BuildObservationVerdictPlan",
			"panel.LoadVerdictArtifact",
			"ApplyObservationVerdictReview",
			"create_hypothesis_candidate",
			"record_product_observation_verdict_review",
			"writeHypothesisCandidate",
			"does not mutate ProductSignal ledger",
			"does not apply panel verdicts",
		},
		"internal/product/review_queue.go": {
			"ObservationVerdictReviewArea",
			"annotateObservationVerdictItem",
		},
		"internal/product/observation_verdict_test.go": {
			"TestObservationVerdictPlanCreatesReviewGatedProductHypothesisMaterial",
			"product_observation_verdict_review:",
			"ledger\", \"product-signals.jsonl",
		},
		"cmd/compound/main.go": {
			"observation-verdict-plan",
			"observation-verdict-review",
		},
		"cmd/compound/product_cli.go": {
			"BuildObservationVerdictPlan",
			"RecordObservationVerdictPlan",
			"ApplyObservationVerdictReview",
		},
		"cmd/compound/main_test.go": {
			"TestSourceObserveBatchCLIRecordsCandidateReviewMaterial",
			"observation-verdict-plan",
			"observation-verdict-review",
			"record_product_observation_verdict_review",
			"product-signals.jsonl",
		},
		"docs/architecture/status.md": {
			"Product observation-verdict bridge note",
			"`compound product observation-verdict-plan --verdict <panel-verdict.json>`",
			"`compound reviews --area product-observation-verdicts --json`",
			"`compound product observation-verdict-review <review.yaml>`",
			"parked product hypothesis candidate",
			"does not mutate ProductSignal",
		},
		"docs/architecture/contracts/README.md": {
			"ProductObservationVerdict",
			"`product-observation-verdicts`",
			"panel verdict evidence",
			"parked `product_hypothesis` candidate",
			"does not apply panel verdicts",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, string(b), phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"product-observation-verdict-contract", Fail, "product consumer for observation panel verdicts lost: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"product-observation-verdict-contract", Info, "product consumes observation panel verdict evidence through review-gated hypothesis material"}}
}

func checkSourceObservationVerdictContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/sourceobs/observation_verdict.go": {
			"SourceObservationVerdictReviewArea",
			"source-observation-verdicts",
			"BuildSourceObservationVerdictPlan",
			"panel.LoadVerdictArtifact",
			"ApplySourceObservationVerdictReview",
			"RecordNoPromote",
			"record_source_observation_verdict_review",
			"does not promote memory or source truth",
			"does not mutate ProductSignal ledger",
		},
		"internal/sourceobs/sourceobs_test.go": {
			"TestSourceObservationVerdictPlanRecordsNoPromoteEvidence",
			"TestSourceObservationVerdictPlanParksUnsupportedNoOwnerVerdict",
			"ledger\", \"claims.jsonl",
			"ledger\", \"product-signals.jsonl",
		},
		"cmd/compound": {
			"source observation-verdict-plan",
			"BuildSourceObservationVerdictPlan",
			"RecordSourceObservationVerdictPlan",
			"source observation-verdict-review",
			"ApplySourceObservationVerdictReview",
		},
		"internal/reviewqueue/reviewqueue.go": {
			"sourceobs.AnnotateObservationVerdictQueue",
		},
		"cmd/compound/main_test.go": {
			"TestSourceObservationVerdictCLIRecordsNoPromoteEvidence",
			"source-observation-verdicts",
			"record_source_observation_verdict_review",
			"product-signals.jsonl",
		},
		"docs/architecture/status.md": {
			"Source observation-verdict bridge note",
			"`compound source observation-verdict-plan --verdict <panel-verdict.json>`",
			"`compound reviews --area source-observation-verdicts --json`",
			"`compound source observation-verdict-review <review.yaml>`",
			"source-owned `NoPromoteDecision`",
			"unsupported no-owner parked evidence",
			"do not apply panel verdicts",
		},
		"docs/architecture/contracts/README.md": {
			"SourceObservationVerdict",
			"`source-observation-verdicts`",
			"source-owned `NoPromoteDecision`",
			"unsupported no-owner parked evidence",
			"does not apply panel verdicts",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"source-observation-verdict-contract", Fail, "source consumer for non-product observation panel verdicts lost: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"source-observation-verdict-contract", Info, "sourceobs consumes non-product observation panel verdict evidence through no-promote review material"}}
}

func checkPanelCapabilityFeedbackContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/panel/capability_feedback.go": {
			"PanelCapabilityFeedbackReviewArea",
			"capability-panel-feedback",
			"BuildPanelCapabilityFeedbackPlan",
			"RecordPanelCapabilityFeedbackPlan",
			"RecordPanelCapabilityFeedbackReview",
			"AnnotateCapabilityFeedbackQueue",
			"panelCapabilityFeedbackEdgeKind",
			"capgraph.EdValidatedBy",
			"capgraph.EdMissedWhen",
			"capgraph.EdFalseTriggeredBy",
			"lens:anti-pattern-operational-detector",
			"redirect context",
			"review artifact only; no graph mutation during plan",
			"feedback-review is the only command that may write CapabilityGraph feedback edges",
		},
		"fixtures/panel/lens-manifest.yaml": {
			"anti-pattern-operational-detector",
			"A valid detector has trigger, redirect behavior, consumer path, proof/guard, boundary, silence condition, and retirement/no-promote outcome.",
			"reviewed capability-feedback path can learn validated_by, missed_when, or false_triggered_by",
		},
		"internal/panel/lens_test.go": {
			"anti-pattern-detectors",
			"lens:anti-pattern-operational-detector",
			"want six included and ten non-memory excluded",
			"TestPanelCapabilityFeedbackReviewAppliesGraphEdgesOnlyAfterReview",
			"TestAntiPatternDetectorVerdictFeedsCapabilityFeedbackPlan",
			"TestAntiPatternDetectorFindingPreservesOperationalRedirectEvidence",
			"anti_pattern:trigger",
			"anti_pattern:retirement",
			"want missed_when detector candidate",
			"feedback plan must not mutate capability graph edges",
		},
		"cmd/compound/panel.go": {
			"feedback-plan --verdict",
			"feedback-review <review.yaml>",
			"BuildPanelCapabilityFeedbackPlan",
			"RecordPanelCapabilityFeedbackReview",
		},
		"internal/reviewqueue/reviewqueue.go": {
			"panel.AnnotateCapabilityFeedbackQueue",
		},
		"cmd/compound/panel_test.go": {
			"TestPanelFeedbackPlanReviewCLIFeedsCapabilityGraph",
			"PanelCapabilityFeedbackReviewArea",
			"EdgeKindCounts[capgraph.EdValidatedBy]",
			"lens:anti-pattern-operational-detector",
		},
		"cmd/compound/orient_operator.go": {
			"CapabilityFeedback",
			"BuildPanelCapabilityFeedbackImpact",
			"capability_feedback: pending=%d applied_reviews=%d applied_edges=%d",
			"Label: \"capability authority\"",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorCarriesReviewedCapabilityFeedbackImpact",
			"capability_feedback: pending=0 applied_reviews=1 applied_edges=1",
			"compound reviews --area capability-panel-feedback",
			"compound capability authority",
		},
		"docs/architecture/status.md": {
			"Panel capability-feedback note",
			"`compound panel feedback-plan --verdict <panel-verdict.json>`",
			"`compound reviews --area capability-panel-feedback --json`",
			"`compound panel feedback-review <review.yaml>`",
			"review-gated CapabilityGraph feedback",
			"`anti-pattern-operational-detector`",
		},
		"docs/architecture/contracts/README.md": {
			"Panel capability feedback note",
			"`compound panel feedback-plan --verdict <panel-verdict.json>`",
			"`capability-panel-feedback`",
			"`compound panel feedback-review <review.yaml>`",
			"does not auto-mutate CapabilityGraph",
			"`anti-pattern-operational-detector`",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"panel-capability-feedback-contract", Fail, "panel verdict capability feedback path lost: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"panel-capability-feedback-contract", Info, "panel verdicts feed reviewed CapabilityGraph feedback, anti-pattern detector findings, and operator orient exposes reviewed impact"}}
}

func checkCapabilityEvaluationContract(repoRoot string) []Finding {
	hasImportEvaluations := false
	hasEvidenceOnly := false
	hasEvaluatedBy := false
	hasNoSurface := false
	hasNoPromote := false
	hasReviewYAML := false
	hasReviewCommand := false
	hasEditedDecisionGate := false
	hasReviewedManifest := false
	hasReviewNoMutation := false
	hasReviewMutationClaim := false
	hasImportSkillsBoundary := false

	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "capability import-evaluations") {
			hasImportEvaluations = true
		}
		if strings.Contains(low, "evidence-only") && strings.Contains(low, "eval") {
			hasEvidenceOnly = true
		}
		if strings.Contains(low, "evaluated_by") {
			hasEvaluatedBy = true
		}
		if strings.Contains(low, "do not surface") ||
			strings.Contains(low, "never surface") ||
			strings.Contains(low, "records never retire/execute/surface") ||
			strings.Contains(low, "surface eval records as offers") {
			hasNoSurface = true
		}
		if strings.Contains(low, "sourceobservation") && (strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasNoPromote = true
		}
		if strings.Contains(low, "review yaml") || strings.Contains(low, "review_yaml") || strings.Contains(low, ".review.yaml") {
			hasReviewYAML = true
		}
		if strings.Contains(low, "capability evaluation-review") {
			hasReviewCommand = true
		}
		if strings.Contains(low, "edited decision") && strings.Contains(low, "rationale") && strings.Contains(low, "confidence") {
			hasEditedDecisionGate = true
		}
		if strings.Contains(low, "reviewed manifest") {
			hasReviewedManifest = true
		}
		if (strings.Contains(low, "evaluation-review") || strings.Contains(low, "review yaml")) &&
			(strings.Contains(low, "without mutating") || strings.Contains(low, "does not mutate") || strings.Contains(low, "no graph mutation")) {
			hasReviewNoMutation = true
		}
		if strings.Contains(low, "evaluation-review mutates") ||
			strings.Contains(low, "evaluation-review updates the graph") ||
			strings.Contains(low, "evaluation-review installs") {
			hasReviewMutationClaim = true
		}
		if strings.Contains(low, "only `import-skills` mutates") ||
			strings.Contains(low, "only import-skills mutates") ||
			(strings.Contains(low, "import-skills") && strings.Contains(low, "mutates the graph")) {
			hasImportSkillsBoundary = true
		}
		return nil
	})
	if !hasImportEvaluations || !hasEvidenceOnly || !hasEvaluatedBy || !hasNoSurface || !hasNoPromote {
		return []Finding{{"capability-evaluation-contract", Fail, "public docs do not describe capability import-evaluations as evidence-only evaluated_by/no-promote records that never surface as agent offers"}}
	}
	if !hasReviewYAML || !hasReviewCommand || !hasEditedDecisionGate || !hasReviewedManifest || !hasReviewNoMutation || !hasImportSkillsBoundary {
		return []Finding{{"capability-evaluation-contract", Fail, "public docs do not describe capability evaluation-review as gated review YAML producing reviewed manifests without graph mutation; import-skills must remain the graph mutation boundary"}}
	}
	if hasReviewMutationClaim {
		return []Finding{{"capability-evaluation-contract", Fail, "public docs claim capability evaluation-review mutates graph state; import-skills must remain the graph mutation boundary"}}
	}
	return []Finding{{"capability-evaluation-contract", Info, "capability evaluations stay evidence-only; review YAML gates activation before import-skills mutates graph offers"}}
}

func checkExternalRepoContextEvaluationContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/capgraph/external_repo_intake.go": {
			"ExternalRepoContextEvaluationStep",
			"EvaluationPlan",
			"externalRepoContextEvaluationPlan",
			"matchExternalRepoDecisionsByContext",
			"ExternalRepoMatchedEvidenceForTask",
			"external_repo_review_evidence",
			"existing-owner-fit",
			"capability-shape-fit",
			"risk-cost-fit",
			"Context-Aware Evaluation Plan",
			"silence:",
			"proof:",
			"retirement:",
		},
		"internal/capgraph/external_repo_review.go": {
			"externalRepoIntakeDecisionCard",
			"evaluation lens: ",
			"evaluation_step_count",
			"no clone/install/serve/auth/graph mutation/memory promotion",
			"externalRepoIntakeDecisionChoices",
		},
		"internal/capgraph/external_repo_evaluation_plan.go": {
			"contextPlan := externalRepoContextEvaluationPlan(report.Repo, match)",
			"externalRepoContextEvaluationSignals(contextPlan)",
			"context_evaluation_lenses",
			"context_evaluation_proofs",
			"context_evaluation_retirement",
			"context_evaluation_step_count",
		},
		"internal/capgraph/evaluation_plan.go": {
			"signalStringSlice(eval.Signals, \"context_evaluation_lenses\")",
			"context lens ",
			"silence condition reviewed",
			"context evaluation proof: ",
		},
		"internal/capgraph/evaluation_review_queue.go": {
			"signalStringSlice(plan.Signals, \"context_evaluation_lenses\")",
			"context_evaluation_silence",
			"context_evaluation_retirement",
			"indexedSignal",
			"evaluation lens: ",
		},
		"internal/capgraph/outbound_distribution_plan.go": {
			"OutboundDistributionPlanAuthorityRef",
			"BuildOutboundDistributionPlan",
			"evidence_only",
			"tier3_public_distribution_policy",
			"product_loop_target",
			"dry_run_receipt",
			"no_social_auth_or_posting",
			"VerdictPath",
			"Boundary",
		},
		"internal/capgraph/outbound_distribution_plan_test.go": {
			"TestOutboundDistributionPlanConsumesMatchedExternalRepoEvidence",
			"gitroomhq/postiz-app",
			"Panniantong/Agent-Reach",
			"cobusgreyling/loop-engineering",
			"no_distribution_evidence",
			"no_social_auth_or_posting",
			"TestOutboundDistributionVerdictPlanRecordsReviewArtifactsWithoutMutation",
			"ReviewRequiredMissingDecisionCard",
			"outbound-distribution-verdict-review",
		},
		"internal/capgraph/outbound_distribution_verdict.go": {
			"OutboundDistributionVerdictPlanSchemaVersion",
			"BuildOutboundDistributionVerdictPlan",
			"RecordOutboundDistributionVerdictPlan",
			"RecordOutboundDistributionVerdictReview",
			"AnnotateOutboundDistributionVerdictQueue",
			"dry_run_receipt_ref",
			"adopt_for_dry_run",
			"does not mutate ProductSignal ledgers",
			"no social auth/post/reply/spend/public side effect",
		},
		"internal/capgraph/outbound_distribution_dry_run.go": {
			"OutboundDistributionDryRunReceiptSchemaVersion",
			"BuildOutboundDistributionDryRunReceipt",
			"RecordOutboundDistributionDryRunReceipt",
			"RecordOutboundDistributionDryRunReview",
			"AnnotateOutboundDistributionDryRunQueue",
			"public side effects/auth/external requests/posting/spend",
			"does not mutate product signals",
			"accept_as_product_loop_evidence",
		},
		"internal/capgraph/outbound_distribution_dry_run_test.go": {
			"TestOutboundDistributionDryRunReceiptConsumesAdoptedVerdictWithoutProductMutation",
			"TestOutboundDistributionDryRunReceiptRejectsPublicSideEffects",
			"public_side_effects",
			"ReviewRequired",
		},
		"internal/product/outbound_dry_run.go": {
			"OutboundDryRunEvidencePlanSchemaVersion",
			"BuildOutboundDryRunEvidencePlan",
			"RecordOutboundDryRunEvidencePlan",
			"ApplyOutboundDryRunEvidenceReview",
			"AnnotateOutboundDryRunEvidenceQueue",
			"does not mutate ProductSignal ledger",
			"does not publish or authenticate social accounts",
			"create_hypothesis_candidate",
		},
		"internal/capgraph/external_repo_intake_test.go": {
			"item.EvaluationPlan",
			"hasExternalRepoEvaluationStep",
			"TestExternalRepoIntakeReportMatchesTaskContextEvidence",
			"harness-fit",
			"DecisionCard.Meta",
			"evaluation lens: harness-fit",
		},
		"cmd/compound/main.go": {
			"ExternalRepoMatchedEvidenceForTask",
		},
		"cmd/compound/product_cli.go": {
			"outbound-dry-run-plan",
			"outbound-dry-run-review",
		},
		"cmd/compound/capability.go": {
			"report.MatchedEvidence",
			"outbound-distribution-plan",
			"outbound-distribution-verdict-plan",
			"outbound-distribution-verdict-review",
			"outbound-distribution-dry-run-receipt",
			"outbound-distribution-dry-run-review",
			"BuildOutboundDistributionPlan",
			"BuildOutboundDistributionVerdictPlan",
			"report.RequiredGates",
		},
		"internal/reviewqueue/reviewqueue.go": {
			"AnnotateOutboundDistributionVerdictQueue",
			"AnnotateOutboundDistributionDryRunQueue",
			"AnnotateOutboundDryRunEvidenceQueue",
		},
		"cmd/compound/main_test.go": {
			"TestCapabilitySurfaceReportCarriesExternalRepoEvidenceWithoutOfferingIt",
			"matched external repo evidence",
			"must not become an offer",
			"TestCapabilityOutboundDistributionPlanJSON",
			"TestCapabilityOutboundDistributionVerdictPlanAndReviewCLI",
			"TestCapabilityOutboundDistributionDryRunReceiptCLI",
			"# product outbound-dry-run-plan",
			"# product outbound-dry-run-review",
			"product.OutboundDryRunEvidenceReviewArea",
			"product-signals.jsonl",
			"tier3_public_distribution_policy",
			"product_loop_target",
			"auto-post",
			"ReviewRequiredMissingDecisionCard",
		},
		"cmd/compound/orient_operator.go": {
			"AdaptationIntake",
			"buildOrientAdaptationIntake",
			"ExternalRepoIntakeReviewArea",
			"capability-evaluation-intake",
			"adaptation_intake",
			"already reviewed/recorded",
		},
		"cmd/compound/orient_operator_test.go": {
			"TestOrientOperatorCarriesAdaptationIntakeProjection",
			"external_repos=1 open=0 reviewed=1 capability_evaluations=1",
			"already reviewed/recorded",
			"external repo intake",
			"capability evaluation intake",
		},
		"docs/architecture/status.md": {
			"External repo context-evaluation note",
			"`compound reviews --area external-repo-intake --json`",
			"`compound orient --operator --json.needs_me.adaptation_intake`",
			"context-aware evaluation plan",
			"trigger/action/boundary/silence/proof/retirement",
			"capability-evaluation plans and decision cards",
			"`context lens ...` review gates",
			"`evaluation lens: ...` reasons",
			"Task-context queries can include matched external repo evidence",
			"capability surface `matched_evidence`",
			"imported `needs_review` graph nodes stay non-offers",
			"`compound capability outbound-distribution-plan --task`",
			"evidence-only outbound distribution review",
			"Tier-3/product-loop gates",
			"no social auth or posting",
			"`compound capability outbound-distribution-verdict-plan`",
			"`compound reviews --area outbound-distribution-verdicts --json`",
			"dry-run receipt requirement",
			"`compound capability outbound-distribution-dry-run-receipt`",
			"`compound reviews --area outbound-distribution-dry-runs --json`",
			"`compound product outbound-dry-run-plan`",
			"`compound reviews --area product-outbound-dry-runs --json`",
			"`compound product outbound-dry-run-review`",
			"no public side effects",
			"does not mutate product signals",
			"does not mutate ProductSignal",
			"no clone/install/serve/auth/graph mutation/memory promotion",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"external-repo-context-evaluation-contract", Fail, "external repo intake lost context-aware evaluation plan surface: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"external-repo-context-evaluation-contract", Info, "external repo intake review cards expose context-aware evaluation plans before capability adoption"}}
}

func checkExternalRepoIntakeStateProjectionContract(repoRoot string) []Finding {
	requiredByFile := map[string][]string{
		"internal/capgraph/external_repo_intake.go": {
			"json:\"open_review_count\"",
			"annotateExternalRepoDecisionState",
			"countPendingExternalRepoOwnerReviews",
			"ExternalRepoCapabilityEvaluationStatus",
			"ExternalRepoCapabilityGraphStatus",
		},
		"internal/capgraph/external_repo_intake_test.go": {
			"default report should keep raw decision count but clear owner-review debt",
			"CapabilityEvaluationStatus != \"pending\"",
			"CapabilityGraphStatus != \"blocked_on_capability_evaluation\"",
		},
		"cmd/compound/capability.go": {
			"keep_needs_review_decision",
			"open_owner_reviews",
			"high_value: %s decision=%s lane=%s owner_review=%s capability_evaluation=%s graph=%s owner=%s",
		},
		"cmd/compound/main_test.go": {
			"default external-intake text should distinguish raw decision count from open owner-review debt",
			"owner_review=recorded capability_evaluation=pending graph=blocked_on_capability_evaluation",
		},
		"docs/architecture/status.md": {
			"Default `capability external-intake` separates raw `keep_needs_review` classification from actual `open_owner_reviews`",
			"high-value rows show owner-review, capability-evaluation, and graph status",
			"instead of calling recorded/imported rows review-next work",
		},
	}
	var failures []string
	for rel, phrases := range requiredByFile {
		text, err := readContractText(repoRoot, rel)
		if err != nil {
			failures = append(failures, rel+" unreadable: "+err.Error())
			continue
		}
		failures = append(failures, missingPhrases(rel, text, phrases)...)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return []Finding{{"external-repo-intake-state-projection-contract", Fail, "external repo default projection can confuse classification with open review debt: " + strings.Join(failures, "; ")}}
	}
	return []Finding{{"external-repo-intake-state-projection-contract", Info, "external repo default projection separates raw classification from open owner-review debt and shows evaluation state"}}
}

func checkConversationMiningContract(repoRoot string) []Finding {
	hasImport := false
	hasSourceNoPromote := false
	hasLossiness := false
	hasImprovementPlan := false
	hasNoDream := false
	hasNoMemoryPromotion := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "import-conversation-mining") {
			hasImport = true
		}
		if strings.Contains(low, "sourceobservation") && (strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasSourceNoPromote = true
		}
		if strings.Contains(low, "lossiness") || strings.Contains(low, "lossy corpus") {
			hasLossiness = true
		}
		if strings.Contains(low, "distiller improvement-plan") {
			hasImprovementPlan = true
		}
		if strings.Contains(low, "no dream execution") || strings.Contains(low, "does not execute dream") || (strings.Contains(low, "dream execution") && strings.Contains(low, "gated")) {
			hasNoDream = true
		}
		if strings.Contains(low, "no memory promotion") || strings.Contains(low, "does not promote memory") || strings.Contains(low, "without memory promotion") {
			hasNoMemoryPromotion = true
		}
		return nil
	})
	if !hasImport || !hasSourceNoPromote || !hasLossiness || !hasImprovementPlan || !hasNoDream || !hasNoMemoryPromotion {
		return []Finding{{"conversation-mining-contract", Fail, "public docs do not describe distiller import-conversation-mining as lossiness-aware SourceObservation/no-promote evidence with reviewed improvement-plan next step, no Dream execution, and no memory promotion"}}
	}
	return []Finding{{"conversation-mining-contract", Info, "conversation mining stays lossiness-aware source evidence, not Dream or memory authority"}}
}

func checkConversationCoverageContract(repoRoot string) []Finding {
	hasImport := false
	hasSourceNoPromote := false
	hasNoRowImport := false
	hasImprovementPlan := false
	hasNoDream := false
	hasNoMemoryPromotion := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "import-conversation-coverage") {
			hasImport = true
		}
		if strings.Contains(low, "sourceobservation") && (strings.Contains(low, "nopromotedecision") || strings.Contains(low, "no-promote")) {
			hasSourceNoPromote = true
		}
		if strings.Contains(low, "no row import") || strings.Contains(low, "does not import row") || strings.Contains(low, "summary-only") {
			hasNoRowImport = true
		}
		if strings.Contains(low, "distiller improvement-plan") {
			hasImprovementPlan = true
		}
		if strings.Contains(low, "no dream execution") || strings.Contains(low, "does not execute dream") || (strings.Contains(low, "dream execution") && strings.Contains(low, "no")) {
			hasNoDream = true
		}
		if strings.Contains(low, "no memory promotion") || strings.Contains(low, "does not promote memory") || strings.Contains(low, "without memory promotion") {
			hasNoMemoryPromotion = true
		}
		return nil
	})
	if !hasImport || !hasSourceNoPromote || !hasNoRowImport || !hasImprovementPlan || !hasNoDream || !hasNoMemoryPromotion {
		return []Finding{{"conversation-coverage-contract", Fail, "public docs do not describe distiller import-conversation-coverage as summary-only SourceObservation/no-promote evidence with reviewed improvement-plan next step, no row import, no Dream execution, and no memory promotion"}}
	}
	return []Finding{{"conversation-coverage-contract", Info, "conversation coverage stays summary-only source-quality evidence, not row import, Dream, or memory authority"}}
}

func checkDecisionSourceAuthorityContract(repoRoot string) []Finding {
	hasGenericSourceImport := false
	hasSourceKindNote := false
	hasPolicyDir := false
	hasAuthorityCommand := false
	hasSourceAuthorityLog := false
	hasLifecycleOwner := false
	hasNoCandidateOnlyParser := false
	hasRuntimeDedupe := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "decisions import --source") || strings.Contains(low, "import --source <") {
			hasGenericSourceImport = true
		}
		if strings.Contains(low, "--source-dir <dir> --source-kind note") || strings.Contains(low, "--source-kind note") {
			hasSourceKindNote = true
		}
		if strings.Contains(low, "--policy-dir <dir>") {
			hasPolicyDir = true
		}
		if strings.Contains(low, "decisions authority") {
			hasAuthorityCommand = true
		}
		if strings.Contains(low, "decision-source-authority.jsonl") || strings.Contains(low, "source-authority coverage") {
			hasSourceAuthorityLog = true
		}
		if strings.Contains(low, "decisionlifecycle") {
			hasLifecycleOwner = true
		}
		if strings.Contains(low, "not a candidate-only parser") || strings.Contains(low, "not candidate-only") {
			hasNoCandidateOnlyParser = true
		}
		if strings.Contains(low, "dedupe") && strings.Contains(low, "decision://") && (strings.Contains(low, "contract compile") || strings.Contains(low, "orient")) {
			hasRuntimeDedupe = true
		}
		return nil
	})
	if !hasGenericSourceImport || !hasSourceKindNote || !hasPolicyDir || !hasAuthorityCommand || !hasSourceAuthorityLog || !hasLifecycleOwner || !hasNoCandidateOnlyParser || !hasRuntimeDedupe {
		return []Finding{{"decision-source-authority-contract", Fail, "public docs do not describe generic DecisionLifecycle source import, policy-dir import, source-authority coverage, non-candidate-only adapter, and runtime decision:// dedupe"}}
	}
	return []Finding{{"decision-source-authority-contract", Info, "decision source authority covers generic note/policy imports, source coverage, and runtime decision ref dedupe"}}
}

func checkDistillerImprovementReviewContract(repoRoot string) []Finding {
	hasPlan := false
	hasReview := false
	hasReviewYAML := false
	hasEvalProof := false
	hasEvalRecord := false
	hasRecordedProof := false
	hasNoMutation := false
	docs := filepath.Join(repoRoot, "docs")
	_ = filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "distiller improvement-plan") {
			hasPlan = true
		}
		if strings.Contains(low, "distiller improvement-review") {
			hasReview = true
		}
		if strings.Contains(low, ".review.yaml") {
			hasReviewYAML = true
		}
		if strings.Contains(low, "eval_proof_refs") {
			hasEvalProof = true
		}
		if strings.Contains(low, "distiller eval") && strings.Contains(low, "--record") {
			hasEvalRecord = true
		}
		if strings.Contains(low, "recorded passing eval proof") || strings.Contains(low, "recorded passing eval proofs") {
			hasRecordedProof = true
		}
		if strings.Contains(low, "does not mutate distiller policy") &&
			strings.Contains(low, "eval corpus") &&
			(strings.Contains(low, "memory truth") || strings.Contains(low, "promote memory")) {
			hasNoMutation = true
		}
		return nil
	})
	if !hasPlan || !hasReview || !hasReviewYAML || !hasEvalProof || !hasEvalRecord || !hasRecordedProof || !hasNoMutation {
		return []Finding{{"distiller-improvement-review-contract", Fail, "public docs do not describe distiller improvement-plan/review as gated review YAML with recorded eval_proof_refs and no policy/eval-corpus/memory mutation"}}
	}
	return []Finding{{"distiller-improvement-review-contract", Info, "distiller improvement review is gated by recorded eval proof, not policy/eval-corpus/memory mutation"}}
}

type epistemicsDoctrineRow struct {
	N                  string
	ImplementingObject string
	ProofCommand       string
}

func checkEpistemicsDoctrine(repoRoot string) []Finding {
	path := filepath.Join(repoRoot, "docs", "architecture", "epistemics.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return []Finding{{"epistemics-doctrine", Fail, "docs/architecture/epistemics.md missing or unreadable: " + err.Error()}}
	}
	rows := parseEpistemicsDoctrineRows(string(b))
	if len(rows) < 7 {
		return []Finding{{"epistemics-doctrine", Fail, "docs/architecture/epistemics.md has " + itoa(len(rows)) + " doctrine row(s); want at least 7 machine-parseable rows"}}
	}
	registeredCommands, err := compoundRegisteredCommands(repoRoot)
	if err != nil {
		return []Finding{{"epistemics-doctrine", Fail, "could not inspect cmd/compound/command_registry.go commands: " + err.Error()}}
	}
	var problems []string
	for _, row := range rows {
		for _, object := range splitEpistemicsObjects(row.ImplementingObject) {
			if !strings.HasPrefix(object, "internal/") {
				problems = append(problems, "row "+row.N+" implementing_object "+strconv(object)+" must name internal/<package>")
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, object)); err != nil {
				problems = append(problems, "row "+row.N+" implementing_object "+strconv(object)+" not found")
			}
		}
		verb := epistemicsProofVerb(row.ProofCommand)
		if verb == "" {
			problems = append(problems, "row "+row.N+" proof_command "+strconv(row.ProofCommand)+" has no compound verb")
			continue
		}
		if !registeredCommands[verb] {
			problems = append(problems, "row "+row.N+" proof_command verb "+strconv(verb)+" is absent from cmd/compound/command_registry.go")
		}
	}
	if len(problems) > 0 {
		return []Finding{{"epistemics-doctrine", Fail, strings.Join(problems, "; ")}}
	}
	return []Finding{{"epistemics-doctrine", Info, "docs/architecture/epistemics.md maps " + itoa(len(rows)) + " doctrine rows to existing packages and compound commands"}}
}

func parseEpistemicsDoctrineRows(text string) []epistemicsDoctrineRow {
	var rows []epistemicsDoctrineRow
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		cells := splitMarkdownTableRow(line)
		if len(cells) == 0 {
			if inTable {
				break
			}
			continue
		}
		if !inTable {
			if len(cells) >= 4 && strings.EqualFold(cells[0], "n") && strings.EqualFold(cells[1], "principle") && strings.EqualFold(cells[2], "implementing_object") && strings.EqualFold(cells[3], "proof_command") {
				inTable = true
			}
			continue
		}
		if isMarkdownSeparatorRow(cells) {
			continue
		}
		if len(cells) < 4 || !isDigits(cells[0]) {
			continue
		}
		rows = append(rows, epistemicsDoctrineRow{
			N:                  cells[0],
			ImplementingObject: cells[2],
			ProofCommand:       cells[3],
		})
	}
	return rows
}

func splitMarkdownTableRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return nil
	}
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, cleanEpistemicsCell(part))
	}
	return out
}

func cleanEpistemicsCell(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`")
	return strings.TrimSpace(value)
}

func isMarkdownSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			return false
		}
		for _, r := range cell {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

func splitEpistemicsObjects(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = cleanEpistemicsCell(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func epistemicsProofVerb(command string) string {
	parts := strings.Fields(cleanEpistemicsCell(command))
	if len(parts) < 2 || parts[0] != "compound" {
		return ""
	}
	return parts[1]
}

func compoundRegisteredCommands(repoRoot string) (map[string]bool, error) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "command_registry.go"))
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`(?m)^\s*"([^"]+)"\s*:`)
	out := map[string]bool{}
	for _, match := range re.FindAllStringSubmatch(string(b), -1) {
		if len(match) == 2 {
			out[match[1]] = true
		}
	}
	return out, nil
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkCmdCompoundBudget ratchets the cmd/compound MONOLITH on two independent metrics:
// non-test .go file count and non-test .go physical LOC. *_test.go does not count (tests are
// the spine, not the debt). `cmd/compound-*` siblings (compound-install-verify etc.) are NOT
// measured — those are separate surfaces, each free to grow on its own budget. The check reads
// cmd/compound DIRECTLY and ignores `internal/` so the two-stack pressure (minting a new organ
// costs three coupled gates; appending to cmd/compound costs zero) cannot bleed past this gate.
// `--strict` flips any over-budget metric to FAIL. Headroom on BOTH metrics is reported in the
// Info message so a maintainer can see exactly how much room is left before the ratchet fires.
func checkCmdCompoundBudget(repoRoot string, strict bool) []Finding {
	if repoRoot == "" {
		return []Finding{{"cmd-compound-budget", Warn, "no repoRoot provided to checkCmdCompoundBudget — skipping the cmd/compound monolith budget check"}}
	}
	compoundDir := filepath.Join(repoRoot, "cmd", "compound")
	entries, err := os.ReadDir(compoundDir)
	if err != nil {
		return []Finding{{"cmd-compound-budget", Warn, "could not read cmd/compound: " + err.Error()}}
	}
	var fileCount, loc int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fileCount++
		b, err := os.ReadFile(filepath.Join(compoundDir, name))
		if err != nil {
			return []Finding{{"cmd-compound-budget", Warn, "could not read cmd/compound/" + name + ": " + err.Error()}}
		}
		loc += strings.Count(string(b), "\n")
		if len(b) > 0 && b[len(b)-1] != '\n' {
			loc++
		}
	}
	var f []Finding
	// each metric is checked INDEPENDENTLY: a gate that only watched files would let a single
	// file bloat unchecked, and a gate that only watched LOC would let file count fragment
	// unchecked. Either exceeds → FAIL (with `--strict`). The Info line reports both actuals
	// and both budgets plus headroom so a maintainer sees the whole picture at once.
	sevFiles := Info
	msgFiles := "cmd/compound/ has " + itoa(fileCount) + "/" + itoa(MaxNonTestCompoundFiles) + " non-test .go files (headroom " + itoa(MaxNonTestCompoundFiles-fileCount) + ")"
	if fileCount > MaxNonTestCompoundFiles {
		sevFiles = Fail
		if !strict {
			sevFiles = Warn
		}
		msgFiles = "cmd/compound/ has " + itoa(fileCount) + " non-test .go files — over budget " + itoa(MaxNonTestCompoundFiles) + " by " + itoa(fileCount-MaxNonTestCompoundFiles) + "; the budget only ratchets DOWN, so new code lands by `git mv cmd/compound/foo.go internal/<organ>/`, never by raising the ceiling"
	}
	sevLOC := Info
	msgLOC := "cmd/compound/ has " + itoa(loc) + "/" + itoa(MaxNonTestCompoundLOC) + " non-test .go LOC (headroom " + itoa(MaxNonTestCompoundLOC-loc) + ")"
	if loc > MaxNonTestCompoundLOC {
		sevLOC = Fail
		if !strict {
			sevLOC = Warn
		}
		msgLOC = "cmd/compound/ has " + itoa(loc) + " non-test .go LOC — over budget " + itoa(MaxNonTestCompoundLOC) + " by " + itoa(loc-MaxNonTestCompoundLOC) + "; the budget only ratchets DOWN, so new code lands by `git mv cmd/compound/foo.go internal/<organ>/`, never by raising the ceiling"
	}
	f = append(f, Finding{"cmd-compound-budget", sevFiles, msgFiles})
	f = append(f, Finding{"cmd-compound-budget", sevLOC, msgLOC})
	return f
}

func convergenceDebt(strict bool) []Finding {
	var pending []string
	for pkg, c := range Classification {
		if c == ConvergePending {
			pending = append(pending, pkg)
		}
	}
	sort.Strings(pending)
	if len(pending) == 0 {
		return []Finding{{"convergence", Info, "memory object family converged — no Gen-1 packages pending; budget is 0"}}
	}
	// the ratchet: in strict mode a debt over budget is a FAILURE (fragmentation grew).
	sev := Info
	msg := itoa(len(pending)) + "/" + itoa(ConvergePendingBudget) + " package(s) converge→retire (BLOCKED on parity + tests): " + strings.Join(pending, ", ")
	if len(pending) > ConvergePendingBudget {
		sev = Fail
		msg = "convergence debt GREW past budget " + itoa(ConvergePendingBudget) + " → " + itoa(len(pending)) + " (new fragmentation: " + strings.Join(pending, ", ") + ")"
	} else if strict && len(pending) < ConvergePendingBudget {
		// a chance to ratchet down — surface it loudly so the budget tracks reality.
		sev = Warn
		msg = "debt is " + itoa(len(pending)) + " but budget is " + itoa(ConvergePendingBudget) + " — lower ConvergePendingBudget to lock the gain"
	}
	return []Finding{{"convergence", sev, msg}}
}

// --- tiny local helpers (no fmt dependency for hot strings) ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func strconv(s string) string { return "\"" + s + "\"" }
