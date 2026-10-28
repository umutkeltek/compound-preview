package main

import (
	"bytes"
	"testing"
)

func TestGitIndexLockCLIRejectsMalformedCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"delete"}, {"apply", "--unknown"}, {"apply", "unexpected"}, {"review", "--expires", "not-a-time"}} {
		var out, err bytes.Buffer
		if code := runAuthorityGitIndexLock(t.TempDir(), args, &out, &err); code == 0 {
			t.Fatalf("accepted %v", args)
		}
		if err.Len() == 0 {
			t.Fatalf("missing failure explanation %v", args)
		}
	}
}
