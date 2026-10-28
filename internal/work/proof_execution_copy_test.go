package work

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type cancellingProofReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancellingProofReader) Read(p []byte) (int, error) {
	r.reads++
	copy(p, "payload")
	r.cancel()
	return len("payload"), nil
}
func TestProofExecutionCopyStopsBeforeWritingCancelledChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &cancellingProofReader{cancel: cancel}
	var target bytes.Buffer
	n, err := copyProofContext(ctx, &target, source)
	if !errors.Is(err, context.Canceled) || n != 0 || source.reads != 1 || target.Len() != 0 {
		t.Fatalf("copy continued after cancellation: bytes=%d reads=%d output=%q error=%v", n, source.reads, target.String(), err)
	}
}
func TestProofExecutionCancelledRuntimeDoesNotResolveOrCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, cleanup, err := PinProofRuntimeExecutable(ctx, func(string) (string, error) { called = true; return "", nil })
	defer cleanup()
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled runtime setup ran: called=%v err=%v", called, err)
	}
}
