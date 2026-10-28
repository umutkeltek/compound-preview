package mem

import (
	"os"
	"strings"
	"testing"
)

// A crash between Write and fsync leaves a torn final line in a ledger log.
// The loader must fold the durable prefix (treating the torn append as
// never-written) instead of wedging the whole ledger on one unparseable line.
func TestLoadClaimsRawToleratesTornFinalLine(t *testing.T) {
	s := newStore(t)
	good := `{"id":"claim_a","seq":1,"entity":"e","predicate":"p","object":"v1"}`
	if err := os.WriteFile(s.claimsLog(), []byte(good+"\n{\"id\":\"claim_b\",\"seq\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	claims, err := s.loadClaimsRaw()
	if err != nil {
		t.Fatalf("torn final line must not wedge the ledger: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != "claim_a" {
		t.Fatalf("folded claims = %+v, want only the durable claim_a", claims)
	}
}

// The same tolerance applies to a terminated-but-garbled final line (a record
// body partially flushed before its newline landed in a prior life).
func TestLoadClaimsRawToleratesGarbledFinalLine(t *testing.T) {
	s := newStore(t)
	good := `{"id":"claim_a","seq":1,"entity":"e","predicate":"p","object":"v1"}`
	if err := os.WriteFile(s.claimsLog(), []byte(good+"\n{garbled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claims, err := s.loadClaimsRaw()
	if err != nil {
		t.Fatalf("garbled final line must be tolerated as never-written: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != "claim_a" {
		t.Fatalf("folded claims = %+v, want only the durable claim_a", claims)
	}
}

// Tolerance is scoped to the FINAL line: an unparseable line with valid
// records after it is mid-file corruption and must fail loud — and a
// syntactically valid line that fails the typed decode is corruption, not a
// tear, even in final position.
func TestLoadClaimsRawFailsLoudPastFinalLine(t *testing.T) {
	s := newStore(t)
	good := `{"id":"claim_a","seq":1}`
	for name, body := range map[string]string{
		"mid-file corruption":        good + "\nnot json\n" + good + "\n",
		"typed-decode final failure": good + "\n" + `{"seq":"not-a-number"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(s.claimsLog(), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := s.loadClaimsRaw(); err == nil {
				t.Fatalf("%s must fail loud, not be tolerated", name)
			}
		})
	}
}

// The append door must repair a torn tail before writing: appending onto the
// fragment would glue the new record to it into one permanently unparseable
// line. After the append, the fragment is gone and every line folds.
func TestAppendJSONLRepairsTornTailInsteadOfGluing(t *testing.T) {
	s := newStore(t)
	good := `{"id":"claim_a","seq":1}`
	if err := os.WriteFile(s.claimsLog(), []byte(good+"\n{\"id\":\"torn\",\"seq\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.claimsLog(), &Claim{ID: "claim_b", Seq: 2}); err != nil {
		t.Fatalf("append after a torn tail must repair, not fail or glue: %v", err)
	}
	claims, err := s.loadClaimsRaw()
	if err != nil {
		t.Fatalf("post-repair log must fold: %v", err)
	}
	if len(claims) != 2 || claims[0].ID != "claim_a" || claims[1].ID != "claim_b" {
		t.Fatalf("claims = %+v, want claim_a + claim_b (torn fragment dropped)", claims)
	}
	data, err := os.ReadFile(s.claimsLog())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "torn") {
		t.Fatalf("torn fragment must be truncated away, got %q", data)
	}
}

// Repair must never hide mid-file corruption: when the kept prefix itself has
// an unparseable line, the append refuses and the file is left untouched for
// forensics.
func TestAppendJSONLRefusesRepairPastMidFileCorruption(t *testing.T) {
	s := newStore(t)
	body := "not json\n" + `{"id":"claim_a","seq":1}` + "\n{torn"
	if err := os.WriteFile(s.claimsLog(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.claimsLog(), &Claim{ID: "claim_b", Seq: 2}); err == nil {
		t.Fatal("append must fail loud when the prefix behind a torn tail is corrupt")
	}
	data, err := os.ReadFile(s.claimsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("mid-file-corrupt log must be left untouched, got %q", data)
	}
}
