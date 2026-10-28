package sourceobs

import (
	"fmt"
	"os"
	"time"

	"compound/internal/candidates"
)

// CanonDriftReport is what ObserveCanonDrift returns: the set of canon files whose live bytes drifted
// from the stored baseline, the capability-graph node refs those files carry, the no-promote
// candidate/decision ids parked for the drift, and the manifest path whose baseline the caller
// commits (via CommitCanonBaseline) once the drift has been recorded.
type CanonDriftReport struct {
	ChangedRefs    []string             `json:"changed_refs"`
	NodeRefs       []string             `json:"node_refs"`
	CandidateIDs   []string             `json:"candidate_ids"`
	NoPromoteIDs   []string             `json:"no_promote_ids,omitempty"`
	AdmissionHolds []CanonAdmissionHold `json:"admission_holds,omitempty"`
	ManifestPath   string               `json:"manifest_path"`
}

// ObserveCanonDrift is the harness-canon Importer — the first half of a two-phase drift observation.
// It loads the stored source-manifest baseline for a family, re-hashes each item's live file, and for
// every changed file parks exactly one CapabilityObs candidate (PromoteAllowed=false => auto
// no-promote) via the existing candidate writers. It NEVER mints a Transition — that is the
// ObjectFamily's job (capgraph.RecordCanonEvolution) — and, unlike the earlier design, it NO LONGER
// re-stores the baseline. It returns the rebaselined manifest (changed items advanced to their live
// hashes) so the caller commits it via CommitCanonBaseline ONLY after the drift has been recorded (a
// successful mint or an explicit observation-only run). This makes drift transaction-safe: a mint that
// fails after observing leaves the stored baseline untouched, so the identical drift stays
// re-detectable. Candidate ids are content-deterministic (they carry the live hash), so re-observing
// the same drift overwrites the same candidate file and re-uses the active no-promote decision rather
// than spraying duplicates. An unchanged family parks nothing and returns the manifest as-loaded.
func ObserveCanonDrift(homePath, family string, now time.Time) (CanonDriftReport, SourceManifest, error) {
	if now.IsZero() {
		now = time.Now()
	}
	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		return CanonDriftReport{}, SourceManifest{}, fmt.Errorf("canon-observe: load manifest %q: %w", family, err)
	}
	report := CanonDriftReport{ManifestPath: SourceManifestPath(homePath, family)}
	seenNode := map[string]bool{}
	for i := range manifest.Items {
		item := &manifest.Items[i]
		liveHash, herr := hashFile(item.SourceRef)
		if herr != nil {
			if surface := instructionSupplyChainSurface(item.StructureOverlays); surface != "" {
				report.AdmissionHolds = append(report.AdmissionHolds, CanonAdmissionHold{
					SourceRef:   item.SourceRef,
					Surface:     surface,
					Reason:      "instruction surface unavailable",
					PriorSchema: instructionSupplyChainSchema(item.StructureOverlays),
				})
				report.ChangedRefs = append(report.ChangedRefs, item.SourceRef)
				for _, ref := range item.CompoundEntityRefs {
					if ref == "" || seenNode[ref] {
						continue
					}
					seenNode[ref] = true
					report.NodeRefs = append(report.NodeRefs, ref)
				}
			}
			// An unreadable canon file is not counted as drift in v1 (a transient read error must
			// not fabricate a candidate); the baseline stays and doctor surfaces staleness instead.
			continue
		}
		if liveHash == item.FileHash {
			continue
		}
		liveData, liveReadErr := os.ReadFile(item.SourceRef)
		var liveOverlays []SourceStructureOverlay
		var liveStructureErr error
		if liveReadErr != nil {
			liveStructureErr = liveReadErr
		} else {
			liveOverlays, liveStructureErr = instructionSupplyChainOverlays(item.SourceRef, liveData)
		}
		admissionHold := compareInstructionSupplyChainStructure(
			item.SourceRef,
			item.StructureOverlays,
			liveOverlays,
			liveStructureErr,
		)
		obs := canonDriftObservation(family, *item, item.FileHash, liveHash, now)
		applied, err := Apply(homePath, obs, now)
		if err != nil {
			return report, manifest, fmt.Errorf("canon-observe: park drift for %s: %w", item.SourceRef, err)
		}
		report.CandidateIDs = append(report.CandidateIDs, applied.CandidateIDs...)
		report.NoPromoteIDs = append(report.NoPromoteIDs, applied.NoPromoteIDs...)
		report.ChangedRefs = append(report.ChangedRefs, item.SourceRef)
		for _, ref := range item.CompoundEntityRefs {
			if ref == "" || seenNode[ref] {
				continue
			}
			seenNode[ref] = true
			report.NodeRefs = append(report.NodeRefs, ref)
		}
		if admissionHold != nil {
			report.AdmissionHolds = append(report.AdmissionHolds, *admissionHold)
			continue
		}
		item.FileHash = liveHash
		item.SnapshotTime = now.UTC().Format(time.RFC3339)
		item.EvidenceRefs = []string{"source:" + item.SourceRef, liveHash}
		if instructionSupplyChainSurface(item.StructureOverlays) != "" {
			retained := make([]SourceStructureOverlay, 0, len(item.StructureOverlays)+len(liveOverlays))
			for _, overlay := range item.StructureOverlays {
				if overlay.Kind == instructionSupplyChainSurfaceOverlay ||
					overlay.Kind == instructionSupplyChainSchemaOverlay ||
					overlay.Kind == instructionSupplyChainSchemaRevisionOverlay {
					continue
				}
				retained = append(retained, overlay)
			}
			item.StructureOverlays = append(retained, liveOverlays...)
		}
	}
	return report, manifest, nil
}

// CommitCanonBaseline re-stores the rebaselined manifest returned by ObserveCanonDrift, consuming the
// observed drift. The command calls it ONLY after the drift has been recorded — a successful
// RecordCanonEvolution mint, or an explicit observation-only run with no cause flags — so a failed
// mint never consumes the drift it could not record. It is a thin wrapper over RecordSourceManifest.
func CommitCanonBaseline(homePath string, manifest SourceManifest) error {
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		return fmt.Errorf("canon-observe: commit baseline: %w", err)
	}
	return nil
}

// canonDriftObservation shapes one drifted canon file as the generic SourceObservation contract so
// the standard candidate/no-promote writers apply. The mapped node is capability/no-promote evidence
// only; a canon file never promotes itself into authority.
func canonDriftObservation(family string, item SourceManifestItem, priorHash, liveHash string, now time.Time) SourceObservation {
	nodeRef := ""
	if len(item.CompoundEntityRefs) > 0 {
		nodeRef = item.CompoundEntityRefs[0]
	}
	entityHint := firstNonEmpty(nodeRef, item.SourceRef)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "harness_canon", family, item.SourceRef, liveHash),
		AdapterID:     "compound.harness-canon",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        item.SourceRef,
			Kind:        firstNonEmpty(item.SourceKind, "harness-canon-file"),
			Owner:       firstNonEmpty(nodeRef, item.SourceRef),
			SourceClass: "harness-canon-file",
			Revision:    liveHash,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "harness_canon_file",
			Consumer:    "compound source canon-observe",
			Generator:   "compound.harness-canon",
			Marker:      SourceManifestSchemaVersion,
			Freshness:   "drift",
			DriftReason: fmt.Sprintf("file_hash %s -> %s", priorHash, liveHash),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     entityHint,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: []string{"source:" + item.SourceRef + "#" + liveHash, priorHash},
	}
}
