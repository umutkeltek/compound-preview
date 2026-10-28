package home

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// archiveEntryBudget is shared by the producer and consumer. Counts include
// directories and the serialized archive manifest, not only checksummed files.
type archiveEntryBudget struct {
	limits        archiveReadLimits
	entries       int
	expandedBytes int64
}

func (b *archiveEntryBudget) observe(hdr *tar.Header) error {
	name := filepath.ToSlash(hdr.Name)
	if err := validateArchivePath(name); err != nil {
		return err
	}
	if b.entries >= b.limits.MaxEntries {
		return fmt.Errorf("archive entry count exceeds %d", b.limits.MaxEntries)
	}
	if len(name) > b.limits.MaxPathBytes {
		return fmt.Errorf("archive path exceeds %d bytes: %s", b.limits.MaxPathBytes, name)
	}
	if len(strings.Split(strings.TrimSuffix(name, "/"), "/")) > b.limits.MaxPathDepth {
		return fmt.Errorf("archive path depth exceeds %d: %s", b.limits.MaxPathDepth, name)
	}
	if hdr.Size < 0 || hdr.Size > b.limits.MaxEntryBytes {
		return fmt.Errorf("archive entry %s exceeds %d bytes", name, b.limits.MaxEntryBytes)
	}
	if b.expandedBytes > b.limits.MaxExpandedBytes-hdr.Size {
		return fmt.Errorf("archive expanded bytes exceed %d", b.limits.MaxExpandedBytes)
	}
	if name == ArchiveManifestName && hdr.Size > b.limits.MaxManifestBytes {
		return fmt.Errorf("archive manifest exceeds %d bytes", b.limits.MaxManifestBytes)
	}
	b.entries++
	b.expandedBytes += hdr.Size
	return nil
}

type archiveEntryWriter interface {
	io.Writer
	WriteHeader(*tar.Header) error
}

type budgetedArchiveWriter struct {
	*tar.Writer
	budget archiveEntryBudget
}

func (w *budgetedArchiveWriter) WriteHeader(hdr *tar.Header) error {
	if err := w.budget.observe(hdr); err != nil {
		return err
	}
	return w.Writer.WriteHeader(hdr)
}

// Count the actual streams, including tar padding/PAX metadata and gzip footer.
// The unpublished temporary file is discarded if either writer reaches its cap.
func newBudgetedArchiveWriters(out io.Writer, limits archiveReadLimits) (*budgetedArchiveWriter, *gzip.Writer, error) {
	if err := validateArchiveReadLimits(limits); err != nil {
		return nil, nil, err
	}
	source := &boundedArchiveOutput{writer: out, remaining: limits.MaxSourceBytes, limit: limits.MaxSourceBytes, kind: "archive source"}
	gz := gzip.NewWriter(source)
	decoded := &boundedArchiveOutput{writer: gz, remaining: limits.MaxDecodedBytes, limit: limits.MaxDecodedBytes, kind: "decoded archive"}
	return &budgetedArchiveWriter{Writer: tar.NewWriter(decoded), budget: archiveEntryBudget{limits: limits}}, gz, nil
}

type boundedArchiveOutput struct {
	writer    io.Writer
	remaining int64
	limit     int64
	kind      string
}

func (w *boundedArchiveOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%s exceeds %d bytes while writing", w.kind, w.limit)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
