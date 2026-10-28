package eventstore

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPostCompletionRepairClockRequiresObservedProgress(t *testing.T) {
	stamp := time.Unix(100, 0)
	samples := 0
	err := waitPostCompletionRepairClock(func() (time.Time, error) {
		samples++
		if samples < 4 {
			return stamp, nil
		}
		return stamp.Add(time.Millisecond), nil
	}, 0)
	if err != nil || samples != 4 {
		t.Fatalf("clock observation ended without progress: samples=%d err=%v", samples, err)
	}
}

func TestPostCompletionRepairClockFailureCannotCertifyQuiescence(t *testing.T) {
	want := errors.New("clock probe failed")
	if err := waitPostCompletionRepairClock(func() (time.Time, error) {
		return time.Time{}, want
	}, 0); !errors.Is(err, want) {
		t.Fatalf("probe failure = %v, want %v", err, want)
	}
	if err := waitPostCompletionRepairClock(func() (time.Time, error) {
		return time.Unix(100, 0), nil
	}, 0); err == nil || !strings.Contains(err.Error(), "quiescence unproven") {
		t.Fatalf("frozen clock accepted: %v", err)
	}
}
