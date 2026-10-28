package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"compound/internal/landlease"
)

// LandingCLISelection names a work-owned landing resource, or the distinct proof resource.
type LandingCLISelection struct {
	WorkID string
	Proof  bool
	Status bool
}

// ResolveLandingCLIResource keeps legacy calls on the legacy API until cutover.
// The legacy API rejects a marker that appears after this check.
func ResolveLandingCLIResource(homePath string, selected LandingCLISelection) (*landlease.Resource, error) {
	if selected.Proof {
		if !selected.Status {
			return nil, fmt.Errorf("--proof selects status only; landing mutations require --work after cutover")
		}
		if strings.TrimSpace(selected.WorkID) != "" {
			return nil, fmt.Errorf("choose --proof or --work, not both")
		}
		return landlease.ProofResource(homePath), nil
	}
	layout, err := landlease.LayoutStatus(homePath)
	if err != nil {
		return nil, err
	}
	if layout.Mode == "legacy" {
		return nil, nil
	}
	if strings.TrimSpace(selected.WorkID) == "" {
		return nil, fmt.Errorf("scoped landing lease requires --work with bound Git delivery")
	}
	store, err := Open(homePath)
	if err != nil {
		return nil, err
	}
	key, err := store.LandingResourceKey(selected.WorkID)
	if err != nil {
		return nil, err
	}
	return landlease.OpenResource(homePath, key)
}

func MutateLandingCLI(homePath, workID, actor string, in LandingResourceMutation, now time.Time) (LandingResourceMutationResult, error) {
	if strings.TrimSpace(actor) == "" {
		return LandingResourceMutationResult{}, fmt.Errorf("scoped landing lease requires --actor holding the work lease")
	}
	in.Actor = strings.TrimSpace(actor)
	store, err := Open(homePath)
	if err != nil {
		return LandingResourceMutationResult{}, err
	}
	return store.MutateLandingResource(strings.TrimSpace(workID), in, now)
}

// AcquireLandingCLI waits outside work admission and rechecks current authority on every attempt.
func AcquireLandingCLI(homePath, workID, actor, holder string, ttl, timeout time.Duration, wait bool) (landlease.Lease, error) {
	if strings.TrimSpace(actor) == "" {
		return landlease.Lease{}, fmt.Errorf("scoped landing lease requires --actor holding the work lease")
	}
	in := LandingResourceMutation{Holder: holder, Operation: "acquire", TTL: ttl}
	if !wait {
		result, err := MutateLandingCLI(homePath, workID, actor, in, time.Now())
		return result.Lease, err
	}
	resource, err := ResolveLandingCLIResource(homePath, LandingCLISelection{WorkID: workID})
	if err != nil {
		return landlease.Lease{}, err
	}
	if resource == nil {
		return landlease.Lease{}, fmt.Errorf("scoped landing resource is unavailable")
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return landlease.Lease{}, fmt.Errorf("scoped landing lease wait timed out")
		}
		if _, err := resource.WaitUntilFree(context.Background(), landlease.RealClock{}, remaining, 2*time.Second); err != nil {
			return landlease.Lease{}, err
		}
		result, err := MutateLandingCLI(homePath, workID, actor, in, time.Now())
		if err == nil {
			return result.Lease, nil
		}
		var held *landlease.HeldError
		if !errors.As(err, &held) || time.Now().After(deadline) {
			return landlease.Lease{}, err
		}
	}
}

// LandingCLIRefusal is renderer-neutral data for a refused CLI operation.
type LandingCLIRefusal struct {
	Command, Gate, Refused, Prevents string
	Tried, DoThis                    []string
}

func refusalValue(value, placeholder string) string {
	if strings.TrimSpace(value) == "" {
		return placeholder
	}
	return value
}

func refusalScope(workID, actor string) (statusFlags, mutationFlags string) {
	if strings.TrimSpace(workID) == "" {
		return "", ""
	}
	statusFlags = fmt.Sprintf(" --work %q", workID)
	mutationFlags = statusFlags
	if strings.TrimSpace(actor) != "" {
		mutationFlags += fmt.Sprintf(" --actor %q", actor)
	}
	return statusFlags, mutationFlags
}

func LandingContentionRefusal(holder string, held *landlease.HeldError, waited bool, observed error, workID, actor string) LandingCLIRefusal {
	me := refusalValue(holder, "<holder>")
	statusFlags, mutationFlags := refusalScope(workID, actor)
	attempt := fmt.Sprintf("acquire for %s: %v", me, observed)
	if waited {
		attempt = fmt.Sprintf("acquire --wait for %s: %v", me, observed)
	}
	return LandingCLIRefusal{
		Command: "compound land-lease acquire",
		Gate:    "land_lease_contention",
		Refused: fmt.Sprintf("the land-lease is held by %q, so %q did not take it", held.Holder, me),
		Tried:   []string{attempt, "transfer it anyway: not attempted — acquire never takes over a LIVE lease, only steal does"},
		DoThis: []string{
			"compound land-lease status" + statusFlags + "   # the holder, remaining seconds, and any prior steal on the record",
			fmt.Sprintf("compound land-lease acquire --holder %s%s --wait --timeout 120   # block until the holder finishes or the window elapses", me, mutationFlags),
			fmt.Sprintf("compound land-lease steal --holder %s%s --reason \"<why the holder cannot finish>\"   # records prior_holder + reason; only when the holder is NOT mid-ceremony", me, mutationFlags),
		},
		Prevents: "two lanes landing to main inside the same window. The lease serializes the landing act; taking it from a live holder without a recorded reason turns a merge collision into one with no trail back to who decided to override whom.",
	}
}

func LandingHolderRefusal(verb, holder string, st landlease.StatusResult, workID, actor string) LandingCLIRefusal {
	me := refusalValue(holder, "<holder>")
	statusFlags, mutationFlags := refusalScope(workID, actor)
	return LandingCLIRefusal{
		Command: "compound land-lease " + verb,
		Gate:    "land_lease_holder",
		Refused: fmt.Sprintf("%q does not hold the land-lease, so %s left %q's lease in place", me, verb, st.Holder),
		Tried: []string{
			fmt.Sprintf("--holder %s against the recorded holder %q: not the holder", me, st.Holder),
			fmt.Sprintf("current lease: holder=%s held=%v expired=%v remaining=%ds", st.Holder, st.Held, st.IsExpired, st.RemainingSeconds),
		},
		DoThis: []string{
			"compound land-lease status" + statusFlags + "   # confirm who holds it and how long is left",
			fmt.Sprintf("compound land-lease release --holder %s%s   # the holder's own release is the ordinary path", refusalValue(st.Holder, "<current-holder>"), mutationFlags),
			fmt.Sprintf("compound land-lease steal --holder %s%s --reason \"<why the holder cannot finish>\"   # the auditable takeover, when the holder cannot release it themselves", me, mutationFlags),
		},
		Prevents: "one lane silently dropping another lane's lease. A release that ignored the holder would let a lane that never landed anything clear the mutex out from under a lane mid-ceremony, with nothing recorded to say it happened.",
	}
}

func LandingStealAuditRefusal(holder string, st landlease.StatusResult, workID, actor string) LandingCLIRefusal {
	me := refusalValue(holder, "<holder>")
	statusFlags, mutationFlags := refusalScope(workID, actor)
	current := "nobody holds it right now"
	if st.Holder != "" {
		current = fmt.Sprintf("%q holds it (held=%v expired=%v remaining=%ds)", st.Holder, st.Held, st.IsExpired, st.RemainingSeconds)
	}
	return LandingCLIRefusal{
		Command: "compound land-lease steal",
		Gate:    "land_lease_steal_audit",
		Refused: fmt.Sprintf("no --reason was given, so the lease was not transferred to %q", me),
		Tried:   []string{"--reason: not passed (or empty)", "current lease: " + current},
		DoThis: []string{
			fmt.Sprintf("compound land-lease steal --holder %s%s --reason \"<why the current holder cannot finish>\"   # the reason lands on the record as prior_holder + reason + stolen_at", me, mutationFlags),
			fmt.Sprintf("compound land-lease acquire --holder %s%s   # if the lease is free or already expired, this is the right verb — a steal is not needed", me, mutationFlags),
			"compound land-lease status" + statusFlags + "   # check whether the lease already aged out before overriding a live holder",
		},
		Prevents: "an unattributable takeover. steal is the ONLY path that overrides a live holder; without a recorded reason the audit trail cannot distinguish a justified override from a lane that simply did not wait.",
	}
}

// RunLandingStatus resolves and renders the same owner-selected resource for CLI consumers.
func RunLandingStatus(homePath string, selected LandingCLISelection, asJSON bool, stdout, stderr io.Writer, now time.Time) int {
	resource, err := ResolveLandingCLIResource(homePath, selected)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if selected.Proof {
		generation, generationErr := landlease.ProofCoordinationGeneration(homePath)
		if generationErr != nil {
			fmt.Fprintln(stderr, "error:", generationErr)
			return 1
		}
		if generation != "" {
			rows, err := landlease.ProofResourceStatuses(homePath, now)
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			if asJSON {
				if err := json.NewEncoder(stdout).Encode(struct {
					Generation string                       `json:"generation"`
					Resources  []landlease.ProofClassStatus `json:"resources"`
				}{generation, rows}); err != nil {
					fmt.Fprintln(stderr, "error:", err)
					return 1
				}
			} else {
				for _, row := range rows {
					fmt.Fprintf(stdout, "proof %s[%d]: holder=%s held=%v expired=%v expires=%s\n", row.Class, row.Slot, row.Status.Holder, row.Status.Held, row.Status.IsExpired, row.Status.ExpiresAt)
				}
			}
			return 0
		}
	}
	var status landlease.StatusResult
	if resource == nil {
		status, err = landlease.StatusReadOnly(homePath, now)
	} else {
		status, err = resource.StatusReadOnly(now)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(status); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	label := "land-lease"
	if selected.Proof {
		label = "proof resource"
	} else if selected.WorkID != "" {
		label = "landing resource for " + selected.WorkID
	}
	if status.Holder == "" {
		fmt.Fprintf(stdout, "%s: none held\n", label)
		return 0
	}
	fmt.Fprintf(stdout, "%s: holder=%s held=%v expired=%v remaining=%ds expires=%s\n", label, status.Holder, status.Held, status.IsExpired, status.RemainingSeconds, status.ExpiresAt)
	if status.PriorHolder != "" {
		fmt.Fprintf(stdout, "  (stolen from %s at %s: %s)\n", status.PriorHolder, status.StolenAt, status.StealReason)
	}
	return 0
}
