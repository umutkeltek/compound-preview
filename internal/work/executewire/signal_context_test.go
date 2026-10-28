package executewire

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Run real signals in a fresh process: a test-local context cancellation cannot
// reproduce the timeout supervisor forwarding TERM a second time to the door.
func TestLaunchSignalProcess(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_SIGNAL_CHILD") != "1" {
		return
	}
	var ctx context.Context
	if os.Getenv("COMPOUND_TEST_SIGNAL_GRACE") == "bounded" {
		ctx = newLaunchSignalContext(100 * time.Millisecond)
	} else {
		ctx = signalLaunchContext()
	}
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cancelled")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() == "finish" {
			os.Exit(0)
		}
		fmt.Println("finalizing")
	}
	os.Exit(2)
}

func TestLaunchSignalRepeatedTermPreservesFinalization(t *testing.T) {
	runLaunchSignalSequence(t, syscall.SIGTERM, "")
}

func TestLaunchSignalInterruptStillHardStops(t *testing.T) {
	runLaunchSignalSequence(t, os.Interrupt, "interrupt")
}

func TestLaunchSignalTermThenInterruptHardStops(t *testing.T) {
	runLaunchSignalSequence(t, syscall.SIGTERM, "interrupt")
}

func TestLaunchSignalGraceIsBounded(t *testing.T) {
	runLaunchSignalSequence(t, syscall.SIGTERM, "deadline")
}

func runLaunchSignalSequence(t *testing.T, first os.Signal, stopMode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLaunchSignalProcess$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_SIGNAL_CHILD=1")
	if stopMode == "deadline" {
		cmd.Env = append(cmd.Env, "COMPOUND_TEST_SIGNAL_GRACE=bounded")
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	wantLine := func(want string) {
		t.Helper()
		select {
		case line, ok := <-lines:
			if !ok || line != want {
				t.Fatalf("signal child stopped before %q: got %q (open=%v)", want, line, ok)
			}
		case <-ctx.Done():
			t.Fatalf("signal child did not reach %q within bound", want)
		}
	}
	wantLine("ready")
	if err := cmd.Process.Signal(first); err != nil {
		t.Fatal(err)
	}
	wantLine("cancelled")
	if stopMode != "" {
		wantSignal := syscall.SIGKILL
		if stopMode == "interrupt" {
			wantSignal = syscall.SIGINT
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
		}
		err := cmd.Wait()
		waited = true
		if ctx.Err() != nil {
			t.Fatal("signal stop exceeded test bound")
		}
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if err == nil || !ok || !status.Signaled() || status.Signal() != wantSignal {
			t.Fatalf("hard stop = %v, %v; want signal %v", status, err, wantSignal)
		}
		return
	}
	for i := 0; i < 8; i++ {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(input, "probe"); err != nil {
			t.Fatal(err)
		}
		wantLine("finalizing")
	}
	if _, err := fmt.Fprintln(input, "finish"); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("graceful finalization did not finish: %v", err)
	}
}
