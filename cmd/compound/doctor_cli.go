package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/doctor"
	"compound/internal/doctor/exitcode"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/landlease"
	"compound/internal/lanes"
	"compound/internal/operatornotice"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
	"compound/internal/work/executewire"
)

// cmdDoctor is the single legible operator health surface. The newest organs each expose real state,
// but scattered behind their own subcommands or raw JSON — so a first-time operator cannot answer
// "did my install work / who holds the landing lease / is anything waiting for me?" from one place.
// doctor composes the states that already exist — per-harness hook install/drift/absent, the
// land-lease (free/held), pending operational findings, and the placement guard mode — into one
// read-only report. It writes nothing and makes no provider call.
func cmdDoctor(args []string) {
	if code := runDoctor(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

type doctorHook struct {
	Harness    string `json:"harness"`
	Status     string `json:"status"` // installed|ok|drift|absent
	ConfigPath string `json:"config_path"`
}

// doctorPermissionPosture is one harness's read-only permission-authority
// drift row (packet work_0fc6126d8fa2 -- the operator-legibility half: a
// silent posture reversion must surface here even if nobody ran
// `compound harness drift --emit-divergence`). It calls harness.PermissionDrift
// only; doctor's "writes nothing" contract holds -- emission of an
// observation batch lives exclusively on the drift verb's --emit-divergence
// flag, never here.
type doctorPermissionPosture struct {
	Harness    string   `json:"harness"`
	Status     string   `json:"status"` // clean|drift|unknown
	TargetPath string   `json:"target_path,omitempty"`
	Details    []string `json:"details,omitempty"`
}

type doctorReport struct {
	Home                   string                       `json:"home"`
	Hooks                  []doctorHook                 `json:"hooks"`
	HookHint               string                       `json:"hook_hint,omitempty"`
	LeaseHeld              bool                         `json:"lease_held"`
	LeaseHolder            string                       `json:"lease_holder,omitempty"`
	LeaseExpiresAt         string                       `json:"lease_expires_at,omitempty"`
	LeaseStealReason       string                       `json:"lease_steal_reason,omitempty"`
	LeaseLayout            string                       `json:"lease_layout,omitempty"`
	ProofLeaseHeld         bool                         `json:"proof_lease_held"`
	ProofCapacities        []landlease.ProofClassStatus `json:"proof_capacities,omitempty"`
	ProofLeaseHolder       string                       `json:"proof_lease_holder,omitempty"`
	ProofLeaseExpiresAt    string                       `json:"proof_lease_expires_at,omitempty"`
	LandingLeaseStatusHint string                       `json:"landing_lease_status_hint,omitempty"`
	LeaseStatusError       string                       `json:"lease_status_error,omitempty"`
	FindingsTotal          int                          `json:"findings_total"`
	FindingsLive           int                          `json:"findings_live"`
	FindingsSuperseded     int                          `json:"findings_superseded"`
	PlacementMode          string                       `json:"placement_guard_mode"`
	PlacementNote          string                       `json:"placement_note,omitempty"`

	// UnattendedExecute — one row per lane the route plan names for
	// work.continue.unattended (R3A slice 2 deliverable 6). Each row
	// carries the route gate role, capacity reading + age, and grant
	// presence; the matching plain-text section is rendered by
	// doctor.PrintUnattendedExecute (under internal/doctor/).
	UnattendedExecute []doctor.UnattendedExecuteLane `json:"unattended_execute,omitempty"`

	// Placement source freshness — for each imported source view, whether the bytes
	// Compound ENFORCES still match the bytes that view renders today.
	PlacementSources []doctorPlacementSource `json:"placement_sources,omitempty"`

	// Retention — what is accumulating on disk, and whether any class owns it.
	Retention *policy.RetentionScanReport `json:"retention,omitempty"`

	// Permission posture: one row per configured harness-permission authority
	// (config/harness-permissions/*.yaml), read-only via harness.PermissionDrift.
	PermissionPosture []doctorPermissionPosture `json:"permission_posture,omitempty"`

	// C3: RunAttempt crash terminality — how many attempts are stuck
	// (pending/running) and how old the oldest is. Doctor surfaces them as
	// a finding until swept (`compound run-attempt sweep`).
	RunAttemptsStuck       int    `json:"run_attempts_stuck"`
	RunAttemptsStuckOldest string `json:"run_attempts_stuck_oldest,omitempty"`
	// RunAttemptsStuckPastTTL is the subset of RunAttemptsStuck whose last
	// activity (LastHeartbeatAt when present, otherwise CreatedAt) is older
	// than the sweep's DefaultSweepThreshold (work_195771b63a58 / DEC-0027
	// build step 1). The sweep guarantees this count goes to zero after a
	// successful pass; a non-zero value here means the sweep either has
	// never run since the attempts accumulated, or it ran and refused to
	// terminalize one (e.g. a write failure landed in Skipped). Either way,
	// the operator must look. Doctor reads this off the on-disk run_attempts
	// JSON in the same pass it counts RunAttemptsStuck — no extra sweep
	// invocation, no double-walk of the directory.
	RunAttemptsStuckPastTTL       int    `json:"run_attempts_stuck_past_ttl"`
	RunAttemptsStuckPastTTLOldest string `json:"run_attempts_stuck_past_ttl_oldest,omitempty"`
	// RunAttemptsSweptAmbiguous counts attempts the sweep terminalized as
	// abandoned with sweep_classification="ambiguous" (had a transcript,
	// diff, test, or observed_external origin before dying). The sweep
	// itself cannot prove those side effects are clean — the operator
	// MUST reconcile. Surfaced here so a non-zero value shows up
	// alongside RunAttemptsStuckPastTTL in the same report.
	RunAttemptsSweptAmbiguous       int      `json:"run_attempts_swept_ambiguous"`
	RunAttemptsSweptAmbiguousIDs    []string `json:"run_attempts_swept_ambiguous_ids,omitempty"`
	RunAttemptsSweptAmbiguousOldest string   `json:"run_attempts_swept_ambiguous_oldest,omitempty"`
	// Unreadable RunAttempts are valid ratt_ records whose main JSON or
	// launch sidecars failed validation. The doctor keeps this separate from
	// stuck/terminal counts because their lifecycle state cannot be trusted.
	RunAttemptsUnreadable    int      `json:"run_attempts_unreadable"`
	RunAttemptsUnreadableIDs []string `json:"run_attempts_unreadable_ids,omitempty"`
	RunAttemptsReadError     bool     `json:"run_attempts_read_error,omitempty"`

	// Hook enforcement: for each binary the installed hooks would invoke, whether it really
	// gates. Every other hook signal above compares CONFIG bytes and stays green when the file
	// at the configured path is a no-op — see doctor_hook_enforcement.go.
	HookEnforcement        []doctorHookEnforcement `json:"hook_enforcement,omitempty"`
	HookEnforcementFailing int                     `json:"hook_enforcement_failing"`

	// Worktree locks over delegate workspaces, and how many of them are held
	// by a delegate that has already gone terminal — a lock protecting nobody
	// that still refuses `git worktree remove`.
	WorktreeLocks      []work.DelegateWorktreeLockRow `json:"worktree_locks,omitempty"`
	WorktreeLocksStale int                            `json:"worktree_locks_stale"`

	// Surfacing coverage: rows = agent lanes/harnesses, columns = communication surfaces
	// (guard hook · orient · memory injection · peer visibility). Read-only, derived from
	// state that already exists; it answers "do all harnesses actually see compound?".
	Surfacing          []surfacingRow     `json:"surfacing"`
	SurfacingNote      string             `json:"surfacing_note,omitempty"`
	SurfacingInjection surfacingInjection `json:"surfacing_injection"`

	// Projection freshness: which persisted projections predate the running binary.
	// Answers "is a fix I just installed actually being served?" — see
	// home.ProjectionFreshnessReport and find_b27a3de99682.
	Projections        []home.ProjectionFreshness `json:"projections,omitempty"`
	ProjectionsStale   int                        `json:"projections_stale"`
	ProjectionsUndated int                        `json:"projections_undated"`
	ProjectionsOldest  string                     `json:"projections_oldest,omitempty"`
	ProjectionsNote    string                     `json:"projections_note,omitempty"`

	// BinaryStaleness is installed binary as new as repo's target tip. Named signal:
	// installed SHA + target tip + relation, never bare boolean. Closes 2026-08-17
	// gap where doctor said all-clear while installed five commits behind main.
	BinaryStaleness doctor.BinaryStalenessReport `json:"binary_staleness"`

	// HarnessVersionDrift surfaces the named signal "harness_version_drift":
	// installed binary version vs the harness row's ExpectedVersion. One row
	// per registered harness, status vocabulary match|drift|unpinned|unobserved.
	HarnessVersionDrift doctor.HarnessVersionDriftReport `json:"harness_version_drift"`

	// Doctor exit-code contract (packet work_3da5d3c76707): Problems
	// is the plain-language list of actionable problems the doctor
	// found, and Warnings is the list of info-only conditions the
	// doctor refuses to fail on (a hook config drift whose binary
	// still enforces, a harness version that's only "drift" because
	// the operator hasn't pinned one, etc.). Both are computed from
	// the existing named counters — nothing the doctor checks changes,
	// only how truthfully it reports and exits. JSON consumers can
	// branch on len(Problems) or len(Warnings) without parsing prose.
	Problems             []string                        `json:"problems"`
	Warnings             []string                        `json:"warnings"`
	BindingFreshness     contract.BindingFreshnessReport `json:"binding_freshness"` // R3B 2026-09-04: hook-manifest vs installed binary (compute+render in contract).
	BindingFreshnessNote string                          `json:"binding_freshness_note,omitempty"`
}

// surfacingRow is one lane/harness × the communication surfaces it should see. Cell vocabulary
// is deliberately small: guard is wired|drift|absent|unknown, orient is wired|never, peers is
// wired|absent (evidence-read from the latest orient-recorded peers-bearing ContextPack for the
// harness). BelowExpectation names the expected-but-unwired columns (the trailing "!" in the text
// render is derived from it) so text and JSON never disagree about what is short; peers is
// informational and never marked below-expectation.
type surfacingRow struct {
	Lane             string   `json:"lane"`     // lane node name, or harness for fallback rows
	Harness          string   `json:"harness"`  // aliased harness the guard/orient columns join on
	Expected         bool     `json:"expected"` // harness-aliased lane => guard + orient expected (v1 derived)
	Guard            string   `json:"guard"`
	Orient           string   `json:"orient"`
	OrientAge        string   `json:"orient_age,omitempty"`
	Injection        string   `json:"injection"`
	Peers            string   `json:"peers"`
	BelowExpectation []string `json:"below_expectation,omitempty"`
}

// surfacingInjection is the memory-injection column's evidence. It is GLOBAL by construction:
// the injection-observation report attributes sessions to work ids, not to harnesses, so an
// honest matrix shows the same numbers on every row rather than faking a per-lane split. Scope
// is always "global" and Note says so; a future per-lane consumption signal would refine this.
//
// NO CONSUMPTION RATE IS COMPUTABLE FROM RECEIPTS, and saying so is this struct's main job.
//
// It is tempting to divide ConsumedEvents by ServedEvents, or to split served events into
// "measured" and "unmeasured" by Receipt.Skills.UsedDeclared and divide by the measured half. Both
// are wrong, for the same reason: UsedDeclared has TWO writers with incompatible meanings and the
// Receipt does not record which one wrote it.
//
//	cmd/compound/work_cli.go  UsedDeclared: has("--used")   — an operator or agent typed a flag
//	internal/hook/hook.go     UsedDeclared: usedDeclared    — the session-end detector actually ran
//
// A detector-produced receipt and a CLI-close receipt are structurally identical: same fields, same
// hardcoded Producer{Kind:"cli"} (work.go's newHeaderWithLane stamps it on BOTH paths). Verified
// 2026-08-11 by field-by-field comparison on the real home. So a "measured" denominator built on
// UsedDeclared silently counts CLI closes that never measured anything — on the real home that was
// 96 of 97 declared sessions, contributing 420 served events and zero claim consumption, next to
// the single session where the detector actually ran and consumed 5 of 5.
//
// What IS honest from receipt data: how many claim events were served, how many were observed to be
// referenced, and across how many sessions each happened. Everything else is unknown — and
// internal/hook/consumption.go's own invariant ("UNDER-DETECTION IS SAFE: a missed consumption
// becomes UNKNOWN, never ignored") means unknown must not be rendered as zero. Recording the
// declaration's provenance on the Receipt is what would make a rate computable; until then this
// surface reports counts and says why it reports no ratio.
type surfacingInjection struct {
	Scope string `json:"scope"` // always "global" in v1

	// ServedEvents and ConsumedEvents are raw counts, never a numerator/denominator pair.
	ServedEvents   int `json:"served_events"`
	ConsumedEvents int `json:"consumed_events"`

	SessionsServed          int `json:"sessions_served"`
	SessionsWithConsumption int `json:"sessions_with_consumption"`
	// SessionsWithoutConsumption recorded no referenced claim. Whether that means "the detector
	// ran and found nothing" or "no detector ever ran" is NOT recoverable from the receipt, which
	// is exactly why these are never called measured zeros.
	SessionsWithoutConsumption int `json:"sessions_without_consumption"`

	// ConsumptionProvenanceRecoverable is false for as long as the Receipt carries no writer for
	// UsedDeclared. It is a constant today on purpose: it is the machine-readable form of the
	// caveat, so a consumer computing a rate has something to check rather than prose to parse.
	ConsumptionProvenanceRecoverable bool `json:"consumption_provenance_recoverable"`

	LastConsumed string `json:"last_consumed"` // coarse age or "never"
	Note         string `json:"note"`
}

func doctorUsage() string {
	return "usage: compound doctor [--base DIR] [--binary PATH] [--home P] [--json]\n" +
		"  read-only operator health: per-harness hook install/drift, land-lease state, pending findings, placement guard mode.\n" +
		"  --base/--binary scope the hook check (default: user home + this binary); writes nothing."
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	if hasUsageFlag(args) {
		fmt.Fprintln(stderr, doctorUsage())
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rep := doctorReport{Home: homePath}
	rep.UnattendedExecute = doctor.BuildUnattendedExecuteRows(homePath, time.Now().UTC(), executewire.CapacityFromSnapshot)

	// 1) Hooks — per-harness installed/ok/drift/absent via the install CHECK path (writes nothing).
	base := strings.TrimSpace(flagValue(args, "--base"))
	if base == "" {
		if h, herr := os.UserHomeDir(); herr == nil {
			base = h
		}
	}
	binary := strings.TrimSpace(flagValue(args, "--binary"))
	if binary == "" {
		if exe, eerr := os.Executable(); eerr == nil {
			binary = exe
		}
	}
	if base != "" && binary != "" {
		if hookRep, herr := hookplan.InstallHooks(base, binary, "", nil, true); herr == nil {
			for _, e := range hookRep.Entries {
				rep.Hooks = append(rep.Hooks, doctorHook{Harness: e.Harness, Status: e.Action, ConfigPath: e.ConfigPath})
			}
		}
	}
	for _, h := range rep.Hooks {
		if h.Status == "absent" || h.Status == "drift" {
			rep.HookHint = "run `compound hook install` to (re)wire the guard"
			break
		}
	}

	// 1b) Hook enforcement — the check above proves the config names Compound's command; this
	// proves the binary behind that command still refuses a mutation. A wired-but-no-op gate is
	// the failure mode with no other signal anywhere.
	rep.HookEnforcement, rep.HookEnforcementFailing = buildHookEnforcementRows(rep.Hooks)

	// 2) The legacy layout has one shared lease. After cutover, proof has
	// one selected resource and landing must be queried for a bound work; there
	// is no truthful home-wide landing holder to project into the old fields.
	if layout, lerr := landlease.LayoutStatus(homePath); lerr == nil {
		rep.LeaseLayout = layout.Mode
		rows, serr := landlease.ProofResourceStatuses(homePath, time.Now())
		if serr != nil {
			rep.LeaseStatusError = serr.Error()
		} else if len(rows) == 1 {
			st := rows[0].Status
			if layout.Mode == "legacy" {
				rep.LeaseHeld = st.Held
				rep.LeaseHolder = st.Holder
				rep.LeaseExpiresAt = st.ExpiresAt
				rep.LeaseStealReason = st.StealReason
			} else {
				rep.ProofLeaseHeld = st.Held
				rep.ProofLeaseHolder = st.Holder
				rep.ProofLeaseExpiresAt = st.ExpiresAt
				rep.LandingLeaseStatusHint = "compound land-lease status --work <work_id>"
			}
		} else {
			rep.ProofCapacities = rows
			for _, row := range rows {
				rep.ProofLeaseHeld = rep.ProofLeaseHeld || row.Status.Held
			}
			rep.LandingLeaseStatusHint = "compound land-lease status --work <work_id>"
		}
	} else {
		rep.LeaseStatusError = lerr.Error()
	}

	// 3) Findings — total, superseded (any id named by another finding's Supersedes), live.
	if fs, ferr := finding.List(homePath, ""); ferr == nil {
		superseded := map[string]bool{}
		for _, f := range fs {
			if s := strings.TrimSpace(f.Supersedes); s != "" {
				superseded[s] = true
			}
		}
		rep.FindingsTotal = len(fs)
		for _, f := range fs {
			if superseded[f.ID] {
				rep.FindingsSuperseded++
			} else {
				rep.FindingsLive++
			}
		}
	}

	// 3b) C3: RunAttempt crash terminality — count stuck (pending/running)
	// attempts and report the oldest. Read-only; sweeping lives on the
	// `compound run-attempt sweep` verb, never here.
	//
	// Sweep-teeth (DEC-0027 build step 1, work_195771b63a58) extends this
	// pass with two extra counters read from the same ListNonTerminal walk:
	//   - RunAttemptsStuckPastTTL: how many of the stuck attempts have last
	//     activity older than DefaultSweepThreshold. The predicate requires
	//     this to be zero after a sweep; non-zero is a finding.
	//   - RunAttemptsSweptAmbiguous: how many abandoned attempts carry
	//     sweep_classification="ambiguous" (had transcript/diff/test or
	//     observed_external before the sweep terminalized them). These
	//     were swept but still need operator reconciliation.
	//
	// All three counters come from one walk of the on-disk run_attempts
	// JSON — doctor never invokes the sweep, never writes, never crosses
	// the read-only boundary this section already lives inside.
	runAttempts, runAttemptsErr := runattempt.Open(homePath).List()
	rep.RunAttemptsReadError = runAttemptsErr != nil
	rep.RunAttemptsUnreadableIDs = runattempt.UnreadableRunAttemptIDs(runAttemptsErr)
	rep.RunAttemptsUnreadable = len(rep.RunAttemptsUnreadableIDs)
	var stuck []runattempt.RunAttempt
	for _, ra := range runAttempts {
		if !runattempt.IsTerminal(ra.Status) {
			stuck = append(stuck, ra)
		}
	}
	if len(stuck) > 0 {
		rep.RunAttemptsStuck = len(stuck)
		oldest := stuck[0].CreatedAt // List sorts by CreatedAt ascending
		var stuckPastTTLOldest time.Time
		for _, ra := range stuck {
			if ra.CreatedAt.Before(oldest) {
				oldest = ra.CreatedAt
			}
			lastActivity := ra.CreatedAt
			if ra.LastHeartbeatAt != nil {
				lastActivity = *ra.LastHeartbeatAt
			}
			if time.Since(lastActivity) > runattempt.DefaultSweepThreshold {
				rep.RunAttemptsStuckPastTTL++
				if stuckPastTTLOldest.IsZero() || lastActivity.Before(stuckPastTTLOldest) {
					stuckPastTTLOldest = lastActivity
				}
			}
		}
		rep.RunAttemptsStuckOldest = surfacingAge(time.Since(oldest))
		if rep.RunAttemptsStuckPastTTL > 0 {
			rep.RunAttemptsStuckPastTTLOldest = surfacingAge(time.Since(stuckPastTTLOldest))
		}
	}

	// RunAttemptsSweptAmbiguous is read from the same FULL attempt snapshot:
	// these are terminalized-but-flagged, while the stuck counters above are
	// the non-terminal subset. One walk also makes unreadable evidence visible
	// consistently across both views.
	var ambiguousOldest time.Time
	for _, ra := range runAttempts {
		if ra.Status != runattempt.StatusAbandoned {
			continue
		}
		if ra.SweepClassification != runattempt.SweepClassificationAmbiguous {
			continue
		}
		rep.RunAttemptsSweptAmbiguous++
		rep.RunAttemptsSweptAmbiguousIDs = append(rep.RunAttemptsSweptAmbiguousIDs, ra.ID)
		if ambiguousOldest.IsZero() || ra.CompletedAt.Before(ambiguousOldest) {
			ambiguousOldest = ra.CompletedAt
		}
	}
	if rep.RunAttemptsSweptAmbiguous > 0 {
		rep.RunAttemptsSweptAmbiguousOldest = surfacingAge(time.Since(ambiguousOldest))
	}

	// 3c: worktree locks whose owning delegate is terminal. Read-only; the
	// release path is delegate teardown, never a health command.
	rep.WorktreeLocks, rep.WorktreeLocksStale = buildWorktreeLockRows(homePath)

	// 4) Placement guard mode — observe is fail-open (record-only).
	if cfg, _, cerr := policy.LoadConfig(homePath); cerr == nil {
		rep.PlacementMode = cfg.PlacementGuardMode
		if cfg.PlacementGuardMode == "observe" {
			rep.PlacementNote = "observe = record-only (a mis-placed write is NOT blocked); set to block to enforce"
		}
	}
	rep.PlacementSources = buildPlacementSourceFreshness(homePath)

	// 4b) Retention — scan the repo this doctor runs against for accumulators, and check them
	// against the declared classes. Scoped to the resolved repo root rather than the whole
	// home: a recursive home sweep inside a health command would be slow enough that people
	// stop running it, and a check nobody runs protects nothing. The report names what it
	// scanned so "clean" is never confused with "did not look".
	if repoRoot := resolveRetentionScanRoot(); repoRoot != "" {
		scan := policy.ScanRetention([]string{repoRoot})
		rep.Retention = &scan
	}

	// 5) Permission posture — per-harness authority drift, read-only (harness.PermissionDrift
	// only reads; it never renders/applies). Emission of an observation batch stays exclusively
	// on `compound harness drift --emit-divergence`, never here.
	rep.PermissionPosture = buildPermissionPostureRows(homePath)

	// 6) Surfacing coverage — read-only lanes/harnesses × communication-surfaces matrix.
	rep.Surfacing, rep.SurfacingNote, rep.SurfacingInjection = buildSurfacingCoverage(homePath, rep.Hooks, time.Now())

	// 7) Projection freshness — which persisted projections predate the running
	// binary, i.e. "is the fix I just installed actually being served?". Read-only:
	// doctor reports the gap and never rebuilds, because a health command that
	// mutates shared projections would both surprise concurrent lanes and destroy
	// the evidence it is showing.
	if codeBuiltAt, exeErr := home.RunningBinaryBuiltAt(); exeErr == nil {
		if rows, projErr := home.ProjectionFreshnessReport(homePath, codeBuiltAt, time.Now()); projErr == nil {
			rep.Projections = rows
			rep.ProjectionsStale, rep.ProjectionsUndated, rep.ProjectionsOldest = home.StaleProjectionSummary(rows)
		} else {
			rep.ProjectionsNote = "projection freshness unavailable: " + projErr.Error()
		}
	} else {
		rep.ProjectionsNote = "projection freshness unavailable: cannot resolve the running binary: " + exeErr.Error()
	}

	// 8) Binary staleness — is installed binary at least as new as repo's target
	// tip? Named signal (installed SHA + target tip + relation) so operator
	// reads verdict instead of guessing from green light. Closes 2026-08-17
	// gap: doctor said all-clear while installed five commits behind main.
	// Unconditional, and deliberately NOT nested in the projections branch above:
	// that check asks whether a projection is as new as the running binary, this one
	// asks whether the running binary is as new as main. Riding on its failure path
	// served the signal only when there was no installed SHA left to compare.
	cwd, _ := os.Getwd()
	if repoRoot, ok := findRepoRootFrom(cwd); ok {
		rep.BinaryStaleness = doctor.ComputeBinaryStaleness(
			repoRoot,
			doctor.InstalledBinaryPath(),
			doctor.RunInstalledVersion,
			doctor.ParseInstalledBuildSHA,
			doctor.GitHeadSHA,
			doctor.GitIsAncestor,
		)
	} else {
		rep.BinaryStaleness = doctor.BinaryStalenessReport{
			Relation: doctor.RelationUnknown,
			Note:     "binary staleness unavailable: cwd not inside a git checkout",
		}
	}
	// 9) Harness version drift — installed binary version vs ExpectedVersion
	// per registered harness. Rows + count stay non-failing named SIGNALS in
	// the report body; under the exit-code contract a non-zero DriftCount is
	// also counted as a problem (exit 1), because a harness running a
	// different binary than the registry pinned is actionable, not advisory.
	rep.HarnessVersionDrift = doctor.ComputeHarnessVersionDrift(harness.DefaultRegistry())
	rep.BindingFreshness, rep.BindingFreshnessNote = contract.BuildBindingFreshnessReport(homePath, binary) // R3B 2026-09-04: hook-binding freshness (compute+render in contract).

	// Doctor exit-code contract (packet work_3da5d3c76707): a doctor that
	// finds a real, actionable problem exits 1, prints a final
	// plain-language count line, and exposes warnings under a
	// `warnings:` heading. The shared helper lives at
	// compound/internal/doctor/exitcode; here we just count the named
	// problem counters the doctor already produces. JSON mode adds a
	// problems[] array and warnings[] array so a CI consumer can grep
	// them without parsing prose.
	rep.Problems = rep.problemStrings()
	rep.Warnings = rep.warningStrings()
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, rep); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return exitcode.CouldNotRun
		}
		if len(rep.Problems) > 0 {
			return exitcode.ProblemFound
		}
		return exitcode.OK
	}
	printDoctor(stdout, rep)
	return exitcode.Doctor{Verb: "doctor", Problems: rep.Problems, Warnings: rep.Warnings, Note: "start with the first problem above"}.ExitWith(stdout)
}

// problemStrings reduces the doctor's existing named counters to the
// plain-language problem list the contract demands. Each sentence names
// the count and the highest-leverage remediation so the canonical
// "doctor: N problem(s) found — <hint>" count line never needs
// internal jargon. Ordering is deterministic so a shell caller grepping
// for "doctor:" sees the same problems in the same order on every run.
func (r doctorReport) problemStrings() []string {
	var p []string
	if r.WorktreeLocksStale > 0 {
		p = append(p, fmt.Sprintf("%d stale worktree lock(s) protect nobody — run `compound work delegate sweep <work_id>` on the affected work id(s)", r.WorktreeLocksStale))
	}
	if r.RunAttemptsStuckPastTTL > 0 {
		p = append(p, fmt.Sprintf("%d run attempt(s) past the sweep TTL — run `compound run-attempt sweep`", r.RunAttemptsStuckPastTTL))
	}
	if r.RunAttemptsUnreadable > 0 {
		p = append(p, fmt.Sprintf("%d run attempt record(s) failed validation — inspect before sweeping", r.RunAttemptsUnreadable))
	}
	if r.RunAttemptsSweptAmbiguous > 0 {
		p = append(p, fmt.Sprintf("%d swept run attempt(s) carry ambiguous classification — reconcile the named id(s)", r.RunAttemptsSweptAmbiguous))
	}
	if r.BinaryStaleness.Relation == doctor.RelationAncestor {
		p = append(p, "installed binary is behind main — reinstall it (`make install`) and re-run doctor")
	}
	if r.HookEnforcementFailing > 0 {
		p = append(p, fmt.Sprintf("%d hook enforcement check(s) failed — the binary at the configured path cannot enforce", r.HookEnforcementFailing))
	}
	if r.HarnessVersionDrift.DriftCount > 0 {
		p = append(p, fmt.Sprintf("%d harness(es) drifted from the expected version — see the harness_version_drift section above", r.HarnessVersionDrift.DriftCount))
	}
	sort.Strings(p)
	return p
}

// warningStrings surfaces info-only conditions the doctor refuses to
// fail on: a hook whose config is drifted or absent but whose
// underlying binary is still enforcing, binary staleness that is
// merely unknown (cwd is not a checkout), and projections that predate
// the running binary (normal after any rebuild; see
// printProjectionFreshness for why alarm there trains operators to
// skip the section). Harness version drift is deliberately NOT here —
// it is a problem (see problemStrings); listing it twice would let a
// reader dismiss the failure as advisory. The JSON shape carries the
// array under `warnings`; the text shape prints them under a
// `warnings:` heading per the shared contract.
func (r doctorReport) warningStrings() []string {
	var w []string
	for _, h := range r.Hooks {
		if h.Status == "drift" || h.Status == "absent" {
			w = append(w, fmt.Sprintf("harness %s %s — install guard via `compound hook install`", h.Harness, h.Status))
		}
	}
	if r.BinaryStaleness.Relation == doctor.RelationUnknown {
		w = append(w, "binary staleness unknown — cwd is not a git checkout; doctor could not compare installed SHA to main")
	}
	if r.ProjectionsStale > 0 {
		// Deliberately a warning, not a problem: rebuilding the binary
		// makes nearly every projection older than it, so a stale row is
		// normal and only matters when the change being trusted touched
		// that projection's producer (see printProjectionFreshness).
		// Counting it toward exit 1 would make every rebuild red and
		// train the operator to ignore the doctor.
		w = append(w, fmt.Sprintf("%d persisted projection(s) predate the running binary — matters only if your change touched their producer", r.ProjectionsStale))
	}
	sort.Strings(w)
	return w
}

func printDoctor(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "# compound doctor")
	fmt.Fprintf(stdout, "home: %s\n", rep.Home)

	fmt.Fprintln(stdout, "\n## hooks (PreToolUse guard)")
	if len(rep.Hooks) == 0 {
		fmt.Fprintln(stdout, "- (could not read harness configs)")
	}
	for _, h := range rep.Hooks {
		fmt.Fprintf(stdout, "- %-12s %-9s %s\n", h.Harness, h.Status, h.ConfigPath)
	}
	if rep.HookHint != "" {
		fmt.Fprintf(stdout, "  -> %s\n", rep.HookHint)
	}

	printHookEnforcement(stdout, rep)

	fmt.Fprintln(stdout, "\n## land-lease (concurrent landing)")
	if rep.LeaseStatusError != "" {
		fmt.Fprintf(stdout, "- status unavailable: %s\n", rep.LeaseStatusError)
	} else if rep.LeaseLayout == "scoped" {
		if len(rep.ProofCapacities) != 0 {
			for _, row := range rep.ProofCapacities {
				fmt.Fprintf(stdout, "- proof %s: held=%v holder=%q expires=%s\n", row.Class, row.Status.Held, row.Status.Holder, row.Status.ExpiresAt)
			}
		} else if rep.ProofLeaseHeld {
			fmt.Fprintf(stdout, "- proof: HELD by %q until %s\n", rep.ProofLeaseHolder, rep.ProofLeaseExpiresAt)
		} else {
			fmt.Fprintln(stdout, "- proof: free")
		}
		fmt.Fprintf(stdout, "- landing: per repository/ref; use `%s`\n", rep.LandingLeaseStatusHint)
	} else {
		if rep.LeaseHeld {
			fmt.Fprintf(stdout, "- HELD by %q until %s\n", rep.LeaseHolder, rep.LeaseExpiresAt)
			fmt.Fprintln(stdout, "  -> wait, or `compound land-lease steal --reason ...` if stale")
		} else {
			fmt.Fprintln(stdout, "- free")
		}
		if rep.LeaseStealReason != "" {
			fmt.Fprintf(stdout, "  last steal: %s\n", rep.LeaseStealReason)
		}
	}

	fmt.Fprintln(stdout, "\n## findings (operational evidence)")
	fmt.Fprintf(stdout, "- %d total (%d live, %d superseded)\n", rep.FindingsTotal, rep.FindingsLive, rep.FindingsSuperseded)
	if rep.FindingsTotal > 0 {
		fmt.Fprintln(stdout, "  -> `compound finding list` to review, `compound finding doctor` for stale/superseded")
	}

	fmt.Fprintln(stdout, "\n## run attempts (crash terminality)")
	if rep.RunAttemptsUnreadable > 0 {
		fmt.Fprintf(stdout, "- UNREADABLE: %d run attempt(s): %s\n", rep.RunAttemptsUnreadable, strings.Join(rep.RunAttemptsUnreadableIDs, ", "))
		fmt.Fprintln(stdout, "  -> inspect or restore the named record/launch evidence before sweeping")
	} else if rep.RunAttemptsReadError {
		fmt.Fprintln(stdout, "- UNREADABLE: run-attempt store could not be inspected; lifecycle counts are incomplete")
	}
	if rep.RunAttemptsStuck == 0 {
		if rep.RunAttemptsReadError {
			fmt.Fprintln(stdout, "- no readable stuck attempts; terminality is unproven while evidence is unreadable")
		} else {
			fmt.Fprintln(stdout, "- no stuck attempts (all terminal)")
		}
	} else {
		fmt.Fprintf(stdout, "- %d stuck (pending/running), oldest %s ago\n", rep.RunAttemptsStuck, rep.RunAttemptsStuckOldest)
		fmt.Fprintln(stdout, "  -> `compound run-attempt sweep` to terminalize stale attempts")
	}

	printWorktreeLocks(stdout, rep)

	fmt.Fprintln(stdout, "\n## placement guard")
	fmt.Fprintf(stdout, "- mode: %s\n", rep.PlacementMode)
	if rep.PlacementNote != "" {
		fmt.Fprintf(stdout, "  note: %s\n", rep.PlacementNote)
	}
	policy.RenderPresenceDoctorLine(stdout, rep.Home, time.Now().UTC())
	operatornotice.RenderDoctorLines(stdout, rep.Home, time.Now().UTC())
	printPlacementSourceFreshness(stdout, rep)
	printRetention(stdout, rep)

	printProjectionFreshness(stdout, rep)
	doctor.RenderBinaryStaleness(stdout, rep.BinaryStaleness)
	doctor.RenderHarnessVersionDrift(stdout, rep.HarnessVersionDrift)
	contract.RenderBindingFreshness(stdout, rep.BindingFreshness, rep.BindingFreshnessNote)
	lanes.RenderInstalledLaneDriftVerdicts(stdout, rep.Home, doctor.InstalledBinaryPath(), time.Now().UTC()) // R3B 2026-09-04.
	printPermissionPosture(stdout, rep)
	printSurfacing(stdout, rep)
	doctor.PrintUnattendedExecute(stdout, rep.UnattendedExecute)
}

// doctorPlacementSource is one imported placement source view and whether the bytes
// Compound enforces still match the bytes that view renders today.
type doctorPlacementSource struct {
	SourcePath string `json:"source_path"`
	// ImportedAt/ImportedRevision come from the NEWEST import for this path.
	ImportedAt       string `json:"imported_at,omitempty"`
	ImportedRevision string `json:"imported_revision,omitempty"`
	LiveRevision     string `json:"live_revision,omitempty"`
	// State: "current" | "stale" | "source-missing" | "source-unreadable".
	State string `json:"state"`
}

// buildPlacementSourceFreshness answers a question no other surface answers: are the
// placement rules Compound ENFORCES still the rules their source view renders?
//
// Compound holds both halves — the append-only import history and the source path it
// imported from — so the comparison belongs here rather than in the repo that renders
// the view. That repo can prove it WROTE the file; only Compound knows what it imported.
//
// Reported, never repaired: doctor writes nothing, and re-importing is a policy
// mutation that must go through `compound policy import-placement-rules`.
//
// A stale row is not automatically a defect — the source may have been regenerated with
// changes that were deliberately not adopted yet. It is a question the operator can
// otherwise only answer by hand-diffing a sha256.
func buildPlacementSourceFreshness(homePath string) []doctorPlacementSource {
	records, err := policy.LoadPlacementSourceAuthority(homePath)
	if err != nil || len(records) == 0 {
		return nil
	}

	// The history is append-only and one path appears once PER IMPORT, so collapse to the
	// newest record per path. Taking the first match would compare today's file against a
	// months-old import and report drift on a correctly-enforced source.
	newest := map[string]policy.PlacementSourceAuthorityRecord{}
	for _, rec := range records {
		if rec.SourcePath == "" {
			continue
		}
		prev, seen := newest[rec.SourcePath]
		if !seen || rec.CreatedAt >= prev.CreatedAt {
			newest[rec.SourcePath] = rec
		}
	}

	paths := make([]string, 0, len(newest))
	for path := range newest {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rows := make([]doctorPlacementSource, 0, len(paths))
	for _, path := range paths {
		rec := newest[path]
		row := doctorPlacementSource{
			SourcePath:       path,
			ImportedAt:       rec.CreatedAt,
			ImportedRevision: rec.SourceRevision,
		}
		data, readErr := os.ReadFile(path)
		switch {
		case os.IsNotExist(readErr):
			row.State = "source-missing"
		case readErr != nil:
			row.State = "source-unreadable"
		default:
			row.LiveRevision = "sha256:" + policy.DigestHex(data)
			if row.LiveRevision == rec.SourceRevision {
				row.State = "current"
			} else {
				row.State = "stale"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// printPlacementSourceFreshness stays quiet when every imported source is current: a
// health surface that prints a wall of green trains the operator to skip it.
func printPlacementSourceFreshness(stdout io.Writer, rep doctorReport) {
	if len(rep.PlacementSources) == 0 {
		return
	}
	notCurrent := make([]doctorPlacementSource, 0, len(rep.PlacementSources))
	for _, row := range rep.PlacementSources {
		if row.State != "current" {
			notCurrent = append(notCurrent, row)
		}
	}
	if len(notCurrent) == 0 {
		fmt.Fprintf(stdout, "- sources: %d imported, all current with what Compound enforces\n", len(rep.PlacementSources))
		return
	}
	for _, row := range notCurrent {
		switch row.State {
		case "stale":
			fmt.Fprintf(stdout, "- source drift: %s\n", row.SourcePath)
			fmt.Fprintf(stdout, "  enforcing %s (imported %s), source now renders %s\n",
				row.ImportedRevision, row.ImportedAt, row.LiveRevision)
			fmt.Fprintln(stdout, "  -> `compound policy import-placement-rules <path>` to adopt, or leave if the change is not meant to be in force")
		case "source-missing":
			fmt.Fprintf(stdout, "- source missing: %s (still enforcing %s from %s)\n",
				row.SourcePath, row.ImportedRevision, row.ImportedAt)
		default:
			fmt.Fprintf(stdout, "- source unreadable: %s\n", row.SourcePath)
		}
	}
}

// resolveRetentionScanRoot returns the repository root to scan for accumulators.
//
// Walks up from cwd to the enclosing git root. Returns "" rather than guessing when there is
// no repo: scanning an arbitrary directory would produce findings the operator cannot act on.
func resolveRetentionScanRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if info, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

// printRetention renders what is accumulating and whether anything owns it.
//
// The section leads with UNDECLARED accumulators because that is the only part that represents
// a gap in the system rather than a number to watch: a class nothing claims is a new way to
// fill the disk that no owner is tracking. Declared-and-rebuildable bytes are reported as an
// opportunity, not an alarm — they are supposed to exist, and knowing the figure is what lets
// a reclaimer act before pressure rather than after.
func printRetention(stdout io.Writer, rep doctorReport) {
	if rep.Retention == nil {
		return
	}
	scan := rep.Retention
	fmt.Fprintln(stdout, "\n## retention (what accumulates, and who owns it)")
	if len(scan.Scanned) == 0 {
		fmt.Fprintln(stdout, "- no repository root resolved; nothing scanned")
		return
	}

	if len(scan.Undeclared) > 0 {
		fmt.Fprintf(stdout, "- UNDECLARED accumulators: %d — no retention class claims these\n", len(scan.Undeclared))
		for _, item := range scan.Undeclared {
			prefix := " "
			if item.Truncated {
				prefix = "\u2265" // budget-truncated: a lower bound, never presented as exact
			}
			fmt.Fprintf(stdout, "    %s%8s  %s\n", prefix, humanBytes(item.Bytes), item.Path)
		}
		fmt.Fprintln(stdout, "  -> declare each in internal/policy/retention.go, or they will accumulate unowned")
	} else {
		fmt.Fprintln(stdout, "- every accumulator found is claimed by a declared class")
	}

	if scan.ReclaimableBytes > 0 {
		fmt.Fprintf(stdout, "- reclaimable now (rebuildable, zero risk): %s across %d instance(s)\n",
			humanBytes(scan.ReclaimableBytes), len(scan.Declared))
	}
	if len(scan.ClassesWithoutMechanism) > 0 {
		// A declared class without a mechanism is operationally incomplete.
		fmt.Fprintf(stdout, "- declared but no reclaim mechanism exists: %s\n",
			strings.Join(scan.ClassesWithoutMechanism, ", "))
	}
	fixedCacheScope := "skipped"
	if scan.FixedPathCachesIncluded {
		fixedCacheScope = "included"
	}
	fmt.Fprintf(stdout, "  fixed-path caches: %s\n", fixedCacheScope)
	fmt.Fprintf(stdout, "  scanned: %s\n", strings.Join(scan.Scanned, ", "))
}

// humanBytes formats a byte count for operator output.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	// KiB/MiB/GiB, not KB/MB/GB: the divisor is 1024. Mislabelling these understates a
	// threshold by ~7% at GiB scale, and this output exists to reason about thresholds.
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printProjectionFreshness renders which persisted projections were NOT built by the
// code now installed.
//
// Deliberately worded as "not built by the running code" rather than "stale": the
// body may be perfectly correct, and only matters when the change being trusted
// touched that projection's producer — which doctor cannot know. Overstating it here
// would train the operator to skip the section, which costs exactly what the silent
// gap cost (find_b27a3de99682).
func printProjectionFreshness(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "\n## projections (is an installed fix actually being served?)")
	if rep.ProjectionsNote != "" {
		fmt.Fprintf(stdout, "- %s\n", rep.ProjectionsNote)
		return
	}
	if len(rep.Projections) == 0 {
		fmt.Fprintln(stdout, "- (no persisted projections)")
		return
	}
	if rep.ProjectionsStale == 0 {
		fmt.Fprintf(stdout, "- all %d projection(s) built by the running binary or newer\n", len(rep.Projections))
	} else {
		// Conditional framing first, count second. Rebuilding the binary makes nearly
		// every projection older than it, so a row here is normal and only matters
		// when the change being trusted touched that projection's producer. Leading
		// with alarm would make the section skippable, which is the failure this
		// section exists to prevent.
		fmt.Fprintf(stdout, "- %d of %d were not built by the running binary — harmless unless your change touched their producer\n",
			rep.ProjectionsStale, len(rep.Projections))
		for _, row := range rep.Projections {
			if !row.OlderThanCode {
				continue
			}
			line := fmt.Sprintf("  %-30s %s=%s (%.0fh old)", row.Name, row.TimestampField, row.ComputedAt, row.AgeHours)
			if row.RebuildCommand != "" {
				line += "  rebuild: " + row.RebuildCommand
			}
			fmt.Fprintln(stdout, line)
		}
		fmt.Fprintln(stdout, "  -> a fix to a projection's PRODUCER is not served until that projection is rebuilt;")
		fmt.Fprintln(stdout, "     rebuild the affected one, then re-observe the REAL path (a test that rebuilds its")
		fmt.Fprintln(stdout, "     own inputs cannot see a stale persisted input)")
	}
	if rep.ProjectionsUndated > 0 {
		fmt.Fprintf(stdout, "- %d projection(s) record no build time, so freshness cannot be proven either way\n", rep.ProjectionsUndated)
	}
}

// printPermissionPosture renders the per-harness permission-authority drift
// rows. Read-only: it never renders or applies -- a drifted row only means
// `compound harness drift --harness <h> --emit-divergence` will declare a
// divergence_declared card the next time it runs.
func printPermissionPosture(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "\n## permission posture (per-harness authority drift)")
	if len(rep.PermissionPosture) == 0 {
		fmt.Fprintln(stdout, "- (no harness-permission authorities configured)")
		return
	}
	for _, p := range rep.PermissionPosture {
		fmt.Fprintf(stdout, "- %-12s %-7s %s\n", p.Harness, p.Status, p.TargetPath)
		for _, d := range p.Details {
			fmt.Fprintf(stdout, "    detail: %s\n", d)
		}
		if p.Status == "drift" {
			fmt.Fprintf(stdout, "  -> `compound harness drift --harness %s --emit-divergence` to declare a divergence card\n", p.Harness)
		}
	}
}

// buildPermissionPostureRows discovers configured harness-permission
// authorities (config/harness-permissions/*.yaml) and probes each with
// harness.PermissionDrift -- read-only, writes nothing. A harness whose
// probe errors (e.g. an unreadable target file) still gets a visible row
// (status "unknown") rather than silently vanishing from the report.
func buildPermissionPostureRows(homePath string) []doctorPermissionPosture {
	matches, err := filepath.Glob(filepath.Join(homePath, "config", "harness-permissions", "*.yaml"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	var rows []doctorPermissionPosture
	for _, match := range matches {
		name := strings.TrimSuffix(filepath.Base(match), ".yaml")
		report, err := harness.PermissionDrift(homePath, name)
		if err != nil {
			rows = append(rows, doctorPermissionPosture{Harness: name, Status: "unknown", Details: []string{err.Error()}})
			continue
		}
		status := "clean"
		if report.Drifted {
			status = "drift"
		}
		rows = append(rows, doctorPermissionPosture{Harness: name, Status: status, TargetPath: report.TargetPath, Details: report.Details})
	}
	return rows
}

// printSurfacing renders the surfacing-coverage matrix. Cells stay narrow (no table wider than
// ~100 cols, no color); the global injection numbers and the S2 peers caveat are stated once in
// note lines rather than repeated per row, and a trailing "!" marks an expected-but-unwired cell.
func printSurfacing(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "\n## surfacing coverage")
	fmt.Fprintln(stdout, "rows = agent lanes/harnesses · cols = communication surfaces · ! = expected surface not wired (guard+orient expected)")
	if len(rep.Surfacing) == 0 {
		fmt.Fprintln(stdout, "- (no lanes or harnesses to report)")
		return
	}
	fmt.Fprintf(stdout, "%-30s %-9s %-15s %-14s %s\n", "lane", "guard", "orient", "injection", "peers")
	seen := map[string]bool{}
	for _, r := range rep.Surfacing {
		below := map[string]bool{}
		for _, c := range r.BelowExpectation {
			below[c] = true
		}
		guard := r.Guard
		if below["guard"] {
			guard += "!"
		}
		orient := r.Orient
		if r.Orient == "wired" && r.OrientAge != "" {
			orient = "wired (" + r.OrientAge + ")"
		}
		if below["orient"] {
			orient += "!"
		}
		line := fmt.Sprintf("%-30s %-9s %-15s %-14s %s", surfacingLaneCell(r.Lane, 30), guard, orient, r.Injection, r.Peers)
		// Byte-identical rendered rows collapse to one; the JSON report still lists every row.
		if seen[line] {
			continue
		}
		seen[line] = true
		fmt.Fprintln(stdout, line)
	}
	if rep.SurfacingNote != "" {
		fmt.Fprintf(stdout, "-> %s\n", rep.SurfacingNote)
	}
	inj := rep.SurfacingInjection
	fmt.Fprintf(stdout, "-> injection %s: served %d event(s) across %d session(s), consumed %d event(s) across %d session(s), last-consumed %s (%s)\n",
		inj.Scope, inj.ServedEvents, inj.SessionsServed,
		inj.ConsumedEvents, inj.SessionsWithConsumption, inj.LastConsumed, inj.Note)
	if !inj.ConsumptionProvenanceRecoverable && inj.SessionsWithoutConsumption > 0 {
		fmt.Fprintf(stdout, "-> injection coverage: no consumption rate is computable from these numbers. %d session(s) recorded no referenced claim, but skills.used_declared has two writers — a typed --used flag (cmd/compound/work_cli.go) and a real session-end detector run (internal/hook/hook.go) — and the receipt records neither, so those sessions are UNKNOWN, not zero.\n",
			inj.SessionsWithoutConsumption)
	}
	fmt.Fprintln(stdout, "-> peers: active leases + agent-lane cards render in each harness's orient ContextPack (contextual, display-only)")
}

// surfacingInjectionCell renders the matrix's injection cell: served/consumed events, then the
// session counts those came from. The session pair is what stops the event pair from reading as a
// rate at a glance — "1142/5 (1/299)" says five references were seen in one session out of 299,
// which is a coverage statement, not a percentage. The arrow lines below the matrix carry the full
// caveat; this cell is only the glance.
func surfacingInjectionCell(inj surfacingInjection) string {
	if inj.SessionsServed > 0 {
		return fmt.Sprintf("%d/%d (%d/%ds)", inj.ServedEvents, inj.ConsumedEvents, inj.SessionsWithConsumption, inj.SessionsServed)
	}
	return fmt.Sprintf("%d/%d (global)", inj.ServedEvents, inj.ConsumedEvents)
}

// surfacingLaneCell renders a lane name into the fixed-width lane column. When the name exceeds the
// column it is truncated, but a short deterministic fingerprint of the FULL name is appended so two
// lanes that share a prefix up to the cut still render as distinct rows (rather than collapsing into
// one indistinguishable truncated cell). The fingerprint is pure ASCII so the fmt width verb pads
// predictably and the table stays within its column budget.
func surfacingLaneCell(name string, width int) string {
	if width <= 0 {
		return name
	}
	r := []rune(name)
	if len(r) <= width {
		return name
	}
	const fp = 6 // hex chars of fingerprint
	keep := width - (1 + fp)
	if keep < 1 {
		keep = 1
	}
	return string(r[:keep]) + "~" + laneCellFingerprint(name, fp)
}

func laneCellFingerprint(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

// buildSurfacingCoverage derives the read-only surfacing matrix from state that already exists:
// the per-harness guard status (reused from the hook check), the last orient-produced evidence
// per harness (recorded EffectiveContracts), and the GLOBAL memory-injection numbers (the
// observation report does not record a per-harness split). Rows are the capgraph's agent-lane
// nodes when any are imported; otherwise the hook harness list, with an explicit note so the
// matrix is never silently empty on a fresh home. It writes nothing and makes no provider call.
func buildSurfacingCoverage(homePath string, hooks []doctorHook, now time.Time) ([]surfacingRow, string, surfacingInjection) {
	guardByHarness := map[string]string{}
	for _, h := range hooks {
		guardByHarness[surfacingHarnessAlias(h.Harness)] = guardStatus(h.Status)
	}
	orientByHarness, harnessByContract := scanEffectiveContracts(homePath)
	peersByHarness := lastPeersByHarness(homePath, harnessByContract)
	inj := injectionGlobalSummary(homePath, now)

	var rows []surfacingRow
	note := ""
	if lanes := agentLaneRows(homePath); len(lanes) > 0 {
		for _, ln := range lanes {
			rows = append(rows, surfacingRowFor(ln.Name, ln.Harness, guardByHarness, orientByHarness, peersByHarness, inj, now))
		}
	} else {
		note = "no agent lanes imported — showing hook harnesses only"
		for _, harness := range hookHarnessList(hooks) {
			rows = append(rows, surfacingRowFor(harness, harness, guardByHarness, orientByHarness, peersByHarness, inj, now))
		}
	}
	return rows, note, inj
}

// surfacingHarnessAlias normalizes a harness token to the join key the matrix uses across lane
// rows, hook entries, and recorded contracts. It mirrors capgraph's agent-lane harness alias
// (lowercase, trailing "-code" stripped) so "claude" and "claude-code" resolve to one row.
func surfacingHarnessAlias(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "_", "-")
	return strings.TrimSuffix(v, "-code")
}

// guardStatus maps a hook install/check action to the matrix guard vocabulary
// (installed/ok/updated -> wired; drift; absent; anything else -> unknown).
func guardStatus(action string) string {
	switch action {
	case "ok", "installed", "updated":
		return "wired"
	case "drift":
		return "drift"
	case "absent":
		return "absent"
	default:
		return "unknown"
	}
}

// surfacingRowFor assembles one row and derives its below-expectation columns: a harness-aliased
// lane is expected to have its guard wired and to have oriented at least once, so an absent/drift
// guard or a never-oriented harness renders with a trailing "!".
func surfacingRowFor(lane, rawHarness string, guardBy map[string]string, orientBy, peersBy map[string]time.Time, inj surfacingInjection, now time.Time) surfacingRow {
	alias := surfacingHarnessAlias(rawHarness)
	guard := guardBy[alias]
	if guard == "" {
		guard = "unknown"
	}
	orient := "never"
	orientAge := ""
	if t, ok := orientBy[alias]; ok {
		orient = "wired"
		orientAge = surfacingAge(now.Sub(t))
	}
	// peers is evidence-read: wired when the harness's latest orient-recorded ContextPack carried a
	// non-empty peers section, else absent. Display-only — never marked below-expectation.
	peers := "absent"
	if t, ok := peersBy[alias]; ok {
		peers = "wired (last " + surfacingAge(now.Sub(t)) + ")"
	}
	row := surfacingRow{
		Lane:      lane,
		Harness:   alias,
		Expected:  true,
		Guard:     guard,
		Orient:    orient,
		OrientAge: orientAge,
		Injection: surfacingInjectionCell(inj),
		Peers:     peers,
	}
	if guard == "absent" || guard == "drift" {
		row.BelowExpectation = append(row.BelowExpectation, "guard")
	}
	if orient == "never" {
		row.BelowExpectation = append(row.BelowExpectation, "orient")
	}
	return row
}

// agentLaneRows lists the non-retired agent-lane nodes for this home. An absent capability graph
// (fresh home) is not an error — it just means the fallback hook-harness rows are used.
func agentLaneRows(homePath string) []capgraph.AgentLaneNode {
	if !capgraph.Exists(homePath) {
		return nil
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return nil
	}
	nodes, err := store.AgentLaneNodes()
	if err != nil {
		return nil
	}
	out := make([]capgraph.AgentLaneNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Retired {
			continue
		}
		out = append(out, n)
	}
	return out
}

// hookHarnessList is the fallback row set when no agent lanes are imported. It uses the harnesses
// the hook check already reported, and falls back to the canonical two so the matrix is never
// silently empty even when the harness configs could not be read.
func hookHarnessList(hooks []doctorHook) []string {
	if len(hooks) == 0 {
		return []string{"claude-code", "codex"}
	}
	out := make([]string, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.Harness)
	}
	return out
}

// scanEffectiveContracts walks contracts/effective EXACTLY ONCE and returns
// the two artifacts that used to come from two independent walks of the same
// directory: orientByHarness (the most recent orient time per aliased
// harness -- what lastOrientByHarness used to return) and harnessByContract
// (contract_id -> aliased harness, the join table lastPeersByHarness's own
// first walk used to build). Both are derived from one decoded header per
// file (P3, find_9edaa8449d55 -- doctor no longer parses every
// EffectiveContract twice). The two outputs preserve their original,
// slightly different validity rules: harnessByContract only requires a
// non-empty id (lastPeersByHarness's join never looked at created_at), while
// orientByHarness additionally requires created_at to parse. Reading these
// artifacts directly keeps doctor read-only; a torn or foreign file is
// skipped, never fatal.
func scanEffectiveContracts(homePath string) (orientByHarness map[string]time.Time, harnessByContract map[string]string) {
	orientByHarness = map[string]time.Time{}
	harnessByContract = map[string]string{}
	base := filepath.Join(homePath, "contracts", "effective")
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var head struct {
			SchemaVersion string `json:"schema_version"`
			ID            string `json:"id"`
			Harness       string `json:"harness"`
			CreatedAt     string `json:"created_at"`
		}
		if json.Unmarshal(data, &head) != nil || (head.SchemaVersion != envelope.SVEffectiveContract && head.SchemaVersion != envelope.SVEffectiveContractV01) {
			return nil
		}
		alias := surfacingHarnessAlias(head.Harness)
		if id := strings.TrimSpace(head.ID); id != "" {
			harnessByContract[id] = alias
		}
		t, terr := time.Parse(time.RFC3339, strings.TrimSpace(head.CreatedAt))
		if terr != nil {
			return nil
		}
		if cur, ok := orientByHarness[alias]; !ok || t.After(cur) {
			orientByHarness[alias] = t
		}
		return nil
	})
	return orientByHarness, harnessByContract
}

// lastPeersByHarness returns the most recent orient time per aliased harness for which a RECORDED
// ContextPack carried a non-empty peers section. A recorded pack carries no harness of its own, so
// its harness is resolved through its effective-contract link (ids.contract_id) against
// harnessByContract, built by the shared contracts/effective walk (scanEffectiveContracts).
// Reading these artifacts directly keeps doctor read-only; a torn or foreign file is skipped,
// never fatal.
func lastPeersByHarness(homePath string, harnessByContract map[string]string) map[string]time.Time {
	out := map[string]time.Time{}
	_ = filepath.WalkDir(filepath.Join(homePath, "context", "packs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var head struct {
			SchemaVersion string `json:"schema_version"`
			CreatedAt     string `json:"created_at"`
			IDs           struct {
				ContractID string `json:"contract_id"`
			} `json:"ids"`
			Peers *struct {
				ActiveLeases []json.RawMessage `json:"active_leases"`
				Lanes        []json.RawMessage `json:"lanes"`
			} `json:"peers"`
		}
		if json.Unmarshal(data, &head) != nil || head.SchemaVersion != envelope.SVContextPack {
			return nil
		}
		// Recorded but empty peers is NOT evidence of a wired peers cell.
		if head.Peers == nil || (len(head.Peers.ActiveLeases) == 0 && len(head.Peers.Lanes) == 0) {
			return nil
		}
		alias, ok := harnessByContract[strings.TrimSpace(head.IDs.ContractID)]
		if !ok || alias == "" {
			return nil
		}
		t, terr := time.Parse(time.RFC3339, strings.TrimSpace(head.CreatedAt))
		if terr != nil {
			return nil
		}
		if cur, ok := out[alias]; !ok || t.After(cur) {
			out[alias] = t
		}
		return nil
	})
	return out
}

// injectionGlobalSummary reuses the memory-injection observation report and rolls it up to global
// served/consumed event totals plus the most recent consumption age. The report attributes
// sessions to work ids, not harnesses, so this is explicitly global (per-lane split not recorded).
func injectionGlobalSummary(homePath string, now time.Time) surfacingInjection {
	inj := surfacingInjection{Scope: "global", LastConsumed: "never", Note: "per-lane split not recorded"}
	rep, err := capgraph.BuildInjectionObservationReport(homePath, capgraph.InjectionObservationOptions{GeneratedAt: now})
	if err != nil {
		return inj
	}
	// Rolled up from Sessions, not Conditions, so the per-session counts below are available at
	// all; the event totals are identical either way (a condition summary is a sum over its own
	// sessions). Sessions are bucketed by OBSERVED consumption — did this session reference a
	// claim its own pack served — and deliberately NOT by UsedDeclared, which cannot distinguish a
	// detector run from a typed --used flag (see the type comment).
	for _, s := range rep.Sessions {
		inj.ServedEvents += s.ServedCount
		inj.ConsumedEvents += s.ConsumedCount
		if s.ServedCount == 0 {
			continue // served nothing: not a session this surface can say anything about
		}
		inj.SessionsServed++
		if s.ConsumedCount > 0 {
			inj.SessionsWithConsumption++
			continue
		}
		inj.SessionsWithoutConsumption++
	}
	var last time.Time
	for _, s := range rep.Sessions {
		if s.ConsumedCount <= 0 {
			continue
		}
		if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(s.CreatedAt)); perr == nil && t.After(last) {
			last = t
		}
	}
	if !last.IsZero() {
		inj.LastConsumed = surfacingAge(now.Sub(last))
	}
	return inj
}

// surfacingAge humanizes a duration coarsely (m/h/d) for the matrix. Ages are computed against a
// runtime clock and a runtime-written timestamp, never a fixed fixture date, so no fixture ages
// into a failure over time.
func surfacingAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
