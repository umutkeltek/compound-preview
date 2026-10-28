package capgraph

import (
	"encoding/json"
	"fmt"
	"io"
)

// RunCompact is the body of the `compound capability compact` CLI verb,
// extracted from cmd/compound/capability.go in slice 7 so the ratchet test
// (TestCheckCmdCompoundBudgetPassesOnRealRepo) can read green without the
// budget law's "never raise the ceiling" rule. The behaviour is
// unchanged: CompactOrPreview by write flag, JSON or plain text by jsonOut.
// Returns 0 on success, 1 on any error (caller maps to os.Exit).
func RunCompact(store *Store, write, jsonOut bool, stdout, stderr io.Writer) int {
	r, err := store.CompactOrPreview(write)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# capability compact rows_before=%d rows_after=%d duplicate_ids=%d\n",
		r.RowsBefore, r.RowsAfter, r.DuplicateIDs)
	return 0
}
