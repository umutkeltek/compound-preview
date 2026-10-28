package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestDelegatedSessionsBatchMatchesPointReduction(t *testing.T) {
	s := newStore(t)
	byWork := map[string][]Event{}
	sequence := []struct{ work, lane, kind, session string }{
		{"work_batch_a", "one", "spawned", "first"},
		{"work_batch_a", "one", "steered", "first"},
		{"work_batch_a", "one", "exited", "first"},
		{"work_batch_a", "one", "refused", ""},
		{"work_batch_a", "two", "spawned", "second"},
		{"work_batch_b", "one", "spawned", "old"},
		{"work_batch_b", "one", "spawned", "new"},
		{"work_batch_b", "one", "exited", "old"},
	}
	for i, item := range sequence {
		id := fmt.Sprintf("wde_batch_%d", i)
		dev := delegateEvent{SchemaVersion: delegateEventSchemaVersion, ID: id,
			WorkID: item.work, Lane: item.lane, Harness: "claude", Holder: "fixture",
			Kind: "work.delegate." + item.kind, Session: item.session,
			TS: ts(fixedNow.Add(time.Duration(i) * time.Second)), Reason: "capacity"}
		if err := s.writeEventJSON(id, dev); err != nil {
			t.Fatal(err)
		}
		// Every occurrence is reduced, even when the same record was fetched once.
		byWork[item.work] = append(byWork[item.work], Event{WorkID: item.work, Attached: []string{id, id}})
	}
	want := map[string]DelegatedSessionRow{}
	ra := runattempt.Open(s.home)
	for workID, events := range byWork {
		rows, err := s.reduceDelegatedSessionsForWork(workID, events, ra)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			want[row.WorkID+"/"+row.Lane] = row
		}
	}
	rows, counts, err := s.reduceDelegatedSessions(byWork)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DelegatedSessionRow{}
	wantCounts := DelegatedSessionCounts{Total: len(want)}
	for _, row := range rows {
		got[row.WorkID+"/"+row.Lane] = row
	}
	for _, row := range want {
		switch row.Status {
		case "running":
			wantCounts.Running++
		case "exited":
			wantCounts.Exited++
		case "refused":
			wantCounts.Refused++
		default:
			wantCounts.Unknown++
		}
	}
	if !reflect.DeepEqual(got, want) || counts != wantCounts {
		t.Fatalf("batch rows/counts differ from point reduction: got=%+v %+v want=%+v %+v", got, counts, want, wantCounts)
	}
}

func TestDelegatedSessionsBatchPreservesNativeErrors(t *testing.T) {
	for _, mode := range []string{"missing", "wrong_work", "wrong_schema", "malformed_field"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			workID, id := "work_batch_error", "wde_batch_error"
			dev := delegateEvent{SchemaVersion: delegateEventSchemaVersion, ID: id,
				WorkID: workID, Kind: "work.delegate.refused", Reason: "capacity", TS: ts(fixedNow)}
			var body any = dev
			switch mode {
			case "wrong_work":
				dev.WorkID = "work_other"
				body = dev
			case "wrong_schema":
				dev.SchemaVersion = "unsupported"
				body = dev
			case "malformed_field":
				body = map[string]any{"id": id, "schema_version": delegateEventSchemaVersion, "ts": 123}
			}
			if mode != "missing" {
				if err := s.writeEventJSON(id, body); err != nil {
					t.Fatal(err)
				}
			}
			events := []Event{{WorkID: workID, Attached: []string{id, id}}}
			_, pointErr := s.reduceDelegatedSessionsForWork(workID, events, runattempt.Open(s.home))
			_, _, batchErr := s.reduceDelegatedSessions(map[string][]Event{workID: events})
			if pointErr == nil || batchErr == nil || pointErr.Error() != batchErr.Error() {
				t.Fatalf("batch=%v point=%v", batchErr, pointErr)
			}
			if errors.Is(pointErr, os.ErrNotExist) != errors.Is(batchErr, os.ErrNotExist) {
				t.Fatalf("missing-error class changed: batch=%v point=%v", batchErr, pointErr)
			}
			var pointType, batchType *json.UnmarshalTypeError
			if errors.As(pointErr, &pointType) != errors.As(batchErr, &batchType) {
				t.Fatalf("JSON-error class changed: batch=%v point=%v", batchErr, pointErr)
			}
		})
	}
}

func TestDelegatedSessionsBatchValidatesRepeatedCrossWorkAttachment(t *testing.T) {
	s, first, second := coordinationFixture(t)
	id, err := s.recordDelegateRefused(first, "claude", "lane", "fixture", "capacity", "private detail", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendDelegateLifecycleEvent(first, "running", "fixture", id, "fixture attachment", fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.appendDelegateLifecycleEvent(second, "claimed", "fixture", id, "wrong work fixture", fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	now := fixedNow.Add(10 * time.Minute)
	bench, err := s.Workbench(now, WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, want := s.reduceDelegatedSessionsForWork(second, bench.eventsByWork[second], runattempt.Open(s.home))
	if want == nil {
		t.Fatal("point reducer must reject the cross-work attachment")
	}
	_, projectionErr := s.ExecutorsProjection(now, ExecutorsProjectionOptions{Workbench: &bench})
	if projectionErr == nil || projectionErr.Error() != want.Error() {
		t.Fatalf("ExecutorsProjection=%v point=%v", projectionErr, want)
	}
	_, coordinationErr := s.Coordination(now, CoordinationOptions{})
	if coordinationErr == nil || coordinationErr.Error() != want.Error() {
		t.Fatalf("Coordination=%v point=%v", coordinationErr, want)
	}
}
