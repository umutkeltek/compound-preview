package mem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// MigrationSnapshot is an owner-defined, point-in-time copy of the memory
// authority's append-only logs and shared sequence counter. Its byte fields
// preserve the files exactly as stored.
type MigrationSnapshot struct {
	ClaimsJSONL []byte
	EdgesJSONL  []byte
	Seq         []byte
	Counter     int64
	HighWater   int64
}

// MigrationSnapshot captures and validates the migration source bytes while
// holding the same shared memory lock used by other stable owner reads.
func (s *Store) MigrationSnapshot() (MigrationSnapshot, error) {
	var snapshot MigrationSnapshot
	lockPath := filepath.Join(s.home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return snapshot, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return snapshot, err
	}
	defer lockFile.Close()
	if s.beforeLockObserver != nil {
		s.beforeLockObserver()
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return snapshot, err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	claims, err := readMigrationSnapshotFile(s.claimsLog())
	if err != nil {
		return snapshot, fmt.Errorf("read claims migration snapshot: %w", err)
	}
	edges, err := readMigrationSnapshotFile(s.edgesLog())
	if err != nil {
		return snapshot, fmt.Errorf("read edges migration snapshot: %w", err)
	}
	seq, err := readMigrationSnapshotFile(s.seqFile())
	if err != nil {
		return snapshot, fmt.Errorf("read seq migration snapshot: %w", err)
	}

	claimHighWater, err := validateMigrationRows[Claim](claims, "claim")
	if err != nil {
		return snapshot, err
	}
	edgeHighWater, err := validateMigrationRows[Edge](edges, "edge")
	if err != nil {
		return snapshot, err
	}
	highWater := claimHighWater
	if edgeHighWater > highWater {
		highWater = edgeHighWater
	}
	counter, err := parseMigrationCounter(seq)
	if err != nil {
		return snapshot, err
	}
	if counter < highWater {
		return snapshot, fmt.Errorf("seq counter %d below ledger high-water %d", counter, highWater)
	}

	return MigrationSnapshot{
		ClaimsJSONL: append([]byte(nil), claims...),
		EdgesJSONL:  append([]byte(nil), edges...),
		Seq:         append([]byte(nil), seq...),
		Counter:     counter,
		HighWater:   highWater,
	}, nil
}

func readMigrationSnapshotFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte{}, nil
		}
		return nil, err
	}
	return data, nil
}

func validateMigrationRows[T interface{ Claim | Edge }](data []byte, kind string) (int64, error) {
	var highWater int64
	for index, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row T
		if err := json.Unmarshal(line, &row); err != nil {
			return 0, fmt.Errorf("malformed %s row %d: %w", kind, index+1, err)
		}
		var seq int64
		switch value := any(row).(type) {
		case Claim:
			if strings.TrimSpace(value.ID) == "" {
				return 0, fmt.Errorf("invalid %s row %d: id is required", kind, index+1)
			}
			seq = value.Seq
		case Edge:
			if strings.TrimSpace(value.Kind) == "" || strings.TrimSpace(value.From) == "" || strings.TrimSpace(value.To) == "" {
				return 0, fmt.Errorf("invalid %s row %d: kind, from, and to are required", kind, index+1)
			}
			seq = value.Seq
		}
		if seq <= 0 {
			return 0, fmt.Errorf("invalid %s row %d: seq must be positive", kind, index+1)
		}
		if seq > highWater {
			highWater = seq
		}
	}
	return highWater, nil
}

func parseMigrationCounter(data []byte) (int64, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, nil
	}
	counter, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed seq counter %q: %w", text, err)
	}
	if counter < 0 {
		return 0, fmt.Errorf("malformed seq counter %q: must be non-negative", text)
	}
	return counter, nil
}
