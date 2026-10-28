package terminal

import (
	"context"
	"testing"
)

// fakeTerminal is a minimal Terminal impl for registry tests. It is not
// used by production code.
type fakeTerminal struct {
	name     string
	bundleID string
}

func (f *fakeTerminal) Name() string     { return f.name }
func (f *fakeTerminal) BundleID() string { return f.bundleID }
func (f *fakeTerminal) Doctor(_ context.Context) (DoctorReport, error) {
	return DoctorReport{Installed: true}, nil
}
func (f *fakeTerminal) Hooks() HookSet          { return HookSet{} }
func (f *fakeTerminal) Workspace() WorkspaceAPI { return nil }
func (f *fakeTerminal) Feed() FeedAPI           { return nil }

func TestRegistry_Get_Names(t *testing.T) {
	r := FromImpls(
		&fakeTerminal{name: "alpha", bundleID: "x.alpha"},
		&fakeTerminal{name: "beta", bundleID: "x.beta"},
	)
	if got, ok := r.Get("alpha"); !ok || got.Name() != "alpha" {
		t.Fatalf("Get(alpha) = %v, %v; want alpha, true", got, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) returned ok=true")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("Names() = %v; want [alpha beta] (sorted)", names)
	}
}

func TestRegistry_NamesEmpty(t *testing.T) {
	r := FromImpls()
	if got := r.Names(); len(got) != 0 {
		t.Fatalf("Names() = %v; want empty", got)
	}
}

func TestRegistry_Get_NilSafe(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("x"); ok {
		t.Fatal("nil registry Get returned ok=true")
	}
	if names := r.Names(); names != nil {
		t.Fatalf("nil registry Names = %v; want nil", names)
	}
}

func TestFromImpls_SkipsNil(t *testing.T) {
	r := FromImpls(nil, &fakeTerminal{name: "z", bundleID: "x.z"})
	if got := r.Names(); len(got) != 1 || got[0] != "z" {
		t.Fatalf("Names() = %v; want [z]", got)
	}
}

func TestFromImpls_DuplicateWinsLast(t *testing.T) {
	first := &fakeTerminal{name: "dup", bundleID: "first"}
	second := &fakeTerminal{name: "dup", bundleID: "second"}
	r := FromImpls(first, second)
	got, ok := r.Get("dup")
	if !ok {
		t.Fatal("Get(dup) returned ok=false")
	}
	if got.(*fakeTerminal).bundleID != "second" {
		t.Fatalf("Get(dup) returned first impl; want last-wins")
	}
}

func TestRegister_NilSafe(t *testing.T) {
	Register(nil) // must not panic
}

func TestErrNotSupported(t *testing.T) {
	if ErrNotSupported == nil {
		t.Fatal("ErrNotSupported is nil")
	}
}

func TestFromImpls_OneNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FromImpls(nil) panicked: %v", r)
		}
	}()
	FromImpls(nil)
}
