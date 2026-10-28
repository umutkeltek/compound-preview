package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCoordinationWaitViewKeepsExactSelectionWithoutQueueLeak(t *testing.T) {
	wb := WorkbenchReport{
		Open: []WorkbenchItem{
			{WorkID: "work_own_running", State: "running", Owner: "lane-a", ClaimedBy: "lane-a"},
			{WorkID: "work_own_held", State: "held", Owner: "lane-a"},
			{WorkID: "work_selected_foreign", State: "claimed", Owner: "lane-b", ClaimedBy: "lane-b"},
		},
		DispatchNext: WorkbenchDispatchNext{WorkID: "work_selected_foreign"},
		AdmissionPreviews: []WorkbenchAdmissionPreview{
			{WorkID: "work_own_preview", Actor: "lane-a"},
			{WorkID: "work_foreign_preview", Actor: "lane-b"},
		},
	}
	for i := 0; i < 500; i++ {
		wb.Open = append(wb.Open, WorkbenchItem{WorkID: fmt.Sprintf("work_foreign_%d", i), Owner: "lane-b", ClaimedBy: "lane-b", State: "running"})
	}
	wb.Counts.Open = len(wb.Open)
	view := coordinationWaitWorkbench(wb, "lane-a")
	if len(view.Open) != 3 || view.Counts.Open != 3 || len(view.AdmissionPreviews) != 1 || view.AdmissionPreviews[0].WorkID != "work_own_preview" {
		t.Fatalf("scope leak: items=%d previews=%v", len(view.Open), view.AdmissionPreviews)
	}
	reason, token := coordinationItemWait(view.Open[2], "lane-a")
	if reason != "claimed_by_other_actor" || token != "work.claimed_by_other_actor:work_selected_foreign" {
		t.Fatal("exact selected foreign relation lost")
	}
	if len(wb.Open) != 503 || wb.Counts.Open != 503 || len(wb.AdmissionPreviews) != 2 {
		t.Fatal("projection changed shared planning snapshot")
	}
}

func TestCoordinationConstraintReportIsDeterministic(t *testing.T) {
	s, _, _ := coordinationFixture(t)
	now := fixedNow.Add(10 * time.Minute)
	a, err := s.Coordination(now, CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Coordination(now, CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatal(err)
	}
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(left, right) {
		t.Fatal("unchanged snapshot produced different constraint report")
	}
}
