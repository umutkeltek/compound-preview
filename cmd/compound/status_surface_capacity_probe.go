package main

import (
	"context"
	"fmt"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/statussurface"
)

const codexBarCapacityFreshness = 10 * time.Minute
const maxCodexBarProbeBytes = routeevidence.MaxCodexBarProbeBytes

var errCodexBarProbeOutputTooLarge = routeevidence.ErrCodexBarProbeOutputTooLarge

type statusCapacityCommandRunner = routeevidence.CapacityCommandRunner

type statusCapacityEvidence struct {
	FiveHour statussurface.CapacityEvidence
	Weekly   statussurface.CapacityEvidence
}

type claudeCapacityEvidence = statusCapacityEvidence

func parseCodexBarClaudeCapacity(body []byte, now time.Time) (claudeCapacityEvidence, error) {
	return parseCodexBarCapacity(body, "claude", now)
}

func parseCodexBarCapacity(body []byte, provider string, now time.Time) (statusCapacityEvidence, error) {
	evidence, err := routeevidence.ParseCodexBarCapacity(body, provider, now, codexBarCapacityFreshness)
	if err != nil {
		return statusCapacityEvidence{}, err
	}
	return statusCapacityEvidenceFromRoute(evidence, provider)
}

func statusCapacityEvidenceFromRoute(evidence routeevidence.CodexBarCapacityEvidence, provider string) (statusCapacityEvidence, error) {
	if !evidence.FiveHour.Present && !evidence.Weekly.Present {
		return statusCapacityEvidence{}, fmt.Errorf("CodexBar %s usage has no five-hour or weekly window", provider)
	}
	return statusCapacityEvidence{
		FiveHour: statusCapacityWindow(evidence.FiveHour),
		Weekly:   statusCapacityWindow(evidence.Weekly),
	}, nil
}

func probeCodexBarClaudeCapacity(now time.Time) (claudeCapacityEvidence, error) {
	return probeCodexBarCapacity("claude", now)
}

func probeCodexBarClaudeCapacityWithRunner(now time.Time, run statusCapacityCommandRunner) (claudeCapacityEvidence, error) {
	return probeCodexBarCapacityWithRunner("claude", now, run)
}

func probeCodexBarCapacity(provider string, now time.Time) (statusCapacityEvidence, error) {
	evidence, err := routeevidence.ProbeCodexBarCapacity(provider, now, codexBarCapacityFreshness)
	if err != nil {
		return statusCapacityEvidence{}, err
	}
	return statusCapacityEvidenceFromRoute(evidence, provider)
}

func probeCodexBarCapacityWithRunner(provider string, now time.Time, run statusCapacityCommandRunner) (statusCapacityEvidence, error) {
	evidence, err := routeevidence.ProbeCodexBarCapacityWithRunner(provider, now, codexBarCapacityFreshness, run)
	if err != nil {
		return statusCapacityEvidence{}, err
	}
	return statusCapacityEvidenceFromRoute(evidence, provider)
}

func runBoundedStatusCapacityCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	return routeevidence.RunBoundedCapacityCommand(ctx, command, args...)
}

func statusCapacityWindow(window routeevidence.CodexBarWindowEvidence) statussurface.CapacityEvidence {
	if !window.Present {
		return statussurface.CapacityEvidence{}
	}
	return statussurface.CapacityEvidence{
		Status:           statussurface.StatusLive,
		RemainingPercent: window.RemainingPercent,
		ResetsAt:         window.ResetsAt,
		Source:           "compound/codexbar-live",
	}
}
