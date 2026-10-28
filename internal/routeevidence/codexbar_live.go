package routeevidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"time"
)

const CodexBarProbeTimeout = 8 * time.Second
const MaxCodexBarProbeBytes = 1 << 20

var ErrCodexBarProbeOutputTooLarge = errors.New("CodexBar usage exceeds 1 MiB")

type CapacityCommandRunner func(context.Context, string, ...string) ([]byte, error)

type CodexBarWindow struct {
	UsedPercent   *float64 `json:"usedPercent"`
	WindowMinutes int      `json:"windowMinutes"`
	ResetsAt      string   `json:"resetsAt"`
}

type codexBarLiveUsage struct {
	Provider string `json:"provider"`
	Usage    *struct {
		UpdatedAt string          `json:"updatedAt"`
		Primary   *CodexBarWindow `json:"primary"`
		Secondary *CodexBarWindow `json:"secondary"`
		Tertiary  *CodexBarWindow `json:"tertiary"`
		Extra     []struct {
			Window CodexBarWindow `json:"window"`
		} `json:"extraRateWindows"`
	} `json:"usage"`
}

type CodexBarWindowEvidence struct {
	Present          bool
	RemainingPercent float64
	WindowMinutes    int
	ResetsAt         time.Time
}

type CodexBarCapacityEvidence struct {
	Provider  string
	UpdatedAt time.Time
	FiveHour  CodexBarWindowEvidence
	Weekly    CodexBarWindowEvidence
	All       []CodexBarWindowEvidence
}

func ParseCodexBarCapacity(body []byte, provider string, now time.Time, freshness time.Duration) (CodexBarCapacityEvidence, error) {
	provider = normalizeCodexBarProvider(provider)
	if provider == "" {
		return CodexBarCapacityEvidence{}, errors.New("CodexBar provider is required")
	}
	if freshness <= 0 {
		return CodexBarCapacityEvidence{}, errors.New("CodexBar freshness must be positive")
	}
	var rows []codexBarLiveUsage
	if err := json.Unmarshal(body, &rows); err != nil {
		return CodexBarCapacityEvidence{}, fmt.Errorf("decode CodexBar %s usage: %w", provider, err)
	}
	for _, row := range rows {
		if normalizeCodexBarProvider(row.Provider) != provider || row.Usage == nil {
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(row.Usage.UpdatedAt))
		if err != nil {
			return CodexBarCapacityEvidence{}, fmt.Errorf("CodexBar %s usage has no valid updatedAt", provider)
		}
		age := now.Sub(updatedAt)
		if age < -capacityClockSkew || age > freshness {
			return CodexBarCapacityEvidence{}, fmt.Errorf("CodexBar %s usage is stale by %s (freshness=%s)", provider, age.Round(time.Second), freshness)
		}
		windows := make([]CodexBarWindow, 0, 3+len(row.Usage.Extra))
		for _, window := range []*CodexBarWindow{row.Usage.Primary, row.Usage.Secondary, row.Usage.Tertiary} {
			if window != nil {
				windows = append(windows, *window)
			}
		}
		for _, extra := range row.Usage.Extra {
			windows = append(windows, extra.Window)
		}
		evidence := CodexBarCapacityEvidence{Provider: provider, UpdatedAt: updatedAt.UTC()}
		for _, window := range windows {
			if !validCodexBarWindow(window) {
				continue
			}
			reduced := reduceCodexBarWindow(window)
			evidence.All = append(evidence.All, reduced)
			switch window.WindowMinutes {
			case 5 * 60:
				if !evidence.FiveHour.Present || reduced.RemainingPercent < evidence.FiveHour.RemainingPercent {
					evidence.FiveHour = reduced
				}
			case 7 * 24 * 60:
				if !evidence.Weekly.Present || reduced.RemainingPercent < evidence.Weekly.RemainingPercent {
					evidence.Weekly = reduced
				}
			}
		}
		if len(evidence.All) == 0 {
			return CodexBarCapacityEvidence{}, fmt.Errorf("CodexBar %s usage has no valid capacity window", provider)
		}
		return evidence, nil
	}
	return CodexBarCapacityEvidence{}, fmt.Errorf("CodexBar output has no %s usage row", provider)
}

func ProbeCodexBarCapacity(provider string, now time.Time, freshness time.Duration) (CodexBarCapacityEvidence, error) {
	return ProbeCodexBarCapacityWithRunner(provider, now, freshness, RunBoundedCapacityCommand)
}

func ProbeCodexBarCapacityWithRunner(provider string, now time.Time, freshness time.Duration, run CapacityCommandRunner) (CodexBarCapacityEvidence, error) {
	provider = normalizeCodexBarProvider(provider)
	if provider == "" {
		return CodexBarCapacityEvidence{}, errors.New("CodexBar provider is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), CodexBarProbeTimeout)
	defer cancel()
	body, err := run(ctx, "codexbar", "usage", "--provider", provider, "--source", "auto", "--format", "json", "--no-color")
	if err != nil {
		return CodexBarCapacityEvidence{}, fmt.Errorf("probe CodexBar %s usage: %w", provider, err)
	}
	if len(body) > MaxCodexBarProbeBytes {
		return CodexBarCapacityEvidence{}, ErrCodexBarProbeOutputTooLarge
	}
	return ParseCodexBarCapacity(body, provider, now, freshness)
}

func RunBoundedCapacityCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open capacity source stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(stdout, MaxCodexBarProbeBytes+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read capacity source stdout: %w", readErr)
	}
	if len(body) > MaxCodexBarProbeBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, ErrCodexBarProbeOutputTooLarge
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return body, nil
}

func codexBarEvidenceCapacity(evidence CodexBarCapacityEvidence, now time.Time, expectedMinutes int) LaneCapacity {
	selected := evidence.All[0]
	for _, window := range evidence.All[1:] {
		if window.RemainingPercent < selected.RemainingPercent {
			selected = window
		}
	}
	capacity := LaneCapacity{
		Remaining:       selected.RemainingPercent,
		ResetsAt:        selected.ResetsAt,
		LastProven:      evidence.UpdatedAt,
		State:           CapacityAlive,
		Source:          "codexbar/live",
		SourceInstalled: true,
		DataRef: fmt.Sprintf("codexbar-live:provider=%s,window=%dm,updated_at=%s",
			evidence.Provider, selected.WindowMinutes, evidence.UpdatedAt.Format(time.RFC3339)),
	}
	if selected.RemainingPercent <= 0 {
		capacity.State = CapacityWalled
	} else if !selected.ResetsAt.IsZero() && !selected.ResetsAt.After(now.Add(time.Duration(expectedMinutes)*time.Minute)) {
		capacity.State = CapacityExpiring
	}
	return capacity
}

func validCodexBarWindow(window CodexBarWindow) bool {
	return window.WindowMinutes > 0 && window.UsedPercent != nil &&
		!math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) &&
		*window.UsedPercent >= 0 && *window.UsedPercent <= 100
}

func reduceCodexBarWindow(window CodexBarWindow) CodexBarWindowEvidence {
	var resetsAt time.Time
	if value := strings.TrimSpace(window.ResetsAt); value != "" {
		resetsAt, _ = time.Parse(time.RFC3339, value)
	}
	return CodexBarWindowEvidence{
		Present:          true,
		RemainingPercent: 100 - *window.UsedPercent,
		WindowMinutes:    window.WindowMinutes,
		ResetsAt:         resetsAt.UTC(),
	}
}

func normalizeCodexBarProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, "_", "")
	return value
}
