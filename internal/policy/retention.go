package policy

// Retention registry — every class of artifact this system generates must declare who owns
// it, when it may be reclaimed, and how. An artifact class with no declared policy is a
// failure, not a default.
//
// The registry is compiled in so discovery cannot silently turn green because a runtime file
// disappeared. ReclaimTargetRef names the consumer whose threshold governs pressure-based
// cleanup, and discovery remains separate from declaration so new accumulators are visible.

import "sort"

const (
	// RetentionAuthorityRef names the registry for evidence refs.
	RetentionAuthorityRef = "compound://registries/retention-classes"

	// ReclaimAutomatic may be executed by a scheduled reclaimer without asking.
	ReclaimAutomatic = "automatic"
	// ReclaimOperatorGated is safe to DETECT and report but never to execute unattended,
	// because reclaiming it can destroy work or evidence that has no other copy.
	ReclaimOperatorGated = "operator_gated"
)

// ArtifactClass is one kind of thing that accumulates on disk.
type ArtifactClass struct {
	// ID is the stable slug used in doctor output and in scan classification.
	ID string `json:"id"`
	// Owner is the system that CREATES instances of this class. Naming it is what makes a
	// class actionable: reclamation without an owner is someone else's data.
	Owner string `json:"owner"`
	// Description says what an instance actually is, in one line.
	Description string `json:"description"`
	// Retention is the human-readable rule for when an instance may go.
	Retention string `json:"retention"`
	// ReclaimCommand is how an instance is reclaimed. Empty means no mechanism exists yet —
	// which is itself a finding worth surfacing, not a reason to omit the class.
	ReclaimCommand string `json:"reclaim_command,omitempty"`
	// ReclaimCommandArgv is the executable template owned by this class. The
	// planner replaces the exact <path> token with an instance path; it never
	// reparses ReclaimCommand.
	ReclaimCommandArgv []string `json:"reclaim_command_argv,omitempty"`
	// Mode is ReclaimAutomatic or ReclaimOperatorGated.
	Mode string `json:"mode"`
	// ReclaimTargetRef names the consumer whose threshold governs reclamation, when one
	// exists. Empty means reclamation is time-based rather than pressure-based.
	ReclaimTargetRef string `json:"reclaim_target_ref,omitempty"`
	// Rebuildable marks a class whose loss costs only time. These are always safe to reclaim
	// first, and a reclaimer should exhaust them before touching anything else.
	Rebuildable bool `json:"rebuildable"`
}

// retentionClasses is the compiled-in declaration set for public-core artifact shapes.
var retentionClasses = []ArtifactClass{
	{
		ID:                 "go-build-cache",
		Owner:              "go toolchain",
		Description:        "Shared Go build cache at the path reported by `go env GOCACHE`.",
		Retention:          "Reclaim under disk pressure; the toolchain rebuilds on demand.",
		ReclaimCommand:     "go clean -cache",
		ReclaimCommandArgv: []string{"go", "clean", "-cache"},
		Mode:               ReclaimAutomatic,
		ReclaimTargetRef:   "consumer:published-threshold",
		Rebuildable:        true,
	},
	{
		ID:                 "worktree-build-cache",
		Owner:              "agent worktrees",
		Description:        "Regenerated build output inside a repository or agent worktree.",
		Retention:          "Reclaim when inactive; source, branches, and commits remain unaffected.",
		ReclaimCommand:     "rm -rf -- <path>",
		ReclaimCommandArgv: []string{"rm", "-rf", "--", "<path>"},
		Mode:               ReclaimAutomatic,
		ReclaimTargetRef:   "consumer:published-threshold",
		Rebuildable:        true,
	},
	{
		ID:    "agent-worktree",
		Owner: "agent worktrees",
		// Named for what it IS, not for the state we hope it is in. An earlier draft called
		// this "stale-worktree", which made the scan report a container holding ACTIVE
		// worktrees — another lane was mid-land in one — as though all of it were reclaimable.
		Description: "An agent worktree, and the containers that hold them (.claude, .codex, .worktrees).",
		// Gated deliberately: containment is checkable, but a clean-looking worktree can still
		// hold uncommitted operator work, and a worktree is not rebuildable.
		Retention:      "Retire an individual worktree once its tip is contained in origin/main and its tree is clean; never reclaim a container wholesale.",
		ReclaimCommand: "compound authority worktree-retire plan|review|apply",
		Mode:           ReclaimOperatorGated,
		Rebuildable:    false,
	},
	{
		ID:          "archived-artifact",
		Owner:       "source adapter",
		Description: "Sealed artifact segments whose authoritative copy has been verified elsewhere.",
		Retention:   "Reclaim only after the owner verifies custody and the configured local retention floor.",
		// The owner executes; compound only names the entrypoint. The command self-gates on a
		// per-day content-hash comparison against the remote and refuses to delete anything when
		// a sealed day is short, so an unverified custody claim cannot reach a deletion.
		// The path is repo-relative on purpose: a leading ~ never expands in an argv (there is
		// no shell to expand it), and an absolute operator path in a compiled-in registry is a
		// machine-specific promise this registry cannot keep. Run it from the compound repo.
		ReclaimCommand:     "scripts/switchback-capture-maintenance.sh --prune --yes",
		ReclaimCommandArgv: []string{"bash", "scripts/switchback-capture-maintenance.sh", "--prune", "--yes"},
		Mode:               ReclaimOperatorGated,
		Rebuildable:        false,
	},
	{
		ID:          "superseded-artifact",
		Owner:       "source adapter",
		Description: "Legacy artifact replaced by a live canonical format.",
		Retention:   "Reclaim after exact backup verification, cutover proof, and rollback-window expiry.",
		Mode:        ReclaimOperatorGated,
		Rebuildable: false,
	},
	{
		ID:          "config-backup",
		Owner:       "operators and adapters",
		Description: "Timestamped config snapshots such as *.pre-<change>-<stamp> next to a live config.",
		Retention:   "Keep the most recent per config; reclaim older ones past their rollback window.",
		Mode:        ReclaimOperatorGated,
		Rebuildable: false,
	},
	{
		ID:                 "stuck-run-attempt",
		Owner:              "compound execution spine",
		Description:        "RunAttempt rows left pending/running by a crashed lane.",
		Retention:          "Terminalize once the owning lane is provably gone.",
		ReclaimCommand:     "compound run-attempt sweep",
		ReclaimCommandArgv: []string{"compound", "run-attempt", "sweep"},
		Mode:               ReclaimAutomatic,
		ReclaimTargetRef:   "",
		Rebuildable:        false,
	},
}

// RetentionClasses returns the declared classes in stable ID order.
func RetentionClasses() []ArtifactClass {
	out := append([]ArtifactClass(nil), retentionClasses...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RetentionClassByID returns one class, or false when nothing declares that ID.
func RetentionClassByID(id string) (ArtifactClass, bool) {
	for _, class := range retentionClasses {
		if class.ID == id {
			return class, true
		}
	}
	return ArtifactClass{}, false
}

// RetentionClassesWithoutMechanism lists declared classes that have no reclaim command.
//
// Missing mechanisms remain visible because declaration without a reclaim path is operationally
// incomplete, even when the class is correctly operator-gated.
func RetentionClassesWithoutMechanism() []ArtifactClass {
	var out []ArtifactClass
	for _, class := range RetentionClasses() {
		if class.ReclaimCommand == "" {
			out = append(out, class)
		}
	}
	return out
}
