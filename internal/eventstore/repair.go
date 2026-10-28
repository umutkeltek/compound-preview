package eventstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const tailWindow int64 = 8 << 20

var ErrCanonicalCorruption = errors.New("event segment canonical corruption")

// CanonicalCorruptionError pins corruption to a physical segment position while
// remaining matchable with ErrCanonicalCorruption.
type CanonicalCorruptionError struct {
	Path   string
	Line   int
	Offset int64
	Cause  error
}

func (e *CanonicalCorruptionError) Error() string {
	if e == nil {
		return ErrCanonicalCorruption.Error()
	}
	return fmt.Sprintf("%s: %s line %d offset %d: %v", ErrCanonicalCorruption, e.Path, e.Line, e.Offset, e.Cause)
}

func (e *CanonicalCorruptionError) Unwrap() error { return ErrCanonicalCorruption }

type parsedFrame struct {
	Frame  Frame
	Line   int
	Offset int64
	End    int64
}

func parseSegment(path string, tolerateFinalTorn bool) ([]parsedFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseSegmentBytes(path, data, tolerateFinalTorn)
}

func parseSegmentBytes(path string, data []byte, tolerateFinalTorn bool) ([]parsedFrame, error) {
	frames := make([]parsedFrame, 0)
	line, offset := 1, 0
	for offset < len(data) {
		relEnd := bytes.IndexByte(data[offset:], '\n')
		complete := relEnd >= 0
		end := len(data)
		if complete {
			end = offset + relEnd
		}
		frame, err := decodeFrame(data[offset:end])
		isFinal := !complete || end+1 == len(data)
		if err != nil {
			if tolerateFinalTorn && isFinal {
				return frames, nil
			}
			return nil, corruptionAt(path, data, line, int64(offset), err)
		}
		if !complete {
			if tolerateFinalTorn {
				return frames, nil
			}
			return nil, corruptionAt(path, data, line, int64(offset), &FrameInvalidError{Reason: "final frame has no newline"})
		}
		frames = append(frames, parsedFrame{Frame: frame, Line: line, Offset: int64(offset), End: int64(end + 1)})
		offset = end + 1
		line++
	}
	return frames, nil
}

func corruptionAt(path string, data []byte, line int, offset int64, cause error) error {
	if line <= 0 {
		line = bytes.Count(data[:minInt64(int64(len(data)), offset)], []byte("\n")) + 1
	}
	return &CanonicalCorruptionError{Path: path, Line: line, Offset: offset, Cause: cause}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (e *engine) repairTail(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	windowStart := int64(0)
	if info.Size() > tailWindow {
		windowStart = info.Size() - tailWindow
	}
	tail := make([]byte, int(info.Size()-windowStart))
	n, err := file.ReadAt(tail, windowStart)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(tail) {
		return io.ErrUnexpectedEOF
	}

	if tail[len(tail)-1] != '\n' {
		lastNewline := bytes.LastIndexByte(tail, '\n')
		if lastNewline < 0 && windowStart > 0 {
			return &CanonicalCorruptionError{Path: path, Line: 1, Offset: windowStart, Cause: &FrameInvalidError{Reason: "unterminated final frame exceeds tail repair window"}}
		}
		truncateAt := int64(0)
		if lastNewline >= 0 {
			truncateAt = windowStart + int64(lastNewline) + 1
		}
		if err := parseSegmentPrefix(path, truncateAt); err != nil {
			return err
		}
		if err := truncateAndSync(path, truncateAt, false); err != nil {
			return err
		}
		invalidateHotSegment(path)
		e.logf("eventstore: repaired torn final frame in %s at offset %d", path, truncateAt)
		return nil
	}

	finalEnd := len(tail) - 1
	lineStartInTail := bytes.LastIndexByte(tail[:finalEnd], '\n') + 1
	if lineStartInTail == 0 && windowStart > 0 {
		return &CanonicalCorruptionError{Path: path, Line: 1, Offset: windowStart, Cause: &FrameInvalidError{Reason: "final frame exceeds tail repair window"}}
	}
	lineStart := windowStart + int64(lineStartInTail)
	if _, err := decodeFrame(tail[lineStartInTail:finalEnd]); err == nil {
		// A valid tail is not permission to ignore interior corruption in the
		// only segment the write path is allowed to repair. Route the sweep
		// through the hot memo instead of an unconditional strict reparse:
		// the tail decode above just validated the final frame under the
		// exclusive append lock, so a fresh memo entry (built with tolerant
		// final-torn parsing, which cannot matter here since the final frame
		// already decoded) is the same every-frame-decoded guarantee the
		// strict sweep gave. If the memo is already fresh for this exact
		// {path, size, mtime}, a prior call in this process already gave that
		// guarantee and stat-unchanged bytes need no reverification. Either
		// way this warms the memo so the same put's checkID is a pure map
		// hit — one full parse per cold process, zero when warm.
		if fresh, err := e.hotSegmentFresh(path); err != nil {
			return err
		} else if fresh {
			return nil
		}
		if _, err := e.hotFrames(path); err != nil {
			return err
		}
		return nil
	}
	if err := parseSegmentPrefix(path, lineStart); err != nil {
		return err
	}
	if err := truncateAndSync(path, lineStart, true); err != nil {
		return err
	}
	invalidateHotSegment(path)
	e.logf("eventstore: repaired invalid final frame in %s at offset %d", path, lineStart)
	return nil
}

func parseSegmentPrefix(path string, size int64) error {
	if size == 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, size))
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return io.ErrUnexpectedEOF
	}
	_, err = parseSegmentBytes(path, data, false)
	return err
}

func truncateAndSync(path string, size int64, syncParent bool) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if syncParent {
		return syncDirectory(filepath.Dir(path))
	}
	return nil
}
