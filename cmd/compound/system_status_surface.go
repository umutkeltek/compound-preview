package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/statussurface"
)

const maxStatusInputBytes = 1 << 20

func cmdSystemStatusSurface(args []string) {
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "system status-surface:", err)
		os.Exit(1)
	}
	out, err := runSystemStatusSurface(args, os.Stdin, homePath, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "system status-surface:", err)
		os.Exit(2)
	}
	fmt.Fprint(os.Stdout, out)
}

func runSystemStatusSurface(args []string, stdin io.Reader, homePath string, now time.Time) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: compound system status-surface <snapshot|render|theme> --harness <claude|codex>")
	}
	switch args[0] {
	case "snapshot":
		fs := newStatusFlagSet("snapshot")
		harness := fs.String("harness", "", "claude or codex")
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		if fs.NArg() != 0 {
			return "", fmt.Errorf("snapshot: unexpected arguments %q", fs.Args())
		}
		snapshot, err := buildLiveStatusSnapshot(parseStatusHarness(*harness), stdin, homePath, now)
		if err != nil {
			return "", err
		}
		body, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			return "", err
		}
		return string(body) + "\n", nil
	case "render":
		fs := newStatusFlagSet("render")
		harness := fs.String("harness", "", "claude or codex")
		theme := fs.String("theme", statussurface.ThemeAuto, "auto or a visual style id")
		width := fs.Int("width", 120, "maximum display width")
		noColor := fs.Bool("no-color", false, "disable ANSI color")
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		if fs.NArg() != 0 || *width <= 0 {
			return "", errors.New("render: width must be positive and positional arguments are not accepted")
		}
		snapshot, err := buildLiveStatusSnapshot(parseStatusHarness(*harness), stdin, homePath, now)
		if err != nil {
			return "", err
		}
		line, err := statussurface.Render(snapshot, statussurface.RenderOptions{Theme: *theme, Width: *width, Color: !*noColor})
		if err != nil {
			return "", err
		}
		return line + "\n", nil
	case "theme":
		fs := newStatusFlagSet("theme")
		harness := fs.String("harness", "", "codex")
		theme := fs.String("theme", statussurface.ThemePAINexus, "Codex theme id")
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		if parseStatusHarness(*harness) != statussurface.HarnessCodex {
			return "", errors.New("theme: only the codex TextMate projection is supported")
		}
		return statussurface.RenderCodexTheme(*theme)
	default:
		return "", fmt.Errorf("unknown status-surface command %q", args[0])
	}
}

func newStatusFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("status-surface "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseStatusHarness(value string) statussurface.Harness {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claude_code":
		return statussurface.HarnessClaude
	case "codex":
		return statussurface.HarnessCodex
	default:
		return statussurface.Harness("")
	}
}

func buildLiveStatusSnapshot(harness statussurface.Harness, stdin io.Reader, homePath string, now time.Time) (statussurface.Snapshot, error) {
	return buildLiveStatusSnapshotWithCapacityReader(harness, stdin, homePath, now, routeevidence.ReadLaneCapacity)
}

func buildLiveStatusSnapshotWithCapacityReader(
	harness statussurface.Harness,
	stdin io.Reader,
	homePath string,
	now time.Time,
	readCapacity func(string, time.Time) (routeevidence.LaneCapacity, error),
) (statussurface.Snapshot, error) {
	if harness != statussurface.HarnessClaude && harness != statussurface.HarnessCodex {
		return statussurface.Snapshot{}, errors.New("--harness must be claude or codex")
	}
	store := statussurface.Store{Home: homePath}
	previous, fresh, err := store.ReadFresh(harness, now)
	if err != nil {
		return statussurface.Snapshot{}, err
	}
	if !fresh {
		previous = statussurface.Snapshot{}
	}
	var snapshot statussurface.Snapshot
	if harness == statussurface.HarnessClaude {
		body, err := io.ReadAll(io.LimitReader(stdin, maxStatusInputBytes+1))
		if err != nil {
			return statussurface.Snapshot{}, err
		}
		if len(body) > maxStatusInputBytes {
			return statussurface.Snapshot{}, errors.New("Claude status input exceeds 1 MiB")
		}
		snapshot, err = statussurface.BuildClaudeSnapshot(body, now, previous)
		if err != nil {
			return statussurface.Snapshot{}, err
		}
		snapshot = supplementLiveClaudeCapacity(snapshot, previous, fresh, now, readCapacity, probeCodexBarClaudeCapacity)
	} else {
		snapshot, err = buildLiveCodexStatusSnapshot(previous, fresh, now, readCapacity, probeCodexBarCapacity)
		if err != nil {
			return statussurface.Snapshot{}, err
		}
	}
	if err := store.Write(&snapshot); err != nil {
		return statussurface.Snapshot{}, err
	}
	return snapshot, nil
}

func buildLiveCodexStatusSnapshot(
	previous statussurface.Snapshot,
	previousFresh bool,
	now time.Time,
	readCapacity func(string, time.Time) (routeevidence.LaneCapacity, error),
	probe func(string, time.Time) (statusCapacityEvidence, error),
) (statussurface.Snapshot, error) {
	capacity, err := readCapacity("codex", now)
	if err != nil {
		return statussurface.Snapshot{}, err
	}
	evidence := statussurface.CapacityEvidence{
		Status: statussurface.StatusUnknown,
		Source: "compound/routeevidence:" + strings.TrimSpace(capacity.Source),
	}
	if capacity.State != routeevidence.CapacityUnknown {
		evidence = statussurface.CapacityEvidence{
			Status: statussurface.StatusLive, RemainingPercent: capacity.Remaining, ResetsAt: capacity.ResetsAt,
			Source: "compound/routeevidence:" + strings.TrimSpace(capacity.Source),
		}
		return statussurface.BuildCodexSnapshot(evidence, now, previous), nil
	}
	if previousFresh && previous.Harness == statussurface.HarnessCodex {
		for _, limit := range previous.Limits {
			if limit.Kind == statussurface.LimitSevenDay && limit.Status == statussurface.StatusLive {
				evidence = statussurface.CapacityEvidence{
					Status: statussurface.StatusLive, RemainingPercent: limit.RemainingPercent,
					ResetsAt: limit.ResetsAt, Source: limit.Source,
				}
				return statussurface.BuildCodexSnapshot(evidence, now, previous), nil
			}
		}
	}
	if probe != nil {
		if live, probeErr := probe("codex", now); probeErr == nil && live.Weekly.Status == statussurface.StatusLive {
			evidence = live.Weekly
		}
	}
	return statussurface.BuildCodexSnapshot(evidence, now, previous), nil
}

func supplementLiveClaudeCapacity(
	snapshot statussurface.Snapshot,
	previous statussurface.Snapshot,
	previousFresh bool,
	now time.Time,
	readCapacity func(string, time.Time) (routeevidence.LaneCapacity, error),
	probe func(time.Time) (claudeCapacityEvidence, error),
) statussurface.Snapshot {
	if !hasLiveStatusLimit(snapshot, statussurface.LimitSevenDay) {
		if capacity, err := readCapacity("claude", now); err == nil && capacity.State != routeevidence.CapacityUnknown {
			snapshot = statussurface.SupplementWeeklyCapacity(snapshot, statussurface.CapacityEvidence{
				Status: statussurface.StatusLive, RemainingPercent: capacity.Remaining, ResetsAt: capacity.ResetsAt,
				Source: "compound/routeevidence:" + strings.TrimSpace(capacity.Source),
			}, previous)
		}
	}
	if previousFresh {
		for _, limit := range previous.Limits {
			if limit.Status != statussurface.StatusLive {
				continue
			}
			evidence := statussurface.CapacityEvidence{
				Status: statussurface.StatusLive, RemainingPercent: limit.RemainingPercent,
				ResetsAt: limit.ResetsAt, Source: limit.Source,
			}
			switch limit.Kind {
			case statussurface.LimitFiveHour:
				snapshot = statussurface.SupplementFiveHourCapacity(snapshot, evidence, previous)
			case statussurface.LimitSevenDay:
				snapshot = statussurface.SupplementWeeklyCapacity(snapshot, evidence, previous)
			}
		}
	}
	if hasLiveStatusLimit(snapshot, statussurface.LimitFiveHour) && hasLiveStatusLimit(snapshot, statussurface.LimitSevenDay) {
		return snapshot
	}
	if probe != nil {
		if evidence, err := probe(now); err == nil {
			snapshot = statussurface.SupplementFiveHourCapacity(snapshot, evidence.FiveHour, previous)
			snapshot = statussurface.SupplementWeeklyCapacity(snapshot, evidence.Weekly, previous)
		}
	}
	return snapshot
}

func hasLiveStatusLimit(snapshot statussurface.Snapshot, kind statussurface.LimitKind) bool {
	for _, limit := range snapshot.Limits {
		if limit.Kind == kind && limit.Status == statussurface.StatusLive {
			return true
		}
	}
	return false
}
