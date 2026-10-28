package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportReleaseGoalHoldsWritesAuthority(t *testing.T) {
	requireSQLite3(t)
	homePath := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "release-orchestrator-state.sqlite")
	createReleaseGoalDB(t, dbPath, `
insert into goals(goal_id, app_id, objective, state, artifact_dir, created_at, updated_at) values
  ('new-hold', 'phone-cleaner', 'Keep phone-cleaner release current', 'HOLD', '/tmp/new-hold', '2026-07-01T00:00:00', '2026-07-01T00:00:00'),
  ('old-hold', 'old-app', 'Pre gate hold', 'HOLD', '/tmp/old-hold', '2026-06-10T11:00:00', '2026-06-10T11:00:00'),
  ('approved', 'approved-app', 'Approved goal', 'APPROVED', '/tmp/approved', '2026-07-01T00:00:00', '2026-07-01T00:00:00');
`)

	report, err := ImportReleaseGoalHolds(homePath, dbPath, DefaultReleaseGoalHoldsGateLive, fixedNow)
	if err != nil {
		t.Fatalf("import release-goal holds: %v", err)
	}
	if report.AuthorityRef != ReleaseGoalHoldsAuthorityRef || report.HoldCount != 1 {
		t.Fatalf("bad import report: %+v", report)
	}
	if len(report.HoldIDs) != 1 || report.HoldIDs[0] != "new-hold" {
		t.Fatalf("bad hold ids: %v", report.HoldIDs)
	}
	if report.SourceAuthority.AuthorityStatus != "compound_primary" {
		t.Fatalf("bad source authority: %+v", report.SourceAuthority)
	}
	if _, err := os.Stat(ReleaseGoalHoldsPath(homePath)); err != nil {
		t.Fatalf("release-goal holds authority missing: %v", err)
	}
	authority, err := LoadReleaseGoalHoldsFromHome(homePath)
	if err != nil {
		t.Fatalf("load release-goal holds: %v", err)
	}
	if authority.HoldCount != 1 || authority.Holds[0].GoalID != "new-hold" || authority.Holds[0].AppID != "phone-cleaner" {
		t.Fatalf("unexpected authority: %+v", authority)
	}
	records, err := LoadReleaseGoalHoldsSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 || records[0].ID != report.SourceAuthorityID {
		t.Fatalf("unexpected records: %+v", records)
	}
}

func TestImportReleaseGoalHoldsRejectsInvalidSource(t *testing.T) {
	homePath := t.TempDir()
	_, err := ImportReleaseGoalHolds(homePath, filepath.Join(t.TempDir(), "missing.sqlite"), "", fixedNow)
	if err == nil {
		t.Fatal("expected missing sqlite source to fail")
	}
	if _, statErr := os.Stat(ReleaseGoalHoldsPath(homePath)); !os.IsNotExist(statErr) {
		t.Fatalf("authority should not be written on invalid input: %v", statErr)
	}
}

func createReleaseGoalDB(t *testing.T, dbPath, seed string) {
	t.Helper()
	schema := `
create table goals (
  goal_id text primary key,
  app_id text not null,
  objective text not null,
  state text not null,
  artifact_dir text not null,
  created_at text not null,
  updated_at text not null
);
create table packets (
  packet_id text primary key,
  goal_id text not null,
  lane text not null,
  state text not null,
  packet_json text not null,
  updated_at text not null
);
create table events (
  id integer primary key autoincrement,
  goal_id text not null,
  event_type text not null,
  payload_json text not null,
  created_at text not null
);
`
	cmd := exec.Command("sqlite3", dbPath)
	cmd.Stdin = strings.NewReader(schema + seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create sqlite db: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

func requireSQLite3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
}
