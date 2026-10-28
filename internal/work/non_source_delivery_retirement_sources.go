package work

import (
	"fmt"
	"path/filepath"

	"compound/internal/authority/gitrepository"
)

func readRetirementDeliverySnapshot(request WorktreeRetirementDeliveryCaptureRequest) (retirementDeliverySnapshot, error) {
	out := retirementDeliverySnapshot{Bodies: map[string][]byte{}}
	read := func(path string) ([]byte, error) { return readDeliveryCaptureFile(path, privateDeliveryMetadataLimit) }
	planBody, err := read(request.PlanPath)
	if err != nil {
		return out, err
	}
	authorizationBody, err := read(request.ReviewReportPath)
	if err != nil {
		return out, err
	}
	var authorization gitrepository.WorktreeRetirementReviewReport
	if err := decodeDeliveryCapture(authorizationBody, &authorization); err != nil {
		return out, err
	}
	if filepath.Clean(request.PlanPath) != filepath.Clean(authorization.PlanPath) || filepath.Clean(request.ReviewReportPath) != filepath.Clean(authorization.ReviewPath) {
		return out, fmt.Errorf("capture paths differ from original retirement authority")
	}
	sourceBody, err := read(authorization.SourcePath)
	if err != nil {
		return out, err
	}
	resultBody, err := read(request.ResultPath)
	if err != nil {
		return out, err
	}
	out.Delivery, err = gitrepository.ValidateRetirementDelivery(planBody, authorizationBody, sourceBody, resultBody)
	if err != nil {
		return out, err
	}
	if filepath.Clean(request.ResultPath) != filepath.Clean(out.Delivery.Result.ResultPath) {
		return out, fmt.Errorf("capture result path differs from original result")
	}
	out.Evidence = retirementDeliveryEvidence{SchemaVersion: retirementDeliveryEvidenceSchema,
		WorkID: out.Delivery.Plan.OwnerProof.WorkID, ActionIntentID: out.Delivery.Result.ActionIntentID,
		PlanRef: privateDeliveryArtifactRef(planBody), AuthorizationRef: privateDeliveryArtifactRef(authorizationBody),
		ReviewedSourceRef: privateDeliveryArtifactRef(sourceBody), ResultRef: privateDeliveryArtifactRef(resultBody)}
	for _, body := range [][]byte{planBody, authorizationBody, sourceBody, resultBody} {
		out.Bodies[privateDeliveryArtifactRef(body)] = body
	}
	err = readRetirementRecoveryInventory(out.Delivery.Plan, func(ref string, body []byte) {
		out.Bodies[ref] = body
		out.Evidence.RecoveryArtifacts = append(out.Evidence.RecoveryArtifacts, ref)
	})
	return out, err
}
