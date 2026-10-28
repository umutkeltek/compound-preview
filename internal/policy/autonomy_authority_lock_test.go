package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	homepkg "compound/internal/home"
)

func TestAutonomyAdmissionAuthoritiesShareLock(t *testing.T) {
	tests := []struct {
		name  string
		write func(home string) error
	}{
		{
			name: "editable surfaces",
			write: func(home string) error {
				return WriteEditableSurfaces(home, EditableSurfaces{})
			},
		},
		{
			name: "operator state",
			write: func(home string) error {
				source := filepath.Join(home, "operator-state-source.json")
				if err := os.WriteFile(source, []byte(`{"autonomy_lever":{"level":"L1"}}`), 0o644); err != nil {
					return err
				}
				_, err := ImportOperatorState(home, source, fixedNow)
				return err
			},
		},
		{
			name: "standing grants",
			write: func(home string) error {
				source := filepath.Join(home, "standing-grants-source.yaml")
				body := []byte("authority:\n  decision_ref: \"grant:test-lock\"\n  reviewed_by: \"operator:test\"\ngrants:\n  - grant_id: test\n    capabilities: [\"work.advance\"]\n    target_scope: \"work/*\"\n    reversibility_ceiling: reversible\n")
				if err := os.WriteFile(source, body, 0o644); err != nil {
					return err
				}
				_, err := ImportStandingGrants(home, source, fixedNow)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			started := make(chan struct{})
			done := make(chan error, 1)
			if err := homepkg.WithAutonomyAdmissionLock(home, func() error {
				go func() {
					close(started)
					done <- tt.write(home)
				}()
				<-started
				select {
				case err := <-done:
					t.Fatalf("authority writer crossed admission lock: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
