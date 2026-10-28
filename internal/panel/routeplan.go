package panel

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/routeevidence"
)

const (
	lensCapabilityClass  = "classify-extract"
	judgeCapabilityClass = "judge-critique"
)

type LanePlan = RoutePlan

type ErrBlockedNoLane struct {
	Class string
}

func (e ErrBlockedNoLane) Error() string {
	class := strings.TrimSpace(e.Class)
	if class == "" {
		class = "unknown"
	}
	return fmt.Sprintf("blocked_no_lane:%s", class)
}

func SelectLanes(homePath string, privacySensitive bool, supervised bool, now time.Time) (LanePlan, error) {
	opts := routeevidence.CapabilityRoutePlanOptions{PrivacySensitive: privacySensitive}
	lensPlan, err := routeevidence.PlanCapabilityRoute(homePath, lensCapabilityClass, opts, now)
	if err != nil {
		return LanePlan{}, err
	}
	judgePlan, err := routeevidence.PlanCapabilityRoute(homePath, judgeCapabilityClass, opts, now)
	if err != nil {
		return LanePlan{}, err
	}

	plan := LanePlan{
		AuthorityRef: routeevidence.ModelCapabilitiesAuthorityRef,
		LensPlanID:   lensPlan.ID,
		JudgePlanID:  judgePlan.ID,
	}
	var lensOK bool
	for _, lane := range lensPlan.Chain {
		if lane.GateRole == "eligible" {
			plan.LensLane = laneRef(lane)
			lensOK = true
			break
		}
		plan.OmittedLanes = appendOmittedLane(plan.OmittedLanes, lane.ModelID, firstNonEmpty(lane.OmitReason, "lane not eligible for lens"))
	}
	for _, lane := range lensPlan.Omitted {
		plan.OmittedLanes = appendOmittedLane(plan.OmittedLanes, lane.ModelID, firstNonEmpty(lane.OmitReason, "lane omitted from lens plan"))
	}
	if !lensOK {
		return plan, ErrBlockedNoLane{Class: lensCapabilityClass}
	}

	var judgeCandidates []routeevidence.CapabilityRouteLane
	for _, lane := range judgePlan.Chain {
		if lane.Tier != "free" && (lane.GateRole == "eligible" || (supervised && lane.GateRole == "supervised_only")) {
			judgeCandidates = append(judgeCandidates, lane)
			continue
		}
		reason := firstNonEmpty(lane.OmitReason, judgeLaneOmitReason(lane, supervised))
		if lane.Tier == "free" || lane.GateRole == "tripwire_only" {
			reason = "free lane may not judge"
		}
		plan.OmittedLanes = appendOmittedLane(plan.OmittedLanes, lane.ModelID, reason)
	}
	// Prefer a judge model distinct from the lens model: a model must not grade its own findings.
	var judgeOK bool
	for _, lane := range judgeCandidates {
		if lane.ModelID != plan.LensLane.ModelID {
			plan.JudgeLane = laneRef(lane)
			judgeOK = true
			break
		}
	}
	if !judgeOK && len(judgeCandidates) > 0 {
		// Self-judgment unavoidable on this chain; ExecuteDryRun records the guardrail.
		plan.JudgeLane = laneRef(judgeCandidates[0])
		judgeOK = true
	} else if judgeOK {
		for _, lane := range judgeCandidates {
			if lane.ModelID == plan.LensLane.ModelID && lane.ModelID != plan.JudgeLane.ModelID {
				plan.OmittedLanes = appendOmittedLane(plan.OmittedLanes, lane.ModelID, "same model as lens lane; distinct judge preferred")
			}
		}
	}
	for _, lane := range judgePlan.Omitted {
		plan.OmittedLanes = appendOmittedLane(plan.OmittedLanes, lane.ModelID, firstNonEmpty(lane.OmitReason, "lane omitted from judge plan"))
	}
	if !judgeOK {
		return plan, ErrBlockedNoLane{Class: judgeCapabilityClass}
	}
	return plan, nil
}

func laneRef(lane routeevidence.CapabilityRouteLane) LaneRef {
	return LaneRef{ModelID: lane.ModelID, Resolution: lane.Resolution, Tier: lane.Tier, GateRole: lane.GateRole}
}

func appendOmittedLane(omitted []OmittedLane, modelID, reason string) []OmittedLane {
	modelID = strings.TrimSpace(modelID)
	reason = strings.TrimSpace(reason)
	if modelID == "" || reason == "" {
		return omitted
	}
	for _, lane := range omitted {
		if lane.ModelID == modelID && lane.Reason == reason {
			return omitted
		}
	}
	return append(omitted, OmittedLane{ModelID: modelID, Reason: reason})
}

func judgeLaneOmitReason(lane routeevidence.CapabilityRouteLane, supervised bool) string {
	if lane.Tier == "free" {
		return "free lane may not judge"
	}
	if lane.GateRole == "supervised_only" && !supervised {
		return "supervised_only judge lane requires operator supervision"
	}
	return "lane not eligible for judge"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
