//go:build darwin

package main

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

const darwinProcessStateZombie = 5

func readTestProcessSnapshots() ([]testProcessSnapshot, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	snapshots := make([]testProcessSnapshot, 0, len(processes))
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		pgid := int(process.Eproc.Pgid)
		if pid <= 0 || pgid <= 0 {
			continue
		}
		snapshots = append(snapshots, testProcessSnapshot{
			pid:     pid,
			pgid:    pgid,
			birthID: fmt.Sprintf("%d:%d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec),
			state:   darwinTestProcessState(process.Proc.P_stat),
		})
	}
	return snapshots, nil
}

func darwinTestProcessState(state int8) testProcessState {
	if state == darwinProcessStateZombie {
		return testProcessStateZombie
	}
	return testProcessStateRunning
}

func TestDarwinTestProcessStateClassifiesZombieOnly(t *testing.T) {
	if got := darwinTestProcessState(darwinProcessStateZombie); got != testProcessStateZombie {
		t.Fatalf("Darwin zombie state = %v, want zombie", got)
	}
	for _, state := range []int8{1, 2, 3, 4, 6} {
		if got := darwinTestProcessState(state); got != testProcessStateRunning {
			t.Fatalf("Darwin state %d = %v, want running", state, got)
		}
	}
}
