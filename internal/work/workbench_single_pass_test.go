package work

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWorkbenchReadsEventLogOnce(t *testing.T) {
	t.Parallel()
	const (
		workCount     = 200
		eventsPerWork = 5
	)

	s := newStore(t)
	var eventLog strings.Builder
	for i := 0; i < workCount; i++ {
		workID := fmt.Sprintf("work_single_pass_%03d", i)
		createdAt := fixedNow.Add(time.Duration(i) * time.Second)
		w := BuildWork(CreateInput{
			WorkID:           workID,
			Owner:            "compound-core",
			SuccessPredicate: "workbench reads the lifecycle log once",
		}, createdAt)
		if err := s.writeJSON(s.workPath(workID), &w); err != nil {
			t.Fatalf("write work %s: %v", workID, err)
		}

		states := [][2]string{
			{"", "proposed"},
			{"proposed", "admitted"},
			{"admitted", "ready"},
			{"ready", "held"},
			{"held", "ready"},
		}
		for step, state := range states {
			event, err := json.Marshal(Event{
				TS:     createdAt.Add(time.Duration(step) * time.Millisecond).UTC().Format(time.RFC3339Nano),
				WorkID: workID,
				From:   state[0],
				To:     state[1],
				By:     "test",
			})
			if err != nil {
				t.Fatalf("marshal event for %s: %v", workID, err)
			}
			eventLog.Write(event)
			eventLog.WriteByte('\n')
		}
	}
	if err := os.WriteFile(s.eventsLog(), []byte(eventLog.String()), 0o644); err != nil {
		t.Fatalf("write event fixture: %v", err)
	}

	readsBefore := s.eventLogReadCount()
	if _, err := s.Workbench(fixedNow.Add(time.Hour), WorkbenchOptions{RecentLimit: workCount}); err != nil {
		t.Fatalf("workbench: %v", err)
	}
	reads := s.eventLogReadCount() - readsBefore
	if reads != 1 {
		t.Fatalf("event_log_reads=%d want=1 (fixture_works=%d events_per_work=%d)", reads, workCount, eventsPerWork)
	}
}
