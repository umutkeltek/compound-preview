package work

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"compound/internal/landlease"
)

// proofResourceWaitPoll bounds each sleep of a queued reservation, and so how
// long a cancel takes to land; the lease watcher wakes it at once on a release.
const proofResourceWaitPoll = time.Second

// proofResourceBusyError is a queued reservation's quiet refusal: the lease was live
// under another holder when checked under the admission lock, and nothing was minted.
type proofResourceBusyError struct{ holder string }

func (e *proofResourceBusyError) Error() string {
	return fmt.Sprintf("proof resource busy: held by %q", e.holder)
}

// ReserveProofResourceWhenFree queues for a busy proof resource instead of
// refusing. ReserveProofResource mints its RunAttempt before it tries the
// land-lease, so every refusal leaves an abandoned attempt that close must later
// acknowledge. This waits, writing nothing, until no live lease is held, then
// reserves. The lease is checked again under the admission lock before anything
// is minted, because another battery can be mid-reservation (holding that lock,
// lease not yet written) when the lease looks free; that case waits again too.
// Only a lease taken outside a reservation (a manual land-lease acquire) can
// still cost one refused attempt. Each holder it waits on is named on progress, and
// a cancelled ctx stops the wait before anything is reserved.
func (s *Store) ReserveProofResourceWhenFree(
	ctx context.Context,
	workID string,
	in ProofResourceReserveInput,
	wait time.Duration,
	clk landlease.Clock,
	progress io.Writer,
) (ProofResourceReserveResult, error) {
	if in.Steal {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource wait cannot be combined with steal")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx == nil {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource wait requires a context")
	}
	if err := ctx.Err(); err != nil {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource wait: %w", err)
	}
	if clk == nil {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource wait requires a clock")
	}
	in.busyWhenHeld = true
	started := clk.Now()
	var waiting time.Duration
	deadline := started.Add(wait)
	in.waitCheck = func() error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("proof resource admission stopped: %w", err)
		}
		if !clk.Now().Before(deadline) {
			return fmt.Errorf("proof resource admission wait expired: %w", context.DeadlineExceeded)
		}
		return nil
	}
	identity, identityErr := s.proofCoordinationForProfile(in.Profile)
	if identityErr != nil {
		return ProofResourceReserveResult{}, identityErr
	}
	if identity != nil {
		return s.reserveCoordinatedProofWhenFree(ctx, workID, in, identity, clk, deadline, progress)
	}
	announced := ""
	for {
		status, err := landlease.ProofResource(s.home).StatusReadOnly(clk.Now())
		if err == nil && status.Held && status.Holder != announced && progress != nil {
			announced = status.Holder
			fmt.Fprintf(progress, "proof resource busy: waiting for %s (lease expires %s)\n", status.Holder, status.ExpiresAt)
		}
		waitStarted := clk.Now()
		if _, err := landlease.ProofResource(s.home).WaitUntilFree(ctx, clk, deadline.Sub(clk.Now()), proofResourceWaitPoll); err != nil {
			return ProofResourceReserveResult{}, fmt.Errorf("proof resource wait: %w", err)
		}
		waiting += clk.Now().Sub(waitStarted)
		result, err := s.ReserveProofResource(workID, in, clk.Now())
		var busy *proofResourceBusyError
		if errors.As(err, &busy) {
			continue // nothing was minted; the next wait enforces the deadline
		}
		var held *ProofResourceHeldError
		if errors.As(err, &held) && clk.Now().Before(deadline) {
			continue
		}
		if err == nil {
			s.observeProofQueue(result.Reservation.ID, clk.Now().Sub(started), waiting)
		}
		return result, err
	}
}

func checkProofWaitAdmission(in ProofResourceReserveInput) error {
	if in.waitCheck != nil {
		return in.waitCheck()
	}
	return nil
}
