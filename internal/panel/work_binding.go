package panel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"compound/internal/envelope"
	"compound/internal/work"
)

type panelWorkBinding struct {
	bound  bool
	workID string
	task   envelope.TaskContract
}

func resolvePanelWorkBinding(homePath, workID string) (panelWorkBinding, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return panelWorkBinding{}, nil
	}
	if !strings.HasPrefix(workID, "work_") {
		return panelWorkBinding{}, fmt.Errorf("panel work_id %q must start with work_", workID)
	}
	store, err := work.Open(homePath)
	if err != nil {
		return panelWorkBinding{}, err
	}
	w, err := store.Get(workID)
	if err != nil {
		return panelWorkBinding{}, fmt.Errorf("resolve panel work_id %q: %w", workID, err)
	}
	if w.ID != workID {
		return panelWorkBinding{}, fmt.Errorf("panel work_id %q resolved artifact %q", workID, w.ID)
	}
	if err := w.Validate(); err != nil {
		return panelWorkBinding{}, fmt.Errorf("panel work_id %q invalid: %w", workID, err)
	}
	if panelWorkTerminal(w.State) {
		return panelWorkBinding{}, fmt.Errorf("panel work_id %q is terminal (%s)", workID, w.State)
	}
	task := taskContractFromWork(w)
	if contextPackID := strings.TrimSpace(w.IDs.ContextPackID); contextPackID != "" {
		path := filepath.Join(homePath, "context", "packs", contextPackID+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return panelWorkBinding{}, fmt.Errorf("read panel work context pack %q: %w", contextPackID, err)
		}
		var pack envelope.ContextPack
		if err := json.Unmarshal(data, &pack); err != nil {
			return panelWorkBinding{}, fmt.Errorf("decode panel work context pack %q: %w", contextPackID, err)
		}
		if pack.ID != contextPackID {
			return panelWorkBinding{}, fmt.Errorf("panel work context pack %q resolved artifact %q", contextPackID, pack.ID)
		}
		if err := pack.Validate(); err != nil {
			return panelWorkBinding{}, fmt.Errorf("panel work context pack %q invalid: %w", contextPackID, err)
		}
		if !pack.TaskContract.IsZero() {
			task = pack.TaskContract
		}
	}
	return panelWorkBinding{bound: true, workID: workID, task: task}, nil
}

func validatePanelWorkBinding(homePath string, run Run) error {
	if !run.WorkBound {
		return nil
	}
	binding, err := resolvePanelWorkBinding(homePath, run.WorkID)
	if err != nil {
		return err
	}
	if !binding.bound || binding.workID != run.WorkID {
		return fmt.Errorf("panel run %s lost work binding %q", run.ID, run.WorkID)
	}
	if !reflect.DeepEqual(binding.task, run.TaskContract) {
		return fmt.Errorf("panel run %s task contract drifted for work_id %s", run.ID, run.WorkID)
	}
	return nil
}

func taskContractFromWork(w envelope.WorkEnvelope) envelope.TaskContract {
	intent := strings.TrimSpace(w.DesiredOutcome)
	if intent == "" {
		intent = strings.TrimSpace(w.SuccessPredicate)
	}
	return envelope.TaskContract{
		Intent:           intent,
		Mode:             "execute",
		DesiredOutcome:   strings.TrimSpace(w.DesiredOutcome),
		SuccessPredicate: strings.TrimSpace(w.SuccessPredicate),
		NonGoals:         append([]string(nil), w.NonGoals...),
		ProofMedium:      strings.TrimSpace(w.ProofMedium),
	}
}

func firstPanelTaskText(task envelope.TaskContract) string {
	for _, value := range []string{task.Intent, task.DesiredOutcome, task.SuccessPredicate} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func panelWorkTerminal(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}
