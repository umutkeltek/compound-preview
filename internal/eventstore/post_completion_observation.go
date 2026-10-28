package eventstore

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Observe a tick on the same filesystem between the writer snapshots. Elapsed
// wall time alone cannot prove that the filesystem's timestamp clock advanced.
// The probe is private to this observation and never touches a retired stream.
func observePostCompletionRepairClock(directory string, interval time.Duration) (resultErr error) {
	probe, err := os.CreateTemp(directory, ".repair-clock-*")
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, probe.Close(), os.Remove(probe.Name()))
	}()
	sample := func() (time.Time, error) {
		if _, err := probe.WriteAt([]byte{0}, 0); err != nil {
			return time.Time{}, err
		}
		info, err := probe.Stat()
		if err != nil {
			return time.Time{}, err
		}
		return info.ModTime(), nil
	}
	return waitPostCompletionRepairClock(sample, interval)
}

func waitPostCompletionRepairClock(sample func() (time.Time, error), interval time.Duration) error {
	before, err := sample()
	if err != nil {
		return err
	}
	if interval > 0 {
		time.Sleep(interval)
	}
	// Bound an unsupported or frozen filesystem clock instead of certifying it
	// as quiet. The requested observation interval is additional to this bound.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		after, err := sample()
		if err != nil {
			return err
		}
		if after.After(before) {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("post-completion repair cannot observe filesystem timestamp progress; quiescence unproven")
		case <-tick.C:
		}
	}
}
