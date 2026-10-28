package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestBatteryRefusesMissingReleaseBeforeTests(t *testing.T) {
	t.Parallel()
	calls := 0
	err := runBattery(t.Context(), func(_ context.Context, _ []string, out, _ io.Writer) error {
		calls++
		fmt.Fprintln(out, "compound/internal/home\ncompound/internal/work")
		return nil
	}, io.Discard, io.Discard)
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "release") {
		t.Fatalf("missing release: calls=%d error=%v; want explicit refusal before tests", calls, err)
	}
}

func TestBatteryRetainsFailuresFromEveryPhase(t *testing.T) {
	t.Parallel()
	failures := []error{batteryExitError(t, 1), batteryExitError(t, 2), batteryExitError(t, 3)}
	calls := 0
	err := runBattery(t.Context(), func(_ context.Context, args []string, out, _ io.Writer) error {
		calls++
		if args[0] == "list" {
			fmt.Fprintln(out, "compound/internal/home\ncompound/internal/work\ncompound/internal/release")
			return nil
		}
		return failures[calls-2]
	}, io.Discard, io.Discard)
	if calls != 4 {
		t.Fatalf("calls=%d; want all three test phases after inventory", calls)
	}
	for i, failure := range failures {
		if !errors.Is(err, failure) {
			t.Errorf("phase %d failure missing from %v", i+1, err)
		}
	}
}

func TestBatteryIsolatesCLIWorkReleaseHookAndHomeWithoutFiltering(t *testing.T) {
	t.Parallel()
	var calls [][]string
	var output bytes.Buffer
	err := runBattery(t.Context(), func(_ context.Context, args []string, out, _ io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "list" {
			fmt.Fprintln(out, "compound/internal/home\ncompound/scripts/testbattery\ncompound/cmd/compound\ncompound/internal/work\ncompound/internal/release\ncompound/internal/hook")
		}
		return nil
	}, &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"list", "./..."},
		{"test", "-count=1", "-timeout=40m", "compound/scripts/testbattery"},
		{"test", "-count=1", "-timeout=40m", "compound/cmd/compound"},
		{"test", "-count=1", "-timeout=40m", "compound/internal/work"},
		{"test", "-count=1", "-timeout=40m", "compound/internal/release"},
		{"test", "-count=1", "-timeout=40m", "compound/internal/hook"},
		{"test", "-count=1", "-timeout=40m", "compound/internal/home"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("machine-wide resource phases overlap or inventory changed:\ngot %v\nwant %v", calls, want)
	}
	for _, marker := range []string{"phase 1/6 packages=1", "phase 2/6 packages=1", "phase 3/6 packages=1", "phase 4/6 packages=1", "phase 5/6 packages=1", "phase 6/6 packages=1"} {
		if !strings.Contains(output.String(), marker) {
			t.Errorf("missing phase evidence %q in %q", marker, output.String())
		}
	}
}

func TestBatteryIsolatedPhasesRetainFailuresAndStopOnCancellation(t *testing.T) {
	t.Parallel()
	testFailure := batteryExitError(t, 1)
	startFailure := errors.New("synthetic isolated start failure")
	for _, mode := range []string{"ordinary", "infrastructure", "cancellation"} {
		for at := 2; at <= 7; at++ {
			t.Run(fmt.Sprintf("%s/command%d", mode, at), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				err := runBattery(ctx, func(_ context.Context, args []string, out, _ io.Writer) error {
					calls++
					if args[0] == "list" {
						fmt.Fprintln(out, "compound/scripts/testbattery\ncompound/cmd/compound\ncompound/internal/release\ncompound/internal/home\ncompound/internal/work\ncompound/internal/hook")
					}
					if calls == at {
						switch mode {
						case "ordinary":
							return testFailure
						case "infrastructure":
							return startFailure
						case "cancellation":
							cancel()
						}
					}
					return nil
				}, io.Discard, io.Discard)
				wantCalls, wantErr := 7, error(testFailure)
				if mode == "infrastructure" {
					wantCalls, wantErr = at, startFailure
				}
				if mode == "cancellation" {
					wantCalls, wantErr = at, context.Canceled
				}
				if calls != wantCalls || !errors.Is(err, wantErr) {
					t.Fatalf("calls=%d error=%v; want calls=%d error containing %v", calls, err, wantCalls, wantErr)
				}
			})
		}
	}
}
