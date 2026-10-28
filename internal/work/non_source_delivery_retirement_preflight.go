package work

import (
	"compound/internal/authority/gitrepository"
	"fmt"
	"path/filepath"
	"strings"
)

// PreflightRetirementRecoveryInventory reads the current recovery inventory
// within capture's existing bounds and verifies its sealed proofs. It does not
// custody bytes or guarantee a later capture if the files change.
func PreflightRetirementRecoveryInventory(plan gitrepository.WorktreeRetirementPlan) error {
	return readRetirementRecoveryInventory(plan, nil)
}

func readRetirementRecoveryInventory(plan gitrepository.WorktreeRetirementPlan, retain func(string, []byte)) error {
	proofs := plan.RecoveryProofs
	if len(proofs) == 0 || len(proofs) > maxPrivateRuntimeDeliveryFiles || len(proofs) != len(plan.RecoveryRefs) {
		return fmt.Errorf("retirement recovery inventory out of bounds")
	}
	remaining := int64(privateDeliveryCaptureTotalBytes)
	for i, proof := range proofs {
		if proof.Kind != "sha256-file" || proof.Reference != plan.RecoveryRefs[i] ||
			!gitrepository.ValidRetirementDigest(proof.Digest) || !filepath.IsAbs(proof.Path) || filepath.Clean(proof.Path) != proof.Path ||
			proof.Reference != "sha256-file:"+strings.TrimPrefix(proof.Digest, "sha256:")+":"+proof.Path {
			return fmt.Errorf("retirement recovery proof is not an exact supported file")
		}
		body, err := readDeliveryCaptureFile(proof.Path, min(remaining, privateDeliveryBodyLimit))
		if err != nil {
			return err
		}
		// Charge every proof before optional content-addressed custody deduplication.
		remaining -= int64(len(body))
		ref := privateDeliveryArtifactRef(body)
		if ref != "artifact:"+proof.Digest {
			return fmt.Errorf("retirement recovery bytes differ from sealed proof")
		}
		if retain != nil {
			retain(ref, body)
		}
	}
	return nil
}
