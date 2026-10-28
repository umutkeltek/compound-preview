package pg

import (
	"strings"
	"testing"
)

func TestSchemaDDLObservationDate(t *testing.T) {
	collapsed := strings.Join(strings.Fields(schemaDDL(768)), " ")
	if !strings.Contains(collapsed, "candidates ( id TEXT PRIMARY KEY, entity TEXT NOT NULL, predicate TEXT NOT NULL, object TEXT NOT NULL, observation_date TEXT") {
		t.Errorf("candidates table DDL must declare the observation_date column")
	}
	if !strings.Contains(collapsed, "ALTER TABLE candidates ADD COLUMN IF NOT EXISTS observation_date TEXT") {
		t.Errorf("EnsureSchema must idempotently add the observation_date column for existing databases")
	}
}
