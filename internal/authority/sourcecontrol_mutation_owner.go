package authority

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"compound/internal/work"
)

// validateSourceControlMutationOwner guards mutation-bound dirty-plan creation.
// Cleanup-only evidence never enters this door. A review owner must be a real,
// current WorkEnvelope so immutable historical authorizations cannot be minted
// under a synthetic session correlation id or already-finished work.
func ValidateSourceControlMutationOwner(homePath, ownerWorkID string) error {
	ownerWorkID = strings.TrimSpace(ownerWorkID)
	if ownerWorkID == "" {
		return fmt.Errorf("durable owner work_id required for source-control mutation review")
	}
	if strings.HasPrefix(ownerWorkID, "work_live_") {
		return fmt.Errorf("durable owner work_id %q must not be synthetic work_live_*", ownerWorkID)
	}
	if !strings.HasPrefix(ownerWorkID, "work_") {
		return fmt.Errorf("durable owner work_id %q must be a work_ id", ownerWorkID)
	}
	store, err := work.Open(homePath)
	if err != nil {
		return fmt.Errorf("open durable owner work store: %w", err)
	}
	owner, err := store.Get(ownerWorkID)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("durable owner work_id %q does not exist", ownerWorkID)
	}
	if err != nil {
		return fmt.Errorf("read durable owner work_id %q: %w", ownerWorkID, err)
	}
	if !SourceControlMutationOwnerCurrent(ownerWorkID, owner.State) {
		return fmt.Errorf("durable owner work_id %q must be claimed|running|verifying|held, got %q", ownerWorkID, owner.State)
	}
	return nil
}
