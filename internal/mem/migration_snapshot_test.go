package mem

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestMigrationSnapshotPreservesRawLedgerBytesAndHighWater(t *testing.T) {
	s := newStore(t)
	claims := []byte(" {\"id\":\"claim-1\",\"seq\":2}\r\n\n{\"id\":\"claim-2\",\"seq\":5} \n")
	edges := []byte("{\"seq\":7,\"kind\":\"relates\",\"from\":\"a\",\"to\":\"b\",\"confidence\":1,\"created_at\":\"2026-08-01T00:00:00Z\"}\r\n")
	seq := []byte(" 9\n")
	writeMigrationSnapshotFixture(t, s.claimsLog(), claims)
	writeMigrationSnapshotFixture(t, s.edgesLog(), edges)
	writeMigrationSnapshotFixture(t, s.seqFile(), seq)

	snapshot, err := s.MigrationSnapshot()
	if err != nil {
		t.Fatalf("MigrationSnapshot: %v", err)
	}
	if !bytes.Equal(snapshot.ClaimsJSONL, claims) {
		t.Fatalf("claims bytes = %q, want %q", snapshot.ClaimsJSONL, claims)
	}
	if !bytes.Equal(snapshot.EdgesJSONL, edges) {
		t.Fatalf("edges bytes = %q, want %q", snapshot.EdgesJSONL, edges)
	}
	if !bytes.Equal(snapshot.Seq, seq) {
		t.Fatalf("seq bytes = %q, want %q", snapshot.Seq, seq)
	}
	if snapshot.HighWater != 7 {
		t.Fatalf("high-water = %d, want 7", snapshot.HighWater)
	}
	if snapshot.Counter != 9 {
		t.Fatalf("counter = %d, want 9", snapshot.Counter)
	}
}

func TestMigrationSnapshotReturnsDeepCopies(t *testing.T) {
	s := newStore(t)
	claims := []byte("{\"id\":\"claim-1\",\"seq\":1}\n")
	writeMigrationSnapshotFixture(t, s.claimsLog(), claims)
	writeMigrationSnapshotFixture(t, s.seqFile(), []byte("1"))

	first, err := s.MigrationSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	first.ClaimsJSONL[0] = 'x'
	first.EdgesJSONL = append(first.EdgesJSONL, 'x')
	first.Seq[0] = '9'

	second, err := s.MigrationSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.ClaimsJSONL, claims) || len(second.EdgesJSONL) != 0 || string(second.Seq) != "1" {
		t.Fatalf("later snapshot was mutated through prior result: %+v", second)
	}
}

func TestMigrationSnapshotRejectsMalformedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(*Store) string
		data []byte
	}{
		{name: "claim", path: (*Store).claimsLog, data: []byte("{\"id\":\"ok\",\"seq\":1}\nnot-json\n")},
		{name: "edge", path: (*Store).edgesLog, data: []byte("{\"seq\":1,\"kind\":\"relates\",\"from\":\"a\",\"to\":\"b\"}\n{broken}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeMigrationSnapshotFixture(t, tc.path(s), tc.data)
			writeMigrationSnapshotFixture(t, s.seqFile(), []byte("1"))
			if _, err := s.MigrationSnapshot(); err == nil || !strings.Contains(err.Error(), "malformed "+tc.name+" row") {
				t.Fatalf("error = %v, want malformed %s row", err, tc.name)
			}
		})
	}
}

func TestMigrationSnapshotRejectsInvalidCounter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []byte
		edges  []byte
		seq    string
		want   string
	}{
		{name: "malformed", seq: "not-a-sequence", want: "malformed seq counter"},
		{name: "negative", seq: "-1", want: "must be non-negative"},
		{name: "lagging claims", claims: []byte("{\"id\":\"claim-1\",\"seq\":5}\n"), seq: "4", want: "below ledger high-water 5"},
		{name: "lagging edges", edges: []byte("{\"seq\":7,\"kind\":\"relates\",\"from\":\"a\",\"to\":\"b\"}\n"), seq: "6", want: "below ledger high-water 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeMigrationSnapshotFixture(t, s.claimsLog(), tc.claims)
			writeMigrationSnapshotFixture(t, s.edgesLog(), tc.edges)
			writeMigrationSnapshotFixture(t, s.seqFile(), []byte(tc.seq))
			if _, err := s.MigrationSnapshot(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func writeMigrationSnapshotFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSnapshotRejectsSemanticallyInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(*Store) string
		data []byte
		want string
	}{
		{name: "null claim", path: (*Store).claimsLog, data: []byte("null\n"), want: "id is required"},
		{name: "claim missing id", path: (*Store).claimsLog, data: []byte("{\"seq\":1}\n"), want: "id is required"},
		{name: "claim zero seq", path: (*Store).claimsLog, data: []byte("{\"id\":\"claim-1\",\"seq\":0}\n"), want: "seq must be positive"},
		{name: "claim negative seq", path: (*Store).claimsLog, data: []byte("{\"id\":\"claim-1\",\"seq\":-1}\n"), want: "seq must be positive"},
		{name: "edge missing identity", path: (*Store).edgesLog, data: []byte("{\"seq\":1}\n"), want: "kind, from, and to are required"},
		{name: "edge zero seq", path: (*Store).edgesLog, data: []byte("{\"seq\":0,\"kind\":\"relates\",\"from\":\"a\",\"to\":\"b\"}\n"), want: "seq must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeMigrationSnapshotFixture(t, tc.path(s), tc.data)
			writeMigrationSnapshotFixture(t, s.seqFile(), []byte("1"))
			if _, err := s.MigrationSnapshot(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
