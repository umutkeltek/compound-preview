package operatornotice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/attention"
)

// presentCardsGolden is the card stream rendered on the B1 tip (07cd39216),
// before away mode existed. While the operator is present, every card must stay
// byte-identical to it: away mode may only change what happens inside an
// active away window.
const presentCardsGolden = "testdata/cards_present.golden"

// goldenFrontier is a fixed needs-you frontier covering the three card shapes:
// a decidable review hold, an actionless workbench hold, and a hold whose
// summary is empty (the fallback ask line).
func goldenFrontier() attention.Report {
	decidable := needsYouGroup("route-promotions", "work_golden_a", true)
	actionless := needsYouGroup("workbench-holds", "work_golden_b", false)
	blank := needsYouGroup("blank-summary", "work_golden_c", true)
	blank.OperatorSummary = ""
	return reportWith(decidable, actionless, blank)
}

// captureCards runs NotifyFromInputs with the channel cap lifted and returns
// every card body handed to the act seam, in emit order.
func captureCards(t *testing.T, homePath string, in attention.Report, now time.Time) string {
	t.Helper()
	prevCap := MaxOpenHolds
	MaxOpenHolds = 10
	t.Cleanup(func() { MaxOpenHolds = prevCap })
	prev := executeFn
	var bodies []string
	executeFn = func(_ string, _ string, body string, _ string, _ time.Time) (string, string, error) {
		bodies = append(bodies, body)
		return "rec_golden", "tr_golden", nil
	}
	t.Cleanup(func() { executeFn = prev })
	if _, err := NotifyFromInputs(homePath, "buzz", in, now); err != nil {
		t.Fatalf("notify: %v", err)
	}
	return strings.Join(bodies, "\n---\n") + "\n"
}

// TestPresentCardsMatchB1Golden pins "present = byte-identical to today". The
// fixture was rendered on B1 with UPDATE_GOLDEN=1 before any away-mode code
// existed; it is never regenerated from the code under test.
func TestPresentCardsMatchB1Golden(t *testing.T) {
	homePath := seedHeldHome(t, "work_golden_a", "work_golden_b", "work_golden_c")
	got := captureCards(t, homePath, goldenFrontier(), fixedNow)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(presentCardsGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(presentCardsGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("present cards drifted from the B1 golden.\nwant:\n%s\ngot:\n%s", want, got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "operator")); !os.IsNotExist(err) {
		t.Fatalf("a present flush must not create the return queue (stat err=%v)", err)
	}
}
