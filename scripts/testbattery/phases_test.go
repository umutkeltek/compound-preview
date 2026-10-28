package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
)

func TestFixedTestPhasesCoverInventoryExactlyOnce(t *testing.T) {
	inventory := sourcePackageInventory{All: []string{"compound/cmd/compound", "compound/internal/work", "compound/internal/home", "compound/internal/release", "compound/internal/hook", "compound/scripts/testbattery"}}
	calls := func(mode string) [][]string {
		var got [][]string
		err := runSourceGate(t.Context(), mode, func(context.Context, io.Writer) (sourcePackageInventory, error) { return inventory, nil }, func(_ context.Context, args []string, _, _ io.Writer) error {
			got = append(got, append([]string(nil), args...))
			return nil
		}, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := calls("test")
	split := append(calls("general-release"), calls("home")...)
	if !reflect.DeepEqual(all, split) {
		t.Fatalf("split changed inventory or order: all=%v split=%v", all, split)
	}
	wantOrder := []string{"compound/scripts/testbattery", "compound/cmd/compound", "compound/internal/work", "compound/internal/release", "compound/internal/hook", "compound/internal/home"}
	if len(split) != len(wantOrder) {
		t.Fatalf("phases = %v; want six disjoint phases", split)
	}
	for i, pkg := range wantOrder {
		if !reflect.DeepEqual(split[i][3:], []string{pkg}) {
			t.Fatalf("phase %d = %v; want singleton %s", i+1, split[i], pkg)
		}
	}
	seen := map[string]int{}
	for _, args := range split {
		if !reflect.DeepEqual(args[:3], []string{"test", "-count=1", "-timeout=40m"}) {
			t.Fatalf("weakened execution flags: %v", args)
		}
		for _, pkg := range args[3:] {
			seen[pkg]++
		}
	}
	if len(seen) != len(inventory.All) {
		t.Fatalf("coverage=%v", seen)
	}
	for _, pkg := range inventory.All {
		if seen[pkg] != 1 {
			t.Fatalf("%s count=%d", pkg, seen[pkg])
		}
	}
	home := calls("home")
	if len(home) != 1 || !reflect.DeepEqual(home[0][3:], []string{"compound/internal/home"}) {
		t.Fatalf("home gate escaped package: %v", home)
	}
}

func TestFixedTestPhasesKeepInventoryFailureAndCancellation(t *testing.T) {
	failure := batteryExitError(t, 1)
	for _, mode := range []string{"general-release", "home"} {
		for _, cancelRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", mode, cancelRun), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				tests := 0
				err := runBatteryPhase(ctx, mode, func(_ context.Context, args []string, out, _ io.Writer) error {
					if args[0] == "list" {
						fmt.Fprintln(out, "compound/internal/work\ncompound/internal/release\ncompound/internal/home")
						return nil
					}
					tests++
					if cancelRun {
						cancel()
					}
					return failure
				}, io.Discard, io.Discard)
				want := 1
				if mode == "general-release" && !cancelRun {
					want = 2
				}
				if tests != want || !errors.Is(err, failure) {
					t.Fatalf("calls=%d err=%v", tests, err)
				}
				if cancelRun && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			})
		}
		calls := 0
		err := runBatteryPhase(t.Context(), mode, func(_ context.Context, args []string, out, _ io.Writer) error {
			calls++
			fmt.Fprintln(out, "compound/internal/home")
			return nil
		}, io.Discard, io.Discard)
		if err == nil || calls != 1 {
			t.Fatalf("%s hid invalid full inventory", mode)
		}
	}
}

func TestFixedTestPhasesWithoutGeneralPackages(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		mode string
		want []string
	}{
		{"test", []string{"compound/cmd/compound", "compound/internal/work", "compound/internal/release", "compound/internal/home"}},
		{"general-release", []string{"compound/cmd/compound", "compound/internal/work", "compound/internal/release"}},
		{"home", []string{"compound/internal/home"}},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			var got []string
			err := runBatteryPhase(t.Context(), tt.mode, func(_ context.Context, args []string, out, _ io.Writer) error {
				if args[0] == "list" {
					fmt.Fprintln(out, "compound/internal/home\ncompound/cmd/compound\ncompound/internal/work\ncompound/internal/release")
					return nil
				}
				if len(args) != 4 || !reflect.DeepEqual(args[:3], []string{"test", "-count=1", "-timeout=40m"}) {
					t.Fatalf("isolated phase changed flags or overlaps packages: %v", args)
				}
				got = append(got, args[3])
				return nil
			}, io.Discard, io.Discard)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("calls=%v error=%v; want %v", got, err, tt.want)
			}
		})
	}
}
