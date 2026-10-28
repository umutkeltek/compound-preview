package mem

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// The entity registry is append-only and both of its readers are last-write-wins
// per slug, so every superseded row is bytes no reader can observe. Because the
// row carries the entity's whole identifier set, appending state-of-size-n at
// step n costs sum(n) rather than n: a set-valued field stored across a time
// axis. Folding rewrites the encoding to the rows the readers already select.
//
// A fold is NOT home.Compact. Compact deletes rebuildable or transient bytes and
// the habitat catalog forbids it on authority-class paths, correctly. A fold
// discards no meaning at all: it is gated on both production readers returning
// deep-equal results over the old and new encodings, and the pre-fold bytes are
// archived with their digest before the swap. If the equality gate does not
// hold, nothing is renamed.

// foldScanBuffer bounds one JSONL record. Rows are large by construction (an
// identifier set inline), so the default scanner limit is far too small; a row
// past this bound is a corruption signal worth failing on rather than truncating.
const foldScanBuffer = 32 << 20

// EntityFoldReport is the receipt for one fold of the entity registry.
type EntityFoldReport struct {
	Path          string `json:"path"`
	RowsBefore    int    `json:"rows_before"`
	RowsAfter     int    `json:"rows_after"`
	BytesBefore   int64  `json:"bytes_before"`
	BytesAfter    int64  `json:"bytes_after"`
	SkippedRows   int    `json:"skipped_rows"`
	ArchivePath   string `json:"archive_path,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	Verified      bool   `json:"verified"`
	Applied       bool   `json:"applied"`
}

// FoldEntities rewrites the entity registry to the rows its readers already
// select. It applies nothing unless both production readers agree across the two
// encodings and the pre-fold bytes are archived and digest-checked first. With
// apply=false it proves the fold and reports the result without touching the
// registry.
func (s *Store) FoldEntities(now time.Time, apply bool) (EntityFoldReport, error) {
	report := EntityFoldReport{Path: s.entitiesLog()}
	err := s.withLock(func() error { return s.foldEntitiesLocked(&report, now, apply) })
	return report, err
}

func (s *Store) foldEntitiesLocked(report *EntityFoldReport, now time.Time, apply bool) error {
	origin := s.entitiesLog()
	info, err := os.Stat(origin)
	if os.IsNotExist(err) {
		report.Verified = true
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("entity registry is not a regular file: %s", origin)
	}
	report.BytesBefore = info.Size()

	kept, order, rows, skipped, err := foldRows(origin)
	if err != nil {
		return err
	}
	report.RowsBefore = rows
	report.RowsAfter = len(order)
	report.SkippedRows = skipped

	tmp := origin + ".fold-" + ts(now)
	if err := writeFoldedRows(tmp, kept, order); err != nil {
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp) // no-op once renamed

	if err := verifyFoldEquivalent(origin, tmp); err != nil {
		return err
	}
	report.Verified = true
	if !apply {
		return nil
	}

	archive := filepath.Join(s.home, "archives", "ledger", "entities-"+ts(now)+".jsonl")
	digest, err := archiveRegistry(origin, archive)
	if err != nil {
		return err
	}
	report.ArchivePath = archive
	report.ArchiveSHA256 = digest

	// The archive is the only copy of the pre-fold bytes once the rename lands,
	// so read it back and prove it reproduces the same entities before swapping.
	if err := verifyArchiveFaithful(origin, archive, digest); err != nil {
		return err
	}

	if err := os.Rename(tmp, origin); err != nil {
		return err
	}
	after, err := os.Stat(origin)
	if err != nil {
		return err
	}
	report.BytesAfter = after.Size()
	report.Applied = true
	return nil
}

// foldRows streams the registry and keeps the last raw line per slug in
// first-appearance order. Raw bytes are kept verbatim rather than re-marshaled:
// re-encoding through the Entity struct would silently drop any field the struct
// does not model, which is exactly the data loss a fold must not cause.
func foldRows(path string) (kept map[string][]byte, order []string, rows, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	defer f.Close()

	kept = map[string][]byte{}
	reader := bufio.NewReaderSize(f, 1<<20)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) > foldScanBuffer {
			return nil, nil, 0, 0, fmt.Errorf("entity row exceeds %d bytes; refusing to fold a possibly corrupt registry", foldScanBuffer)
		}
		if trimmedLen(line) > 0 {
			rows++
			// Mirror loadEntities exactly: a row that does not decode, or
			// decodes without a slug, is already invisible to every reader.
			var e Entity
			if json.Unmarshal(line, &e) != nil || e.Slug == "" {
				skipped++
			} else {
				if _, seen := kept[e.Slug]; !seen {
					order = append(order, e.Slug)
				}
				kept[e.Slug] = append([]byte(nil), line...)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, 0, 0, readErr
		}
	}
	return kept, order, rows, skipped, nil
}

func trimmedLen(b []byte) int {
	n := 0
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			n++
		}
	}
	return n
}

func writeFoldedRows(path string, kept map[string][]byte, order []string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, slug := range order {
		if _, err := w.Write(kept[slug]); err != nil {
			f.Close()
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// verifyFoldEquivalent is the gate, and it has two independent halves because
// neither alone is sufficient.
//
// Reader equality proves meaning survives, but it is blind to any field the
// Entity struct does not model: both readers decode into structs, so a rewrite
// that silently dropped an unknown key would still compare equal. Byte
// provenance covers exactly that blind spot by requiring every surviving line to
// be a verbatim line of the original. Neither check subsumes the other: reader
// equality catches keeping the wrong row, byte provenance catches losing bytes
// the readers cannot see.
func verifyFoldEquivalent(origin, folded string) error {
	if err := verifyFoldByteProvenance(origin, folded); err != nil {
		return err
	}
	return verifyFoldReaderEquality(origin, folded)
}

// verifyFoldByteProvenance requires each line of the folded file to appear
// verbatim in the original. Lines are compared by digest so the check costs a
// hash set rather than a second copy of the registry.
func verifyFoldByteProvenance(origin, folded string) error {
	seen := map[[sha256.Size]byte]bool{}
	if err := eachLine(origin, func(line []byte) error {
		seen[sha256.Sum256(line)] = true
		return nil
	}); err != nil {
		return fmt.Errorf("fold verify: scan origin: %w", err)
	}
	index := 0
	if err := eachLine(folded, func(line []byte) error {
		index++
		if !seen[sha256.Sum256(line)] {
			return fmt.Errorf("fold verify: folded row %d is not a verbatim row of the original; registry left untouched", index)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func eachLine(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 1<<20)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if trimmedLen(line) > 0 {
			if err := fn(line); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func verifyFoldReaderEquality(origin, folded string) error {
	beforeEntities, err := loadEntitiesFrom(origin)
	if err != nil {
		return fmt.Errorf("fold verify: load before: %w", err)
	}
	afterEntities, err := loadEntitiesFrom(folded)
	if err != nil {
		return fmt.Errorf("fold verify: load after: %w", err)
	}
	if !reflect.DeepEqual(beforeEntities, afterEntities) {
		return fmt.Errorf("fold verify: registry loader disagrees across encodings (%d before, %d after); registry left untouched", len(beforeEntities), len(afterEntities))
	}

	beforeTopics, err := topicEntitiesFrom(origin)
	if err != nil {
		return fmt.Errorf("fold verify: topics before: %w", err)
	}
	afterTopics, err := topicEntitiesFrom(folded)
	if err != nil {
		return fmt.Errorf("fold verify: topics after: %w", err)
	}
	if !reflect.DeepEqual(beforeTopics, afterTopics) {
		return fmt.Errorf("fold verify: topic reader disagrees across encodings (%d before, %d after); registry left untouched", len(beforeTopics), len(afterTopics))
	}
	return nil
}

func archiveRegistry(origin, archive string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(origin)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, hasher), src); err != nil {
		dst.Close()
		return "", err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func verifyArchiveFaithful(origin, archive, digest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("fold verify: reopen archive: %w", err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, f)
	f.Close()
	if copyErr != nil {
		return fmt.Errorf("fold verify: rehash archive: %w", copyErr)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != digest {
		return fmt.Errorf("fold verify: archive digest mismatch (wrote %s, read %s); registry left untouched", digest, got)
	}
	originEntities, err := loadEntitiesFrom(origin)
	if err != nil {
		return fmt.Errorf("fold verify: reload origin: %w", err)
	}
	archived, err := loadEntitiesFrom(archive)
	if err != nil {
		return fmt.Errorf("fold verify: load archive: %w", err)
	}
	if !reflect.DeepEqual(originEntities, archived) {
		return fmt.Errorf("fold verify: archive does not reproduce the pre-fold registry; registry left untouched")
	}
	return nil
}
