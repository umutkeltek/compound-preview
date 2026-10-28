package work

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"compound/internal/envelope"
)

const dispatchRecipeHeading = "## Verified recipe (run this; do not rediscover the setup)"

// dispatchWorkspace resolves the lane workspace the way delegation does:
// git_delivery.target_worktree_root, then the caller's cwd (os.Getwd when cwd
// is empty). It never falls back to COMPOUND_HOME.
func dispatchWorkspace(w envelope.WorkEnvelope, cwd string) string {
	if w.GitDelivery != nil && strings.TrimSpace(w.GitDelivery.TargetWorktreeRoot) != "" {
		return strings.TrimSpace(w.GitDelivery.TargetWorktreeRoot)
	}
	if strings.TrimSpace(cwd) == "" {
		cwd, _ = os.Getwd()
	}
	return strings.TrimSpace(cwd)
}

// attachDispatchRecipe delivers the workspace's repo-owned proof recipe
// (compound-proof.json) into the dispatch pack, so the lane runs the verified
// build/test command instead of rediscovering schemes, simulators, or build
// steps. It is the same declaration the canonical proof gate certifies from,
// checked by the same parser; an unreadable or invalid file is recorded as
// excluded with its reason and never reaches the lane. COMPOUND_HOME is never
// a workspace.
func attachDispatchRecipe(pack *envelope.ContextPack, workspace, home string) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || filepath.Clean(workspace) == filepath.Clean(home) {
		return
	}
	path := filepath.Join(workspace, CanonicalConsumerContractPath)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return
	}
	if err == nil && (!info.Mode().IsRegular() || info.Size() > CanonicalConsumerContractLimit) {
		err = fmt.Errorf("must be a regular file of at most %d bytes (mode %s, %d bytes)", CanonicalConsumerContractLimit, info.Mode(), info.Size())
	}
	var body []byte
	if err == nil {
		body, err = os.ReadFile(path)
	}
	var contract canonicalConsumerContract
	if err == nil {
		contract, err = parseCanonicalConsumerContract(body)
	}
	if err != nil {
		pack.Excluded = append(pack.Excluded, envelope.ExcludedRef{Ref: "file://" + path, Reason: "proof recipe not delivered: " + err.Error()})
		return
	}
	for _, c := range contract.Consumers {
		check := fmt.Sprintf("consumer:%s: run `%s` in %s (timeout %ds; tools %s", c.ID, dispatchShellJoin(c.Command), filepath.Join(workspace, c.Dir), c.TimeoutSeconds, strings.Join(c.RequiredTools, ","))
		if len(c.RequiredEnvironment) > 0 {
			check += "; env " + strings.Join(c.RequiredEnvironment, ",")
		}
		pack.RequiredChecks = append(pack.RequiredChecks, check+"; recipe "+path+")")
	}
	sum := sha256.Sum256(body)
	pack.SourceRevisions = append(pack.SourceRevisions, envelope.SourceRev{Source: "file://" + path, Rev: "sha256:" + hex.EncodeToString(sum[:])})
}

var dispatchShellSafeArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// dispatchShellJoin renders an argv as one copy-pasteable POSIX shell line.
func dispatchShellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = arg
		if !dispatchShellSafeArg.MatchString(arg) {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
