package landlease

import (
	"fmt"
	"strings"
	"time"
)

// Historical observation checks the original capacity/repository barriers even
// when a different holder legitimately occupies that class. Exact-holder
// mutations (refresh, release, rearm, termination request) keep exact binding.
func (r *Resource) historicalBinding(root, holder string) (*leaseBinding, error) {
	if strings.TrimSpace(holder) == "" {
		return nil, fmt.Errorf("historical holder required")
	}
	b := r.binding(root, false)
	b.historicalHolder = holder
	return b, nil
}
func (r *Resource) ObserveHistoricalHolder(holder string, now time.Time) (StatusResult, error) {
	return selectedReadCall(r, func(root string) (StatusResult, error) {
		binding, err := r.historicalBinding(root, holder)
		if err != nil {
			return StatusResult{}, err
		}
		return statusReadOnlyBound(root, now, binding)
	})
}
func (r *Resource) HistoricalExecutionPending(holder string) (bool, error) {
	return selectedReadCall(r, func(root string) (bool, error) {
		binding, err := r.historicalBinding(root, holder)
		if err != nil {
			return false, err
		}
		return executionPendingBound(root, holder, binding)
	})
}
func (r *Resource) AttestHistoricalExecutionTerminated(holder string) (ExecutionTerminationAttestation, error) {
	return selectedReadCall(r, func(root string) (ExecutionTerminationAttestation, error) {
		binding, err := r.historicalBinding(root, holder)
		if err != nil {
			return ExecutionTerminationAttestation{}, err
		}
		return attestExecutionTerminatedBound(root, holder, binding)
	})
}

// Try variants preserve exact checks while returning a queue retry instead of
// waiting for a class finalizer inside an admission callback.
func (r *Resource) TryAttestHistoricalExecutionTerminated(holder string) (ExecutionTerminationAttestation, error) {
	return selectedReadCall(r, func(root string) (ExecutionTerminationAttestation, error) {
		binding, err := r.historicalBinding(root, holder)
		if err != nil {
			return ExecutionTerminationAttestation{}, err
		}
		binding.nonblocking = r.proofIdentity != nil
		return attestExecutionTerminatedBound(root, holder, binding)
	})
}
func (r *Resource) TryRequestExecutionTermination(holder, requestedBy, reason string, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		binding := r.binding(root, false)
		binding.nonblocking = r.proofIdentity != nil
		return requestExecutionTerminationBound(root, holder, requestedBy, reason, now, binding)
	})
}
