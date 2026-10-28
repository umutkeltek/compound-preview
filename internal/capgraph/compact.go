package capgraph

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CompactReport is what CompactNodes and PreviewCompact hand back. RowsBefore
// is the byte-count of non-blank lines pre-call; RowsAfter is the deduped node
// count (loadNodes() returns exactly that many); DuplicateIDs is the count of
// ids that appeared more than once. DuplicateIDList lists the affected ids in
// stable order so a dry-run output tells the operator exactly what would
// change. The 3-tuple {RowsBefore, RowsAfter, DuplicateIDs} is what F2 pins;
// DuplicateIDList is observable additive so the operator can audit a run
// without re-scanning the log.
type CompactReport struct {
	RowsBefore           int      `json:"rows_before"`
	RowsAfter            int      `json:"rows_after"`
	DuplicateIDs         int      `json:"duplicate_ids"`
	DuplicateIDList      []string `json:"duplicate_id_list,omitempty"`
	MalformedRows        int      `json:"malformed_rows,omitempty"`
	MalformedLineNumbers []int    `json:"malformed_line_numbers,omitempty"`
}

// CompactOrPreview dispatches by write flag so the CLI verb stays a single
// call site (cmd/compound budget ratchet -- see slice 4 report). write=true
// rewrites the log; write=false reports only. The contract is otherwise
// identical: byte-equivalent loadNodes() before vs after, same report shape.
func (s *Store) CompactOrPreview(write bool) (CompactReport, error) {
	if write {
		return s.CompactNodes()
	}
	return s.PreviewCompact()
}

// PreviewCompact reports what CompactNodes WOULD do without rewriting the log.
// Operators run this first to see the duplicate count and the affected ids
// before opting in with `compound capability compact --yes`.
//
// The byte-equivalence safety proof holds at read-time only: a Preview's
// report is computed from the same loadNodes the deduped log would produce,
// but on the CURRENT (dirty) log -- that is intentional. A preview over a
// dirty log and a compact that subsequently dedupes to the same nodes are
// the same answer; both honour F2's "byte-for-byte loadNodes equivalence".
func (s *Store) PreviewCompact() (CompactReport, error) {
	report, _, err := computeCompactReport(s.nodesLog())
	return report, err
}

// CompactNodes collapses the append-only nodes log to one row per id
// (last-wins-first-seen, byte-equivalent to what loadNodes already returns
// from a dirty log). It is the one-shot remedy for the 302-row duplicate-id
// pile measured 2026-08-25 in capability-nodes.jsonl, and the safety net
// behind ApplySkillManifest's "within one import" guarantee. The log is
// rewritten atomically (temp file in the same dir, then rename), so a crash
// mid-Compact leaves the previous bytes intact. F2 pins the report shape and
// the loadNodes-before vs loadNodes-after byte equivalence.
func (s *Store) CompactNodes() (CompactReport, error) {
	// The rewrite here is the dangerous one: without the graph write lock, a
	// node APPENDED between computeCompactReport's read and the rename below
	// is silently erased — compaction converting concurrency into deletion.
	// The lock spans load-validate-rewrite so an in-flight Add either lands
	// before the read (and survives) or after the rename (and lands on the
	// compacted log).
	return s.compactNodesLocked()
}

func (s *Store) compactNodesLocked() (CompactReport, error) {
	var report CompactReport
	err := s.withGraphWriteLock(func() error {
		var nodes []Node
		var cerr error
		report, nodes, cerr = computeCompactReport(s.nodesLog())
		if cerr != nil {
			return cerr
		}
		return s.rewriteNodesAtomic(nodes)
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

// computeCompactReport is the shared core of PreviewCompact and CompactNodes.
// It reads the raw nodes log line-by-line, dedupes last-wins-first-seen the
// same way loadNodes does (so the post-compact loadNodes cannot differ), and
// emits the per-id duplicate counts for the report.
func computeCompactReport(logPath string) (CompactReport, []Node, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return CompactReport{RowsBefore: 0, RowsAfter: 0}, nil, nil
		}
		return CompactReport{}, nil, fmt.Errorf("open nodes log: %w", err)
	}
	defer f.Close()

	byID := map[string]Node{}
	order := []string{}
	var rawIDs []string
	rawRows := 0
	lineNumber := 0
	var malformedLines []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		lineNumber++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rawRows++
		var n Node
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			// Compaction is a rewrite, not a repair door. Keep scanning so the
			// report names every malformed line, then refuse before rename.
			malformedLines = append(malformedLines, lineNumber)
			continue
		}
		rawIDs = append(rawIDs, n.ID)
		if _, seen := byID[n.ID]; !seen {
			order = append(order, n.ID)
		}
		byID[n.ID] = n
	}
	if err := sc.Err(); err != nil {
		return CompactReport{}, nil, fmt.Errorf("scan nodes log: %w", err)
	}

	nodes := make([]Node, 0, len(order))
	for _, id := range order {
		nodes = append(nodes, byID[id])
	}

	counts := map[string]int{}
	for _, id := range rawIDs {
		counts[id]++
	}
	report := CompactReport{
		RowsBefore:           rawRows,
		RowsAfter:            len(nodes),
		MalformedRows:        len(malformedLines),
		MalformedLineNumbers: malformedLines,
	}
	for id, n := range counts {
		if n < 2 {
			continue
		}
		report.DuplicateIDs++
		report.DuplicateIDList = append(report.DuplicateIDList, id)
	}
	sort.Strings(report.DuplicateIDList)
	if report.MalformedRows > 0 {
		return report, nil, fmt.Errorf("nodes log contains %d malformed row(s) at lines %v; use an explicit repair operation", report.MalformedRows, report.MalformedLineNumbers)
	}
	return report, nodes, nil
}

// rewriteNodesAtomic replaces the nodes log wholesale via a temp file in the
// SAME directory and a rename. Same-dir + rename is atomic-replace on POSIX
// (the inode is replaced; readers see the old file or the new one, never a
// half-new file). This is the only path that may replace the log mid-life --
// normal writes go through appendJSONL.
//
// Empty nodes is allowed: an empty rewrite leaves a zero-byte log, which
// readJSONL treats as "zero rows" (TrimSpace -> "", Split -> [""], empty).
// That keeps a clean delete indistinguishable from an absent file at load
// time, so neither caller code nor loadNodes needs a special case.
func (s *Store) rewriteNodesAtomic(nodes []Node) error {
	logPath := s.nodesLog()
	dir := filepath.Dir(logPath)
	tmp, err := os.CreateTemp(dir, "capability-nodes.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp for nodes log: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if len(nodes) > 0 {
		enc := json.NewEncoder(tmp)
		for i := range nodes {
			if err := enc.Encode(&nodes[i]); err != nil {
				_ = tmp.Close()
				return fmt.Errorf("write nodes log temp: %w", err)
			}
		}
	} else {
		// An empty list still needs a valid file on disk so the rename below
		// can replace the live log with a zero-byte one (readJSONL handles
		// empty bodies cleanly).
		if _, err := tmp.WriteString(""); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("write empty nodes log temp: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync nodes log temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close nodes log temp: %w", err)
	}
	if err := os.Rename(tmpName, logPath); err != nil {
		return fmt.Errorf("rename nodes log temp into place: %w", err)
	}
	committed = true
	return nil
}
