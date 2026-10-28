package harness

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// verifyNativeCodexExecutable refuses script wrappers before read-only argv or
// subscription credentials are entrusted to the selected process.
func verifyNativeCodexExecutable(selected string) error {
	canonical, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("codex adapter: native executable unavailable")
	}
	want, err := os.Stat(canonical)
	if err != nil || !want.Mode().IsRegular() || want.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("codex adapter: native executable identity invalid")
	}
	f, err := os.Open(selected)
	if err != nil {
		return fmt.Errorf("codex adapter: native executable unavailable")
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil || !os.SameFile(want, got) {
		return fmt.Errorf("codex adapter: selected executable is not native codex")
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("codex adapter: selected executable is not native codex")
	}
	// Mach-O (thin and fat), ELF and PE are the supported native formats.
	native := bytes.Equal(magic[:], []byte{0xcf, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(magic[:], []byte{0xfe, 0xed, 0xfa, 0xcf}) ||
		bytes.Equal(magic[:], []byte{0xca, 0xfe, 0xba, 0xbe}) ||
		bytes.Equal(magic[:], []byte{0xbe, 0xba, 0xfe, 0xca}) ||
		bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) ||
		bytes.Equal(magic[:2], []byte{'M', 'Z'})
	if !native {
		return fmt.Errorf("codex adapter: selected executable is not native codex")
	}
	return nil
}

func resolvedCodexCommand(command string) (string, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("codex adapter: native executable unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("codex adapter: native executable unavailable")
	}
	return path, nil
}
