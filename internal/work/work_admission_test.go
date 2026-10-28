package work

import (
	"testing"

	homepkg "compound/internal/home"
)

func TestCreatePlanAdmissionUsesSharedWorkAdmissionLock(t *testing.T) {
	store := newStore(t)
	workID := "work_create_plan_shared_admission"
	if err := store.withCreatePlanAdmissionLock(workID, func() error {
		acquired, err := homepkg.TryWithWorkAdmissionLock(store.home, workID, func() error { return nil })
		if err != nil {
			return err
		}
		if acquired {
			t.Fatal("shared work admission inode was not held by create-plan admission")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCloseHoldsSharedWorkAdmissionThroughTerminalization(t *testing.T) {
	store := newStore(t)
	work := atomicCloseWork(t, store)
	original := closeAfterTerminalizationPrepare
	t.Cleanup(func() { closeAfterTerminalizationPrepare = original })
	checked := false
	closeAfterTerminalizationPrepare = func() {
		acquired, err := homepkg.TryWithWorkAdmissionLock(store.home, work.ID, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			t.Fatal("close terminalization ran outside shared work admission")
		}
		checked = true
	}
	if _, err := store.Close(work.ID, atomicCloseInput(), fixedNow.Add(4)); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("close did not reach terminalization admission assertion")
	}
}
