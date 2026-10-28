package executewire

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Matches the detached supervisor's kill-after bound. TERM requests graceful
// finalization; it must not make an unresponsive door immortal.
const launchSignalGrace = 60 * time.Second

var (
	launchSignalOnce sync.Once
	launchSignalCtx  context.Context
)

// Install lazily: subcommands that never launch retain default signal handling.
func signalLaunchContext() context.Context {
	launchSignalOnce.Do(func() {
		launchSignalCtx = newLaunchSignalContext(launchSignalGrace)
	})
	return launchSignalCtx
}

func newLaunchSignalContext(grace time.Duration) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	interrupts := make(chan os.Signal, 1)
	terms := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	signal.Notify(terms, syscall.SIGTERM)
	go func() {
		select {
		case <-interrupts:
		case <-terms:
		}
		// A group TERM also reaches GNU timeout, which can forward another TERM
		// to the same door. Keep TERM handled until exit so that duplicate cannot
		// kill the process between cancellation and durable attempt finalization.
		// Restore only Ctrl-C, BEFORE publishing cancellation: a subsequent
		// explicit interrupt remains a reliable immediate hard stop.
		signal.Stop(interrupts)
		time.AfterFunc(grace, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) })
		cancel()
		for range terms {
			// Repeated TERM is the same cancellation request, not a new budget.
		}
	}()
	return ctx
}
