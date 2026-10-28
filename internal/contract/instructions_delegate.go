package contract

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// AppendDelegateVariantToPrompt compiles a minimal EffectiveContract for the
// delegate's harness, renders the preset-scoped variant, and returns the
// input prompt with the variant appended after a blank line. Empty prompt
// becomes the variant alone; empty cwd falls back to the current working
// directory. The render never writes the repo file — the variant is a
// read-only projection, drift is not checked (there is no tracked preset
// surface), and no runtime materialization is recorded to COMPOUND_HOME.
// DefaultPolicy is used so the call site does not have to load orientation
// policy or resolve a home path; the delegate's dispatch path is opt-in and
// transient.
//
// This is the internal/ home for the --instruction-block wire: the
// cmd/compound side holds a thin flag, and this function does the work.
func AppendDelegateVariantToPrompt(harness, cwd, preset, prompt string) (string, error) {
	harness = strings.TrimSpace(harness)
	preset = strings.TrimSpace(preset)
	if harness == "" {
		return "", fmt.Errorf("append delegate variant: harness required")
	}
	if preset == "" {
		return "", fmt.Errorf("append delegate variant: preset required")
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	in := Input{
		Harness:     harness,
		Cwd:         cwd,
		Task:        "",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec := Compile(in, DefaultPolicy(), time.Time{})
	render, err := RenderInstructionBlockVariant(ec, in.Cwd, preset)
	if err != nil {
		return "", fmt.Errorf("render variant: %w", err)
	}
	if prompt == "" {
		return render.Content, nil
	}
	return prompt + "\n\n" + render.Content, nil
}
