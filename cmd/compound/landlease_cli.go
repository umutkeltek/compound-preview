package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
)

// cmdLandLease is the operator/orchestrator door onto the land-lease — the single-holder, TTL-bounded,
// steal-with-reason advisory MUTEX over the ACT of landing to main (a flock-guarded JSON lease under
// COMPOUND_HOME). Concurrent executor lanes acquire it before their ff-merge so they serialize the
// landing window instead of colliding. It delegates to runLandLease, which returns an exit code (never
// calls os.Exit) so the refusal/timeout paths are exercised by tests.
func cmdLandLease(args []string) {
	if code := runLandLease(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func landLeaseUsage() string {
	return "usage:\n" +
		"  compound land-lease acquire --holder H [--work work_ID --actor A] [--ttl S] [--wait] [--timeout S] [--home P] [--json]\n" +
		"    take (or refresh) the land-lease for holder H (default ttl 300s). Without --wait, a live lease\n" +
		"    held by a DIFFERENT holder exits non-zero with the current holder printed; --wait polls until the\n" +
		"    lease is free or --timeout (default 120s) elapses. A steal never happens here — only `steal` does.\n" +
		"  compound land-lease release --holder H [--work work_ID --actor A] [--home P] [--json]\n" +
		"    free the lease ONLY if H holds it; a non-holder release is refused (someone else's lease is never\n" +
		"    dropped), and a release with nothing held is an idempotent no-op\n" +
		"  compound land-lease status [--work work_ID|--proof] [--home P] [--json]\n" +
		"    show the current holder, acquired_at, expires_at, remaining seconds, and is_expired\n" +
		"  compound land-lease steal --holder H --reason R [--work work_ID --actor A] [--home P] [--json]\n" +
		"    transfer the lease to H and RECORD prior_holder + reason + stolen_at (auditable). A reason is\n" +
		"    REQUIRED — this is the only path that takes over a LIVE lease.\n" +
		"  compound land-lease orphan-plan --work W --reservation pfr_ID [--home P]\n" +
		"    read-only exact plan for a failed orphan proof-completion journal.\n" +
		"  compound land-lease orphan-apply --work W --plan FILE --actor A --operator operator:ID --evidence REF --writers-paused --expires RFC3339 [--home P]\n" +
		"    retire only the approved exact orphan journal under a current writer pause.\n" +
		" compound land-lease migrate --work W --actor A --operator operator:ID --evidence REF --writers-paused --expires RFC3339 [--backing-home P]\n" +
		" cut over after governed writer quiescence and legacy resource validation.\n" +
		"after scoped cutover, landing verbs require --work and --actor; holder includes exact work_id as a colon-delimited component. The key comes from bound Git delivery; status --proof selects the separate proof resource.\n" +
		"the land-lease is a COORDINATION HINT (it records who holds the right to land); it never mutates repo\n" +
		"state and the compound-land ceremony is the consumer that acts on it. No provider/network calls."
}

func runLandLease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, landLeaseUsage())
		return 2
	}
	switch args[0] {
	case "acquire":
		return runLandLeaseAcquire(args[1:], stdout, stderr)
	case "release":
		return runLandLeaseRelease(args[1:], stdout, stderr)
	case "status":
		return runLandLeaseStatus(args[1:], stdout, stderr)
	case "steal":
		return runLandLeaseSteal(args[1:], stdout, stderr)
	case "migrate":
		return work.RunResourceMigrationCLI(args[1:], stdout, stderr)
	case "orphan-plan", "orphan-apply":
		return work.RunOrphanProofRetirementCLI(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, landLeaseUsage())
		return 2
	}
}

func writeLandingRefusal(out io.Writer, refusal work.LandingCLIRefusal) {
	gateRefusal{
		Command:  refusal.Command,
		Gate:     refusal.Gate,
		Refused:  refusal.Refused,
		Tried:    refusal.Tried,
		DoThis:   refusal.DoThis,
		Prevents: refusal.Prevents,
	}.writeTo(out)
}

// secondsFlag reads an integer-seconds flag, returning fallback when absent.
func secondsFlag(args []string, name string, fallback int) (time.Duration, error) {
	raw := flagValue(args, name)
	if raw == "" {
		return time.Duration(fallback) * time.Second, nil
	}
	n, err := intFlag(args, name)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

func landingLeaseResource(homePath string, args []string, status bool) (*landlease.Resource, error) {
	return work.ResolveLandingCLIResource(homePath, work.LandingCLISelection{
		WorkID: strings.TrimSpace(flagValue(args, "--work")),
		Proof:  hasFlag(args, "--proof"),
		Status: status,
	})
}

func mutateScopedLanding(homePath string, args []string, in work.LandingResourceMutation, now time.Time) (work.LandingResourceMutationResult, error) {
	return work.MutateLandingCLI(homePath, flagValue(args, "--work"), flagValue(args, "--actor"), in, now)
}

func runLandLeaseAcquire(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	resource, err := landingLeaseResource(homePath, args, false)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	holder := flagValue(args, "--holder")
	ttl, err := secondsFlag(args, "--ttl", landlease.DefaultTTLSeconds)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	var lease landlease.Lease
	if hasFlag(args, "--wait") {
		timeout, tErr := secondsFlag(args, "--timeout", 120)
		if tErr != nil {
			fmt.Fprintln(stderr, "error:", tErr)
			return 1
		}
		if resource == nil {
			lease, err = landlease.AcquireWait(homePath, holder, ttl, landlease.RealClock{}, timeout, 2*time.Second)
		} else {
			lease, err = work.AcquireLandingCLI(homePath, flagValue(args, "--work"), flagValue(args, "--actor"), holder, ttl, timeout, true)
		}
	} else {
		if resource == nil {
			lease, err = landlease.Acquire(homePath, holder, ttl, time.Now())
		} else {
			lease, err = work.AcquireLandingCLI(homePath, flagValue(args, "--work"), flagValue(args, "--actor"), holder, ttl, 0, false)
		}
	}
	if err != nil {
		// Contention is the ONE outcome here that is a gate rather than a fault: another lane
		// legitimately holds the right to land. It is typed (*landlease.HeldError, which
		// AcquireWait also wraps on timeout), so the refusal is selected by the error's kind
		// and never by matching its text. Anything else -- a bad ttl, an unreadable home -- is
		// a plain error and stays one.
		var held *landlease.HeldError
		if errors.As(err, &held) {
			writeLandingRefusal(stderr, work.LandingContentionRefusal(holder, held, hasFlag(args, "--wait"), err, flagValue(args, "--work"), flagValue(args, "--actor")))
			return 1
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, lease); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "acquired land-lease: holder=%s ttl=%ds expires=%s\n", lease.Holder, lease.TTLSeconds, lease.ExpiresAt)
	return 0
}

func runLandLeaseRelease(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	resource, err := landingLeaseResource(homePath, args, false)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	holder := flagValue(args, "--holder")
	var res landlease.ReleaseResult
	if resource == nil {
		res, err = landlease.Release(homePath, holder, time.Now())
	} else {
		var result work.LandingResourceMutationResult
		result, err = mutateScopedLanding(homePath, args, work.LandingResourceMutation{Holder: holder, Operation: "release"}, time.Now())
		res = result.Release
	}
	if err != nil {
		// Release refuses exactly one thing on purpose: dropping a lease the caller does not
		// hold. Which case fired is decided by re-reading the lease, not by matching Release's
		// error text -- a non-holder release and an unreadable home must not share a remedy.
		var st landlease.StatusResult
		var sErr error
		if resource == nil {
			st, sErr = landlease.Status(homePath, time.Now())
		} else {
			st, sErr = resource.Status(time.Now())
		}
		if sErr == nil && st.Holder != "" && st.Holder != strings.TrimSpace(holder) {
			writeLandingRefusal(stderr, work.LandingHolderRefusal("release", holder, st, flagValue(args, "--work"), flagValue(args, "--actor")))
			return 1
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, res); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s (freed=%v)\n", res.Message, res.Freed)
	return 0
}

func runLandLeaseStatus(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return work.RunLandingStatus(homePath, work.LandingCLISelection{
		WorkID: strings.TrimSpace(flagValue(args, "--work")), Proof: hasFlag(args, "--proof"), Status: true,
	}, hasFlag(args, "--json"), stdout, stderr, time.Now())
}

func runLandLeaseSteal(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	resource, err := landingLeaseResource(homePath, args, false)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	holder := flagValue(args, "--holder")
	// The reason is the whole difference between a steal and a silent takeover: it is what
	// makes the transfer auditable afterwards. Checked here, before Steal, so the refusal can
	// name the alternative (acquire, for an uncontested lease) rather than repeat the rule.
	if strings.TrimSpace(flagValue(args, "--reason")) == "" && strings.TrimSpace(holder) != "" {
		var st landlease.StatusResult
		if resource == nil {
			st, _ = landlease.Status(homePath, time.Now())
		} else {
			st, _ = resource.Status(time.Now())
		}
		writeLandingRefusal(stderr, work.LandingStealAuditRefusal(holder, st, flagValue(args, "--work"), flagValue(args, "--actor")))
		return 1
	}
	var lease landlease.Lease
	if resource == nil {
		lease, err = landlease.Steal(homePath, holder, flagValue(args, "--reason"), time.Now())
	} else {
		var result work.LandingResourceMutationResult
		result, err = mutateScopedLanding(homePath, args, work.LandingResourceMutation{Holder: holder, Operation: "steal", Reason: flagValue(args, "--reason")}, time.Now())
		lease = result.Lease
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, lease); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "stole land-lease: holder=%s prior=%s reason=%q expires=%s\n",
		lease.Holder, lease.PriorHolder, lease.StealReason, lease.ExpiresAt)
	return 0
}
