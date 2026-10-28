package home

import (
	"errors"
	"os"
	"testing"
)

func privateSpoolStore(t *testing.T) *ArtifactStore {
	t.Helper()
	h := t.TempDir()
	if err := os.Chmod(h, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := NewArtifactStore(h)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPrivateSpoolIsUnlinkedFDAndBounded(t *testing.T) {
	s := privateSpoolStore(t)
	p, err := s.CreatePrivateSpool()
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.OpenForChild()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("private-canary")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenForChild(); err == nil {
		t.Fatal("second child descriptor accepted")
	}
	got, err := p.Consume(64)
	if err != nil || string(got) != "private-canary" {
		t.Fatalf("consume %q %v", got, err)
	}
	if _, err := p.Consume(64); err == nil {
		t.Fatal("spool consumed twice")
	}
	if err := p.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained fd open after cleanup: %v", err)
	}
}
func TestPrivateSpoolRejectsBoundedGrowth(t *testing.T) {
	p, err := privateSpoolStore(t).CreatePrivateSpool()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := p.OpenForChild()
	if _, err = f.Write([]byte("oversize")); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Consume(3); err == nil {
		t.Fatal("oversize capture consumed")
	}
	if err := p.Remove(); err != nil {
		t.Fatal(err)
	}
}
