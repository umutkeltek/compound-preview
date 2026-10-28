//go:build darwin

package home

import (
	"io/fs"
	"os"
	"sync"
)

const captureDirectoryWorkers = 4

type captureBulkTraversal struct {
	root     *os.Root
	visit    func(string, fs.FileInfo) error
	slots    chan struct{}
	stopped  chan struct{}
	visitMu  sync.Mutex
	failOnce sync.Once
	first    error
}

func (w *captureBulkTraversal) fail(err error) {
	if err != nil {
		w.failOnce.Do(func() { w.first = err; close(w.stopped) })
	}
}
func (w *captureBulkTraversal) check() error {
	select {
	case <-w.stopped:
		return w.first
	default:
		return nil
	}
}
func (w *captureBulkTraversal) emit(path string, info fs.FileInfo) error {
	w.visitMu.Lock()
	defer w.visitMu.Unlock()
	if err := w.check(); err != nil {
		return err
	}
	return w.visit(path, info)
}

// Each directory joins child subtrees before its final identity check.
// Nonblocking permits cap traversal goroutines without ancestors waiting
// for a permit held by a descendant. Full capacity recurses inline.
func (w *captureBulkTraversal) descend(dir string, expected fs.FileInfo, children *sync.WaitGroup) error {
	if err := w.check(); err != nil {
		return err
	}
	select {
	case w.slots <- struct{}{}:
		children.Add(1)
		go func() {
			defer children.Done()
			defer func() { <-w.slots }()
			w.fail(w.walk(dir, expected))
		}()
		return nil
	default:
		return w.walk(dir, expected)
	}
}
func runCaptureBulkTraversal(root *os.Root, dir string, visit func(string, fs.FileInfo) error, extraWorkers int) error {
	w := &captureBulkTraversal{root: root, visit: visit, slots: make(chan struct{}, extraWorkers), stopped: make(chan struct{})}
	w.fail(w.walk(dir, nil))
	return w.first
}
func walkCaptureRootConcurrent(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	return runCaptureBulkTraversal(root, dir, visit, captureDirectoryWorkers-1)
}
